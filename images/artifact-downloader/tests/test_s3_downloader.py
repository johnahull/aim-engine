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

import io
import logging
import os
import unittest
from types import SimpleNamespace

from s3_downloader import __main__ as main_module


class LoggingTests(unittest.TestCase):
    def setUp(self):
        self.previous_level = os.environ.get("AIM_S3_LOG_LEVEL")

    def tearDown(self):
        if self.previous_level is None:
            os.environ.pop("AIM_S3_LOG_LEVEL", None)
        else:
            os.environ["AIM_S3_LOG_LEVEL"] = self.previous_level

    def test_debug_is_scoped_to_downloader_and_never_enables_sdk_secrets(self):
        os.environ["AIM_S3_LOG_LEVEL"] = "DEBUG"
        names = ("", "s3_downloader", *main_module._THIRD_PARTY_LOGGERS)
        previous = {name: logging.getLogger(name).level for name in names}
        stream = io.StringIO()
        handler = logging.StreamHandler(stream)
        root = logging.getLogger()
        root.addHandler(handler)
        try:
            main_module._configure_logging()
            main_module.logger.debug("downloader diagnostic")
            logging.getLogger("botocore.parsers").debug(
                "SECRET_ACCESS_KEY_SENTINEL SESSION_TOKEN_SENTINEL"
            )
        finally:
            root.removeHandler(handler)
            for name, level in previous.items():
                logging.getLogger(name).setLevel(level)

        output = stream.getvalue()
        self.assertIn("downloader diagnostic", output)
        self.assertNotIn("SECRET_ACCESS_KEY_SENTINEL", output)
        self.assertNotIn("SESSION_TOKEN_SENTINEL", output)

    def test_object_worker_count_is_bounded(self):
        for value in ("0", "65", "not-an-integer"):
            with self.subTest(value=value):
                os.environ["AIM_S3_MAX_WORKERS"] = value
                with self.assertRaisesRegex(ValueError, "must be an integer"):
                    main_module._max_workers()
        os.environ["AIM_S3_MAX_WORKERS"] = "64"
        self.assertEqual(main_module._max_workers(), 64)
        os.environ.pop("AIM_S3_MAX_WORKERS", None)

    def test_object_size_reads_content_length(self):
        obj = SimpleNamespace(
            bucket="bucket",
            key="object",
            client=SimpleNamespace(
                client=SimpleNamespace(
                    head_object=lambda **_kwargs: {"ContentLength": 8}
                ),
                boto3_dl_extra_args={},
            ),
        )
        self.assertEqual(main_module._object_size(obj), 8)


if __name__ == "__main__":
    unittest.main()
