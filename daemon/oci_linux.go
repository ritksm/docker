package daemon

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/Sirupsen/logrus"
	"github.com/docker/docker/container"
	"github.com/docker/docker/daemon/caps"
	"github.com/docker/docker/libcontainerd"
	"github.com/docker/docker/oci"
	"github.com/docker/docker/pkg/idtools"
	"github.com/docker/docker/pkg/mount"
	"github.com/docker/docker/pkg/stringutils"
	"github.com/docker/docker/pkg/symlink"
	"github.com/docker/docker/volume"
	containertypes "github.com/docker/engine-api/types/container"
	"github.com/opencontainers/runc/libcontainer/apparmor"
	"github.com/opencontainers/runc/libcontainer/devices"
	"github.com/opencontainers/runc/libcontainer/user"
	"github.com/opencontainers/specs/specs-go"
)

func setResources(s *specs.Spec, r containertypes.Resources) error {
	weightDevices, err := getBlkioWeightDevices(r)
	if err != nil {
		return err
	}
	readBpsDevice, err := getBlkioReadBpsDevices(r)
	if err != nil {
		return err
	}
	writeBpsDevice, err := getBlkioWriteBpsDevices(r)
	if err != nil {
		return err
	}
	readIOpsDevice, err := getBlkioReadIOpsDevices(r)
	if err != nil {
		return err
	}
	writeIOpsDevice, err := getBlkioWriteIOpsDevices(r)
	if err != nil {
		return err
	}

	memoryRes := getMemoryResources(r)
	cpuRes := getCPUResources(r)
	blkioWeight := r.BlkioWeight

	specResources := &specs.Resources{
		Memory: memoryRes,
		CPU:    cpuRes,
		BlockIO: &specs.BlockIO{
			Weight:                  &blkioWeight,
			WeightDevice:            weightDevices,
			ThrottleReadBpsDevice:   readBpsDevice,
			ThrottleWriteBpsDevice:  writeBpsDevice,
			ThrottleReadIOPSDevice:  readIOpsDevice,
			ThrottleWriteIOPSDevice: writeIOpsDevice,
		},
		DisableOOMKiller: r.OomKillDisable,
		Pids: &specs.Pids{
			Limit: &r.PidsLimit,
		},
	}

	if s.Linux.Resources != nil && len(s.Linux.Resources.Devices) > 0 {
		specResources.Devices = s.Linux.Resources.Devices
	}

	s.Linux.Resources = specResources
	return nil
}

func setDevices(s *specs.Spec, c *container.Container) error {
	// Build lists of devices allowed and created within the container.
	var devs []specs.Device
	devPermissions := s.Linux.Resources.Devices
	if c.HostConfig.Privileged {
		hostDevices, err := devices.HostDevices()
		if err != nil {
			return err
		}
		for _, d := range hostDevices {
			devs = append(devs, specDevice(d))
		}
		rwm := "rwm"
		devPermissions = []specs.DeviceCgroup{
			{
				Allow:  true,
				Access: &rwm,
			},
		}
	} else {
		for _, deviceMapping := range c.HostConfig.Devices {
			d, dPermissions, err := getDevicesFromPath(deviceMapping)
			if err != nil {
				return err
			}
			devs = append(devs, d...)
			devPermissions = append(devPermissions, dPermissions...)
		}
	}

	s.Linux.Devices = append(s.Linux.Devices, devs...)
	s.Linux.Resources.Devices = devPermissions
	return nil
}

func setRlimits(daemon *Daemon, s *specs.Spec, c *container.Container) error {
	var rlimits []specs.Rlimit

	ulimits := c.HostConfig.Ulimits
	// Merge ulimits with daemon defaults
	ulIdx := make(map[string]struct{})
	for _, ul := range ulimits {
		ulIdx[ul.Name] = struct{}{}
	}
	for name, ul := range daemon.configStore.Ulimits {
		if _, exists := ulIdx[name]; !exists {
			ulimits = append(ulimits, ul)
		}
	}

	for _, ul := range ulimits {
		rlimits = append(rlimits, specs.Rlimit{
			Type: "RLIMIT_" + strings.ToUpper(ul.Name),
			Soft: uint64(ul.Soft),
			Hard: uint64(ul.Hard),
		})
	}

	s.Process.Rlimits = rlimits
	return nil
}

func setUser(s *specs.Spec, c *container.Container) error {
	uid, gid, additionalGids, err := getUser(c, c.Config.User)
	if err != nil {
		return err
	}
	s.Process.User.UID = uid
	s.Process.User.GID = gid
	s.Process.User.AdditionalGids = additionalGids
	return nil
}

