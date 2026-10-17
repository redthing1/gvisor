# Mim gVisor

A downstream gVisor distribution for rootless Podman. Source versions and patch
provenance are recorded in [release.json](release.json).

## Build

Run from the checkout under an ordinary Linux user with rootless Podman:

```sh
mim/build test
mim/build bundle
```

The builder uses a pinned Debian image and package snapshot, verified Bazel,
and the upstream dependency declarations. It runs without privileged mode or a
host engine socket, with two CPUs, 4 GiB memory, and two concurrent build jobs.
Build caches and output live under `.mim-build/`.

The test action runs the cgroup and build-tooling suites.
Bundle creation requires a clean checkout. The archive contains runsc, its
sidecars, the containerd shim, license, and a manifest identifying the source,
builder image, and file checksums. A SHA-256 file accompanies the archive.

Extract the complete archive into a versioned installation directory and use
its absolute runsc path. Keep `gvisor-bin/` beside runsc. Building does not install
a runtime or change host engine configuration.

## Maintain

Keep `master` aligned with upstream. Downstream release branches start at the
tag in `release.json`; carry focused fixes as separate commits with their
upstream provenance. Advance the base by replaying those changes onto a new
release and running the tests and runtime qualification. Published builds retain
their source identity.
