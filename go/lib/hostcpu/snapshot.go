package hostcpu

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// CPU describes one logical CPU from sysfs.
type CPU struct {
	Id        int
	CoreId    int
	PackageId int
	NodeId    int
	Siblings  CPUSet
}

// NUMANode describes one NUMA node from sysfs.
type NUMANode struct {
	Id   int
	CPUs CPUSet
}

// Snapshot is an immutable host CPU topology snapshot.
type Snapshot struct {
	Online   CPUSet
	Possible CPUSet
	Present  CPUSet
	Isolated CPUSet
	NoHZFull CPUSet
	RCUNocbs CPUSet
	CPUs     []CPU
	Nodes    []NUMANode
}

// FS identifies Linux virtual filesystem roots. Empty fields use standard roots.
type FS struct {
	SysfsRoot  string
	ProcfsRoot string
	CgroupRoot string
}

func (fs FS) roots() (string, string, string) {
	sys, proc, cgroup := fs.SysfsRoot, fs.ProcfsRoot, fs.CgroupRoot
	if sys == "" {
		sys = "/sys"
	}
	if proc == "" {
		proc = "/proc"
	}
	if cgroup == "" {
		cgroup = "/sys/fs/cgroup"
	}
	return sys, proc, cgroup
}

// ReadSnapshot reads topology, NUMA, and CPU isolation state.
func (fs FS) ReadSnapshot() (Snapshot, error) {
	sys, proc, _ := fs.roots()
	cpuRoot := filepath.Join(sys, "devices/system/cpu")
	var snapshot Snapshot
	var err error
	if snapshot.Online, err = readCPUSet(filepath.Join(cpuRoot, "online"), false); err != nil {
		return Snapshot{}, err
	}
	if snapshot.Possible, err = readCPUSet(filepath.Join(cpuRoot, "possible"), false); err != nil {
		return Snapshot{}, err
	}
	if snapshot.Present, err = readCPUSet(filepath.Join(cpuRoot, "present"), false); err != nil {
		return Snapshot{}, err
	}
	if snapshot.Isolated, err = readCPUSet(filepath.Join(cpuRoot, "isolated"), true); err != nil {
		return Snapshot{}, err
	}
	if snapshot.NoHZFull, err = readCPUSet(filepath.Join(cpuRoot, "nohz_full"), true); err != nil {
		return Snapshot{}, err
	}
	if snapshot.RCUNocbs, err = readKernelParameter(filepath.Join(proc, "cmdline"), "rcu_nocbs"); err != nil {
		return Snapshot{}, err
	}

	var cpuNodes map[int]int
	snapshot.Nodes, cpuNodes, err = readNUMANodes(filepath.Join(sys, "devices/system/node"))
	if err != nil {
		return Snapshot{}, err
	}
	presentCPUs := snapshot.Present.CPUs()
	snapshot.CPUs = make([]CPU, 0, len(presentCPUs))
	for _, id := range presentCPUs {
		topology := filepath.Join(cpuRoot, "cpu"+strconv.Itoa(id), "topology")
		core, err := readInt(filepath.Join(topology, "core_id"))
		if err != nil {
			return Snapshot{}, err
		}
		pkg, err := readInt(filepath.Join(topology, "physical_package_id"))
		if err != nil {
			return Snapshot{}, err
		}
		siblings, err := readCPUSet(filepath.Join(topology, "thread_siblings_list"), false)
		if err != nil {
			return Snapshot{}, err
		}
		node := -1
		if nodeId, ok := cpuNodes[id]; ok {
			node = nodeId
		}
		snapshot.CPUs = append(snapshot.CPUs, CPU{Id: id, CoreId: core, PackageId: pkg, NodeId: node, Siblings: siblings})
	}
	return snapshot, nil
}

// ReadSnapshot reads the live host snapshot.
func ReadSnapshot() (Snapshot, error) { return (FS{}).ReadSnapshot() }

func readCPUSet(path string, optional bool) (CPUSet, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if optional && errors.Is(err, os.ErrNotExist) {
			return CPUSet{}, nil
		}
		return CPUSet{}, fmt.Errorf("read CPU set %s: %w", path, err)
	}
	text := strings.TrimSpace(string(data))
	if optional && text == "(null)" {
		return CPUSet{}, nil
	}
	set, err := ParseCPUSet(text)
	if err != nil {
		return CPUSet{}, fmt.Errorf("read CPU set %s: %w", path, err)
	}
	return set, nil
}

func readInt(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", path, err)
	}
	value, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", path, err)
	}
	return value, nil
}

func readNUMANodes(nodeRoot string) ([]NUMANode, map[int]int, error) {
	entries, err := filepath.Glob(filepath.Join(nodeRoot, "node[0-9]*"))
	if err != nil {
		return nil, nil, fmt.Errorf("list NUMA nodes: %w", err)
	}
	nodes := make([]NUMANode, 0, len(entries))
	for _, path := range entries {
		id, err := strconv.Atoi(strings.TrimPrefix(filepath.Base(path), "node"))
		if err != nil {
			continue
		}
		cpus, err := readCPUSet(filepath.Join(path, "cpulist"), false)
		if err != nil {
			return nil, nil, err
		}
		nodes = append(nodes, NUMANode{Id: id, CPUs: cpus})
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Id < nodes[j].Id })

	cpuCount := 0
	for _, node := range nodes {
		cpuCount += node.CPUs.Count()
	}
	cpuNodes := make(map[int]int, cpuCount)
	for _, node := range nodes {
		for _, cpu := range node.CPUs.CPUs() {
			if _, exists := cpuNodes[cpu]; !exists {
				cpuNodes[cpu] = node.Id
			}
		}
	}
	return nodes, cpuNodes, nil
}

func readKernelParameter(path, name string) (CPUSet, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return CPUSet{}, fmt.Errorf("read kernel command line %s: %w", path, err)
	}
	prefix := name + "="
	for _, field := range strings.Fields(string(data)) {
		if strings.HasPrefix(field, prefix) {
			return ParseCPUSet(strings.TrimPrefix(field, prefix))
		}
	}
	return CPUSet{}, nil
}
