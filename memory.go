package cgroups

import (
	"bufio"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	specs "github.com/opencontainers/runtime-spec/specs-go"
)

func NewMemory(root string) *memoryController {
	return &memoryController{
		root: filepath.Join(root, string(Memory)),
	}
}

type memoryController struct {
	root string
}

func (m *memoryController) Name() Name {
	return Memory
}

func (m *memoryController) Path(path string) string {
	return filepath.Join(m.root, path)
}

func (m *memoryController) Create(path string, resources *specs.LinuxResources) error {
	if err := os.MkdirAll(m.Path(path), defaultDirPerm); err != nil {
		return err
	}
	if resources.Memory == nil {
		return nil
	}
	if resources.Memory.Kernel != nil {
		// Check if kernel memory is enabled
		// We have to limit the kernel memory here as it won't be accounted at all
		// until a limit is set on the cgroup and limit cannot be set once the
		// cgroup has children, or if there are already tasks in the cgroup.
		for _, i := range []int64{1, -1} {
			if err := ioutil.WriteFile(
				filepath.Join(m.Path(path), "memory.kmem.limit_in_bytes"),
				[]byte(strconv.FormatInt(i, 10)),
				defaultFilePerm,
			); err != nil {
				return checkEBUSY(err)
			}
		}
	}
	return m.set(path, getMemorySettings(resources))
}

func (m *memoryController) Update(path string, resources *specs.LinuxResources) error {
	if resources.Memory == nil {
		return nil
	}
	g := func(v *uint64) bool {
		return v != nil && *v > 0
	}
	settings := getMemorySettings(resources)
	if g(resources.Memory.Limit) && g(resources.Memory.Swap) {
		// if the updated swap value is larger than the current memory limit set the swap changes first
		// then set the memory limit as swap must always be larger than the current limit
		current, err := readUint(filepath.Join(m.Path(path), "memory.limit_in_bytes"))
		if err != nil {
			return err
		}
		if current < uint64(*resources.Memory.Swap) {
			settings[0], settings[1] = settings[1], settings[0]
		}
	}
	return m.set(path, settings)
}

func (m *memoryController) Stat(path string, stats *Stats) error {
	raw, err := m.parseStats(path)
	if err != nil {
		return err
	}
	stats.Memory = &MemoryStat{
		Cache:                   raw["cache"],
		RSS:                     raw["rss"],
		RSSHuge:                 raw["rss_huge"],
		MappedFile:              raw["mapped_file"],
		Dirty:                   raw["dirty"],
		Writeback:               raw["writeback"],
		PgPgIn:                  raw["pgpgin"],
		PgPgOut:                 raw["pgpgout"],
		PgFault:                 raw["pgfault"],
		PgMajFault:              raw["pgmajfault"],
		InactiveAnon:            raw["inactive_anon"],
		ActiveAnon:              raw["active_anon"],
		InactiveFile:            raw["inactive_file"],
		ActiveFile:              raw["active_file"],
		Unevictable:             raw["unevictable"],
		HierarchicalMemoryLimit: raw["hierarchical_memory_limit"],
		HierarchicalSwapLimit:   raw["hierarchical_memsw_limit"],
		TotalCache:              raw["total_cache"],
		TotalRSS:                raw["total_rss"],
		TotalRSSHuge:            raw["total_rss_huge"],
		TotalMappedFile:         raw["total_mapped_file"],
		TotalDirty:              raw["total_dirty"],
		TotalWriteback:          raw["total_writeback"],
		TotalPgPgIn:             raw["total_pgpgin"],
		TotalPgPgOut:            raw["total_pgpgout"],
		TotalPgFault:            raw["total_pgfault"],
		TotalPgMajFault:         raw["total_pgmajfault"],
		TotalInactiveAnon:       raw["total_inactive_anon"],
		TotalActiveAnon:         raw["total_active_anon"],
		TotalInactiveFile:       raw["total_inactive_file"],
		TotalActiveFile:         raw["total_active_file"],
		TotalUnevictable:        raw["total_unevictable"],
	}
	for _, t := range []struct {
		module string
		entry  *MemoryEntry
	}{
		{
			module: "",
			entry:  &stats.Memory.Usage,
		},
		{
			module: "memsw",
			entry:  &stats.Memory.Swap,
		},
		{
			module: "kmem",
			entry:  &stats.Memory.Kernel,
		},
		{
			module: "kmem.tcp",
			entry:  &stats.Memory.KernelTCP,
		},
	} {
		for _, tt := range []struct {
			name  string
			value *uint64
		}{
			{
				name:  "usage_in_bytes",
				value: &t.entry.Usage,
			},
			{
				name:  "max_usage_in_bytes",
				value: &t.entry.Max,
			},
			{
				name:  "failcnt",
				value: &t.entry.Failcnt,
			},
			{
				name:  "limit_in_bytes",
				value: &t.entry.Limit,
			},
		} {
			parts := []string{"memory"}
			if t.module != "" {
				parts = append(parts, t.module)
			}
			parts = append(parts, tt.name)
			v, err := readUint(filepath.Join(m.Path(path), strings.Join(parts, ".")))
			if err != nil {
				return err
			}
			*tt.value = v
		}
	}
	return nil
}

