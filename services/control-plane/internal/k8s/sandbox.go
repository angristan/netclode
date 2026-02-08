package k8s

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/angristan/netclode/services/control-plane/internal/config"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
)

// SandboxReadyCallback is called when a sandbox becomes ready
type SandboxReadyCallback func(sessionID string, serviceFQDN string, err error)

// ClaimBoundCallback is called when a SandboxClaim is bound to a sandbox
type ClaimBoundCallback func(sessionID string, sandboxName string, err error)

type k8sRuntime struct {
	dynamicClient dynamic.Interface
	clientset     *kubernetes.Clientset
	namespace     string
	config        *config.Config

	// Informer for watching sandbox changes
	informer     cache.SharedIndexInformer
	informerStop chan struct{}

	// Callbacks for sandbox ready notifications (multiple waiters supported)
	readyCallbacks map[string][]SandboxReadyCallback
	callbacksMu    sync.RWMutex

	// Cache of sandbox states
	sandboxCache map[string]*Sandbox
	cacheMu      sync.RWMutex

	// SandboxClaim informer and cache (for warm pool mode)
	claimInformer  cache.SharedIndexInformer
	claimCallbacks map[string]ClaimBoundCallback
	claimCache     map[string]*SandboxClaim
}

func newK8sRuntime(cfg *config.Config) (*k8sRuntime, error) {
	restConfig, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("get in-cluster config: %w", err)
	}

	dynamicClient, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("create dynamic client: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("create clientset: %w", err)
	}

	r := &k8sRuntime{
		dynamicClient:  dynamicClient,
		clientset:      clientset,
		namespace:      cfg.K8sNamespace,
		config:         cfg,
		informerStop:   make(chan struct{}),
		readyCallbacks: make(map[string][]SandboxReadyCallback),
		sandboxCache:   make(map[string]*Sandbox),
		claimCallbacks: make(map[string]ClaimBoundCallback),
		claimCache:     make(map[string]*SandboxClaim),
	}

	// Setup sandbox informer
	if err := r.setupInformer(); err != nil {
		return nil, fmt.Errorf("setup sandbox informer: %w", err)
	}

	// Setup claim informer if warm pool is enabled
	if cfg.UseWarmPool {
		if err := r.setupClaimInformer(); err != nil {
			return nil, fmt.Errorf("setup claim informer: %w", err)
		}
	}

	slog.Info("Kubernetes client initialized with informer", "namespace", cfg.K8sNamespace, "warmPool", cfg.UseWarmPool)
	return r, nil
}

func (r *k8sRuntime) setupInformer() error {
	factory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(
		r.dynamicClient,
		30*time.Second, // Resync period
		r.namespace,
		func(opts *metav1.ListOptions) {
			opts.LabelSelector = "netclode.io/session"
		},
	)

	r.informer = factory.ForResource(SandboxGVR).Informer()

	_, err := r.informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    r.onSandboxAdd,
		UpdateFunc: r.onSandboxUpdate,
		DeleteFunc: r.onSandboxDelete,
	})
	if err != nil {
		return err
	}

	// Start informer in background
	go r.informer.Run(r.informerStop)

	// Wait for initial sync
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if !cache.WaitForCacheSync(ctx.Done(), r.informer.HasSynced) {
		return fmt.Errorf("timeout waiting for informer sync")
	}

	slog.Info("Sandbox informer synced")
	return nil
}

func (r *k8sRuntime) onSandboxAdd(obj interface{}) {
	sandbox := r.unstructuredToSandbox(obj)
	if sandbox == nil {
		return
	}

	sessionID := r.getSessionID(sandbox)
	slog.Debug("Sandbox added", "sessionID", sessionID, "ready", sandbox.IsReady())

	r.cacheMu.Lock()
	r.sandboxCache[sessionID] = sandbox
	r.cacheMu.Unlock()

	r.checkAndNotify(sessionID, sandbox)
}

func (r *k8sRuntime) onSandboxUpdate(oldObj, newObj interface{}) {
	sandbox := r.unstructuredToSandbox(newObj)
	if sandbox == nil {
		return
	}

	sessionID := r.getSessionID(sandbox)
	slog.Debug("Sandbox updated", "sessionID", sessionID, "ready", sandbox.IsReady(), "fqdn", sandbox.Status.ServiceFQDN)

	r.cacheMu.Lock()
	r.sandboxCache[sessionID] = sandbox
	r.cacheMu.Unlock()

	r.checkAndNotify(sessionID, sandbox)
}

func (r *k8sRuntime) onSandboxDelete(obj interface{}) {
	sandbox := r.unstructuredToSandbox(obj)
	if sandbox == nil {
		// Handle DeletedFinalStateUnknown
		if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
			sandbox = r.unstructuredToSandbox(tombstone.Obj)
		}
	}
	if sandbox == nil {
		return
	}

	sessionID := r.getSessionID(sandbox)
	slog.Debug("Sandbox deleted", "sessionID", sessionID)

	r.cacheMu.Lock()
	delete(r.sandboxCache, sessionID)
	r.cacheMu.Unlock()
}

func (r *k8sRuntime) unstructuredToSandbox(obj interface{}) *Sandbox {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return nil
	}

	data, err := u.MarshalJSON()
	if err != nil {
		slog.Warn("Failed to marshal unstructured", "error", err)
		return nil
	}

	var sandbox Sandbox
	if err := json.Unmarshal(data, &sandbox); err != nil {
		slog.Warn("Failed to unmarshal sandbox", "error", err)
		return nil
	}

	return &sandbox
}

func (r *k8sRuntime) getSessionID(sandbox *Sandbox) string {
	if id, ok := sandbox.Labels["netclode.io/session"]; ok {
		return id
	}
	// Fallback: extract from name
	name := sandbox.Name
	if strings.HasPrefix(name, "sess-") {
		return strings.TrimPrefix(name, "sess-")
	}
	return ""
}

func (r *k8sRuntime) checkAndNotify(sessionID string, sandbox *Sandbox) {
	r.callbacksMu.RLock()
	callbacks, ok := r.readyCallbacks[sessionID]
	r.callbacksMu.RUnlock()

	if !ok || len(callbacks) == 0 {
		return
	}

	if sandbox.IsReady() {
		fqdn := r.getServiceFQDN(sandbox)

		// Remove all callbacks before invoking to prevent double-call
		r.callbacksMu.Lock()
		callbacksCopy := r.readyCallbacks[sessionID]
		delete(r.readyCallbacks, sessionID)
		r.callbacksMu.Unlock()

		// Notify all waiters
		for _, callback := range callbacksCopy {
			callback(sessionID, fqdn, nil)
		}
	} else if errMsg := sandbox.GetError(); errMsg != "" {
		// Log error but don't fail immediately - some errors are transient
		// (e.g., "Operation cannot be fulfilled" conflicts with sandbox controller)
		// The sandbox may still become ready, so keep waiting
		slog.Warn("Sandbox has error, continuing to wait", "sessionID", sessionID, "error", errMsg)
	}
}

// Close stops the informer
func (r *k8sRuntime) Close() {
	close(r.informerStop)
}

func sandboxName(sessionID string) string {
	return "sess-" + sessionID
}

// getServiceFQDN returns the service FQDN for a sandbox.
// If the sandbox status doesn't have it, we construct it from the sandbox name.
func (r *k8sRuntime) getServiceFQDN(sandbox *Sandbox) string {
	if sandbox.Status.ServiceFQDN != "" {
		return sandbox.Status.ServiceFQDN
	}
	// Construct FQDN if not set (warm pool controller doesn't populate it)
	return fmt.Sprintf("%s.%s.svc.cluster.local", sandbox.Name, r.namespace)
}

func secretName(sessionID string) string {
	return "sess-" + sessionID + "-env"
}

func pvcName(sessionID string) string {
	return "workspace-sess-" + sessionID
}

// CreateSandbox creates a new sandbox for a session.
// ExistingPVCEnvKey is a special env key used to pass an existing PVC name.
// This is used for snapshot restore where the PVC is created separately before the sandbox.
// It's not passed to the actual container, just used to configure the sandbox.
const ExistingPVCEnvKey = "_EXISTING_PVC_NAME"

