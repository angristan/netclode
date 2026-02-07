# Deployment

Here's how to get Netclode running on your own server.

## Prerequisites

- Linux machine with nested virtualization (2 vCPU, 8GB RAM minimum)
- S3-compatible storage backend:
  - External provider (DigitalOcean Spaces, Cloudflare R2, etc.), or
  - Self-hosted MinIO on the same server (optional, can be auto-configured)
- Tailscale account
- LLM credentials:
  - At least one API key (Anthropic, OpenAI, Mistral, etc.) for non-Codex providers, or
  - Local CLI Codex OAuth login (`netclode auth codex`) for Codex `:oauth` sessions without API keys
- Ansible installed locally

## 1. Clone the repo

```bash
git clone https://github.com/angristan/netclode.git
cd netclode
```

## 2. Provision a server

Requirements:

- Debian or Ubuntu
- Nested virtualization support
- 2+ vCPU, 8GB+ RAM

## 3. Setup server access

SSH into your server and:

1. Add your SSH public key to `~/.ssh/authorized_keys`
2. Install Tailscale: `curl -fsSL https://tailscale.com/install.sh | sh`
3. Connect to your tailnet: `tailscale up --ssh`

Your server is now accessible via its Tailscale hostname (e.g., `my-server`).

## 4. Configure Tailscale for k8s ingress

