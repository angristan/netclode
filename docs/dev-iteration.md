# Fast Developer Iteration Loop

This workflow is optimized for day-to-day backend/agent development.

It avoids the slow path of:
- local build on laptop
- GHCR push/pull
- full Ansible workload redeploy

It keeps production deployment unchanged (Ansible + versioned GHCR images).

## What it does

Fast dev loop (`make dev-loop-ansible` or `make dev-loop-remote`):
1. Syncs only Docker-needed sources (`services/control-plane`, `services/agent`) to the dev host
2. Builds `linux/amd64` images directly on that host
3. Imports those images into k3s containerd (`k3s ctr images import`)
4. Patches only runtime workload objects with `kubectl`
5. Verifies rollout, image wiring, and recent logs

No registry push is required for this loop.

## Prerequisites

- `.env` contains `DEPLOY_HOST` (or export it in your shell)
- SSH access to `$DEPLOY_HOST` (host alias can define user in `~/.ssh/config`)
- Remote host has Docker Buildx and `k3s`
- Local machine has `rsync`, `ssh`, and `kubectl` with context `netclode`

Optional:
- `DEPLOY_SSH_TARGET` to override SSH target independently from `DEPLOY_HOST`
- Ansible path: `sync_ssh_target` extra var to override rsync SSH target explicitly

Install Docker builder tooling once (recommended):

```bash
cd /Volumes/Projects/SoftwareReferences/netclode
make dev-install-builder ANSIBLE_USER=ubuntu
```

## Preferred command (Ansible)

```bash
cd /Volumes/Projects/SoftwareReferences/netclode
make dev-loop-ansible ANSIBLE_USER=ubuntu
```

Equivalent direct playbook run:

```bash
cd /Volumes/Projects/SoftwareReferences/netclode/infra/ansible
set -a && source ../../.env && set +a

ansible-playbook playbooks/dev-loop.yaml \
  -e ansible_user=ubuntu
```

Useful tag-scoped runs:

```bash
# Build/import only
ansible-playbook playbooks/dev-loop.yaml --tags dev-build -e ansible_user=ubuntu

# Deploy only (use existing local images in k3s)
ansible-playbook playbooks/dev-loop.yaml --tags dev-deploy -e ansible_user=ubuntu \
  -e control_plane_image=netclode-control-plane:dev-123 \
  -e agent_image=netclode-agent:dev-123

# Verify only
ansible-playbook playbooks/dev-loop.yaml --tags dev-verify -e ansible_user=ubuntu
```

## Legacy script path

You can still run the script-driven path:

```bash
cd /Volumes/Projects/SoftwareReferences/netclode
make dev-loop-remote
```

This generates a `TAG=dev-YYYYMMDD-HHMMSS` and uses:
- `netclode-control-plane:$TAG`
- `netclode-agent:$TAG`

## Useful variants

Build only control-plane:

```bash
make dev-build-remote-control-plane TAG=dev-mytag
make dev-deploy-images TAG=dev-mytag
make dev-verify
```

Build only agent:

```bash
make dev-build-remote-agent TAG=dev-mytag
make dev-deploy-images TAG=dev-mytag
make dev-verify
```

Use a fixed custom tag:

```bash
make dev-loop-remote TAG=dev-jane-001
```

## What gets patched

The fast deploy phase updates:
- `Deployment/control-plane` container image
- `Deployment/control-plane` env `AGENT_IMAGE`
- `SandboxTemplate/netclode-agent` container image
- `SandboxTemplate/netclode-agent` `imagePullPolicy=IfNotPresent`

Then it refreshes the warm pool (`SandboxWarmPool/netclode-agent-pool`) so new warm sandboxes pick up the new agent image.

## Back to canonical deployment

This fast path is intended for developer iteration only.

## When to run full deployment instead

Use full deployment (`infra/ansible/playbooks/site.yaml`) when you need full state convergence, not just fast app-image iteration:

1. Initial deployment of a new host/cluster.
2. Host-level changes (k3s, CNI, Kata, firewall, Tailscale, GPU/Ollama, MinIO, base packages).
3. Kubernetes foundation changes (CRDs, RBAC, controllers, runtime/storage classes, namespace policies).
4. Secrets/cert changes (host secrets, k8s secrets, pull secrets, secret-proxy CA).
5. Drift correction when the host or cluster may have diverged from Ansible-managed state.
6. Release/canonical rollouts where you want the same path used for production.

Before sharing/releasing changes, use the canonical runbook:
- build and push traceable GHCR tags
- deploy manifests via Ansible (`site.yaml --tags k8s-manifests`)

That keeps infra state and release artifacts aligned with production procedures.
