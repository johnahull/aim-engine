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
import shlex
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path


SCRIPT_DIR = Path(__file__).resolve().parents[1]


def write_executable(path: Path, content: str):
    path.write_text(content)
    path.chmod(0o755)


class AdapterScriptTests(unittest.TestCase):
    def run_script(self, script: str, env: dict[str, str]):
        return subprocess.run(
            ["sh", str(SCRIPT_DIR / script)],
            env={**os.environ, **env},
            text=True,
            capture_output=True,
            check=False,
        )

    def test_subtree_sync_succeeds_when_final_cleanup_fails(self):
        with tempfile.TemporaryDirectory() as temp:
            disk = Path(temp) / "disk"
            service_root = disk / "service-1"
            (service_root / "keep").mkdir(parents=True)
            (service_root / "remove").mkdir()
            (service_root / "keep" / "payload").write_text("keep")
            (service_root / "remove" / "payload").write_text("remove")

            fake_bin = Path(temp) / "bin"
            fake_bin.mkdir()
            write_executable(fake_bin / "rm", "#!/bin/sh\nexit 1\n")

            result = self.run_script(
                "adapter-subtree-sync.sh",
                {
                    "ADAPTER_PVC_ROOT": str(disk),
                    "SERVICE_ID": "service-1",
                    "KEEP_ADAPTER_PATHS": "keep",
                    "PATH": f"{fake_bin}:{os.environ['PATH']}",
                },
            )

            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertTrue((service_root / "keep" / "payload").is_file())
            self.assertFalse((service_root / "remove").exists())
            slots = list((disk / ".unload-tmp" / "service-1").iterdir())
            self.assertEqual(len(slots), 1)
            self.assertEqual(
                (slots[0] / "adapter" / "payload").read_text(),
                "remove",
            )
            self.assertIn("the reaper will retry", result.stderr)

    def test_subtree_sync_attempts_other_moves_before_reporting_failure(self):
        with tempfile.TemporaryDirectory() as temp:
            disk = Path(temp) / "disk"
            service_root = disk / "service-1"
            (service_root / "adapter-a").mkdir(parents=True)
            (service_root / "adapter-b").mkdir()

            fake_bin = Path(temp) / "bin"
            fake_bin.mkdir()
            real_mv = shlex.quote(shutil.which("mv") or "/bin/mv")
            write_executable(
                fake_bin / "mv",
                (
                    "#!/bin/sh\n"
                    'case "$1" in\n'
                    "  */adapter-a) exit 1 ;;\n"
                    "esac\n"
                    f'exec {real_mv} "$@"\n'
                ),
            )

            result = self.run_script(
                "adapter-subtree-sync.sh",
                {
                    "ADAPTER_PVC_ROOT": str(disk),
                    "SERVICE_ID": "service-1",
                    "KEEP_ADAPTER_PATHS": "",
                    "PATH": f"{fake_bin}:{os.environ['PATH']}",
                },
            )

            self.assertNotEqual(result.returncode, 0)
            self.assertTrue((service_root / "adapter-a").is_dir())
            self.assertFalse((service_root / "adapter-b").exists())
            self.assertFalse((disk / ".unload-tmp" / "service-1").exists())
            self.assertIn("one or more adapters", result.stderr)

    def test_reaper_retries_unload_slots_for_live_services_best_effort(self):
        with tempfile.TemporaryDirectory() as temp:
            disk = Path(temp) / "disk"
            (disk / "service-1").mkdir(parents=True)
            unload_root = disk / ".unload-tmp" / "service-1"
            (unload_root / "a-slot" / "adapter").mkdir(parents=True)
            (unload_root / "b-slot" / "adapter").mkdir(parents=True)

            fake_bin = Path(temp) / "bin"
            fake_bin.mkdir()
            real_rm = shlex.quote(shutil.which("rm") or "/bin/rm")
            write_executable(
                fake_bin / "rm",
                (
                    "#!/bin/sh\n"
                    'case "$*" in\n'
                    "  *a-slot*) exit 1 ;;\n"
                    "esac\n"
                    f'exec {real_rm} "$@"\n'
                ),
            )

            result = self.run_script(
                "adapter-reap.sh",
                {
                    "ADAPTER_PVC_ROOT": str(disk),
                    "KEEP_SERVICE_IDS": "service-1",
                    "MIN_AGE_SECONDS": "0",
                    "PATH": f"{fake_bin}:{os.environ['PATH']}",
                },
            )

            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertTrue((disk / "service-1").is_dir())
            self.assertTrue((unload_root / "a-slot").is_dir())
            self.assertFalse((unload_root / "b-slot").exists())
            self.assertIn("a later reaper will retry", result.stderr)


if __name__ == "__main__":
    unittest.main()
