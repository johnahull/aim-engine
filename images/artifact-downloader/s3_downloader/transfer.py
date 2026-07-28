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

"""Download task model and parallel-download primitives."""

import logging
import os
from abc import ABC, abstractmethod
from concurrent.futures import ThreadPoolExecutor, as_completed
from pathlib import Path
from typing import List

from cloudpathlib import CloudPath
from tenacity import retry, stop_after_attempt, wait_exponential

logger = logging.getLogger(__name__)


class DownloadTask(ABC):
    """A unit of work executed by ``parallel_downloads``."""

    @abstractmethod
    def run(self) -> None:
        pass

    @property
    @abstractmethod
    def title(self) -> str:
        pass


# reraise=True: without it tenacity raises RetryError on exhaustion, which
# hides the underlying botocore message (AccessDenied/NoSuchBucket/...). The
# operator classifies download failures by pattern-matching that message in the
# pod logs, so the real error string must propagate.
@retry(stop=stop_after_attempt(3), wait=wait_exponential(multiplier=1, min=1, max=10), reraise=True)
def download_file(source: CloudPath, destination: Path) -> None:
    destination.parent.mkdir(exist_ok=True, parents=True)

    # Skip objects already fully present so Job retries don't re-pull them.
    # Existence is a trustworthy completion signal ONLY because completed files
    # are atomically renamed into place below: a killed download can leave at
    # most a `.part` temp, never a complete-looking final file. The size check
    # is a cheap secondary guard for the source object changing between runs.
    if destination.exists():
        try:
            if destination.stat().st_size == source.stat().st_size:
                logger.info(f"Skipping {source} (already present, size matches)")
                return
            logger.warning(f"Size mismatch for {destination} (have {destination.stat().st_size}, want {source.stat().st_size}); re-downloading")
        except OSError:
            pass

    # boto3/s3transfer writes ranged multipart chunks at byte offsets directly
    # into the target, so an interrupted transfer (e.g. the progress monitor's
    # SIGKILL on stall) leaves a truncated/sparse file whose apparent size can
    # already match the full object. Download to a sibling temp on the SAME
    # filesystem and rename only after a clean return, so the final path never
    # exists unless the object was fully written. The deterministic `.part`
    # name means a prior kill's temp is overwritten on retry, not accumulated.
    tmp = destination.with_name(destination.name + ".part")
    logger.info(f"Downloading {source} to {destination}")
    try:
        source.download_to(tmp)
        os.replace(tmp, destination)
    except BaseException:
        try:
            tmp.unlink()
        except OSError:
            pass
        raise


class CloudDownloadTask(DownloadTask):
    """A single object -> local file download."""

    def __init__(self, cloud_path: CloudPath, target_path: Path):
        self.cloud_path = cloud_path
        self.target_path = target_path

    def run(self) -> None:
        logger.info(f"Downloading {self.title}")
        download_file(source=self.cloud_path, destination=self.target_path)

    @property
    def title(self) -> str:
        return f"{self.cloud_path} -> {self.target_path}"


def parallel_downloads(tasks: List[DownloadTask], max_workers: int = 5) -> None:
    """Run every task concurrently, aborting once any task fails.

    On failure this stops scheduling new work and re-raises; transfers already
    in flight drain before the executor exits.
    """
    with ThreadPoolExecutor(max_workers=max_workers) as executor:
        future_to_task = {executor.submit(task.run): task for task in tasks}

        try:
            for future in as_completed(future_to_task):
                task = future_to_task[future]
                try:
                    future.result()  # Raises exception if occurred
                    logger.info(f"Completed download task: {task.title}")
                except Exception as e:
                    logger.error(f"Task failed: {task.title} with error: {e}")

                    for fut in future_to_task:
                        if not fut.done():
                            fut.cancel()
                            logger.debug(f"Cancelled task: {future_to_task[fut].title}")

                    executor.shutdown(wait=False, cancel_futures=True)
                    raise  # Re-raise exception to propagate error

        except Exception as e:
            logger.critical(f"Download process aborted due to error: {e}")
            raise e
