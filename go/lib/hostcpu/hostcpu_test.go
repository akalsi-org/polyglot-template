//go:build linux && (amd64 || arm64)

package hostcpu

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"syscall"
	"testing"
)

func TestParseCPUSetAndAlgebra(t *testing.T) {
	set, err := ParseCPUSet("0-3,8,10-11\n")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := set.String(), "0-3,8,10-11"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
	if got := set.CPUs(); !reflect.DeepEqual(got, []int{0, 1, 2, 3, 8, 10, 11}) {
		t.Fatalf("CPUs() = %v", got)
	}
	other, _ := ParseCPUSet("2-8")
	if got := set.Intersection(other).String(); got != "2-3,8" {
		t.Fatalf("intersection = %q", got)
	}
	if got := set.Union(other).String(); got != "0-8,10-11" {
		t.Fatalf("union = %q", got)
	}
	if got := set.Difference(other).String(); got != "0-1,10-11" {
		t.Fatalf("difference = %q", got)
	}
	if !set.Intersection(other).IsSubsetOf(set) {
		t.Fatal("intersection is not a subset")
	}
}

func TestParseCPUSetRejectsInvalidInput(t *testing.T) {
	for _, input := range []string{"-1", "3-1", "1,,2", "a", "1-2-3"} {
		if _, err := ParseCPUSet(input); err == nil {
			t.Errorf("ParseCPUSet(%q) succeeded", input)
		}
	}
}

func TestParseCPUSetLargeRangeUsesBoundedAllocations(t *testing.T) {
	var parsed CPUSet
	allocations := testing.AllocsPerRun(100, func() {
		var err error
		parsed, err = ParseCPUSet("0-1048575")
		if err != nil {
			t.Fatal(err)
		}
	})
	if got, want := parsed.Count(), 1048576; got != want {
		t.Fatalf("Count() = %d, want %d", got, want)
	}
	if allocations > 3 {
		t.Fatalf("ParseCPUSet allocated %.0f times, want at most 3", allocations)
	}
}

func TestReadSnapshotFromFixture(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "sys/devices/system/cpu/online", "0-3\n")
	writeFixture(t, root, "sys/devices/system/cpu/possible", "0-7\n")
	writeFixture(t, root, "sys/devices/system/cpu/present", "0-3\n")
	writeFixture(t, root, "sys/devices/system/cpu/isolated", "2-3\n")
	writeFixture(t, root, "sys/devices/system/cpu/nohz_full", "(null)\n")
	writeFixture(t, root, "proc/cmdline", "quiet rcu_nocbs=2-3 console=tty0\n")
	for cpu := 0; cpu < 4; cpu++ {
		base := filepath.Join("sys/devices/system/cpu", "cpu"+string(rune('0'+cpu)), "topology")
		writeFixture(t, root, filepath.Join(base, "core_id"), string(rune('0'+cpu/2))+"\n")
		writeFixture(t, root, filepath.Join(base, "physical_package_id"), "0\n")
		if cpu < 2 {
			writeFixture(t, root, filepath.Join(base, "thread_siblings_list"), "0-1\n")
		} else {
			writeFixture(t, root, filepath.Join(base, "thread_siblings_list"), "2-3\n")
		}
		nodeDir := filepath.Join(root, "sys/devices/system/cpu", "cpu"+string(rune('0'+cpu)), "node"+string(rune('0'+cpu/2)))
		if err := os.MkdirAll(nodeDir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFixture(t, root, "sys/devices/system/node/node0/cpulist", "0-1\n")
	writeFixture(t, root, "sys/devices/system/node/node1/cpulist", "2-3\n")

	snapshot, err := (FS{SysfsRoot: filepath.Join(root, "sys"), ProcfsRoot: filepath.Join(root, "proc")}).ReadSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if got := snapshot.Isolated.String(); got != "2-3" {
		t.Fatalf("isolated = %q", got)
	}
	if got := snapshot.NoHZFull.String(); got != "" {
		t.Fatalf("nohz_full = %q", got)
	}
	if got := snapshot.RCUNocbs.String(); got != "2-3" {
		t.Fatalf("rcu_nocbs = %q", got)
	}
	if len(snapshot.CPUs) != 4 || snapshot.CPUs[3].NodeId != 1 {
		t.Fatalf("CPUs = %+v", snapshot.CPUs)
	}
	if len(snapshot.Nodes) != 2 || snapshot.Nodes[1].CPUs.String() != "2-3" {
		t.Fatalf("nodes = %+v", snapshot.Nodes)
	}
}

func TestReadSnapshotMapsCPUsFromNUMANodeLists(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "sys/devices/system/cpu/online", "0,63-64,130\n")
	writeFixture(t, root, "sys/devices/system/cpu/possible", "0-130\n")
	writeFixture(t, root, "sys/devices/system/cpu/present", "0,63-64,130\n")
	writeFixture(t, root, "proc/cmdline", "quiet\n")
	for _, cpu := range []int{0, 63, 64, 130} {
		base := filepath.Join("sys/devices/system/cpu", fmt.Sprintf("cpu%d/topology", cpu))
		writeFixture(t, root, filepath.Join(base, "core_id"), fmt.Sprintf("%d\n", cpu))
		writeFixture(t, root, filepath.Join(base, "physical_package_id"), "0\n")
		writeFixture(t, root, filepath.Join(base, "thread_siblings_list"), fmt.Sprintf("%d\n", cpu))
	}
	writeFixture(t, root, "sys/devices/system/node/node11/cpulist", "63,130\n")
	writeFixture(t, root, "sys/devices/system/node/node2/cpulist", "0,64\n")

	snapshot, err := (FS{SysfsRoot: filepath.Join(root, "sys"), ProcfsRoot: filepath.Join(root, "proc")}).ReadSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := []int{snapshot.CPUs[0].NodeId, snapshot.CPUs[1].NodeId, snapshot.CPUs[2].NodeId, snapshot.CPUs[3].NodeId}, []int{2, 11, 2, 11}; !reflect.DeepEqual(got, want) {
		t.Fatalf("CPU node IDs = %v, want %v", got, want)
	}
	if got, want := []int{snapshot.Nodes[0].Id, snapshot.Nodes[1].Id}, []int{2, 11}; !reflect.DeepEqual(got, want) {
		t.Fatalf("NUMA node IDs = %v, want %v", got, want)
	}
}

