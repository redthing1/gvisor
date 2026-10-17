#!/usr/bin/env python3
# Copyright 2026 The gVisor Authors.
# SPDX-License-Identifier: Apache-2.0

"""Build coinstallable RPM and Debian packages from a verified runtime bundle."""

import json
import os
import re
import shutil
import subprocess
import sys
import tarfile
import tempfile
from pathlib import Path, PurePosixPath

from package import sha256

ARCHITECTURES = {"x86_64": ("amd64", "x86_64"), "aarch64": ("arm64", "aarch64")}


def unpack(archive: Path, destination: Path) -> tuple[dict, Path]:
    checksum = archive.with_suffix(archive.suffix + ".sha256").read_text().split()
    if checksum != [sha256(archive), archive.name]:
        raise ValueError("bundle checksum mismatch")
    prefix = archive.name.removesuffix(".tar.bz2")
    with tarfile.open(archive) as payload:
        names = [entry.name for entry in payload]
        for entry in payload:
            if (
                not entry.isfile()
                or not entry.name.startswith(prefix + "/")
                or ".." in PurePosixPath(entry.name).parts
                or PurePosixPath(entry.name).as_posix() != entry.name
                or entry.mode not in (0o644, 0o755)
            ):
                raise ValueError("unexpected bundle entry")
        if len(set(names)) != len(names):
            raise ValueError("duplicate bundle entry")
        # Extract only validated regular files into the private package tree.
        for entry in payload:
            target = destination / entry.name
            target.parent.mkdir(parents=True, exist_ok=True)
            with target.open("xb") as output, payload.extractfile(entry) as source:
                shutil.copyfileobj(source, output)
            target.chmod(entry.mode)
    root = destination / prefix
    manifest = json.loads((root / "manifest.json").read_text())
    version = manifest["version"]
    if not re.fullmatch(r"[0-9][a-z0-9.+-]*", version):
        raise ValueError("invalid release version")
    expected = {f"{prefix}/{name}" for name in manifest["files"]}
    if set(names) != expected | {f"{prefix}/manifest.json"}:
        raise ValueError("bundle payload differs from manifest")
    for name, digest in manifest["files"].items():
        if sha256(root / name) != digest:
            raise ValueError(f"bundle file checksum mismatch: {name}")
    return manifest, root


def packages(archive: Path, destination: Path) -> list[Path]:
    archive, destination = archive.resolve(), destination.resolve()
    with tempfile.TemporaryDirectory(prefix="mim-packages-") as directory:
        work = Path(directory)
        manifest, payload = unpack(archive, work)
        version = manifest["version"]
        deb_arch, rpm_arch = ARCHITECTURES[manifest["architecture"]]
        name = f"mim-gvisor-{version}"
        install = Path("usr/lib/mim-gvisor") / version
        root = work / "root"
        shutil.copytree(payload, root / install)
        environment = {
            **os.environ,
            "SOURCE_DATE_EPOCH": str(manifest["source_date_epoch"]),
        }
        control = root / "DEBIAN"
        control.mkdir()
        (control / "control").write_text(
            f"Package: {name}\nVersion: {version}\nArchitecture: {deb_arch}\n"
            "Maintainer: Mim maintainers <redthing1@alt.icu>\n"
            "Section: admin\nPriority: optional\nDepends: dash\n"
            "Homepage: https://github.com/redthing1/gvisor\n"
            "Description: Versioned gVisor runtime for rootless Podman\n"
            " Includes fixed OCI runtime profiles. No engine configuration is changed.\n"
        )
        deb = destination / f"{name}_{version}_{deb_arch}.deb"
        subprocess.run(
            ["dpkg-deb", "--root-owner-group", "--build", str(root), str(deb)],
            check=True,
            env=environment,
        )
        shutil.rmtree(control)
        rpm_version, rpm_release = version.rsplit("-", 1)
        spec = work / "runtime.spec"
        spec.write_text(
            f"Name: {name}\nVersion: {rpm_version}\nRelease: {rpm_release}\n"
            "Summary: Versioned gVisor runtime for rootless Podman\n"
            "License: Apache-2.0\nURL: https://github.com/redthing1/gvisor\n"
            f"BuildArch: {rpm_arch}\nAutoReqProv: no\nRequires: /bin/sh\n"
            # Preserve upstream binaries and the checksums in their manifest.
            "%global __os_install_post %{nil}\n%global _build_id_links none\n"
            "%description\nVersioned gVisor with fixed OCI runtime profiles.\n"
            "%install\n"
            f"mkdir -p %{{buildroot}}/usr/lib/mim-gvisor\n"
            f"cp -a {root / install} %{{buildroot}}/usr/lib/mim-gvisor/\n"
            f"%files\n%defattr(-,root,root,-)\n/{install}\n"
        )
        subprocess.run(
            [
                "rpmbuild",
                "-bb",
                str(spec),
                "--define",
                f"_topdir {work / 'rpm'}",
                "--define",
                "_buildhost localhost",
                "--define",
                "use_source_date_epoch_as_buildtime 1",
                "--define",
                "_binary_filedigest_algorithm 8",
                "--define",
                "clamp_mtime_to_source_date_epoch 1",
            ],
            check=True,
            env=environment,
        )
        built = list((work / "rpm/RPMS" / rpm_arch).glob("*.rpm"))
        if len(built) != 1:
            raise ValueError("expected exactly one RPM")
        rpm = destination / built[0].name
        shutil.copyfile(built[0], rpm)
        outputs = [deb, rpm]
        for output in outputs:
            output.with_suffix(output.suffix + ".sha256").write_text(
                f"{sha256(output)}  {output.name}\n"
            )
        return outputs


if __name__ == "__main__":
    if len(sys.argv) != 3:
        sys.exit("usage: distro.py BUNDLE DESTINATION")
    for output in packages(Path(sys.argv[1]), Path(sys.argv[2])):
        print(output.name)
