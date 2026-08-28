//go:build linux && (amd64 || arm64)

package mpsc

import (
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"unsafe"
)

func TestSpinWait(t *testing.T) {
	SpinWait()
}

func TestOptionalClaimBackoffSchedule(t *testing.T) {
	want := []uint32{1, 2, 4, 8, 8, 8}
	for failures, pauses := range want {
		if got := claimBackoffPauses(uint32(failures)); got != pauses {
			t.Fatalf("prior failures %d: got %d pauses, want %d", failures, got, pauses)
		}
	}
}

func TestOrderedAtomicOperations(t *testing.T) {
	var word32 uint32
	storeRelaxed32(&word32, 11)
	if got := loadRelaxed32(&word32); got != 11 {
		t.Fatalf("relaxed uint32 load: got %d", got)
	}
	storeRelease32(&word32, 12)
	if got := loadAcquire32(&word32); got != 12 {
		t.Fatalf("acquire uint32 load: got %d", got)
	}
	if !compareAndSwap32(&word32, 12, 13) || compareAndSwap32(&word32, 12, 14) {
		t.Fatal("uint32 compare-and-swap result")
	}

	var word64 uint64
	storeRelaxed64(&word64, 21)
	if got := loadRelaxed64(&word64); got != 21 {
		t.Fatalf("relaxed uint64 load: got %d", got)
	}
	storeRelease64(&word64, 22)
	if got := loadAcquire64(&word64); got != 22 {
		t.Fatalf("acquire uint64 load: got %d", got)
	}
	if !compareAndSwap64(&word64, 22, 23) || compareAndSwap64(&word64, 22, 24) {
		t.Fatal("uint64 compare-and-swap result")
	}
	if !compareAndSwapAcquire64(&word64, 23, 24) || compareAndSwapAcquire64(&word64, 23, 25) {
		t.Fatal("uint64 acquire compare-and-swap result")
	}
	if old := fetchOrAcqRel64(&word64, 7); old != 24 || word64 != 31 {
		t.Fatalf("uint64 fetch-or: old=%d value=%d", old, word64)
	}
	if old := fetchAndRelease64(&word64, 15); old != 31 || word64 != 15 {
		t.Fatalf("uint64 fetch-and: old=%d value=%d", old, word64)
	}
	if old := fetchAddAcqRel32(&word32, 7); old != 13 || word32 != 20 {
		t.Fatalf("uint32 fetch-add: old=%d value=%d", old, word32)
	}
}

func TestReleaseAcquire32PublishesPayload(t *testing.T) {
	const iterations = 100000
	var payload uint32
	var published uint32
	var consumed uint32
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for sequence := uint32(1); sequence <= iterations; sequence++ {
			for loadAcquire32(&consumed) != sequence-1 {
				runtime.Gosched()
			}
			payload = sequence ^ 0xa5a5a5a5
			storeRelease32(&published, sequence)
		}
	}()
	go func() {
		defer wg.Done()
		for sequence := uint32(1); sequence <= iterations; sequence++ {
			for loadAcquire32(&published) != sequence {
				runtime.Gosched()
			}
			if got := payload; got != sequence^0xa5a5a5a5 {
				t.Errorf("sequence %d: payload %d", sequence, got)
				return
			}
			storeRelease32(&consumed, sequence)
		}
	}()
	wg.Wait()
}

func TestReleaseAcquirePublishesPayload(t *testing.T) {
	const iterations = 100000
	var payload uint64
	var published uint64
	var consumed uint64
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for sequence := uint64(1); sequence <= iterations; sequence++ {
			for loadAcquire64(&consumed) != sequence-1 {
				runtime.Gosched()
			}
			payload = sequence
			storeRelease64(&published, sequence)
		}
	}()
	go func() {
		defer wg.Done()
		for sequence := uint64(1); sequence <= iterations; sequence++ {
			for loadAcquire64(&published) != sequence {
				runtime.Gosched()
			}
			if got := payload; got != sequence {
				t.Errorf("sequence %d: payload %d", sequence, got)
				return
			}
			storeRelease64(&consumed, sequence)
		}
	}()
	wg.Wait()
}

func TestMappedAtomicPointersAreAligned(t *testing.T) {
	for _, layout := range []MPSCLayout{MPSCCompact, MPSCPadded64, MPSCPadded256} {
		q, err := CreateMPSC(Config{Capacity: 4096, MPSCLayout: layout})
		if err != nil {
			t.Fatal(err)
		}
		for _, ptr := range []*uint64{q.ptr64(controlSize), q.ptr64(controlSize + 128), q.claim(0)} {
			if uintptr(unsafe.Pointer(ptr))&7 != 0 {
				t.Fatalf("layout %d has unaligned uint64 pointer %p", layout, ptr)
			}
		}
		length, tag := q.result(0)
		for _, ptr := range []*uint64{length, tag} {
			if uintptr(unsafe.Pointer(ptr))&7 != 0 {
				t.Fatalf("layout %d has unaligned result pointer %p", layout, ptr)
			}
		}
		if uintptr(unsafe.Pointer(q.ptr32(28)))&3 != 0 {
			t.Fatalf("layout %d has unaligned uint32 pointer", layout)
		}
		if err := q.Close(); err != nil {
			t.Fatal(err)
		}
	}

	spsc, err := CreateSPSC(Config{Capacity: 4096})
	if err != nil {
		t.Fatal(err)
	}
	for _, ptr := range []*uint64{
		spsc.ptr64(controlSize),
		spsc.ptr64(controlSize + 128),
		spsc.ptr64(controlSize + shardSize),
		spsc.slot(0),
	} {
		if uintptr(unsafe.Pointer(ptr))&7 != 0 {
			t.Fatalf("SPSC has unaligned uint64 pointer %p", ptr)
		}
	}
	for _, ptr := range []*uint32{
		spsc.ptr32(28),
		spsc.ptr32(controlSize + shardSize + 8),
		spsc.ptr32(controlSize + shardSize + 12),
	} {
		if uintptr(unsafe.Pointer(ptr))&3 != 0 {
			t.Fatalf("SPSC has unaligned uint32 pointer %p", ptr)
		}
	}
	if err := spsc.Close(); err != nil {
		t.Fatal(err)
	}
}

var atomicBenchmarkSink64 uint64
var atomicBenchmarkSinkBool bool

func BenchmarkCPURelax(b *testing.B) {
	b.ReportAllocs()
	for range b.N {
		cpuRelax()
	}
}

func BenchmarkLoadRelaxed64(b *testing.B) {
	var value uint64
	b.ReportAllocs()
	for range b.N {
		atomicBenchmarkSink64 = loadRelaxed64(&value)
	}
}

func BenchmarkLoadSequential64(b *testing.B) {
	var value uint64
	b.ReportAllocs()
	for range b.N {
		atomicBenchmarkSink64 = atomic.LoadUint64(&value)
	}
}

func BenchmarkStoreRelease64(b *testing.B) {
	var value uint64
	b.ReportAllocs()
	for i := range b.N {
		storeRelease64(&value, uint64(i))
	}
	atomicBenchmarkSink64 = value
}

func BenchmarkStoreSequential64(b *testing.B) {
	var value uint64
	b.ReportAllocs()
	for i := range b.N {
		atomic.StoreUint64(&value, uint64(i))
	}
	atomicBenchmarkSink64 = value
}

func BenchmarkCompareAndSwap64(b *testing.B) {
	var value uint64
	b.ReportAllocs()
	for i := range b.N {
		old := uint64(i & 1)
		atomicBenchmarkSinkBool = compareAndSwap64(&value, old, old^1)
	}
}