func TestEffectiveCPUSetV2Fixture(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "proc/42/cgroup", "0::/work.slice/service\n")
	writeFixture(t, root, "cgroup/work.slice/service/cpuset.cpus.effective", "1-3\n")
	set, err := (FS{ProcfsRoot: filepath.Join(root, "proc"), CgroupRoot: filepath.Join(root, "cgroup")}).EffectiveCPUSet(42)
	if err != nil {
		t.Fatal(err)
	}
	if got := set.String(); got != "1-3" {
		t.Fatalf("effective set = %q", got)
	}
}

func TestEffectiveCPUSetV1Fixture(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "proc/42/cgroup", "5:memory:/x\n4:cpuset:/jobs/a\n")
	writeFixture(t, root, "cgroup/cpuset/jobs/a/cpuset.effective_cpus", "4,6\n")
	set, err := (FS{ProcfsRoot: filepath.Join(root, "proc"), CgroupRoot: filepath.Join(root, "cgroup")}).EffectiveCPUSet(42)
	if err != nil {
		t.Fatal(err)
	}
	if got := set.String(); got != "4,6" {
		t.Fatalf("effective set = %q", got)
	}
}

func TestLiveSnapshotAndEffectiveCPUSet(t *testing.T) {
	snapshot, err := ReadSnapshot()
	if err != nil {
		t.Skipf("live topology is unavailable: %v", err)
	}
	if snapshot.Online.Empty() || !snapshot.Online.IsSubsetOf(snapshot.Present) {
		t.Fatalf("online CPUs %s are not a nonempty subset of present CPUs %s", snapshot.Online, snapshot.Present)
	}
	effective, err := EffectiveCPUSet()
	if err != nil {
		t.Skipf("live cgroup cpuset is unavailable: %v", err)
	}
	if effective.Empty() {
		t.Fatal("the live effective cgroup cpuset is empty")
	}
}

