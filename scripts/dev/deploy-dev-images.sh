#!/usr/bin/env bash
set -euo pipefail

CONTEXT=${CONTEXT:-netclode}
NAMESPACE=${NAMESPACE:-netclode}
WARM_POOL_NAME=${WARM_POOL_NAME:-netclode-agent-pool}
SANDBOX_TEMPLATE=${SANDBOX_TEMPLATE:-netclode-agent}
CONTROL_PLANE_DEPLOYMENT=${CONTROL_PLANE_DEPLOYMENT:-control-plane}
REFRESH_WARM_POOL=${REFRESH_WARM_POOL:-1}

CONTROL_PLANE_IMAGE=${CONTROL_PLANE_IMAGE:-}
AGENT_IMAGE=${AGENT_IMAGE:-}

if [[ -z "$CONTROL_PLANE_IMAGE" || -z "$AGENT_IMAGE" ]]; then
	echo "CONTROL_PLANE_IMAGE and AGENT_IMAGE are required." >&2
	echo "Example: CONTROL_PLANE_IMAGE=netclode-control-plane:dev-123 AGENT_IMAGE=netclode-agent:dev-123 $0" >&2
	exit 1
fi

KUBECTL=(kubectl --context "$CONTEXT" -n "$NAMESPACE")

echo "[dev-deploy] Updating control-plane deployment image to $CONTROL_PLANE_IMAGE"
"${KUBECTL[@]}" set image "deployment/$CONTROL_PLANE_DEPLOYMENT" "control-plane=$CONTROL_PLANE_IMAGE"

echo "[dev-deploy] Ensuring control-plane uses AGENT_IMAGE=$AGENT_IMAGE"
"${KUBECTL[@]}" set env "deployment/$CONTROL_PLANE_DEPLOYMENT" "AGENT_IMAGE=$AGENT_IMAGE"

echo "[dev-deploy] Setting control-plane imagePullPolicy=IfNotPresent for local dev images"
"${KUBECTL[@]}" patch "deployment/$CONTROL_PLANE_DEPLOYMENT" --type='json' -p="[
  {\"op\":\"replace\",\"path\":\"/spec/template/spec/containers/0/imagePullPolicy\",\"value\":\"IfNotPresent\"}
]"

echo "[dev-deploy] Updating sandbox template image to $AGENT_IMAGE"
"${KUBECTL[@]}" patch "sandboxtemplate/$SANDBOX_TEMPLATE" --type='json' -p="[
  {\"op\":\"replace\",\"path\":\"/spec/podTemplate/spec/containers/0/image\",\"value\":\"$AGENT_IMAGE\"},
  {\"op\":\"replace\",\"path\":\"/spec/podTemplate/spec/containers/0/imagePullPolicy\",\"value\":\"IfNotPresent\"}
]"

echo "[dev-deploy] Waiting for control-plane rollout"
"${KUBECTL[@]}" rollout status "deployment/$CONTROL_PLANE_DEPLOYMENT" --timeout=180s

if [[ "$REFRESH_WARM_POOL" == "1" ]]; then
	current_replicas=$("${KUBECTL[@]}" get "sandboxwarmpool/$WARM_POOL_NAME" -o jsonpath='{.spec.replicas}')
	if [[ -z "$current_replicas" ]]; then
		current_replicas=1
	fi

	echo "[dev-deploy] Refreshing warm pool ($WARM_POOL_NAME): $current_replicas -> 0 -> $current_replicas"
	"${KUBECTL[@]}" patch "sandboxwarmpool/$WARM_POOL_NAME" --type=merge -p '{"spec":{"replicas":0}}'
	sleep 3
	"${KUBECTL[@]}" patch "sandboxwarmpool/$WARM_POOL_NAME" --type=merge -p "{\"spec\":{\"replicas\":$current_replicas}}"
fi

echo "[dev-deploy] Done"
