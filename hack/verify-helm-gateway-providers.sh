#!/usr/bin/env bash
#
# Verify the gateway-provider contract in the generated AIM Engine Helm chart.
# This is intentionally a render-only check: live Envoy integration is covered
# by the Chainsaw CI jobs, which explicitly opt into envoyGateway.

set -euo pipefail

CHART_DIR="${1:-dist/chart}"
INSTALLER_MANIFEST="${2:-dist/install.yaml}"
WORK_DIR="$(mktemp -d)"
trap 'rm -rf "${WORK_DIR}"' EXIT

fail() {
    echo "gateway provider render check failed: $*" >&2
    exit 1
}

matching_lines() {
    local manifest="$1"
    local expression="$2"
    yq eval --no-doc "${expression}" "${manifest}" |
        awk 'NF { count++ } END { print count + 0 }'
}

assert_resource_count() {
    local manifest="$1"
    local kind="$2"
    local name="$3"
    local expected="$4"
    local actual
    actual="$(matching_lines "${manifest}" \
        "select(.kind == \"${kind}\" and .metadata.name == \"${name}\") | .kind")"
    [[ "${actual}" == "${expected}" ]] ||
        fail "${kind}/${name}: expected ${expected}, got ${actual}"
}

render_case() {
    local name="$1"
    local expected_scope="$2"
    local expected_collectors="$3"
    shift 3

    local manifest="${WORK_DIR}/${name}.yaml"
    helm template aim-engine "${CHART_DIR}" "$@" >"${manifest}"

    local scope
    scope="$(
        yq eval --no-doc '
          select(.kind == "Deployment" and .metadata.name == "aim-engine-controller-manager")
          | .spec.template.spec.containers[]
          | select(.name == "manager")
          | .env[]
          | select(.name == "AIM_GATEWAY_ACTIVATION_SCOPE")
          | .value
        ' "${manifest}" | awk 'NF { print; exit }'
    )"
    [[ "${scope}" == "${expected_scope}" ]] ||
        fail "${name}: expected controller scope ${expected_scope}, got ${scope:-<missing>}"

    local collectors
    collectors="$(matching_lines "${manifest}" \
        'select(.metadata.labels."app.kubernetes.io/component" == "gateway-metrics-collector") | .kind')"
    [[ "${collectors}" == "${expected_collectors}" ]] ||
        fail "${name}: expected ${expected_collectors} collector resources, got ${collectors}"

    local platform_resources
    platform_resources="$(matching_lines "${manifest}" \
        'select(.kind == "Gateway" or .kind == "GatewayClass" or .kind == "EnvoyProxy" or .kind == "EnvoyExtensionPolicy") | .kind')"
    [[ "${platform_resources}" == "0" ]] ||
        fail "${name}: chart rendered ${platform_resources} platform-owned gateway resources"
}

render_case default none 0
assert_resource_count "${WORK_DIR}/default.yaml" \
    ServiceAccount aim-engine-controller-manager 1
render_case none-external none 0 \
    --set scaleFromZero.gatewayMetricsCollector.management=external
render_case envoy-helm httproute 2 \
    --set scaleFromZero.gatewayProvider=envoyGateway \
    --set scaleFromZero.gatewayMetricsCollector.management=helm
assert_resource_count "${WORK_DIR}/envoy-helm.yaml" \
    OpenTelemetryCollector aim-engine-envoy-gateway-metrics 1
assert_resource_count "${WORK_DIR}/envoy-helm.yaml" \
    ClusterRole aim-engine-envoy-gateway-metrics-collector 0
assert_resource_count "${WORK_DIR}/envoy-helm.yaml" \
    ClusterRoleBinding aim-engine-envoy-gateway-metrics-collector 0
envoy_automount="$(
    yq eval --no-doc '
      select(.kind == "ServiceAccount" and .metadata.name == "aim-engine-envoy-gateway-metrics-collector")
      | .automountServiceAccountToken
    ' "${WORK_DIR}/envoy-helm.yaml" | awk 'NF { print; exit }'
)"
[[ "${envoy_automount}" == "false" ]] ||
    fail "envoy-helm: collector service account must not mount an API token"
