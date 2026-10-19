# Mim gVisor

A downstream gVisor distribution for rootless Podman, usable directly by Compose
or through Mim. Source versions and patch provenance are recorded in
[release.json](release.json).

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

The archive includes runsc, sidecars, the containerd shim, license and a manifest
identifying the source commit, builder image and file checksums. Artifacts have
adjacent SHA-256 files. Packages are unsigned; obtain them from a trusted build
or transfer. Checksums detect changes but do not authenticate a publisher.

## Install and update

Build once per architecture and copy the resulting package to compatible hosts.
On Fedora, install the selected local RPM with DNF:

```sh
sudo dnf install /path/to/package.rpm
```

Each release has its own package name and installs under
`/usr/lib/mim-gvisor/VERSION/`. Packages contain no install scripts and change no
engine configuration. Workloads run under the rootless deployment account.

For an archive installation, keep the complete layout together in a versioned
directory, including `gvisor-bin/`. Runtime helpers need to traverse that path
after changing credentials; a private home directory can prevent execution.

To update, install the new release alongside the old one, select its versioned
runtime path and recreate the affected containers. Installing binaries does not
update a running sandbox. Retain old packages while running or stopped containers
reference them. Distinct package names make release installation explicit;
ordinary `dnf upgrade` does not select a newly named runtime release.

For standalone HTTP services behind a host reverse proxy, see
[rootless Compose](compose.md).

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

## Maintain

Keep focused downstream commits on the upstream release recorded in
`release.json`. For an update, replay required changes onto the new base, drop
changes supplied upstream and select a new downstream version. Commit the source,
run `mim/build release` and qualify the resulting bundle on supported hosts.
Tag the qualified source and preserve published artifact identity.

Upstream publishes weekly and provides no LTS releases. Track its
[release policy](https://gvisor.dev/release/) and security fixes, and maintain a
regular tested update cycle. Qualify Podman/provider changes as well as runtime
changes; the engine and network helpers remain part of the deployment.
