# NVIDIA cluster dependency

This directory installs the NVIDIA Kubernetes device plugin and its bundled
Node Feature Discovery deployment:

```bash
make install-nvidia-dependencies
```

Set `NVIDIA_DEVICE_PLUGIN_VERSION` to override the pinned chart version.

The host must already provide a working NVIDIA driver, NVIDIA container
toolkit/runtime, and an `nvidia` RuntimeClass. This dependency does not install
or manage host drivers. Use the NVIDIA GPU Operator instead when Kubernetes
must own that lifecycle.
