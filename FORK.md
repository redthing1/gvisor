# Rootless Podman fork

This fork packages gVisor for rootless Podman and Compose. The maintained branch
is `main`; `upstream` refers to <https://github.com/google/gvisor.git>.
[mim/release.json](mim/release.json) records the upstream tag, exact commit and
downstream version. Published tags identify tested builds.

## Changes from upstream

| Change | Purpose |
| --- | --- |
| Rootless systemd cgroups | Use the caller's user manager and delegated hierarchy. Includes [PR 13853](https://github.com/google/gvisor/pull/13853). |
| Resource-controller checks | Refuse unavailable requested controllers on creation, updates and restored state; respect systemd controller disablement. |
| Imported terminals | Give donated terminals guest devpts names, ownership and normal reopening/lifetime behavior. |
| Distribution tooling in `mim/` | Build complete archives, RPMs and DEBs; provide fixed runtime profiles and disposable integration checks. |

gVisor supplies the syscall sandbox. Both ordinary Podman services and Mim can
consume these packages. See [installation](mim/README.md) and
[Compose configuration](mim/compose.md).

## Update upstream

Merge a selected upstream release into `main`. Keep downstream fixes as focused
commits and remove changes when upstream supplies equivalent behavior.

```sh
git switch main
git pull --ff-only origin main
# Once per clone:
git remote add upstream https://github.com/google/gvisor.git
```

For each update, select a published release from the
[upstream releases](https://github.com/google/gvisor/releases):

```sh
upstream_tag=release-YYYYMMDD.N
git fetch --no-tags upstream tag "$upstream_tag"
git merge --no-ff --no-edit "$upstream_tag"
git diff "$upstream_tag" -- runsc pkg
```

Resolve conflicts and review the remaining runtime differences. Check build
dependencies and the upstream release layout when they change. Update
`upstream.tag` and `upstream.commit` in `mim/release.json`, using
`git rev-parse "$upstream_tag^{commit}"` for the commit. Set `version` to
`YYYYMMDD.N-mim.1`, or increment the `mim` suffix for another release on the same
base. Update the version example in `mim/compose.md` and commit the changes.

Upstream publishes weekly and has no LTS releases. Check new releases regularly
and prioritize security updates; see its [release policy](https://gvisor.dev/release/).

## Build and qualify

From the clean, committed tree, run under an ordinary rootless Podman account:

```sh
mim/build release
```

This runs focused tests and builds the archive, RPM, DEB and their checksums in
`.mim-build/out/`. The containerized toolchain uses two CPUs, 4 GiB memory and
persistent caches. Build once per architecture and reuse the packages across
qualified hosts. Subsequent builds reuse the builder image and Bazel cache;
`mim/build packages` repackages an existing archive without compiling it again.

Qualify the resulting archive on a supported deployment host:

```sh
version=$(python3 -c 'import json; print(json.load(open("mim/release.json"))["version"])')
bundle=".mim-build/out/mim-gvisor-$version-$(uname -m).tar.bz2"
python3 mim/check_compose.py "$bundle" --pull
python3 mim/check-controllers.py "$bundle" --image TRUSTED_LOCAL_IMAGE
```

The Compose check exercises Gitea HTTP Git, persistence and recreation. The
controller check uses a trusted local image with `/bin/sh`. Both use bounded,
disposable resources; see [qualification prerequisites](mim/README.md#qualification).
Keep reports and build logs local.

## Publish prebuilt packages

After qualification, tag the exact source and push it. With GitHub CLI installed
and authenticated, create a draft containing only that version's release assets:

```sh
git tag -a "$version" -m "Mim gVisor $version"
git push origin main "refs/tags/$version"
gh release create "$version" --verify-tag --draft \
  --title "Mim gVisor $version" --notes-file /path/to/release-notes.md \
  .mim-build/out/mim-gvisor-"$version"-* \
  .mim-build/out/mim-gvisor-"$version"_*
```

Release notes state the upstream base, downstream changes and qualified
host/engine combination. Verify the draft's assets and checksums, then publish:

```sh
gh release edit "$version" --draft=false --latest
```

Preserve published tags and asset bytes. Corrections get a new version.
Deployment machines download and verify the package, install it, then recreate
selected containers using its versioned runtime path. Retain older runtime
packages while containers reference them.
