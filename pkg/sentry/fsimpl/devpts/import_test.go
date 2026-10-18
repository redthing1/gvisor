// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package devpts

import (
	"fmt"
	"testing"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/fspath"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/host"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/tmpfs"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/kernel/auth"
	kernelcontexttest "gvisor.dev/gvisor/pkg/sentry/kernel/contexttest"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
)

func TestImportedTTYDeviceIdentityAndLifetime(t *testing.T) {
	for _, detach := range []bool{false, true} {
		t.Run(fmt.Sprintf("detach=%t", detach), func(t *testing.T) {
			testImportedTTYDeviceIdentityAndLifetime(t, detach)
		})
	}
}

func testImportedTTYDeviceIdentityAndLifetime(t *testing.T, detach bool) {
	ctx := kernelcontexttest.Context(t)
	creds := auth.NewRootCredentials(auth.NewRootUserNamespace())
	ctx = auth.ContextWithCredentials(ctx, creds)
	k := kernel.KernelFromContext(ctx)
	if err := k.VFS().Init(ctx); err != nil {
		t.Fatal(err)
	}
	k.VFS().MustRegisterFilesystemType("tmpfs", tmpfs.FilesystemType{}, &vfs.RegisterFilesystemTypeOptions{AllowUserMount: true})
	k.VFS().MustRegisterFilesystemType(Name, &FilesystemType{}, &vfs.RegisterFilesystemTypeOptions{AllowUserMount: true})
	mntns, err := k.VFS().NewMountNamespace(ctx, creds, "", "tmpfs", &vfs.MountOptions{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mntns.DecRef(ctx)
	root := mntns.Root(ctx)
	defer root.DecRef(ctx)
	ctx = vfs.WithRoot(vfs.WithMountNamespace(ctx, mntns), root)
	path := func(name string) vfs.PathOperation {
		return vfs.PathOperation{Root: root, Start: root, Path: fspath.Parse(name)}
	}
	for _, name := range []string{"dev", "dev/pts"} {
		pop := path(name)
		if err := k.VFS().MkdirAt(ctx, creds, &pop, &vfs.MkdirOptions{Mode: 0755}); err != nil {
			t.Fatal(err)
		}
	}
	pop := path("dev/pts")
	_, err = k.VFS().MountAt(ctx, creds, "", &pop, Name, &vfs.MountOptions{GetFilesystemOptions: vfs.GetFilesystemOptions{Data: "newinstance"}})
	if err != nil {
		t.Fatal(err)
	}
	hostfs, err := host.NewFilesystem(k.VFS())
	if err != nil {
		t.Fatal(err)
	}
	hostMount := k.VFS().NewDisconnectedMount(hostfs, nil, &vfs.MountOptions{})
	defer hostMount.DecRef(ctx)

	master, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(master)
	if err := unix.IoctlSetPointerInt(master, unix.TIOCSPTLCK, 0); err != nil {
		t.Fatal(err)
	}
	number, err := unix.IoctlGetInt(master, unix.TIOCGPTN)
	if err != nil {
		t.Fatal(err)
	}
	slave, err := unix.Open(fmt.Sprintf("/dev/pts/%d", number), unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	file, err := host.NewFD(ctx, hostMount, slave, &host.NewFDOptions{IsTTY: true, VirtualOwner: true, UID: 1234, GID: 5678})
	if err != nil {
		unix.Close(slave)
		t.Fatal(err)
	}
	imported, err := ImportTTY(ctx, file.Impl().(*host.TTYFileDescription))
	file.DecRef(ctx)
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	defer func() {
		if !closed {
			imported.DecRef(ctx)
		}
	}()
	if got, err := k.VFS().PathnameWithDeleted(ctx, root, imported.VirtualDentry()); err != nil || got != "/dev/pts/0" {
		t.Fatalf("imported path: got %q, %v, want /dev/pts/0", got, err)
	}
	stat, err := imported.Stat(ctx, vfs.StatOptions{Mask: linux.STATX_BASIC_STATS})
	if err != nil {
		t.Fatal(err)
	}
	pop = path("dev/pts/0")
	named, err := k.VFS().StatAt(ctx, creds, &pop, &vfs.StatOptions{Mask: linux.STATX_BASIC_STATS})
	if err != nil {
		t.Fatal(err)
	}
	if stat.Ino != named.Ino || stat.DevMinor != named.DevMinor || stat.RdevMajor != linux.UNIX98_PTY_REPLICA_MAJOR || stat.RdevMinor != 0 || stat.UID != 1234 || stat.GID != 5678 {
		t.Fatalf("inconsistent imported terminal identity: fd=%+v, path=%+v", stat, named)
	}
	other := auth.NewUserCredentials(2345, 2345, nil, &auth.TaskCapabilities{}, creds.UserNamespace)
	if _, err := k.VFS().OpenAt(ctx, other, &pop, &vfs.OpenOptions{Flags: linux.O_RDWR | linux.O_NOCTTY}); !linuxerr.Equals(linuxerr.EACCES, err) {
		t.Fatalf("unrelated user opened imported terminal: %v", err)
	}
	reopened, err := k.VFS().OpenAt(ctx, creds, &pop, &vfs.OpenOptions{Flags: linux.O_RDWR | linux.O_NOCTTY})
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Impl().(*host.TTYFileDescription).TTY() != imported.Impl().(*host.TTYFileDescription).TTY() {
		reopened.DecRef(ctx)
		t.Fatal("reopening the name created a different controlling terminal")
	}
	imported.DecRef(ctx)
	closed = true
	if _, err := k.VFS().StatAt(ctx, creds, &pop, &vfs.StatOptions{Mask: linux.STATX_BASIC_STATS}); err != nil {
		t.Fatalf("name disappeared while a reopened descriptor remained: %v", err)
	}
	// A session may retain its controlling terminal after closing every file.
	tty := reopened.Impl().(*host.TTYFileDescription).TTY()
	fs := reopened.Mount().Filesystem()
	tty.IncRef()
	ttyHeld := true
	defer func() {
		if ttyHeld {
			tty.DecRef(ctx)
		}
	}()
	reopened.DecRef(ctx)
	if _, err := k.VFS().StatAt(ctx, creds, &pop, &vfs.StatOptions{Mask: linux.STATX_BASIC_STATS}); err != nil {
		t.Fatalf("name disappeared while a controlling terminal reference remained: %v", err)
	}
	if detach {
		mountpoint := path("dev/pts")
		if err := k.VFS().UmountAt(ctx, creds, &mountpoint, &vfs.UmountOptions{Flags: linux.MNT_DETACH}); err != nil {
			t.Fatal(err)
		}
		if !fs.TryIncRef() {
			t.Fatal("detached filesystem released while the controlling terminal remained")
		}
		fs.DecRef(ctx)
	}
	// Weak directory entries must not keep the terminal or its mount alive.
	tty.DecRef(ctx)
	ttyHeld = false
	if detach {
		if fs.TryIncRef() {
			fs.DecRef(ctx)
			t.Fatal("detached filesystem retained after releasing the terminal")
		}
		return
	}
	if _, err := k.VFS().StatAt(ctx, creds, &pop, &vfs.StatOptions{Mask: linux.STATX_BASIC_STATS}); !linuxerr.Equals(linuxerr.ENOENT, err) {
		t.Fatalf("released terminal name remains: %v", err)
	}
}
