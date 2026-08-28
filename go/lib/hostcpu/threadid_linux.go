//go:build linux && (amd64 || arm64)

package hostcpu

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// ThreadId identifies a Linux OS thread within the caller's PID namespace.
// The same thread can have a different identifier in another PID namespace.
// The kernel can reuse a nonzero identifier after the thread exits.
type ThreadId int

// ThreadIdentity identifies one Go runtime-managed OS thread in this process.
// Values are comparable and valid only during this process lifetime.
// A zero value does not identify a thread.
// A reused ThreadId on another live runtime thread has a different identity.
type ThreadIdentity struct {
	tid      ThreadId
	runtimeM uintptr
}

// ThreadId returns the Linux thread identifier in this identity.
func (identity ThreadIdentity) ThreadId() ThreadId { return identity.tid }

// CurrentThreadIdentity returns the identity of the calling Go runtime thread.
// A goroutine can move to another thread unless it calls runtime.LockOSThread.
// The caller must run on a Go runtime-managed thread.
// Do not call this function in a raw-clone child before exec.
// Raw clone bypasses the runtime hooks that update the runtime thread state.
//
// The fast path reads runtime.m and runtime.m.procid using the pinned Go 1.27 layout.
// Architecture-specific assembly and build guards contain that coupling.
func CurrentThreadIdentity() ThreadIdentity {
	tid, runtimeM := currentThreadIdentityFast()
	if tid == 0 {
		tid = currentThreadIdSyscall()
	}
	return ThreadIdentity{tid: tid, runtimeM: runtimeM}
}

// CurrentThreadId returns the identifier for the calling OS thread.
// The result identifies that thread only until it exits.
// A goroutine can move to another thread unless it calls runtime.LockOSThread.
// The caller must run on a Go runtime-managed thread.
// Do not call this function in a raw-clone child before exec.
// Raw clone bypasses the runtime hook that updates runtime.m.procid.
func CurrentThreadId() ThreadId { return CurrentThreadIdentity().ThreadId() }

func currentThreadIdSyscall() ThreadId { return ThreadId(syscall.Gettid()) }

func currentThreadIdentityFast() (ThreadId, uintptr)

// ErrProcfsPIDNamespace reports that procfs cannot safely address local thread IDs.
var ErrProcfsPIDNamespace = errors.New("hostcpu: procfs PID namespace differs")

// ProcfsPIDNamespaceError reports the expected thread ID and procfs NSpid values.
type ProcfsPIDNamespaceError struct {
	ThreadId ThreadId
	NSpids   []ThreadId
}

// Error returns the procfs PID namespace mismatch description.
func (err *ProcfsPIDNamespaceError) Error() string {
	return fmt.Sprintf("%v: thread ID %d, NSpid values %v", ErrProcfsPIDNamespace, err.ThreadId, err.NSpids)
}

// Unwrap supports errors.Is with ErrProcfsPIDNamespace.
func (err *ProcfsPIDNamespaceError) Unwrap() error { return ErrProcfsPIDNamespace }

// ValidateProcfsPIDNamespace verifies that procfs uses the caller's PID namespace.
// One NSpid value means procfs and the caller use the same PID namespace.
func ValidateProcfsPIDNamespace() error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	return validateProcfsPIDNamespace("/proc/thread-self/status", CurrentThreadIdentity().ThreadId())
}

func validateProcfsPIDNamespace(statusPath string, expected ThreadId) error {
	return validateProcfsPIDNamespaceRead(statusPath, expected, os.ReadFile)
}

func validateProcfsPIDNamespaceRead(statusPath string, expected ThreadId, readFile func(string) ([]byte, error)) error {
	status, err := readFile(statusPath)
	if err != nil {
		return fmt.Errorf("read procfs thread status: %w", err)
	}
	for _, line := range strings.Split(string(status), "\n") {
		name, values, found := strings.Cut(line, ":")
		if !found || name != "NSpid" {
			continue
		}
		fields := strings.Fields(values)
		nspids := make([]ThreadId, 0, len(fields))
		for _, field := range fields {
			value, parseErr := strconv.ParseInt(field, 10, 64)
			if parseErr != nil || value <= 0 {
				return &ProcfsPIDNamespaceError{ThreadId: expected, NSpids: nspids}
			}
			nspids = append(nspids, ThreadId(value))
		}
		if len(nspids) != 1 || nspids[0] != expected {
			return &ProcfsPIDNamespaceError{ThreadId: expected, NSpids: nspids}
		}
		return nil
	}
	return &ProcfsPIDNamespaceError{ThreadId: expected}
}
