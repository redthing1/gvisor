#!/bin/sh
# Copyright 2026 The gVisor Authors.
# SPDX-License-Identifier: Apache-2.0
set -eu

# Podman persists this entry point, whereas runtime flags are invocation-local.
# Accept only engine-owned global options. Stop at the OCI command so that guest
# arguments (including strings resembling runtime flags) remain untouched.
reject() {
    printf 'mim runsc: unsupported global option or command: %s\n' "${1:-<missing>}" >&2
    exit 2
}
validate() {
    while [ "$#" -gt 0 ]; do
        case "$1" in
            --root|--log|--log-format)
                [ "$#" -ge 2 ] || reject "$1"
                shift 2 ;;
            --root=*|--log=*|--log-format=*|--systemd-cgroup|--systemd-cgroup=true)
                shift ;;
            --version)
                [ "$#" -eq 1 ] || reject "$1"
                return ;;
            create|start|state|exec|kill|delete|update|events|list|features|pause|resume|ps|wait)
                return ;;
            *) reject "$1" ;;
        esac
    done
    reject
}
case "$0" in /*/profiles/*/runsc) ;; *) reject "$0" ;; esac
validate "$@"
exec "${0%/*}/../../runsc" @FLAGS@ "$@"
