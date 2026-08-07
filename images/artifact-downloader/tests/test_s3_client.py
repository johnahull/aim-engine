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

import os
import tempfile
import unittest
from unittest.mock import patch

from s3_downloader.client import _credential_mode, _verify_setting


class S3ClientConfigurationTests(unittest.TestCase):
    def test_explicit_auth_modes_are_unambiguous(self):
        with patch.dict(os.environ, {"AIM_S3_AUTH_MODE": "chain"}, clear=True):
            self.assertEqual(_credential_mode(None, None), "chain")

        with patch.dict(os.environ, {"AIM_S3_AUTH_MODE": "anonymous"}, clear=True):
            self.assertEqual(_credential_mode("ignored", "ignored"), "anonymous")

        with patch.dict(os.environ, {"AIM_S3_AUTH_MODE": "static"}, clear=True):
            with self.assertRaisesRegex(ValueError, "requires both"):
                _credential_mode(None, None)

        with patch.dict(os.environ, {"AIM_S3_AUTH_MODE": "invalid"}, clear=True):
            with self.assertRaisesRegex(ValueError, "must be one of"):
                _credential_mode(None, None)

    def test_custom_ca_and_insecure_mode_map_to_boto_verify(self):
        with tempfile.NamedTemporaryFile() as ca:
            with patch.dict(os.environ, {"AWS_CA_BUNDLE": ca.name}, clear=True):
                self.assertEqual(_verify_setting(), ca.name)

            with patch.dict(
                os.environ,
                {
                    "AWS_CA_BUNDLE": ca.name,
                    "AIM_S3_INSECURE_SKIP_VERIFY": "true",
                },
                clear=True,
            ):
                self.assertIs(_verify_setting(), False)

        with patch.dict(os.environ, {}, clear=True):
            self.assertIsNone(_verify_setting())

        with patch.dict(
            os.environ,
            {"AIM_S3_INSECURE_SKIP_VERIFY": "false"},
            clear=True,
        ):
            self.assertIs(_verify_setting(), True)


if __name__ == "__main__":
    unittest.main()
