# Mim gVisor

A downstream gVisor distribution for rootless Podman, usable directly by Compose
or through Mim. Source versions and patch provenance are recorded in
[release.json](release.json).

## Install and update

Download the package for your architecture and its adjacent `.sha256` file from
[releases](https://github.com/redthing1/gvisor/releases). Verify it in the download
directory, then install the local RPM on Fedora:

```sh
sha256sum --check PACKAGE.rpm.sha256
sudo dnf install ./PACKAGE.rpm
```

The package is named `mim-gvisor` and installs under `/usr/lib/mim-gvisor/`.
Release versions remain in package metadata, filenames and `manifest.json`;
Compose runtime paths stay unchanged across upgrades. Packages contain no
install scripts and change no engine configuration. Workloads run under the
rootless deployment account.

For standalone HTTP services behind a host reverse proxy, follow the
[Compose recipe](compose.md). Mim is optional.

To update, stop the affected containers, download and verify the new package,
install it using the same command, then recreate the containers. The package
manager replaces the installed release. Installing binaries does not update
running sandboxes. Packages are distributed through GitHub releases; host package
updates alone do not download a new release.

Packages before `20261019.0-mim.2` used versioned names and directories. Change
their Compose runtime paths once to `/usr/lib/mim-gvisor/profiles/online/runsc`
(or the selected profile), recreate the containers, then remove the old packages
when nothing references them.

For an archive installation, keep the complete layout together, including
`gvisor-bin/`. Runtime helpers need to traverse that path
after changing credentials; a private home directory can prevent execution.

## Build

From a clean checkout under an ordinary Linux user with working rootless Podman:

```sh
mim/build release
```

This runs the focused downstream tests, builds the complete runtime bundle and
produces RPM and Debian packages in `.mim-build/out/`. Use `test` for checks alone,
`bundle` for the archive, or `packages` to package an existing archive.

The host needs Bash, Git, coreutils and rootless Podman with enforced cgroup v2
limits. The compiler and packaging toolchains run in a pinned container, limited
to two CPUs, 4 GiB memory and two build jobs. The builder uses a fixed Debian
package snapshot, verified Bazel and upstream dependency declarations. Caches
live under `.mim-build/cache/`.

Persistent caches make subsequent builds incremental. Build once per architecture
and copy the packages to qualified hosts; deployment machines need no toolchain.
For upstream updates and publishing, follow [FORK.md](../FORK.md).

The archive includes runsc, sidecars, the containerd shim, license and a manifest
identifying the source commit, builder image and file checksums. Artifacts have
adjacent SHA-256 files. Packages are unsigned; obtain them from a trusted build
or transfer. Checksums detect changes but do not authenticate a publisher.

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

Profiles require Podman's systemd cgroup driver, a user systemd manager and
cgroup v2 delegation. They use Gofer-mediated filesystem access and Podman's
writable rootfs. Compose and the engine determine mounts, credentials and other
grants; the profiles do not provide Mim's grant verification or aggregate helper
budgets.

## Qualification

Qualify a bundle on the intended host before adopting it. The checks require
Python 3.11+, Git and a working rootless Podman account with a user systemd
manager; the Compose check also needs podman-compose and pasta from passt.

```sh
python3 mim/check_compose.py BUNDLE --pull
python3 mim/check-controllers.py BUNDLE --image TRUSTED_LOCAL_IMAGE
```

`BUNDLE` is the `.tar.bz2` archive with its adjacent `.sha256`. The Compose check
uses a digest-pinned Gitea image and synthetic data to exercise HTTP Git,
health-check exec, SQLite persistence, graceful stop/start and recreation. Its
outer budget is two CPUs, 2 GiB memory, no swap and 1,024 tasks. `--pull` permits
downloading the fixture if missing. Fixture images stay in Podman's shared cache
for repeated checks.

The controller probe needs a trusted local image containing `/bin/sh`. It checks
refusal of an unavailable memory controller inside an independent budget,
without starting the application.

Both checks operate on uniquely owned test resources. The Compose check prints
its report path and retains logs in `.mim-build/compose-*`; `--output NEW_DIRECTORY`
selects another location. Cleanup uncertainty fails the check and preserves the
temporary runtime for diagnosis.