func (r *k8sRuntime) CreateSandbox(ctx context.Context, sessionID string, env map[string]string, resources *SandboxResourceConfig) error {
	// Extract existing PVC name if present (not passed to container)
	existingPVCName := env[ExistingPVCEnvKey]
	delete(env, ExistingPVCEnvKey)

	// First create the environment secret
	if err := r.createEnvSecret(ctx, sessionID, env); err != nil {
		return fmt.Errorf("create env secret: %w", err)
	}

	// Create the Sandbox CRD
	sandbox := r.buildSandboxManifest(sessionID, existingPVCName, resources)

	data, err := json.Marshal(sandbox)
	if err != nil {
		_ = r.DeleteSecret(ctx, sessionID)
		return fmt.Errorf("marshal sandbox: %w", err)
	}

	var u unstructured.Unstructured
	if err := json.Unmarshal(data, &u.Object); err != nil {
		_ = r.DeleteSecret(ctx, sessionID)
		return fmt.Errorf("convert to unstructured: %w", err)
	}

	_, err = r.dynamicClient.Resource(SandboxGVR).Namespace(r.namespace).Create(ctx, &u, metav1.CreateOptions{})
	if err != nil {
		_ = r.DeleteSecret(ctx, sessionID)
		return fmt.Errorf("create sandbox: %w", err)
	}

	slog.Info("Sandbox created", "sessionID", sessionID, "name", sandboxName(sessionID))
	return nil
}

func (r *k8sRuntime) createEnvSecret(ctx context.Context, sessionID string, env map[string]string) error {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName(sessionID),
			Namespace: r.namespace,
			Labels: map[string]string{
				"netclode.io/session": sessionID,
			},
		},
		Type:       corev1.SecretTypeOpaque,
		StringData: env,
	}

	_, err := r.clientset.CoreV1().Secrets(r.namespace).Create(ctx, secret, metav1.CreateOptions{})
	if err != nil && !errors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

func (r *k8sRuntime) buildSandboxManifest(sessionID string, existingPVCName string, resources *SandboxResourceConfig) *Sandbox {
	name := sandboxName(sessionID)

	// If we have an existing PVC (from restore), use Volumes instead of VolumeClaimTemplates
	var volumeClaimTemplates []PVCTemplate
	var volumes []Volume

	if existingPVCName != "" {
		// Use existing PVC that was pre-created with restored data
		slog.Info("Building sandbox with existing PVC", "sessionID", sessionID, "pvc", existingPVCName)
		volumes = []Volume{
			{
				Name: "agent-home",
				PersistentVolumeClaim: &PersistentVolumeClaimVolumeSource{
					ClaimName: existingPVCName,
				},
			},
		}
	} else {
		// Create new PVC via volumeClaimTemplate
		volumeClaimTemplates = []PVCTemplate{
			{
				Metadata: metav1.ObjectMeta{
					Name: "agent-home",
					Labels: map[string]string{
						"netclode.io/session": sessionID,
					},
				},
				Spec: PVCSpec{
					AccessModes:      []string{"ReadWriteOnce"},
					StorageClassName: "juicefs-sc",
					Resources: ResourceRequirements{
						Requests: map[string]string{
							"storage": "10Gi",
						},
					},
				},
			},
		}
	}

	// Build pod metadata with Kata annotations for custom resources
	podMetadata := r.buildPodMetadata(sessionID, resources)

	// Build container resources for K8s scheduling
	containerResources := r.buildContainerResources(resources)

	return &Sandbox{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "agents.x-k8s.io/v1alpha1",
			Kind:       "Sandbox",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: r.namespace,
			Labels: map[string]string{
				"netclode.io/session": sessionID,
			},
		},
		Spec: SandboxSpec{
			PodTemplate: PodTemplateSpec{
				Metadata: podMetadata,
				Spec: PodSpec{
					RuntimeClassName: "kata-clh",
					Containers: []Container{
						{
							Name:  "agent",
							Image: r.config.AgentImage,
							SecurityContext: &SecurityContext{
								Privileged:             true,
								ReadOnlyRootFilesystem: false,
							},
							Ports: []Port{
								{ContainerPort: 3002, Name: "http"},
							},
							Env: []EnvVar{
								{Name: "NODE_ENV", Value: "production"},
							},
							EnvFrom: []EnvFromSource{
								{SecretRef: &SecretRef{Name: secretName(sessionID)}},
							},
							VolumeMounts: []VolumeMount{
								{Name: "agent-home", MountPath: "/agent"},
							},
							ReadinessProbe: &Probe{
								Exec: &ExecAction{
									Command: []string{"test", "-f", "/tmp/agent-ready"},
								},
								InitialDelaySeconds: 3,
								PeriodSeconds:       5,
							},
							Resources: containerResources,
						},
					},
					Volumes: volumes,
				},
			},
			VolumeClaimTemplates: volumeClaimTemplates,
		},
	}
}

// buildPodMetadata creates pod metadata with labels and Kata annotations for VM sizing.
func (r *k8sRuntime) buildPodMetadata(sessionID string, resources *SandboxResourceConfig) PodMetadata {
	metadata := PodMetadata{
		Labels: map[string]string{
			"netclode.io/session": sessionID,
		},
	}

	// Add Kata annotations for custom VM sizing
	if resources != nil {
		annotations := make(map[string]string)
		if resources.VCPUs > 0 {
			annotations["io.katacontainers.config.hypervisor.default_vcpus"] = fmt.Sprintf("%d", resources.VCPUs)
		}
		if resources.MemoryMB > 0 {
			annotations["io.katacontainers.config.hypervisor.default_memory"] = fmt.Sprintf("%d", resources.MemoryMB)
		}
		if len(annotations) > 0 {
			metadata.Annotations = annotations
			slog.Info("Setting custom VM resources", "sessionID", sessionID, "vcpus", resources.VCPUs, "memoryMB", resources.MemoryMB)
		}
	}

	return metadata
}

// buildContainerResources creates container resource requests for K8s scheduling.
// We use requests (NOT limits!) to inform the scheduler while avoiding cgroup throttling.
// Requests are divided by overcommit ratio to allow packing more pods on the node.
// The actual VM resources (via Kata annotations) remain at full size.
func (r *k8sRuntime) buildContainerResources(resources *SandboxResourceConfig) *ContainerResources {
	if resources == nil {
		return nil
	}

	requests := make(map[string]string)

	if resources.VCPUs > 0 {
		cpuRequest := int(resources.VCPUs)
		if r.config.CPUOvercommitRatio > 1 {
			cpuRequest = int(resources.VCPUs) / r.config.CPUOvercommitRatio
			if cpuRequest < 1 {
				cpuRequest = 1 // Minimum 1 CPU request
			}
		}
		requests["cpu"] = fmt.Sprintf("%d", cpuRequest)
	}
	if resources.MemoryMB > 0 {
		memRequest := int(resources.MemoryMB)
		if r.config.MemoryOvercommitRatio > 1 {
			memRequest = int(resources.MemoryMB) / r.config.MemoryOvercommitRatio
			if memRequest < 512 {
				memRequest = 512 // Minimum 512Mi request
			}
		}
		requests["memory"] = fmt.Sprintf("%dMi", memRequest)
	}

	if len(requests) == 0 {
		return nil
	}

	return &ContainerResources{
		Requests: requests,
	}
}

