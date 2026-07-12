# Kubernetes Infrastructure for Netclode

## Overview

This directory contains Kubernetes manifests for deploying the Netclode agent sandbox infrastructure.

## Kubeconfig Setup

**IMPORTANT:** Always use explicit contexts to avoid deploying to the wrong cluster.

### 1. Copy k3s config from the netclode host

```bash
# On the netclode host (e.g., via SSH)
cat /etc/rancher/k3s/k3s.yaml

# Copy the output and save locally, replacing the server address
# Change: server: https://127.0.0.1:6443
# To:     server: https://<netclode-host>:6443
```

### 2. Configure kubeconfig with named contexts

```bash
# Backup existing config
cp ~/.kube/config ~/.kube/config.backup

# Create a merged config with explicit contexts
# Option A: Use KUBECONFIG env var to merge
export KUBECONFIG=~/.kube/config:~/.kube/netclode.yaml
kubectl config view --flatten > ~/.kube/config.merged
mv ~/.kube/config.merged ~/.kube/config

# Option B: Manually add the netclode context
kubectl config set-cluster netclode --server=https://<netclode-host>:6443 --certificate-authority=...
kubectl config set-credentials netclode-admin --client-certificate=... --client-key=...
kubectl config set-context netclode --cluster=netclode --user=netclode-admin
```

### 3. Rename your current context to 'silo' (or appropriate name)

```bash
# Check current context name
kubectl config current-context

# Rename it (e.g., if it's "default")
kubectl config rename-context default silo
```

### 4. Disable default context (require explicit --context)

```bash
# Unset the current context - kubectl will error without --context flag
kubectl config unset current-context
```

### 5. Usage

```bash
# Always specify context explicitly
kubectl --context=netclode get pods -n netclode
kubectl --context=silo get pods

# Or set for current shell session only
export KUBECTL_CONTEXT=netclode
kubectl get pods -n netclode  # uses $KUBECTL_CONTEXT
```

## Components

### Agent Sandbox Controller

The agent-sandbox-controller manages Sandbox, SandboxClaim, SandboxTemplate, and SandboxWarmPool CRDs.

**Files:**
- `extensions.controller.yaml` - StatefulSet for the controller
- `extensions.yaml` - ClusterRoleBindings
- `extensions-rbac.generated.yaml` - ClusterRole for extensions controller
- `rbac.generated.yaml` - ClusterRole for core controller

**Custom Image:**
We use a custom-built controller image (`ghcr.io/angristan/agent-sandbox-controller:volumeclaim-v7`)
that includes:
- volumeClaimTemplates support for SandboxTemplate
- Fix for PVC explosion bug in warm pools (see below)
- PVC adoption: when SandboxClaim adopts a warm pool pod, it also adopts its PVCs

### Warm Pool

SandboxWarmPool keeps pre-warmed pods with JuiceFS PVCs ready for instant allocation.

**Files:**
- `sandbox-warmpool.yaml` - SandboxWarmPool resource
- `sandbox-template.yaml` - SandboxTemplate with volumeClaimTemplates

**Control-Plane Configuration:**

To enable warm pool allocation in the control-plane, set the environment variable:

```yaml
env:
  - name: WARM_POOL_ENABLED
    value: "true"
```

When enabled, the control-plane creates `SandboxClaim` resources instead of direct `Sandbox` resources. The controller assigns a pre-warmed pod from the pool (or creates a new one if the pool is empty).

**Session Assignment:**

Since warm pool pods are already running, they cannot receive per-session environment variables dynamically. Instead, agents connect via gRPC and authenticate using their Kubernetes ServiceAccount token:

1. Agent reads SA token from `/var/run/secrets/kubernetes.io/serviceaccount/token`
2. Agent connects to control-plane via gRPC, sending the token in registration
3. Control-plane validates token via Kubernetes TokenReview API (extracts verified pod name)
4. When SandboxClaim binds to this pod, control-plane pushes `SessionAssigned` message

This prevents rogue agents from impersonating legitimate pods - identity is cryptographically verified.

### Storage

**Files:**
- `storage.yaml` - JuiceFS StorageClass

**Requirements:**
- JuiceFS CSI driver must be installed: `helm install juicefs-csi juicefs/juicefs-csi-driver -n kube-system`
- `juicefs-secret` must exist in the netclode namespace with valid Redis metadata URL

### Runtime

**Files:**
- `runtime-class.yaml` - RuntimeClass for Kata Containers (kata-clh)

## Deployment Order

**Always use `--context=netclode`** to ensure you're deploying to the correct cluster.

