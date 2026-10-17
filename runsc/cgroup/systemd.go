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
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	systemdDbus "github.com/coreos/go-systemd/v22/dbus"
	dbus "github.com/godbus/dbus/v5"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"

	"gvisor.dev/gvisor/pkg/cleanup"
	"gvisor.dev/gvisor/pkg/log"
)

// compatDirControllers are the v2 controllers populated on the compat cgroup
// directory created by installCompatDir for cAdvisor's container_spec_*
// series. Limited to controllers whose interface files cAdvisor reads as
// spec values (memory.max, cpu.max / cpu.weight, pids.max). Other controllers
// (cpuset, io, hugetlb) are intentionally excluded: they don't surface as
// container_spec_* and writing them widens the failure surface on hosts where
// they aren't enabled in the parent slice's cgroup.subtree_control.
var compatDirControllers = []string{"cpu", "memory", "pids"}

var (
	// ErrBadResourceSpec indicates that a cgroupSystemd function was
	// passed a specs.LinuxResources object that is impossible or illegal
	// to process.
	ErrBadResourceSpec = errors.New("misconfigured resource spec")
	// ErrInvalidSlice indicates that the slice name passed via cgroup.Path is
	// invalid.
	ErrInvalidSlice = errors.New("invalid slice name")
)

// cgroupSystemd represents a cgroupv2 managed by systemd.
type cgroupSystemd struct {
	cgroupV2
	// Name is the name of the of the systemd scope that controls the cgroups.
	Name string
	// Parent is the encapsulating slice.
	Parent string
	// ScopePrefix is the prefix for the scope name.
	ScopePrefix string
	// Rootless indicates that the unit is managed by the per-user systemd
	// instance rather than the system instance.
	Rootless bool
	// ManagerCG is the cgroup of the systemd instance that manages this unit.
	// Unit cgroups are nested under it. It is empty for the system instance,
	// which manages units at the root of the hierarchy.
	ManagerCG string
	// RequestedControllers must survive serialization so that processes joining
	// an existing scope also verify the controllers its limits depend on.
	RequestedControllers []string `json:"requestedControllers,omitempty"`

	// initialResources is retained from Install until a successful Join.
	// It is not serialized; Update receives the current resources explicitly.
	initialResources *specs.LinuxResources

	properties []systemdDbus.Property
	dbusConn   *systemdDbus.Conn
}

// checkControllers returns an error if a limit was requested for a controller
// that is not available in the cgroup. systemd accepts properties for
// controllers it was not delegated and silently drops them, so looking
// afterwards is the only way to know whether a limit took effect.
//
// Preconditions: The cgroup must have been created.
func (c *cgroupSystemd) checkControllers() error {
	if len(c.RequestedControllers) == 0 {
		return nil
	}
	path := c.MakePath("")
	data, err := os.ReadFile(filepath.Join(path, controllersFile))
	if err != nil {
		return fmt.Errorf("reading cgroup controllers for %q: %w", path, err)
	}
	available := make(map[string]struct{})
	for _, name := range strings.Fields(string(data)) {
		available[name] = struct{}{}
	}
	var missing []string
	for _, name := range c.RequestedControllers {
		if _, ok := available[name]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("resource limits were requested for cgroup controllers %s, but they are not available in %q; they may not be delegated to this systemd instance", strings.Join(missing, ","), path)
	}
	return nil
}

// translateUID translates uid through the user namespace ID map in mapPath.
// See user_namespaces(7) for the format.
func translateUID(uid int, mapPath string) int {
	data, err := os.ReadFile(mapPath)
	if err != nil {
		log.Debugf("Unable to read %q, using UID %d as-is: %v", mapPath, uid, err)
		return uid
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 {
			continue
		}
		inside, err1 := strconv.ParseUint(fields[0], 10, 32)
		outside, err2 := strconv.ParseUint(fields[1], 10, 32)
		length, err3 := strconv.ParseUint(fields[2], 10, 32)
		if err1 != nil || err2 != nil || err3 != nil {
			continue
		}
		if u := uint64(uid); u >= inside && u < inside+length {
			return int(outside + (u - inside))
		}
	}
	log.Debugf("UID %d is not mapped in %q, using it as-is", uid, mapPath)
	return uid
}

