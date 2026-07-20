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

# Progress monitor - updates AIMArtifact status with download progress

terminated=false
trap 'terminated=true' TERM

URL="${1:?Usage: $0 <hf://org/model or s3://bucket/path>}"
TARGET_DIR="${2:?Usage: $0 <target directory>}"

if [ -z "${EXPECTED_SIZE_BYTES:-}" ]; then
    case "$URL" in
        hf://*)
            # Fetch expected size if not set
            echo "Fetching model size from Hugging Face..."
            export MODEL_PATH="${URL#hf://}"
            EXPECTED_SIZE_BYTES=$(python /check-size/check-hf-size.py 2>/dev/null || echo 0)
            ;;
        s3://*)
            # Get size from S3 (s3cmd du returns human-readable, need bytes)
            EXPECTED_SIZE_BYTES=$(s3cmd du "$URL" 2>/dev/null | awk '{print $1}' || echo 0)
            ;;
    esac
    export EXPECTED_SIZE_BYTES
fi

echo "Expected size: $EXPECTED_SIZE_BYTES bytes"

expected_size=${EXPECTED_SIZE_BYTES:-0}
log_interval=${PROGRESS_INTERVAL:-5}
stall_timeout=${STALL_TIMEOUT:-120}

# Upper bound (seconds) for a single `du` call. Kept high so only genuinely stuck
# filesystems warn, not merely slow ones. On overrun we warn once and keep going.
# Reject empty/non-numeric/0 (0 would disable the guard).
du_timeout=${DU_TIMEOUT:-300}
case "$du_timeout" in
    ''|*[!0-9]*|0) du_timeout=300 ;;
esac

# Scratch file for the backgrounded `du` output, plus state for warn-once dedup.
du_tmp="${TMPDIR:-/tmp}/progress-monitor-du.$$"
du_pid=""
du_started=0
du_warned=false
du_calls=0

last_size=0
last_change_time=$(date +%s)

update_progress() {
    [ -z "${ARTIFACT_NAME:-}" ] && echo "WARN: ARTIFACT_NAME not set" >&2 && return
    [ -z "${ARTIFACT_NAMESPACE:-}" ] && echo "WARN: ARTIFACT_NAMESPACE not set" >&2 && return
    
    if ! kubectl patch aimartifact "$ARTIFACT_NAME" -n "$ARTIFACT_NAMESPACE" \
        --type=merge --subresource=status \
        -p "{\"status\":{\"progress\":{\"percentage\":$1,\"displayPercentage\":\"$1 %\",\"downloadedBytes\":$2,\"totalBytes\":$3}}}"; then
        echo "WARN: Failed to update progress: $1% ($2/$3 bytes)" >&2
    fi
}

# Surface a diagnostic onto status.progress.message (the monitor's own field,
# distinct from status.download.message). Best-effort; pass "" to clear.
set_progress_message() {
    [ -z "${ARTIFACT_NAME:-}" ] && return
    [ -z "${ARTIFACT_NAMESPACE:-}" ] && return
    kubectl patch aimartifact "$ARTIFACT_NAME" -n "$ARTIFACT_NAMESPACE" \
        --type=merge --subresource=status \
        -p "{\"status\":{\"progress\":{\"message\":\"$1\"}}}" >/dev/null 2>&1 || true
}

# Measures on-disk size; runs backgrounded (see loop). In debug mode the first
# AIM_DEBUG_SIMULATE_DU_HANG_CALLS calls sleep AIM_DEBUG_SIMULATE_DU_HANG_SECONDS
# (set above DU_TIMEOUT) to exercise the stuck-fs path without a real wedged PVC.
measure_size() {
    if [ -n "${AIM_DEBUG_SIMULATE_DU_HANG_SECONDS:-}" ] \
        && [ "$du_calls" -le "${AIM_DEBUG_SIMULATE_DU_HANG_CALLS:-1}" ]; then
        sleep "$AIM_DEBUG_SIMULATE_DU_HANG_SECONDS"
    fi
    du -sb "$TARGET_DIR"
}

