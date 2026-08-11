# Optional Envoy Gateway scale-from-zero

Envoy Gateway integration is optional. A default AIM Engine installation uses
`scaleFromZero.gatewayProvider=none`, does not install a gateway metrics
collector, and continues to support fixed replicas and autoscaling with
`minReplicas >= 1`.

Follow this guide only when all of the following are true:

- AIMServices route through Envoy Gateway.
- At least one AIMService uses `minReplicas: 0`.
- AIM Engine should manage the Envoy activation metrics collector.

For an existing kgateway installation, use
[Migrate from kgateway to Envoy Gateway](migrating-kgateway-to-envoy-gateway.md)
to coordinate the routing and activation-metric changes.

## Architecture and ownership

Scale-from-zero needs three Envoy-specific components:

1. A platform-owned `EnvoyExtensionPolicy` attached to each Gateway used by
   AIMServices. It counts requests before Envoy returns a zero-endpoint `503`.
2. A platform-owned `EnvoyProxy` metrics sink that converts the proxy-owned
   cumulative counters to deltas and pushes them over OTLP.
3. An OpenTelemetry collector that receives those deltas, normalizes their
   names, and forwards them to `keda-otel-add-on`.

The AIM Engine chart manages the collector when
`scaleFromZero.gatewayProvider=envoyGateway` and
`scaleFromZero.gatewayMetricsCollector.management=helm`. It does not create the
Gateway or Gateway-targeted policy, and it does not modify the platform-owned
`EnvoyProxy`.

Use one activation sink per `EnvoyProxy`. When multiple AIM Engine releases
share a Gateway, designate one collector for that Gateway (or use a shared
externally managed collector) and set collector management to `external` in the
other releases. Sending the same deltas through multiple collectors to one
keda-otel-add-on instance would duplicate activation samples.

## Prerequisites

The tested dependency combination is:

- Envoy Gateway v1.8.2
- KEDA 2.18+
- `keda-otel-add-on`
- OpenTelemetry Operator 0.101+
- cert-manager

Cluster NetworkPolicies must allow the Envoy proxy pods to reach the collector
Service on TCP 4317 and the collector to reach keda-otel-add-on. This path does
not require Kubernetes API discovery or pod-list RBAC.

AIM Engine and its CRDs should already be installed. The general installation
procedure is covered in [Installation](../getting-started/installation.md).

## 1. Install Envoy Gateway

Install Envoy Gateway with Lua extensions enabled:

```bash
helm upgrade --install envoy-gateway \
  oci://docker.io/envoyproxy/gateway-helm \
  --version v1.8.2 \
  --namespace envoy-gateway-system \
  --create-namespace \
  --set config.envoyGateway.extensionApis.disableLua=false \
  --wait \
  --timeout 10m
```

## 2. Create an Envoy Gateway

Envoy Gateway v1.8 does not yet accept Envoy 1.38's `handle:stats()` API under
strict Lua validation. The `EnvoyProxy` used by the Gateway must therefore set
`luaValidation: InsecureSyntax`. It must also push source-side counter deltas to
the collector with `reportCountersAsDeltas: true`; collector-side conversion is
not restart-safe.

Apply the following example, or add the equivalent setting to the
`EnvoyProxy` and `GatewayClass` that own an existing Gateway:

```yaml
apiVersion: gateway.envoyproxy.io/v1alpha1
kind: EnvoyProxy
metadata:
  name: aim-engine-proxy
  namespace: envoy-gateway-system
spec:
  luaValidation: InsecureSyntax
  telemetry:
    metrics:
      sinks:
        - type: OpenTelemetry
          openTelemetry:
            # This example assumes release aim-engine in namespace aim-system.
            host: aim-engine-envoy-gateway-metrics-collector.aim-system.svc.cluster.local
            port: 4317
            reportCountersAsDeltas: true
  provider:
    type: Kubernetes
    kubernetes:
      envoyService:
        type: ClusterIP
---
apiVersion: gateway.networking.k8s.io/v1
kind: GatewayClass
metadata:
  name: envoy-gateway
spec:
  controllerName: gateway.envoyproxy.io/gatewayclass-controller
  parametersRef:
    group: gateway.envoyproxy.io
    kind: EnvoyProxy
    name: aim-engine-proxy
    namespace: envoy-gateway-system
---
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: kserve-ingress-gateway
  namespace: envoy-gateway-system
spec:
  gatewayClassName: envoy-gateway
  listeners:
    - name: http
      protocol: HTTP
      port: 80
      allowedRoutes:
        namespaces:
          from: All
```

