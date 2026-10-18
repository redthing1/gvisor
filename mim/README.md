# Mim gVisor

A downstream gVisor distribution for rootless Podman. Source versions and patch
provenance are recorded in [release.json](release.json).

## Build

Run from the checkout under an ordinary Linux user with rootless Podman:

```sh
mim/build test
mim/build bundle
mim/build packages
```

The builder uses a pinned Debian image and package snapshot, verified Bazel,
and the upstream dependency declarations. It runs without privileged mode or a
host engine socket, with two CPUs, 4 GiB memory, and two concurrent build jobs.
Build caches and output live under `.mim-build/`.

The test action runs cgroup, terminal, static-analysis, runtime-profile, and
packaging checks, including terminal syscall tests against native Linux.
Bundle creation requires a clean checkout. The archive contains runsc, its
sidecars, the containerd shim, license, and a manifest identifying the source,
builder image, and file checksums. A SHA-256 file accompanies the archive.

`packages` wraps the current bundle as RPM and Debian packages. Each release has
its own package name and installs under `/usr/lib/mim-gvisor/VERSION/`, so upgrades
can retain runtimes needed by existing containers. Packages contain no install
scripts and change no engine configuration. Remove old packages only after their
containers have been removed or explicitly upgraded.

Archives can instead be extracted into an operator-chosen versioned directory.
Keep the complete layout together, including `gvisor-bin/`.

## Runtime profiles

Use the absolute `profiles/PROFILE/runsc` path with Podman's `--runtime` option:

| Profile | Network | Host Unix sockets |
| --- | --- | --- |
| `offline` | None | Disabled |
| `online` | Sandbox | Disabled |
| `offline-agent` | None | Enabled |
| `online-agent` | Sandbox | Enabled |

Profiles fix the settings in [policy.json](policy.json) across separate Podman
invocations. They accept engine-owned OCI global options and reject runtime flag
overrides. Guest command arguments are passed through unchanged. Socket-enabled
profiles allow access to any reachable host Unix socket; mounts determine what
is reachable. The base `runsc` remains available for direct runtime use.

Profiles require the systemd cgroup driver and retain Gofer-mediated filesystem
access and Podman's writable rootfs. They do not verify the engine's OCI grants
or provide an aggregate resource budget for conmon and networking helpers.

To exercise controller refusal on a Linux host with a user systemd manager:

```sh
python3 mim/check-controllers.py BUNDLE --image TRUSTED_LOCAL_IMAGE
```

The probe verifies an independent ancestor budget before creating its test
container. It disables memory control only in an owned child slice, initializes
without starting a workload, and removes its container and transient units.

## Maintain

Keep `master` aligned with upstream. Downstream release branches start at the
tag in `release.json`; carry focused fixes as separate commits with their
upstream provenance. Advance the base by replaying those changes onto a new
release and running the tests and runtime qualification. Published builds retain
their source identity.