// WaitForReady registers a callback to be called when sandbox becomes ready.
// Uses informer-based watching instead of polling.
func (r *k8sRuntime) WaitForReady(ctx context.Context, sessionID string, timeout time.Duration) (string, error) {
	// Check if already ready from cache
	r.cacheMu.RLock()
	sandbox, exists := r.sandboxCache[sessionID]
	r.cacheMu.RUnlock()

	if exists && sandbox.IsReady() {
		return r.getServiceFQDN(sandbox), nil
	}

	// Setup callback channel
	resultCh := make(chan struct {
		fqdn string
		err  error
	}, 1)

	// Append callback to slice (supports multiple concurrent waiters)
	r.callbacksMu.Lock()
	r.readyCallbacks[sessionID] = append(r.readyCallbacks[sessionID], func(sid string, fqdn string, err error) {
		resultCh <- struct {
			fqdn string
			err  error
		}{fqdn, err}
	})
	r.callbacksMu.Unlock()

	// Note: No cleanup needed here - checkAndNotify clears all callbacks when sandbox becomes ready.
	// If this goroutine times out, the callback stays but is harmless (sends to buffered channel).

	// Wait for result or timeout
	select {
	case result := <-resultCh:
		if result.err != nil {
			return "", result.err
		}
		slog.Info("Sandbox ready (via informer)", "sessionID", sessionID, "serviceFQDN", result.fqdn)
		return result.fqdn, nil
	case <-time.After(timeout):
		return "", fmt.Errorf("timeout waiting for sandbox %s to be ready", sandboxName(sessionID))
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// WatchSandboxReady registers a callback without blocking.
// The callback will be called when the sandbox becomes ready or errors.
func (r *k8sRuntime) WatchSandboxReady(sessionID string, callback SandboxReadyCallback) {
	// Check if already ready from cache
	r.cacheMu.RLock()
	sandbox, exists := r.sandboxCache[sessionID]
	r.cacheMu.RUnlock()

	if exists && sandbox.IsReady() {
		go callback(sessionID, r.getServiceFQDN(sandbox), nil)
		return
	}

	if exists {
		if errMsg := sandbox.GetError(); errMsg != "" {
			// Log error but don't fail immediately - some errors are transient
			slog.Warn("Sandbox has error on check, will wait for ready", "sessionID", sessionID, "error", errMsg)
		}
	}

	// Register callback for future updates (append to support multiple waiters)
	r.callbacksMu.Lock()
	r.readyCallbacks[sessionID] = append(r.readyCallbacks[sessionID], callback)
	r.callbacksMu.Unlock()
}

// GetStatus retrieves the status of a sandbox from cache.
func (r *k8sRuntime) GetStatus(ctx context.Context, sessionID string) (*SandboxStatusInfo, error) {
	r.cacheMu.RLock()
	sandbox, exists := r.sandboxCache[sessionID]
	r.cacheMu.RUnlock()

	if !exists {
		// Try fetching directly
		name := sandboxName(sessionID)
		u, err := r.dynamicClient.Resource(SandboxGVR).Namespace(r.namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			if errors.IsNotFound(err) {
				return &SandboxStatusInfo{Exists: false}, nil
			}
			return nil, err
		}
		sandbox = r.unstructuredToSandbox(u)
		if sandbox == nil {
			return &SandboxStatusInfo{Exists: false}, nil
		}
	}

	return &SandboxStatusInfo{
		Exists:      true,
		Ready:       sandbox.IsReady(),
		ServiceFQDN: r.getServiceFQDN(sandbox),
		Error:       sandbox.GetError(),
	}, nil
}

// DeleteSandbox deletes a sandbox.
func (r *k8sRuntime) DeleteSandbox(ctx context.Context, sessionID string) error {
	name := sandboxName(sessionID)

	err := r.dynamicClient.Resource(SandboxGVR).Namespace(r.namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !errors.IsNotFound(err) {
		return err
	}

	slog.Info("Sandbox deleted", "sessionID", sessionID, "name", name)
	return nil
}

// waitForSandboxDeletion polls until the sandbox is actually deleted.
// This is faster than a blind sleep since deletion usually completes quickly.
func (r *k8sRuntime) waitForSandboxDeletion(ctx context.Context, sessionID string, timeout time.Duration) error {
	name := sandboxName(sessionID)
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		_, err := r.dynamicClient.Resource(SandboxGVR).Namespace(r.namespace).Get(ctx, name, metav1.GetOptions{})
		if errors.IsNotFound(err) {
			return nil // Sandbox is deleted
		}
		if err != nil {
			return fmt.Errorf("check sandbox deletion: %w", err)
		}
		time.Sleep(200 * time.Millisecond)
	}

	return fmt.Errorf("timeout waiting for sandbox %s deletion", name)
}

// DeletePVC deletes the persistent volume claim for a session.
func (r *k8sRuntime) DeletePVC(ctx context.Context, sessionID string) error {
	name := pvcName(sessionID)

	err := r.clientset.CoreV1().PersistentVolumeClaims(r.namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !errors.IsNotFound(err) {
		return err
	}

	slog.Info("PVC deleted", "sessionID", sessionID, "name", name)
	return nil
}

// DeletePVCByName deletes a PVC by its exact name.
func (r *k8sRuntime) DeletePVCByName(ctx context.Context, name string) error {
	err := r.clientset.CoreV1().PersistentVolumeClaims(r.namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !errors.IsNotFound(err) {
		return err
	}

	slog.Info("PVC deleted by name", "name", name)
	return nil
}

// sessionAnchorName returns the name for a session's anchor ConfigMap.
func sessionAnchorName(sessionID string) string {
	return fmt.Sprintf("session-anchor-%s", sessionID)
}

// EnsureSessionAnchor creates a ConfigMap that acts as an anchor for the session's PVC.
// This ConfigMap becomes a second owner of the PVC, preventing it from being garbage-collected
// when the Sandbox is deleted (during pause). The PVC will only be deleted when both
// the Sandbox AND the ConfigMap are deleted.
func (r *k8sRuntime) EnsureSessionAnchor(ctx context.Context, sessionID string) error {
	name := sessionAnchorName(sessionID)

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: r.namespace,
			Labels: map[string]string{
				"netclode.io/session":        sessionID,
				"netclode.io/session-anchor": "true",
			},
		},
		Data: map[string]string{
			"sessionID": sessionID,
			"purpose":   "Anchor for session PVC ownership. Do not delete manually.",
		},
	}

	_, err := r.clientset.CoreV1().ConfigMaps(r.namespace).Create(ctx, cm, metav1.CreateOptions{})
	if err != nil {
		if errors.IsAlreadyExists(err) {
			slog.Debug("Session anchor already exists", "sessionID", sessionID)
			return nil
		}
		return fmt.Errorf("create session anchor: %w", err)
	}

	slog.Info("Session anchor created", "sessionID", sessionID, "name", name)
	return nil
}

// DeleteSessionAnchor deletes the session's anchor ConfigMap.
// This should be called when deleting a session, which will allow the PVC to be garbage-collected
// (assuming the Sandbox is also deleted or doesn't exist).
func (r *k8sRuntime) DeleteSessionAnchor(ctx context.Context, sessionID string) error {
	name := sessionAnchorName(sessionID)

	err := r.clientset.CoreV1().ConfigMaps(r.namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("delete session anchor: %w", err)
	}

	slog.Info("Session anchor deleted", "sessionID", sessionID, "name", name)
	return nil
}

// AddSessionAnchorToPVC adds the session's anchor ConfigMap as an owner of the PVC.
// This ensures the PVC won't be garbage-collected when the Sandbox is deleted.
// The PVC will only be GC'd when both owners (Sandbox and ConfigMap) are deleted.
func (r *k8sRuntime) AddSessionAnchorToPVC(ctx context.Context, sessionID, pvcName string) error {
	anchorName := sessionAnchorName(sessionID)

	// Get the ConfigMap to get its UID
	cm, err := r.clientset.CoreV1().ConfigMaps(r.namespace).Get(ctx, anchorName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get session anchor: %w", err)
	}

	// Get the PVC
	pvc, err := r.clientset.CoreV1().PersistentVolumeClaims(r.namespace).Get(ctx, pvcName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get PVC: %w", err)
	}

	// Check if ConfigMap is already an owner
	for _, ref := range pvc.OwnerReferences {
		if ref.UID == cm.UID {
			slog.Debug("Session anchor already owns PVC", "sessionID", sessionID, "pvc", pvcName)
			return nil
		}
	}

	// Add ConfigMap as owner (non-controller owner, so it doesn't conflict with Sandbox)
	pvc.OwnerReferences = append(pvc.OwnerReferences, metav1.OwnerReference{
		APIVersion:         "v1",
		Kind:               "ConfigMap",
		Name:               cm.Name,
		UID:                cm.UID,
		Controller:         ptr(false),
		BlockOwnerDeletion: ptr(true),
	})

	_, err = r.clientset.CoreV1().PersistentVolumeClaims(r.namespace).Update(ctx, pvc, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("update PVC with session anchor owner: %w", err)
	}

	slog.Info("Added session anchor as PVC owner", "sessionID", sessionID, "pvc", pvcName, "anchor", anchorName)
	return nil
}

// ptr returns a pointer to the given value.
func ptr[T any](v T) *T {
	return &v
}

// DeleteSecret deletes the environment secret for a session.
func (r *k8sRuntime) DeleteSecret(ctx context.Context, sessionID string) error {
	name := secretName(sessionID)

	err := r.clientset.CoreV1().Secrets(r.namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !errors.IsNotFound(err) {
		return err
	}

	slog.Info("Secret deleted", "sessionID", sessionID, "name", name)
	return nil
}

// CreateSandboxService creates a Kubernetes Service for the sandbox with Tailscale annotations.
// This enables preview URLs for web apps running inside the sandbox.
func (r *k8sRuntime) CreateSandboxService(ctx context.Context, sessionID string) error {
	sandboxSvcName := sandboxName(sessionID)
	tailscaleSvcName := fmt.Sprintf("ts-%s", sessionID)

	// Look up the existing headless service created by the sandbox controller
	// to get the correct selector labels for the pod
	existingSvc, err := r.clientset.CoreV1().Services(r.namespace).Get(ctx, sandboxSvcName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get sandbox service: %w", err)
	}

	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tailscaleSvcName,
			Namespace: r.namespace,
			Labels: map[string]string{
				"netclode.io/session": sessionID,
			},
			Annotations: map[string]string{
				"tailscale.com/expose":   "true",
				"tailscale.com/hostname": fmt.Sprintf("sandbox-%s", sessionID),
			},
		},
		Spec: corev1.ServiceSpec{
			Selector: existingSvc.Spec.Selector, // Copy selector from sandbox controller's service
			Ports: []corev1.ServicePort{
				{
					Name:       "agent",
					Port:       3002,
					TargetPort: intstr.FromInt(3002),
				},
			},
			Type: corev1.ServiceTypeClusterIP,
		},
	}

	_, err = r.clientset.CoreV1().Services(r.namespace).Create(ctx, service, metav1.CreateOptions{})
	if err != nil && !errors.IsAlreadyExists(err) {
		return fmt.Errorf("create service: %w", err)
	}

	slog.Info("Sandbox service created", "sessionID", sessionID, "name", tailscaleSvcName, "hostname", fmt.Sprintf("sandbox-%s", sessionID))
	return nil
}

// DeleteSandboxService deletes the Kubernetes Service for a sandbox.
func (r *k8sRuntime) DeleteSandboxService(ctx context.Context, sessionID string) error {
	name := fmt.Sprintf("ts-%s", sessionID)

	err := r.clientset.CoreV1().Services(r.namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !errors.IsNotFound(err) {
		return err
	}

	slog.Info("Sandbox service deleted", "sessionID", sessionID, "name", name)
	return nil
}

// ListTailscaleServices returns the session IDs that have Tailscale services (ts-* services).
// This is used during startup reconciliation to clean up orphaned services.
func (r *k8sRuntime) ListTailscaleServices(ctx context.Context) ([]string, error) {
	services, err := r.clientset.CoreV1().Services(r.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "netclode.io/session",
	})
	if err != nil {
		return nil, fmt.Errorf("list services: %w", err)
	}

	sessionIDs := make([]string, 0, len(services.Items))
	for _, svc := range services.Items {
		if sessionID, ok := svc.Labels["netclode.io/session"]; ok {
			sessionIDs = append(sessionIDs, sessionID)
		}
	}

	return sessionIDs, nil
}

// ExposePort adds a port to the Tailscale service and NetworkPolicy for a sandbox.
// This is called when a port_exposed event is received from the agent.
func (r *k8sRuntime) ExposePort(ctx context.Context, sessionID string, port int) error {
	tailscaleSvcName := fmt.Sprintf("ts-%s", sessionID)
	networkPolicyName := fmt.Sprintf("sess-%s-network-policy", sessionID)

	// 1. Add port to the Tailscale service
	svc, err := r.clientset.CoreV1().Services(r.namespace).Get(ctx, tailscaleSvcName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get tailscale service: %w", err)
	}

	// Check if port already exists
	portName := fmt.Sprintf("preview-%d", port)
	portExists := false
	for _, p := range svc.Spec.Ports {
		if p.Port == int32(port) {
			portExists = true
			break
		}
	}

	if !portExists {
		svc.Spec.Ports = append(svc.Spec.Ports, corev1.ServicePort{
			Name:       portName,
			Port:       int32(port),
			TargetPort: intstr.FromInt(port),
			Protocol:   corev1.ProtocolTCP,
		})

		_, err = r.clientset.CoreV1().Services(r.namespace).Update(ctx, svc, metav1.UpdateOptions{})
		if err != nil {
			return fmt.Errorf("update service: %w", err)
		}
		slog.Info("Added port to Tailscale service", "sessionID", sessionID, "port", port)
	}

	// 2. Add port to the NetworkPolicy
	np, err := r.clientset.NetworkingV1().NetworkPolicies(r.namespace).Get(ctx, networkPolicyName, metav1.GetOptions{})
	if err != nil {
		// NetworkPolicy might not exist (e.g., if sandbox was created without one)
		if errors.IsNotFound(err) {
			slog.Warn("NetworkPolicy not found, skipping", "sessionID", sessionID, "name", networkPolicyName)
			return nil
		}
		return fmt.Errorf("get network policy: %w", err)
	}

	// Check if an ingress rule for this port from Tailscale already exists
	tailscaleNSSelector := metav1.LabelSelector{
		MatchLabels: map[string]string{
			"kubernetes.io/metadata.name": "tailscale",
		},
	}
	portProtocol := corev1.ProtocolTCP
	portVal := intstr.FromInt(port)

	ruleExists := false
	for _, rule := range np.Spec.Ingress {
		// Check if this rule is for Tailscale namespace
		isTailscaleRule := false
		for _, from := range rule.From {
			if from.NamespaceSelector != nil &&
				from.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] == "tailscale" {
				isTailscaleRule = true
				break
			}
		}
		if isTailscaleRule {
			// Check if port is in this rule
			for _, p := range rule.Ports {
				if p.Port != nil && p.Port.IntValue() == port {
					ruleExists = true
					break
				}
			}
		}
		if ruleExists {
			break
		}
	}

	if !ruleExists {
		// Add new ingress rule for this port
		np.Spec.Ingress = append(np.Spec.Ingress, networkingv1.NetworkPolicyIngressRule{
			From: []networkingv1.NetworkPolicyPeer{
				{
					NamespaceSelector: &tailscaleNSSelector,
				},
			},
			Ports: []networkingv1.NetworkPolicyPort{
				{
					Protocol: &portProtocol,
					Port:     &portVal,
				},
			},
		})

		_, err = r.clientset.NetworkingV1().NetworkPolicies(r.namespace).Update(ctx, np, metav1.UpdateOptions{})
		if err != nil {
			return fmt.Errorf("update network policy: %w", err)
		}
		slog.Info("Added port to NetworkPolicy", "sessionID", sessionID, "port", port)
	}

	return nil
}

