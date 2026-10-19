#!/usr/bin/env python3
# Copyright 2026 The gVisor Authors.
# SPDX-License-Identifier: Apache-2.0

"""Qualify a runtime bundle with disposable rootless Compose and HTTP Git.

Requires local Podman, podman-compose, Git, Python 3.11+ and a user systemd
manager. Only nonce-labelled test objects and independently bounded user slices
are operated. The pinned fixture image is pulled only with --pull.
"""

import argparse
import base64
import errno
import json
import os
from pathlib import Path
import shutil
import signal
import subprocess
import sys
import tempfile
import time
import urllib.request
import uuid

from distro import unpack


IMAGE = "docker.gitea.com/gitea@sha256:b1a90409ae9afcd5a5725404c3639da7245248848b99e35332968891290b953e"
LABEL = "io.mim.gvisor.qualification"
PODMAN = ["podman", "--remote=false", "--cgroup-manager=systemd"]
OUTER_LIMITS = {
    "memory.max": "2147483648",
    "memory.swap.max": "0",
    "cpu.max": "200000 100000",
    "pids.max": "1024",
}
SERVICE_LIMITS = {
    "memory.max": "1073741824",
    "memory.swap.max": "0",
    "cpu.max": "100000 100000",
    "pids.max": "512",
}


def require(condition, message):
    if not condition:
        raise ValueError(message)


def process_label(pid):
    try:
        return Path(f"/proc/{pid}/attr/current").read_text().rstrip("\0\n")
    except OSError as error:
        # Linux exposes attr/current even when no LSM implements getprocattr.
        if error.errno not in (errno.ENOENT, errno.EINVAL):
            raise
        return None