// hostUID returns the caller's effective UID as seen outside of any user
// namespace it may be running in.
func hostUID() int {
	return translateUID(os.Geteuid(), "/proc/self/uid_map")
}

// useUserSystemd reports whether cgroup operations should be directed at the
// per-user systemd instance rather than the system instance. Unprivileged users
// cannot create system-wide transient units, but they can create per-user ones.
//
// Note that geteuid() alone is not sufficient: container runtimes may run runsc
// inside a user namespace in which the unprivileged caller is mapped to UID 0.
func useUserSystemd() bool {
	return hostUID() != 0
}

// userBusAddress returns the address of the caller's session bus, which is
// where the per-user systemd instance can be reached.
func userBusAddress() (string, error) {
	if addr := os.Getenv("DBUS_SESSION_BUS_ADDRESS"); addr != "" {
		return addr, nil
	}
	// Container runtimes do not always propagate DBUS_SESSION_BUS_ADDRESS. The
	// session bus lives at a well-known path under XDG_RUNTIME_DIR, so fall back
	// to that when the socket is present.
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		busPath := filepath.Join(dir, "bus")
		if _, err := os.Stat(busPath); err == nil {
			return "unix:path=" + dbus.EscapeBusAddressValue(busPath), nil
		}
	}
	return "", errors.New("no session bus: DBUS_SESSION_BUS_ADDRESS unset and $XDG_RUNTIME_DIR/bus missing")
}

// newDBusConn connects to the systemd instance that manages this cgroup.
func (c *cgroupSystemd) newDBusConn(ctx context.Context) (*systemdDbus.Conn, error) {
	if !c.Rootless {
		return systemdDbus.NewWithContext(ctx)
	}
	addr, err := userBusAddress()
	if err != nil {
		return nil, err
	}
	// systemdDbus.NewUserConnectionContext is not usable here: it authenticates
	// as geteuid(), which is 0 inside the user namespace a rootless runtime puts
	// us in, and it falls back to spawning dbus-launch(1) when it cannot find an
	// existing bus.
	//
	// Note that systemdDbus.NewConnection calls this function more than once and
	// requires each call to return an independent connection.
	uid := hostUID()
	return systemdDbus.NewConnection(func() (*dbus.Conn, error) {
		conn, err := dbus.Dial(addr, dbus.WithContext(ctx))
		if err != nil {
			return nil, fmt.Errorf("dialing %q: %w", addr, err)
		}
		// The bus resolves peer credentials in its own user namespace, so it must
		// be given the UID from that namespace rather than the one we see.
		if err := conn.Auth([]dbus.Auth{dbus.AuthExternal(strconv.Itoa(uid))}); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("authenticating to %q as UID %d: %w", addr, uid, err)
		}
		if err := conn.Hello(); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("sending Hello to %q: %w", addr, err)
		}
		return conn, nil
	})
}

// managerControlGroup returns the cgroup of the systemd instance that conn is
// connected to. Units it manages are nested under it.
func managerControlGroup(conn *systemdDbus.Conn) (string, error) {
	// GetManagerProperty returns the property's dbus.Variant rendering, which
	// quotes strings.
	quoted, err := conn.GetManagerProperty("ControlGroup")
	if err != nil {
		return "", fmt.Errorf("getting systemd ControlGroup property: %w", err)
	}
	cg, err := strconv.Unquote(quoted)
	if err != nil {
		return "", fmt.Errorf("parsing systemd ControlGroup property %q: %w", quoted, err)
	}
	return cg, nil
}

func newCgroupV2Systemd(cgv2 *cgroupV2) (*cgroupSystemd, error) {
	if !isRunningSystemd() {
		return nil, fmt.Errorf("systemd not running on host")
	}
	ctx := context.Background()
	cg := &cgroupSystemd{cgroupV2: *cgv2}
	// Parse the path from expected "slice:prefix:name"
	// for e.g. "system.slice:docker:1234"
	parts := strings.Split(cg.Path, ":")
	if len(parts) != 3 {
		return nil, fmt.Errorf("expected cgroupsPath to be of format \"slice:prefix:name\" for systemd cgroups, got %q instead", cg.Path)
	}
	cg.Parent = parts[0]
	cg.ScopePrefix = parts[1]
	cg.Name = parts[2]
	if err := validSlice(cg.Parent); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidGroupPath, err)
	}
	cg.Rootless = useUserSystemd()
	conn, err := cg.newDBusConn(ctx)
	if err != nil {
		return nil, err
	}
	var version int
	if version, err = systemdVersion(conn); err != nil {
		return nil, fmt.Errorf("error parsing systemd version: %v", err)
	}
	if version < 244 {
		return nil, fmt.Errorf("systemd version %d not supported, please upgrade to at least 244", version)
	}
	if cg.Rootless {
		if cg.ManagerCG, err = managerControlGroup(conn); err != nil {
			return nil, err
		}
	}
	// Rewrite Path so that it is compatible with cgroupv2 methods.
	cg.Path = filepath.Join(cg.ManagerCG, expandSlice(cg.Parent), cg.unitName())
	cg.dbusConn = conn
	return cg, err
}

