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
client (Signature V4). Validated end-to-end against MinIO. Other S3-v4 backends
(AWS S3, Cloudflare R2, Backblaze B2, GCS, Ceph RGW) use the same boto3 path
and are expected to work with the corresponding endpoint and credentials.

Prefer the typed S3 configuration. It keeps credentials out of Pod environment
variables, mounts custom trust roots consistently into size-check and download
Jobs, and applies the connection only to `s3://` artifacts:

```yaml
apiVersion: aim.eai.amd.com/v1alpha1
kind: AIMRuntimeConfig
metadata:
  name: default
  namespace: ml-team
spec:
  artifact:
    s3:
      endpoint: https://s3.example.com
      region: us-east-1
      addressingStyle: path
      auth:
        mode: static
        credentialsSecretRef:
          name: s3-credentials
          accessKeyIdKey: accessKeyId
          secretAccessKeyKey: secretAccessKey
      tls:
        caBundleRef:
          kind: ConfigMap
          name: s3-ca
          key: ca.crt
```

Both references are namespace-local. The Secret values are mounted as files and
never copied into the Job specification:

```bash
kubectl create secret generic s3-credentials \
  --from-literal=accessKeyId='ACCESS_KEY' \
  --from-literal=secretAccessKey='SECRET_KEY' \
  -n ml-team

kubectl create configmap s3-ca \
  --from-file=ca.crt=/path/to/internal-ca.pem \
  -n ml-team
```

The namespace-level S3 object replaces the cluster-level object as one complete
connection. Endpoint, credentials, and CA settings are never field-merged
across those trust boundaries.

### How S3 credentials are resolved

Typed `auth.mode` selects one of three modes:

| Mode | Behaviour |
| --- | --- |
| `static` | Mounts the referenced Secret and signs with those keys, plus the optional session token. |
| `anonymous` | Sends unsigned requests. Use only for public buckets. |
| `chain` | Uses boto3's standard provider chain: web identity (IRSA), EKS Pod Identity, ECS task role, EC2 instance profile, shared profile, or credential process. This is the default. |

Existing environment-based configuration remains supported. When no typed
connection is present, setting both `AWS_ACCESS_KEY_ID` and
`AWS_SECRET_ACCESS_KEY` selects static credentials,
`AIM_S3_ANONYMOUS=true` selects anonymous access, and leaving both keys absent
selects the credential chain. Typed fields take precedence over corresponding
legacy variables.

!!! warning "Absent credentials do not mean anonymous"
    Leaving both key variables unset means "resolve credentials the normal way",
    because that is exactly how every role-based source presents itself — IRSA,
    Pod Identity and instance profiles all deliberately leave
    `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY` empty. Treating that as a
    public bucket would send unsigned requests and turn a private bucket into an
    opaque `403 AccessDenied`. To read a public bucket, explicitly set
    `auth.mode: anonymous` (or `AIM_S3_ANONYMOUS=true` for legacy configuration).

!!! note "Role-based credentials need a ServiceAccount you can annotate"
    Chain mode resolves whatever the download Pod's environment offers. Download
    Jobs currently run under the namespace's `default` ServiceAccount and
    `AIMArtifact` has no field to select a different one, so IRSA and EKS Pod
    Identity require annotating that `default` ServiceAccount — which grants the
    role to every Pod in the namespace. EC2 instance profiles, shared profiles
    and credential processes need no ServiceAccount changes.

### S3 control knobs

Typed connection fields cover endpoint, region, authentication, addressing,
signing, and TLS. The downloader also recognises these optional environment
variables for legacy configuration and transfer tuning:

When a typed S3 connection is active, transport, proxy, trust, and credential
provider variables must come from the administrator-owned RuntimeConfig.
Artifact-level `spec.env` accepts only the bounded `AIM_S3_LOG_LEVEL`,
`AIM_S3_MAX_WORKERS`, `AIM_S3_MAX_CONCURRENCY`, and
`AIM_S3_MULTIPART_CHUNKSIZE_MB` tuning variables. Other artifact variables are
reported as an invalid configuration rather than copied into the transfer Job.

| Variable | Default | Purpose |
| --- | --- | --- |
| `AIM_S3_ANONYMOUS` | unset (credentials are resolved) | Set to `true` to read a public bucket with unsigned requests. |
| `AWS_REGION` / `AWS_DEFAULT_REGION` | `us-east-1` when a custom endpoint is set, otherwise left to boto3 | Signing region (most non-AWS backends ignore it; R2 aliases `us-east-1` → `auto`). On real AWS it is left unset so the bucket's region is discovered rather than pinned wrong. |
| `AIM_S3_ADDRESSING_STYLE` | `path` when a custom endpoint is set, else `auto` | Force `path`, `virtual`, or `auto` bucket addressing. |
| `AIM_S3_SIGNATURE_VERSION` | boto3 default (V4) | Override the signing version (e.g. `s3v4`). Rarely needed. |
| `AWS_CA_BUNDLE` | botocore's default CA bundle | Path to a PEM CA bundle. Prefer `artifact.s3.tls.caBundleRef`, which mounts it automatically. |
| `AIM_S3_INSECURE_SKIP_VERIFY` | `false` | Disable certificate and hostname verification. Prefer the typed field when this temporary diagnostic bypass is unavoidable. |
| `AIM_S3_LOG_LEVEL` | `INFO` | Downloader-only verbosity (`DEBUG`, `INFO`, `WARNING`, `ERROR`, or `CRITICAL`). Botocore, boto3, s3transfer, and urllib3 remain at `WARNING`. |
| `AIM_S3_MAX_WORKERS` | `8` | Number of objects downloaded in parallel; artifact values must be integers from 1 through 64. |
| `AIM_S3_MAX_CONCURRENCY` | boto3 default (`10`) | Multipart threads per object; artifact values must be integers from 1 through 64. |
| `AIM_S3_MULTIPART_CHUNKSIZE_MB` | boto3 default (`8`) | Multipart chunk size in MiB; artifact values must be integers from 5 through 5120. |

!!! note "Effective connection fan-out"
    Total in-flight connections is roughly `AIM_S3_MAX_WORKERS` × `AIM_S3_MAX_CONCURRENCY` (default 8 × 10 = 80). On small/constrained gateways (single-node MinIO, on-prem RGW) consider lowering one of these.

!!! tip "Keep secrets out of the manifest"
    Use `artifact.s3.auth.credentialsSecretRef` for static credentials. AIM
    Engine mounts the selected keys and configures the downloader through
    `_FILE` variables, keeping values out of both the resource and the process
    environment.

S3 downloads bind each transfer to object metadata collected before the body is
written. The downloader verifies the resulting file size before its atomic
rename, so a truncated or partial transfer is never published.

!!! danger "Disabling TLS verification"
    `insecureSkipVerify: true` disables both certificate-chain and hostname
    verification. It is mutually exclusive with `caBundleRef`, emits a warning
    event, and should only be used briefly while diagnosing a certificate
    problem.

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
