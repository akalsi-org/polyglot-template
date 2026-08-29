//go:build linux && (amd64 || arm64)

package uring_test

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/akalsi-org/polyglot-template/go/lib/uring"
)

func newRing(t *testing.T, entries uint32) *uring.Ring {
	t.Helper()
	if !uring.Available() {
		t.Skip("io_uring is not available on this kernel")
	}
	r, err := uring.New(entries)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

func tempFile(t *testing.T, size int64) int {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ring")
	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_CREAT|syscall.O_EXCL, 0600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Close(fd) })
	if err := syscall.Ftruncate(fd, size); err != nil {
		t.Fatal(err)
	}
	return fd
}

func TestRingReportsWhatTheKernelSupports(t *testing.T) {
	r := newRing(t, 8)
	// These four are the operations this package exists to stage. A kernel
	// that lacks one forces the caller back to the synchronous syscall, so
	// the report must be accurate rather than optimistic.
	for _, op := range []struct {
		name string
		code uint8
	}{
		{"SyncFileRange", uring.OpSyncFileRange},
		{"Fallocate", uring.OpFallocate},
		{"Fadvise", uring.OpFadvise},
		{"Madvise", uring.OpMadvise},
	} {
		t.Logf("%-15s supported=%v", op.name, r.Supported(op.code))
	}
	if r.Supported(200) {
		t.Fatal("an operation this kernel cannot have reported as supported")
	}
}

// Fallocate without KEEP_SIZE must both allocate and extend, so a caller
// never needs a separate truncate.
func TestFallocateExtendsTheFile(t *testing.T) {
	r := newRing(t, 8)
	if !r.Supported(uring.OpFallocate) {
		t.Skip("no OpFallocate on this kernel")
	}
	fd := tempFile(t, 0)
	const want = 1 << 20
	if !r.Fallocate(fd, 0, 0, want) {
		t.Fatal("the ring refused a staged fallocate")
	}
	if err := r.Submit(); err != nil {
		t.Fatal(err)
	}
	r.Drain()
	if r.Failures != 0 {
		t.Fatalf("fallocate reported %d failures", r.Failures)
	}
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		t.Fatal(err)
	}
	if st.Size != want {
		t.Fatalf("file size %d, want %d", st.Size, want)
	}
}

// The whole point is that staging does the work, not just that it returns.
func TestMadviseActuallyReleasesPages(t *testing.T) {
	r := newRing(t, 64)
	if !r.Supported(uring.OpMadvise) {
		t.Skip("no OpMadvise on this kernel")
	}
	const size = 4 << 20
	fd := tempFile(t, size)
	m, err := syscall.Mmap(fd, 0, size, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Munmap(m)
	for i := range m {
		m[i] = byte(i)
	}
	before := mappingRSS(t, m)
	if before == 0 {
		t.Fatal("the mapping was not resident after writing to it")
	}
	if !r.Madvise(m, syscall.MADV_DONTNEED) {
		t.Fatal("the ring refused a staged madvise")
	}
	if err := r.Submit(); err != nil {
		t.Fatal(err)
	}
	r.Drain()
	if r.Failures != 0 {
		t.Fatalf("madvise reported %d failures", r.Failures)
	}
	if after := mappingRSS(t, m); after >= before {
		t.Fatalf("mapping RSS %d -> %d KiB: the staged madvise did nothing", before, after)
	}
}

// A full ring must refuse work rather than block. Advisory work that is
// dropped costs nothing; a blocked caller costs exactly what the ring avoids.
func TestFullRingRefusesInsteadOfBlocking(t *testing.T) {
	r := newRing(t, 4)
	fd := tempFile(t, 1<<20)
	staged := 0
	for i := 0; i < int(r.Entries())+8; i++ {
		if !r.SyncFileRange(fd, 0, 4096, 0) {
			break
		}
		staged++
	}
	if staged != int(r.Entries()) {
		t.Fatalf("staged %d entries, want the ring capacity %d", staged, r.Entries())
	}
	if r.Free() != 0 {
		t.Fatalf("ring reports %d free slots when full", r.Free())
	}
	if r.Dropped == 0 {
		t.Fatal("a full ring accepted more work without counting a drop")
	}
	if err := r.Submit(); err != nil {
		t.Fatal(err)
	}
	r.Drain()
	if r.Free() != r.Entries() {
		t.Fatalf("after draining, %d of %d slots are free", r.Free(), r.Entries())
	}
}

func TestReapNeverBlocksOnAnEmptyRing(t *testing.T) {
	r := newRing(t, 8)
	if n := r.Reap(); n != 0 {
		t.Fatalf("Reap on an empty ring returned %d", n)
	}
	if got := r.Inflight(); got != 0 {
		t.Fatalf("Inflight on an empty ring returned %d", got)
	}
}

// mappingRSS reports the kilobytes of b that hold page-table entries.
//
// mincore is the wrong probe here. It reports page-cache residency, and
// MADV_DONTNEED on a shared file mapping deliberately keeps the page cache
// and drops only the mapping. Resident set size is what actually changes.
func mappingRSS(t *testing.T, b []byte) int {
	t.Helper()
	start := uintptr(ptr(b))
	raw, err := os.ReadFile("/proc/self/smaps")
	if err != nil {
		t.Skipf("smaps: %v", err)
	}
	inVMA := false
	for _, line := range strings.Split(string(raw), "\n") {
		if len(line) > 0 && line[0] != ' ' && strings.Contains(line, "-") {
			head := strings.SplitN(line, " ", 2)[0]
			lo, err := strconv.ParseUint(strings.SplitN(head, "-", 2)[0], 16, 64)
			inVMA = err == nil && uintptr(lo) == start
			continue
		}
		if inVMA && strings.HasPrefix(line, "Rss:") {
			f := strings.Fields(line)
			if len(f) >= 2 {
				n, err := strconv.Atoi(f[1])
				if err == nil {
					return n
				}
			}
			return 0
		}
	}
	t.Skip("could not find the mapping in smaps")
	return 0
}

// A linked pair must run in order. Writeback then drop is the whole reason
// the link exists: dropping a range the kernel has not written back yet
// leaves the dirty pages in place and does nothing.
func TestLinkedOperationsRunInOrder(t *testing.T) {
	r := newRing(t, 8)
	if !r.Supported(uring.OpSyncFileRange) || !r.Supported(uring.OpFadvise) {
		t.Skip("this kernel lacks a linked operation")
	}
	const size = 2 << 20
	fd := tempFile(t, size)
	if _, err := syscall.Pwrite(fd, make([]byte, size), 0); err != nil {
		t.Fatal(err)
	}
	// syncFileRangeWaitBefore|Write|WaitAfter, then POSIX_FADV_DONTNEED.
	if !r.SyncFileRange(fd, 0, size, 1|2|4) {
		t.Fatal("the ring refused the writeback")
	}
	r.Link()
	if !r.Fadvise(fd, 0, size, 4) {
		t.Fatal("the ring refused the drop")
	}
	if err := r.Submit(); err != nil {
		t.Fatal(err)
	}
	r.Drain()
	if r.Failures != 0 {
		t.Fatalf("the linked chain reported %d failures", r.Failures)
	}
}
