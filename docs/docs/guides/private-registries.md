# Private Registries

This guide covers configuring authentication for private container registries, HuggingFace Hub, and S3-compatible storage.

:::{admonition} v1alpha2
:class: note

`AIMService` examples use `aim.eai.amd.com/v1alpha2`. `AIMRuntimeConfig` / `AIMClusterRuntimeConfig` remain `aim.eai.amd.com/v1alpha1` resources — both pipelines consume them unchanged. `spec.imagePullSecrets`, `spec.serviceAccountName`, and the runtime-config env fields are identical regardless of pipeline.
:::
## Container Image Pull Secrets

### Per-Service Secrets

Provide image pull secrets directly on the service:

```yaml
apiVersion: aim.eai.amd.com/v1alpha2
kind: AIMService
metadata:
  name: qwen-chat
  namespace: ml-team
spec:
  model:
    name: qwen-qwen3-32b
  imagePullSecrets:
    - name: my-registry-creds
```

The secret must exist in the same namespace as the service.

### Model Source Secrets

For `AIMClusterModelSource` pulling from private registries, secrets must be in the operator namespace:

```yaml
apiVersion: aim.eai.amd.com/v1alpha1
kind: AIMClusterModelSource
metadata:
  name: private-models
spec:
  registry: ghcr.io
  imagePullSecrets:
    - name: ghcr-pull-secret
  images:
    - "my-org/private-model:1.2.3"
```

## HuggingFace Authentication

For models sourced from private HuggingFace repositories (`hf://` URLs), provide a token via environment variables in the runtime configuration:

```yaml
apiVersion: aim.eai.amd.com/v1alpha1
kind: AIMRuntimeConfig
metadata:
  name: default
  namespace: ml-team
spec:
  env:
    - name: HF_TOKEN
      valueFrom:
        secretKeyRef:
          name: hf-credentials
          key: token
```

Create the secret:

```bash
kubectl create secret generic hf-credentials \
  --from-literal=token=hf_your_token_here \
  -n ml-team
```

## S3-Compatible Storage

Model artifacts with an `s3://` source URI are downloaded by a boto3-backed
client (Signature V4). Validated end-to-end against MinIO; SeaweedFS is also
exercised as the built-in artifact cache backend. Other S3-v4 backends (AWS S3,
Cloudflare R2, Backblaze B2, GCS, Ceph RGW) use the same boto3 path and are
expected to work — point `AWS_ENDPOINT_URL` at your gateway and provide
credentials.

Configure credentials via environment variables:

```yaml
apiVersion: aim.eai.amd.com/v1alpha1
kind: AIMRuntimeConfig
metadata:
  name: default
  namespace: ml-team
spec:
  env:
    - name: AWS_ACCESS_KEY_ID
      valueFrom:
        secretKeyRef:
          name: s3-credentials
          key: access-key
    - name: AWS_SECRET_ACCESS_KEY
      valueFrom:
        secretKeyRef:
          name: s3-credentials
          key: secret-key
    - name: AWS_ENDPOINT_URL
      value: "https://s3.example.com"
```

!!! warning "Where to put download credentials"
    `AIMRuntimeConfig.spec.env` is applied to **both** the download Job and the
    inference pod. For credentials that should only reach the downloader, prefer
    a download-only channel: `AIMArtifact.spec.env`, an `AIMModel` model source's
    `env`, or (v1alpha2) `AIMProfile.spec.caching.env`. An `AIMService.spec.env`
    is applied to the inference pod only and does **not** reach the downloader.

### How S3 credentials are resolved

The downloader selects one of three modes:

| Mode | Selected when | Behaviour |
| --- | --- | --- |
| Static keys | `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY` both hold real values | Signs with those keys, plus `AWS_SESSION_TOKEN` if set. |
| Anonymous | `AIM_S3_ANONYMOUS=true`, or either key is set to `anonymous` or to an empty string | Sends unsigned requests. Use for public buckets. |
| Credential chain | Neither key is set | Hands resolution to boto3: web identity (IRSA), EKS Pod Identity, ECS task role, EC2 instance profile, shared profile, credential process. |

Setting only one of the two keys is rejected up front, as is combining a real
value with an `anonymous` sentinel, so a partial configuration fails with a clear
message instead of a signing error on the first request.

