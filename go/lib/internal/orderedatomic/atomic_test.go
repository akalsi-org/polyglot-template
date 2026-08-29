//go:build linux && (amd64 || arm64)

package orderedatomic

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestObjectCode(t *testing.T) {
	goTool := filepath.Join(runtime.GOROOT(), "bin", "go")
	executable := filepath.Join(t.TempDir(), "orderedatomic.test")
	compileOutput, err := exec.Command(goTool, "test", "-c", "-o", executable, ".").CombinedOutput()
	if err != nil {
		t.Fatalf("compile object-code fixture: %v\n%s", err, compileOutput)
	}
	objectCode := func(t *testing.T, name string) string {
		t.Helper()
		pattern := "orderedatomic." + name + ".abi0$"
		output, err := exec.Command(goTool, "tool", "objdump", "-s", pattern, executable).CombinedOutput()
		if err != nil {
			t.Fatalf("objdump %s: %v\n%s", name, err, output)
		}
		text := string(output)
		if !strings.Contains(text, "RET") {
			t.Fatalf("%s has no RET instruction:\n%s", name, text)
		}
		return text
	}

	t.Run("StoreBarrier", func(t *testing.T) {
		text := objectCode(t, "StoreBarrier")
		switch runtime.GOARCH {
		case "amd64":
			if strings.Contains(text, "SFENCE") || strings.Contains(text, "DMB") {
				t.Fatalf("amd64 StoreBarrier is not a TSO no-op:\n%s", text)
			}
		case "arm64":
			if !strings.Contains(text, "DMB $10") {
				t.Fatalf("arm64 StoreBarrier has no DMB ISHST instruction:\n%s", text)
			}
		default:
			t.Fatalf("unsupported architecture %q", runtime.GOARCH)
		}
	})

	t.Run("LoadBarrier", func(t *testing.T) {
		text := objectCode(t, "LoadBarrier")
		switch runtime.GOARCH {
		case "amd64":
			if strings.Contains(text, "LFENCE") || strings.Contains(text, "MFENCE") || strings.Contains(text, "DMB") {
				t.Fatalf("amd64 LoadBarrier is not a TSO no-op:\n%s", text)
			}
		case "arm64":
			if !strings.Contains(text, "DMB $9") {
				t.Fatalf("arm64 LoadBarrier has no DMB ISHLD instruction:\n%s", text)
			}
		default:
			t.Fatalf("unsupported architecture %q", runtime.GOARCH)
		}
	})

	t.Run("FillRelaxed64", func(t *testing.T) {
		text := objectCode(t, "FillRelaxed64")
		switch runtime.GOARCH {
		case "amd64":
			if !strings.Contains(text, "MOVQ DX, 0(AX)") {
				t.Fatalf("amd64 FillRelaxed64 has no ordinary store:\n%s", text)
			}
			if strings.Contains(text, "LOCK") || strings.Contains(text, "XCHG") || strings.Contains(text, "FENCE") {
				t.Fatalf("amd64 FillRelaxed64 contains ordered stores or fences:\n%s", text)
			}
		case "arm64":
			if !strings.Contains(text, "MOVD R2, (R0)") {
				t.Fatalf("arm64 FillRelaxed64 has no ordinary store:\n%s", text)
			}
			if strings.Contains(text, "STLR") || strings.Contains(text, "DMB") {
				t.Fatalf("arm64 FillRelaxed64 contains release stores or barriers:\n%s", text)
			}
		default:
			t.Fatalf("unsupported architecture %q", runtime.GOARCH)
		}
	})
}

func TestFillRelaxed64(t *testing.T) {
	const (
		sentinel = uint64(0x0123456789abcdef)
		filled   = uint64(0xfedcba9876543210)
	)
	values := [7]uint64{sentinel, sentinel, sentinel, sentinel, sentinel, sentinel, sentinel}
	FillRelaxed64(&values[2], 3, filled)
	want := [7]uint64{sentinel, sentinel, filled, filled, filled, sentinel, sentinel}
	if values != want {
		t.Fatalf("filled values: got %#x, want %#x", values, want)
	}
	FillRelaxed64(nil, 0, filled)
}

func TestFillRelaxed64Allocations(t *testing.T) {
	var values [64]uint64
	allocations := testing.AllocsPerRun(1000, func() {
		FillRelaxed64(&values[0], uintptr(len(values)), 1)
	})
	if allocations != 0 {
		t.Fatalf("FillRelaxed64 allocations: got %g, want 0", allocations)
	}
}

func TestStoreBarrierPublishesPriorStore(t *testing.T) {
	const iterations = 100000
	var prior uint64
	var published uint64
	var consumed uint64
	done := make(chan struct{})
	go func() {
		defer close(done)
		for sequence := uint64(1); sequence <= iterations; sequence++ {
			for LoadAcquire64(&consumed) != sequence-1 {
				runtime.Gosched()
			}
			StoreRelaxed64(&prior, sequence)
			StoreBarrier()
			StoreRelaxed64(&published, sequence)
		}
	}()
	for sequence := uint64(1); sequence <= iterations; sequence++ {
		for LoadAcquire64(&published) != sequence {
			runtime.Gosched()
		}
		if got := LoadRelaxed64(&prior); got != sequence {
			t.Fatalf("sequence %d: prior store is %d", sequence, got)
		}
		StoreRelease64(&consumed, sequence)
	}
	<-done
}

func TestLoadBarrierCall(t *testing.T) {
	LoadBarrier()
}

func BenchmarkFillRelaxed64(b *testing.B) {
	const count = 1024
	values := make([]uint64, count)
	b.Run("Bulk", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(count * 8)
		for range b.N {
			FillRelaxed64(&values[0], count, 1)
		}
		runtime.KeepAlive(values)
	})
	b.Run("PerElement", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(count * 8)
		for range b.N {
			for i := range values {
				StoreRelaxed64(&values[i], 1)
			}
		}
		runtime.KeepAlive(values)
	})
}

func BenchmarkLoadBarrier(b *testing.B) {
	b.ReportAllocs()
	for range b.N {
		LoadBarrier()
	}
}

func BenchmarkStoreBarrier(b *testing.B) {
	b.ReportAllocs()
	for range b.N {
		StoreBarrier()
	}
}
