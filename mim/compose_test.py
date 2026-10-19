# Copyright 2026 The gVisor Authors.
# SPDX-License-Identifier: Apache-2.0

"""Protect the qualification tool's ownership and host-credential boundaries."""

import errno
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

from check_compose import LABEL, PODMAN, Qualification, process_label


class ComposeTest(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        self.root = Path(directory.name)
        self.check = Qualification(self.root)

    def result(self, value=None, code=0):
        return subprocess.CompletedProcess([], code, json.dumps(value), "")

    def test_host_git_configuration_and_remote_engine_are_not_inherited(self):
        with patch.dict(
            os.environ,
            GIT_CONFIG_COUNT="1",
            GIT_CONFIG_KEY_0="credential.helper",
            GIT_CONFIG_VALUE_0="host-helper",
            CONTAINER_HOST="ssh://host",
            CONTAINER_CONNECTION="host",
            QUALIFY_RUNTIME="/host/runtime",
        ):
            check = Qualification(self.root)
        self.assertEqual(check.environment["GIT_CONFIG_GLOBAL"], "/dev/null")
        self.assertEqual(check.environment["GIT_CONFIG_NOSYSTEM"], "1")
        for key in (
            "GIT_CONFIG_COUNT",
            "GIT_CONFIG_KEY_0",
            "GIT_CONFIG_VALUE_0",
            "CONTAINER_HOST",
            "CONTAINER_CONNECTION",
            "QUALIFY_RUNTIME",
        ):
            self.assertNotIn(key, check.environment)

    def test_inspection_failure_is_not_treated_as_absence(self):
        with patch.object(self.check, "run", return_value=self.result(code=125)):
            with self.assertRaisesRegex(ValueError, "cannot establish"):
                self.check.owned("container", "owned-name")

    def test_cleanup_refuses_another_owners_container_and_retains_runtime(self):
        self.check.slice_requested = True
        (self.root / "runtime").mkdir()
        self.check.installation = self.root / "runtime"
        obj = {
            "Id": "another-container",
            "Config": {"Labels": {LABEL: "another-token"}},
        }
        with patch.object(self.check, "run", return_value=self.result([obj])) as run:
            with self.assertRaisesRegex(ValueError, "cleanup incomplete"):
                self.check.cleanup()
        self.assertEqual(run.call_count, 1)
        self.assertTrue((self.root / "runtime").exists())
        self.assertFalse(self.check.report["cleanup"]["complete"])

    def test_cleanup_removes_the_verified_full_id(self):
        self.check.slice_requested = True
        obj = {
            "Id": "verified-full-container-id",
            "Config": {"Labels": {LABEL: self.check.token}},
        }
        calls = []

        def run(*args, **kwargs):
            calls.append(args)
            if args[:3] == ("systemctl", "--user", "show"):
                return subprocess.CompletedProcess(
                    [], 4, "LoadState=not-found\nControlGroup=\n", ""
                )
            command = args[len(PODMAN) :]
            if command[:2] == ("container", "inspect") and len(calls) == 1:
                return self.result([obj])
            if len(command) > 1 and command[1] in ("inspect", "exists"):
                return self.result(code=1)
            return self.result("")

        with patch.object(self.check, "run", side_effect=run):
            self.check.cleanup()
        self.assertIn((*PODMAN, "container", "rm", "--force", obj["Id"]), calls)
        self.assertTrue(self.check.report["cleanup"]["complete"])

    def test_preflight_failure_does_not_require_a_working_engine_to_clean_up(self):
        self.check.installation = self.root / "runtime"
        self.check.installation.mkdir()
        with patch.object(self.check, "run") as run:
            self.check.cleanup()
        run.assert_not_called()
        self.assertFalse(self.check.installation.exists())
        self.assertTrue(self.check.report["cleanup"]["complete"])

    def test_optional_process_label_preserves_real_permission_errors(self):
        with patch("check_compose.Path.read_text", return_value="label\0\n"):
            self.assertEqual(process_label(123), "label")
        for error in (errno.ENOENT, errno.EINVAL):
            with self.subTest(errno=error):
                with patch(
                    "check_compose.Path.read_text",
                    side_effect=OSError(error, "unavailable"),
                ):
                    self.assertIsNone(process_label(123))
        with patch(
            "check_compose.Path.read_text",
            side_effect=PermissionError(errno.EACCES, "denied"),
        ):
            with self.assertRaises(PermissionError):
                process_label(123)

    def test_cleanup_does_not_mutate_the_shared_image_cache(self):
        self.check.slice_requested = True
        self.check.report["image_id"] = "cached-fixture"

        def result(*args, **kwargs):
            if args[:3] == ("systemctl", "--user", "show"):
                return subprocess.CompletedProcess(
                    [], 4, "LoadState=not-found\nControlGroup=\n", ""
                )
            return self.result(code=1)

        with patch.object(self.check, "run", side_effect=result) as run:
            self.check.cleanup()
        for call in run.call_args_list:
            self.assertNotEqual(call.args[: len(PODMAN) + 1], (*PODMAN, "image"))
        self.assertTrue(self.check.report["cleanup"]["complete"])

    def test_slice_cleanup_recovers_a_lost_creation_reply(self):
        with patch.object(
            self.check, "run", side_effect=subprocess.TimeoutExpired("busctl", 1)
        ):
            with self.assertRaises(subprocess.TimeoutExpired):
                self.check.budget()
        group = "/user.slice/" + self.check.slice
        unit = f"Description={self.check.description}\nControlGroup={group}\nLoadState=loaded\n"
        calls = []

        def run(*args, **kwargs):
            calls.append(args)
            if args[:3] == ("systemctl", "--user", "show"):
                return subprocess.CompletedProcess([], 0, unit, "")
            if args[:3] == ("systemctl", "--user", "stop"):
                return self.result("")
            return self.result(code=1)

        with patch.object(self.check, "run", side_effect=run):
            self.check.cleanup()
        self.assertIn(("systemctl", "--user", "stop", self.check.slice), calls)
        self.assertTrue(self.check.report["cleanup"]["complete"])

    def test_slice_cleanup_requires_confirmed_absence_or_ownership(self):
        cases = [
            ("absent", 4, "LoadState=not-found\nControlGroup=\n", True),
            (
                "foreign",
                0,
                "LoadState=loaded\nDescription=another owner\nControlGroup=/user.slice/foreign.slice\n",
                False,
            ),
            ("unreachable", 1, "", False),
        ]
        for name, code, properties, clean in cases:
            with self.subTest(state=name):
                check = Qualification(self.root)
                check.slice_requested = True
                calls = []

                def run(*args, **kwargs):
                    calls.append(args)
                    if args[:3] == ("systemctl", "--user", "show"):
                        return subprocess.CompletedProcess([], code, properties, "")
                    return self.result(code=1)

                with patch.object(check, "run", side_effect=run):
                    if clean:
                        check.cleanup()
                    else:
                        with self.assertRaisesRegex(ValueError, "cleanup incomplete"):
                            check.cleanup()
                self.assertNotIn(("systemctl", "--user", "stop", check.slice), calls)
                self.assertEqual(check.report["cleanup"]["complete"], clean)

    def test_failed_command_records_output_and_reports_the_engine_error(self):
        result = subprocess.CompletedProcess([], 125, "", "OCI permission denied\n")
        with patch("check_compose.subprocess.run", return_value=result) as run:
            with self.assertRaisesRegex(RuntimeError, "OCI permission denied"):
                self.check.run("podman", "init", "fixture")
        self.assertEqual(run.call_args.kwargs["stdin"], subprocess.DEVNULL)
        event = json.loads((self.root / "commands.jsonl").read_text())
        self.assertEqual(event["argv"], ["podman", "init", "fixture"])
        self.assertEqual(event["returncode"], 125)

    def test_timed_out_command_retains_partial_diagnostics(self):
        timeout = subprocess.TimeoutExpired(
            "busctl", 1, output=b"partial\n", stderr=b"diagnostic\n"
        )
        with patch("check_compose.subprocess.run", side_effect=timeout):
            with self.assertRaises(subprocess.TimeoutExpired):
                self.check.run("busctl", "--user")
        event = json.loads((self.root / "commands.jsonl").read_text())
        self.assertIn("timed out", event["error"])
        self.assertEqual(event["stdout"], "partial\n")
        self.assertEqual(event["stderr"], "diagnostic\n")


if __name__ == "__main__":
    unittest.main()
