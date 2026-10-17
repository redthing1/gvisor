// Copyright The runc Authors.
// Copyright The containerd Authors.
// Copyright 2022 The gVisor Authors.
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

package cgroup

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	systemdDbus "github.com/coreos/go-systemd/v22/dbus"
	dbus "github.com/godbus/dbus/v5"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

var (
	defaultProps         = []systemdDbus.Property{}
	mandatoryControllers = []string{"cpu", "cpuset", "io", "memory", "pids"}
)

func TestIsValidSlice(t *testing.T) {
	for _, tc := range []struct {
		name  string
		slice string
		err   error
	}{
		{
			name:  "success",
			slice: "system.slice",
		},
		{
			name:  "root slice",
			slice: "-.slice",
		},
		{
			name:  "path in slice",
			slice: "system-child-grandchild.slice",
		},
		{
			name:  "bad suffix",
			slice: "system.scope",
			err:   ErrInvalidSlice,
		},
		{
			name:  "has path separators",
			slice: "systemd.slice/child.slice",
			err:   ErrInvalidSlice,
		},
		{
			name:  "invalid separator pattern",
			slice: "systemd--child.slice",
			err:   ErrInvalidSlice,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validSlice(tc.slice)
			if !errors.Is(err, tc.err) {
				t.Errorf("validSlice(%s) = %v, want %v", tc.slice, err, tc.err)
			}
		})
	}
}

func TestExpandSlice(t *testing.T) {
	original := "test-a-b.slice"
	want := "/test.slice/test-a.slice/test-a-b.slice"
	expanded := expandSlice(original)
	if expanded != want {
		t.Errorf("expandSlice(%q) = %q, want %q", original, expanded, want)
	}
}

func TestTranslateUID(t *testing.T) {
	for _, tc := range []struct {
		name   string
		uid    int
		uidMap string
		want   int
	}{
		{
			name:   "initial user namespace",
			uid:    1000,
			uidMap: "         0          0 4294967295\n",
			want:   1000,
		},
		{
			// Rootless container runtimes map the unprivileged caller to UID 0, so
			// the UID visible to runsc says nothing about its privileges.
			name:   "caller mapped to root",
			uid:    0,
			uidMap: "         0       1000          1\n         1     100000      65536\n",
			want:   1000,
		},
		{
			name:   "offset within range",
			uid:    5,
			uidMap: "         0       1000          1\n         1     100000      65536\n",
			want:   100004,
		},
		{
			name:   "unmapped",
			uid:    99999,
			uidMap: "         0       1000          1\n",
			want:   99999,
		},
		{
			name:   "malformed",
			uid:    7,
			uidMap: "this is not a uid map\n",
			want:   7,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "uid_map")
			if err := os.WriteFile(path, []byte(tc.uidMap), 0644); err != nil {
				t.Fatalf("os.WriteFile(%q): %v", path, err)
			}
			if got := translateUID(tc.uid, path); got != tc.want {
				t.Errorf("translateUID(%d, %q) = %d, want %d", tc.uid, path, got, tc.want)
			}
		})
	}
}

func TestTranslateUIDMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist")
	if got := translateUID(1000, path); got != 1000 {
		t.Errorf("translateUID(1000, %q) = %d, want 1000", path, got)
	}
}

func TestUserBusAddress(t *testing.T) {
	t.Run("from environment", func(t *testing.T) {
		const want = "unix:path=/run/user/1000/bus"
		t.Setenv("DBUS_SESSION_BUS_ADDRESS", want)
		t.Setenv("XDG_RUNTIME_DIR", "")
		got, err := userBusAddress()
		if err != nil {
			t.Fatalf("userBusAddress() failed: %v", err)
		}
		if got != want {
			t.Errorf("userBusAddress() = %q, want %q", got, want)
		}
	})

	// Container runtimes do not always propagate DBUS_SESSION_BUS_ADDRESS, so the
	// well-known path under XDG_RUNTIME_DIR must also be found.
	t.Run("from XDG_RUNTIME_DIR", func(t *testing.T) {
		dir := t.TempDir()
		busPath := filepath.Join(dir, "bus")
		if err := os.WriteFile(busPath, nil, 0644); err != nil {
			t.Fatalf("os.WriteFile(%q): %v", busPath, err)
		}
		t.Setenv("DBUS_SESSION_BUS_ADDRESS", "")
		t.Setenv("XDG_RUNTIME_DIR", dir)
		got, err := userBusAddress()
		if err != nil {
			t.Fatalf("userBusAddress() failed: %v", err)
		}
		if want := "unix:path=" + dbus.EscapeBusAddressValue(busPath); got != want {
			t.Errorf("userBusAddress() = %q, want %q", got, want)
		}
	})

	t.Run("not found", func(t *testing.T) {
		t.Setenv("DBUS_SESSION_BUS_ADDRESS", "")
		t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
		if got, err := userBusAddress(); err == nil {
			t.Errorf("userBusAddress() = %q, want error", got)
		}
	})
}

