//go:build linux

package runner

import (
	"os"
	"strconv"
	"strings"
)

func systemMemoryPressure() bool {
	if current, limit, ok := cgroupMemory(); ok && limit > 0 {
		return memoryPressure(current, limit, 0, 0, true)
	}
	available, total, ok := hostMemory()
	return ok && memoryPressure(0, 0, available, total, false)
}

func cgroupMemory() (current, limit uint64, ok bool) {
	currentBytes, err := os.ReadFile("/sys/fs/cgroup/memory.current")
	if err != nil {
		return 0, 0, false
	}
	limitBytes, err := os.ReadFile("/sys/fs/cgroup/memory.max")
	if err != nil {
		return 0, 0, false
	}
	current, err = strconv.ParseUint(strings.TrimSpace(string(currentBytes)), 10, 64)
	if err != nil {
		return 0, 0, false
	}
	limit, err = strconv.ParseUint(strings.TrimSpace(string(limitBytes)), 10, 64)
	if err != nil {
		return 0, 0, false // "max" means the host is the useful boundary.
	}
	return current, limit, true
}

func hostMemory() (available, total uint64, ok bool) {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0, false
	}
	for _, line := range strings.Split(string(b), "\n") {
		name, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		fields := strings.Fields(value)
		if len(fields) == 0 {
			continue
		}
		n, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			continue
		}
		// /proc/meminfo reports these values in KiB.
		n *= 1024
		switch name {
		case "MemAvailable":
			available = n
		case "MemTotal":
			total = n
		}
	}
	return available, total, available > 0 && total > 0
}
