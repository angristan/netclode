package k8s

import (
	"context"
	"time"

	"github.com/angristan/netclode/services/control-plane/internal/config"
)

// Runtime defines the interface for Kubernetes operations.
type Runtime interface {
	// Direct Sandbox operations (legacy mode)
	// resources is optional - if nil, uses global defaults from Kata config
	CreateSandbox(ctx context.Context, sessionID string, env map[string]string, resources *SandboxResourceConfig) error
	WaitForReady(ctx context.Context, sessionID string, timeout time.Duration) (serviceFQDN string, err error)
	WatchSandboxReady(sessionID string, callback SandboxReadyCallback)
	GetStatus(ctx context.Context, sessionID string) (*SandboxStatusInfo, error)
	DeleteSandbox(ctx context.Context, sessionID string) error
	DeletePVC(ctx context.Context, sessionID string) error
	DeletePVCByName(ctx context.Context, pvcName string) error
	DeleteSecret(ctx context.Context, sessionID string) error
	ListSandboxes(ctx context.Context) ([]SandboxInfo, error)

	// Session anchor ConfigMap - prevents PVC from being garbage-collected when Sandbox is deleted.
	// The ConfigMap acts as a second owner of the PVC, so the PVC survives pause/resume cycles.
	EnsureSessionAnchor(ctx context.Context, sessionID string) error
	DeleteSessionAnchor(ctx context.Context, sessionID string) error
	AddSessionAnchorToPVC(ctx context.Context, sessionID, pvcName string) error

	// SandboxClaim operations (warm pool mode)
	CreateSandboxClaim(ctx context.Context, sessionID string, templateName string) error
	WaitForClaimBound(ctx context.Context, sessionID string, timeout time.Duration) (sandboxName string, err error)
	GetSandboxByName(ctx context.Context, name string) (*Sandbox, error)
	GetSessionIDByPodName(ctx context.Context, podName string) (string, error)
	GetSessionIDByPodIP(ctx context.Context, podIP string) (string, error)
	LabelSandbox(ctx context.Context, sandboxName string, sessionID string) error
	DeleteSandboxClaim(ctx context.Context, sessionID string) error
	ListSandboxClaims(ctx context.Context) ([]SandboxClaimInfo, error)

	// Service operations (for Tailscale preview URLs)
	CreateSandboxService(ctx context.Context, sessionID string) error
	DeleteSandboxService(ctx context.Context, sessionID string) error
	ListTailscaleServices(ctx context.Context) ([]string, error) // Returns session IDs with ts-* services
	ExposePort(ctx context.Context, sessionID string, port int) error
	UnexposePort(ctx context.Context, sessionID string, port int) error
	GetSandboxPreviewHostname(ctx context.Context, sessionID string) (string, error)

	// Network policy operations
	ConfigureNetwork(ctx context.Context, sessionID string, networkEnabled bool) error
	ConfigureTailnetAccess(ctx context.Context, sessionID string, tailnetEnabled bool) error
	DeleteNetworkRestriction(ctx context.Context, sessionID string) error

	// VolumeSnapshot operations (for session snapshots)
	CreateVolumeSnapshot(ctx context.Context, sessionID, snapshotID string) error
	WaitForSnapshotReady(ctx context.Context, sessionID, snapshotID string, timeout time.Duration) error
	DeleteVolumeSnapshot(ctx context.Context, sessionID, snapshotID string) error
	ListVolumeSnapshots(ctx context.Context, sessionID string) ([]VolumeSnapshotInfo, error)
	RestoreFromSnapshot(ctx context.Context, sessionID, snapshotID string) (oldPVCName string, err error)
	CreatePVCFromSnapshot(ctx context.Context, sessionID, snapshotID string) (pvcName string, err error)
	WaitForRestoreJob(ctx context.Context, sessionID, snapshotID string, timeout time.Duration) error
	GetPVCName(ctx context.Context, sessionID string) (string, error)

	// Agent authentication
	// VerifyAgentToken validates a Kubernetes ServiceAccount token and returns the pod name.
	// This is used to verify the identity of agents connecting to the control plane.
	// If audiences is non-empty, the token is validated against those specific audiences.
	// If audiences is empty/nil, the token is validated against the default API server audiences.
	VerifyAgentToken(ctx context.Context, token string, audiences []string) (podName string, err error)

	Close()
}

// NewRuntime creates a new Kubernetes runtime with informer-based watching.
func NewRuntime(cfg *config.Config) (Runtime, error) {
	return newK8sRuntime(cfg)
}
