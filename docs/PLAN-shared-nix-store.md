# Plan: Shared Nix Store for Agent VMs

## Goal

Enable instant, shared Nix package access across all agent VMs:
- Agent A installs a package → Agent B gets it instantly (no re-download)
- No pre-warming or pre-baking required
- Storage offloaded to JuiceFS (S3-backed with local cache)

## Architecture Overview

```
┌─────────────────────────────────────────────────────────────────┐
│  Host                                                           │
│                                                                 │
│  /nix/store ← bind mount from /juicefs/nix/store               │
│  /nix/var   ← bind mount from /juicefs/nix/var                 │
│       ↑                                                         │
│  JuiceFS (50GB local cache + S3 backend)                       │
│       ↑                                                         │
│  nix-daemon (single writer, handles all store operations)       │
│       ↑                                                         │
│  vsock listener (socat: vsock port 6000 → nix-daemon socket)   │
│                                                                 │
│  Kata sandbox_bind_mounts = ["/nix/store", "/nix/var/nix/db"]  │
└─────────────────────────────────────────────────────────────────┘
              │
              │ virtiofs (read - near native speed)
              │ vsock CID 2 port 6000 (write - daemon protocol)
              ▼
┌─────────────────────────────────────────────────────────────────┐
│  Agent VM (Cloud Hypervisor)                                    │
│                                                                 │
│  /nix/store ← virtiofs from sandbox_bind_mounts (READ)         │
│                                                                 │
│  socat: /run/nix-daemon.sock → vsock CID 2 port 6000           │
│  NIX_REMOTE=daemon (uses /run/nix-daemon.sock)                 │
│                                                                 │
│  nix-shell -p python                                            │
│    → daemon fetches → writes to host store → visible via virtiofs│
└─────────────────────────────────────────────────────────────────┘
```

## Implementation Steps

### Phase 1: Switch from Firecracker to Cloud Hypervisor

#### 1.1 Update Kata configuration in k3s.nix

Change the containerd config template to use `kata-clh` instead of `kata-fc`:

```nix
# In infra/nixos/modules/k3s.nix

# Change runtime type
[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.kata-clh]
  runtime_type = "io.containerd.kata-clh.v2"
  privileged_without_host_devices = true
  pod_annotations = ["io.katacontainers.*"]
  [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.kata-clh.options]
    ConfigPath = "${kataDir}/share/defaults/kata-containers/configuration-clh.toml"
```

Update shim symlinks:
```nix
# In tmpfiles.rules
"L+ /usr/local/bin/containerd-shim-kata-clh-v2 - - - - ${kataDir}/bin/containerd-shim-kata-v2"
```

#### 1.2 Update Kubernetes manifests

Files to update:
- `infra/k8s/runtime-class.yaml`: Change handler from `kata-fc` to `kata-clh`
- `infra/k8s/sandbox-template.yaml`: Change `runtimeClassName` from `kata-fc` to `kata-clh`

#### 1.3 Update any Go code references

Search for `kata-fc` in `apps/` and update to `kata-clh`.

---

### Phase 2: JuiceFS-backed Nix Store on Host

#### 2.1 Create new NixOS module: `modules/nix-juicefs.nix`

```nix
# infra/nixos/modules/nix-juicefs.nix
{ config, lib, pkgs, ... }: {
  # Ensure JuiceFS directories exist for Nix store
  systemd.tmpfiles.rules = [
    "d /juicefs/nix 0755 root root -"
    "d /juicefs/nix/store 0755 root root -"
    "d /juicefs/nix/var 0755 root root -"
    "d /juicefs/nix/var/nix 0755 root root -"
    "d /juicefs/nix/var/nix/db 0755 root root -"
  ];

  # Bind mount JuiceFS nix store to /nix
  # Note: This must happen AFTER juicefs.service mounts /juicefs
  fileSystems."/nix/store" = {
    device = "/juicefs/nix/store";
    fsType = "none";
    options = [ "bind" ];
    depends = [ "/juicefs" ];
  };

  fileSystems."/nix/var/nix" = {
    device = "/juicefs/nix/var/nix";
    fsType = "none";
    options = [ "bind" ];
    depends = [ "/juicefs" ];
  };

  # Ensure nix-daemon starts after mounts are ready
  systemd.services.nix-daemon = {
    after = [ "juicefs.service" ];
    requires = [ "juicefs.service" ];
  };
}
```

