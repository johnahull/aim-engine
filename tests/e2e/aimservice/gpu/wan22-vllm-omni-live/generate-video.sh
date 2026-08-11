#!/usr/bin/env bash
# Submit a synchronous WAN 2.2 T2V request through the AIM HTTPRoute and verify
# that the response is a non-empty, approximately three-second MP4.
set -euo pipefail

NS="${HTTP_NS:-kgateway-system}"
SVC="${HTTP_SVC:-kserve-ingress-gateway}"
SVC_PORT="${HTTP_PORT:-80}"
BASE_PATH="${HTTP_BASE_PATH:?HTTP_BASE_PATH must be set}"
TIMEOUT="${HTTP_TIMEOUT:-1200}"

need() {
  command -v "$1" >/dev/null 2>&1 || {
    echo "Missing required command: $1" >&2
    exit 2
  }
}
need kubectl
need curl
need python3

OUTPUT="$(mktemp /tmp/wan22-vllm-omni-live.XXXXXX.mp4)"
HEADERS="$(mktemp /tmp/wan22-vllm-omni-live.XXXXXX.headers)"
PROXY_PID=""
trap 'rm -f "$OUTPUT" "$HEADERS"; if [ -n "$PROXY_PID" ]; then kill "$PROXY_PID" 2>/dev/null || true; fi' EXIT

start_proxy() {
  local port
  for port in 8001 8002 8003 8004 8005; do
    kubectl proxy --port="$port" >/dev/null 2>&1 &
    PROXY_PID=$!
    sleep 1
    if kill -0 "$PROXY_PID" 2>/dev/null; then
      PROXY_PORT="$port"
      return 0
    fi
  done
  echo "ERROR: could not start kubectl proxy on ports 8001-8005" >&2
  exit 1
}

start_proxy
URL="http://127.0.0.1:${PROXY_PORT}/api/v1/namespaces/${NS}/services/${SVC}:${SVC_PORT}/proxy${BASE_PATH}/videos/sync"
echo "POST $URL"

CODE="$(
  curl -sS \
    --max-time "$TIMEOUT" \
    -D "$HEADERS" \
    -o "$OUTPUT" \
    -w '%{http_code}' \
    -F 'prompt=A cinematic tracking shot of a red fox walking through a snowy pine forest at sunrise, natural motion, detailed fur, stable camera' \
    -F 'model=Wan-AI/Wan2.2-T2V-A14B-Diffusers' \
    -F 'seconds=3' \
    -F 'width=832' \
    -F 'height=480' \
    -F 'num_frames=49' \
    -F 'fps=16' \
    -F 'num_inference_steps=20' \
    -F 'seed=42' \
    "$URL"
)"

if [ "$CODE" != "200" ]; then
  echo "ERROR: video endpoint returned HTTP $CODE" >&2
  head -c 2000 "$OUTPUT" >&2 || true
  echo >&2
  exit 1
fi

if ! grep -qi '^content-type: video/mp4' "$HEADERS"; then
  echo "ERROR: response Content-Type is not video/mp4" >&2
  sed -n '1,40p' "$HEADERS" >&2
  exit 1
fi

python3 - "$OUTPUT" <<'PY'
import struct
import sys
from pathlib import Path

path = Path(sys.argv[1])
data = path.read_bytes()

if len(data) < 100_000:
    raise SystemExit(f"MP4 is unexpectedly small: {len(data)} bytes")


def boxes(start, end):
    pos = start
    while pos + 8 <= end:
        size = struct.unpack_from(">I", data, pos)[0]
        kind = data[pos + 4 : pos + 8]
        header = 8
        if size == 1:
            if pos + 16 > end:
                break
            size = struct.unpack_from(">Q", data, pos + 8)[0]
            header = 16
        elif size == 0:
            size = end - pos
        if size < header or pos + size > end:
            break
        yield kind, pos + header, pos + size
        pos += size


def child(start, end, wanted):
    for kind, payload_start, box_end in boxes(start, end):
        if kind == wanted:
            return payload_start, box_end
    return None


top = list(boxes(0, len(data)))
if not any(kind == b"ftyp" for kind, _, _ in top):
    raise SystemExit("response has no MP4 ftyp box")
if not any(kind == b"mdat" and end - start > 50_000 for kind, start, end in top):
    raise SystemExit("response has no non-empty MP4 mdat box")

video = None
for kind, moov_start, moov_end in top:
    if kind != b"moov":
        continue
    for child_kind, trak_start, trak_end in boxes(moov_start, moov_end):
        if child_kind != b"trak":
            continue
        mdia = child(trak_start, trak_end, b"mdia")
        if not mdia:
            continue
        hdlr = child(*mdia, b"hdlr")
        if not hdlr:
            continue
        hdlr_start, hdlr_end = hdlr
        if hdlr_end - hdlr_start < 12 or data[hdlr_start + 8 : hdlr_start + 12] != b"vide":
            continue
        video = mdia
        break

if video is None:
    raise SystemExit("MP4 has no video track")

mdhd = child(*video, b"mdhd")
if not mdhd:
    raise SystemExit("video track has no mdhd box")
mdhd_start, _ = mdhd
version = data[mdhd_start]
if version == 1:
    timescale = struct.unpack_from(">I", data, mdhd_start + 20)[0]
    duration_units = struct.unpack_from(">Q", data, mdhd_start + 24)[0]
else:
    timescale = struct.unpack_from(">I", data, mdhd_start + 12)[0]
    duration_units = struct.unpack_from(">I", data, mdhd_start + 16)[0]
duration = duration_units / timescale

minf = child(*video, b"minf")
stbl = child(*minf, b"stbl") if minf else None
stsz = child(*stbl, b"stsz") if stbl else None
if not stsz:
    raise SystemExit("video track has no stsz sample table")
stsz_start, _ = stsz
frames = struct.unpack_from(">I", data, stsz_start + 8)[0]

if frames < 48:
    raise SystemExit(f"expected at least 48 video frames, found {frames}")
if not 2.7 <= duration <= 3.4:
    raise SystemExit(f"expected about 3 seconds of video, found {duration:.3f}s")

print(f"Validated MP4: {len(data)} bytes, {frames} frames, {duration:.3f}s")
PY
