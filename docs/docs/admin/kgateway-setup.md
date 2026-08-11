# kgateway Setup

Use this setup when AIM Engine routes services through
[kgateway](https://kgateway.dev). The configuration below is tested with:

- kgateway and kgateway CRDs v2.1.2;
- Gateway API v1.2.0;
- cert-manager v1.16 or later.

## Install kgateway

Install the Gateway API CRDs, followed by the kgateway CRDs and controller:

```bash
kubectl apply --server-side \
  -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.2.0/standard-install.yaml

helm upgrade --install kgateway-crd \
  oci://cr.kgateway.dev/kgateway-dev/charts/kgateway-crds \
  --version v2.1.2 \
  --namespace kgateway-system \
  --create-namespace \
  --wait

helm upgrade --install kgateway \
  oci://cr.kgateway.dev/kgateway-dev/charts/kgateway \
  --version v2.1.2 \
  --namespace kgateway-system \
  --set agentgateway.enabled=true
```

Create a Gateway for AIM Engine routes:

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: kserve-ingress-gateway
  namespace: kgateway-system
spec:
  gatewayClassName: kgateway
  listeners:
    - name: http
      protocol: HTTP
      port: 80
      allowedRoutes:
        namespaces:
          from: All
```

## Configure AIM Engine

Configure routing and select kgateway as the activation provider. The chart
then derives the deployment-scoped metric contract and renders the matching
kgateway collector:

```bash
helm upgrade aim-engine oci://docker.io/amdenterpriseai/aim-engine-chart \
  --namespace aim-system \
  --set clusterRuntimeConfig.enable=true \
  --set clusterRuntimeConfig.spec.routing.enabled=true \
  --set clusterRuntimeConfig.spec.routing.gatewayRef.name=kserve-ingress-gateway \
  --set clusterRuntimeConfig.spec.routing.gatewayRef.namespace=kgateway-system \
  --set scaleFromZero.gatewayProvider=kgateway \
  --set scaleFromZero.gatewayMetricsCollector.kgateway.gatewayName=kserve-ingress-gateway
```

Keep `scaleFromZero.gatewayMetricsCollector.replicas=1` for kgateway. Its
Prometheus targets are not sharded, so multiple collector replicas would
duplicate the same series; the chart rejects that configuration.

The provider and collector management settings are independent. If the
platform manages the collector outside the AIM Engine release, set
`management=external` and install the standalone manifest matching the AIM
Engine release. When migrating from legacy Helm keys, follow the
[upgrade value migration](upgrading.md#scale-from-zero-helm-value-migration)
instead of using `--reuse-values`.

```bash
helm upgrade aim-engine oci://docker.io/amdenterpriseai/aim-engine-chart \
  --namespace aim-system \
  --reuse-values \
  --set scaleFromZero.gatewayProvider=kgateway \
  --set scaleFromZero.gatewayMetricsCollector.management=external

curl -LO https://github.com/amd-enterprise-ai/aim-engine/releases/download/<version>/kgateway-metrics-collector.yaml
kubectl apply -f kgateway-metrics-collector.yaml
kubectl -n keda rollout status deploy/kgateway-metrics-collector
```

The activation scope and collector must use the same metric contract. The
kgateway collector exports
`envoy_cluster_external_upstream_rq_completed` with `namespace` and
`deployment` attributes; AIM Engine selects those attributes when
`gatewayProvider=kgateway`.

To replace an existing kgateway installation without interrupting
scale-from-zero services, follow
[Migrate from kgateway to Envoy Gateway](migrating-kgateway-to-envoy-gateway.md).
