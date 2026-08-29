//go:build linux && (amd64 || arm64)

package atmc

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type atomicInteger interface {
	~uint32 | ~uint64 | ~int32 | ~int64 | ~uintptr
}

type atomicOperations[T atomicInteger] struct {
	loadRelaxed  func(*T) T
	loadAcquire  func(*T) T
	storeRelaxed func(*T, T)
	storeRelease func(*T, T)
	casRelaxed   func(*T, T, T) bool
	casAcquire   func(*T, T, T) bool
	casRelease   func(*T, T, T) bool
	casAcqRel    func(*T, T, T) bool
	swapAcqRel   func(*T, T) T
	addRelaxed   func(*T, T) T
	addAcqRel    func(*T, T) T
	andAcqRel    func(*T, T) T
	orAcqRel     func(*T, T) T
}

func TestOperations(t *testing.T) {
	maxI32 := int32(^uint32(0) >> 1)
	maxI64 := int64(^uint64(0) >> 1)
	runOperationTests(t, "U32", []uint32{0, 1, ^uint32(0)}, atomicOperations[uint32]{
		LoadRelaxedU32, LoadAcquireU32, StoreRelaxedU32, StoreReleaseU32,
		CompareAndSwapRelaxedU32, CompareAndSwapAcquireU32,
		CompareAndSwapReleaseU32, CompareAndSwapAcqRelU32,
		SwapAcqRelU32, AddRelaxedU32, AddAcqRelU32, AndAcqRelU32, OrAcqRelU32,
	})
	runOperationTests(t, "U64", []uint64{0, 1, ^uint64(0)}, atomicOperations[uint64]{
		LoadRelaxedU64, LoadAcquireU64, StoreRelaxedU64, StoreReleaseU64,
		CompareAndSwapRelaxedU64, CompareAndSwapAcquireU64,
		CompareAndSwapReleaseU64, CompareAndSwapAcqRelU64,
		SwapAcqRelU64, AddRelaxedU64, AddAcqRelU64, AndAcqRelU64, OrAcqRelU64,
	})
	runOperationTests(t, "I32", []int32{0, 1, maxI32, -maxI32 - 1, -1}, atomicOperations[int32]{
		LoadRelaxedI32, LoadAcquireI32, StoreRelaxedI32, StoreReleaseI32,
		CompareAndSwapRelaxedI32, CompareAndSwapAcquireI32,
		CompareAndSwapReleaseI32, CompareAndSwapAcqRelI32,
		SwapAcqRelI32, AddRelaxedI32, AddAcqRelI32, AndAcqRelI32, OrAcqRelI32,
	})
	runOperationTests(t, "I64", []int64{0, 1, maxI64, -maxI64 - 1, -1}, atomicOperations[int64]{
		LoadRelaxedI64, LoadAcquireI64, StoreRelaxedI64, StoreReleaseI64,
		CompareAndSwapRelaxedI64, CompareAndSwapAcquireI64,
		CompareAndSwapReleaseI64, CompareAndSwapAcqRelI64,
		SwapAcqRelI64, AddRelaxedI64, AddAcqRelI64, AndAcqRelI64, OrAcqRelI64,
	})
	runOperationTests(t, "Uintptr", []uintptr{0, 1, ^uintptr(0)}, atomicOperations[uintptr]{
		LoadRelaxedUintptr, LoadAcquireUintptr, StoreRelaxedUintptr, StoreReleaseUintptr,
		CompareAndSwapRelaxedUintptr, CompareAndSwapAcquireUintptr,
		CompareAndSwapReleaseUintptr, CompareAndSwapAcqRelUintptr,
		SwapAcqRelUintptr, AddRelaxedUintptr, AddAcqRelUintptr,
		AndAcqRelUintptr, OrAcqRelUintptr,
	})
}