```bash
# Set context for all commands (or add --context=netclode to each)
CTX="--context=netclode"

# 1. Create namespaces
kubectl $CTX apply -f namespace.yaml

# 2. Install CRDs
kubectl $CTX apply -f agents.x-k8s.io_sandboxes.yaml
kubectl $CTX apply -f extensions.agents.x-k8s.io_sandboxclaims.yaml
kubectl $CTX apply -f extensions.agents.x-k8s.io_sandboxtemplates.yaml
kubectl $CTX apply -f extensions.agents.x-k8s.io_sandboxwarmpools.yaml

# 3. Install RBAC
kubectl $CTX apply -f rbac.generated.yaml
kubectl $CTX apply -f extensions-rbac.generated.yaml
kubectl $CTX apply -f extensions.yaml

# 4. Install controller
kubectl $CTX apply -f extensions.controller.yaml

# 5. Install runtime and storage prerequisites
kubectl $CTX apply -f runtime-class.yaml
kubectl $CTX apply -f storage.yaml
kubectl $CTX apply -f juicefs-config.yaml
kubectl $CTX rollout restart statefulset juicefs-csi-controller -n kube-system

# 6. Deploy sandbox template and warm pool
kubectl $CTX apply -f sandbox-template.yaml
kubectl $CTX apply -f sandbox-warmpool.yaml
```

## Cleanup

To remove all netclode k8s resources:

```bash
CTX="--context=netclode"

# Delete namespace (removes controller, serviceaccount, etc.)
kubectl $CTX delete ns agent-sandbox-system

# Delete CRDs
kubectl $CTX delete crd sandboxclaims.extensions.agents.x-k8s.io
kubectl $CTX delete crd sandboxes.agents.x-k8s.io
kubectl $CTX delete crd sandboxtemplates.extensions.agents.x-k8s.io
kubectl $CTX delete crd sandboxwarmpools.extensions.agents.x-k8s.io

# Delete ClusterRoles and ClusterRoleBindings
kubectl $CTX delete clusterrolebinding agent-sandbox-controller agent-sandbox-controller-extensions
kubectl $CTX delete clusterrole agent-sandbox-controller agent-sandbox-controller-extensions

# Delete RuntimeClass and StorageClass
kubectl $CTX delete runtimeclass kata-clh
kubectl $CTX delete sc juicefs-sc

# Delete any orphaned PVs (if PVC explosion occurred)
kubectl $CTX get pv --no-headers | grep Released | awk '{print $1}' | xargs kubectl $CTX delete pv
```

## Known Issues and Learnings

### PVC Preservation During Pause (Session Anchors)

**Problem:** When a session is paused, the Sandbox CR is deleted. PVCs created via `volumeClaimTemplates` have an `ownerReference` to the Sandbox, so Kubernetes garbage-collects them, causing **data loss**.

**Solution:** The control-plane creates a "session anchor" ConfigMap that acts as a second owner of the PVC. With two owners (Sandbox + ConfigMap), the PVC survives when the Sandbox is deleted during pause. The PVC only gets GC'd when both owners are deleted.

**How it works:**
1. When a session is created, control-plane creates `ConfigMap/session-anchor-<sessionID>`
2. The ConfigMap is added as a non-controller `ownerReference` on the PVC
3. When paused: Sandbox deleted → PVC survives (anchor still owns it)
4. When resumed: New Sandbox created with same PVC
5. When deleted: Anchor deleted → PVC explicitly deleted

**RBAC Requirements:**
The control-plane service account needs permissions for ConfigMaps and PVC updates:
```yaml
# In namespace.yaml Role sandbox-manager
- apiGroups: [""]
  resources: ["configmaps"]
  verbs: ["get", "list", "watch", "create", "update", "delete"]
- apiGroups: [""]
  resources: ["persistentvolumeclaims"]
  verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
```

**Verifying anchors:**
```bash
# List session anchors
kubectl --context netclode -n netclode get configmap -l netclode.dev/component=session-anchor

# Check PVC ownership (should show both Sandbox and ConfigMap)
kubectl --context netclode -n netclode get pvc <pvc-name> -o jsonpath='{.metadata.ownerReferences}' | jq
```

### PVC Explosion Bug (Fixed in volumeclaim-v6)

**Problem:** The warm pool controller was creating thousands of PVCs in a loop.

**Root Cause:** The controller watches PVCs it owns (`Owns(&corev1.PersistentVolumeClaim{})`).
When a PVC is created:
1. PVC creation triggers a reconcile
2. The reconcile runs before the pod is created
3. Reconcile sees 0 pods, thinks it needs to create one
4. Creates a NEW PVC (with new random suffix) and pod
5. New PVC triggers another reconcile... infinite loop

**Fix:** Before creating new pods, count owned PVCs and compare to pod count.
If `ownedPVCs > currentPods`, a creation is in progress - skip creating more.

```go
// If there are more PVCs than pods, a creation is in progress
creationInProgress := ownedPVCs > currentReplicas
if currentReplicas < desiredReplicas && !creationInProgress {
    // Safe to create new pods
}
```

### JuiceFS Configuration

The JuiceFS secret must have a valid `metaurl` pointing to an accessible Redis server.
`redis://localhost:6379` will NOT work from inside pods.

Example working secret:
```yaml
apiVersion: v1
kind: Secret
metadata:
  name: juicefs-secret
  namespace: netclode
stringData:
  name: netclode-vol
  metaurl: redis://<redis-host>:6379/0
  storage: s3
  bucket: <bucket-url>
  access-key: <access-key>
  secret-key: <secret-key>
```