func readUserFile(c *container.Container, p string) (io.ReadCloser, error) {
	fp, err := symlink.FollowSymlinkInScope(filepath.Join(c.BaseFS, p), c.BaseFS)
	if err != nil {
		return nil, err
	}
	return os.Open(fp)
}

func getUser(c *container.Container, username string) (uint32, uint32, []uint32, error) {
	passwdPath, err := user.GetPasswdPath()
	if err != nil {
		return 0, 0, nil, err
	}
	groupPath, err := user.GetGroupPath()
	if err != nil {
		return 0, 0, nil, err
	}
	passwdFile, err := readUserFile(c, passwdPath)
	if err == nil {
		defer passwdFile.Close()
	}
	groupFile, err := readUserFile(c, groupPath)
	if err == nil {
		defer groupFile.Close()
	}

	execUser, err := user.GetExecUser(username, nil, passwdFile, groupFile)
	if err != nil {
		return 0, 0, nil, err
	}

	// todo: fix this double read by a change to libcontainer/user pkg
	groupFile, err = readUserFile(c, groupPath)
	if err == nil {
		defer groupFile.Close()
	}
	var addGroups []int
	if len(c.HostConfig.GroupAdd) > 0 {
		addGroups, err = user.GetAdditionalGroups(c.HostConfig.GroupAdd, groupFile)
		if err != nil {
			return 0, 0, nil, err
		}
	}
	uid := uint32(execUser.Uid)
	gid := uint32(execUser.Gid)
	sgids := append(execUser.Sgids, addGroups...)
	var additionalGids []uint32
	for _, g := range sgids {
		additionalGids = append(additionalGids, uint32(g))
	}
	return uid, gid, additionalGids, nil
}

func setNamespace(s *specs.Spec, ns specs.Namespace) {
	for i, n := range s.Linux.Namespaces {
		if n.Type == ns.Type {
			s.Linux.Namespaces[i] = ns
			return
		}
	}
	s.Linux.Namespaces = append(s.Linux.Namespaces, ns)
}

func setCapabilities(s *specs.Spec, c *container.Container) error {
	var caplist []string
	var err error
	if c.HostConfig.Privileged {
		caplist = caps.GetAllCapabilities()
	} else {
		caplist, err = caps.TweakCapabilities(s.Process.Capabilities, c.HostConfig.CapAdd, c.HostConfig.CapDrop)
		if err != nil {
			return err
		}
	}
	s.Process.Capabilities = caplist
	return nil
}

func delNamespace(s *specs.Spec, nsType specs.NamespaceType) {
	idx := -1
	for i, n := range s.Linux.Namespaces {
		if n.Type == nsType {
			idx = i
		}
	}
	if idx >= 0 {
		s.Linux.Namespaces = append(s.Linux.Namespaces[:idx], s.Linux.Namespaces[idx+1:]...)
	}
}

func setNamespaces(daemon *Daemon, s *specs.Spec, c *container.Container) error {
	userNS := false
	// user
	if c.HostConfig.UsernsMode.IsPrivate() {
		uidMap, gidMap := daemon.GetUIDGIDMaps()
		if uidMap != nil {
			userNS = true
			ns := specs.Namespace{Type: "user"}
			setNamespace(s, ns)
			s.Linux.UIDMappings = specMapping(uidMap)
			s.Linux.GIDMappings = specMapping(gidMap)
		}
	}
	// network
	if !c.Config.NetworkDisabled {
		ns := specs.Namespace{Type: "network"}
		parts := strings.SplitN(string(c.HostConfig.NetworkMode), ":", 2)
		if parts[0] == "container" {
			nc, err := daemon.getNetworkedContainer(c.ID, c.HostConfig.NetworkMode.ConnectedContainer())
			if err != nil {
				return err
			}
			ns.Path = fmt.Sprintf("/proc/%d/ns/net", nc.State.GetPID())
			if userNS {
				// to share a net namespace, they must also share a user namespace
				nsUser := specs.Namespace{Type: "user"}
				nsUser.Path = fmt.Sprintf("/proc/%d/ns/user", nc.State.GetPID())
				setNamespace(s, nsUser)
			}
		} else if c.HostConfig.NetworkMode.IsHost() {
			ns.Path = c.NetworkSettings.SandboxKey
		}
		setNamespace(s, ns)
	}
	// ipc
	if c.HostConfig.IpcMode.IsContainer() {
		ns := specs.Namespace{Type: "ipc"}
		ic, err := daemon.getIpcContainer(c)
		if err != nil {
			return err
		}
		ns.Path = fmt.Sprintf("/proc/%d/ns/ipc", ic.State.GetPID())
		setNamespace(s, ns)
		if userNS {
			// to share an IPC namespace, they must also share a user namespace
			nsUser := specs.Namespace{Type: "user"}
			nsUser.Path = fmt.Sprintf("/proc/%d/ns/user", ic.State.GetPID())
			setNamespace(s, nsUser)
		}
	} else if c.HostConfig.IpcMode.IsHost() {
		delNamespace(s, specs.NamespaceType("ipc"))
	} else {
		ns := specs.Namespace{Type: "ipc"}
		setNamespace(s, ns)
	}
	// pid
	if c.HostConfig.PidMode.IsHost() {
		delNamespace(s, specs.NamespaceType("pid"))
	}
	// uts
	if c.HostConfig.UTSMode.IsHost() {
		delNamespace(s, specs.NamespaceType("uts"))
		s.Hostname = ""
	}

	return nil
}