// ConfigureNetwork enables or disables internet access for a sandbox.
// ConfigureNetwork adds or removes internet access for a sandbox.
// The default SandboxTemplate has NO internet access (only DNS + control-plane).
// This is always called with networkEnabled=true to add internet access (required for LLM APIs).
func (r *k8sRuntime) ConfigureNetwork(ctx context.Context, sessionID string, networkEnabled bool) error {
	internetPolicyName := fmt.Sprintf("sess-%s-internet-access", sessionID)

	if !networkEnabled {
		// Network disabled: remove the internet policy if it exists
		err := r.clientset.NetworkingV1().NetworkPolicies(r.namespace).Delete(ctx, internetPolicyName, metav1.DeleteOptions{})
		if err != nil && !errors.IsNotFound(err) {
			return fmt.Errorf("delete internet access policy: %w", err)
		}
		if err == nil {
			slog.Info("Removed internet access policy", "sessionID", sessionID)
		}
		return nil
	}

	// Get the claim UID to use as pod selector
	r.cacheMu.RLock()
	claim, exists := r.claimCache[sessionID]
	r.cacheMu.RUnlock()

	if !exists || claim == nil {
		return fmt.Errorf("sandbox claim not found for session %s", sessionID)
	}

	claimUID := string(claim.UID)
	if claimUID == "" {
		return fmt.Errorf("sandbox claim UID is empty for session %s", sessionID)
	}

	// Network enabled: create policy allowing internet access (0.0.0.0/0)
	// This policy adds to the template policy (which only allows DNS + control-plane)
	// Private ranges are excluded - use ConfigureTailnetAccess for Tailscale (100.64.0.0/10)
	policy := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      internetPolicyName,
			Namespace: r.namespace,
			Labels: map[string]string{
				"netclode.io/session":     sessionID,
				"netclode.io/policy-type": "internet-access",
			},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					"agents.x-k8s.io/claim-uid": claimUID,
				},
			},
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeEgress,
			},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				// Allow internet (excluding private ranges)
				{
					To: []networkingv1.NetworkPolicyPeer{
						{
							IPBlock: &networkingv1.IPBlock{
								CIDR: "0.0.0.0/0",
								Except: []string{
									"10.0.0.0/8",
									"172.16.0.0/12",
									"192.168.0.0/16",
									"100.64.0.0/10", // Tailscale CGNAT - use ConfigureTailnetAccess
								},
							},
						},
					},
				},
			},
		},
	}

	_, err := r.clientset.NetworkingV1().NetworkPolicies(r.namespace).Create(ctx, policy, metav1.CreateOptions{})
	if errors.IsAlreadyExists(err) {
		_, err = r.clientset.NetworkingV1().NetworkPolicies(r.namespace).Update(ctx, policy, metav1.UpdateOptions{})
	}
	if err != nil {
		return fmt.Errorf("apply internet access policy: %w", err)
	}

	slog.Info("Applied internet access policy", "sessionID", sessionID)
	return nil
}

