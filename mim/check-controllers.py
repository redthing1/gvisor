#!/usr/bin/env python3
# Copyright 2026 The gVisor Authors.
# SPDX-License-Identifier: Apache-2.0

"""Verify refusal of an unavailable memory controller inside a separate budget.

Only unique test containers and transient user units are created. The workload
is never started. No host controller settings or installed runtimes are changed.
"""

import argparse
import json
import os
import shutil
import subprocess
import tempfile
import uuid
from pathlib import Path

from distro import unpack


def run(*args, check=True):
    return subprocess.run(args, check=check, capture_output=True, text=True, timeout=60)


def start_slice(name, properties):
    run(
        "busctl",
        "--user",
        "call",
        "org.freedesktop.systemd1",
        "/org/freedesktop/systemd1",
        "org.freedesktop.systemd1.Manager",
        "StartTransientUnit",
        "ssa(sv)a(sa(sv))",
        name,
        "fail",
        str(len(properties)),
        *(value for prop in properties for value in prop),
        "0",
    )


def check(archive: Path, image: str, report_path: Path | None):
    if os.getuid() == 0:
        raise ValueError("run under an ordinary user with local rootless Podman")
    token = uuid.uuid4().hex
    outer = f"mimcheck{token}.slice"
    inner = f"mimcheck{token}-disabled.slice"
    name = f"mim-gvisor-check-{token}"
    installation = Path(tempfile.mkdtemp(prefix="mim-gvisor-check.", dir="/tmp"))
    installation.chmod(0o755)
    cidfile = installation / "container.id"
    podman = ["podman", "--remote=false", "--cgroup-manager=systemd"]
    report = {"outer": outer, "inner": inner, "installation": str(installation)}
    removed = False
    owner = None
    try:
        run(*podman, "unshare", "/usr/bin/true")
        start_slice(
            outer,
            [
                ("Description", "s", "Mim controller regression budget"),
                ("MemoryMax", "t", "1073741824"),
                ("MemorySwapMax", "t", "0"),
                ("CPUQuotaPerSecUSec", "t", "1000000"),
                ("CPUQuotaPeriodUSec", "t", "100000"),
                ("TasksMax", "t", "512"),
            ],
        )
        relative = run(
            "systemctl", "--user", "show", outer, "--property=ControlGroup", "--value"
        ).stdout.strip()
        owner = Path("/sys/fs/cgroup") / relative.lstrip("/")
        report["outer_limits"] = {
            key: (owner / key).read_text().strip()
            for key in ("memory.max", "memory.swap.max", "cpu.max", "pids.max")
        }
        if report["outer_limits"] != {
            "memory.max": "1073741824",
            "memory.swap.max": "0",
            "cpu.max": "100000 100000",
            "pids.max": "512",
        }:
            raise ValueError(
                "independent outer limits are not enforced; no container created"
            )
        start_slice(
            inner,
            [
                ("Description", "s", "Mim unavailable controller regression"),
                ("DisableControllers", "as", "1", "memory"),
            ],
        )
        manifest, distribution = unpack(archive.resolve(), installation)
        runtime = distribution / "profiles/offline/runsc"
        podman += ["--runtime", str(runtime)]
        run(
            *podman,
            "create",
            "--cidfile",
            str(cidfile),
            "--name",
            name,
            "--label",
            f"io.mim.gvisor.check={token}",
            "--pull=never",
            "--http-proxy=false",
            "--network=none",
            "--cgroup-parent",
            inner,
            "--memory=1g",
            "--memory-swap=1g",
            "--cpus=1",
            "--pids-limit=512",
            "--user=0:0",
            "--security-opt=no-new-privileges",
            "--security-opt=label=disable",
            "--entrypoint=/bin/sh",
            image,
            "-c",
            "exit 97",
        )
        cid = cidfile.read_text().strip()
        report.update(container_id=cid, source_commit=manifest["source_commit"])
        result = run(*podman, "init", cid, check=False)
        report.update(
            init_returncode=result.returncode, init_error=result.stderr.strip()
        )
        if result.returncode == 0:
            raise ValueError(
                "runtime accepted a requested controller disabled by systemd"
            )
        if "memory" not in result.stderr or "not available" not in result.stderr:
            raise ValueError(
                f"initialization failed for an unexpected reason: {result.stderr.strip()}"
            )
        report["passed"] = True
    except BaseException as error:
        report["error"] = str(error)
        raise
    finally:
        # The reserved name is only a recovery locator; remove a freshly
        # inspected full ID after checking this invocation's unique label.
        inspected = run(
            *podman,
            "inspect",
            cidfile.read_text().strip() if cidfile.exists() else name,
            check=False,
        )
        if inspected.returncode == 0:
            observed = json.loads(inspected.stdout)[0]
            if observed["Config"]["Labels"].get("io.mim.gvisor.check") != token:
                raise ValueError("test container ownership mismatch; cleanup refused")
            cid = observed["Id"]
            result = run(*podman, "rm", "--force", "--ignore", cid, check=False)
            removed = (
                result.returncode == 0
                and run(*podman, "container", "exists", cid, check=False).returncode
                == 1
            )
        else:
            removed = (
                run(*podman, "container", "exists", name, check=False).returncode == 1
            )
        report["container_removed"] = removed
        if removed:
            run("systemctl", "--user", "stop", outer, check=False)
            report["owner_removed"] = owner is None or not owner.exists()
            if report["owner_removed"]:
                shutil.rmtree(installation)
        if report_path:
            report_path.write_text(json.dumps(report, indent=2) + "\n")
        if not removed or not report.get("owner_removed", False):
            raise ValueError(
                f"test cleanup incomplete; retained runtime at {installation}"
            )
    print(
        "Requested memory controller refused; test container and user slices removed."
    )


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("bundle", type=Path)
    parser.add_argument(
        "--image", required=True, help="trusted local image containing /bin/sh"
    )
    parser.add_argument("--report", type=Path)
    args = parser.parse_args()
    check(args.bundle, args.image, args.report)
