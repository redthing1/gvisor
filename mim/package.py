#!/usr/bin/env python3
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

"""Package the upstream release layout with source and artifact identity."""

import hashlib
import io
import json
import os
import platform
import re
import shlex
import subprocess
import sys
import tarfile
from pathlib import Path


def git(*args: str) -> str:
    return subprocess.check_output(["git", *args], text=True).strip()


def sha256(path: Path) -> str:
    with path.open("rb") as source:
        return hashlib.file_digest(source, "sha256").hexdigest()


def package(stage: Path, destination: Path) -> Path:
    if git("status", "--porcelain"):
        raise ValueError("bundle requires a clean source checkout")
    release = json.loads(Path("mim/release.json").read_text())
    if not re.fullmatch(r"[0-9][a-z0-9.+-]*", release["version"]):
        raise ValueError("invalid release version")
    subprocess.run(
        ["git", "merge-base", "--is-ancestor", release["upstream"]["commit"], "HEAD"],
        check=True,
    )
    files = {}
    for path in sorted(stage.rglob("*")):
        if path.is_symlink():
            raise ValueError(f"release payload contains a symlink: {path}")
        if path.is_file():
            files[path.relative_to(stage).as_posix()] = path
    if not {"runsc", "gvisor-bin/gvisor_sentry"} <= files.keys():
        raise ValueError("incomplete runtime distribution")
    policy = json.loads(Path("mim/policy.json").read_text())
    template = Path("mim/runtime.sh").read_text()
    profiles = {}
    for name, extra_flags in policy["profiles"].items():
        flags = [*policy["flags"], *extra_flags]
        relative = f"profiles/{name}/runsc"
        path = stage / relative
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(
            template.replace("@FLAGS@", shlex.join([f"--{f}" for f in flags]))
        )
        path.chmod(0o755)
        files[relative] = path
        profiles[name] = {"path": relative, "flags": flags}
    files["LICENSE"] = Path("LICENSE")
    epoch = int(git("show", "-s", "--format=%ct", "HEAD"))
    release.update(
        source_date_epoch=epoch,
        policy_version=policy["version"],
        profiles=profiles,
        source_commit=git("rev-parse", "HEAD"),
        architecture=platform.machine(),
        builder_image=os.environ["MIM_BUILDER_IMAGE"],
        bazel_version=Path(".bazelversion").read_text().strip(),
        files={name: sha256(path) for name, path in sorted(files.items())},
    )
    manifest = (json.dumps(release, indent=2, sort_keys=True) + "\n").encode()
    name = f"mim-gvisor-{release['version']}-{release['architecture']}"
    archive = destination / f"{name}.tar.bz2"
    with tarfile.open(archive, "w:bz2") as output:
        for path in sorted([*files, "manifest.json"]):
            entry = tarfile.TarInfo(f"{name}/{path}")
            entry.mtime = epoch
            entry.mode = 0o644 if path in {"LICENSE", "manifest.json"} else 0o755
            if path == "manifest.json":
                entry.size = len(manifest)
                output.addfile(entry, io.BytesIO(manifest))
            else:
                entry.size = files[path].stat().st_size
                with files[path].open("rb") as source:
                    output.addfile(entry, source)
    digest = sha256(archive)
    archive.with_suffix(archive.suffix + ".sha256").write_text(
        f"{digest}  {archive.name}\n"
    )
    return archive


if __name__ == "__main__":
    if len(sys.argv) != 3:
        sys.exit("usage: package.py STAGE DESTINATION")
    print(package(Path(sys.argv[1]), Path(sys.argv[2])).name)
