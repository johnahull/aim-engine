#!/usr/bin/env bash
#
# Prepare the gateway side of the Kustomize-backed Tilt development flow.
# Envoy Gateway and its Gateway-targeted extension remain platform-owned; Tilt
# verifies them and ensures the standalone external collector is installed.

set -euo pipefail

OPERATOR_NAMESPACE="${OPERATOR_NAMESPACE:-aim-system}"
OPERATOR_DEPLOYMENT="${OPERATOR_DEPLOYMENT:-aim-engine-controller-manager}"
HELM_COLLECTOR="${HELM_COLLECTOR:-aim-engine-envoy-gateway-metrics}"
GATEWAY_NAMESPACE="${GATEWAY_NAMESPACE:-envoy-gateway-system}"
GATEWAY_POLICY="${GATEWAY_POLICY:-route-activation-metrics}"

helm_release="$(
    kubectl get deployment "${OPERATOR_DEPLOYMENT}" \
        --namespace "${OPERATOR_NAMESPACE}" \
        --output jsonpath='{.metadata.annotations.meta\.helm\.sh/release-name}' \
        2>/dev/null || true
)"
if [[ -n "${helm_release}" ]]; then
    cat >&2 <<EOF
Tilt cannot deploy its Kustomize-managed controller while Helm release
"${helm_release}" owns ${OPERATOR_NAMESPACE}/${OPERATOR_DEPLOYMENT}.

Uninstall that AIM Engine Helm release before starting Tilt. Tilt deliberately
uses the external collector path; release and CI testing cover Helm ownership.
EOF
    exit 1
fi

if kubectl get opentelemetrycollector "${HELM_COLLECTOR}" \
    --namespace "${OPERATOR_NAMESPACE}" >/dev/null 2>&1; then
    cat >&2 <<EOF
Tilt found Helm-style collector ${OPERATOR_NAMESPACE}/${HELM_COLLECTOR}.
Remove its owning AIM Engine release/resources before starting Tilt to avoid
ambiguous ownership of the Envoy OTLP activation pipeline.
EOF
    exit 1
fi

if ! kubectl get envoyextensionpolicy "${GATEWAY_POLICY}" \
    --namespace "${GATEWAY_NAMESPACE}" >/dev/null 2>&1; then
    cat >&2 <<EOF
Missing platform-owned EnvoyExtensionPolicy
${GATEWAY_NAMESPACE}/${GATEWAY_POLICY}.

Prepare the development platform with "make kind-create" or
"make vcluster-create". Tilt verifies the extension but does not install or own
Gateway-targeted platform resources.
EOF
    exit 1
fi

mise exec -- make verify-gateway
kubectl wait \
    --for=jsonpath='{.status.ancestors[0].conditions[?(@.type=="Accepted")].status}'=True \
    "envoyextensionpolicy/${GATEWAY_POLICY}" \
    --namespace "${GATEWAY_NAMESPACE}" \
    --timeout=180s

mise exec -- make install-dev-envoy-collector