func (m *memoryController) OOMEventFD(path string) (uintptr, error) {
	root := m.Path(path)
	f, err := os.Open(filepath.Join(root, "memory.oom_control"))
	if err != nil {
		return 0, err
	}
	defer f.Close()
	fd, _, serr := syscall.RawSyscall(syscall.SYS_EVENTFD2, 0, syscall.FD_CLOEXEC, 0)
	if serr != 0 {
		return 0, serr
	}
	if err := writeEventFD(root, f.Fd(), fd); err != nil {
		syscall.Close(int(fd))
		return 0, err
	}
	return fd, nil
}

func writeEventFD(root string, cfd, efd uintptr) error {
	f, err := os.OpenFile(filepath.Join(root, "cgroup.event_control"), os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	_, err = f.WriteString(fmt.Sprintf("%d %d", efd, cfd))
	f.Close()
	return err
}

func (m *memoryController) parseStats(path string) (map[string]uint64, error) {
	f, err := os.Open(filepath.Join(m.Path(path), "memory.stat"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var (
		out = make(map[string]uint64)
		sc  = bufio.NewScanner(f)
	)
	for sc.Scan() {
		if err := sc.Err(); err != nil {
			return nil, err
		}
		key, v, err := parseKV(sc.Text())
		if err != nil {
			return nil, err
		}
		out[key] = v
	}
	return out, nil
}

func (m *memoryController) set(path string, settings []memorySettings) error {
	for _, t := range settings {
		if t.value != nil {
			if err := ioutil.WriteFile(
				filepath.Join(m.Path(path), fmt.Sprintf("memory.%s", t.name)),
				[]byte(strconv.FormatUint(*t.value, 10)),
				defaultFilePerm,
			); err != nil {
				return err
			}
		}
	}
	return nil
}

type memorySettings struct {
	name  string
	value *uint64
}

func getMemorySettings(resources *specs.LinuxResources) []memorySettings {
	mem := resources.Memory
	var swappiness *uint64
	if mem.Swappiness != nil {
		v := uint64(*mem.Swappiness)
		swappiness = &v
	}
	return []memorySettings{
		{
			name:  "limit_in_bytes",
			value: mem.Limit,
		},
		{
			name:  "memsw.limit_in_bytes",
			value: mem.Swap,
		},
		{
			name:  "kmem.limit_in_bytes",
			value: mem.Kernel,
		},
		{
			name:  "kmem.tcp.limit_in_bytes",
			value: mem.KernelTCP,
		},
		{
			name:  "oom_control",
			value: getOomControlValue(resources),
		},
		{
			name:  "swappiness",
			value: swappiness,
		},
	}
}

func checkEBUSY(err error) error {
	if pathErr, ok := err.(*os.PathError); ok {
		if errNo, ok := pathErr.Err.(syscall.Errno); ok {
			if errNo == syscall.EBUSY {
				return fmt.Errorf(
					"failed to set memory.kmem.limit_in_bytes, because either tasks have already joined this cgroup or it has children")
			}
		}
	}
	return err
}

func getOomControlValue(resources *specs.LinuxResources) *uint64 {
	if resources.DisableOOMKiller != nil && *resources.DisableOOMKiller {
		i := uint64(1)
		return &i
	}
	return nil
}
