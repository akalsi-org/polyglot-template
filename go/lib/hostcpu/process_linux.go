//go:build linux && (amd64 || arm64)

package hostcpu

import (
	"strconv"
	"syscall"
)

// PIDNamespaceIdentity identifies one Linux PID namespace.
// Values are comparable and stable while the namespace exists.
type PIDNamespaceIdentity struct {
	Dev uint64
	Ino uint64
}

// CurrentPIDNamespaceIdentity returns the caller's PID namespace identity.
func CurrentPIDNamespaceIdentity() (PIDNamespaceIdentity, error) {
	return pidNamespaceIdentityAt("/proc/self/ns/pid")
}

func pidNamespaceIdentityAt(path string) (PIDNamespaceIdentity, error) {
	var stat syscall.Stat_t
	if err := syscall.Stat(path, &stat); err != nil {
		return PIDNamespaceIdentity{}, err
	}
	return PIDNamespaceIdentity{Dev: uint64(stat.Dev), Ino: stat.Ino}, nil
}

func schedulerReportsDead(tid ThreadId) bool {
	_, _, errno := syscall.Syscall(syscall.SYS_SCHED_GETSCHEDULER, uintptr(tid), 0, 0)
	return errno == syscall.ESRCH
}

var (
	schedulerDeadProbe = schedulerReportsDead
	openThreadStat     = syscall.Open
)

// ThreadAlive reports whether tid can still identify a live Linux thread.
// Ambiguous scheduler and procfs results report the thread as live.
func ThreadAlive(tid ThreadId) bool {
	if tid == 0 {
		return false
	}
	if schedulerDeadProbe(tid) {
		return false
	}
	path := "/proc/" + strconv.FormatUint(uint64(tid), 10) + "/stat"
	fd, err := openThreadStat(path, syscall.O_RDONLY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return err != syscall.ENOENT
	}
	var buffer [256]byte
	n, err := syscall.Read(fd, buffer[:])
	_ = syscall.Close(fd)
	if err != nil || n <= 0 {
		return !schedulerDeadProbe(tid)
	}
	last := -1
	for index := n - 1; index >= 0; index-- {
		if buffer[index] == ')' {
			last = index
			break
		}
	}
	if last < 0 {
		return true
	}
	index := last + 1
	for index < n && buffer[index] == ' ' {
		index++
	}
	return index >= n || (buffer[index] != 'Z' && buffer[index] != 'X')
}
