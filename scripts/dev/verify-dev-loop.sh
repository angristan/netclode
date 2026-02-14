#!/usr/bin/env bash
set -euo pipefail

CONTEXT=${CONTEXT:-netclode}
NAMESPACE=${NAMESPACE:-netclode}
CONTROL_PLANE_DEPLOYMENT=${CONTROL_PLANE_DEPLOYMENT:-control-plane}
SANDBOX_TEMPLATE=${SANDBOX_TEMPLATE:-netclode-agent}

KUBECTL=(kubectl --context "$CONTEXT" -n "$NAMESPACE")

echo "[verify] control-plane rollout"
"${KUBECTL[@]}" rollout status "deployment/$CONTROL_PLANE_DEPLOYMENT" --timeout=180s

echo "[verify] control-plane image"
"${KUBECTL[@]}" get "deployment/$CONTROL_PLANE_DEPLOYMENT" -o jsonpath='{.spec.template.spec.containers[0].image}{"\n"}'

echo "[verify] control-plane AGENT_IMAGE env"
"${KUBECTL[@]}" get "deployment/$CONTROL_PLANE_DEPLOYMENT" -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="AGENT_IMAGE")].value}{"\n"}'

echo "[verify] sandbox template agent image"
"${KUBECTL[@]}" get "sandboxtemplate/$SANDBOX_TEMPLATE" -o jsonpath='{.spec.podTemplate.spec.containers[0].image}{"\n"}'

echo "[verify] sandbox template imagePullPolicy"
"${KUBECTL[@]}" get "sandboxtemplate/$SANDBOX_TEMPLATE" -o jsonpath='{.spec.podTemplate.spec.containers[0].imagePullPolicy}{"\n"}'

echo "[verify] recent control-plane logs"
"${KUBECTL[@]}" logs "deployment/$CONTROL_PLANE_DEPLOYMENT" --tail=60
