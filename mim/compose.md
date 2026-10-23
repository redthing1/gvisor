# Rootless Compose

Use the `online` runtime profile for a standalone HTTP service behind a
host-installed reverse proxy. Mim is not required.

## Configure

Install a runtime release using the [build and install instructions](README.md).
The host needs working rootless Podman with its systemd cgroup driver,
podman-compose, pasta from passt, a user systemd manager and cgroup v2 delegation.

Add these settings to an existing service, retaining its image, storage and
application security policy:

```yaml
services:
  app:
    runtime: /usr/lib/mim-gvisor/profiles/online/runsc
    network_mode: pasta:--no-splice
    security_opt:
      - label=disable
    ports:
      - "127.0.0.1:3000:3000"
```

Point the host reverse proxy, such as Nginx, at `http://127.0.0.1:3000`. Configure
the application's public URL and proxy handling normally. Git over HTTP uses the
same route. Adjust the port to match the application.

Package upgrades keep this runtime path unchanged. Stop the affected services,
install the updated package and recreate them with
`podman-compose up -d --force-recreate`. A running sandbox uses the old runtime until it
is recreated.

## Network and isolation

`pasta:--no-splice` gives the service a private network with outbound access and
the declared localhost port. It replaces Compose bridge membership and
service-name DNS. This recipe does not cover stacks that require those bridge
networks. With Podman 5.8.4, publishing a gVisor bridge service through the
rootlessport helper does not work.

The per-container `label=disable` setting is required by runsc. Host SELinux
continues enforcing its policy, but ordinary per-container MCS labels are not
applied to the gVisor host processes.

gVisor mediates application system calls, and rootlessness limits host authority
to the deployment account. Application mounts, secrets and reachable services
remain accessible according to the deployment's grants. The profile excludes
GPU access and host Unix sockets. Resource ceilings are separate, optional
deployment policy. With podman-compose 1.6.0, `mem_limit`, `cpus` and `pids_limit`
are applied; `memswap_limit` and `cgroup_parent` are ignored and need explicit
Podman arguments. Verify effective limits when configuring them.

The maintained [host qualification](README.md#qualification) exercises this
network pattern and a Gitea HTTP/Git workflow. It has been validated with
Podman 5.8.4 and podman-compose 1.6.0. Qualify the bundle on your intended host
and retain your existing supervision and backup arrangement.