func TestWithAffinityRestoresCurrentThread(t *testing.T) {
	before, err := Affinity(0)
	if err != nil {
		t.Skipf("affinity is unavailable: %v", err)
	}
	cpus := before.CPUs()
	if len(cpus) == 0 {
		t.Skip("the affinity is empty")
	}
	one, _ := NewCPUSet(cpus[0])
	callbackErr := errors.New("callback failure")
	if err := WithAffinity(one, func() error {
		inside, err := Affinity(0)
		if err != nil {
			return err
		}
		if !inside.Equal(one) {
			t.Fatalf("inside affinity = %s, want %s", inside, one)
		}
		return callbackErr
	}); !errors.Is(err, callbackErr) {
		t.Fatalf("WithAffinity error = %v", err)
	}
	after, err := Affinity(0)
	if err != nil {
		t.Fatal(err)
	}
	if !after.Equal(before) {
		t.Fatalf("restored affinity = %s, want %s", after, before)
	}
}

func TestProcessPolicyRollback(t *testing.T) {
	var registry ThreadRegistry
	registration, err := registry.RegisterCurrentThread()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := registration.Close(); err != nil {
			t.Error(err)
		}
	}()
	before, err := Affinity(registration.Tid())
	if err != nil {
		t.Fatal(err)
	}
	cpus := before.CPUs()
	if len(cpus) == 0 {
		t.Skip("the affinity is empty")
	}
	one, _ := NewCPUSet(cpus[0])
	rollback, err := (ProcessPolicy{Registry: &registry, CPUs: one}).Apply()
	if err != nil {
		t.Fatal(err)
	}
	inside, _ := Affinity(registration.Tid())
	if !inside.Equal(one) {
		t.Fatalf("policy affinity = %s, want %s", inside, one)
	}
	if err := rollback(); err != nil {
		t.Fatal(err)
	}
	if err := rollback(); err != nil {
		t.Fatal(err)
	}
	after, _ := Affinity(registration.Tid())
	if !after.Equal(before) {
		t.Fatalf("rollback affinity = %s, want %s", after, before)
	}
}

func TestSyscallThreadIdAfterFork(t *testing.T) {
	if os.Getenv("PGT_HOSTCPU_FORK_HELPER") == "1" {
		runtime.LockOSThread()
		before := currentThreadIdSyscall()
		pid, _, errno := syscall.RawSyscall6(syscall.SYS_CLONE, uintptr(syscall.SIGCHLD), 0, 0, 0, 0, 0)
		if errno != 0 {
			os.Exit(4)
		}
		if pid == 0 {
			got := currentThreadIdSyscall()
			kernel := ThreadId(syscall.Gettid())
			code := uintptr(0)
			if got == before {
				code = 2
			} else if got != kernel {
				code = 3
			}
			syscall.RawSyscall(syscall.SYS_EXIT, code, 0, 0)
			for {
			}
		}
		var status syscall.WaitStatus
		if _, err := syscall.Wait4(int(pid), &status, 0, nil); err != nil || !status.Exited() {
			os.Exit(4)
		}
		os.Exit(status.ExitStatus())
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestSyscallThreadIdAfterFork$")
	cmd.Env = append(os.Environ(), "PGT_HOSTCPU_FORK_HELPER=1")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fork helper: %v\n%s", err, output)
	}
}

func TestCurrentThreadIdRuntimeVersionContract(t *testing.T) {
	if got, want := runtime.Version(), "go1.27.0"; got != want {
		t.Fatalf("runtime version = %q, want %q; rederive thread ID offsets before upgrading", got, want)
	}
}

func TestCurrentThreadIdentityIsStableOnLockedThread(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	identity := CurrentThreadIdentity()
	if identity == (ThreadIdentity{}) {
		t.Fatal("CurrentThreadIdentity() returned the zero identity")
	}
	if identity.ThreadId() != CurrentThreadId() {
		t.Fatalf("identity thread ID = %d, CurrentThreadId() = %d", identity.ThreadId(), CurrentThreadId())
	}
	for range 1024 {
		if got := CurrentThreadIdentity(); got != identity {
			t.Fatalf("CurrentThreadIdentity() = %v, want stable identity %v", got, identity)
		}
	}
}

func TestCurrentThreadIdentityDiffersAcrossLockedThreads(t *testing.T) {
	const threadCount = 64
	identities := make(chan ThreadIdentity, threadCount)
	release := make(chan struct{})
	for range threadCount {
		go func() {
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()
			identities <- CurrentThreadIdentity()
			<-release
		}()
	}

	seenIdentities := make(map[ThreadIdentity]struct{}, threadCount)
	seenThreadIds := make(map[ThreadId]struct{}, threadCount)
	for range threadCount {
		identity := <-identities
		if identity == (ThreadIdentity{}) {
			close(release)
			t.Fatal("CurrentThreadIdentity() returned the zero identity")
		}
		if _, exists := seenIdentities[identity]; exists {
			close(release)
			t.Fatalf("duplicate thread identity %v", identity)
		}
		if _, exists := seenThreadIds[identity.ThreadId()]; exists {
			close(release)
			t.Fatalf("duplicate live thread ID %d", identity.ThreadId())
		}
		seenIdentities[identity] = struct{}{}
		seenThreadIds[identity.ThreadId()] = struct{}{}
	}
	close(release)
}

