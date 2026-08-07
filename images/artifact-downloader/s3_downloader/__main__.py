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

"""CLI for downloading / sizing S3-compatible sources.

Usage:
    python -m s3_downloader download <s3://bucket/prefix> <target-dir>
    python -m s3_downloader size <s3://bucket/prefix>

``download`` recursively syncs a prefix: every object under the source prefix
is fetched into ``target-dir`` preserving the object layout relative to the
prefix. ``size`` prints the total byte count of those objects to stdout (used
by the size-check Job to provision the cache PVC).

Both commands enumerate objects through the SAME generator (``iter_objects``)
with the SAME include/exclude filter, so the size estimate always matches what
the download actually writes -- otherwise the PVC could be undersized.
"""

import argparse
import logging
import os
import sys
from pathlib import Path
from typing import Iterator, List, Optional, Tuple

from cloudpathlib import CloudPath, S3Path
from botocore.exceptions import SSLError

from s3_downloader.client import build_client
from s3_downloader.filters import DownloadFilter
from s3_downloader.transfer import CloudDownloadTask, parallel_downloads

logger = logging.getLogger("s3_downloader")

_THIRD_PARTY_LOGGERS = ("botocore", "boto3", "s3transfer", "urllib3")
_LOG_LEVELS = {
    "DEBUG": logging.DEBUG,
    "INFO": logging.INFO,
    "WARNING": logging.WARNING,
    "ERROR": logging.ERROR,
    "CRITICAL": logging.CRITICAL,
}


def _configure_logging() -> None:
    """Enable downloader diagnostics without enabling SDK wire logging."""
    requested = os.environ.get("AIM_S3_LOG_LEVEL", "INFO").strip().upper()
    level = _LOG_LEVELS.get(requested, logging.INFO)
    logging.basicConfig(
        level=logging.WARNING,
        format="%(asctime)s %(levelname)s %(name)s: %(message)s",
        stream=sys.stderr,
    )
    logging.getLogger().setLevel(logging.WARNING)
    logger.setLevel(level)
    for name in _THIRD_PARTY_LOGGERS:
        logging.getLogger(name).setLevel(logging.WARNING)


def _max_workers(value: Optional[int] = None) -> int:
    raw = str(value) if value is not None else os.environ.get("AIM_S3_MAX_WORKERS", "8")
    try:
        workers = int(raw)
    except ValueError as exc:
        raise ValueError("AIM_S3_MAX_WORKERS must be an integer from 1 through 64") from exc
    if workers < 1 or workers > 64:
        raise ValueError("AIM_S3_MAX_WORKERS must be an integer from 1 through 64")
    return workers


def _normalize_uri(uri: str) -> str:
    if not uri.startswith("s3://"):
        raise ValueError(f"Expected an s3:// URI, got: {uri}")
    return uri.rstrip("/")


def iter_objects(root: S3Path, flt: DownloadFilter) -> Iterator[Tuple[S3Path, Path]]:
    """Yield (object, relative_path) for every file under ``root`` passing ``flt``.

    If ``root`` is itself an object, it is yielded with its basename as the
    relative path. This is the single source of truth shared by both the
    download and size commands.
    """
    if root.is_file():
        yield root, Path(root.name)
        return
    for obj in root.glob("**/*"):
        if not obj.is_file():
            continue  # skip directory placeholders
        rel = Path(obj.relative_to(root))
        if flt.matches(str(rel)):
            yield obj, rel


def _object_size(obj: S3Path) -> int:
    metadata = obj.client.client.head_object(
        Bucket=obj.bucket,
        Key=obj.key,
        **obj.client.boto3_dl_extra_args,
    )
    return int(metadata["ContentLength"])


def cmd_download(uri: str, target_dir: str, max_workers: int) -> int:
    client = build_client()
    root = CloudPath(_normalize_uri(uri), client=client)
    target = Path(target_dir)
    flt = DownloadFilter.from_env()

    tasks: List[CloudDownloadTask] = []
    for obj, rel in iter_objects(root, flt):
        tasks.append(
            CloudDownloadTask(
                cloud_path=obj,
                target_path=target / rel,
                expected_size=_object_size(obj),
            )
        )

    if not tasks:
        logger.error(f"No objects found at {uri} (after applying download filter)")
        return 1

    logger.info(f"Syncing {len(tasks)} object(s) from {uri} to {target_dir}")
    parallel_downloads(tasks, max_workers=max_workers)

    # Post-download verification: every expected object must be present as a
    # real file. parallel_downloads already raises on any failed transfer and
    # files are atomically renamed into place, so this should always hold -- but
    # it is the last line of defense against an empty/partial model directory
    # being marked Ready (e.g. a misconfigured prefix slipping past the checks
    # above, or only `.part` temps left behind). Cheap: local stat per file.
    missing = [t.target_path for t in tasks if not t.target_path.is_file()]
    if missing:
        logger.error(
            f"Sync incomplete: {len(missing)} of {len(tasks)} expected file(s) "
            f"missing from {target_dir}, e.g. {missing[0]}"
        )
        return 1

    logger.info(f"Sync complete: {len(tasks)} verified file(s) in {target_dir}")
    return 0


def cmd_size(uri: str) -> int:
    client = build_client()
    root = CloudPath(_normalize_uri(uri), client=client)
    flt = DownloadFilter.from_env()

    total = 0
    found = False
    for obj, _rel in iter_objects(root, flt):
        try:
            total += obj.stat().st_size
        except OSError as e:
            logger.error(f"Failed to stat {obj}: {e}")
            return 1
        found = True

    if not found:
        logger.error(f"No objects found at {uri} (after applying download filter)")
        return 1

    # Only the integer byte count goes to stdout so callers can capture it.
    print(total)
    return 0


def main() -> int:
    _configure_logging()

    parser = argparse.ArgumentParser(prog="s3_downloader", description="S3-compatible downloader")
    sub = parser.add_subparsers(dest="command", required=True)

    p_download = sub.add_parser("download", help="Download all objects under a prefix")
    p_download.add_argument("uri", help="Source s3://bucket/prefix")
    p_download.add_argument("target_dir", help="Local target directory")
    p_download.add_argument(
        "--max-workers",
        type=int,
        default=None,
        help="Number of concurrent object downloads (default: AIM_S3_MAX_WORKERS or 8)",
    )

    p_size = sub.add_parser("size", help="Print total byte size of objects under a prefix")
    p_size.add_argument("uri", help="Source s3://bucket/prefix")

    args = parser.parse_args()

    try:
        if args.command == "download":
            return cmd_download(args.uri, args.target_dir, _max_workers(args.max_workers))
        if args.command == "size":
            return cmd_size(args.uri)
    except SSLError as e:
        logger.error(f"S3 TLS certificate verification failed: {e}")
        return 1
    except Exception as e:  # noqa: BLE001 - surface a clean non-zero exit for the Job
        logger.error(f"S3 operation failed: {e}")
        return 1

    return 2


if __name__ == "__main__":
    sys.exit(main())