func specMapping(s []idtools.IDMap) []specs.IDMapping {
	var ids []specs.IDMapping
	for _, item := range s {
		ids = append(ids, specs.IDMapping{
			HostID:      uint32(item.HostID),
			ContainerID: uint32(item.ContainerID),
			Size:        uint32(item.Size),
		})
	}
	return ids
}

func getMountInfo(mountinfo []*mount.Info, dir string) *mount.Info {
	for _, m := range mountinfo {
		if m.Mountpoint == dir {
			return m
		}
	}
	return nil
}

// Get the source mount point of directory passed in as argument. Also return
// optional fields.
func getSourceMount(source string) (string, string, error) {
	// Ensure any symlinks are resolved.
	sourcePath, err := filepath.EvalSymlinks(source)
	if err != nil {
		return "", "", err
	}

	mountinfos, err := mount.GetMounts()
	if err != nil {
		return "", "", err
	}

	mountinfo := getMountInfo(mountinfos, sourcePath)
	if mountinfo != nil {
		return sourcePath, mountinfo.Optional, nil
	}

	path := sourcePath
	for {
		path = filepath.Dir(path)

		mountinfo = getMountInfo(mountinfos, path)
		if mountinfo != nil {
			return path, mountinfo.Optional, nil
		}

		if path == "/" {
			break
		}
	}

	// If we are here, we did not find parent mount. Something is wrong.
	return "", "", fmt.Errorf("Could not find source mount of %s", source)
}

// Ensure mount point on which path is mounted, is shared.
func ensureShared(path string) error {
	sharedMount := false

	sourceMount, optionalOpts, err := getSourceMount(path)
	if err != nil {
		return err
	}
	// Make sure source mount point is shared.
	optsSplit := strings.Split(optionalOpts, " ")
	for _, opt := range optsSplit {
		if strings.HasPrefix(opt, "shared:") {
			sharedMount = true
			break
		}
	}

	if !sharedMount {
		return fmt.Errorf("Path %s is mounted on %s but it is not a shared mount.", path, sourceMount)
	}
	return nil
}

// Ensure mount point on which path is mounted, is either shared or slave.
func ensureSharedOrSlave(path string) error {
	sharedMount := false
	slaveMount := false

	sourceMount, optionalOpts, err := getSourceMount(path)
	if err != nil {
		return err
	}
	// Make sure source mount point is shared.
	optsSplit := strings.Split(optionalOpts, " ")
	for _, opt := range optsSplit {
		if strings.HasPrefix(opt, "shared:") {
			sharedMount = true
			break
		} else if strings.HasPrefix(opt, "master:") {
			slaveMount = true
			break
		}
	}

	if !sharedMount && !slaveMount {
		return fmt.Errorf("Path %s is mounted on %s but it is not a shared or slave mount.", path, sourceMount)
	}
	return nil
}

var (
	mountPropagationMap = map[string]int{
		"private":  mount.PRIVATE,
		"rprivate": mount.RPRIVATE,
		"shared":   mount.SHARED,
		"rshared":  mount.RSHARED,
		"slave":    mount.SLAVE,
		"rslave":   mount.RSLAVE,
	}

	mountPropagationReverseMap = map[int]string{
		mount.PRIVATE:  "private",
		mount.RPRIVATE: "rprivate",
		mount.SHARED:   "shared",
		mount.RSHARED:  "rshared",
		mount.SLAVE:    "slave",
		mount.RSLAVE:   "rslave",
	}
)

