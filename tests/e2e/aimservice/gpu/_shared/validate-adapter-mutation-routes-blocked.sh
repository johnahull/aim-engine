#!/usr/bin/env bash
# Verify dynamic vLLM LoRA mutation APIs exist inside the predictor but are
# rewritten to /__aim_blocked__ at the AIM-managed Gateway route.
set -euo pipefail

WORKLOAD_NS="${NAMESPACE:?NAMESPACE is required}"
AIM_SERVICE="${AIM_SERVICE:?AIM_SERVICE is required}"
ADAPTER_NAME="${ADAPTER_NAME:?ADAPTER_NAME is required}"
SERVICE_PATH="${HTTP_SERVICE_PATH:?HTTP_SERVICE_PATH is required}"
GATEWAY_NS="${HTTP_NS:-envoy-gateway-system}"
GATEWAY="${HTTP_GATEWAY:-${HTTP_SVC:-kserve-ingress-gateway}}"
SVC_PORT="${HTTP_PORT:-80}"
TIMEOUT="${HTTP_TIMEOUT:-60}"

need() {
  command -v "$1" >/dev/null 2>&1 || {
    echo "Missing: $1" >&2
    exit 2
  }
}
need kubectl
need curl
need jq
need lsof

SERVICE_PATH="${SERVICE_PATH%/}"
POD="$(kubectl get pods -n "$WORKLOAD_NS" \
  -l "aim.eai.amd.com/service.name=${AIM_SERVICE},app.kubernetes.io/component=inference" \
  -o jsonpath='{.items[0].metadata.name}')"
test -n "$POD"

for endpoint in /v1/load_lora_adapter /v1/unload_lora_adapter; do
  # An empty JSON object reaches the handler's request validation without
  # naming an adapter, so this confirms registration without mutating state.
  INTERNAL_CODE="$(kubectl exec -n "$WORKLOAD_NS" "$POD" \
    -c kserve-container -- python3 -c '
import sys
import urllib.error
import urllib.request

request = urllib.request.Request(
    sys.argv[1],
    data=b"{}",
    headers={"Content-Type": "application/json"},
)
try:
    print(urllib.request.urlopen(request, timeout=10).getcode())
except urllib.error.HTTPError as error:
    print(error.code)
except Exception:
    print("000")
' "http://127.0.0.1:8000${endpoint}")"
  if [[ "$INTERNAL_CODE" == "000" || "$INTERNAL_CODE" == "404" ]]; then
    echo "ERROR: predictor returned HTTP $INTERNAL_CODE for dynamic route $endpoint" >&2
    exit 1
  fi
done
echo "Confirmed both dynamic LoRA mutation routes respond inside the predictor"

SVC="${HTTP_SVC:-}"
if [[ -z "$SVC" || "$SVC" == "$GATEWAY" ]]; then
  RESOLVED_SVC="$(kubectl get svc -n "$GATEWAY_NS" \
    -l "gateway.envoyproxy.io/owning-gateway-name=${GATEWAY},gateway.envoyproxy.io/owning-gateway-namespace=${GATEWAY_NS}" \
    -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"
  SVC="${RESOLVED_SVC:-$GATEWAY}"
fi

start_proxy() {
  local port
  for port in 8001 8002 8003 8004 8005; do
    if ! lsof -iTCP:"$port" -sTCP:LISTEN -P -n >/dev/null 2>&1; then
      kubectl proxy --port="$port" >/dev/null 2>&1 &
      PROXY_PID=$!
      sleep 1
      if kill -0 "$PROXY_PID" 2>/dev/null; then
        PROXY_PORT="$port"
        trap 'kill "$PROXY_PID" 2>/dev/null || true' EXIT
        return 0
      fi
    fi
  done
  echo "ERROR: could not start kubectl proxy (8001-8005 busy?)" >&2
  exit 1
}

start_proxy
PROXY_BASE="http://127.0.0.1:${PROXY_PORT}/api/v1/namespaces/${GATEWAY_NS}/services/${SVC}:${SVC_PORT}/proxy"

for endpoint in /v1/load_lora_adapter /v1/unload_lora_adapter; do
  URL="${PROXY_BASE}${SERVICE_PATH}${endpoint}"
  RESPONSE="$(curl -sS -w $'\n%{http_code}' --max-time "$TIMEOUT" \
    -H 'Content-Type: application/json' -d '{}' "$URL" || true)"
  BODY="${RESPONSE%$'\n'*}"
  CODE="${RESPONSE##*$'\n'}"
  if [[ "$CODE" != "404" ]]; then
    echo "ERROR: $endpoint returned HTTP $CODE through the Gateway, want 404" >&2
    echo "$BODY" >&2
    exit 1
  fi
  echo "Gateway blocked $endpoint with HTTP 404"
done

MODELS_URL="${PROXY_BASE}${SERVICE_PATH}/v1/models"
RESPONSE="$(curl -sS -w $'\n%{http_code}' --max-time "$TIMEOUT" "$MODELS_URL" || true)"
BODY="${RESPONSE%$'\n'*}"
CODE="${RESPONSE##*$'\n'}"
if [[ "$CODE" != "200" ]] ||
  ! jq -e --arg adapter "$ADAPTER_NAME" \
    '.data[] | select(.id == $adapter)' <<<"$BODY" >/dev/null; then
  echo "ERROR: normal inference route failed after blocked mutation requests (HTTP $CODE)" >&2
  echo "$BODY" >&2
  exit 1
fi
echo "Normal /v1/models routing still works and adapter remains loaded"
