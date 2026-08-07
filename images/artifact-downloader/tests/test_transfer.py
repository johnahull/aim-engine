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

import tempfile
import unittest
from pathlib import Path
from unittest import mock

from s3_downloader import transfer


class FakeCloudPath:
    def __init__(self, payload: bytes):
        self.payload = payload
        self.calls = 0

    def download_to(self, target: Path):
        self.calls += 1
        target.write_bytes(self.payload)

    def __str__(self):
        return "s3://bucket/object"


class TransferIntegrityTests(unittest.TestCase):
    def test_verified_body_atomically_replaces_destination(self):
        content = b"complete"
        source = FakeCloudPath(content)
        with tempfile.TemporaryDirectory() as temp:
            destination = Path(temp) / "model.bin"
            destination.write_bytes(b"old")

            transfer.download_file(
                source,
                destination,
                expected_size=len(content),
            )

            self.assertEqual(destination.read_bytes(), content)
            self.assertFalse(destination.with_name("model.bin.part").exists())

    def test_wrong_size_never_replaces_previous_destination(self):
        source = FakeCloudPath(b"short")
        with tempfile.TemporaryDirectory() as temp:
            destination = Path(temp) / "model.bin"
            destination.write_bytes(b"old")

            with mock.patch.object(
                transfer.download_file.retry,
                "wait",
                return_value=0,
            ):
                with self.assertRaisesRegex(RuntimeError, "size mismatch"):
                    transfer.download_file(
                        source,
                        destination,
                        expected_size=8,
                    )

            self.assertEqual(source.calls, 3)
            self.assertEqual(destination.read_bytes(), b"old")
            self.assertFalse(destination.with_name("model.bin.part").exists())


if __name__ == "__main__":
    unittest.main()