The collector Service name is `<collector-resource-name>-collector`. If the
AIM Engine release uses another namespace, change the sink host accordingly.
For the standalone collector manifest defaults, use
`envoy-gateway-metrics-collector.keda.svc.cluster.local`.

Wait for the Gateway to become ready:

```bash
kubectl wait --for=condition=Accepted gatewayclass/envoy-gateway \
  --timeout=2m
kubectl wait --for=condition=Programmed \
  gateway/kserve-ingress-gateway \
  --namespace envoy-gateway-system \
  --timeout=5m
```

## 3. Install the activation policy

Download the policy matching the AIM Engine release. Change
`metadata.namespace` and `spec.targetRefs[0].name` if the Gateway does not use
the example namespace and name:

```bash
curl -LO \
  https://github.com/amd-enterprise-ai/aim-engine/releases/download/<release-tag>/envoy-gateway-route-metrics.yaml
kubectl apply -f envoy-gateway-route-metrics.yaml
```

Restrict write access to `EnvoyExtensionPolicy` resources to platform
administrators. A route-level extension policy can override this shared
Gateway-level policy.

Confirm that Envoy Gateway accepted the policy:

```bash
kubectl get envoyextensionpolicy route-activation-metrics \
  --namespace envoy-gateway-system \
  -o jsonpath='{.status.ancestors[0].conditions[?(@.type=="Accepted")].status}{"\n"}'
```

The expected value is `True`.

## 4. Enable the Helm-managed collector

Create an add-on values file:

```yaml
scaleFromZero:
  gatewayProvider: envoyGateway
  gatewayMetricsCollector:
    management: helm

# Optional cluster-wide routing defaults. Omit this block when routing is
# already supplied by an AIMRuntimeConfig, AIMClusterRuntimeConfig, or service.
clusterRuntimeConfig:
  enable: true
  spec:
    routing:
      enabled: true
      gatewayRef:
        name: kserve-ingress-gateway
        namespace: envoy-gateway-system
      pathTemplate: "/{.metadata.namespace}/{.metadata.name}"
```

Apply it to the existing AIM Engine release:

```bash
helm upgrade aim-engine \
  oci://docker.io/amdenterpriseai/aim-engine-chart \
  --version <chart-version> \
  --namespace aim-system \
  --reuse-values \
  --values envoy-scale-from-zero-values.yaml \
  --wait \
  --timeout 10m
```

The chart requires the OpenTelemetry Operator CRDs when collector management is
`helm`. Use `management=external` only when platform infrastructure installs
the standalone collector manifest instead. The Envoy collector does not need
Kubernetes discovery RBAC; Envoy pushes to its Service directly.

## 5. Verify the add-on

Verify the Helm-managed collector in the AIM Engine release namespace:

```bash
kubectl get opentelemetrycollector \
  aim-engine-envoy-gateway-metrics \
  --namespace aim-system

kubectl rollout status \
  deployment/aim-engine-envoy-gateway-metrics-collector \
  --namespace aim-system \
  --timeout=3m
```

After an AIMService has created an `HTTPRoute` and a request has passed through
the Gateway, inspect the collector:

```bash
kubectl logs \
  deployment/aim-engine-envoy-gateway-metrics-collector \
  --namespace aim-system \
  --tail=100
```

The Lua policy creates each route-specific
`http.lua.aim_activation_requests.*` counter lazily on the first request. The
collector normalizes that native OTLP name to the existing
`envoy_http_lua_aim_activation_requests_*` KEDA query contract.
Envoy Gateway v1.8.2 flushes this sink approximately every five seconds, so a
single cold request may wait up to one flush interval before KEDA can observe
it. Envoy Gateway v1.8 does not expose a sink-specific stat matcher; the proxy
therefore sends its complete metrics payload and the collector drops
non-activation metrics. Account for that internal traffic when sizing the
collector. Source-side deltas prevent historical replay, but they are not a
durable request queue: an activation emitted during a complete collector outage
can be lost. Use more than one Envoy collector replica when receiver
availability is required, keep services above zero during planned pipeline
maintenance, and retain client retries for cold-start `503` responses.

## Disable the add-on

Before disabling the provider, change every affected AIMService to
`minReplicas >= 1`. Then restore the default:

```bash
helm upgrade aim-engine \
  oci://docker.io/amdenterpriseai/aim-engine-chart \
  --version <chart-version> \
  --namespace aim-system \
  --reuse-values \
  --set scaleFromZero.gatewayProvider=none
```

Remove the `EnvoyExtensionPolicy` and OTLP metrics sink only after no remaining
AIM Engine release uses that Gateway for scale-from-zero.
