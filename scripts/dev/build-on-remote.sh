#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)

if [[ -f "$ROOT_DIR/.env" ]]; then
	set -a
	# shellcheck disable=SC1091
	source "$ROOT_DIR/.env"
	set +a
fi

DEPLOY_HOST=${DEPLOY_HOST:-}
if [[ -z "$DEPLOY_HOST" ]]; then
	echo "DEPLOY_HOST is required (.env or env var)." >&2
	exit 1
fi

# SSH target defaults to DEPLOY_HOST so user/login comes from ~/.ssh/config.
DEPLOY_SSH_TARGET=${DEPLOY_SSH_TARGET:-$DEPLOY_HOST}
TAG=${TAG:-dev-$(date +%Y%m%d-%H%M%S)}
REMOTE_DIR=${REMOTE_DIR:-/tmp/netclode-dev-worktree}
BUILD_CONTROL_PLANE=${BUILD_CONTROL_PLANE:-1}
BUILD_AGENT=${BUILD_AGENT:-1}
IMPORT_TO_K3S=${IMPORT_TO_K3S:-1}

CONTROL_PLANE_IMAGE=${CONTROL_PLANE_IMAGE:-netclode-control-plane:$TAG}
AGENT_IMAGE=${AGENT_IMAGE:-netclode-agent:$TAG}

if [[ "$BUILD_CONTROL_PLANE" != "1" && "$BUILD_AGENT" != "1" ]]; then
	echo "Nothing to build. Set BUILD_CONTROL_PLANE=1 and/or BUILD_AGENT=1." >&2
	exit 1
fi

for cmd in rsync ssh; do
	if ! command -v "$cmd" >/dev/null 2>&1; then
		echo "Missing required command: $cmd" >&2
		exit 1
	fi
done

echo "[dev-build] Preparing remote workspace on $DEPLOY_SSH_TARGET:$REMOTE_DIR"
ssh "$DEPLOY_SSH_TARGET" "mkdir -p '$REMOTE_DIR/services'"

if [[ "$BUILD_CONTROL_PLANE" == "1" ]]; then
	echo "[dev-build] Syncing services/control-plane"
	rsync -az --delete \
		--exclude '.DS_Store' \
		"$ROOT_DIR/services/control-plane/" "$DEPLOY_SSH_TARGET:$REMOTE_DIR/services/control-plane/"
fi

if [[ "$BUILD_AGENT" == "1" ]]; then
	echo "[dev-build] Syncing services/agent"
	rsync -az --delete \
		--exclude '.DS_Store' \
		--exclude 'node_modules' \
		--exclude 'dist' \
		"$ROOT_DIR/services/agent/" "$DEPLOY_SSH_TARGET:$REMOTE_DIR/services/agent/"
fi

echo "[dev-build] Building images on $DEPLOY_SSH_TARGET"
ssh "$DEPLOY_SSH_TARGET" \
	"TAG='$TAG' REMOTE_DIR='$REMOTE_DIR' BUILD_CONTROL_PLANE='$BUILD_CONTROL_PLANE' BUILD_AGENT='$BUILD_AGENT' IMPORT_TO_K3S='$IMPORT_TO_K3S' CONTROL_PLANE_IMAGE='$CONTROL_PLANE_IMAGE' AGENT_IMAGE='$AGENT_IMAGE' bash -se" <<'EOSSH'
set -euo pipefail

cd "$REMOTE_DIR"

if [[ "$BUILD_CONTROL_PLANE" == "1" || "$BUILD_AGENT" == "1" ]]; then
	if ! command -v docker >/dev/null 2>&1; then
		echo "[remote-build] docker is required on the remote host but was not found in PATH." >&2
		echo "[remote-build] Install Docker (with buildx) on the dev host or disable remote build targets." >&2
		exit 127
	fi
fi

if [[ "$BUILD_CONTROL_PLANE" == "1" ]]; then
	echo "[remote-build] Building control-plane image: $CONTROL_PLANE_IMAGE"
	docker buildx build --platform linux/amd64 \
		-f services/control-plane/Dockerfile \
		-t "$CONTROL_PLANE_IMAGE" \
		--load services/control-plane
	if [[ "$IMPORT_TO_K3S" == "1" ]]; then
		echo "[remote-build] Importing control-plane image into k3s containerd"
		docker save "$CONTROL_PLANE_IMAGE" | sudo k3s ctr images import -
	fi
fi

if [[ "$BUILD_AGENT" == "1" ]]; then
	echo "[remote-build] Building agent image: $AGENT_IMAGE"
	docker buildx build --platform linux/amd64 \
		-f services/agent/Dockerfile \
		-t "$AGENT_IMAGE" \
		--load .
	if [[ "$IMPORT_TO_K3S" == "1" ]]; then
		echo "[remote-build] Importing agent image into k3s containerd"
		docker save "$AGENT_IMAGE" | sudo k3s ctr images import -
	fi
fi
EOSSH

echo "[dev-build] Built images"
if [[ "$BUILD_CONTROL_PLANE" == "1" ]]; then
	echo "  CONTROL_PLANE_IMAGE=$CONTROL_PLANE_IMAGE"
fi
if [[ "$BUILD_AGENT" == "1" ]]; then
	echo "  AGENT_IMAGE=$AGENT_IMAGE"
fi
