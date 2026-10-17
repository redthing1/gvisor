# Copyright 2026 The gVisor Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     https://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""Check archive identity, layout, and rejection of ambiguous inputs."""

import contextlib
import hashlib
import json
import os
import subprocess
import tarfile
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from package import package


class PackageTest(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        root = Path(self.directory.name)
        self.source = root / "source"
        self.source.mkdir()
        self.stage = root / "stage"
        (self.stage / "gvisor-bin").mkdir(parents=True)
        self.output = root / "output"
        self.output.mkdir()
        for name in ("runsc", "gvisor-bin/gvisor_sentry", "gvisor-bin/checkpointgofer"):
            (self.stage / name).write_bytes(f"test binary: {name}".encode())
        self.enterContext(contextlib.chdir(self.source))
        self.enterContext(
            patch.dict(os.environ, MIM_BUILDER_IMAGE="sha256:test-builder")
        )
        self.git("init", "--quiet")
        self.git("config", "user.name", "Build Test")
        self.git("config", "user.email", "build@example.invalid")
        Path("LICENSE").write_text("Test license\n")
        Path(".bazelversion").write_text("8.8.1\n")
        self.commit()
        self.upstream = self.git("rev-parse", "HEAD")
        Path("mim").mkdir()
        Path("mim/release.json").write_text(
            json.dumps(
                {
                    "version": "test.1",
                    "upstream": {"commit": self.upstream, "tag": "test-base"},
                    "patches": [],
                }
            )
        )
        self.commit()

    def git(self, *args):
        return subprocess.check_output(["git", *args], text=True).strip()

    def commit(self):
        self.git("add", ".")
        self.git("commit", "--quiet", "-m", "Test source")

    def test_archive_identity_and_reproducibility(self):
        archive = package(self.stage, self.output)
        first = archive.read_bytes()
        with tarfile.open(archive) as payload:
            prefix = archive.name.removesuffix(".tar.bz2") + "/"
            manifest = json.load(payload.extractfile(prefix + "manifest.json"))
            self.assertEqual(manifest["source_commit"], self.git("rev-parse", "HEAD"))
            self.assertEqual(manifest["upstream"]["commit"], self.upstream)
            self.assertEqual(manifest["builder_image"], "sha256:test-builder")
            self.assertEqual(
                set(manifest["files"]),
                {
                    "LICENSE",
                    "runsc",
                    "gvisor-bin/gvisor_sentry",
                    "gvisor-bin/checkpointgofer",
                },
            )
            for name, digest in manifest["files"].items():
                entry = payload.getmember(prefix + name)
                content = payload.extractfile(entry).read()
                self.assertEqual(digest, hashlib.sha256(content).hexdigest())
                self.assertEqual(entry.mode, 0o644 if name == "LICENSE" else 0o755)
                self.assertEqual((entry.uid, entry.gid), (0, 0))
        self.assertEqual(
            archive.with_suffix(".bz2.sha256").read_text(),
            f"{hashlib.sha256(first).hexdigest()}  {archive.name}\n",
        )
        self.assertEqual(package(self.stage, self.output).read_bytes(), first)

    def test_dirty_source_is_rejected(self):
        Path("LICENSE").write_text("Changed\n")
        with self.assertRaisesRegex(ValueError, "clean source"):
            package(self.stage, self.output)

    def test_missing_sidecar_is_rejected(self):
        (self.stage / "gvisor-bin/gvisor_sentry").unlink()
        with self.assertRaisesRegex(ValueError, "incomplete"):
            package(self.stage, self.output)

    def test_symlink_is_rejected(self):
        (self.stage / "gvisor-bin/linked").symlink_to(self.stage / "runsc")
        with self.assertRaisesRegex(ValueError, "symlink"):
            package(self.stage, self.output)

    def test_unrelated_source_is_rejected(self):
        self.git("checkout", "--quiet", "--orphan", "unrelated")
        self.commit()
        with self.assertRaises(subprocess.CalledProcessError):
            package(self.stage, self.output)


if __name__ == "__main__":
    unittest.main()
