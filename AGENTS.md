# Netclode

## Rules

- Always ask before running `git push`

## Deployment

Server is configured in `.env` as `DEPLOY_HOST`.

### Infrastructure Provisioning (Ansible)

The server infrastructure is managed with Ansible in `infra/ansible/`.

```bash
cd infra/ansible

# Deploy secrets (from .env)
ENV_FILE=../../.env DEPLOY_HOST=your-server-ip ansible-playbook playbooks/secrets.yaml

# Full infrastructure deployment (with secrets)
ENV_FILE=../../.env DEPLOY_HOST=your-server-ip ansible-playbook playbooks/site.yaml

# Fetch kubeconfig for local kubectl access
DEPLOY_HOST=your-server-ip ansible-playbook playbooks/fetch-kubeconfig.yaml

# Deploy only k8s manifests (fast updates)
DEPLOY_HOST=your-server-ip ansible-playbook playbooks/k8s-only.yaml
```

See `infra/ansible/README.md` for detailed documentation.

### Rollout after CI

After pushing changes, CI builds Docker images. To deploy:

```bash
# Wait for CI to complete
gh run watch

# Rollout all deployments (control-plane, web)
./scripts/rollout.sh

# Or rollout specific deployment
./scripts/rollout.sh control-plane
```

### Manual kubectl access

Use the `netclode` kubectl context (configured via Tailscale):

```bash
kubectl --context netclode -n netclode get pods
kubectl --context netclode -n netclode logs -l app=control-plane -f
kubectl --context netclode apply -f infra/k8s/control-plane.yaml
```

Or SSH to the server:

```bash
ssh root@netclode-host
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
kubectl -n netclode get pods
```

## Architecture

- **control-plane**: API server, WebSocket handler, session manager
- **web**: React frontend
- **agent**: Runs inside sandboxes created by control-plane (not a separate deployment)

## Tailscale Hostnames

- `netclode-host` - The server itself
- `netclode-control-plane` - Control plane service (port 80)
- `netclode-web` - Web frontend service (port 80)
