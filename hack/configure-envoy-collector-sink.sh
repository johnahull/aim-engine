#!/usr/bin/env bash
#
# Point the development EnvoyProxy at a ready OTLP activation collector and
# wait for the managed proxy rollout. Production installations configure their
# platform-owned EnvoyProxy through their normal infrastructure workflow.

set -euo pipefail

GATEWAY_NAMESPACE="${GATEWAY_NAMESPACE:-envoy-gateway-system}"
ENVOY_PROXY_NAME="${ENVOY_PROXY_NAME:-kserve-proxy-config}"
COLLECTOR_HOST="${COLLECTOR_HOST:?COLLECTOR_HOST is required}"
COLLECTOR_PORT="${COLLECTOR_PORT:-4317}"

PROXY_UID_BEFORE=""
for _ in $(seq 1 90); do
    PROXY_UID_BEFORE="$(
        kubectl -n "${GATEWAY_NAMESPACE}" get pod \
            -l app.kubernetes.io/component=proxy \
            -o jsonpath='{.items[0].metadata.uid}' 2>/dev/null || true
    )"
    if [[ -n "${PROXY_UID_BEFORE}" ]]; then
        break
    fi
    sleep 2
done
if [[ -z "${PROXY_UID_BEFORE}" ]]; then
    echo "Envoy proxy pod did not appear in ${GATEWAY_NAMESPACE}" >&2
    exit 1
fi

CURRENT_HOST="$(
    kubectl -n "${GATEWAY_NAMESPACE}" get envoyproxy "${ENVOY_PROXY_NAME}" \
        -o jsonpath='{.spec.telemetry.metrics.sinks[0].openTelemetry.host}' \
        2>/dev/null || true
)"
CURRENT_PORT="$(
    kubectl -n "${GATEWAY_NAMESPACE}" get envoyproxy "${ENVOY_PROXY_NAME}" \
        -o jsonpath='{.spec.telemetry.metrics.sinks[0].openTelemetry.port}' \
        2>/dev/null || true
)"
CURRENT_DELTAS="$(
    kubectl -n "${GATEWAY_NAMESPACE}" get envoyproxy "${ENVOY_PROXY_NAME}" \
        -o jsonpath='{.spec.telemetry.metrics.sinks[0].openTelemetry.reportCountersAsDeltas}' \
        2>/dev/null || true
)"
CURRENT_READY="$(
    kubectl -n "${GATEWAY_NAMESPACE}" get pod \
        -l app.kubernetes.io/component=proxy \
        -o jsonpath='{.items[0].status.containerStatuses[0].ready}' \
        2>/dev/null || true
)"
CONFIG_ALREADY_MATCHES=false
if [[ "${CURRENT_HOST}" == "${COLLECTOR_HOST}" \
    && "${CURRENT_PORT}" == "${COLLECTOR_PORT}" \
    && "${CURRENT_DELTAS}" == "true" ]]; then
    CONFIG_ALREADY_MATCHES=true
    if [[ "${CURRENT_READY}" == "true" ]]; then
        echo "EnvoyProxy ${GATEWAY_NAMESPACE}/${ENVOY_PROXY_NAME} already exports deltas to ${COLLECTOR_HOST}:${COLLECTOR_PORT}."
        exit 0
    fi
fi

PATCH="$(
    jq -cn \
        --arg host "${COLLECTOR_HOST}" \
        --argjson port "${COLLECTOR_PORT}" \
        '{
          spec: {
            telemetry: {
              metrics: {
                sinks: [{
                  type: "OpenTelemetry",
                  openTelemetry: {
                    host: $host,
                    port: $port,
                    reportCountersAsDeltas: true
                  }
                }]
              }
            }
          }
        }'
)"

kubectl -n "${GATEWAY_NAMESPACE}" patch envoyproxy "${ENVOY_PROXY_NAME}" \
    --type=merge \
    -p="${PATCH}"

for _ in $(seq 1 90); do
    PROXY_UID_AFTER="$(
        kubectl -n "${GATEWAY_NAMESPACE}" get pod \
            -l app.kubernetes.io/component=proxy \
            -o jsonpath='{.items[0].metadata.uid}' 2>/dev/null || true
    )"
    PROXY_READY="$(
        kubectl -n "${GATEWAY_NAMESPACE}" get pod \
            -l app.kubernetes.io/component=proxy \
            -o jsonpath='{.items[0].status.containerStatuses[0].ready}' \
            2>/dev/null || true
    )"
    if [[ -n "${PROXY_UID_AFTER}" \
        && "${PROXY_READY}" == "true" \
        && ( "${CONFIG_ALREADY_MATCHES}" == "true" \
            || "${PROXY_UID_AFTER}" != "${PROXY_UID_BEFORE}" ) ]]; then
        echo "EnvoyProxy ${GATEWAY_NAMESPACE}/${ENVOY_PROXY_NAME} now exports deltas to ${COLLECTOR_HOST}:${COLLECTOR_PORT}."
        exit 0
    fi
    sleep 2
done

echo "Envoy proxy did not adopt collector sink ${COLLECTOR_HOST}:${COLLECTOR_PORT}" >&2
kubectl -n "${GATEWAY_NAMESPACE}" get envoyproxy,deploy,pod -o wide >&2
exit 1