func setMounts(daemon *Daemon, s *specs.Spec, c *container.Container, mounts []container.Mount) error {
	userMounts := make(map[string]struct{})
	for _, m := range mounts {
		userMounts[m.Destination] = struct{}{}
	}

	// Filter out mounts that are overriden by user supplied mounts
	var defaultMounts []specs.Mount
	_, mountDev := userMounts["/dev"]
	for _, m := range s.Mounts {
		if _, ok := userMounts[m.Destination]; !ok {
			if mountDev && strings.HasPrefix(m.Destination, "/dev/") {
				continue
			}
			defaultMounts = append(defaultMounts, m)
		}
	}

	s.Mounts = defaultMounts
	for _, m := range mounts {
		for _, cm := range s.Mounts {
			if cm.Destination == m.Destination {
				return fmt.Errorf("Duplicate mount point '%s'", m.Destination)
			}
		}

		if m.Source == "tmpfs" {
			opt := []string{"noexec", "nosuid", "nodev", volume.DefaultPropagationMode}
			if m.Data != "" {
				opt = append(opt, strings.Split(m.Data, ",")...)
			} else {
				opt = append(opt, "size=65536k")
			}

			s.Mounts = append(s.Mounts, specs.Mount{Destination: m.Destination, Source: m.Source, Type: "tmpfs", Options: opt})
			continue
		}

		mt := specs.Mount{Destination: m.Destination, Source: m.Source, Type: "bind"}

		// Determine property of RootPropagation based on volume
		// properties. If a volume is shared, then keep root propagation
		// shared. This should work for slave and private volumes too.
		//
		// For slave volumes, it can be either [r]shared/[r]slave.
		//
		// For private volumes any root propagation value should work.
		pFlag := mountPropagationMap[m.Propagation]
		if pFlag == mount.SHARED || pFlag == mount.RSHARED {
			if err := ensureShared(m.Source); err != nil {
				return err
			}
			rootpg := mountPropagationMap[s.Linux.RootfsPropagation]
			if rootpg != mount.SHARED && rootpg != mount.RSHARED {
				s.Linux.RootfsPropagation = mountPropagationReverseMap[mount.SHARED]
			}
		} else if pFlag == mount.SLAVE || pFlag == mount.RSLAVE {
			if err := ensureSharedOrSlave(m.Source); err != nil {
				return err
			}
			rootpg := mountPropagationMap[s.Linux.RootfsPropagation]
			if rootpg != mount.SHARED && rootpg != mount.RSHARED && rootpg != mount.SLAVE && rootpg != mount.RSLAVE {
				s.Linux.RootfsPropagation = mountPropagationReverseMap[mount.RSLAVE]
			}
		}

		opts := []string{"rbind"}
		if !m.Writable {
			opts = append(opts, "ro")
		}
		if pFlag != 0 {
			opts = append(opts, mountPropagationReverseMap[pFlag])
		}

		mt.Options = opts
		s.Mounts = append(s.Mounts, mt)
	}

	if s.Root.Readonly {
		for i, m := range s.Mounts {
			switch m.Destination {
			case "/proc", "/dev/pts", "/dev/mqueue": // /dev is remounted by runc
				continue
			}
			if _, ok := userMounts[m.Destination]; !ok {
				if !stringutils.InSlice(m.Options, "ro") {
					s.Mounts[i].Options = append(s.Mounts[i].Options, "ro")
				}
			}
		}
	}

	if c.HostConfig.Privileged {
		if !s.Root.Readonly {
			// clear readonly for /sys
			for i := range s.Mounts {
				if s.Mounts[i].Destination == "/sys" {
					clearReadOnly(&s.Mounts[i])
				}
			}
		}
		s.Linux.ReadonlyPaths = nil
		s.Linux.MaskedPaths = nil
	}

	// TODO: until a kernel/mount solution exists for handling remount in a user namespace,
	// we must clear the readonly flag for the cgroups mount (@mrunalp concurs)
	if uidMap, _ := daemon.GetUIDGIDMaps(); uidMap != nil || c.HostConfig.Privileged {
		for i, m := range s.Mounts {
			if m.Type == "cgroup" {
				clearReadOnly(&s.Mounts[i])
			}
		}
	}

	return nil
}

