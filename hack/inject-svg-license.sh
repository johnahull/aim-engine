#!/usr/bin/env bash
# MIT License

# Copyright (c) 2025 Advanced Micro Devices, Inc.

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

# Inject the repository license (.copyright-template) into SVG files as an XML
# comment. Idempotent: files that already carry the header are left untouched,
# so it is safe to run from `make diagrams`, pre-commit, or by hand.
#
# Usage:
#   hack/inject-svg-license.sh [file.svg ...]   # inject into the given files
#   hack/inject-svg-license.sh                  # inject into all diagram SVGs

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TEMPLATE="${COPYRIGHT_TEMPLATE:-$REPO_ROOT/.copyright-template}"
DIAGRAM_DIR="${DIAGRAM_OUT_DIR:-$REPO_ROOT/docs/docs/assets/diagrams}"

# Distinctive phrase from the license; used to detect an already-injected header.
MARKER="Permission is hereby granted, free of charge"

if [ ! -f "$TEMPLATE" ]; then
	echo "inject-svg-license: template not found: $TEMPLATE" >&2
	exit 1
fi

# Build the XML comment block once. XML comments must not contain "--", which the
# MIT license text does not, so the license can be embedded verbatim.
COMMENT_FILE="$(mktemp)"
trap 'rm -f "$COMMENT_FILE"' EXIT
{
	printf '<!--\n'
	cat "$TEMPLATE"
	printf -- '-->\n'
} >"$COMMENT_FILE"

inject() {
	local f="$1"
	[ -f "$f" ] || return 0
	if grep -qF "$MARKER" "$f"; then
		return 0
	fi

	local tmp
	tmp="$(mktemp)"
	# Insert the comment right after the "<?xml ... ?>" declaration if present,
	# otherwise at the very top of the file. The XML declaration must remain the
	# first thing in the document, so a comment can never precede it.
	awk -v cf="$COMMENT_FILE" '
		BEGIN {
			comment = ""
			while ((getline line < cf) > 0) comment = comment line "\n"
		}
		NR == 1 {
			if (index($0, "<?xml") == 1) {
				p = index($0, "?>")
				if (p > 0) {
					printf "%s\n", substr($0, 1, p + 1)
					printf "%s", comment
					rest = substr($0, p + 2)
					if (length(rest) > 0) printf "%s\n", rest
					next
				}
			}
			printf "%s", comment
			print
			next
		}
		{ print }
	' "$f" >"$tmp"

	mv "$tmp" "$f"
	echo "license: injected -> $f"
}

if [ "$#" -gt 0 ]; then
	for f in "$@"; do
		case "$f" in
		*.svg) inject "$f" ;;
		esac
	done
else
	while IFS= read -r -d '' f; do
		inject "$f"
	done < <(find "$DIAGRAM_DIR" -name '*.svg' -print0)
fi
