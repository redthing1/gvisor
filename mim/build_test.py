# Copyright 2026 The gVisor Authors.
# SPDX-License-Identifier: Apache-2.0

"""Release source admission must precede any engine or heavy build operation."""

import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest


class BuildTest(unittest.TestCase):
    def test_dirty_release_is_refused_before_calling_podman(self):
        with tempfile.TemporaryDirectory(prefix="build source ") as directory:
            root = Path(directory)
            (root / "mim").mkdir()
            shutil.copyfile(Path(__file__).with_name("build"), root / "mim/build")
            subprocess.run(["git", "init", "--quiet", str(root)], check=True)
            marker = root / "engine-called"
            binary = root / "bin"
            binary.mkdir()
            (binary / "podman").write_text(
                '#!/bin/sh\ntouch "$ENGINE_MARKER"\nexit 91\n'
            )
            (binary / "podman").chmod(0o755)
            environment = {
                **os.environ,
                "PATH": str(binary) + ":" + os.environ["PATH"],
                "ENGINE_MARKER": str(marker),
            }
            for action in ("release", "bundle"):
                with self.subTest(action=action):
                    result = subprocess.run(
                        ["bash", str(root / "mim/build"), action],
                        env=environment,
                        capture_output=True,
                        text=True,
                    )
                    self.assertEqual(result.returncode, 1, result.stderr)
                    self.assertIn("clean source checkout", result.stderr)
                    self.assertFalse(marker.exists())
                    self.assertFalse((root / ".mim-build").exists())

    def test_missing_git_metadata_is_refused_before_building(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "mim").mkdir()
            shutil.copyfile(Path(__file__).with_name("build"), root / "mim/build")
            result = subprocess.run(
                ["bash", str(root / "mim/build"), "release"],
                capture_output=True,
                text=True,
            )
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("not a git repository", result.stderr)
            self.assertFalse((root / ".mim-build").exists())


if __name__ == "__main__":
    unittest.main()
