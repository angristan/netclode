# Nix Shared Store Exploration - Postmortem

## Goal

Enable sandboxes (Kata VMs) to install arbitrary packages on-demand using Nix, with:
- Shared package cache across all sandboxes (download once, use everywhere)
- Storage on JuiceFS (offload from local disk)
- Single-writer safety (avoid SQLite corruption on network FS)
- Fast package installation

## Architecture Attempted

```
┌─────────────────────────────────────────────────────────────────┐
│                           Host                                   │
│  ┌─────────────────┐    ┌──────────────────────────────────┐   │
│  │ nix-daemon-agent│◄───│ vsock proxy (per-VM listeners)   │   │
│  │ (single writer) │    └──────────────────────────────────┘   │
│  └────────┬────────┘                  ▲                        │
│           │                           │ vsock                   │
│           ▼                           │                        │
│  ┌─────────────────────────────────┐  │                        │
│  │ /juicefs/nix-agent/nix/store    │  │                        │
│  │ (JuiceFS - chroot store)        │  │                        │
│  └─────────────────────────────────┘  │                        │
└───────────────────────────────────────┼────────────────────────┘
                                        │
┌───────────────────────────────────────┼────────────────────────┐
│                    Kata VM (Sandbox)  │                        │
│  ┌─────────────────────────────────┐  │                        │
│  │ /nix/store (read-only mount)    │◄─┘                        │
│  │ from /juicefs/nix-agent/nix/store                           │
│  └─────────────────────────────────┘                           │
│  NIX_REMOTE=daemon → vsock → host daemon                       │
└────────────────────────────────────────────────────────────────┘
```

## Key Components Implemented

### 1. Agent Image (`services/agent/image.nix`)
- Nix-based OCI image using `dockerTools.buildLayeredImage`
- Included nix, nodejs, docker, git, and common tools
- Entrypoint script to set up vsock proxy to host daemon

### 2. Host-side Daemon (`infra/nixos/modules/nix-agent-store.nix`)
- Secondary `nix-daemon-agent` using chroot store on JuiceFS
- `nix daemon --store 'local?root=/juicefs/nix-agent'`
- Per-VM vsock proxy watcher using inotify + socat
- Socket at `/juicefs/nix-agent/nix/var/nix/daemon-socket/socket`

### 3. Sandbox Template (`infra/k8s/sandbox-template.yaml`)
- Mount `/juicefs/nix-agent/nix/store` at `/nix/store` (read-only)
- Environment: `NIX_REMOTE=daemon`

## Problems Encountered

### 1. Chroot Store Path Complexity
- Nix chroot store: files at `/juicefs/nix-agent/nix/store/xxx`
- Internal paths still `/nix/store/xxx` (for binary cache compatibility)
- Required careful path management everywhere

### 2. Image Package Hiding
**Critical issue**: Mounting JuiceFS store over `/nix/store` hides the image's own packages.

The agent image contains packages in `/nix/store` (bash, nodejs, etc). When we mount the JuiceFS store at `/nix/store`, these packages become invisible, breaking the container.

Attempted solutions:
- Copy image packages to JuiceFS store (complex bootstrapping)
- Use overlay mounts (not well supported in Kata)
- Build image without `/nix/store` dependencies (requires static binaries)

### 3. Evaluation Performance
**Fundamental issue**: Nixpkgs evaluation is slow (~1-2 minutes).

When a sandbox runs `nix shell nixpkgs#python3`:
1. Nix fetches nixpkgs (fast if cached)
2. Nix evaluates nixpkgs to find python3 derivation (SLOW - parses thousands of .nix files)
3. Nix fetches package from binary cache (fast)

This evaluation cost is paid:
- Once per sandbox (eval cache not shared)
- Or requires prebuild step (user requirement: no prebuild)

### 4. Eval Cache Sharing
- Evaluation happens client-side (in sandbox)
- Each sandbox has its own `~/.cache/nix/`
- Sharing eval cache across sandboxes has concurrency issues
- Without sharing, every sandbox pays the 1-2 min eval cost

### 5. JuiceFS I/O Issues
- Redis OOM errors caused JuiceFS I/O failures
- Required increasing Redis maxmemory multiple times
- Network filesystem adds latency to Nix operations

## Options Considered to Fix Eval Performance

1. **Pre-built profiles** - Build common tools ahead of time, sandboxes source them
   - Rejected: User doesn't want prebuild steps

2. **Shared eval cache** - Mount shared `~/.cache/nix/` from JuiceFS
   - Rejected: Concurrency issues with multiple writers

3. **Flake registry with pre-resolved refs** - Pin nixpkgs, cache tarball locally
   - Only speeds up fetching, not evaluation

4. **Package → store path lookup table** - Skip eval entirely for known packages
   - Requires maintaining the table (essentially prebuild)

5. **Alternative package managers** - mise, asdf, apt
   - Pre-built binaries, no eval needed, fast

## Replit's Approach (and Why It Won't Work For Us)

Replit is often cited as a successful example of Nix in production. However, their approach fundamentally relies on prebuild steps and doesn't support truly on-demand arbitrary package installation.

### How Replit Uses Nix

