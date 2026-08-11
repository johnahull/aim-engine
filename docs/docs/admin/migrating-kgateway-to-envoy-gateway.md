# Migrate from kgateway to Envoy Gateway

This guide migrates AIM Engine routing and scale-from-zero activation from
kgateway to Envoy Gateway. The two providers use different activation metrics:

- kgateway exposes deployment-scoped cumulative counters that its collector
  scrapes and converts to deltas.
- Envoy Gateway exposes route-scoped counters and pushes source-side deltas over
  OTLP.

Treat this as a coordinated platform and AIM Engine configuration change. Do
not uninstall kgateway until every route and scale-from-zero service has been
verified on Envoy Gateway.

## Before you begin

Review the [Envoy Gateway scale-from-zero guide](envoy-gateway-scale-from-zero.md)
and confirm the required Envoy Gateway, OpenTelemetry Operator, KEDA, and
`keda-otel-add-on` versions are available.

Inventory:

- AIMServices with `minReplicas: 0`.
- Namespace, cluster, and service-level RuntimeConfigs that set `gatewayRef`.
- Explicit `scaleFromZero.activationMetricQueryTemplate` overrides. A custom
  query takes precedence over the provider default and must be removed or
  replaced during migration.
- Other workloads that still use the kgateway Gateway or controller.
- Every AIM Engine release sharing the Gateway and activation collector.

Use a new Envoy Gateway name or namespace while both providers coexist. This
makes rollback explicit and prevents an in-place Gateway replacement from
temporarily detaching routes.

## 1. Keep affected services running

Before changing the activation path, update every affected AIMService declaration
to use `minReplicas: 1` or greater and wait for its predictor to be ready. Make
the change in the source of truth used by Helm or GitOps, not only with an
imperative patch.

This prevents a service from becoming unreachable while the Gateway, collector,
and generated KEDA trigger are changing.

## 2. Prepare Envoy Gateway

Complete sections 1 through 3 of the
[Envoy Gateway scale-from-zero guide](envoy-gateway-scale-from-zero.md):

1. Install Envoy Gateway with Lua extensions enabled.
2. Create an `EnvoyProxy`, `GatewayClass`, and Gateway.
3. Configure the `EnvoyProxy` OTLP sink with
   `reportCountersAsDeltas: true`.
4. Install and verify the Gateway-scoped `EnvoyExtensionPolicy`.
5. Allow proxy-to-collector traffic on TCP 4317.

The OTLP sink host must match the collector that the migrated AIM Engine release
will manage. For release `aim-engine` in namespace `aim-system`, the default is:

```text
aim-engine-envoy-gateway-metrics-collector.aim-system.svc.cluster.local:4317
```

It is safe for this Service to appear only when the AIM Engine release is
upgraded in the next step because affected services are still pinned above zero.

## 3. Switch AIM Engine

Create a migration values file:

```yaml
scaleFromZero:
  gatewayProvider: envoyGateway
  gatewayMetricsCollector:
    management: helm

clusterRuntimeConfig:
  enable: true
  spec:
    routing:
      enabled: true
      gatewayRef:
        name: kserve-ingress-gateway
        namespace: envoy-gateway-system
```

If routing is configured on individual AIMServices or another RuntimeConfig,
update those `gatewayRef` values instead. Remove any kgateway-specific
`activationMetricQueryTemplate` override so AIM Engine can derive the Envoy
route-scoped query.

Apply the values:

```bash
helm upgrade aim-engine \
  oci://docker.io/amdenterpriseai/aim-engine-chart \
  --version <chart-version> \
  --namespace aim-system \
  --reuse-values \
  --values migrate-to-envoy-gateway.yaml \
  --wait \
  --timeout 10m
```

With Helm-managed collectors, this replaces the kgateway collector resources
with the Envoy OTLP collector. If collectors are platform-managed, set
`management=external`, install the standalone Envoy collector first, and remove
the standalone kgateway collector only after verification.

## 4. Verify routing and metrics

Verify the new collector and Gateway:

```bash
kubectl rollout status \
  deployment/aim-engine-envoy-gateway-metrics-collector \
  --namespace aim-system \
  --timeout=3m

kubectl wait --for=condition=Programmed \
  gateway/kserve-ingress-gateway \
  --namespace envoy-gateway-system \
  --timeout=5m

kubectl get httproute --all-namespaces
```

Confirm each migrated `HTTPRoute` references the Envoy Gateway and reports
`Accepted=True` and `ResolvedRefs=True`. Send a request through the Gateway and
verify that the collector receives a route-specific
`envoy_http_lua_aim_activation_requests_*` metric.

## 5. Re-enable scale-to-zero gradually

Change one canary AIMService back to `minReplicas: 0` and verify the complete
lifecycle:

1. The predictor scales from one replica to zero.
2. A request reaches the service's `HTTPRoute`.
3. The first request may receive a cold-start `503`.
4. KEDA scales the predictor from zero to one.
5. A client retry succeeds.
6. `ActivationMetricAvailable=True` appears after the HPA reports the exact
   `s0-*` metric.

Repeat for the remaining services only after the canary succeeds.

## 6. Retire kgateway

After all services are verified:

1. Confirm no `HTTPRoute` still references the kgateway Gateway.
2. Remove an externally managed kgateway collector, if present.
3. Remove the old kgateway Gateway.
4. Uninstall the kgateway controller and CRDs only when no other platform
   workload uses them.
5. Remove obsolete kgateway collector RBAC left by standalone `kubectl apply`
   installations.

## Roll back

If Envoy routing or activation fails:

1. Set affected AIMServices to `minReplicas: 1`.
2. Restore their kgateway `gatewayRef` values.
3. Restore `scaleFromZero.gatewayProvider=kgateway` and the matching collector
   management settings.
4. Wait for routes and the kgateway collector to become healthy.
5. Re-enable `minReplicas: 0` only after a cold-start activation test succeeds.

Keeping kgateway installed until the migration is complete makes this rollback
possible without reinstalling the old data plane.