// ConfigureTailnetAccess enables or disables Tailnet access for a sandbox.
// When enabled, creates a NetworkPolicy allowing egress to the Tailscale CGNAT range (100.64.0.0/10).
// This overrides the default template policy that blocks private networks.
func (r *k8sRuntime) ConfigureTailnetAccess(ctx context.Context, sessionID string, tailnetEnabled bool) error {
	tailnetPolicyName := fmt.Sprintf("sess-%s-tailnet-access", sessionID)

	if !tailnetEnabled {
		// Tailnet disabled: remove the allow policy
		err := r.clientset.NetworkingV1().NetworkPolicies(r.namespace).Delete(ctx, tailnetPolicyName, metav1.DeleteOptions{})
		if err != nil && !errors.IsNotFound(err) {
			return fmt.Errorf("delete tailnet access policy: %w", err)
		}
		if err == nil {
			slog.Info("Removed tailnet access policy", "sessionID", sessionID)
		}
		return nil
	}

	// Get the claim UID to use as pod selector
	r.cacheMu.RLock()
	claim, exists := r.claimCache[sessionID]
	r.cacheMu.RUnlock()

	if !exists || claim == nil {
		return fmt.Errorf("sandbox claim not found for session %s", sessionID)
	}

	claimUID := string(claim.UID)
	if claimUID == "" {
		return fmt.Errorf("sandbox claim UID is empty for session %s", sessionID)
	}

	// Tailnet enabled: create policy allowing 100.64.0.0/10
	// This policy adds to the template policy, allowing Tailscale CGNAT range
	policy := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tailnetPolicyName,
			Namespace: r.namespace,
			Labels: map[string]string{
				"netclode.io/session":     sessionID,
				"netclode.io/policy-type": "tailnet-access",
			},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					"agents.x-k8s.io/claim-uid": claimUID,
				},
			},
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeEgress,
			},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				// Allow Tailscale CGNAT range (100.64.0.0/10)
				{
					To: []networkingv1.NetworkPolicyPeer{
						{
							IPBlock: &networkingv1.IPBlock{
								CIDR: "100.64.0.0/10",
							},
						},
					},
				},
			},
		},
	}

	_, err := r.clientset.NetworkingV1().NetworkPolicies(r.namespace).Create(ctx, policy, metav1.CreateOptions{})
	if errors.IsAlreadyExists(err) {
		_, err = r.clientset.NetworkingV1().NetworkPolicies(r.namespace).Update(ctx, policy, metav1.UpdateOptions{})
	}
	if err != nil {
		return fmt.Errorf("apply tailnet access policy: %w", err)
	}

	slog.Info("Applied tailnet access policy", "sessionID", sessionID)
	return nil
}

// DeleteNetworkRestriction removes any network restriction and tailnet access policies for a session.
// This is called during sandbox cleanup.
func (r *k8sRuntime) DeleteNetworkRestriction(ctx context.Context, sessionID string) error {
	// Delete internet access policy (created by ConfigureNetwork)
	internetPolicyName := fmt.Sprintf("sess-%s-internet-access", sessionID)
	err := r.clientset.NetworkingV1().NetworkPolicies(r.namespace).Delete(ctx, internetPolicyName, metav1.DeleteOptions{})
	if err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("delete internet access policy: %w", err)
	}

	// Delete tailnet access policy
	tailnetPolicyName := fmt.Sprintf("sess-%s-tailnet-access", sessionID)
	err = r.clientset.NetworkingV1().NetworkPolicies(r.namespace).Delete(ctx, tailnetPolicyName, metav1.DeleteOptions{})
	if err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("delete tailnet access policy: %w", err)
	}

	return nil
}

// ListSandboxes lists all sandboxes from cache.
func (r *k8sRuntime) ListSandboxes(ctx context.Context) ([]SandboxInfo, error) {
	r.cacheMu.RLock()
	defer r.cacheMu.RUnlock()

	sandboxes := make([]SandboxInfo, 0, len(r.sandboxCache))
	for sessionID, sandbox := range r.sandboxCache {
		sandboxes = append(sandboxes, SandboxInfo{
			SessionID:   sessionID,
			ServiceFQDN: r.getServiceFQDN(sandbox),
			Ready:       sandbox.IsReady(),
		})
	}

	return sandboxes, nil
}

// ============================================================================
// SandboxClaim operations (warm pool mode)
// ============================================================================

func sandboxClaimName(sessionID string) string {
	return "sess-" + sessionID
}

func (r *k8sRuntime) setupClaimInformer() error {
	factory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(
		r.dynamicClient,
		30*time.Second,
		r.namespace,
		func(opts *metav1.ListOptions) {
			opts.LabelSelector = "netclode.io/session"
		},
	)

	r.claimInformer = factory.ForResource(SandboxClaimGVR).Informer()

	_, err := r.claimInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    r.onClaimAdd,
		UpdateFunc: r.onClaimUpdate,
		DeleteFunc: r.onClaimDelete,
	})
	if err != nil {
		return err
	}

	// Start informer in background
	go r.claimInformer.Run(r.informerStop)

	// Wait for initial sync
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if !cache.WaitForCacheSync(ctx.Done(), r.claimInformer.HasSynced) {
		return fmt.Errorf("timeout waiting for claim informer sync")
	}

	slog.Info("SandboxClaim informer synced")
	return nil
}

func (r *k8sRuntime) onClaimAdd(obj interface{}) {
	claim := r.unstructuredToClaim(obj)
	if claim == nil {
		return
	}

	sessionID := r.getSessionIDFromClaim(claim)
	slog.Debug("SandboxClaim added", "sessionID", sessionID, "bound", claim.IsBound())

	r.cacheMu.Lock()
	r.claimCache[sessionID] = claim
	r.cacheMu.Unlock()

	r.checkAndNotifyClaim(sessionID, claim)
}

func (r *k8sRuntime) onClaimUpdate(oldObj, newObj interface{}) {
	claim := r.unstructuredToClaim(newObj)
	if claim == nil {
		return
	}

	sessionID := r.getSessionIDFromClaim(claim)
	sandboxName := claim.GetBoundSandboxName()
	slog.Debug("SandboxClaim updated", "sessionID", sessionID, "bound", claim.IsBound(), "sandbox", sandboxName)

	r.cacheMu.Lock()
	r.claimCache[sessionID] = claim
	r.cacheMu.Unlock()

	r.checkAndNotifyClaim(sessionID, claim)
}