func runOperationTests[T atomicInteger](
	t *testing.T,
	name string,
	values []T,
	operations atomicOperations[T],
) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		var target T
		for index, value := range values {
			next := values[(index+1)%len(values)]

			operations.storeRelaxed(&target, value)
			if got := operations.loadRelaxed(&target); got != value {
				t.Fatalf("relaxed round trip for %v: got %v", value, got)
			}
			operations.storeRelease(&target, value)
			if got := operations.loadAcquire(&target); got != value {
				t.Fatalf("release/acquire round trip for %v: got %v", value, got)
			}

			for casName, cas := range map[string]func(*T, T, T) bool{
				"Relaxed": casFunction(operations.casRelaxed),
				"Acquire": casFunction(operations.casAcquire),
				"Release": casFunction(operations.casRelease),
				"AcqRel":  casFunction(operations.casAcqRel),
			} {
				operations.storeRelaxed(&target, value)
				if !cas(&target, value, next) {
					t.Fatalf("%s compare-and-swap rejected %v", casName, value)
				}
				if got := operations.loadRelaxed(&target); got != next {
					t.Fatalf("%s compare-and-swap stored %v, want %v", casName, got, next)
				}
				if cas(&target, value, next) {
					t.Fatalf("%s compare-and-swap accepted stale value %v", casName, value)
				}
			}

			operations.storeRelaxed(&target, 0)
			if old := operations.swapAcqRel(&target, value); old != 0 {
				t.Fatalf("swap returned %v, want 0", old)
			}
			if got := operations.loadRelaxed(&target); got != value {
				t.Fatalf("swap stored %v, want %v", got, value)
			}

			operations.storeRelaxed(&target, 0)
			if got := operations.addRelaxed(&target, value); got != value {
				t.Fatalf("relaxed add returned %v, want %v", got, value)
			}
			operations.storeRelaxed(&target, 0)
			if got := operations.addAcqRel(&target, value); got != value {
				t.Fatalf("acquire-release add returned %v, want %v", got, value)
			}

			allBits := ^T(0)
			operations.storeRelaxed(&target, allBits)
			if old := operations.andAcqRel(&target, value); old != allBits {
				t.Fatalf("and returned %v, want %v", old, allBits)
			}
			if got := operations.loadRelaxed(&target); got != value {
				t.Fatalf("and stored %v, want %v", got, value)
			}
			operations.storeRelaxed(&target, 0)
			if old := operations.orAcqRel(&target, value); old != 0 {
				t.Fatalf("or returned %v, want 0", old)
			}
			if got := operations.loadRelaxed(&target); got != value {
				t.Fatalf("or stored %v, want %v", got, value)
			}
		}
	})
}

func casFunction[T atomicInteger](function func(*T, T, T) bool) func(*T, T, T) bool {
	return function
}

func TestAllocations(t *testing.T) {
	testAllocations(t, "U32", atomicOperations[uint32]{
		LoadRelaxedU32, LoadAcquireU32, StoreRelaxedU32, StoreReleaseU32,
		CompareAndSwapRelaxedU32, CompareAndSwapAcquireU32,
		CompareAndSwapReleaseU32, CompareAndSwapAcqRelU32,
		SwapAcqRelU32, AddRelaxedU32, AddAcqRelU32, AndAcqRelU32, OrAcqRelU32,
	})
	testAllocations(t, "U64", atomicOperations[uint64]{
		LoadRelaxedU64, LoadAcquireU64, StoreRelaxedU64, StoreReleaseU64,
		CompareAndSwapRelaxedU64, CompareAndSwapAcquireU64,
		CompareAndSwapReleaseU64, CompareAndSwapAcqRelU64,
		SwapAcqRelU64, AddRelaxedU64, AddAcqRelU64, AndAcqRelU64, OrAcqRelU64,
	})
	testAllocations(t, "I32", atomicOperations[int32]{
		LoadRelaxedI32, LoadAcquireI32, StoreRelaxedI32, StoreReleaseI32,
		CompareAndSwapRelaxedI32, CompareAndSwapAcquireI32,
		CompareAndSwapReleaseI32, CompareAndSwapAcqRelI32,
		SwapAcqRelI32, AddRelaxedI32, AddAcqRelI32, AndAcqRelI32, OrAcqRelI32,
	})
	testAllocations(t, "I64", atomicOperations[int64]{
		LoadRelaxedI64, LoadAcquireI64, StoreRelaxedI64, StoreReleaseI64,
		CompareAndSwapRelaxedI64, CompareAndSwapAcquireI64,
		CompareAndSwapReleaseI64, CompareAndSwapAcqRelI64,
		SwapAcqRelI64, AddRelaxedI64, AddAcqRelI64, AndAcqRelI64, OrAcqRelI64,
	})
	testAllocations(t, "Uintptr", atomicOperations[uintptr]{
		LoadRelaxedUintptr, LoadAcquireUintptr, StoreRelaxedUintptr, StoreReleaseUintptr,
		CompareAndSwapRelaxedUintptr, CompareAndSwapAcquireUintptr,
		CompareAndSwapReleaseUintptr, CompareAndSwapAcqRelUintptr,
		SwapAcqRelUintptr, AddRelaxedUintptr, AddAcqRelUintptr,
		AndAcqRelUintptr, OrAcqRelUintptr,
	})
}