**Important considerations:**
- The existing `/nix/store` has packages needed for the system to boot
- You may need to copy essential packages to JuiceFS first, or use an overlay approach
- Alternative: Only use JuiceFS for agent packages, keep system packages on local disk

#### 2.2 Alternative: Separate store for agents

If the above is too complex, use a separate store path for agents:

```nix
# Host serves a secondary store at /juicefs/agent-nix-store
# Agents use: NIX_REMOTE=daemon --store /juicefs/agent-nix-store
```

---

### Phase 3: Kata sandbox_bind_mounts Configuration

#### 3.1 Create custom Kata configuration

Create a custom configuration that extends the default Cloud Hypervisor config:

```nix
# In infra/nixos/modules/k3s.nix

kataConfigClh = pkgs.writeText "configuration-clh-custom.toml" ''
  # Include defaults
  # Copy from /opt/kata/share/defaults/kata-containers/configuration-clh.toml
  # and add:

  [runtime]
  # Share host's nix store with all VMs
  sandbox_bind_mounts = ["/nix/store", "/nix/var/nix/db"]

  # ... rest of config
'';
```

Or patch the existing config:

```nix
systemd.services.kata-config = {
  description = "Configure Kata for shared Nix store";
  wantedBy = ["multi-user.target"];
  after = ["kata-install.service"];

  script = ''
    CONFIG="/opt/kata/share/defaults/kata-containers/configuration-clh.toml"

    # Add sandbox_bind_mounts if not present
    if ! grep -q "sandbox_bind_mounts" "$CONFIG"; then
      sed -i '/\[runtime\]/a sandbox_bind_mounts = ["/nix/store", "/nix/var/nix/db"]' "$CONFIG"
    fi
  '';
};
```

---

### Phase 4: vsock Proxy for nix-daemon

#### 4.1 Create vsock listener service on host

```nix
# infra/nixos/modules/nix-vsock.nix
{ config, lib, pkgs, ... }: {
  # vsock proxy: listens on vsock, forwards to nix-daemon socket
  systemd.services.nix-vsock-proxy = {
    description = "Nix daemon vsock proxy";
    wantedBy = ["multi-user.target"];
    after = ["nix-daemon.service"];
    requires = ["nix-daemon.service"];

    serviceConfig = {
      Type = "simple";
      Restart = "always";
      RestartSec = "5s";
      # socat: listen on vsock port 6000, forward to nix-daemon socket
      ExecStart = "${pkgs.socat}/bin/socat VSOCK-LISTEN:6000,fork UNIX-CONNECT:/nix/var/nix/daemon-socket/socket";
    };
  };

  environment.systemPackages = [ pkgs.socat ];
}
```

#### 4.2 Verify vsock kernel module

Already in your config:
```nix
boot.kernelModules = ["kvm-intel" "kvm-amd" "vhost_net" "vhost_vsock"];
```

---

### Phase 5: Agent Image Updates

#### 5.1 Install Nix in agent image

Update `apps/agent/Dockerfile`:

```dockerfile
FROM debian:trixie-slim AS base

# Install Nix (single-user mode for simplicity in container)
RUN apt-get update && apt-get install -y --no-install-recommends \
    curl xz-utils ca-certificates \
    && curl -L https://nixos.org/nix/install | sh -s -- --no-daemon \
    && rm -rf /var/lib/apt/lists/*

# Add nix to path
ENV PATH="/root/.nix-profile/bin:/nix/var/nix/profiles/default/bin:${PATH}"

# Install socat for vsock proxy
RUN apt-get update && apt-get install -y socat && rm -rf /var/lib/apt/lists/*
```

#### 5.2 Update agent entrypoint

Update `apps/agent/entrypoint.sh`:

```bash
#!/bin/bash
set -e

# --- Shared Nix Store Setup ---

# The host's /nix/store is available via sandbox_bind_mounts at:
SHARED_NIX_STORE="/run/kata-containers/shared/containers/sandbox-mounts/nix-store"
SHARED_NIX_DB="/run/kata-containers/shared/containers/sandbox-mounts/nix-var-nix-db"

# Wait for shared mounts to be available (Kata sets these up)
echo "Waiting for shared Nix store..."
for i in $(seq 1 30); do
  if [ -d "$SHARED_NIX_STORE" ]; then
    break
  fi
  sleep 1
done

if [ -d "$SHARED_NIX_STORE" ]; then
  echo "Shared Nix store found, setting up bind mounts..."

  # Create /nix structure
  mkdir -p /nix/store /nix/var/nix/db

  # Bind mount shared store (read-only for direct access)
  mount --bind "$SHARED_NIX_STORE" /nix/store

  # For the database, we need read access
  if [ -d "$SHARED_NIX_DB" ]; then
    mount --bind "$SHARED_NIX_DB" /nix/var/nix/db
  fi

  echo "Nix store mounted from host"
else
  echo "WARNING: Shared Nix store not found, using local store"
fi

# --- vsock Proxy to Host nix-daemon ---

# Start socat to bridge vsock to local unix socket
# CID 2 = host, port 6000 = nix-daemon proxy on host
mkdir -p /nix/var/nix/daemon-socket
socat UNIX-LISTEN:/nix/var/nix/daemon-socket/socket,fork VSOCK-CONNECT:2:6000 &
SOCAT_PID=$!

# Configure Nix to use the daemon
export NIX_REMOTE=daemon

echo "Nix daemon proxy started (PID $SOCAT_PID)"

# --- Rest of existing entrypoint ---

# Start dockerd if needed
if [ -S /var/run/docker.sock ] || command -v dockerd &> /dev/null; then
  dockerd &
fi

# Drop to agent user and run agent
exec su - agent -c "cd /opt/agent && node agent.js"
```

#### 5.3 Add required packages to agent image

```dockerfile
# Required for nix and vsock
RUN apt-get update && apt-get install -y --no-install-recommends \
    socat \
    && rm -rf /var/lib/apt/lists/*
```

---

### Phase 6: Testing

#### 6.1 Test on host

```bash
# Verify JuiceFS nix store
ls -la /juicefs/nix/store/

# Verify vsock proxy
systemctl status nix-vsock-proxy

# Test nix-daemon locally
nix-shell -p hello --run "hello"
```

#### 6.2 Test in agent VM

```bash
# Exec into an agent pod
kubectl exec -it <agent-pod> -- bash

# Check shared mount
ls /run/kata-containers/shared/containers/sandbox-mounts/

# Check nix store mount
ls /nix/store/

# Test nix-shell (should use shared store)
nix-shell -p cowsay --run "cowsay 'Shared Nix works!'"

# Second agent should have cowsay instantly
```

#### 6.3 Verify sharing works

1. Start Agent A: `nix-shell -p ripgrep`
2. Start Agent B: `nix-shell -p ripgrep` (should be instant)
3. Check host: `ls /juicefs/nix/store/ | grep ripgrep`

---

## File Changes Summary

| File | Change |
|------|--------|
| `infra/nixos/modules/k3s.nix` | Switch to kata-clh, update containerd template |
| `infra/nixos/modules/nix-juicefs.nix` | NEW: JuiceFS-backed nix store |
| `infra/nixos/modules/nix-vsock.nix` | NEW: vsock proxy for nix-daemon |
| `infra/nixos/hosts/netclode-do/default.nix` | Import new modules |
| `infra/k8s/runtime-class.yaml` | kata-fc → kata-clh |
| `infra/k8s/sandbox-template.yaml` | kata-fc → kata-clh |
| `apps/agent/Dockerfile` | Add nix, socat |
| `apps/agent/entrypoint.sh` | Add nix store mount and vsock proxy setup |

---

## Risks and Mitigations

| Risk | Mitigation |
|------|------------|
| JuiceFS latency for uncached packages | 50GB local cache, S3 in same region |
| nix-daemon becomes bottleneck | Single daemon handles many concurrent requests well |
| vsock connection issues | Retry logic in socat, health checks |
| Host Nix store corruption | JuiceFS snapshots, separate store for host system |
| Memory overhead of Cloud Hypervisor | ~20-50MB more per VM, acceptable trade-off |

---

## Rollback Plan

If issues occur:
1. Revert k3s.nix to use `kata-fc`
2. Revert runtime-class.yaml and sandbox-template.yaml
3. Revert agent Dockerfile and entrypoint
4. Remove nix-juicefs.nix and nix-vsock.nix imports

---

## Future Improvements

1. **Environment caching**: Cache `nix-shell` environment variables to skip evaluation
2. **Popular package pre-fetch**: Background job to fetch commonly used packages
3. **Metrics**: Track cache hit rate, daemon latency, package fetch times