1. Create an [OAuth client](https://login.tailscale.com/admin/settings/oauth) with these scopes:
   - `General` -> `Services`
   - `Devices` -> `Core`
   - `Keys` -> `Auth Keys`
2. Allow these tags for the OAuth client:
   - `tag:k8s-operator`
   - `tag:k8s`
3. Ensure both tags exist in your tailnet policy and `tag:k8s-operator` can own `tag:k8s`.
4. Enable [MagicDNS](https://login.tailscale.com/admin/dns).
5. Enable tailnet HTTPS certificates (DNS page -> HTTPS certificates). Without this, the ingress proxy cannot serve on `:443`.

## 5. Configure secrets

Create `.env` at the repo root:

```bash
# LLM credentials (choose one path - see docs/sdk-support.md)
# Path A: API key(s)
# ANTHROPIC_API_KEY=sk-ant-api03-xxx
# OPENAI_API_KEY=sk-xxx
# MISTRAL_API_KEY=xxx
#
# Optional: required only if using Codex OAuth sessions
# 32-byte base64 key for encrypting session OAuth refresh tokens at rest
# CODEX_OAUTH_ENCRYPTION_KEY_B64=$(openssl rand -base64 32)

# Tailscale (OAuth client from step 4)
TS_OAUTH_CLIENT_ID=your-oauth-client-id
TS_OAUTH_CLIENT_SECRET=your-oauth-client-secret

# Option A: external S3-compatible storage (manual)
DO_SPACES_ACCESS_KEY=your-spaces-access-key
DO_SPACES_SECRET_KEY=your-spaces-secret-key
JUICEFS_BUCKET=https://fra1.digitaloceanspaces.com/your-bucket

# Option B: self-host MinIO on the same VPS (automatic)
# MINIO_ENABLED=true
# MINIO_BUCKET_NAME=netclode-juicefs
# MINIO_API_PORT=9000

# JuiceFS metadata (optional - default shown)
JUICEFS_META_URL=redis://redis-juicefs.netclode.svc.cluster.local:6379/0

# Deployment target (Tailscale hostname from step 3)
DEPLOY_HOST=my-server

# GitHub App (optional - for repo picker)
GITHUB_APP_ID=123456
GITHUB_APP_PRIVATE_KEY_B64=base64-encoded-pem-private-key
GITHUB_INSTALLATION_ID=12345678
```

Storage notes:
- If using external S3, create a bucket (e.g., `netclode-juicefs`) with read/write credentials.
- If `MINIO_ENABLED=true`, MinIO is installed by Ansible and `deploy-secrets` auto-wires JuiceFS credentials/bucket from `/var/secrets/minio-root-*` if `DO_SPACES_*` / `JUICEFS_BUCKET` are omitted.

## 6. Install Ansible dependencies

```bash
cd infra/ansible
ansible-galaxy collection install -r requirements.yaml
```

## 7. Deploy

If your server disables root SSH login, pass the SSH user explicitly:

```bash
ANSIBLE_USER=ubuntu
```

```bash
cd infra/ansible

# Full infrastructure deployment (reads secrets from .env)
DEPLOY_HOST=<server-ip> ansible-playbook playbooks/site.yaml

# Full infrastructure deployment (non-root SSH user)
DEPLOY_HOST=<server-ip> ansible-playbook playbooks/site.yaml -e ansible_user=$ANSIBLE_USER
```

This installs:

- k3s (single-node Kubernetes)
- Kata Containers (microVM runtime)
- Cilium CNI (NetworkPolicy support)
- Tailscale (secure networking)
- JuiceFS CSI (S3-backed storage)
- Control plane and warm pool

## 8. Fetch kubeconfig

```bash
cd infra/ansible
DEPLOY_HOST=<server-ip> ansible-playbook playbooks/fetch-kubeconfig.yaml

# If using non-root SSH user
DEPLOY_HOST=<server-ip> ansible-playbook playbooks/fetch-kubeconfig.yaml -e ansible_user=$ANSIBLE_USER
```

This merges the `netclode` context into `~/.kube/config`. Use it with:

```bash
kubectl --context netclode get nodes
```

## 9. Verify

```bash
kubectl --context netclode -n netclode get pods
```

You should see `control-plane`, `redis-sessions`, and warm pool pods running.

Get the ingress hostname:

```bash
kubectl --context netclode -n netclode get ingress control-plane -o jsonpath='{.status.loadBalancer.ingress[0].hostname}'
```

## 10. Connect clients

Build and run the macOS app:

```bash
make run-macos
```

Then go to Settings → enter `<ingress-hostname>` → Connect.

If you plan to use Codex OAuth models from CLI, authenticate once locally:

```bash
netclode auth codex
```

For iOS, see [clients/ios/README.md](/clients/ios/README.md).

## Configuration

### Control plane

| Variable              | Default                     | Description             |
| --------------------- | --------------------------- | ----------------------- |
| `PORT`                | `3000`                      | Server port             |
| `K8S_NAMESPACE`       | `netclode`                  | Kubernetes namespace    |
| `REDIS_URL`           | `redis://redis-sessions...` | Redis URL               |
| `WARM_POOL_ENABLED`   | `true`                      | Use warm pool           |
| `MAX_ACTIVE_SESSIONS` | `2`                         | Max concurrent sessions |

### Agent

| Variable     | Description                                                 |
| ------------ | ----------------------------------------------------------- |
| `SESSION_ID` | Session identifier                                          |
| `GIT_REPOS`  | Optional JSON array of repos to clone (URL or `owner/repo`) |

For LLM API keys, see [SDK Support](sdk-support.md).

## Updating

Re-run Ansible to update infrastructure:

```bash
cd infra/ansible
DEPLOY_HOST=<server-ip> ansible-playbook playbooks/site.yaml

# If using non-root SSH user
DEPLOY_HOST=<server-ip> ansible-playbook playbooks/site.yaml -e ansible_user=$ANSIBLE_USER
```

Or deploy only k8s manifests (faster):

```bash
cd infra/ansible
DEPLOY_HOST=<server-ip> ansible-playbook playbooks/k8s-only.yaml

# If using non-root SSH user
DEPLOY_HOST=<server-ip> ansible-playbook playbooks/k8s-only.yaml -e ansible_user=$ANSIBLE_USER
```

To deploy custom images (for example, locally built images in your own GHCR namespace):

```bash
cd infra/ansible
DEPLOY_HOST=<server-ip> ansible-playbook playbooks/k8s-only.yaml \
  -e ansible_user=$ANSIBLE_USER \
  -e control_plane_image=ghcr.io/<owner>/netclode-control-plane:<tag> \
  -e agent_image=ghcr.io/<owner>/netclode-agent:<tag>
```

If those images are private, also pass registry pull credentials:

```bash
cd infra/ansible
DEPLOY_HOST=<server-ip> ansible-playbook playbooks/k8s-only.yaml \
  -e ansible_user=$ANSIBLE_USER \
  -e control_plane_image=ghcr.io/<owner>/netclode-control-plane:<tag> \
  -e agent_image=ghcr.io/<owner>/netclode-agent:<tag> \
  -e image_pull_secret_name=ghcr-pull-secret \
  -e image_pull_secret_registry=ghcr.io \
  -e image_pull_secret_username=<github-username> \
  -e image_pull_secret_password=<github-token-with-read-packages>
```

To restart deployments after image updates:

```bash
make rollout-control-plane
make rollout-agent
```

## Rollback

```bash
kubectl --context netclode -n netclode rollout undo deployment/control-plane
```

## GPU Support (Optional)

For local model inference with Ollama, see [GPU Setup in the Ansible README](/infra/ansible/README.md#gpu-support-optional).

## Troubleshooting

**Pods stuck in Pending** - check warm pool:

```bash
kubectl --context netclode -n netclode get sandboxclaim
kubectl --context netclode -n netclode get sandbox
```

**JuiceFS mount failures** - check CSI driver:

```bash
kubectl --context netclode -n kube-system logs -l app=juicefs-csi-driver
```

**Tailscale services not getting IPs** - check operator:

```bash
kubectl --context netclode -n tailscale logs -l app=operator
```

**Kata pods not starting** - verify Kata installation:

```bash
ssh root@<server> /opt/kata/bin/kata-runtime kata-env
```

For more troubleshooting, see [infra/ansible/README.md](/infra/ansible/README.md#troubleshooting).