envoy_receivers="$(
    yq eval --no-doc '
      select(.kind == "OpenTelemetryCollector" and .metadata.name == "aim-engine-envoy-gateway-metrics")
      | .spec.config.service.pipelines.metrics.receivers[]
    ' "${WORK_DIR}/envoy-helm.yaml" | awk 'NF { print }'
)"
[[ "${envoy_receivers}" == "otlp" ]] ||
    fail "envoy-helm: expected only the OTLP receiver, got ${envoy_receivers:-<missing>}"
envoy_cumulative_to_delta="$(
    matching_lines "${WORK_DIR}/envoy-helm.yaml" '
      select(.kind == "OpenTelemetryCollector" and .metadata.name == "aim-engine-envoy-gateway-metrics")
      | select(.spec.config.processors.cumulativetodelta != null)
      | .spec.config.processors.cumulativetodelta
    '
)"
[[ "${envoy_cumulative_to_delta}" == "0" ]] ||
    fail "envoy-helm: source-side deltas must not pass through cumulativetodelta"
render_case envoy-external httproute 0 \
    --set scaleFromZero.gatewayProvider=envoyGateway \
    --set scaleFromZero.gatewayMetricsCollector.management=external
render_case kgateway-helm deployment 4 \
    --set scaleFromZero.gatewayProvider=kgateway \
    --set scaleFromZero.gatewayMetricsCollector.management=helm
assert_resource_count "${WORK_DIR}/kgateway-helm.yaml" \
    OpenTelemetryCollector aim-engine-kgateway-metrics 1
render_case kgateway-external deployment 0 \
    --set scaleFromZero.gatewayProvider=kgateway \
    --set scaleFromZero.gatewayMetricsCollector.management=external
if helm template aim-engine "${CHART_DIR}" \
    --set scaleFromZero.gatewayProvider=kgateway \
    --set scaleFromZero.gatewayMetricsCollector.replicas=2 >/dev/null 2>&1; then
    fail "kgateway unexpectedly accepted multiple unsharded collector replicas"
fi
if ! helm template aim-engine "${CHART_DIR}" \
    --set scaleFromZero.gatewayProvider=envoyGateway \
    --set scaleFromZero.gatewayMetricsCollector.replicas=2 >/dev/null 2>&1; then
    fail "Envoy Gateway unexpectedly rejected multiple OTLP receiver replicas"
fi
render_case custom-external custom 0 \
    --set scaleFromZero.gatewayProvider=custom \
    --set scaleFromZero.gatewayMetricsCollector.management=external

if helm template aim-engine "${CHART_DIR}" \
    --set scaleFromZero.gatewayProvider=custom \
    --set scaleFromZero.gatewayMetricsCollector.management=helm >/dev/null 2>&1; then
    fail "custom provider unexpectedly accepted Helm collector management"
fi
if helm template aim-engine "${CHART_DIR}" \
    --set scaleFromZero.gatewayProvider=unknown >/dev/null 2>&1; then
    fail "unknown gateway provider unexpectedly rendered"
fi
if helm template aim-engine "${CHART_DIR}" \
    --set scaleFromZero.gatewayMetricsCollector.management=unknown >/dev/null 2>&1; then
    fail "unknown collector management mode unexpectedly rendered"
fi

if [[ -f "${INSTALLER_MANIFEST}" ]]; then
    installer_gateway_resources="$(matching_lines "${INSTALLER_MANIFEST}" \
        'select(.kind == "OpenTelemetryCollector" or .kind == "Gateway" or .kind == "GatewayClass" or .kind == "EnvoyProxy" or .kind == "EnvoyExtensionPolicy" or .metadata.labels."app.kubernetes.io/component" == "gateway-metrics-collector") | .kind')"
    [[ "${installer_gateway_resources}" == "0" ]] ||
        fail "${INSTALLER_MANIFEST}: base installer contains ${installer_gateway_resources} gateway-specific resources"
fi

echo "Gateway provider render matrix passed."
