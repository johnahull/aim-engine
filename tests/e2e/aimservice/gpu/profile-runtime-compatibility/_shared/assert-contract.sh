#!/usr/bin/env bash

set -euo pipefail

NAMESPACE="${NAMESPACE:?NAMESPACE is required}"
EXPECTED_IMAGE="${EXPECTED_IMAGE:?EXPECTED_IMAGE is required}"
BASE_MODEL="${BASE_MODEL:-runtime-base}"
DERIVED_MODEL="${DERIVED_MODEL:-runtime-derived}"
SERVICE="${SERVICE:-runtime-compatibility}"
PROFILE_GV="aimprofiles.v1alpha2.aim.eai.amd.com"

fail() {
  echo "ERROR: $*" >&2
  exit 1
}

service_json="$(kubectl -n "$NAMESPACE" get aimservices.v1alpha2.aim.eai.amd.com "$SERVICE" -o json)"
derived_name="$(jq -r '.status.resolvedProfile.name // empty' <<<"$service_json")"
derived_scope="$(jq -r '.status.resolvedProfile.scope // empty' <<<"$service_json")"
[[ -n "$derived_name" ]] || fail "$SERVICE has no resolved profile"
[[ "$derived_scope" == "Namespace" ]] || fail "$SERVICE resolved unexpected profile scope $derived_scope"

derived_json="$(kubectl -n "$NAMESPACE" get "$PROFILE_GV" "$derived_name" -o json)"
[[ "$(jq -r '.metadata.labels["aim.eai.amd.com/source-model"] // empty' <<<"$derived_json")" == "$DERIVED_MODEL" ]] ||
  fail "$derived_name is not owned by $DERIVED_MODEL"
[[ "$(jq -r '.spec.image // empty' <<<"$derived_json")" == "$EXPECTED_IMAGE" ]] ||
  fail "$derived_name does not use $EXPECTED_IMAGE"

derived_contract="$(jq -r '.metadata.annotations["aim.eai.amd.com/profile-yaml-contract"] // empty' <<<"$derived_json")"
[[ -n "$derived_contract" ]] || fail "$derived_name has no profile YAML contract"
[[ "$(jq -r '.codec // empty' <<<"$derived_contract")" == "aim-profile/v1" ]] ||
  fail "$derived_name uses an unexpected profile YAML codec"
normalized_derived="$(jq -S -c . <<<"$derived_contract")"

source_match="$(kubectl -n "$NAMESPACE" get "$PROFILE_GV" -o json | jq -c \
  --arg model "$BASE_MODEL" \
  --arg accelerator "$(jq -r '.spec.acceleratorModel' <<<"$derived_json")" \
  --argjson count "$(jq -r '.spec.acceleratorCount' <<<"$derived_json")" \
  --arg metric "$(jq -r '.spec.metric' <<<"$derived_json")" \
  --arg precision "$(jq -r '.spec.precision' <<<"$derived_json")" \
  --argjson contract "$normalized_derived" '
    .items
    | map(select(
        .metadata.labels["aim.eai.amd.com/source-model"] == $model
        and .metadata.labels["aim.eai.amd.com/profile-role"] == "base"
        and .spec.acceleratorModel == $accelerator
        and .spec.acceleratorCount == $count
        and .spec.metric == $metric
        and .spec.precision == $precision
        and ((.metadata.annotations["aim.eai.amd.com/profile-yaml-contract"] | fromjson) == $contract)
      ))
    | .[0] // empty')"
[[ -n "$source_match" ]] ||
  fail "$derived_name did not inherit a matching contract from a $BASE_MODEL base profile"

runtime="$(jq -r '.status.projectedRuntimeName // empty' <<<"$derived_json")"
[[ -n "$runtime" ]] || fail "$derived_name has no projected runtime"
profile_yaml="$(kubectl -n "$NAMESPACE" get configmap "$runtime" -o json | jq -r '.data | to_entries[0].value')"

while IFS= read -r field; do
  [[ -z "$field" ]] && continue
  grep -Eq "^  ${field}:" <<<"$profile_yaml" ||
    fail "projected YAML is missing declared metadata field $field"
done < <(jq -r '(.metadataFields[]?), ((.extensions.metadata? // {}) | keys[])' <<<"$derived_contract")

while IFS= read -r field; do
  [[ -z "$field" ]] && continue
  grep -Eq "^${field}:" <<<"$profile_yaml" ||
    fail "projected YAML is missing declared top-level field $field"
done < <(jq -r '((.extensions.topLevel? // {}) | keys[])' <<<"$derived_contract")

compatibility_fields=(
  gpu
  gpu_count
  accelerator_model
  accelerator_type
  accelerator_count
  manual_selection_only
  variant
  features
  primary
  auto_selection_policy
)
for field in "${compatibility_fields[@]}"; do
  if jq -e --arg field "$field" '(.metadataFields // []) | index($field)' <<<"$derived_contract" >/dev/null; then
    grep -Eq "^  ${field}:" <<<"$profile_yaml" ||
      fail "projected YAML dropped declared field $field"
  elif grep -Eq "^  ${field}:" <<<"$profile_yaml"; then
    fail "projected YAML introduced absent field $field"
  fi
done

echo "Resolved profile: $derived_name"
echo "Matching base profile: $(jq -r '.metadata.name' <<<"$source_match")"
echo "Projected runtime: $runtime"
echo "Profile contract: $normalized_derived"
