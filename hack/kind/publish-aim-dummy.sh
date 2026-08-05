#!/usr/bin/env bash
set -euo pipefail

KIND_CLUSTER="${KIND_CLUSTER:-aim-engine-test-e2e}"
CHAINSAW_TEST_DIR="${CHAINSAW_TEST_DIR:-tests/e2e}"
ZOT_NAMESPACE="${ZOT_NAMESPACE:-zot-system}"
ZOT_SERVICE="${ZOT_SERVICE:-zot}"
ZOT_CA_CONFIGMAP="${ZOT_CA_CONFIGMAP:-aim-engine-zot-ca}"
ZOT_CLUSTER_REGISTRY="${ZOT_CLUSTER_REGISTRY:-zot.zot-system.svc.cluster.local:5000}"
ZOT_AIM_DUMMY_REPO="${ZOT_AIM_DUMMY_REPO:-${ZOT_CLUSTER_REGISTRY}/aim-dummy}"

for command_name in crane docker kind kubectl; do
    if ! command -v "${command_name}" >/dev/null 2>&1; then
        echo "ERROR: ${command_name} is required" >&2
        exit 1
    fi
done

tags="$(
    grep -rhoE 'docker\.io/amdenterpriseai/aim-dummy:[A-Za-z0-9._-]+' \
        "${CHAINSAW_TEST_DIR}" |
        sed 's|^.*:||' |
        sort -u
)"
if [[ -z "${tags}" ]]; then
    echo "ERROR: no aim-dummy refs found under ${CHAINSAW_TEST_DIR}" >&2
    exit 1
fi

tmp_dir="$(mktemp -d /tmp/aim-engine-zot-push.XXXXXX)"
port_forward_pid=""
cleanup() {
    if [[ -n "${port_forward_pid}" ]]; then
        kill "${port_forward_pid}" >/dev/null 2>&1 || true
        wait "${port_forward_pid}" >/dev/null 2>&1 || true
    fi
    rm -rf "${tmp_dir}"
}
trap cleanup EXIT

mkdir -p "${tmp_dir}/certs"
kubectl wait --for=condition=Available --timeout=120s \
    "deployment/${ZOT_SERVICE}" -n "${ZOT_NAMESPACE}"
kubectl get configmap "${ZOT_CA_CONFIGMAP}" -n aim-system \
    -o jsonpath='{.data.ca\.crt}' >"${tmp_dir}/certs/ca.crt"

port_forward_log="${tmp_dir}/port-forward.log"
kubectl port-forward --address 127.0.0.1 -n "${ZOT_NAMESPACE}" \
    "service/${ZOT_SERVICE}" :5000 >"${port_forward_log}" 2>&1 &
port_forward_pid="$!"

forward_port=""
for _ in $(seq 1 100); do
    forward_port="$(
        sed -n 's/.*127\.0\.0\.1:\([0-9][0-9]*\).*/\1/p' \
            "${port_forward_log}" |
            head -n1
    )"
    if [[ -n "${forward_port}" ]]; then
        break
    fi
    if ! kill -0 "${port_forward_pid}" >/dev/null 2>&1; then
        cat "${port_forward_log}" >&2
        echo "ERROR: Zot port-forward exited before becoming ready" >&2
        exit 1
    fi
    sleep 0.1
done
if [[ -z "${forward_port}" ]]; then
    cat "${port_forward_log}" >&2
    echo "ERROR: timed out waiting for the Zot port-forward" >&2
    exit 1
fi

local_registry="localhost:${forward_port}"
source_hash="$(
    find images/aim-dummy -type f -exec sha256sum {} \; |
        sort |
        sha256sum |
        cut -c1-16
)"
local_image="aim-engine-zot-aim-dummy:${source_hash}"
oci_archive="${tmp_dir}/aim-dummy-oci.tar"
oci_layout="${tmp_dir}/aim-dummy-oci"

echo "Building aim-dummy once for fixture tags: $(tr '\n' ' ' <<<"${tags}")"
docker buildx build --provenance=false --sbom=false --load \
    -t "${local_image}" images/aim-dummy
# Export a native OCI layout for Zot. Pushing a Docker save archive makes crane
# reconstruct a Docker schema-2 manifest, which strict OCI registries can reject
# even though all of its blobs are valid. This second export reuses the build
# cache, so it does not rebuild the image.
docker buildx build --provenance=false --sbom=false \
    --output "type=oci,dest=${oci_archive}" images/aim-dummy
mkdir -p "${oci_layout}"
tar -xf "${oci_archive}" -C "${oci_layout}"

cert_dirs="/etc/ssl/certs:${tmp_dir}/certs"
first_tag="${tags%%$'\n'*}"
push_ref="${local_registry}/aim-dummy:${first_tag}"

echo "Pushing ${push_ref} through the TLS port-forward"
SSL_CERT_DIR="${cert_dirs}" crane push "${oci_layout}" "${push_ref}"
SSL_CERT_DIR="${cert_dirs}" crane digest "${push_ref}" >/dev/null

for tag in ${tags}; do
    tagged_ref="${local_registry}/aim-dummy:${tag}"
    cluster_ref="${ZOT_AIM_DUMMY_REPO}:${tag}"

    if [[ "${tag}" != "${first_tag}" ]]; then
        echo "Adding Zot tag ${tag} to ${push_ref}"
        SSL_CERT_DIR="${cert_dirs}" crane tag "${push_ref}" "${tag}"
        SSL_CERT_DIR="${cert_dirs}" crane digest "${tagged_ref}" >/dev/null
    fi

    echo "Loading ${cluster_ref} into Kind cluster ${KIND_CLUSTER}"
    docker tag "${local_image}" "${cluster_ref}"
    kind load docker-image "${cluster_ref}" --name "${KIND_CLUSTER}"
done

echo "Published aim-dummy to ${ZOT_AIM_DUMMY_REPO} and loaded all fixture tags into Kind"