class Qualification:
    def __init__(self, output):
        self.output = output
        self.token = uuid.uuid4().hex
        self.project = "mimcompose" + self.token[:16]
        self.slice = self.project + ".slice"
        self.description = "gVisor Compose qualification " + self.token
        self.outer = None
        self.installation = None
        self.slice_requested = False
        self.environment = {
            k: v
            for k, v in os.environ.items()
            if not k.startswith(("GIT_", "QUALIFY_"))
            and k not in ("CONTAINER_HOST", "CONTAINER_CONNECTION")
        }
        self.environment.update(
            CONTAINERS_CONF_OVERRIDE=str(output / "engine.conf"),
            REGISTRY_AUTH_FILE=str(output / "auth.json"),
            GIT_CONFIG_NOSYSTEM="1",
            GIT_CONFIG_GLOBAL="/dev/null",
            GIT_TERMINAL_PROMPT="0",
        )
        (output / "engine.conf").write_text(
            '[engine]\nremote = false\ncgroup_manager = "systemd"\n'
        )
        (output / "auth.json").write_text("{}\n")
        self.password = "Synthetic-only-" + self.token
        self.authorization = (
            "Basic "
            + base64.b64encode(("qualification:" + self.password).encode()).decode()
        )
        self.opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
        self.report = {
            "project": self.project,
            "token": self.token,
            "slice": self.slice,
            "image": IMAGE,
            "checks": [],
            "snapshots": [],
        }

    def run(self, *args, check=True, scoped=False, timeout=60):
        command = list(args)
        if scoped:
            command = [
                "systemd-run",
                "--user",
                "--scope",
                "--quiet",
                "--collect",
                "--slice=" + self.slice,
                "--expand-environment=no",
                "--",
                *command,
            ]
        event = {"argv": command}
        try:
            result = subprocess.run(
                command,
                cwd=self.output,
                env=self.environment,
                stdin=subprocess.DEVNULL,
                capture_output=True,
                text=True,
                timeout=timeout,
            )
            event.update(
                returncode=result.returncode, stdout=result.stdout, stderr=result.stderr
            )
        except BaseException as error:
            event["error"] = str(error) or type(error).__name__
            for stream in ("stdout", "stderr"):
                data = getattr(error, stream, "") or ""
                event[stream] = (
                    data.decode(errors="replace") if isinstance(data, bytes) else data
                )
            raise
        finally:
            with (self.output / "commands.jsonl").open("a") as log:
                log.write(json.dumps(event) + "\n")
        if check and result.returncode:
            detail = (result.stderr or result.stdout).strip()
            raise RuntimeError(
                f"{args[0]} failed ({result.returncode}): {detail[-2000:]}"
            )
        return result

    def save(self):
        (self.output / "report.json").write_text(
            json.dumps(self.report, indent=2) + "\n"
        )

    def passed(self, name, detail=None):
        self.report["checks"].append({"name": name, "detail": detail})
        self.save()
        print("PASS", name, flush=True)

    def compose(self, *args, **kwargs):
        return self.run(*self.compose_command, *args, scoped=True, **kwargs)

    def owned(self, kind, name):
        result = self.run(*PODMAN, kind, "inspect", name, check=False)
        if result.returncode:
            require(
                self.run(*PODMAN, kind, "exists", name, check=False).returncode == 1,
                f"cannot establish whether {kind} {name} exists",
            )
            return None
        obj = json.loads(result.stdout)[0]
        labels = obj["Config"]["Labels"] if kind == "container" else obj["Labels"]
        require(labels.get(LABEL) == self.token, f"{kind} ownership mismatch: {name}")
        return obj

    def container(self):
        obj = self.owned("container", self.project + "-gitea")
        require(obj is not None, "test container is missing")
        return obj

    def budget(self):
        properties = [
            ("Description", "s", self.description),
            ("MemoryMax", "t", "2147483648"),
            ("MemorySwapMax", "t", "0"),
            ("CPUQuotaPerSecUSec", "t", "2000000"),
            ("CPUQuotaPeriodUSec", "t", "100000"),
            ("TasksMax", "t", "1024"),
        ]
        # The manager may create the unit even if the caller loses its reply.
        self.slice_requested = True
        self.run(
            "busctl",
            "--user",
            "call",
            "org.freedesktop.systemd1",
            "/org/freedesktop/systemd1",
            "org.freedesktop.systemd1.Manager",
            "StartTransientUnit",
            "ssa(sv)a(sa(sv))",
            self.slice,
            "fail",
            str(len(properties)),
            *(v for prop in properties for v in prop),
            "0",
        )
        relative = self.run(
            "systemctl", "--user", "show", self.slice, "-p", "ControlGroup", "--value"
        ).stdout.strip()
        require(
            relative.startswith("/user.slice/") and relative.endswith("/" + self.slice),
            "unexpected test slice path",
        )
        self.outer = Path("/sys/fs/cgroup") / relative.lstrip("/")
        limits = {key: (self.outer / key).read_text().strip() for key in OUTER_LIMITS}
        require(limits == OUTER_LIMITS, f"outer budget not enforced: {limits}")
        self.passed("independent-outer-budget", limits)

    def initialize(self):
        self.run(*PODMAN, "init", self.container()["Id"], scoped=True)
        obj = self.container()
        require(obj["OCIRuntime"] == str(self.runtime), "selected runtime changed")
        pid = obj["State"]["Pid"]
        relative = Path(f"/proc/{pid}/cgroup").read_text().strip().split("::")[1]
        group = Path("/sys/fs/cgroup") / relative.lstrip("/")
        require(group.is_relative_to(self.outer), "container escaped the test budget")
        limits = {key: (group / key).read_text().strip() for key in SERVICE_LIMITS}
        require(limits == SERVICE_LIMITS, f"service limits not enforced: {limits}")
        spec = json.loads(Path(obj["OCIConfigPath"]).read_text())
        require(spec["process"]["user"]["uid"] == 1000, "unexpected guest UID")
        require(
            not spec["process"].get("selinuxLabel"), "unsupported SELinux process label"
        )
        self.report["snapshots"].append(
            {
                "id": obj["Id"],
                "limits": limits,
                "sentry_domain": process_label(pid),
            }
        )
        self.passed("runtime-and-effective-service-limits", limits)

    def endpoint(self):
        ports = self.container()["NetworkSettings"]["Ports"]["3000/tcp"]
        require(
            len(ports) == 1 and ports[0]["HostIp"] == "127.0.0.1",
            "test port is not restricted to localhost",
        )
        return "http://127.0.0.1:" + ports[0]["HostPort"]

    def request(self, path, data=None, authenticate=False):
        headers = {"Authorization": self.authorization} if authenticate else {}
        if data is not None:
            headers["Content-Type"] = "application/json"
        request = urllib.request.Request(
            self.endpoint() + path,
            headers=headers,
            data=json.dumps(data).encode() if data is not None else None,
        )
        with self.opener.open(request, timeout=5) as response:
            return json.load(response)

    def ready(self):
        deadline = time.monotonic() + 60
        while True:
            try:
                version = self.request("/api/v1/version")
                self.passed("host-loopback-http", version)
                return
            except (OSError, ValueError):
                if time.monotonic() >= deadline:
                    raise
                time.sleep(0.5)

    def git(self, *args, cwd=None):
        return self.run(
            "git",
            "-C",
            str(cwd or self.output),
            "-c",
            "credential.helper=",
            "-c",
            "http.proxy=",
            "-c",
            "http.extraHeader=Authorization: " + self.authorization,
            "-c",
            "http.followRedirects=false",
            "-c",
            "core.hooksPath=/dev/null",
            "-c",
            "commit.gpgSign=false",
            "-c",
            "init.templateDir=",
            *args,
            scoped=True,
        )

    def exercise(self):
        self.compose("up", "--no-build", "--no-start")
        self.initialize()
        self.compose("start")
        self.ready()
        self.run(*PODMAN, "healthcheck", "run", self.container()["Id"], scoped=True)
        require(
            self.container()["State"]["Health"]["Status"] == "healthy",
            "health-check exec did not succeed",
        )
        self.passed("healthcheck-exec")
        self.compose(
            "exec",
            "-T",
            "gitea",
            "gitea",
            "--config",
            "/etc/gitea/app.ini",
            "admin",
            "user",
            "create",
            "--username",
            "qualification",
            "--email",
            "qualification@example.invalid",
            "--password",
            self.password,
            "--admin",
            "--must-change-password=false",
        )
        repo = self.request(
            "/api/v1/user/repos",
            {
                "name": "fixture",
                "private": True,
                "auto_init": False,
                "default_branch": "main",
            },
            authenticate=True,
        )
        self.passed("sqlite-user-and-repository", {"repository_id": repo["id"]})
        work = self.output / "checkout"
        self.git("init", "-b", "main", str(work))
        content = "Synthetic gVisor Compose qualification.\n"
        (work / "README.md").write_text(content)
        self.git("add", "README.md", cwd=work)
        self.git(
            "-c",
            "user.name=Qualification",
            "-c",
            "user.email=qualification@example.invalid",
            "commit",
            "-m",
            "Synthetic fixture",
            cwd=work,
        )
        revision = self.git("rev-parse", "HEAD", cwd=work).stdout.strip()
        path = "/qualification/fixture.git"
        self.git("push", self.endpoint() + path, "main", cwd=work)
        require(
            self.git(
                "ls-remote", self.endpoint() + path, "refs/heads/main"
            ).stdout.split()[0]
            == revision,
            "pushed Git object differs",
        )
        self.git("clone", self.endpoint() + path, str(self.output / "clone"))
        require(
            (self.output / "clone/README.md").read_text() == content,
            "clone content differs",
        )
        self.passed("authenticated-http-git-push-fetch-clone", revision)

        self.compose("stop", "--timeout", "10")
        stopped = self.container()["State"]
        require(
            stopped["Status"] == "exited" and stopped["ExitCode"] == 0,
            "application did not stop cleanly",
        )
        self.compose("start")
        self.ready()
        require(
            self.request("/api/v1/repos/qualification/fixture", authenticate=True)["id"]
            == repo["id"],
            "stop/start lost database state",
        )
        self.passed("graceful-stop-start-and-persistence")

        previous = self.container()["Id"]
        self.compose(
            "up", "--no-build", "--no-start", "--force-recreate", "--no-deps", "gitea"
        )
        require(self.container()["Id"] != previous, "service was not recreated")
        self.initialize()
        self.compose("start")
        self.ready()
        require(
            self.request("/api/v1/repos/qualification/fixture", authenticate=True)["id"]
            == repo["id"],
            "recreation lost database state",
        )
        require(
            self.git(
                "ls-remote", self.endpoint() + path, "refs/heads/main"
            ).stdout.split()[0]
            == revision,
            "recreation lost Git objects",
        )
        clone = self.output / "recreated-clone"
        self.git("clone", self.endpoint() + path, str(clone))
        require(
            (clone / "README.md").read_text() == content,
            "recreated repository content differs",
        )
        self.passed("recreation-retains-database-and-git-objects")

    def cleanup(self):
        errors = []
        objects = []
        if self.slice_requested:
            objects = [
                ("container", self.project + "-gitea"),
                ("pod", "pod_" + self.project),
                ("volume", self.project + "-data"),
                ("volume", self.project + "-config"),
            ]
        for kind, name in objects:
            try:
                obj = self.owned(kind, name)
                if obj:
                    identifier = obj["Id"] if kind != "volume" else obj["Name"]
                    if kind == "container":
                        self.run(*PODMAN, "stop", "--time=10", identifier, check=False)
                        logs = self.run(*PODMAN, "logs", identifier, check=False)
                        (self.output / "gitea.log").write_text(
                            logs.stdout + logs.stderr
                        )
                    self.run(
                        *PODMAN,
                        kind,
                        "rm",
                        *(["--force"] if kind == "container" else []),
                        identifier,
                    )
                    require(
                        self.run(
                            *PODMAN, kind, "exists", identifier, check=False
                        ).returncode
                        == 1,
                        f"{kind} remains: {name}",
                    )
            except Exception as error:
                errors.append(f"{kind} {name}: {error}")
                break  # Retain dependencies and runtime when ownership or removal is uncertain.
        if not errors and self.slice_requested:
            try:
                observed = self.run(
                    "systemctl",
                    "--user",
                    "show",
                    self.slice,
                    "--property=Description,ControlGroup,LoadState",
                    check=False,
                )
                unit = dict(
                    line.split("=", 1)
                    for line in observed.stdout.splitlines()
                    if "=" in line
                )
                require(
                    observed.returncode == 0 or unit.get("LoadState") == "not-found",
                    "cannot inspect test slice: " + observed.stderr.strip(),
                )
                if unit.get("Description") == self.description:
                    self.run("systemctl", "--user", "stop", self.slice)
                    group = unit.get("ControlGroup")
                    require(
                        not group
                        or not (Path("/sys/fs/cgroup") / group.lstrip("/")).exists(),
                        "test slice remains",
                    )
                else:
                    require(
                        not unit.get("ControlGroup"), "test slice ownership mismatch"
                    )
            except Exception as error:
                errors.append(str(error))
        if not errors and self.installation is not None:
            try:
                shutil.rmtree(self.installation)
            except OSError as error:
                errors.append(str(error))
        self.report["cleanup"] = {"complete": not errors, "errors": errors}
        if errors:
            self.report["passed"] = False
        self.save()
        require(
            not errors, "cleanup incomplete; see " + str(self.output / "report.json")
        )

    def check(self, archive, pull):
        try:
            require(os.getuid() != 0, "run under an ordinary rootless Podman account")
            # Runtime helpers execute after credential changes. Keep their
            # complete path traversable, independently of private report dirs.
            self.installation = Path(
                tempfile.mkdtemp(prefix="mim-compose-runtime.", dir="/tmp")
            )
            self.installation.chmod(0o755)
            self.report["installation"] = str(self.installation)
            manifest, root = unpack(archive, self.installation)
            self.runtime = root / "profiles/online/runsc"
            require(self.runtime.is_file(), "bundle has no online profile")
            self.report.update(
                runtime=str(self.runtime),
                source_commit=manifest["source_commit"],
                runtime_version=manifest["version"],
            )
            self.report["versions"] = {
                name: self.run(*command).stdout.strip()
                for name, command in {
                    "podman": ["podman", "--version"],
                    "compose": ["podman-compose", "--version"],
                    "kernel": ["uname", "-r"],
                }.items()
            }
            self.report["selinux"] = (
                self.run("getenforce").stdout.strip()
                if shutil.which("getenforce")
                else None
            )
            require(
                self.run(
                    *PODMAN, "info", "--format", "{{.Host.Security.Rootless}}"
                ).stdout.strip()
                == "true",
                "engine is not rootless",
            )
            present = self.run(
                *PODMAN, "image", "exists", IMAGE, check=False
            ).returncode
            require(present in (0, 1), "cannot inspect fixture image presence")
            require(
                present == 0 or pull,
                "fixture image missing; rerun with --pull to download its pinned digest",
            )
            self.budget()
            if present == 1:
                self.run(
                    *PODMAN,
                    "pull",
                    "--quiet",
                    "--authfile",
                    str(self.output / "auth.json"),
                    IMAGE,
                    scoped=True,
                    timeout=180,
                )
            self.report["image_id"] = json.loads(
                self.run(*PODMAN, "image", "inspect", IMAGE).stdout
            )[0]["Id"]
            self.environment.update(
                QUALIFY_PROJECT=self.project,
                QUALIFY_TOKEN=self.token,
                QUALIFY_IMAGE=IMAGE,
                QUALIFY_RUNTIME=str(self.runtime),
            )
            self.compose_command = [
                "podman-compose",
                "--in-pod=true",
                "--pod-args=--infra=false --share= --cgroup-parent="
                + self.slice
                + " --label="
                + LABEL
                + "="
                + self.token,
                "--podman-run-args=--memory-swap=1g",
                "-p",
                self.project,
                "-f",
                str(Path(__file__).with_name("compose.yaml")),
            ]
            self.exercise()
            if self.report["selinux"] is not None:
                require(
                    self.run("getenforce").stdout.strip() == self.report["selinux"],
                    "host SELinux mode changed",
                )
            self.passed("host-selinux-mode-preserved", self.report["selinux"])
            self.report["passed"] = True
        except BaseException as error:
            self.report.update(passed=False, error=str(error) or type(error).__name__)
            raise
        finally:
            self.cleanup()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "bundle", type=Path, help="runtime .tar.bz2 with its adjacent .sha256"
    )
    parser.add_argument(
        "--pull",
        action="store_true",
        help="download the pinned fixture image if missing",
    )
    parser.add_argument(
        "--output", type=Path, help="new directory for the report and diagnostic logs"
    )
    args = parser.parse_args()
    for tool in (
        "podman",
        "podman-compose",
        "git",
        "systemd-run",
        "systemctl",
        "busctl",
    ):
        if not shutil.which(tool):
            parser.error("required tool missing: " + tool)
    if os.getuid() == 0:
        parser.error("run under an ordinary rootless Podman account")
    try:
        if args.output:
            output = args.output.resolve()
            output.mkdir(parents=True, mode=0o700)
        else:
            build = Path(__file__).resolve().parents[1] / ".mim-build"
            build.mkdir(exist_ok=True)
            output = Path(tempfile.mkdtemp(prefix="compose-", dir=build))
    except OSError as error:
        parser.error(str(error))
    print("Report:", output / "report.json", flush=True)
    signal.signal(signal.SIGTERM, interrupted)
    try:
        Qualification(output).check(args.bundle.resolve(), args.pull)
    except KeyboardInterrupt:
        print("Interrupted; cleanup details are in the report.", file=sys.stderr)
        return 130
    except Exception as error:
        print("FAIL:", error, file=sys.stderr)
        return 1
    print("Compose qualification passed; all owned test objects removed.")
    return 0


def interrupted(signum, frame):
    raise KeyboardInterrupt


if __name__ == "__main__":
    sys.exit(main())