echo "Progress monitor started: expected=${expected_size} bytes, interval=${log_interval}s, stall_timeout=${stall_timeout}s, du_timeout=${du_timeout}s" >&2

while [ "$terminated" = "false" ]; do
    # Run `du` backgrounded so a wedged filesystem can't freeze the monitor: a
    # stuck `du` can't be killed (even SIGKILL), so a foreground `timeout du`
    # would block forever. We poll instead and, on overrun, warn once and leave
    # the orphaned `du` behind (reaped if the I/O ever returns).
    if [ -z "$du_pid" ]; then
        : > "$du_tmp" 2>/dev/null || true
        du_calls=$((du_calls + 1))
        measure_size > "$du_tmp" 2>/dev/null &
        du_pid=$!
        du_started=$(date +%s)
    fi

    # Wait for this `du` to finish or exceed du_timeout, staying responsive to
    # termination (re-checked every second).
    while kill -0 "$du_pid" 2>/dev/null \
        && [ $(( $(date +%s) - du_started )) -lt "$du_timeout" ] \
        && [ "$terminated" = "false" ]; do
        sleep 1
    done

    if kill -0 "$du_pid" 2>/dev/null; then
        # `du` is still running past du_timeout: the filesystem is likely stuck.
        [ "$terminated" = "true" ] && break
        if [ "$du_warned" = "false" ]; then
            echo "WARN: 'du' on ${TARGET_DIR} did not return within ${du_timeout}s - the filesystem may be stuck. Progress reporting AND stall detection are paused until it returns." >&2
            set_progress_message "Filesystem may be stuck: 'du' exceeded ${du_timeout}s. Progress and stall detection paused."
            du_warned=true
        fi
        # Can't unstick it; leave it orphaned and re-check next tick without
        # spawning another. Skip progress/stall handling (size unknown).
        sleep "$log_interval"
        continue
    fi

    # `du` finished within the bound: reap and read the size. dash (this image's
    # /bin/sh) auto-reaps background children, so `wait` just retrieves status
    # without blocking. Keep /bin/sh a reaping shell.
    wait "$du_pid" 2>/dev/null || true
    du_pid=""
    if [ "$du_warned" = "true" ]; then
        # Filesystem recovered: clear the diagnostic and resume normal handling.
        echo "INFO: 'du' on ${TARGET_DIR} returned; resuming progress and stall detection." >&2
        set_progress_message ""
        du_warned=false
    fi
    current_size=$(cut -f1 "$du_tmp" 2>/dev/null)
    case "$current_size" in
        ''|*[!0-9]*) current_size=0 ;;
    esac
    now=$(date +%s)

    # Stall detection
    if [ "$current_size" -gt "$last_size" ]; then
        last_size=$current_size
        last_change_time=$now
    elif [ $((now - last_change_time)) -ge "$stall_timeout" ]; then
        echo "Download stalled for ${stall_timeout}s, killing download process" >&2
        pkill -9 -f "python|s3cmd" 2>/dev/null || true
        # Keep monitoring: hf-download.sh will retry the next protocol.
        last_size=0
        last_change_time=$now
    fi

    # Calculate and update progress
    if [ "$expected_size" -gt 0 ] && [ "$current_size" -gt 0 ]; then
        percent=$((current_size * 100 / expected_size))
        [ "$percent" -gt 99 ] && percent=99 # We only set 100% after the download is complete
    else
        percent=0
    fi
    update_progress "$percent" "$current_size" "$expected_size"
    echo "Progress: ${percent}% (${current_size}/${expected_size} bytes)" >&2
    sleep "$log_interval"
done

# Reuse the last measured size rather than risk a fresh `du` blocking on teardown.
echo "Progress monitor terminated: final_size=${current_size:-0} bytes" >&2
rm -f "$du_tmp" 2>/dev/null || true
