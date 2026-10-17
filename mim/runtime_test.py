# Copyright 2026 The gVisor Authors.
# SPDX-License-Identifier: Apache-2.0

"""Exercise the installed OCI adapter with real shell argument parsing."""

import json
import shlex
import subprocess
import tempfile
import unittest
from pathlib import Path


class RuntimeTest(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory(prefix="runtime test ")
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)
        (self.root / "runsc").write_text(
            "#!/usr/bin/python3\nimport json,sys\nprint(json.dumps(sys.argv[1:]))\n"
        )
        (self.root / "runsc").chmod(0o755)
        scripts = Path(__file__).resolve().parent
        self.policy = json.loads((scripts / "policy.json").read_text())
        template = (scripts / "runtime.sh").read_text()
        for name, flags in self.policy["profiles"].items():
            runtime = self.root / "profiles" / name / "runsc"
            runtime.parent.mkdir(parents=True)
            args = [f"--{f}" for f in [*self.policy["flags"], *flags]]
            runtime.write_text(template.replace("@FLAGS@", shlex.join(args)))
            runtime.chmod(0o755)

    def run_profile(self, profile, args):
        return subprocess.run(
            [str(self.root / "profiles" / profile / "runsc"), *args],
            capture_output=True,
            text=True,
        )

    def test_profiles_apply_policy_and_preserve_guest_arguments(self):
        args = [
            "--root",
            "/path with spaces",
            "--log=/tmp/log",
            "--log-format",
            "json",
            "--systemd-cgroup",
            "exec",
            "cid",
            "echo",
            "--directfs=true",
            "$(false)",
            "",
            "a\nb",
        ]
        for name, flags in self.policy["profiles"].items():
            with self.subTest(profile=name):
                result = self.run_profile(name, args)
                self.assertEqual(result.returncode, 0, result.stderr)
                expected = [f"--{f}" for f in [*self.policy["flags"], *flags]]
                self.assertEqual(json.loads(result.stdout), [*expected, *args])

    def test_global_overrides_and_ambiguous_invocations_are_rejected(self):
        for args in (
            ["--directfs=true", "create", "cid"],
            ["--directfs", "true", "create", "cid"],
            ["-directfs=true", "create", "cid"],
            ["--allow-flag-override=true", "create", "cid"],
            ["--ignore-cgroups=true", "create", "cid"],
            ["--systemd-cgroup=false", "create", "cid"],
            ["--rootless", "create", "cid"],
            ["--", "create", "cid"],
            ["--log"],
            ["--version", "create"],
            [],
        ):
            with self.subTest(args=args):
                result = self.run_profile("offline", args)
                self.assertEqual(result.returncode, 2)
                self.assertEqual(result.stdout, "")

    def test_version_probe_is_supported(self):
        self.assertEqual(self.run_profile("offline", ["--version"]).returncode, 0)


if __name__ == "__main__":
    unittest.main()