func TestCurrentThreadIdentityRejectsAnotherGoroutine(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	owner := CurrentThreadIdentity()
	other := make(chan ThreadIdentity, 1)
	release := make(chan struct{})
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		other <- CurrentThreadIdentity()
		<-release
	}()
	got := <-other
	close(release)
	if got == owner {
		t.Fatalf("another locked goroutine received owner identity %v", owner)
	}
}

func TestValidateProcfsPIDNamespace(t *testing.T) {
	if err := ValidateProcfsPIDNamespace(); err != nil {
		t.Fatal(err)
	}
}

func TestValidateProcfsPIDNamespaceFixture(t *testing.T) {
	path := filepath.Join(t.TempDir(), "status")
	status := "Name:\tfixture\nState:\tR (running)\nNSpid:\t4242\nCpus_allowed_list:\t0-3\n"
	if err := os.WriteFile(path, []byte(status), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateProcfsPIDNamespace(path, 4242); err != nil {
		t.Fatal(err)
	}
}

func TestValidateProcfsPIDNamespaceRejectsMultipleNSpids(t *testing.T) {
	err := validateProcfsPIDNamespaceFixture(t, "NSpid:\t17\t4242\n", 4242)
	assertProcfsPIDNamespaceError(t, err, 4242, []ThreadId{17, 4242})
}

func TestValidateProcfsPIDNamespaceRejectsMismatchedNSpid(t *testing.T) {
	err := validateProcfsPIDNamespaceFixture(t, "NSpid:\t17\n", 4242)
	assertProcfsPIDNamespaceError(t, err, 4242, []ThreadId{17})
}

func TestValidateProcfsPIDNamespaceRejectsMissingNSpid(t *testing.T) {
	err := validateProcfsPIDNamespaceFixture(t, "Name:\tfixture\nState:\tR (running)\n", 4242)
	assertProcfsPIDNamespaceError(t, err, 4242, nil)
}

func TestValidateProcfsPIDNamespaceReportsPermissionError(t *testing.T) {
	err := validateProcfsPIDNamespaceRead("status", 4242, func(string) ([]byte, error) {
		return nil, syscall.EACCES
	})
	if !errors.Is(err, syscall.EACCES) {
		t.Fatalf("permission error = %v, want EACCES", err)
	}
}

func validateProcfsPIDNamespaceFixture(t *testing.T, status string, expected ThreadId) error {
	t.Helper()
	path := filepath.Join(t.TempDir(), "status")
	if err := os.WriteFile(path, []byte(status), 0o600); err != nil {
		t.Fatal(err)
	}
	return validateProcfsPIDNamespace(path, expected)
}

func assertProcfsPIDNamespaceError(t *testing.T, err error, expected ThreadId, nspids []ThreadId) {
	t.Helper()
	if !errors.Is(err, ErrProcfsPIDNamespace) {
		t.Fatalf("validation error = %v, want ErrProcfsPIDNamespace", err)
	}
	var mismatch *ProcfsPIDNamespaceError
	if !errors.As(err, &mismatch) {
		t.Fatalf("validation error type = %T, want *ProcfsPIDNamespaceError", err)
	}
	if mismatch.ThreadId != expected || !reflect.DeepEqual(mismatch.NSpids, nspids) {
		t.Fatalf("validation diagnostics = %+v, want thread ID %d and NSpids %v", mismatch, expected, nspids)
	}
}

func TestCurrentPIDNamespaceIdentityMatchesProcfsStat(t *testing.T) {
	first, err := CurrentPIDNamespaceIdentity()
	if err != nil {
		t.Fatal(err)
	}
	second, err := CurrentPIDNamespaceIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if first != second || first == (PIDNamespaceIdentity{}) {
		t.Fatalf("PID namespace identities = %+v and %+v", first, second)
	}
	var stat syscall.Stat_t
	if err := syscall.Stat("/proc/self/ns/pid", &stat); err != nil {
		t.Fatal(err)
	}
	want := PIDNamespaceIdentity{Dev: uint64(stat.Dev), Ino: stat.Ino}
	if first != want {
		t.Fatalf("PID namespace identity = %+v, want %+v", first, want)
	}
}

func TestCurrentPIDNamespaceIdentityPropagatesStatError(t *testing.T) {
	_, err := pidNamespaceIdentityAt(filepath.Join(t.TempDir(), "missing"))
	if !errors.Is(err, syscall.ENOENT) {
		t.Fatalf("namespace stat error = %v, want ENOENT", err)
	}
}

func TestThreadAliveTreatsProcStatENOENTAsDeathBeforeTIDReuse(t *testing.T) {
	oldScheduler := schedulerDeadProbe
	oldOpen := openThreadStat
	defer func() {
		schedulerDeadProbe = oldScheduler
		openThreadStat = oldOpen
	}()

	calls := 0
	schedulerDeadProbe = func(ThreadId) bool {
		calls++
		return false
	}
	openThreadStat = func(string, int, uint32) (int, error) {
		return -1, syscall.ENOENT
	}
	if ThreadAlive(123) {
		t.Fatal("ENOENT remained live after the recorded owner disappeared")
	}
	if calls != 1 {
		t.Fatalf("scheduler probes = %d, want 1", calls)
	}
}

func TestThreadAliveReportsReapedProcessDead(t *testing.T) {
	cmd := exec.Command("/bin/true")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	tid := ThreadId(cmd.Process.Pid)
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if ThreadAlive(tid) {
		t.Fatalf("reaped thread %d is alive", tid)
	}
}

func TestThreadAliveRejectsZero(t *testing.T) {
	if ThreadAlive(0) {
		t.Fatal("zero thread ID is alive")
	}
}

func TestCurrentThreadIdMatchesSyscallAcrossLockedThreads(t *testing.T) {
	const threadCount = 64
	const checksPerThread = 256
	errs := make(chan error, threadCount)
	for range threadCount {
		go func() {
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()
			for range checksPerThread {
				fast := CurrentThreadId()
				kernel := currentThreadIdSyscall()
				if fast != kernel {
					errs <- fmt.Errorf("fast thread ID = %d, syscall thread ID = %d", fast, kernel)
					return
				}
			}
			errs <- nil
		}()
	}
	for range threadCount {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
}

var benchmarkThreadId ThreadId
var benchmarkThreadIdentity ThreadIdentity

func BenchmarkCurrentThreadIdentity(b *testing.B) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	b.ReportAllocs()
	for b.Loop() {
		benchmarkThreadIdentity = CurrentThreadIdentity()
	}
}

func BenchmarkCurrentThreadIdFast(b *testing.B) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	b.ReportAllocs()
	for b.Loop() {
		benchmarkThreadId = CurrentThreadId()
	}
}

