//go:build linux && (amd64 || arm64)

package hostcpu

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"syscall"
	"unsafe"
)

const affinityMaskBytes = 1024

// Affinity returns the affinity of tid. A zero tid selects the calling thread.
func Affinity(tid ThreadId) (CPUSet, error) {
	mask := make([]byte, affinityMaskBytes)
	_, _, errno := syscall.RawSyscall(syscall.SYS_SCHED_GETAFFINITY, uintptr(tid), uintptr(len(mask)), uintptr(unsafe.Pointer(&mask[0])))
	if errno != 0 {
		return CPUSet{}, fmt.Errorf("get affinity for thread %d: %w", tid, errno)
	}
	var result CPUSet
	for cpu := 0; cpu < len(mask)*8; cpu++ {
		if mask[cpu/8]&(byte(1)<<uint(cpu%8)) != 0 {
			result = result.with(cpu)
		}
	}
	return result, nil
}

// SetAffinity changes the affinity of tid. A zero tid selects the calling thread.
func SetAffinity(tid ThreadId, cpus CPUSet) error {
	if cpus.Empty() {
		return errors.New("affinity CPU set must not be empty")
	}
	mask := make([]byte, affinityMaskBytes)
	for _, cpu := range cpus.CPUs() {
		if cpu >= len(mask)*8 {
			return fmt.Errorf("CPU %d exceeds affinity mask capacity", cpu)
		}
		mask[cpu/8] |= byte(1) << uint(cpu%8)
	}
	_, _, errno := syscall.RawSyscall(syscall.SYS_SCHED_SETAFFINITY, uintptr(tid), uintptr(len(mask)), uintptr(unsafe.Pointer(&mask[0])))
	if errno != 0 {
		return fmt.Errorf("set affinity for thread %d: %w", tid, errno)
	}
	return nil
}

