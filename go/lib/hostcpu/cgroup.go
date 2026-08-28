package hostcpu

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// EffectiveCPUSet reads the effective cpuset for pid. A zero pid selects this process.
func (fs FS) EffectiveCPUSet(pid int) (CPUSet, error) {
	_, proc, cgroup := fs.roots()
	if pid == 0 {
		pid = os.Getpid()
	}
	data, err := os.ReadFile(filepath.Join(proc, strconv.Itoa(pid), "cgroup"))
	if err != nil {
		return CPUSet{}, fmt.Errorf("read process cgroup: %w", err)
	}

	var unified, cpuset string
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		parts := strings.SplitN(scanner.Text(), ":", 3)
		if len(parts) != 3 {
			return CPUSet{}, fmt.Errorf("parse process cgroup line %q", scanner.Text())
		}
		if parts[0] == "0" && parts[1] == "" {
			unified = parts[2]
		}
		for _, controller := range strings.Split(parts[1], ",") {
			if controller == "cpuset" {
				cpuset = parts[2]
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return CPUSet{}, fmt.Errorf("scan process cgroup: %w", err)
	}

	if unified != "" {
		return readCgroupCPUSet(cgroupPath(cgroup, unified), []string{"cpuset.cpus.effective", "cpuset.cpus"})
	}
	if cpuset != "" {
		root := cgroup
		if _, err := os.Stat(filepath.Join(root, "cpuset")); err == nil {
			root = filepath.Join(root, "cpuset")
		}
		return readCgroupCPUSet(cgroupPath(root, cpuset), []string{"cpuset.effective_cpus", "cpuset.cpus"})
	}
	return CPUSet{}, errors.New("process has no cpuset cgroup")
}

// EffectiveCPUSet reads the live process effective cpuset.
func EffectiveCPUSet() (CPUSet, error) { return (FS{}).EffectiveCPUSet(0) }

func cgroupPath(root, group string) string {
	group = filepath.Clean("/" + group)
	return filepath.Join(root, strings.TrimPrefix(group, "/"))
}

func readCgroupCPUSet(dir string, names []string) (CPUSet, error) {
	var last error
	for _, name := range names {
		set, err := readCPUSet(filepath.Join(dir, name), false)
		if err == nil {
			return set, nil
		}
		last = err
		if !errors.Is(err, os.ErrNotExist) {
			return CPUSet{}, err
		}
	}
	return CPUSet{}, last
}
