#!/usr/bin/env bash

set -euo pipefail

failures=0
live_tests=0

fail() {
  echo "check-chainsaw-hf: $*" >&2
  failures=$((failures + 1))
}

while IFS= read -r -d '' test_file; do
  test_dir="${test_file%/chainsaw-test.yaml}"
  access="$(yq -r '.metadata.labels."hf-access" // ""' "$test_file")"
  needs_secret="$(yq -r '.metadata.labels."needs-secret" // ""' "$test_file")"
  uses_hf_template=false

  while IFS= read -r template; do
    [ -n "$template" ] || continue
    case "$template" in
      *hf-token-step.yaml)
        uses_hf_template=true
        [ -f "$test_dir/$template" ] || fail "$test_file references missing template $template"
        ;;
    esac
  done < <(yq -r '.spec.steps[]?.use.template // ""' "$test_file")

  case "$access" in
    live)
      live_tests=$((live_tests + 1))
      [ "$uses_hf_template" = true ] || fail "$test_file is hf-access=live but does not provision the HF token"
      grep -R -q 'name: huggingface-creds' "$test_dir" || fail "$test_dir has no huggingface-creds reference"
      ;;
    anonymous)
      [ "$uses_hf_template" = false ] || fail "$test_file is hf-access=anonymous but provisions the HF token"
      if grep -R -q 'name: huggingface-creds' "$test_dir"; then
        fail "$test_dir is hf-access=anonymous but references huggingface-creds"
      fi
      ;;
    "")
      [ "$uses_hf_template" = false ] || fail "$test_file provisions the HF token but has no hf-access=live label"
      ;;
    *)
      fail "$test_file has unsupported hf-access value: $access"
      ;;
  esac

  if [ "$needs_secret" = hf_token ] && [ "$access" != live ]; then
    fail "$test_file needs an HF token but is not hf-access=live"
  fi
done < <(find tests/e2e -name chainsaw-test.yaml -print0)

if [ "$failures" -ne 0 ]; then
  exit 1
fi

echo "check-chainsaw-hf: validated $live_tests live tests"