// installCompatDir creates the compat cgroup directory under the parent slice
// (tracked in c.Own so cgroupV2.Uninstall removes it at container destroy)
// and, when res is non-nil, best-effort populates the spec-limit files
// cAdvisor reads as container_spec_*. Used in place of Install for systemd v2,
// where Install only stages dbus properties and the directory is otherwise
// created by Join() via StartTransientUnit -- inappropriate for a
// process-less compat cgroup.
func (c *cgroupSystemd) installCompatDir(res *specs.LinuxResources) error {
	path := c.MakePath("")
	if err := os.MkdirAll(path, 0o755); err != nil {
		return fmt.Errorf("creating compat cgroup dir %q: %w", path, err)
	}
	alreadyTracked := false
	for _, owned := range c.Own {
		if owned == path {
			alreadyTracked = true
			break
		}
	}
	if !alreadyTracked {
		c.Own = append(c.Own, path)
	}

	// Best-effort spec-file population. Controllers that aren't enabled in
	// the parent slice's cgroup.subtree_control don't have leaf interface
	// files; setValue then returns ENOENT, which we swallow. The compat dir
	// must always succeed at directory level (#6657 precedent: the compat
	// path must never block container start).
	if res == nil {
		return nil
	}
	for _, name := range compatDirControllers {
		ctrlr, ok := controllers2[name]
		if !ok {
			continue
		}
		if err := ctrlr.set(res, path); err != nil {
			if isCompatDirIgnorableErr(err) {
				log.Debugf("Skipping %q spec-file population for compat cgroup %q: %v", name, path, err)
				continue
			}
			return fmt.Errorf("populating %q spec files for compat cgroup %q: %w", name, path, err)
		}
	}
	return nil
}

// isCompatDirIgnorableErr reports whether err from a best-effort spec-file
// write is safe to swallow: the interface file may not exist (controller not
// in subtree_control), the mount may be read-only, or we may lack permission.
// None of these should block container start.
func isCompatDirIgnorableErr(err error) bool {
	return errors.Is(err, os.ErrNotExist) ||
		errors.Is(err, os.ErrPermission) ||
		errors.Is(err, unix.EROFS)
}

// Install configures the properties for a scope unit but does not start the
// unit.
func (c *cgroupSystemd) Install(res *specs.LinuxResources) error {
	log.Debugf("Installing systemd cgroup resource controller under %v", c.Parent)
	// The slice is named relative to the systemd instance that manages it, so it
	// must not be prefixed with ManagerCG even though the cgroup path is.
	c.properties = append(c.properties, systemdDbus.PropSlice(c.Parent))
	c.properties = append(c.properties, systemdDbus.PropDescription("Secure container "+c.Name))
	pid := os.Getpid()
	c.properties = append(c.properties, systemdDbus.PropPids(uint32(pid)))
	// We always want proper accounting for the container for reporting resource
	// usage.
	c.addProp("MemoryAccounting", true)
	c.addProp("CPUAccounting", true)
	c.addProp("TasksAccounting", true)
	c.addProp("IOAccounting", true)
	// Delegate must be true so that the container can manage its own cgroups.
	c.addProp("Delegate", true)
	// For compatibility with runc.
	c.addProp("DefaultDependencies", false)

	if err := c.updateControllersProps(res); err != nil {
		return err
	}
	c.initialResources = res
	return nil
}