func (r *k8sRuntime) onClaimDelete(obj interface{}) {
	claim := r.unstructuredToClaim(obj)
	if claim == nil {
		if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
			claim = r.unstructuredToClaim(tombstone.Obj)
		}
	}
	if claim == nil {
		return
	}

	sessionID := r.getSessionIDFromClaim(claim)
	slog.Debug("SandboxClaim deleted", "sessionID", sessionID)

	r.cacheMu.Lock()
	delete(r.claimCache, sessionID)
	r.cacheMu.Unlock()
}

func (r *k8sRuntime) unstructuredToClaim(obj interface{}) *SandboxClaim {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return nil
	}

	data, err := u.MarshalJSON()
	if err != nil {
		slog.Warn("Failed to marshal unstructured claim", "error", err)
		return nil
	}

	var claim SandboxClaim
	if err := json.Unmarshal(data, &claim); err != nil {
		slog.Warn("Failed to unmarshal claim", "error", err)
		return nil
	}

	return &claim
}

func (r *k8sRuntime) getSessionIDFromClaim(claim *SandboxClaim) string {
	if id, ok := claim.Labels["netclode.io/session"]; ok {
		return id
	}
	// Fallback: extract from name
	name := claim.Name
	if strings.HasPrefix(name, "sess-") {
		return strings.TrimPrefix(name, "sess-")
	}
	return ""
}

func (r *k8sRuntime) checkAndNotifyClaim(sessionID string, claim *SandboxClaim) {
	r.callbacksMu.RLock()
	callback, ok := r.claimCallbacks[sessionID]
	r.callbacksMu.RUnlock()

	if !ok {
		return
	}

	if claim.IsBound() {
		// Remove callback before invoking to prevent double-call
		r.callbacksMu.Lock()
		delete(r.claimCallbacks, sessionID)
		r.callbacksMu.Unlock()

		callback(sessionID, claim.GetBoundSandboxName(), nil)
	} else if errMsg := claim.GetError(); errMsg != "" {
		r.callbacksMu.Lock()
		delete(r.claimCallbacks, sessionID)
		r.callbacksMu.Unlock()

		callback(sessionID, "", fmt.Errorf("claim error: %s", errMsg))
	}
}

// CreateSandboxClaim creates a claim to request a sandbox from the warm pool.
// templateName specifies which SandboxTemplate to use (e.g., "netclode-agent").
func (r *k8sRuntime) CreateSandboxClaim(ctx context.Context, sessionID string, templateName string) error {
	claim := &SandboxClaim{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "extensions.agents.x-k8s.io/v1alpha1",
			Kind:       "SandboxClaim",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      sandboxClaimName(sessionID),
			Namespace: r.namespace,
			Labels: map[string]string{
				"netclode.io/session": sessionID,
			},
		},
		Spec: SandboxClaimSpec{
			SandboxTemplateRef: SandboxTemplateRef{
				Name: templateName,
			},
		},
	}

	data, err := json.Marshal(claim)
	if err != nil {
		return fmt.Errorf("marshal claim: %w", err)
	}

	var u unstructured.Unstructured
	if err := json.Unmarshal(data, &u.Object); err != nil {
		return fmt.Errorf("convert to unstructured: %w", err)
	}

	_, err = r.dynamicClient.Resource(SandboxClaimGVR).Namespace(r.namespace).Create(ctx, &u, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("create claim: %w", err)
	}

	slog.Info("SandboxClaim created", "sessionID", sessionID, "template", templateName)
	return nil
}