#### JuiceFS cache size

JuiceFS mounts use the upstream defaults unless we override them. The client read cache size defaults to `--cache-size=102400` (MiB, ~100 GiB) and also respects `--free-space-ratio=0.1`, so cache will shrink if disk space is tight. To change it for all PVCs, add `mountOptions` in the CSI driver ConfigMap (`infra/k8s/juicefs-config.yaml`) and restart the CSI controller so new mount pods pick it up:

```yaml
data:
  config.yaml: |
    mountOptions:
      - cache-size=204800
      - free-space-ratio=0.1
```

### Kata Containers

The sandbox pods use `runtimeClassName: kata-clh` for VM-level isolation.
Ensure Kata Containers is installed on the cluster nodes.

### Warm Pool Pods Stuck in Pending

If warm pool pods stay in Pending state, check:

1. **Insufficient CPU** - Check `kubectl describe node` for allocated resources
   - JuiceFS mount pods request 1 CPU by default (see `storage.yaml` for fix)
   - On small nodes, scale down coredns: `kubectl scale deployment coredns -n kube-system --replicas=2`

2. **Unbound PVCs** - The scheduler may fail before PVC is bound
   - Check PVC status: `kubectl get pvc -n netclode`
   - If PVC is Bound but pod is Pending, delete pod to trigger reschedule

3. **Orphaned PVCs blocking controller** - If `ownedPVCs > currentPods`, controller thinks creation is in progress
   - Check controller logs: `kubectl logs -n agent-sandbox-system agent-sandbox-controller-0`
   - Delete orphaned PVCs: `kubectl delete pvc -n netclode -l agents.x-k8s.io/pool`

4. **JuiceFS delvol jobs consuming CPU** - When PVCs are deleted, JuiceFS creates cleanup jobs
   - These jobs request 1 CPU each and can exhaust node resources
   - Clean up stuck jobs: `kubectl get jobs -n kube-system -o name | grep delvol | xargs kubectl delete -n kube-system`

### Small Node Configuration (2 CPU)

On a 2-CPU node, resource management is critical:

```bash
CTX="--context=netclode"

# Scale down coredns (default 5 replicas is too many)
kubectl $CTX scale deployment coredns -n kube-system --replicas=2

# Configure JuiceFS mount pods to use less CPU (see storage.yaml for details)
# Default is 1 CPU per mount pod - with multiple PVCs this exhausts the node
```

### Warm Pool Troubleshooting

**Warm pool not being used:**
- Verify `WARM_POOL_ENABLED=true` is set in control-plane deployment
- Check control-plane logs for "warmPool=true" at startup
- Verify SandboxWarmPool has ready replicas: `kubectl get sandboxwarmpool -n netclode`

**Claims not binding:**
- Check SandboxClaim status: `kubectl get sandboxclaim -n netclode`
- Check controller logs: `kubectl logs -n agent-sandbox-system agent-sandbox-controller-0`
- Verify warm pool has available pods: `kubectl get pods -n netclode -l agents.x-k8s.io/pool`

**Agent not connecting:**
- Verify agent can reach its isolated listener: `curl http://control-plane-agent.netclode.svc.cluster.local:3001/health`

## Control Plane Exposure

The control plane is exposed via Tailscale Ingress with HTTPS and automatic Let's Encrypt certificates.
The client listener intentionally has no second application credential: Tailscale identity and ACLs
are the authentication boundary. Restrict the ingress destination to trusted user/device identities;
do not grant the Netclode host identity or sandbox identities access to it.

### Architecture

The iOS app uses URLSession which only supports HTTP/2 over HTTPS. HTTP/2 is required for
bidirectional streaming (Connect protocol). The setup uses:

- **Tailscale Ingress** (`control-plane-ingress.yaml`) - Exposes the control-plane on the tailnet
- **Custom proxy image** (`ghcr.io/angristan/tailscale:connect-fix`) - Adds h2c support for Connect protocol content types

The custom proxy image patches Tailscale's reverse proxy to enable h2c (HTTP/2 cleartext) for
Connect RPC content types (`application/connect+proto`, `application/connect+json`), not just gRPC.

### Configuration

The Tailscale operator is deployed via Ansible and configured in `infra/ansible/roles/tailscale-operator/`.
It uses OAuth credentials from `/var/secrets/ts-oauth-client-id` and `/var/secrets/ts-oauth-client-secret`.

### Accessing the Control Plane

After deployment, the control plane will be available at:
`https://netclode-control-plane-ingress.YOUR-TAILNET.ts.net`

### Finding Your Tailnet Name

```bash
# From any machine with Tailscale installed
tailscale status

# Or check the Tailscale admin console
# https://login.tailscale.com/admin/machines
```

### Verifying the Connection

```bash
# Test HTTPS endpoint
curl -v https://netclode-control-plane-ingress.YOUR-TAILNET.ts.net/health

# Check the ingress status
kubectl --context netclode -n netclode describe ingress control-plane
```