func (daemon *Daemon) populateCommonSpec(s *specs.Spec, c *container.Container) error {
	linkedEnv, err := daemon.setupLinkedContainers(c)
	if err != nil {
		return err
	}
	s.Root = specs.Root{
		Path:     c.BaseFS,
		Readonly: c.HostConfig.ReadonlyRootfs,
	}
	rootUID, rootGID := daemon.GetRemappedUIDGID()
	if err := c.SetupWorkingDirectory(rootUID, rootGID); err != nil {
		return err
	}
	cwd := c.Config.WorkingDir
	if len(cwd) == 0 {
		cwd = "/"
	}
	s.Process.Args = append([]string{c.Path}, c.Args...)
	s.Process.Cwd = cwd
	s.Process.Env = c.CreateDaemonEnvironment(linkedEnv)
	s.Process.Terminal = c.Config.Tty
	s.Hostname = c.FullHostname()

	return nil
}

func (daemon *Daemon) createSpec(c *container.Container) (*libcontainerd.Spec, error) {
	s := oci.DefaultSpec()
	if err := daemon.populateCommonSpec(&s, c); err != nil {
		return nil, err
	}

	var cgroupsPath string
	scopePrefix := "docker"
	parent := "/docker"
	useSystemd := UsingSystemd(daemon.configStore)
	if useSystemd {
		parent = "system.slice"
	}

	if c.HostConfig.CgroupParent != "" {
		parent = c.HostConfig.CgroupParent
	} else if daemon.configStore.CgroupParent != "" {
		parent = daemon.configStore.CgroupParent
	}

	if useSystemd {
		cgroupsPath = parent + ":" + scopePrefix + ":" + c.ID
		logrus.Debugf("createSpec: cgroupsPath: %s", cgroupsPath)
	} else {
		cgroupsPath = filepath.Join(parent, c.ID)
	}
	s.Linux.CgroupsPath = &cgroupsPath

	if err := setResources(&s, c.HostConfig.Resources); err != nil {
		return nil, fmt.Errorf("linux runtime spec resources: %v", err)
	}
	s.Linux.Resources.OOMScoreAdj = &c.HostConfig.OomScoreAdj
	s.Linux.Sysctl = c.HostConfig.Sysctls
	if err := setDevices(&s, c); err != nil {
		return nil, fmt.Errorf("linux runtime spec devices: %v", err)
	}
	if err := setRlimits(daemon, &s, c); err != nil {
		return nil, fmt.Errorf("linux runtime spec rlimits: %v", err)
	}
	if err := setUser(&s, c); err != nil {
		return nil, fmt.Errorf("linux spec user: %v", err)
	}
	if err := setNamespaces(daemon, &s, c); err != nil {
		return nil, fmt.Errorf("linux spec namespaces: %v", err)
	}
	if err := setCapabilities(&s, c); err != nil {
		return nil, fmt.Errorf("linux spec capabilities: %v", err)
	}
	if err := setSeccomp(daemon, &s, c); err != nil {
		return nil, fmt.Errorf("linux seccomp: %v", err)
	}

	if err := daemon.setupIpcDirs(c); err != nil {
		return nil, err
	}

	ms, err := daemon.setupMounts(c)
	if err != nil {
		return nil, err
	}
	ms = append(ms, c.IpcMounts()...)
	ms = append(ms, c.TmpfsMounts()...)
	sort.Sort(mounts(ms))
	if err := setMounts(daemon, &s, c, ms); err != nil {
		return nil, fmt.Errorf("linux mounts: %v", err)
	}

	for _, ns := range s.Linux.Namespaces {
		if ns.Type == "network" && ns.Path == "" && !c.Config.NetworkDisabled {
			target, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(os.Getpid()), "exe"))
			if err != nil {
				return nil, err
			}

			s.Hooks = specs.Hooks{
				Prestart: []specs.Hook{{
					Path: target, // FIXME: cross-platform
					Args: []string{"libnetwork-setkey", c.ID, daemon.netController.ID()},
				}},
			}
		}
	}

	if apparmor.IsEnabled() {
		appArmorProfile := "docker-default"
		if len(c.AppArmorProfile) > 0 {
			appArmorProfile = c.AppArmorProfile
		} else if c.HostConfig.Privileged {
			appArmorProfile = "unconfined"
		}
		s.Process.ApparmorProfile = appArmorProfile
	}
	s.Process.SelinuxLabel = c.GetProcessLabel()
	s.Process.NoNewPrivileges = c.NoNewPrivileges
	s.Linux.MountLabel = c.MountLabel

	return (*libcontainerd.Spec)(&s), nil
}

func clearReadOnly(m *specs.Mount) {
	var opt []string
	for _, o := range m.Options {
		if o != "ro" {
			opt = append(opt, o)
		}
	}
	m.Options = opt
}