func testAllocations[T atomicInteger](t *testing.T, suffix string, operations atomicOperations[T]) {
	t.Helper()
	var target T
	assertZeroAllocations(t, "LoadRelaxed"+suffix, func() { operations.loadRelaxed(&target) })
	assertZeroAllocations(t, "LoadAcquire"+suffix, func() { operations.loadAcquire(&target) })
	assertZeroAllocations(t, "StoreRelaxed"+suffix, func() { operations.storeRelaxed(&target, 1) })
	assertZeroAllocations(t, "StoreRelease"+suffix, func() { operations.storeRelease(&target, 1) })
	assertZeroAllocations(t, "CompareAndSwapRelaxed"+suffix, func() { operations.casRelaxed(&target, 0, 1) })
	assertZeroAllocations(t, "CompareAndSwapAcquire"+suffix, func() { operations.casAcquire(&target, 0, 1) })
	assertZeroAllocations(t, "CompareAndSwapRelease"+suffix, func() { operations.casRelease(&target, 0, 1) })
	assertZeroAllocations(t, "CompareAndSwapAcqRel"+suffix, func() { operations.casAcqRel(&target, 0, 1) })
	assertZeroAllocations(t, "SwapAcqRel"+suffix, func() { operations.swapAcqRel(&target, 1) })
	assertZeroAllocations(t, "AddRelaxed"+suffix, func() { operations.addRelaxed(&target, 1) })
	assertZeroAllocations(t, "AddAcqRel"+suffix, func() { operations.addAcqRel(&target, 1) })
	assertZeroAllocations(t, "AndAcqRel"+suffix, func() { operations.andAcqRel(&target, 1) })
	assertZeroAllocations(t, "OrAcqRel"+suffix, func() { operations.orAcqRel(&target, 1) })
}

func assertZeroAllocations(t *testing.T, name string, function func()) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		if allocations := testing.AllocsPerRun(1000, function); allocations != 0 {
			t.Fatalf("allocations: got %g, want 0", allocations)
		}
	})
}

func TestPublishObserve(t *testing.T) {
	const iterations = 100000
	var payload [32]uint64
	var published uint32
	done := make(chan struct{})
	go func() {
		defer close(done)
		for sequence := uint64(1); sequence <= iterations; sequence++ {
			for LoadAcquireU32(&published) != 0 {
				Relax()
			}
			for index := range payload {
				payload[index] = sequence ^ uint64(index)*0x9e3779b97f4a7c15
			}
			StoreReleaseU32(&published, 1)
		}
	}()
	for sequence := uint64(1); sequence <= iterations; sequence++ {
		for LoadAcquireU32(&published) == 0 {
			Relax()
		}
		for index, got := range payload {
			want := sequence ^ uint64(index)*0x9e3779b97f4a7c15
			if got != want {
				t.Fatalf("sequence %d, payload %d: got %#x, want %#x", sequence, index, got, want)
			}
		}
		StoreReleaseU32(&published, 0)
	}
	<-done
}

var objectCodeValue uint64

//go:noinline
func objectLoadAcquire(ptr *uint64) uint64 {
	return LoadAcquireU64(ptr)
}