Based on their open-source [nixmodules](https://github.com/replit/nixmodules) repository:

1. **Pre-built module bundles**: Replit maintains ~41 language/tool modules (python-3.10, nodejs-20, go-1.21, etc.) that are **built ahead of time**, not on-demand.

2. **SquashFS disk images**: Modules are compiled into SquashFS images (`nix build .#bundle-squashfs`) for deployment. This is a CI/CD pipeline step, not a runtime operation.

3. **Version-specific modules**: Each language version is a separate, pre-built module. Want Python 3.12? It must already exist in their module set.

4. **Limited package scope**: Users can only use packages that Replit has pre-configured in their modules. Running `nix-shell -p some-random-package` is not part of their model.

### Why This Doesn't Work For Our Use Case

| Requirement | Replit's Approach | Our Requirement |
|-------------|-------------------|-----------------|
| Prebuild steps | Yes - CI builds all modules | No prebuild steps |
| Arbitrary packages | No - only pre-configured modules | Any nixpkgs package on-demand |
| Maintenance | Dedicated team maintains 40+ modules | Minimal maintenance overhead |
| First-use latency | Fast (pre-built) | Must be fast without prebuild |

**Key insight**: Replit solves the eval performance problem by **not doing eval at runtime**. Everything is pre-evaluated and pre-built. When a user selects "Python 3.10", they get a pre-built SquashFS image - no nixpkgs evaluation happens.

This is essentially the "Pre-built profiles" option we rejected because it requires:
- Maintaining a module set (engineering overhead)
- Building and distributing bundles (infrastructure overhead)
- Limiting users to pre-defined packages (flexibility constraint)

If we wanted Replit's approach, we'd need to:
1. Define which packages/versions to support
2. Build them into bundles in CI
3. Distribute bundles to nodes
4. Accept that users can't install arbitrary packages

At that point, **mise does the same thing with less complexity** - it serves pre-built binaries without requiring us to build or maintain anything.

## Why We're Abandoning Nix

1. **Fundamental performance issue**: Nixpkgs evaluation takes 1-2 minutes and cannot be avoided without prebuild steps.

2. **Complexity vs benefit**: The architecture became increasingly complex (chroot stores, vsock proxies, image rebuilds) for a feature that would still be slow on first use.

3. **User requirements incompatible**: "No prebuild steps" + "fast package installation" + "arbitrary packages" cannot all be satisfied with Nix.

4. **Simpler alternatives exist**: Debian + mise provides fast package installation without the complexity.

5. **Replit's approach requires prebuild**: The most successful production Nix deployment (Replit) avoids eval latency by pre-building everything - which contradicts our "no prebuild" requirement.

## Lessons Learned

1. **Nix's strength is reproducibility, not speed**: Evaluation ensures reproducibility but adds latency.

2. **Sharing Nix stores is complex**: Path management, chroot stores, and daemon architecture add significant complexity.

3. **Binary cache ≠ fast**: Binary cache speeds up builds, but eval still required.

4. **Consider requirements carefully upfront**: "No prebuild" + "fast" + "flexible" is a hard combination.

## Alternative Approach: Debian + mise

The replacement approach:
- Debian slim base image with common tools (`apt-get install bash nodejs git curl docker`)
- `mise` (formerly rtx) for runtime version management
- Fast installs (~seconds, pre-built binaries)
- No evaluation step
- Simpler architecture

## Files to Revert

- `services/agent/image.nix` - Nix-based image
- `infra/nixos/modules/nix-agent-store.nix` - Host daemon and vsock proxy
- `infra/k8s/sandbox-template.yaml` - Nix store mount configuration

## Commits to Revert

```
e1faf89 Fix entrypoint shebang indentation and add NIX_STORE_DIR
283dc34 Fix shared Nix store: use daemon mode with read-only mount
73b3f7b Switch agent to Nix-built OCI image with nixpkgs 25.11
6d1c7f7 Fix entrypoint: don't mount over /nix/store, use env vars instead
e6e55ad Fix: include util-linux.mount output for mount binary
7ab720f Debug: add filesystem state logging to entrypoint
8a6054d Fix entrypoint: use /bin/bash shebang instead of nix store path
4a669e7 Fix extraCommands: remove shadow package reference
0a75773 Fix agent image: remove su usage, run agent as root
df2bc99 Fix image: create /bin symlinks for all packages
76a0e9a Fix shared nix store for agent VMs
d6fc904 Fix image: create user files from scratch instead of fakeNss
0ff9c0f Fix image: use buildLayeredImage to avoid duplicate files
11969f3 Fix CI: use working-directory for agent build
c27f7e4 Fix agent build: use npm for deps, Nix for image
9afcaca Fix dockerTools config format and nixpkgs version
5779a6f Add shared Nix store for agent VMs
```

---

## Update: NixOS Infrastructure Replaced with Ansible

We've also dropped NixOS for the host itself, replacing `infra/nixos/` with `infra/ansible/`.

### Why

The main reason we used NixOS was to run the nix-daemon for the shared store experiment above. Now that we've dropped Nix for agents, there's no reason to keep NixOS on the host.

Debian + Ansible is just easier. Boring, but it works.

### Migration

New infra is in `infra/ansible/`.