!!! warning "Absent credentials do not mean anonymous"
    Leaving both key variables unset means "resolve credentials the normal way",
    because that is exactly how every role-based source presents itself — IRSA,
    Pod Identity and instance profiles all deliberately leave
    `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY` empty. Treating that as a
    public bucket would send unsigned requests and turn a private bucket into an
    opaque `403 AccessDenied`. To read a public bucket, say so explicitly with
    `AIM_S3_ANONYMOUS=true`.

!!! note "Role-based credentials need a ServiceAccount you can annotate"
    Chain mode resolves whatever the download Pod's environment offers. Download
    Jobs currently run under the namespace's `default` ServiceAccount and
    `AIMArtifact` has no field to select a different one, so IRSA and EKS Pod
    Identity require annotating that `default` ServiceAccount — which grants the
    role to every Pod in the namespace. EC2 instance profiles, shared profiles
    and credential processes need no ServiceAccount changes.

### S3 control knobs

The downloader recognises these optional environment variables, set on the same
(download-only) channel as the credentials, for backends that need tuning:

| Variable | Default | Purpose |
| --- | --- | --- |
| `AIM_S3_ANONYMOUS` | unset (credentials are resolved) | Set to `true` to read a public bucket with unsigned requests. |
| `AWS_REGION` / `AWS_DEFAULT_REGION` | `us-east-1` when a custom endpoint is set, otherwise left to boto3 | Signing region (most non-AWS backends ignore it; R2 aliases `us-east-1` → `auto`). On real AWS it is left unset so the bucket's region is discovered rather than pinned wrong. |
| `AIM_S3_ADDRESSING_STYLE` | `path` when a custom endpoint is set, else `auto` | Force `path`, `virtual`, or `auto` bucket addressing. |
| `AIM_S3_SIGNATURE_VERSION` | boto3 default (V4) | Override the signing version (e.g. `s3v4`). Rarely needed. |
| `AIM_S3_MAX_WORKERS` | `8` | Number of objects downloaded in parallel. |
| `AIM_S3_MAX_CONCURRENCY` | boto3 default (`10`) | Multipart threads per object (throughput for large files). |
| `AIM_S3_MULTIPART_CHUNKSIZE_MB` | boto3 default (`8`) | Multipart chunk size in MiB. |

!!! note "Effective connection fan-out"
    Total in-flight connections is roughly `AIM_S3_MAX_WORKERS` × `AIM_S3_MAX_CONCURRENCY` (default 8 × 10 = 80). On small/constrained gateways (single-node MinIO, on-prem RGW) consider lowering one of these.

!!! tip "Keep secrets out of the manifest"
    Reference the secret with `valueFrom.secretKeyRef` (as in the example above)
    rather than inlining the value, so it never appears in the resource. Any of
    the credential variables also accept a `_FILE` suffix pointing at a mounted
    file — e.g. `AWS_SECRET_ACCESS_KEY_FILE=/var/run/secrets/s3/secret-key` —
    which the downloader reads and strips, keeping the secret out of the Pod
    environment entirely. Note that the AIM CRDs offer no way to mount a volume
    into a download Job today, so `_FILE` is only usable where something else in
    your platform provides the mount.

## Credential Scope

Environment variables from runtime configurations are merged in this order (highest to lowest priority):

1. `AIMService.spec.env` — per-service
2. `AIMRuntimeConfig.spec.env` — per-namespace
3. `AIMClusterRuntimeConfig.spec.env` — cluster-wide

This allows you to set cluster-wide defaults (e.g., HuggingFace token) and override per namespace or service when needed.

## Troubleshooting

### Image Pull Errors

Check pod events for pull failures:

```bash
kubectl get pods -l serving.kserve.io/inferenceservice=<service-name> -n <namespace>
kubectl describe pod <pod-name> -n <namespace>
```

Common causes:

- Secret doesn't exist in the correct namespace
- Secret has incorrect credentials
- Registry URL is wrong in the model image

### Download Authentication Failures

Check artifact download job logs:

```bash
kubectl get jobs -l aim.eai.amd.com/artifact=<artifact-name> -n <namespace>
kubectl logs job/<job-name> -n <namespace>
```

## Next Steps

- [Multi-Tenancy](multi-tenancy.md) — Per-namespace credential isolation
- [Runtime Configuration](../concepts/runtime-config.md) — Environment variable resolution
