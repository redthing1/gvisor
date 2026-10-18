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
	"math"
	"strconv"

	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/fspath"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/host"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/kernfs"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/kernel/auth"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
)

// ImportTTY gives an unpublished imported terminal a name in the process's
// devpts mount. It returns a new file description backed by the same terminal.
// A guest without devpts retains the original unnamed terminal behavior.
func ImportTTY(ctx context.Context, tty *host.TTYFileDescription) (*vfs.FileDescription, error) {
	k := kernel.KernelFromContext(ctx)
	root := vfs.RootFromContext(ctx)
	if !root.Ok() {
		tty.VFSFileDescription().IncRef()
		return tty.VFSFileDescription(), nil
	}
	defer root.DecRef(ctx)
	creds := auth.CredentialsFromContext(ctx)
	pts, err := k.VFS().GetDentryAt(ctx, creds, &vfs.PathOperation{
		Root: root, Start: root, Path: fspath.Parse("/dev/pts"), FollowFinalSymlink: true,
	}, &vfs.GetDentryOptions{})
	if err != nil {
		if linuxerr.Equals(linuxerr.ENOENT, err) {
			tty.VFSFileDescription().IncRef()
			return tty.VFSFileDescription(), nil
		}
		return nil, err
	}
	defer pts.DecRef(ctx)
	fs, ok := pts.Mount().Filesystem().Impl().(*filesystem)
	if !ok || pts.Dentry() != pts.Mount().Root() {
		tty.VFSFileDescription().IncRef()
		return tty.VFSFileDescription(), nil
	}
	i := pts.Dentry().Impl().(*kernfs.Dentry).Inode().(*rootInode)
	i.mu.Lock()
	if i.nextIdx == math.MaxUint32 {
		i.mu.Unlock()
		return nil, linuxerr.ENOMEM
	}
	index := i.nextIdx
	i.nextIdx++
	i.replicas[index] = tty.RegisterDevice(i, pts.Mount(), fs.devMinor, index)
	i.mu.Unlock()

	vd, err := k.VFS().GetDentryAt(ctx, creds, &vfs.PathOperation{
		Root: pts, Start: pts, Path: fspath.Parse(strconv.FormatUint(uint64(index), 10)),
	}, &vfs.GetDentryOptions{})
	if err != nil {
		return nil, err
	}
	defer vd.DecRef(ctx)
	return tty.TTY().OpenTTY(ctx, vd.Mount(), vd.Dentry(), vfs.OpenOptions{Flags: tty.VFSFileDescription().StatusFlags()})
}

// UnregisterTTY removes the weak reference to a released imported terminal.
//
// +checklocksexclude:i.mu
func (i *rootInode) UnregisterTTY(ctx context.Context, index uint32) {
	i.mu.Lock()
	delete(i.replicas, index)
	i.mu.Unlock()
}