// WaitForClaimBound waits for a SandboxClaim to be bound to a sandbox.
func (r *k8sRuntime) WaitForClaimBound(ctx context.Context, sessionID string, timeout time.Duration) (string, error) {
	// Check if already bound from cache
	r.cacheMu.RLock()
	claim, exists := r.claimCache[sessionID]
	r.cacheMu.RUnlock()

	if exists && claim.IsBound() {
		return claim.GetBoundSandboxName(), nil
	}

	// Setup callback channel
	resultCh := make(chan struct {
		sandboxName string
		err         error
	}, 1)

	r.callbacksMu.Lock()
	r.claimCallbacks[sessionID] = func(sid, name string, err error) {
		resultCh <- struct {
			sandboxName string
			err         error
		}{name, err}
	}
	r.callbacksMu.Unlock()

	// Cleanup callback on exit
	defer func() {
		r.callbacksMu.Lock()
		delete(r.claimCallbacks, sessionID)
		r.callbacksMu.Unlock()
	}()

	// Wait for result or timeout
	select {
	case result := <-resultCh:
		if result.err != nil {
			return "", result.err
		}
		slog.Info("SandboxClaim bound", "sessionID", sessionID, "sandbox", result.sandboxName)
		return result.sandboxName, nil
	case <-time.After(timeout):
		return "", fmt.Errorf("timeout waiting for claim %s to be bound", sandboxClaimName(sessionID))
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// GetSandboxByName retrieves a sandbox by its name.
func (r *k8sRuntime) GetSandboxByName(ctx context.Context, name string) (*Sandbox, error) {
	u, err := r.dynamicClient.Resource(SandboxGVR).Namespace(r.namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	sandbox := r.unstructuredToSandbox(u)
	if sandbox == nil {
		return nil, fmt.Errorf("failed to parse sandbox %s", name)
	}

	return sandbox, nil
}

// GetSessionIDByPodName finds the session ID for a sandbox by its pod name.
// This is used by warm pool agents which don't have session ID in their pod name.
// It searches sandboxes for one with the agents.x-k8s.io/pod-name annotation matching the given pod name.
func (r *k8sRuntime) GetSessionIDByPodName(ctx context.Context, podName string) (string, error) {
	// First check the cache for efficiency
	r.cacheMu.RLock()
	for sessionID, sandbox := range r.sandboxCache {
		if sandbox.Annotations != nil {
			if sandbox.Annotations["agents.x-k8s.io/pod-name"] == podName {
				r.cacheMu.RUnlock()
				return sessionID, nil
			}
		}
	}
	r.cacheMu.RUnlock()

	// If not in cache, list all sandboxes from K8s API
	list, err := r.dynamicClient.Resource(SandboxGVR).Namespace(r.namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", fmt.Errorf("list sandboxes: %w", err)
	}

	for _, item := range list.Items {
		sandbox := r.unstructuredToSandbox(&item)
		if sandbox == nil {
			continue
		}
		if sandbox.Annotations != nil && sandbox.Annotations["agents.x-k8s.io/pod-name"] == podName {
			// Found it - get session ID from label
			if sessionID, ok := sandbox.Labels["netclode.io/session"]; ok && sessionID != "" {
				return sessionID, nil
			}
		}
	}

	return "", fmt.Errorf("no sandbox found for pod %s", podName)
}

// GetSessionIDByPodIP finds the session ID for a sandbox by its pod IP.
// This is used by the secret-proxy to validate requests from sandbox pods.
// Security: NetworkPolicy ensures only sandbox pods can reach the secret-proxy,
// but we still validate that the IP belongs to a known session.
func (r *k8sRuntime) GetSessionIDByPodIP(ctx context.Context, podIP string) (string, error) {
	// List all pods in our namespace
	pods, err := r.clientset.CoreV1().Pods(r.namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", fmt.Errorf("list pods: %w", err)
	}

	// Find the pod with matching IP
	var podName string
	for _, pod := range pods.Items {
		if pod.Status.PodIP == podIP {
			podName = pod.Name
			break
		}
	}

	if podName == "" {
		return "", fmt.Errorf("no pod found with IP %s", podIP)
	}

	// Now use the existing method to get the session ID from the pod name
	return r.GetSessionIDByPodName(ctx, podName)
}

// LabelSandbox adds the netclode.io/session label to a sandbox so the informer can track it.
func (r *k8sRuntime) LabelSandbox(ctx context.Context, sandboxName string, sessionID string) error {
	patch := []byte(fmt.Sprintf(`{"metadata":{"labels":{"netclode.io/session":"%s"}}}`, sessionID))

	_, err := r.dynamicClient.Resource(SandboxGVR).Namespace(r.namespace).Patch(
		ctx, sandboxName, types.MergePatchType, patch, metav1.PatchOptions{},
	)
	if err != nil {
		return fmt.Errorf("patch sandbox: %w", err)
	}

	slog.Info("Sandbox labeled", "sandbox", sandboxName, "sessionID", sessionID)
	return nil
}

// DeleteSandboxClaim deletes a SandboxClaim.
func (r *k8sRuntime) DeleteSandboxClaim(ctx context.Context, sessionID string) error {
	name := sandboxClaimName(sessionID)

	err := r.dynamicClient.Resource(SandboxClaimGVR).Namespace(r.namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !errors.IsNotFound(err) {
		return err
	}

	slog.Info("SandboxClaim deleted", "sessionID", sessionID, "name", name)
	return nil
}

// ListSandboxClaims lists all SandboxClaims from cache.
func (r *k8sRuntime) ListSandboxClaims(ctx context.Context) ([]SandboxClaimInfo, error) {
	r.cacheMu.RLock()
	defer r.cacheMu.RUnlock()

	claims := make([]SandboxClaimInfo, 0, len(r.claimCache))
	for sessionID, claim := range r.claimCache {
		claims = append(claims, SandboxClaimInfo{
			SessionID:   sessionID,
			Bound:       claim.IsBound(),
			SandboxName: claim.GetBoundSandboxName(),
		})
	}

	return claims, nil
}

// ============================================================================
// VolumeSnapshot operations (for session snapshots)
// ============================================================================

const volumeSnapshotClassName = "juicefs-snapclass"

func snapshotName(sessionID, snapshotID string) string {
	return fmt.Sprintf("sess-%s-snap-%s", sessionID, snapshotID)
}

// CreateVolumeSnapshot creates a VolumeSnapshot from the session's PVC.
func (r *k8sRuntime) CreateVolumeSnapshot(ctx context.Context, sessionID, snapshotID string) error {
	// Get the PVC name for this session
	pvcName, err := r.GetPVCName(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("get PVC name: %w", err)
	}

	name := snapshotName(sessionID, snapshotID)
	className := volumeSnapshotClassName

	snapshot := &VolumeSnapshot{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "snapshot.storage.k8s.io/v1",
			Kind:       "VolumeSnapshot",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: r.namespace,
			Labels: map[string]string{
				"netclode.io/session":  sessionID,
				"netclode.io/snapshot": snapshotID,
			},
		},
		Spec: VolumeSnapshotSpec{
			VolumeSnapshotClassName: &className,
			Source: VolumeSnapshotSource{
				PersistentVolumeClaimName: &pvcName,
			},
		},
	}

	data, err := json.Marshal(snapshot)
	if err != nil {
		return fmt.Errorf("marshal snapshot: %w", err)
	}

	var u unstructured.Unstructured
	if err := json.Unmarshal(data, &u.Object); err != nil {
		return fmt.Errorf("convert to unstructured: %w", err)
	}

	_, err = r.dynamicClient.Resource(VolumeSnapshotGVR).Namespace(r.namespace).Create(ctx, &u, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("create volume snapshot: %w", err)
	}

	slog.Info("VolumeSnapshot created", "sessionID", sessionID, "snapshotID", snapshotID, "name", name, "pvc", pvcName)
	return nil
}

// WaitForSnapshotReady waits for a VolumeSnapshot to be ready.
func (r *k8sRuntime) WaitForSnapshotReady(ctx context.Context, sessionID, snapshotID string, timeout time.Duration) error {
	name := snapshotName(sessionID, snapshotID)
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		u, err := r.dynamicClient.Resource(VolumeSnapshotGVR).Namespace(r.namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("get snapshot: %w", err)
		}

		data, err := u.MarshalJSON()
		if err != nil {
			return fmt.Errorf("marshal snapshot: %w", err)
		}

		var snapshot VolumeSnapshot
		if err := json.Unmarshal(data, &snapshot); err != nil {
			return fmt.Errorf("unmarshal snapshot: %w", err)
		}

		if snapshot.IsReady() {
			slog.Info("VolumeSnapshot ready", "sessionID", sessionID, "snapshotID", snapshotID)
			return nil
		}

		if errMsg := snapshot.GetError(); errMsg != "" {
			return fmt.Errorf("snapshot error: %s", errMsg)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
			// Poll again
		}
	}

	return fmt.Errorf("timeout waiting for snapshot %s to be ready", name)
}

// DeleteVolumeSnapshot deletes a VolumeSnapshot.
func (r *k8sRuntime) DeleteVolumeSnapshot(ctx context.Context, sessionID, snapshotID string) error {
	name := snapshotName(sessionID, snapshotID)

	err := r.dynamicClient.Resource(VolumeSnapshotGVR).Namespace(r.namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("delete snapshot: %w", err)
	}

	slog.Info("VolumeSnapshot deleted", "sessionID", sessionID, "snapshotID", snapshotID)
	return nil
}

// ListVolumeSnapshots lists all VolumeSnapshots for a session.
func (r *k8sRuntime) ListVolumeSnapshots(ctx context.Context, sessionID string) ([]VolumeSnapshotInfo, error) {
	list, err := r.dynamicClient.Resource(VolumeSnapshotGVR).Namespace(r.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("netclode.io/session=%s", sessionID),
	})
	if err != nil {
		return nil, fmt.Errorf("list snapshots: %w", err)
	}

	snapshots := make([]VolumeSnapshotInfo, 0, len(list.Items))
	for _, item := range list.Items {
		data, err := item.MarshalJSON()
		if err != nil {
			continue
		}

		var snapshot VolumeSnapshot
		if err := json.Unmarshal(data, &snapshot); err != nil {
			continue
		}

		snapshotID := snapshot.Labels["netclode.io/snapshot"]
		info := VolumeSnapshotInfo{
			Name:       snapshot.Name,
			SessionID:  sessionID,
			SnapshotID: snapshotID,
			Ready:      snapshot.IsReady(),
			Error:      snapshot.GetError(),
		}
		if snapshot.Status != nil {
			info.CreationTime = snapshot.Status.CreationTime
		}
		snapshots = append(snapshots, info)
	}

	return snapshots, nil
}

// RestoreFromSnapshot prepares a session for restore by cleaning up existing resources.
// Returns the old PVC name so it can be deleted after the restore completes.
// The actual PVC restore happens when CreateSandbox is called with ExistingPVCEnvKey.
func (r *k8sRuntime) RestoreFromSnapshot(ctx context.Context, sessionID, snapshotID string) (string, error) {
	snapName := snapshotName(sessionID, snapshotID)

	// Verify snapshot exists and is ready
	u, err := r.dynamicClient.Resource(VolumeSnapshotGVR).Namespace(r.namespace).Get(ctx, snapName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("get snapshot: %w", err)
	}

	data, err := u.MarshalJSON()
	if err != nil {
		return "", fmt.Errorf("marshal snapshot: %w", err)
	}

	var snapshot VolumeSnapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return "", fmt.Errorf("unmarshal snapshot: %w", err)
	}

	if !snapshot.IsReady() {
		return "", fmt.Errorf("snapshot %s is not ready", snapName)
	}

	slog.Info("Preparing restore from snapshot", "sessionID", sessionID, "snapshotID", snapshotID)

	// Get old PVC name before deleting sandbox (needed for cleanup after restore).
	// The PVC survives sandbox deletion because the session anchor ConfigMap is a second owner.
	// JuiceFS snapshots reference the source subvolume data, so we need the old PVC to stay
	// alive until the restore completes. It will be explicitly deleted later via DeletePVCByName.
	oldPVCName, _ := r.GetPVCName(ctx, sessionID)

	// Delete sandbox claim if using warm pool
	if r.config.UseWarmPool {
		if err := r.DeleteSandboxClaim(ctx, sessionID); err != nil {
			slog.Warn("Failed to delete sandbox claim", "sessionID", sessionID, "error", err)
		}
	}
	// Also try to delete direct sandbox
	if err := r.DeleteSandbox(ctx, sessionID); err != nil && !errors.IsNotFound(err) {
		slog.Warn("Failed to delete sandbox", "sessionID", sessionID, "error", err)
	}

	// Wait for sandbox to actually be deleted (poll instead of blind sleep)
	if err := r.waitForSandboxDeletion(ctx, sessionID, 30*time.Second); err != nil {
		slog.Warn("Timeout waiting for sandbox deletion, proceeding anyway", "sessionID", sessionID, "error", err)
	}

	slog.Info("Ready for restore", "sessionID", sessionID, "snapshotID", snapshotID, "oldPVC", oldPVCName)
	return oldPVCName, nil
}

