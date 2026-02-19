# Debian-slim + mise agent image
#
# On-demand tooling via:
#   mise install python@3.12
#   mise install go@latest
#   mise install rust@latest
#
# Layer ordering is optimized for cache efficiency:
#   1. Base OS packages (rarely changes)
#   2. Global tool installs (changes on tool version bumps)
#   3. Package dependency install (changes on package.json/lock updates)
#   4. Source copy + bundle (changes on every code change)
#
FROM debian:trixie-slim AS base

# Install essentials + mise + docker
# Note: Use iptables-legacy because Kata VM kernel lacks nftables support
# Note: Node is installed in builder stage only, then copied to runtime
RUN apt-get update && apt-get install -y --no-install-recommends \
    bash \
    curl \
    git \
    ca-certificates \
    build-essential \
    docker.io \
    docker-cli \
    iptables \
    sudo \
    && update-alternatives --set iptables /usr/sbin/iptables-legacy \
    && update-alternatives --set ip6tables /usr/sbin/ip6tables-legacy \
    && rm -rf /var/lib/apt/lists/* \
    && curl https://mise.run | sh

ENV PATH="/root/.local/bin:/root/.local/share/mise/shims:${PATH}"

# =============================================================================
# Go Builder (for auth-proxy)
# =============================================================================
FROM golang:1.25-bookworm AS go-builder

WORKDIR /build
COPY services/agent/auth-proxy ./auth-proxy
WORKDIR /build/auth-proxy
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o auth-proxy .

# =============================================================================
# Builder
# =============================================================================
FROM base AS builder

# Install Python for node-gyp (required by node-pty) and Node via mise
RUN apt-get update && apt-get install -y --no-install-recommends python3 && rm -rf /var/lib/apt/lists/* && \
    mise install node@24 && \
    mise use -g node@24 && \
    mise reshim

WORKDIR /build

# Install esbuild for bundling
RUN npm install -g esbuild

# =============================================================================
# Global tool installs (BEFORE source copy for layer caching)
# These rarely change and are expensive to install. By placing them before
# the source COPY, they stay cached on typical code-only changes.
# =============================================================================

# Claude CLI
RUN --mount=type=cache,target=/root/.npm \
    npm install -g @anthropic-ai/claude-code

# Copilot CLI
RUN --mount=type=cache,target=/root/.npm \
    npm install -g @github/copilot

# OpenCode CLI (download Linux x64 binary from GitHub releases)
RUN curl -fsSL "https://github.com/anomalyco/opencode/releases/latest/download/opencode-linux-x64.tar.gz" -o /tmp/opencode.tar.gz && \
    tar -xzf /tmp/opencode.tar.gz -C /tmp && \
    mv /tmp/opencode /usr/local/bin/opencode && \
    chmod +x /usr/local/bin/opencode && \
    rm -rf /tmp/opencode.tar.gz

# GitHub CLI (download Linux x64 binary)
RUN GH_VERSION=$(curl -fsSL https://api.github.com/repos/cli/cli/releases/latest | grep -oP '"tag_name": "v\K[^"]+') && \
    curl -fsSL "https://github.com/cli/cli/releases/download/v${GH_VERSION}/gh_${GH_VERSION}_linux_amd64.tar.gz" -o /tmp/gh.tar.gz && \
    tar -xzf /tmp/gh.tar.gz -C /tmp && \
    mv /tmp/gh_${GH_VERSION}_linux_amd64/bin/gh /usr/local/bin/gh && \
    chmod +x /usr/local/bin/gh && \
    rm -rf /tmp/gh.tar.gz /tmp/gh_${GH_VERSION}_linux_amd64

# Remove C headers from Node (only needed for compiling native modules)
# Also remove unused platform binaries from global @github/copilot CLI (~84MB savings)
RUN rm -rf /root/.local/share/mise/installs/node/24/include \
           /root/.local/share/mise/installs/node/24/lib/node_modules/@github/copilot/prebuilds/darwin-* \
           /root/.local/share/mise/installs/node/24/lib/node_modules/@github/copilot/prebuilds/win32-* \
           /root/.local/share/mise/installs/node/24/lib/node_modules/@github/copilot/prebuilds/linux-arm64 \
           /root/.local/share/mise/installs/node/24/lib/node_modules/@github/copilot/ripgrep/bin/darwin-* \
           /root/.local/share/mise/installs/node/24/lib/node_modules/@github/copilot/ripgrep/bin/win32-* \
           /root/.local/share/mise/installs/node/24/lib/node_modules/@github/copilot/ripgrep/bin/linux-arm64

# =============================================================================
# Agent dependency install (cached unless package.json/lock change)
# =============================================================================
COPY services/agent/package.json services/agent/package-lock.json /build/services/agent/
WORKDIR /build/services/agent
RUN --mount=type=cache,target=/root/.npm npm install

# =============================================================================
# Agent source copy + bundle (re-runs on every code change)
# =============================================================================
COPY services/agent /build/services/agent

# Bundle agent
RUN esbuild src/index.ts --bundle --platform=node --format=esm --packages=external --outfile=dist/agent.js

# Remove devDependencies (typescript, vitest, etc. not needed at runtime)
RUN npm prune --omit=dev

# Remove unused platform binaries (keep only linux-x64)
# @openai/codex-sdk: ships binaries for all 6 platforms (~270MB savings)
# @github/copilot: prebuilds and ripgrep for all platforms (~60MB savings)
RUN rm -rf node_modules/@openai/codex-sdk/vendor/aarch64-* \
           node_modules/@openai/codex-sdk/vendor/x86_64-apple-* \
           node_modules/@openai/codex-sdk/vendor/x86_64-pc-windows-* \
           node_modules/@github/copilot/prebuilds/darwin-* \
           node_modules/@github/copilot/prebuilds/win32-* \
           node_modules/@github/copilot/prebuilds/linux-arm64 \
           node_modules/@github/copilot/ripgrep/bin/darwin-* \
           node_modules/@github/copilot/ripgrep/bin/win32-* \
           node_modules/@github/copilot/ripgrep/bin/linux-arm64

# =============================================================================
# Runtime
# =============================================================================
FROM base AS runtime

# Create non-root user with docker access and passwordless sudo
# Note: /agent is the persistent home dir, mounted from JuiceFS
# Sudo is safe because we run inside an isolated Kata VM
RUN useradd -m -u 1000 -G docker -d /agent agent && \
    echo 'agent ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/agent && \
    chmod 0440 /etc/sudoers.d/agent && \
    mkdir -p /agent /opt/agent && \
    chown -R agent:agent /agent /opt/agent

# Install bun (no dependency on builder - can run in parallel)
RUN apt-get update && apt-get install -y --no-install-recommends unzip && \
    curl -fsSL "https://github.com/oven-sh/bun/releases/latest/download/bun-linux-x64.zip" -o /tmp/bun.zip && \
    unzip /tmp/bun.zip -d /tmp && \
    mv /tmp/bun-linux-x64/bun /opt/bun && \
    chmod +x /opt/bun && \
    rm -rf /tmp/bun.zip /tmp/bun-linux-x64 /var/lib/apt/lists/*

# Copy stable binaries (these rarely change, so place before frequently-changing layers)
COPY --from=builder /usr/local/bin/opencode /usr/local/bin/opencode
COPY --from=builder /usr/local/bin/gh /usr/local/bin/gh
COPY --from=go-builder /build/auth-proxy/auth-proxy /usr/local/bin/auth-proxy

# Copy mise binary to /opt (NOT /agent which is a volume mount)
# mise binary needs to be in a persistent location for on-demand tooling
# Node is copied separately from builder (headers already removed in builder stage)
RUN mkdir -p /opt/mise/bin && \
    cp /root/.local/bin/mise /opt/mise/bin/
COPY --from=builder /root/.local/share/mise/installs/node/24 /opt/node

# Pre-install OpenCode plugin to both:
# 1. Bun's global cache (/opt/bun-cache) - for faster installs if needed
# 2. OpenCode's config dir (/opt/opencode-config) - so OpenCode skips install entirely
#
# OpenCode checks if node_modules exists in its config dir before running bun add.
# By pre-creating this directory with the plugin installed, we skip the ~4s install.
# Note: @opencode-ai/plugin@1.1.45 has broken workspace references, so we use 1.1.44 as fallback
RUN OPENCODE_VERSION=$(/usr/local/bin/opencode --version) && \
    export HOME=/opt/bun-cache-home && \
    mkdir -p $HOME/.bun && \
    mkdir -p /opt/opencode-config && cd /opt/opencode-config && \
    echo '{"name": "opencode-config", "private": true}' > package.json && \
    if /opt/bun add "@opencode-ai/plugin@${OPENCODE_VERSION}" "@opencode-ai/sdk@${OPENCODE_VERSION}" "zod" --exact; then \
      echo "Preinstalled OpenCode plugin ${OPENCODE_VERSION}"; \
    else \
      echo "Warning: OpenCode plugin ${OPENCODE_VERSION} not found, falling back to 1.1.44"; \
      /opt/bun add "@opencode-ai/plugin@1.1.44" "@opencode-ai/sdk@1.1.44" "zod@4.1.8" --exact; \
    fi && \
    if [ -d "$HOME/.bun" ]; then mv $HOME/.bun /opt/bun-cache; fi && \
    rm -rf $HOME

# Create profile for agent user that sets PATH and mise env vars
# This ensures any shell spawned (including by Claude Code) has access to mise
# Add to both /etc/profile.d (login shells) and /etc/bash.bashrc (interactive non-login)
# MISE_DATA_DIR is in /agent so installed tools persist across pod restarts
# Shims path is included so mise-installed tools are available
# XDG_CONFIG_HOME avoids JuiceFS .config file at mount root
RUN echo 'export MISE_DATA_DIR=/agent/.local/share/mise' >> /etc/profile.d/mise.sh && \
    echo 'export MISE_CACHE_DIR=/agent/.cache/mise' >> /etc/profile.d/mise.sh && \
    echo 'export XDG_CONFIG_HOME=/agent/.local/config' >> /etc/profile.d/mise.sh && \
    echo 'export PATH="/agent/.local/share/mise/shims:/opt/mise/bin:/opt/node/bin:/opt:$PATH"' >> /etc/profile.d/mise.sh && \
    chmod +x /etc/profile.d/mise.sh && \
    echo 'export MISE_DATA_DIR=/agent/.local/share/mise' >> /etc/bash.bashrc && \
    echo 'export MISE_CACHE_DIR=/agent/.cache/mise' >> /etc/bash.bashrc && \
    echo 'export XDG_CONFIG_HOME=/agent/.local/config' >> /etc/bash.bashrc && \
    echo 'export PATH="/agent/.local/share/mise/shims:/opt/mise/bin:/opt/node/bin:/opt:$PATH"' >> /etc/bash.bashrc && \
    echo 'export PS1='"'"'\[\033[01;32m\]\u\[\033[00m\]:\[\033[01;34m\]\w\[\033[00m\]\$ '"'" >> /etc/bash.bashrc

# Copy frequently-changing files LAST to maximize cache hits above
COPY --from=builder --chown=agent:agent /build/services/agent/dist/agent.js /opt/agent/
COPY --from=builder --chown=agent:agent /build/services/agent/node_modules /opt/agent/node_modules

# Copy entrypoint script
COPY services/agent/entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

WORKDIR /opt/agent

ENV NODE_ENV=production
ENV HOME=/agent
ENV MISE_DATA_DIR=/agent/.local/share/mise
ENV MISE_CACHE_DIR=/agent/.cache/mise
ENV XDG_CONFIG_HOME=/agent/.local/config
ENV PATH="/agent/.local/share/mise/shims:/opt/mise/bin:/opt/node/bin:/opt:${PATH}"

EXPOSE 3002

# Start as root to run dockerd, then drop to agent user
ENTRYPOINT ["/entrypoint.sh"]