// Update updates the cgroup resources of an existing systemd unit.
func (c *cgroupSystemd) Update(res *specs.LinuxResources) error {
	log.Debugf("Updating systemd cgroup resource controller under %v", c.Parent)
	// An update sends only its own properties, including when retrying after a
	// failed update. Do not replay stale values from Install or an earlier call.
	c.properties = nil
	if err := c.updateControllersProps(res); err != nil {
		return err
	}

	ctx := context.Background()
	conn, err := c.newDBusConn(ctx)
	if err != nil {
		return err
	}
	c.dbusConn = conn
	// Our systemd units are transient, hence runtime=true.
	if err := c.dbusConn.SetUnitPropertiesContext(ctx, c.unitName(), true /* runtime */, c.properties...); err != nil {
		return fmt.Errorf("error setting systemd unit properties: %v", err)
	}
	if err := c.checkControllers(); err != nil {
		return err
	}
	return (&io2{}).set(res, c.MakePath(""))
}

func (c *cgroupSystemd) unitName() string {
	return fmt.Sprintf("%s-%s.scope", c.ScopePrefix, c.Name)
}

// MakePath builds a path to the given controller.
func (c *cgroupSystemd) MakePath(string) string {
	fullSlicePath := expandSlice(c.Parent)
	path := filepath.Join(c.Mountpoint, c.ManagerCG, fullSlicePath, c.unitName())
	return path
}

// CloneIntoCgroup implements Cgroup.CloneIntoCgroup.
func (c *cgroupSystemd) CloneIntoCgroup() (*os.File, error) {
	// Can't use CLONE_INTO_CGROUP here because systemd is the thing doing the
	// process migration, not us.
	return nil, fmt.Errorf("CLONE_INTO_CGROUP cannot be used with systemd cgroup %q", c.unitName())
}

// Join implements Cgroup.Join.
func (c *cgroupSystemd) Join() (func(), error) {
	log.Debugf("Joining systemd cgroup %v", c.unitName())
	timeout := 30 * time.Second
	ctx := context.Background()
	// Clean up partially created cgroups on error. Errors during cleanup itself
	// are ignored.
	clean := cleanup.Make(func() { _ = c.Uninstall() })
	defer clean.Clean()

	conn, err := c.newDBusConn(ctx)
	if err != nil {
		return nil, err
	}
	c.dbusConn = conn
	unitName := c.unitName()
	statusChan := make(chan string)
	timedCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if _, err := c.dbusConn.StartTransientUnitContext(timedCtx, unitName, "replace", c.properties, statusChan); err == nil {
		s := <-statusChan
		close(statusChan)
		switch s {
		case "done":
		// All cases that are not "done" according to the dbus package.
		case "cancelled", "timeout", "failed", "dependency", "skipped":
			c.dbusConn.ResetFailedUnitContext(ctx, unitName)
			return nil, fmt.Errorf("error creating systemd unit `%s`: got %s", unitName, s)
		default:
			c.dbusConn.ResetFailedUnitContext(ctx, unitName)
			return nil, fmt.Errorf("unknown job completion status %q", s)
		}
	} else if unitAlreadyExists(err) {
		if err := c.checkControllers(); err != nil {
			return nil, err
		}
		if err := c.dbusConn.AttachProcessesToUnit(timedCtx, unitName, "" /* subcgroup */, []uint32{uint32(os.Getpid())}); err != nil {
			return nil, fmt.Errorf("error joining systemd unit `%s`: %w", unitName, err)
		}
		c.initialResources = nil
		return clean.Release(), nil
	} else {
		return nil, fmt.Errorf("systemd error: %w", err)
	}
	// systemd owns the scope and its ancestor controllers. Enabling controllers
	// here can undo DisableControllers after systemd has already skipped the
	// requested properties, exposing controller files with unconstrained limits.
	// The scope is a transient unit, so systemd releases it once this process
	// exits; returning an error here does not leak it.
	if err := c.checkControllers(); err != nil {
		return nil, err
	}
	if err := (&io2{}).set(c.initialResources, c.MakePath("")); err != nil {
		return nil, err
	}
	c.initialResources = nil
	return clean.Release(), nil
}

// unitAlreadyExists returns true if the error is that a systemd unit already
// exists.
func unitAlreadyExists(err error) bool {
	if err != nil {
		var derr dbus.Error
		if errors.As(err, &derr) {
			return strings.Contains(derr.Name, "org.freedesktop.systemd1.UnitExists")
		}
	}
	return false
}