func BenchmarkCurrentThreadIdSyscall(b *testing.B) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	b.ReportAllocs()
	for b.Loop() {
		benchmarkThreadId = currentThreadIdSyscall()
	}
}

func TestCurrentThreadIdMatchesRegistration(t *testing.T) {
	var registry ThreadRegistry
	registration, err := registry.RegisterCurrentThread()
	if err != nil {
		t.Fatal(err)
	}
	if got := CurrentThreadId(); got == 0 || got != registration.Tid() {
		t.Fatalf("CurrentThreadId() = %d, registration Tid = %d", got, registration.Tid())
	}
	if err := registration.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestThreadRegistrationRejectsWrongThreadClose(t *testing.T) {
	var registry ThreadRegistry
	registration, err := registry.RegisterCurrentThread()
	if err != nil {
		t.Fatal(err)
	}
	wrongThreadErr := make(chan error, 1)
	go func() { wrongThreadErr <- registration.Close() }()
	if err := <-wrongThreadErr; err == nil {
		t.Fatal("Close succeeded from a different thread")
	}
	if err := registration.Close(); err != nil {
		t.Fatalf("Close on the registered thread failed: %v", err)
	}
}

func TestRegisterCurrentThreadDuringActivePolicy(t *testing.T) {
	allowed, err := Affinity(0)
	if err != nil {
		t.Skipf("affinity is unavailable: %v", err)
	}
	cpus := allowed.CPUs()
	if len(cpus) == 0 {
		t.Skip("the affinity is empty")
	}
	one, _ := NewCPUSet(cpus[0])
	var registry ThreadRegistry
	rollback, err := (ProcessPolicy{Registry: &registry, CPUs: one}).Apply()
	if err != nil {
		t.Fatal(err)
	}
	type registeredThread struct {
		registration *ThreadRegistration
		affinity     CPUSet
		err          error
	}
	registered := make(chan registeredThread, 1)
	finish := make(chan CPUSet, 1)
	finished := make(chan error, 1)
	go func() {
		registration, err := registry.RegisterCurrentThread()
		if err != nil {
			registered <- registeredThread{err: err}
			return
		}
		set, affinityErr := Affinity(0)
		registered <- registeredThread{registration: registration, affinity: set, err: affinityErr}
		expected := <-finish
		restored, restoreErr := Affinity(0)
		if restoreErr == nil && !restored.Equal(expected) {
			restoreErr = fmt.Errorf("restored affinity = %s, want %s", restored, expected)
		}
		if closeErr := registration.Close(); closeErr != nil {
			restoreErr = errors.Join(restoreErr, closeErr)
		}
		finished <- restoreErr
	}()
	result := <-registered
	if result.err != nil {
		t.Fatal(result.err)
	}
	if !result.affinity.Equal(one) {
		t.Fatalf("new thread affinity = %s, want %s", result.affinity, one)
	}
	registry.mu.Lock()
	original, exists := registry.active.originals[result.registration]
	registry.mu.Unlock()
	if !exists {
		t.Fatal("the active policy did not save the new registration affinity")
	}
	if err := rollback(); err != nil {
		t.Fatal(err)
	}
	finish <- original
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
}

func TestCPUSetContainsAndConstructionErrors(t *testing.T) {
	set, err := NewCPUSet(0, 63, 64, 130)
	if err != nil {
		t.Fatal(err)
	}
	for _, cpu := range []int{0, 63, 64, 130} {
		if !set.Contains(cpu) {
			t.Errorf("Contains(%d) = false", cpu)
		}
	}
	for _, cpu := range []int{-1, 1, 129, 131} {
		if set.Contains(cpu) {
			t.Errorf("Contains(%d) = true", cpu)
		}
	}
	if _, err := NewCPUSet(-1); err == nil {
		t.Fatal("NewCPUSet(-1) succeeded")
	}
	if got := set.Intersection(CPUSet{}).String(); got != "" {
		t.Fatalf("intersection with an empty set = %q", got)
	}
}

func TestAffinityRejectsInvalidArguments(t *testing.T) {
	if _, err := Affinity(ThreadId(-1)); err == nil {
		t.Fatal("Affinity(-1) succeeded")
	}
	if err := SetAffinity(0, CPUSet{}); err == nil {
		t.Fatal("SetAffinity accepted an empty CPU set")
	}
	tooLarge, err := NewCPUSet(affinityMaskBytes * 8)
	if err != nil {
		t.Fatal(err)
	}
	if err := SetAffinity(0, tooLarge); err == nil {
		t.Fatal("SetAffinity accepted a CPU beyond its mask capacity")
	}
	one, err := NewCPUSet(0)
	if err != nil {
		t.Fatal(err)
	}
	if err := SetAffinity(ThreadId(-1), one); err == nil {
		t.Fatal("SetAffinity(-1) succeeded")
	}
	if err := WithAffinity(one, nil); err == nil {
		t.Fatal("WithAffinity accepted a nil callback")
	}
}

func TestThreadRegistrationValidation(t *testing.T) {
	var nilRegistry *ThreadRegistry
	if _, err := nilRegistry.RegisterCurrentThread(); err == nil {
		t.Fatal("registration with a nil registry succeeded")
	}
	var nilRegistration *ThreadRegistration
	if got := nilRegistration.Tid(); got != 0 {
		t.Fatalf("nil registration Tid = %d", got)
	}
	if err := nilRegistration.Close(); err != nil {
		t.Fatalf("nil registration Close = %v", err)
	}

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	var registry ThreadRegistry
	registration, err := registry.RegisterCurrentThread()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.RegisterCurrentThread(); err == nil {
		t.Fatal("duplicate thread registration succeeded")
	}
	if err := registration.Close(); err != nil {
		t.Fatal(err)
	}
	if err := registration.Close(); err == nil {
		t.Fatal("closing a registration twice succeeded")
	}
}

func TestProcessPolicyValidationAndActiveClose(t *testing.T) {
	one, err := NewCPUSet(0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (ProcessPolicy{CPUs: one}).Apply(); err == nil {
		t.Fatal("policy without a registry succeeded")
	}
	var emptyRegistry ThreadRegistry
	if _, err := (ProcessPolicy{Registry: &emptyRegistry}).Apply(); err == nil {
		t.Fatal("policy with an empty CPU set succeeded")
	}

	allowed, err := Affinity(0)
	if err != nil {
		t.Skipf("affinity is unavailable: %v", err)
	}
	cpus := allowed.CPUs()
	if len(cpus) == 0 {
		t.Skip("the affinity is empty")
	}
	one, _ = NewCPUSet(cpus[0])
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	var registry ThreadRegistry
	registration, err := registry.RegisterCurrentThread()
	if err != nil {
		t.Fatal(err)
	}
	rollback, err := (ProcessPolicy{Registry: &registry, CPUs: one}).Apply()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (ProcessPolicy{Registry: &registry, CPUs: one}).Apply(); err == nil {
		t.Fatal("a second active policy succeeded")
	}
	if err := registration.Close(); err != nil {
		t.Fatal(err)
	}
	if len(registry.threads) != 0 || len(registry.active.originals) != 0 {
		t.Fatal("registry retained a closed registration")
	}
	if err := rollback(); err != nil {
		t.Fatal(err)
	}
}

func TestEffectiveCPUSetFallbacks(t *testing.T) {
	t.Run("unified fallback", func(t *testing.T) {
		root := t.TempDir()
		writeFixture(t, root, "proc/42/cgroup", "0::/service\n")
		writeFixture(t, root, "cgroup/service/cpuset.cpus", "2,4\n")
		set, err := (FS{ProcfsRoot: filepath.Join(root, "proc"), CgroupRoot: filepath.Join(root, "cgroup")}).EffectiveCPUSet(42)
		if err != nil {
			t.Fatal(err)
		}
		if got := set.String(); got != "2,4" {
			t.Fatalf("effective set = %q", got)
		}
	})

	t.Run("legacy root and fallback", func(t *testing.T) {
		root := t.TempDir()
		writeFixture(t, root, "proc/42/cgroup", "4:cpuset:/jobs/a\n")
		writeFixture(t, root, "cgroup/jobs/a/cpuset.cpus", "3-5\n")
		set, err := (FS{ProcfsRoot: filepath.Join(root, "proc"), CgroupRoot: filepath.Join(root, "cgroup")}).EffectiveCPUSet(42)
		if err != nil {
			t.Fatal(err)
		}
		if got := set.String(); got != "3-5" {
			t.Fatalf("effective set = %q", got)
		}
	})
}

func TestEffectiveCPUSetErrors(t *testing.T) {
	tests := []struct {
		name   string
		line   string
		create bool
	}{
		{name: "missing process file"},
		{name: "malformed line", line: "malformed\n", create: true},
		{name: "no cpuset controller", line: "5:memory:/x\n", create: true},
		{name: "missing cpuset files", line: "0::/service\n", create: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			if test.create {
				writeFixture(t, root, "proc/42/cgroup", test.line)
			}
			if _, err := (FS{ProcfsRoot: filepath.Join(root, "proc"), CgroupRoot: filepath.Join(root, "cgroup")}).EffectiveCPUSet(42); err == nil {
				t.Fatal("EffectiveCPUSet succeeded")
			}
		})
	}

	root := t.TempDir()
	writeFixture(t, root, "proc/42/cgroup", "0::/service\n")
	writeFixture(t, root, "cgroup/service/cpuset.cpus.effective", "invalid\n")
	writeFixture(t, root, "cgroup/service/cpuset.cpus", "1-2\n")
	if _, err := (FS{ProcfsRoot: filepath.Join(root, "proc"), CgroupRoot: filepath.Join(root, "cgroup")}).EffectiveCPUSet(42); err == nil {
		t.Fatal("EffectiveCPUSet ignored an invalid effective cpuset")
	}
}

func TestCgroupPathStaysBelowRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "cgroup")
	if got, want := cgroupPath(root, "../../service"), filepath.Join(root, "service"); got != want {
		t.Fatalf("cgroupPath = %q, want %q", got, want)
	}
}

