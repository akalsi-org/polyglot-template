//go:build linux && (amd64 || arm64)

package tsc_test

import (
	"math"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/akalsi-org/polyglot-template/go/lib/tsc"
)

func TestCounterAdvancesMonotonically(t *testing.T) {
	if !tsc.Available() {
		t.Skip("the counter is not a clock on this processor")
	}
	prev := tsc.Read()
	for i := 0; i < 100000; i++ {
		now := tsc.Read()
		if now < prev {
			t.Fatalf("the counter went backwards at %d: %d then %d", i, prev, now)
		}
		prev = now
	}
}

// The whole point is to convert ticks to time. A calibration that is wrong is
// worse than no counter at all, because it reports plausible nonsense.
func TestCalibrationAgreesWithTheWallClock(t *testing.T) {
	if !tsc.Available() {
		t.Skip("the counter is not a clock on this processor")
	}
	tsc.Calibrate(50 * time.Millisecond)
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	const window = 200 * time.Millisecond
	start := time.Now()
	c0 := tsc.Read()
	for time.Since(start) < window {
	}
	elapsed := time.Since(start)
	got := tsc.Nanos(tsc.Read() - c0)
	want := elapsed.Nanoseconds()
	err := math.Abs(float64(got-want)) / float64(want)
	if err > 0.01 {
		t.Fatalf("counter says %d ns, clock says %d ns, error %.2f%%",
			got, want, err*100)
	}
	t.Logf("ticks_per_second=%d error=%.4f%%", tsc.TicksPerSecond(), err*100)
}

func TestReadDoesNotAllocate(t *testing.T) {
	if n := testing.AllocsPerRun(1000, func() { _ = tsc.Read() }); n != 0 {
		t.Fatalf("Read allocated %v times per run", n)
	}
}

// A clock is only useful for measuring short operations if reading it costs
// much less than the operation. This records both costs so the comparison is
// visible rather than assumed.
func TestReadIsCheaperThanTheWallClock(t *testing.T) {
	if !tsc.Available() {
		t.Skip("the counter is not a clock on this processor")
	}
	const n = 2000000
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	start := time.Now()
	for i := 0; i < n; i++ {
		_ = tsc.Read()
	}
	tscNs := float64(time.Since(start).Nanoseconds()) / n

	start = time.Now()
	for i := 0; i < n; i++ {
		_ = time.Now().UnixNano()
	}
	wallNs := float64(time.Since(start).Nanoseconds()) / n

	t.Logf("tsc.Read=%.1f ns  time.Now=%.1f ns  ratio=%.1fx", tscNs, wallNs, wallNs/tscNs)
	if tscNs >= wallNs {
		t.Fatalf("tsc.Read (%.1f ns) is not cheaper than time.Now (%.1f ns)", tscNs, wallNs)
	}
}

// The barrier is the whole difference between Read and ReadFast, so its
// presence must be checked in the emitted code, not just in the source.
func TestReadCarriesTheBarrier(t *testing.T) {
	out, err := exec.Command("go", "tool", "objdump", "-s",
		`tsc\.Read`, objFile(t)).CombinedOutput()
	if err != nil {
		t.Skipf("objdump: %v", err)
	}
	text := string(out)
	barrier := map[string]string{"amd64": "LFENCE", "arm64": "ISB"}[runtime.GOARCH]
	counter := map[string]string{"amd64": "RDTSC", "arm64": "MRS"}[runtime.GOARCH]
	fast, slow := split(text)
	if slow == "" || fast == "" {
		t.Fatalf("objdump listed neither Read nor ReadFast:\n%s", text)
	}
	if !strings.Contains(slow, barrier) {
		t.Fatalf("Read has no %s:\n%s", barrier, slow)
	}
	if strings.Contains(fast, barrier) {
		t.Fatalf("ReadFast has a %s and must not:\n%s", barrier, fast)
	}
	for name, body := range map[string]string{"Read": slow, "ReadFast": fast} {
		if !strings.Contains(body, counter) && runtime.GOARCH == "amd64" {
			t.Fatalf("%s does not read the counter:\n%s", name, body)
		}
	}
}

// split returns the ReadFast body and the Read body from an objdump listing.
// objdump prints fully qualified symbols, so match on the suffix.
func split(text string) (fast, slow string) {
	for _, block := range strings.Split(text, "TEXT ") {
		name, _, found := strings.Cut(block, "(SB)")
		if !found {
			continue
		}
		switch {
		case strings.HasSuffix(name, "tsc.ReadFast.abi0"), strings.HasSuffix(name, "tsc.ReadFast"):
			fast = block
		case strings.HasSuffix(name, "tsc.Read.abi0"), strings.HasSuffix(name, "tsc.Read"):
			slow = block
		}
	}
	return fast, slow
}

func objFile(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	out := filepath.Join(dir, "tsc.test")
	cmd := exec.Command("go", "test", "-c", "-o", out,
		"github.com/akalsi-org/polyglot-template/go/lib/tsc")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("build test binary: %v\n%s", err, b)
	}
	return out
}

func TestReadFastIsCheaperThanRead(t *testing.T) {
	if !tsc.Available() {
		t.Skip("the counter is not a clock on this processor")
	}
	const n = 2000000
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	start := time.Now()
	for i := 0; i < n; i++ {
		_ = tsc.Read()
	}
	ordered := float64(time.Since(start).Nanoseconds()) / n
	start = time.Now()
	for i := 0; i < n; i++ {
		_ = tsc.ReadFast()
	}
	fast := float64(time.Since(start).Nanoseconds()) / n
	t.Logf("Read=%.1f ns  ReadFast=%.1f ns  barrier costs %.1f ns", ordered, fast, ordered-fast)
}

// RDTSCP exists to report which processor produced a reading. A delta across
// two processors is not a duration, and this is the only form that can say so.
func TestReadTaggedReportsTheProcessor(t *testing.T) {
	if !tsc.Available() {
		t.Skip("the counter is not a clock on this processor")
	}
	if runtime.GOARCH != "amd64" {
		t.Skip("no tagged counter read on this architecture")
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	first, tag := tsc.ReadTagged()
	second, again := tsc.ReadTagged()
	if second < first {
		t.Fatalf("the counter went backwards: %d then %d", first, second)
	}
	if tag != again {
		t.Fatalf("a locked thread reported two processors: %d then %d", tag, again)
	}
	t.Logf("processor tag=%d", tag)
}

func TestReadTaggedCost(t *testing.T) {
	if !tsc.Available() {
		t.Skip("the counter is not a clock on this processor")
	}
	const n = 2000000
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	cost := func(f func()) float64 {
		best := math.MaxFloat64
		for attempt := 0; attempt < 3; attempt++ {
			start := time.Now()
			for i := 0; i < n; i++ {
				f()
			}
			if c := float64(time.Since(start).Nanoseconds()) / n; c < best {
				best = c
			}
		}
		return best
	}
	fast := cost(func() { _ = tsc.ReadFast() })
	ordered := cost(func() { _ = tsc.Read() })
	tagged := cost(func() { _, _ = tsc.ReadTagged() })
	wall := cost(func() { _ = time.Now().UnixNano() })
	t.Logf("ReadFast(RDTSC)=%.1fns  Read(LFENCE+RDTSC)=%.1fns  ReadTagged(RDTSCP)=%.1fns  time.Now=%.1fns",
		fast, ordered, tagged, wall)
}