// CreatePVCFromSnapshot creates a standalone PVC from a VolumeSnapshot.
// This must be done BEFORE creating the sandbox so the restore job can complete
// before the pod tries to mount the volume.
func (r *k8sRuntime) CreatePVCFromSnapshot(ctx context.Context, sessionID, snapshotID string) (string, error) {
	pvcName := fmt.Sprintf("agent-home-sess-%s", sessionID)
	snapName := snapshotName(sessionID, snapshotID)

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pvcName,
			Namespace: r.namespace,
			Labels: map[string]string{
				"netclode.io/session": sessionID,
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: strPtr("juicefs-sc"),
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse("10Gi"),
				},
			},
			DataSource: &corev1.TypedLocalObjectReference{
				APIGroup: strPtr("snapshot.storage.k8s.io"),
				Kind:     "VolumeSnapshot",
				Name:     snapName,
			},
		},
	}

	_, err := r.clientset.CoreV1().PersistentVolumeClaims(r.namespace).Create(ctx, pvc, metav1.CreateOptions{})
	if err != nil && !errors.IsAlreadyExists(err) {
		return "", fmt.Errorf("create PVC from snapshot: %w", err)
	}

	slog.Info("Created PVC from snapshot", "sessionID", sessionID, "pvc", pvcName, "snapshot", snapName)
	return pvcName, nil
}

// strPtr returns a pointer to a string
func strPtr(s string) *string {
	return &s
}

// WaitForRestoreJob waits for the JuiceFS restore job to complete.
// JuiceFS CSI creates a restore job when a PVC is created from a snapshot.
// The job name follows the pattern: juicefs-restore-snapshot-{volumesnapshotcontent-uid}
func (r *k8sRuntime) WaitForRestoreJob(ctx context.Context, sessionID, snapshotID string, timeout time.Duration) error {
	snapName := snapshotName(sessionID, snapshotID)

	// Get the VolumeSnapshot to find its bound VolumeSnapshotContent
	u, err := r.dynamicClient.Resource(VolumeSnapshotGVR).Namespace(r.namespace).Get(ctx, snapName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get snapshot %s: %w", snapName, err)
	}

	data, err := u.MarshalJSON()
	if err != nil {
		return fmt.Errorf("marshal snapshot: %w", err)
	}

	var snapshot VolumeSnapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return fmt.Errorf("unmarshal snapshot: %w", err)
	}

	if snapshot.Status == nil || snapshot.Status.BoundVolumeSnapshotContentName == nil {
		return fmt.Errorf("snapshot %s has no bound VolumeSnapshotContent", snapName)
	}

	// Extract the UID from the VolumeSnapshotContent name (snapcontent-{uid})
	contentName := *snapshot.Status.BoundVolumeSnapshotContentName
	uid := strings.TrimPrefix(contentName, "snapcontent-")
	if uid == contentName {
		// Not the expected format, try using the full name
		uid = contentName
	}

	// JuiceFS restore job name follows pattern: juicefs-restore-snapshot-{uid}
	jobName := fmt.Sprintf("juicefs-restore-snapshot-%s", uid)

	slog.Info("Waiting for JuiceFS restore job", "sessionID", sessionID, "snapshotID", snapshotID, "jobName", jobName)

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		job, err := r.clientset.BatchV1().Jobs("kube-system").Get(ctx, jobName, metav1.GetOptions{})
		if err != nil {
			if errors.IsNotFound(err) {
				// Job might not exist yet, wait and retry
				time.Sleep(500 * time.Millisecond)
				continue
			}
			return fmt.Errorf("get restore job %s: %w", jobName, err)
		}

		// Check job status
		if job.Status.Succeeded > 0 {
			slog.Info("JuiceFS restore job completed successfully", "sessionID", sessionID, "jobName", jobName)
			return nil
		}

		if job.Status.Failed > 0 && job.Spec.BackoffLimit != nil && job.Status.Failed >= *job.Spec.BackoffLimit {
			return fmt.Errorf("restore job %s failed after %d attempts", jobName, job.Status.Failed)
		}

		slog.Debug("Restore job still running", "sessionID", sessionID, "jobName", jobName, "active", job.Status.Active, "succeeded", job.Status.Succeeded, "failed", job.Status.Failed)
		time.Sleep(500 * time.Millisecond)
	}

	return fmt.Errorf("timeout waiting for restore job %s", jobName)
}

// GetPVCName returns the PVC name for a session.
// For warm pool mode, we need to look up the actual PVC name from the sandbox.
func (r *k8sRuntime) GetPVCName(ctx context.Context, sessionID string) (string, error) {
	// First check the cache
	r.cacheMu.RLock()
	sandbox, exists := r.sandboxCache[sessionID]
	r.cacheMu.RUnlock()

	if !exists {
		// Try to get the sandbox directly
		name := sandboxName(sessionID)
		u, err := r.dynamicClient.Resource(SandboxGVR).Namespace(r.namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			// Try warm pool sandbox name lookup via claim
			if r.config.UseWarmPool {
				r.cacheMu.RLock()
				claim, claimExists := r.claimCache[sessionID]
				r.cacheMu.RUnlock()
				if claimExists && claim.IsBound() {
					sandboxName := claim.GetBoundSandboxName()
					u, err = r.dynamicClient.Resource(SandboxGVR).Namespace(r.namespace).Get(ctx, sandboxName, metav1.GetOptions{})
					if err != nil {
						return "", fmt.Errorf("get sandbox %s: %w", sandboxName, err)
					}
				} else {
					return "", fmt.Errorf("no bound sandbox for session %s", sessionID)
				}
			} else {
				return "", fmt.Errorf("get sandbox: %w", err)
			}
		}
		sandbox = r.unstructuredToSandbox(u)
		if sandbox == nil {
			return "", fmt.Errorf("failed to parse sandbox")
		}
	}

	// The PVC name follows the pattern: {volumeClaimTemplate.name}-{pod.name}
	// From sandbox-template.yaml, the volume claim template name is "agent-home"
	// For warm pool mode, the pod name comes from the pool and is stored in annotations
	podName := sandbox.Name
	if annotatedPodName, ok := sandbox.Annotations["agents.x-k8s.io/pod-name"]; ok && annotatedPodName != "" {
		podName = annotatedPodName
	}
	return fmt.Sprintf("agent-home-%s", podName), nil
}

// mustParseQuantity parses a resource quantity, panicking on error (for static values)
func mustParseQuantity(s string) corev1.ResourceList {
	q, err := resource.ParseQuantity(s)
	if err != nil {
		panic(fmt.Sprintf("invalid quantity %q: %v", s, err))
	}
	return corev1.ResourceList{corev1.ResourceStorage: q}
}

// VerifyAgentToken validates a Kubernetes ServiceAccount token and returns the pod name.
// This is used to verify the identity of agents connecting to the control plane,
// preventing rogue agents from impersonating legitimate ones.
// If audiences is non-empty, the token is validated against those specific audiences.
// If audiences is empty/nil, the token is validated against the default API server audiences.
func (r *k8sRuntime) VerifyAgentToken(ctx context.Context, token string, audiences []string) (string, error) {
	review := &authenticationv1.TokenReview{
		Spec: authenticationv1.TokenReviewSpec{
			Token:     token,
			Audiences: audiences, // nil/empty means use default K8s audiences
		},
	}

	result, err := r.clientset.AuthenticationV1().TokenReviews().Create(ctx, review, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("token review failed: %w", err)
	}

	if !result.Status.Authenticated {
		return "", fmt.Errorf("token not authenticated: %s", result.Status.Error)
	}

	// Extract pod name from the token's extra claims
	// The projected service account token includes kubernetes.io/pod/name
	podNames, ok := result.Status.User.Extra["authentication.kubernetes.io/pod-name"]
	if !ok || len(podNames) == 0 {
		return "", fmt.Errorf("pod name not found in token claims")
	}

	podName := podNames[0]

	// Verify the pod exists in our namespace
	_, err = r.clientset.CoreV1().Pods(r.namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("pod %s not found in namespace %s: %w", podName, r.namespace, err)
	}

	slog.Debug("Agent token verified", "podName", podName)
	return podName, nil
}

// Ensure k8sRuntime implements Runtime
var _ Runtime = (*k8sRuntime)(nil)