func TestReadSnapshotOptionalFilesAndNUMAFallbacks(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "sys/devices/system/cpu/online", "0\n")
	writeFixture(t, root, "sys/devices/system/cpu/possible", "0\n")
	writeFixture(t, root, "sys/devices/system/cpu/present", "0\n")
	writeFixture(t, root, "proc/cmdline", "quiet\n")
	writeFixture(t, root, "sys/devices/system/cpu/cpu0/topology/core_id", "7\n")
	writeFixture(t, root, "sys/devices/system/cpu/cpu0/topology/physical_package_id", "2\n")
	writeFixture(t, root, "sys/devices/system/cpu/cpu0/topology/thread_siblings_list", "0\n")
	if err := os.MkdirAll(filepath.Join(root, "sys/devices/system/cpu/cpu0/node1x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "sys/devices/system/node/node2x"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, root, "sys/devices/system/node/node3/cpulist", "1\n")

	snapshot, err := (FS{SysfsRoot: filepath.Join(root, "sys"), ProcfsRoot: filepath.Join(root, "proc")}).ReadSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.Isolated.Empty() || !snapshot.NoHZFull.Empty() || !snapshot.RCUNocbs.Empty() {
		t.Fatalf("optional CPU sets are not empty: %+v", snapshot)
	}
	if len(snapshot.CPUs) != 1 || snapshot.CPUs[0].NodeId != -1 {
		t.Fatalf("CPUs = %+v", snapshot.CPUs)
	}
	if len(snapshot.Nodes) != 1 || snapshot.Nodes[0].Id != 3 {
		t.Fatalf("nodes = %+v", snapshot.Nodes)
	}
}

func TestReadSnapshotReportsFixtureErrors(t *testing.T) {
	tests := []struct {
		name string
		path string
		data string
	}{
		{name: "online", path: "sys/devices/system/cpu/online", data: "invalid\n"},
		{name: "possible", path: "sys/devices/system/cpu/possible", data: "invalid\n"},
		{name: "present", path: "sys/devices/system/cpu/present", data: "invalid\n"},
		{name: "isolated", path: "sys/devices/system/cpu/isolated", data: "invalid\n"},
		{name: "nohz full", path: "sys/devices/system/cpu/nohz_full", data: "invalid\n"},
		{name: "kernel parameter", path: "proc/cmdline", data: "rcu_nocbs=invalid\n"},
		{name: "core id", path: "sys/devices/system/cpu/cpu0/topology/core_id", data: "invalid\n"},
		{name: "package id", path: "sys/devices/system/cpu/cpu0/topology/physical_package_id", data: "invalid\n"},
		{name: "siblings", path: "sys/devices/system/cpu/cpu0/topology/thread_siblings_list", data: "invalid\n"},
		{name: "node cpulist", path: "sys/devices/system/node/node0/cpulist", data: "invalid\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			writeMinimalSnapshotFixture(t, root)
			writeFixture(t, root, test.path, test.data)
			if _, err := (FS{SysfsRoot: filepath.Join(root, "sys"), ProcfsRoot: filepath.Join(root, "proc")}).ReadSnapshot(); err == nil {
				t.Fatal("ReadSnapshot succeeded")
			}
		})
	}
}

func writeMinimalSnapshotFixture(t *testing.T, root string) {
	t.Helper()
	writeFixture(t, root, "sys/devices/system/cpu/online", "0\n")
	writeFixture(t, root, "sys/devices/system/cpu/possible", "0\n")
	writeFixture(t, root, "sys/devices/system/cpu/present", "0\n")
	writeFixture(t, root, "sys/devices/system/cpu/isolated", "\n")
	writeFixture(t, root, "sys/devices/system/cpu/nohz_full", "\n")
	writeFixture(t, root, "proc/cmdline", "quiet\n")
	writeFixture(t, root, "sys/devices/system/cpu/cpu0/topology/core_id", "0\n")
	writeFixture(t, root, "sys/devices/system/cpu/cpu0/topology/physical_package_id", "0\n")
	writeFixture(t, root, "sys/devices/system/cpu/cpu0/topology/thread_siblings_list", "0\n")
	writeFixture(t, root, "sys/devices/system/node/node0/cpulist", "0\n")
}

func writeFixture(t *testing.T, root, relative, content string) {
	t.Helper()
	path := filepath.Join(root, relative)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