//go:noinline
func objectStoreRelease(ptr *uint64, value uint64) {
	StoreReleaseU64(ptr, value)
}

func TestObjectCode(t *testing.T) {
	objectStoreRelease(&objectCodeValue, 1)
	_ = objectLoadAcquire(&objectCodeValue)
	FullBarrier()

	goTool := filepath.Join(runtime.GOROOT(), "bin", "go")
	executable := filepath.Join(t.TempDir(), "atmc.test")
	output, err := exec.Command(goTool, "test", "-c", "-o", executable, ".").CombinedOutput()
	if err != nil {
		t.Fatalf("compile object-code fixture: %v\n%s", err, output)
	}
	objectCode := func(t *testing.T, pattern string) string {
		t.Helper()
		output, err := exec.Command(goTool, "tool", "objdump", "-s", pattern, executable).CombinedOutput()
		if err != nil {
			t.Fatalf("objdump %s: %v\n%s", pattern, err, output)
		}
		text := string(output)
		if !strings.Contains(text, "RET") {
			t.Fatalf("%s has no RET instruction:\n%s", pattern, text)
		}
		return text
	}

	load := objectCode(t, `atmc\.objectLoadAcquire$`)
	store := objectCode(t, `atmc\.objectStoreRelease$`)
	fullBarrier := objectCode(t, `atmc\.FullBarrier\.abi0$`)
	switch runtime.GOARCH {
	case "amd64":
		assertContainsInstruction(t, "LoadAcquireU64", load, "MOVQ")
		assertNoInstruction(t, "LoadAcquireU64", load, "FENCE", "LOCK", "XCHG")
		assertContainsInstruction(t, "StoreReleaseU64", store, "MOVQ")
		assertNoInstruction(t, "StoreReleaseU64", store, "FENCE", "LOCK", "XCHG")
		assertContainsInstruction(t, "FullBarrier", fullBarrier, "MFENCE")
	case "arm64":
		assertContainsInstruction(t, "LoadAcquireU64", load, "LDAR")
		assertContainsInstruction(t, "StoreReleaseU64", store, "STLR")
		assertContainsInstruction(t, "FullBarrier", fullBarrier, "DMB $11")
	default:
		t.Fatalf("unsupported architecture %q", runtime.GOARCH)
	}
}

func assertContainsInstruction(t *testing.T, name, objectCode, instruction string) {
	t.Helper()
	if !strings.Contains(objectCode, instruction) {
		t.Fatalf("%s has no %s instruction:\n%s", name, instruction, objectCode)
	}
}

func assertNoInstruction(t *testing.T, name, objectCode string, instructions ...string) {
	t.Helper()
	for _, instruction := range instructions {
		if strings.Contains(objectCode, instruction) {
			t.Fatalf("%s contains %s:\n%s", name, instruction, objectCode)
		}
	}
}

func TestTypedOperationsAreInlinable(t *testing.T) {
	goTool := filepath.Join(runtime.GOROOT(), "bin", "go")
	command := exec.Command(goTool, "build", "-gcflags=-m", ".")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("inspect inlining: %v\n%s", err, output)
	}
	text := string(output)
	for _, suffix := range []string{"U32", "U64", "I32", "I64", "Uintptr"} {
		for _, prefix := range []string{
			"LoadRelaxed", "LoadAcquire", "StoreRelaxed", "StoreRelease",
			"CompareAndSwapRelaxed", "CompareAndSwapAcquire",
			"CompareAndSwapRelease", "CompareAndSwapAcqRel", "SwapAcqRel",
			"AddRelaxed", "AddAcqRel", "AndAcqRel", "OrAcqRel",
		} {
			name := prefix + suffix
			if !strings.Contains(text, "can inline "+name) {
				t.Errorf("%s is not reported inlinable\ncompiler output:\n%s", name, text)
			}
		}
	}
}

func Example_ordering() {
	var payload uint64
	var ready uint32
	payload = 42
	StoreReleaseU32(&ready, 1)
	if LoadAcquireU32(&ready) != 0 {
		fmt.Println(payload)
	}
	// Output: 42
}
