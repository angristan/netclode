# Deployment

Here's how to get Netclode running on your own server.

## Prerequisites

- Linux machine with nested virtualization (2 vCPU, 8GB RAM minimum)
- S3-compatible storage (DigitalOcean Spaces, Cloudflare R2, etc.)
- Tailscale account
- At least one LLM API key (Anthropic, OpenAI, Mistral, etc.) - see [SDK Support](sdk-support.md)
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

1. Create an [OAuth client](https://login.tailscale.com/admin/settings/oauth) with **Devices: Write** and **Auth Keys: Write** scopes
2. Enable [MagicDNS](https://login.tailscale.com/admin/dns)

## 5. Configure secrets

Create `.env` at the repo root:

```bash
# LLM provider (at least one required - see docs/sdk-support.md)
ANTHROPIC_API_KEY=sk-ant-api03-xxx
# OPENAI_API_KEY=sk-xxx
# MISTRAL_API_KEY=xxx

# Tailscale (OAuth client from step 4)
TS_OAUTH_CLIENT_ID=your-oauth-client-id
TS_OAUTH_CLIENT_SECRET=your-oauth-client-secret

# Tailscale auth key for host access (optional)
# If not set, authenticate manually on the host: tailscale up --ssh
# TAILSCALE_AUTHKEY=tskey-auth-xxx

# JuiceFS / S3 storage
DO_SPACES_ACCESS_KEY=your-spaces-access-key
DO_SPACES_SECRET_KEY=your-spaces-secret-key
JUICEFS_BUCKET=https://fra1.digitaloceanspaces.com/your-bucket
JUICEFS_META_URL=redis://redis-juicefs.netclode.svc.cluster.local:6379/0

# Deployment target (Tailscale hostname from step 3)
DEPLOY_HOST=your-server

# GitHub App (optional - for repo picker and github-bot)
GITHUB_APP_ID=123456
GITHUB_APP_PRIVATE_KEY_B64=base64-encoded-pem-private-key
GITHUB_INSTALLATION_ID=12345678

# GitHub Bot webhook secret (required if using github-bot)
# GITHUB_WEBHOOK_SECRET=your-webhook-secret

# Kata VM resources (optional, defaults shown)
# KATA_VM_CPUS=4
# KATA_VM_MEMORY_MB=4096

# Datadog observability (optional)
# DD_API_KEY=your-datadog-api-key
# DD_APP_KEY=your-datadog-app-key
# DD_SITE=datadoghq.com
```

Create a bucket (e.g., `netclode-juicefs`) with read/write credentials.

## 6. Install Ansible dependencies

```bash
cd infra/ansible
ansible-galaxy collection install -r requirements.yaml
```

## 7. Deploy

```bash
cd infra/ansible

# Full infrastructure deployment (reads secrets from .env)
DEPLOY_HOST=<server-ip> ansible-playbook playbooks/site.yaml
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

For iOS, see [clients/ios/README.md](/clients/ios/README.md).

## Configuration

### Control plane

| Variable              | Default                     | Description             |
| --------------------- | --------------------------- | ----------------------- |
| `CLIENT_PORT`         | `3000`                      | Tailscale-only client listener |
| `AGENT_PORT`          | `3001`                      | Sandbox AgentService listener |
| `INTERNAL_PORT`       | `3002`                      | Secret-proxy validation listener |
| `BOT_PORT`            | `3003`                      | GitHub bot listener |
| `K8S_NAMESPACE`       | `netclode`                  | Kubernetes namespace    |
| `REDIS_URL`           | `redis://redis-sessions...` | Redis URL               |
| `WARM_POOL_ENABLED`   | `true`                      | Use warm pool           |
| `MAX_ACTIVE_SESSIONS` | `5`                         | Max concurrent sessions |
| `IDLE_TIMEOUT_MINUTES` | `0` (disabled)             | Auto-pause sessions after N minutes of inactivity |

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
```

Or deploy only k8s manifests (faster):

```bash
cd infra/ansible
DEPLOY_HOST=<server-ip> ansible-playbook playbooks/k8s-only.yaml
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