// systemd represents slice hierarchy using `-`, so we need to follow suit when
// generating the path of slice. Essentially, test-a-b.slice becomes
// /test.slice/test-a.slice/test-a-b.slice.
func expandSlice(slice string) string {
	var path, prefix string
	suffix := ".slice"
	sliceName := strings.TrimSuffix(slice, suffix)
	// If input was -.slice, we should just return root now.
	if sliceName == "-" {
		return "/"
	}
	for _, component := range strings.Split(sliceName, "-") {
		// Append the component to the path and to the prefix.
		path += "/" + prefix + component + suffix
		prefix += component + "-"
	}
	return path
}

func validSlice(slice string) error {
	suffix := ".slice"
	// Name has to end with ".slice", but can't be just ".slice".
	if slice == suffix || !strings.HasSuffix(slice, suffix) {
		return fmt.Errorf("%w: %s", ErrInvalidSlice, slice)
	}

	// Path-separators are not allowed.
	if strings.Contains(slice, "/") {
		return fmt.Errorf("%w: %s", ErrInvalidSlice, slice)
	}

	sliceName := strings.TrimSuffix(slice, suffix)
	// If input was -.slice, we should just return root now.
	if sliceName == "-" {
		return nil
	}
	for _, component := range strings.Split(sliceName, "-") {
		// test--a.slice isn't permitted, nor is -test.slice.
		if component == "" {
			return fmt.Errorf("%w: %s", ErrInvalidSlice, slice)
		}
	}
	return nil
}

var systemdCheck struct {
	once  sync.Once
	cache bool
}

func isRunningSystemd() bool {
	systemdCheck.once.Do(func() {
		fi, err := os.Lstat("/run/systemd/system")
		systemdCheck.cache = err == nil && fi.IsDir()
	})
	return systemdCheck.cache
}

func systemdVersion(conn *systemdDbus.Conn) (int, error) {
	vStr, err := conn.GetManagerProperty("Version")
	if err != nil {
		return -1, errors.New("unable to get systemd version")
	}
	// vStr should be of the form:
	// "v245.4-1.fc32", "245", "v245-1.fc32", "245-1.fc32" (without quotes).
	// The result for all of the above should be 245.
	// Thus, we unconditionally remove the "v" prefix
	// and then match on the first integer we can grab.
	re := regexp.MustCompile(`v?([0-9]+)`)
	matches := re.FindStringSubmatch(vStr)
	if len(matches) < 2 {
		return -1, fmt.Errorf("can't parse version %q: incorrect number of matches %d", vStr, len(matches))
	}
	version, err := strconv.Atoi(matches[1])
	if err != nil {
		return -1, fmt.Errorf("%w: can't parse version %q", err, vStr)
	}
	return version, nil
}

func (c *cgroupSystemd) addProp(name string, value any) {
	if value == nil {
		return
	}
	c.properties = append(c.properties, newProp(name, value))
}

func newProp(name string, units any) systemdDbus.Property {
	return systemdDbus.Property{
		Name:  name,
		Value: dbus.MakeVariant(units),
	}
}

func (c *cgroupSystemd) updateControllersProps(res *specs.LinuxResources) error {
	// Update() may be called more than once.
	c.RequestedControllers = nil
	for controllerName, ctrlr := range controllers2 {
		// First check if our controller is found in the system.
		found := false
		for _, knownController := range c.Controllers {
			if controllerName == knownController {
				found = true
				break
			}
		}
		if found {
			props, err := ctrlr.generateProperties(res)
			if err != nil {
				return err
			}
			if len(props) > 0 {
				c.RequestedControllers = append(c.RequestedControllers, controllerName)
			}
			c.properties = append(c.properties, props...)
			continue
		}
		if ctrlr.optional() {
			if err := ctrlr.skip(res); err != nil {
				return err
			}
		} else {
			return fmt.Errorf("mandatory cgroup controller %q is missing for %q", controllerName, c.Path)
		}
	}
	return nil
}

// CreateMockSystemdCgroup returns a mock Cgroup configured for systemd. This
// is useful for testing.
func CreateMockSystemdCgroup() Cgroup {
	return &cgroupSystemd{
		Name:        "test",
		ScopePrefix: "runsc",
		Parent:      "system.slice",
		cgroupV2: cgroupV2{
			Mountpoint: "/sys/fs/cgroup",
			Path:       "/a/random/path",
		},
	}
}
