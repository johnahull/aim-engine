#!/bin/sh
# MIT License

# Copyright (c) 2026 Advanced Micro Devices, Inc.

# Permission is hereby granted, free of charge, to any person obtaining a copy
# of this software and associated documentation files (the "Software"), to deal
# in the Software without restriction, including without limitation the rights
# to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
# copies of the Software, and to permit persons to whom the Software is
# furnished to do so, subject to the following conditions:

# The above copyright notice and this permission notice shall be included in all
# copies or substantial portions of the Software.

# THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
# IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
# FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
# AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
# LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
# OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
# SOFTWARE.

# adapter-subtree-sync.sh reconciles a single AIMService's per-service adapter
# subtree on the shared adapter disk toward the service's declared adapter set.
# It is owned by the AIMService (so it is garbage-collected with the service) and
# does two things:
#   1. Creates the per-service subtree directory if missing, so the
#      InferenceService can mount it read-only (subPath) before any adapter has
#      downloaded — the aim-runtime errors on a missing subPath at startup.
#   2. Atomically moves (unloads) any adapter directory no longer in the
#      declared keep-list out of the live subtree, then best-effort deletes the
#      moved bytes. In dynamic mode the in-pod watcher observes the atomic
#      disappearance (the disk is ground truth). Failed cleanup is left under
#      .unload-tmp for the model-artifact-owned reaper and does not fail sync.
#      It never touches other services' live subtrees.
#
# Contract (env):
#   ADAPTER_PVC_ROOT     Mount path of the adapter-disk PVC (mounted RW at root).
#   SERVICE_ID           Per-service subtree directory name (the AIMService UID).
#   KEEP_ADAPTER_PATHS   Comma-separated adapter directory names to keep.

set -eu

: "${ADAPTER_PVC_ROOT:?ADAPTER_PVC_ROOT is required}"
: "${SERVICE_ID:?SERVICE_ID is required}"
KEEP="${KEEP_ADAPTER_PATHS:-}"

SERVICE_ROOT="${ADAPTER_PVC_ROOT}/${SERVICE_ID}"
UNLOAD_ROOT="${ADAPTER_PVC_ROOT}/.unload-tmp/${SERVICE_ID}"
move_failed=0

# is_kept <name> -> 0 when name is in the keep list.
is_kept() {
    name="$1"
    [ -z "$name" ] && return 1
    OLDIFS="$IFS"; IFS=','
    for k in $KEEP; do
        if [ "$k" = "$name" ]; then
            IFS="$OLDIFS"
            return 0
        fi
    done
    IFS="$OLDIFS"
    return 1
}

mkdir -p "$SERVICE_ROOT"
echo "Syncing adapter subtree ${SERVICE_ROOT}; keep=[${KEEP}]"

for dir in "$SERVICE_ROOT"/*; do
    [ -e "$dir" ] || continue
    [ -d "$dir" ] || continue
    base=$(basename "$dir")
    if is_kept "$base"; then
        continue
    fi

    # Use a unique directory on the same PVC. Moving the live directory into
    # the slot is one rename(2), so readers see the whole adapter disappear at
    # once rather than observing rm -rf remove its files incrementally.
    if ! mkdir -p "$UNLOAD_ROOT"; then
        echo "Error: cannot create unload directory for adapter '${base}'" >&2
        move_failed=1
        continue
    fi
    if ! slot=$(mktemp -d "${UNLOAD_ROOT}/${base}.XXXXXX"); then
        echo "Error: cannot allocate unload slot for adapter '${base}'" >&2
        move_failed=1
        continue
    fi

    echo "Atomically unloading adapter directory no longer declared: ${base}"
    if ! mv "$dir" "${slot}/adapter"; then
        echo "Error: failed to move adapter '${base}' out of the live subtree" >&2
        rmdir "$slot" 2>/dev/null || true
        move_failed=1
    fi
done

# Cleanup is deliberately last and best-effort. A failed rm must not undo the
# successful live-tree reconciliation; the periodic reaper retries old entries.
if [ -d "$UNLOAD_ROOT" ]; then
    echo "Cleaning unloaded adapter bytes under ${UNLOAD_ROOT}"
    if ! rm -rf "$UNLOAD_ROOT"; then
        echo "Warning: could not remove all unloaded adapter bytes; the reaper will retry" >&2
    fi
fi

if [ "$move_failed" -ne 0 ]; then
    echo "Error: one or more adapters could not be moved out of the live subtree" >&2
    exit 1
fi

echo "Subtree sync complete for ${SERVICE_ROOT}"