func TestMakePath(t *testing.T) {
	for _, tc := range []struct {
		name string
		cg   cgroupSystemd
		want string
	}{
		{
			name: "system instance",
			cg: cgroupSystemd{
				Name:        "123",
				Parent:      "system.slice",
				ScopePrefix: "runsc",
				cgroupV2:    cgroupV2{Mountpoint: "/sys/fs/cgroup"},
			},
			want: "/sys/fs/cgroup/system.slice/runsc-123.scope",
		},
		{
			// Units managed by a per-user systemd instance are nested under that
			// instance's own cgroup.
			name: "user instance",
			cg: cgroupSystemd{
				Name:        "123",
				Parent:      "user.slice",
				ScopePrefix: "libpod",
				Rootless:    true,
				ManagerCG:   "/user.slice/user-1000.slice/user@1000.service",
				cgroupV2:    cgroupV2{Mountpoint: "/sys/fs/cgroup"},
			},
			want: "/sys/fs/cgroup/user.slice/user-1000.slice/user@1000.service/user.slice/libpod-123.scope",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cg.MakePath(""); got != tc.want {
				t.Errorf("MakePath() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCheckControllers(t *testing.T) {
	for _, tc := range []struct {
		name        string
		controllers string
		requested   []string
		wantErr     bool
	}{
		{
			name:        "nothing requested",
			controllers: "cpu memory\n",
			requested:   nil,
		},
		{
			name:        "all available",
			controllers: "cpuset cpu io memory pids\n",
			requested:   []string{"cpu", "memory"},
		},
		{
			// systemd accepts properties for controllers it was not delegated and
			// silently drops them, so this must be caught here.
			name:        "requested but not delegated",
			controllers: "cpuset cpu memory pids\n",
			requested:   []string{"cpu", "io"},
			wantErr:     true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cg := cgroupSystemd{
				Name:                 "123",
				Parent:               "test.slice",
				ScopePrefix:          "runsc",
				RequestedControllers: tc.requested,
				cgroupV2:             cgroupV2{Mountpoint: t.TempDir()},
			}
			path := cg.MakePath("")
			if err := os.MkdirAll(path, 0755); err != nil {
				t.Fatalf("os.MkdirAll(%q): %v", path, err)
			}
			ctrlFile := filepath.Join(path, controllersFile)
			if err := os.WriteFile(ctrlFile, []byte(tc.controllers), 0644); err != nil {
				t.Fatalf("os.WriteFile(%q): %v", ctrlFile, err)
			}
			err := cg.checkControllers()
			if gotErr := err != nil; gotErr != tc.wantErr {
				t.Errorf("checkControllers() error = %v, wantErr = %t", err, tc.wantErr)
			}
		})
	}
}

func TestCheckControllersUnreadable(t *testing.T) {
	cg := cgroupSystemd{
		Name:                 "123",
		Parent:               "test.slice",
		ScopePrefix:          "runsc",
		RequestedControllers: []string{"cpu"},
		cgroupV2:             cgroupV2{Mountpoint: t.TempDir()},
	}
	if err := cg.checkControllers(); err == nil {
		t.Error("checkControllers() = nil, want error when cgroup.controllers is missing")
	}
}

func TestCheckControllersAfterSerialization(t *testing.T) {
	cg := &cgroupSystemd{
		Name:        "123",
		Parent:      "test.slice",
		ScopePrefix: "runsc",
		Rootless:    true,
		ManagerCG:   "/user.slice/user-1000.slice/user@1000.service",
		cgroupV2: cgroupV2{
			Mountpoint:  t.TempDir(),
			Controllers: mandatoryControllers,
		},
	}
	if err := cg.Install(&specs.LinuxResources{
		Memory: &specs.LinuxMemory{Limit: int64Ptr(1024)},
	}); err != nil {
		t.Fatalf("Install: %v", err)
	}
	path := cg.MakePath("")
	if err := os.MkdirAll(path, 0755); err != nil {
		t.Fatal(err)
	}
	// A restored cgroup must still reject the missing memory controller.
	if err := os.WriteFile(filepath.Join(path, controllersFile), []byte("cpu pids\n"), 0644); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(&CgroupJSON{Cgroup: cg})
	if err != nil {
		t.Fatal(err)
	}
	var restored CgroupJSON
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}
	got := restored.Cgroup.(*cgroupSystemd)
	if !got.Rootless || got.MakePath("") != path {
		t.Fatalf("restored cgroup points to wrong manager: %+v", got)
	}
	if err := got.checkControllers(); err == nil {
		t.Fatal("restored cgroup accepted an unavailable requested controller")
	}
}

func TestUpdateReplacesProperties(t *testing.T) {
	// Fail before touching a systemd unit, after translating the resources.
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path="+filepath.Join(t.TempDir(), "missing-bus"))
	cg := &cgroupSystemd{Rootless: true}
	cg.Controllers = mandatoryControllers
	for _, limit := range []int64{1024, 2048} {
		if err := cg.Update(&specs.LinuxResources{
			Memory: &specs.LinuxMemory{Limit: &limit},
		}); err == nil {
			t.Fatal("Update unexpectedly connected to missing bus")
		}
		if len(cg.properties) != 1 || cg.properties[0].Name != "MemoryMax" || cg.properties[0].Value.Value() != uint64(limit) {
			t.Fatalf("Update retained stale properties: %v", cg.properties)
		}
	}
}

func TestInstall(t *testing.T) {
	for _, tc := range []struct {
		name             string
		res              *specs.LinuxResources
		wantProps        []systemdDbus.Property
		updatedRes       *specs.LinuxResources
		wantUpdatedProps []systemdDbus.Property
		err              error
	}{
		{
			name: "defaults",
			res:  nil,
			wantProps: []systemdDbus.Property{
				{"Slice", dbus.MakeVariant("parent.slice")},
				{Name: "Description", Value: dbus.MakeVariant("Secure container 123")},
				{Name: "MemoryAccounting", Value: dbus.MakeVariant(true)},
				{Name: "CPUAccounting", Value: dbus.MakeVariant(true)},
				{Name: "TasksAccounting", Value: dbus.MakeVariant(true)},
				{Name: "IOAccounting", Value: dbus.MakeVariant(true)},
				{Name: "Delegate", Value: dbus.MakeVariant(true)},
				{Name: "DefaultDependencies", Value: dbus.MakeVariant(false)},
			},
		},
		{
			name: "memory",
			res: &specs.LinuxResources{
				Memory: &specs.LinuxMemory{
					Limit:       int64Ptr(1),
					Swap:        int64Ptr(2),
					Reservation: int64Ptr(3),
				},
			},
			wantProps: []systemdDbus.Property{
				{"MemoryMax", dbus.MakeVariant(uint64(1))},
				{"MemoryLow", dbus.MakeVariant(uint64(3))},
				{"MemorySwapMax", dbus.MakeVariant(uint64(1))},
			},
		},
		{
			name: "memory no limit",
			res: &specs.LinuxResources{
				Memory: &specs.LinuxMemory{
					Swap: int64Ptr(1),
				},
			},
			err: ErrBadResourceSpec,
		},
		{
			name: "cpu defaults",
			res: &specs.LinuxResources{
				CPU: &specs.LinuxCPU{
					Shares: uint64Ptr(0),
					Quota:  int64Ptr(5),
					Period: uint64Ptr(0),
				},
			},
			wantProps: []systemdDbus.Property{
				{"CPUQuotaPerSecUSec", dbus.MakeVariant(uint64(10000))},
			},
		},
		{
			name: "cpu",
			res: &specs.LinuxResources{
				CPU: &specs.LinuxCPU{
					Shares: uint64Ptr(1),
					Period: uint64Ptr(20000),
					Quota:  int64Ptr(300000),
					Cpus:   "4",
					Mems:   "5",
				},
			},
			wantProps: []systemdDbus.Property{
				{"CPUWeight", dbus.MakeVariant(convertCPUSharesToCgroupV2Value(1))},
				{"CPUQuotaPeriodUSec", dbus.MakeVariant(uint64(20000))},
				{"CPUQuotaPerSecUSec", dbus.MakeVariant(uint64(15000000))},
				{"AllowedCPUs", dbus.MakeVariant([]byte{1 << 4})},
				{"AllowedMemoryNodes", dbus.MakeVariant([]byte{1 << 5})},
			},
			updatedRes: &specs.LinuxResources{
				CPU: &specs.LinuxCPU{
					Shares: uint64Ptr(1),
					Period: uint64Ptr(10000),
					Quota:  int64Ptr(300000),
					Cpus:   "2",
					Mems:   "3",
				},
			},
			wantUpdatedProps: []systemdDbus.Property{
				// initial properties
				{"CPUWeight", dbus.MakeVariant(convertCPUSharesToCgroupV2Value(1))},
				{"CPUQuotaPeriodUSec", dbus.MakeVariant(uint64(20000))},
				{"CPUQuotaPerSecUSec", dbus.MakeVariant(uint64(15000000))},
				{"AllowedCPUs", dbus.MakeVariant([]byte{1 << 4})},
				{"AllowedMemoryNodes", dbus.MakeVariant([]byte{1 << 5})},
				// updated properties
				{"CPUWeight", dbus.MakeVariant(convertCPUSharesToCgroupV2Value(1))},
				{"CPUQuotaPeriodUSec", dbus.MakeVariant(uint64(10000))},
				{"CPUQuotaPerSecUSec", dbus.MakeVariant(uint64(30000000))},
				{"AllowedCPUs", dbus.MakeVariant([]byte{1 << 2})},
				{"AllowedMemoryNodes", dbus.MakeVariant([]byte{1 << 3})},
			},
		},
		{
			name: "cpuset",
			res: &specs.LinuxResources{
				CPU: &specs.LinuxCPU{
					Cpus: "1-3,5",
					Mems: "5-8",
				},
			},
			wantProps: []systemdDbus.Property{
				{"AllowedCPUs", dbus.MakeVariant([]byte{0b_101110})},
				{"AllowedMemoryNodes", dbus.MakeVariant([]byte{1, 0b_11100000})},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Rootless is set explicitly because the expected properties and the
			// dial error below describe the system instance.
			cg := cgroupSystemd{Name: "123", Parent: "parent.slice", Rootless: false}
			cg.Controllers = mandatoryControllers
			err := cg.Install(tc.res)
			if !errors.Is(err, tc.err) {
				t.Fatalf("Wrong error, got: %s, want: %s", err, tc.err)
			}
			cmper := cmp.Comparer(func(a dbus.Variant, b dbus.Variant) bool {
				return a.String() == b.String()
			})
			sorter := cmpopts.SortSlices(func(a systemdDbus.Property, b systemdDbus.Property) bool {
				return (a.Name + a.Value.String()) > (b.Name + b.Value.String())
			})
			filteredProps := filterProperties(cg.properties, tc.wantProps)
			if diff := cmp.Diff(filteredProps, tc.wantProps, cmper, sorter); diff != "" {
				t.Errorf("cgroup properties list diff %s", diff)
			}

			if tc.updatedRes != nil {
				// Check the generated properties without updating a host systemd unit.
				if err := cg.updateControllersProps(tc.updatedRes); !errors.Is(err, tc.err) {
					t.Fatalf("updateControllersProps() = %v, want %v", err, tc.err)
				}
				filteredProps = filterProperties(cg.properties, tc.wantUpdatedProps)
				if diff := cmp.Diff(filteredProps, tc.wantUpdatedProps, cmper, sorter); diff != "" {
					t.Errorf("cgroup properties list diff %s", diff)
				}
			}
		})
	}
}

// newCompatDirTestCgroup constructs a cgroupSystemd anchored at a temporary
// mountpoint with its parent slice pre-created, mirroring what kubelet+systemd
// arrange on a real host. The leaf scope directory itself is intentionally
// not created -- installCompatDir is the unit under test for that.
func newCompatDirTestCgroup(t *testing.T) (*cgroupSystemd, string) {
	t.Helper()
	mountpoint := t.TempDir()
	parentSlicePath := filepath.Join(mountpoint, "/parent.slice")
	if err := os.MkdirAll(parentSlicePath, 0o755); err != nil {
		t.Fatalf("mkdir parent slice: %v", err)
	}
	cg := &cgroupSystemd{
		Name:        "abc",
		ScopePrefix: "cri-containerd",
		Parent:      "parent.slice",
		cgroupV2: cgroupV2{
			Mountpoint: mountpoint,
			// Path is set by newCgroupV2Systemd to expanded slice + unitName.
			Path: filepath.Join(expandSlice("parent.slice"), "cri-containerd-abc.scope"),
		},
	}
	return cg, cg.MakePath("")
}

// seedCompatLeafFiles touches the v2 controller interface files in the leaf
// scope directory that the kernel would auto-create on a real cgroupfs mount
// when the corresponding controllers are enabled in the parent's
// cgroup.subtree_control. Without these, setValue (which uses O_WRONLY|
// O_TRUNC and does not create) returns ENOENT, which is the
// controller-not-enabled path.
func seedCompatLeafFiles(t *testing.T, leafDir string, names ...string) {
	t.Helper()
	if err := os.MkdirAll(leafDir, 0o755); err != nil {
		t.Fatalf("mkdir leaf dir: %v", err)
	}
	for _, name := range names {
		f, err := os.Create(filepath.Join(leafDir, name))
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		f.Close()
	}
}

// readCgroupFile reads a single cgroup interface file under leafDir and
// returns its contents.
func readCgroupFile(t *testing.T, leafDir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(leafDir, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

// TestInstallCompatDir verifies that installCompatDir creates the cgroup
// directory at the resolved scope path under the parent slice (so cAdvisor
// can discover it via inotify), and that the embedded cgroupV2.Uninstall
// removes it via the c.Own bookkeeping. This is the cgroup v2 + systemd
// counterpart of #6500 / PR #6657, which created cAdvisor compat
// directories on cgroup v1.
func TestInstallCompatDir(t *testing.T) {
	cg, wantDir := newCompatDirTestCgroup(t)
	if got, want := wantDir, filepath.Join(cg.Mountpoint, "/parent.slice", "cri-containerd-abc.scope"); got != want {
		t.Fatalf("MakePath() = %q, want %q", got, want)
	}
	if _, err := os.Stat(wantDir); !os.IsNotExist(err) {
		t.Fatalf("compat dir already exists or unexpected error before install: %v", err)
	}

	if err := cg.installCompatDir(nil); err != nil {
		t.Fatalf("installCompatDir(nil) error: %v", err)
	}
	info, err := os.Stat(wantDir)
	if err != nil {
		t.Fatalf("compat dir not created: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("compat dir %q is not a directory", wantDir)
	}
	// The path must be tracked in Own so Uninstall can clean it up.
	if got := len(cg.Own); got != 1 || cg.Own[0] != wantDir {
		t.Fatalf("c.Own = %v, want exactly [%q]", cg.Own, wantDir)
	}

	// Idempotency: calling again on an existing dir should not error and
	// should not double-track the path (avoid leaking entries on retries).
	if err := cg.installCompatDir(nil); err != nil {
		t.Fatalf("installCompatDir(nil) second call error: %v", err)
	}
	if got := len(cg.Own); got != 1 {
		t.Fatalf("len(c.Own) after second installCompatDir = %d, want 1", got)
	}

	// Uninstall must remove the dir we created.
	if err := cg.Uninstall(); err != nil {
		t.Fatalf("Uninstall() error: %v", err)
	}
	if _, err := os.Stat(wantDir); !os.IsNotExist(err) {
		t.Fatalf("compat dir still exists after Uninstall: %v", err)
	}
}

// TestInstallCompatDirSpecFiles verifies that installCompatDir populates the
// cgroup interface files cAdvisor reads as container_spec_* (memory.max,
// cpu.max, cpu.weight, pids.max, memory.swap.max) when handed a non-nil
// LinuxResources. The leaf interface files are pre-touched to simulate what
// the kernel auto-creates when the corresponding controllers are enabled in
// the parent slice's cgroup.subtree_control on a real cgroupfs mount.
func TestInstallCompatDirSpecFiles(t *testing.T) {
	cg, leafDir := newCompatDirTestCgroup(t)
	seedCompatLeafFiles(t, leafDir,
		"memory.max", "memory.swap.max", "memory.low",
		"cpu.max", "cpu.weight",
		"pids.max",
	)

	memLimit := int64(536870912) // 512 MiB
	memSwap := int64(1073741824) // 1 GiB combined memory+swap (runc-style)
	cpuQuota := int64(50000)
	cpuPeriod := uint64(100000)
	cpuShares := uint64(2048)
	pidsLimit := int64(100)
	res := &specs.LinuxResources{
		Memory: &specs.LinuxMemory{
			Limit: &memLimit,
			Swap:  &memSwap,
		},
		CPU: &specs.LinuxCPU{
			Quota:  &cpuQuota,
			Period: &cpuPeriod,
			Shares: &cpuShares,
		},
		Pids: &specs.LinuxPids{
			Limit: pidsLimit,
		},
	}
	if err := cg.installCompatDir(res); err != nil {
		t.Fatalf("installCompatDir(res) error: %v", err)
	}

	if got, want := readCgroupFile(t, leafDir, "memory.max"), "536870912"; got != want {
		t.Errorf("memory.max = %q, want %q", got, want)
	}
	// Swap in v2 is the swap-only value (memorySwap - memory).
	if got, want := readCgroupFile(t, leafDir, "memory.swap.max"), "536870912"; got != want {
		t.Errorf("memory.swap.max = %q, want %q", got, want)
	}
	if got, want := readCgroupFile(t, leafDir, "cpu.max"), "50000 100000"; got != want {
		t.Errorf("cpu.max = %q, want %q", got, want)
	}
	// cpu.shares=2048 maps to cpu.weight via the runc-compatible formula.
	wantWeight := strconv.FormatUint(convertCPUSharesToCgroupV2Value(cpuShares), 10)
	if got := readCgroupFile(t, leafDir, "cpu.weight"); got != wantWeight {
		t.Errorf("cpu.weight = %q, want %q", got, wantWeight)
	}
	if got, want := readCgroupFile(t, leafDir, "pids.max"), "100"; got != want {
		t.Errorf("pids.max = %q, want %q", got, want)
	}
}

// TestInstallCompatDirBestEffort verifies installCompatDir is best-effort:
// when controller interface files are absent (controller not enabled in the
// parent slice's cgroup.subtree_control on a real host), the missing-file
// errors from setValue are swallowed, the directory is still created, and
// installCompatDir returns success. This preserves the #6657 invariant that
// the compat path must never block container start.
func TestInstallCompatDirBestEffort(t *testing.T) {
	cg, leafDir := newCompatDirTestCgroup(t)

	memLimit := int64(536870912)
	res := &specs.LinuxResources{
		Memory: &specs.LinuxMemory{Limit: &memLimit},
	}

	// Deliberately do NOT seed any leaf interface files. set() will hit
	// ENOENT for every controller it tries to write; installCompatDir must
	// swallow that and still report success.
	if err := cg.installCompatDir(res); err != nil {
		t.Fatalf("installCompatDir(res) with no leaf files: got error %v, want nil (best-effort)", err)
	}
	if _, err := os.Stat(leafDir); err != nil {
		t.Fatalf("compat dir not created: %v", err)
	}
	if got := len(cg.Own); got != 1 || cg.Own[0] != leafDir {
		t.Fatalf("c.Own = %v, want exactly [%q]", cg.Own, leafDir)
	}
}

// TestInstallSubcontainerCompatDirSystemd verifies the public dispatcher
// routes systemd cgroups to installCompatDir (not the dbus Install path) and
// that resources reach spec-file population on the leaf. Uninstall is covered
// by TestInstallCompatDir; it isn't exercised here because the spec files
// written into the tmpdir leaf would block rmdir (a real cgroupfs removes
// them atomically).
func TestInstallSubcontainerCompatDirSystemd(t *testing.T) {
	cg, leafDir := newCompatDirTestCgroup(t)
	// Pre-create only memory.max so we can assert end-to-end that resources
	// passed via the dispatcher reach the per-controller set() methods.
	seedCompatLeafFiles(t, leafDir, "memory.max")

	memLimit := int64(123456789)
	res := &specs.LinuxResources{
		Memory: &specs.LinuxMemory{Limit: &memLimit},
	}
	if err := InstallSubcontainerCompatDir(cg, res); err != nil {
		t.Fatalf("InstallSubcontainerCompatDir(systemd, res) error: %v", err)
	}
	if _, err := os.Stat(leafDir); err != nil {
		t.Fatalf("compat dir not created via InstallSubcontainerCompatDir: %v", err)
	}
	if got, want := readCgroupFile(t, leafDir, "memory.max"), "123456789"; got != want {
		t.Errorf("memory.max = %q, want %q", got, want)
	}
}

// filterProperties filters the list of properties in got to ones with
// the names of properties specified in want.
func filterProperties(got []systemdDbus.Property, want []systemdDbus.Property) []systemdDbus.Property {
	if want == nil {
		return nil
	}
	filterMap := map[string]any{}
	for _, prop := range want {
		filterMap[prop.Name] = nil
	}
	filtered := []systemdDbus.Property{}
	for _, prop := range got {
		if _, ok := filterMap[prop.Name]; ok {
			filtered = append(filtered, prop)
		}
	}
	return filtered
}