// WithAffinity locks the goroutine to its thread, applies cpus, and restores the prior affinity.
func WithAffinity(cpus CPUSet, fn func() error) (err error) {
	if fn == nil {
		return errors.New("affinity callback must not be nil")
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	original, err := Affinity(0)
	if err != nil {
		return err
	}
	if err = SetAffinity(0, cpus); err != nil {
		return err
	}
	defer func() {
		if restoreErr := SetAffinity(0, original); restoreErr != nil {
			if err == nil {
				err = restoreErr
			} else {
				err = errors.Join(err, restoreErr)
			}
		}
	}()
	return fn()
}

// ThreadRegistry tracks cooperating, registered OS threads.
// Only registered threads receive an active ProcessPolicy.
type ThreadRegistry struct {
	mu      sync.Mutex
	threads map[ThreadId]*ThreadRegistration
	active  *activePolicy
}

type activePolicy struct {
	cpus      CPUSet
	originals map[*ThreadRegistration]CPUSet
}

// ThreadRegistration represents a locked and registered OS thread.
// Close must run on the goroutine that called RegisterCurrentThread.
type ThreadRegistration struct {
	registry *ThreadRegistry
	tid      ThreadId
	closed   bool
}

// RegisterCurrentThread locks the current goroutine to its OS thread and registers that thread.
// If a policy is active, the new thread receives it before this function returns.
func (r *ThreadRegistry) RegisterCurrentThread() (*ThreadRegistration, error) {
	if r == nil {
		return nil, errors.New("thread registry must not be nil")
	}
	runtime.LockOSThread()
	tid := CurrentThreadId()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.threads == nil {
		r.threads = make(map[ThreadId]*ThreadRegistration)
	}
	if _, exists := r.threads[tid]; exists {
		runtime.UnlockOSThread()
		return nil, fmt.Errorf("thread %d is already registered", tid)
	}
	registration := &ThreadRegistration{registry: r, tid: tid}
	if r.active != nil {
		original, err := Affinity(tid)
		if err != nil {
			runtime.UnlockOSThread()
			return nil, err
		}
		if err := SetAffinity(tid, r.active.cpus); err != nil {
			runtime.UnlockOSThread()
			return nil, err
		}
		r.active.originals[registration] = original
	}
	r.threads[tid] = registration
	return registration, nil
}

// Tid returns the registered Linux thread identifier.
func (r *ThreadRegistration) Tid() ThreadId {
	if r == nil {
		return 0
	}
	return r.tid
}

// Close unregisters the thread and unlocks the calling goroutine from it.
func (r *ThreadRegistration) Close() error {
	if r == nil {
		return nil
	}
	if tid := CurrentThreadId(); tid != r.tid {
		return fmt.Errorf("close thread registration %d from thread %d", r.tid, tid)
	}
	r.registry.mu.Lock()
	if r.closed {
		r.registry.mu.Unlock()
		return errors.New("thread registration is already closed")
	}
	if r.registry.active != nil {
		original := r.registry.active.originals[r]
		if err := SetAffinity(r.tid, original); err != nil {
			r.registry.mu.Unlock()
			return err
		}
		delete(r.registry.active.originals, r)
	}
	delete(r.registry.threads, r.tid)
	r.closed = true
	r.registry.mu.Unlock()
	runtime.UnlockOSThread()
	return nil
}

// ProcessPolicy applies one affinity to all cooperating registered threads.
type ProcessPolicy struct {
	Registry *ThreadRegistry
	CPUs     CPUSet
}

// Apply changes every registered thread and returns an idempotent rollback function.
// New registrations receive the policy before RegisterCurrentThread returns.
// Apply restores changed threads before it returns any application error.
func (p ProcessPolicy) Apply() (func() error, error) {
	if p.Registry == nil {
		return nil, errors.New("process policy requires a thread registry")
	}
	if p.CPUs.Empty() {
		return nil, errors.New("process policy CPU set must not be empty")
	}
	p.Registry.mu.Lock()
	if p.Registry.active != nil {
		p.Registry.mu.Unlock()
		return nil, errors.New("a process policy is already active")
	}
	active := &activePolicy{cpus: p.CPUs, originals: make(map[*ThreadRegistration]CPUSet, len(p.Registry.threads))}
	changed := make([]*ThreadRegistration, 0, len(p.Registry.threads))
	for _, registration := range p.Registry.threads {
		original, err := Affinity(registration.tid)
		if err != nil {
			rollbackErr := rollbackRegistrations(changed, active.originals, err)
			p.Registry.mu.Unlock()
			return nil, rollbackErr
		}
		active.originals[registration] = original
		if err := SetAffinity(registration.tid, p.CPUs); err != nil {
			delete(active.originals, registration)
			rollbackErr := rollbackRegistrations(changed, active.originals, err)
			p.Registry.mu.Unlock()
			return nil, rollbackErr
		}
		changed = append(changed, registration)
	}
	p.Registry.active = active
	p.Registry.mu.Unlock()

	var once sync.Once
	var rollbackErr error
	rollback := func() error {
		once.Do(func() {
			p.Registry.mu.Lock()
			registrations := make([]*ThreadRegistration, 0, len(active.originals))
			for registration := range active.originals {
				registrations = append(registrations, registration)
			}
			rollbackErr = rollbackRegistrations(registrations, active.originals, nil)
			if p.Registry.active == active {
				p.Registry.active = nil
			}
			p.Registry.mu.Unlock()
		})
		return rollbackErr
	}
	return rollback, nil
}

func rollbackRegistrations(registrations []*ThreadRegistration, original map[*ThreadRegistration]CPUSet, cause error) error {
	err := cause
	for i := len(registrations) - 1; i >= 0; i-- {
		registration := registrations[i]
		if restoreErr := SetAffinity(registration.tid, original[registration]); restoreErr != nil {
			err = errors.Join(err, restoreErr)
		}
	}
	return err
}
