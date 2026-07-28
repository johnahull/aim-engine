# MIT License
#
# Copyright (c) 2026 Advanced Micro Devices, Inc.
#
# Permission is hereby granted, free of charge, to any person obtaining a copy
# of this software and associated documentation files (the "Software"), to deal
# in the Software without restriction, including without limitation the rights
# to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
# copies of the Software, and to permit persons to whom the Software is
# furnished to do so, subject to the following conditions:
#
# The above copyright notice and this permission notice shall be included in all
# copies or substantial portions of the Software.
#
# THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
# IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
# FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
# AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
# LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
# OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
# SOFTWARE.

"""Download include/exclude filtering.

Mirrors huggingface_hub's allow/ignore semantics so the S3 path applies the
same filter the HF path does (and, critically, so the size-check and download
jobs select the identical set of files -- a mismatch would undersize the cache
PVC). Patterns are fnmatch-style globs evaluated against the path relative to
the source prefix.

Filter order:
  1. Include: if any include patterns are set, only paths matching at least one
     are kept.
  2. Exclude: paths matching any exclude pattern are then removed.
"""

import fnmatch
import os
from typing import List, Optional


def _split_env(name: str) -> List[str]:
    raw = os.environ.get(name, "")
    return [p.strip() for p in raw.split(",") if p.strip()]


class DownloadFilter:
    def __init__(self, include: Optional[List[str]] = None, exclude: Optional[List[str]] = None):
        self.include = include or []
        self.exclude = exclude or []

    @classmethod
    def from_env(cls) -> "DownloadFilter":
        """Build from AIM_HF_INCLUDE / AIM_HF_EXCLUDE (set by the operator)."""
        return cls(include=_split_env("AIM_HF_INCLUDE"), exclude=_split_env("AIM_HF_EXCLUDE"))

    @property
    def active(self) -> bool:
        return bool(self.include or self.exclude)

    def matches(self, relpath: str) -> bool:
        """Return True if ``relpath`` should be downloaded."""
        relpath = relpath.replace(os.sep, "/")
        if self.include and not any(fnmatch.fnmatch(relpath, pat) for pat in self.include):
            return False
        if any(fnmatch.fnmatch(relpath, pat) for pat in self.exclude):
            return False
        return True
