//go:build linux && (amd64 || arm64)

package mpsc

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"syscall"
	"testing"
)

func TestPublicAPIErrorsAndAccessors(t *testing.T) {
	if (*MPSC)(nil).Capacity() != 0 || (*SPSC)(nil).Capacity() != 0 {
		t.Fatal("nil queue has capacity")
	}
	if err := (*MPSC)(nil).Close(); err != nil {
		t.Fatal(err)
	}
	if err := (*SPSC)(nil).Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := (*MPSC)(nil).DupFD(); !errors.Is(err, ErrClosed) {
		t.Fatalf("nil MPSC DupFD: %v", err)
	}
	if _, err := (*SPSC)(nil).DupFD(); !errors.Is(err, ErrClosed) {
		t.Fatalf("nil SPSC DupFD: %v", err)
	}
	if err := (*MPSCProducer)(nil).Close(); err != nil {
		t.Fatal(err)
	}
	if err := (*MPSCConsumer)(nil).Close(); err != nil {
		t.Fatal(err)
	}
	if err := (*SPSCProducer)(nil).Close(); err != nil {
		t.Fatal(err)
	}
	if err := (*SPSCConsumer)(nil).Close(); err != nil {
		t.Fatal(err)
	}

	read := ReadSpan{bytes: []byte("payload")}
	if read.Len() != 7 {
		t.Fatalf("length: %d", read.Len())
	}
	dst := make([]byte, 4)
	if n := read.CopyTo(dst); n != 4 || string(dst) != "payl" {
		t.Fatalf("copy: %d %q", n, dst)
	}

	m, err := CreateMPSC(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	mp, err := m.NewProducer()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mp.Reserve(uint64(math.MaxInt) + 1); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("large MPSC reserve: %v", err)
	}
	if err := mp.Commit(WriteSpan{}, 0); !errors.Is(err, ErrMisuse) {
		t.Fatalf("empty MPSC commit: %v", err)
	}
	if err := mp.Abort(WriteSpan{}); !errors.Is(err, ErrMisuse) {
		t.Fatalf("empty MPSC abort: %v", err)
	}
	if err := mp.Close(); err != nil {
		t.Fatal(err)
	}
	if err := mp.Close(); err != nil {
		t.Fatal(err)
	}
	if err := mp.Write(nil); !errors.Is(err, ErrMisuse) {
		t.Fatalf("closed MPSC write: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := m.DupFD(); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed MPSC DupFD: %v", err)
	}
	if _, err := m.AttachConsumer(); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed MPSC consumer: %v", err)
	}

	s, err := CreateSPSC(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	sp, err := s.AttachProducer()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sp.Reserve(uint64(math.MaxInt) + 1); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("large SPSC reserve: %v", err)
	}
	if err := sp.Commit(WriteSpan{}, 0); !errors.Is(err, ErrMisuse) {
		t.Fatalf("empty SPSC commit: %v", err)
	}
	if err := sp.Abort(WriteSpan{}); !errors.Is(err, ErrMisuse) {
		t.Fatalf("empty SPSC abort: %v", err)
	}
	if err := sp.Close(); err != nil {
		t.Fatal(err)
	}
	if err := sp.Close(); err != nil {
		t.Fatal(err)
	}
	if err := sp.Write(nil); !errors.Is(err, ErrMisuse) {
		t.Fatalf("closed SPSC write: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DupFD(); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed SPSC DupFD: %v", err)
	}
	if _, err := s.AttachConsumer(); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed SPSC consumer: %v", err)
	}
}

func TestRegionBackendsAndAttachValidation(t *testing.T) {
	for _, cfg := range []Config{
		{Capacity: 4096, Backend: BackendSHM},
		{Capacity: 4096, Backend: BackendSHM, Name: "../bad"},
		{Capacity: 4096, Backend: BackendSHM, Name: "bad/name"},
		{Capacity: 4096, Backend: BackendFile},
		{Capacity: 4096, Backend: Backend(255)},
		{Capacity: maxExtent + 1},
	} {
		if q, err := CreateMPSC(cfg); q != nil || !errors.Is(err, ErrFormat) {
			if q != nil {
				q.Close()
			}
			t.Fatalf("configuration %+v: queue=%v err=%v", cfg, q, err)
		}
	}

	name := "pgt-mpsc-coverage-" + strconv.Itoa(os.Getpid()) + "-" + filepath.Base(t.TempDir())
	shmPath := filepath.Join("/dev/shm", name)
	q, err := CreateSPSC(Config{Capacity: 4096, Backend: BackendSHM, Name: name})
	if err != nil {
		t.Fatal(err)
	}
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(shmPath); err != nil {
		t.Fatal(err)
	}

	filePath := filepath.Join(t.TempDir(), "queue")
	q, err = CreateSPSC(Config{Capacity: 4096, Backend: BackendFile, Name: filePath, DisablePreallocate: true})
	if err != nil {
		t.Fatal(err)
	}
	fd, err := q.DupFD()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AttachMPSC(fd); !errors.Is(err, ErrFormat) {
		t.Fatalf("MPSC attached to SPSC: %v", err)
	}
	if err := syscall.Close(fd); err != nil {
		t.Fatal(err)
	}
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}

	m, err := CreateMPSC(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	fd, err = m.DupFD()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AttachSPSC(fd); !errors.Is(err, ErrFormat) {
		t.Fatalf("SPSC attached to MPSC: %v", err)
	}
	if err := syscall.Close(fd); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := attachRegion(-1); err == nil {
		t.Fatal("attached invalid descriptor")
	}
	shortPath := filepath.Join(t.TempDir(), "short")
	if err := os.WriteFile(shortPath, []byte("short"), 0600); err != nil {
		t.Fatal(err)
	}
	shortFD, err := syscall.Open(shortPath, syscall.O_RDONLY|syscall.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(shortFD)
	if _, err := attachRegion(shortFD); !errors.Is(err, ErrFormat) {
		t.Fatalf("short region: %v", err)
	}
}

func TestAttachRejectsInvalidHeaderGeometryAndSize(t *testing.T) {
	tests := []struct {
		name   string
		offset uintptr
		value  uint64
		width  int
		resize int64
	}{
		{name: "shards", offset: 12, value: 2, width: 4},
		{name: "zero capacity", offset: 16, value: 0, width: 8},
		{name: "non-power capacity", offset: 16, value: 6144, width: 8},
		{name: "claim stride", offset: 32, value: 7, width: 4},
		{name: "result stride", offset: 36, value: 7, width: 4},
		{name: "grain", offset: 40, value: 32, width: 4},
		{name: "file size", resize: 4096},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfig()
			if tt.resize != 0 {
				cfg.Backend = BackendFile
				cfg.Name = filepath.Join(t.TempDir(), "queue")
				cfg.DisablePreallocate = true
			}
			q, err := CreateMPSC(cfg)
			if err != nil {
				t.Fatal(err)
			}
			fd, err := q.DupFD()
			if err != nil {
				t.Fatal(err)
			}
			defer syscall.Close(fd)
			defer q.Close()
			if tt.resize != 0 {
				if err := syscall.Ftruncate(fd, tt.resize); err != nil {
					t.Fatal(err)
				}
			} else {
				var encoded [8]byte
				if tt.width == 4 {
					put32(encoded[:], 0, uint32(tt.value))
				} else {
					put64(encoded[:], 0, tt.value)
				}
				if _, err := syscall.Pwrite(fd, encoded[:tt.width], int64(tt.offset)); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := AttachMPSC(fd); !errors.Is(err, ErrFormat) {
				t.Fatalf("attach: %v", err)
			}
		})
	}
}

func TestGeometryAndOwnerColdPaths(t *testing.T) {
	if _, ok := mpscGeometry(MPSCLayout(255)); ok {
		t.Fatal("unknown layout has geometry")
	}
	if isMPSCGeometry(1, 2, 3) {
		t.Fatal("unknown geometry is valid")
	}
	if validPlaneGeometry(4096, 0, 0, 64) {
		t.Fatal("plain geometry accepted a grain")
	}
	if validPlaneGeometry(32, 8, 0, 64) {
		t.Fatal("oversized grain accepted")
	}
	if planeBytes(4096, 0, 0, 0) != 0 {
		t.Fatal("plain region has plane bytes")
	}
	if got, err := normalizeCapacity(1, 4096); err != nil || got != 4096 {
		t.Fatalf("normalized capacity: %d %v", got, err)
	}

	old := nextSpanOwner.Load()
	nextSpanOwner.Store(math.MaxUint64)
	if owner := newSpanOwner(); owner == 0 {
		t.Fatal("zero span owner")
	}
	nextSpanOwner.Store(old)
	claimBackoff(5)
}

func TestMPSCInternalColdPaths(t *testing.T) {
	q, err := CreateMPSC(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	writer, err := q.newWriter()
	if err != nil {
		t.Fatal(err)
	}
	if _, status := writer.Reserve(-1); status != statusTooLarge {
		t.Fatalf("negative reserve: %v", status)
	}
	span, status := writer.Reserve(1)
	if status != statusOK {
		t.Fatal(status)
	}
	if _, status := writer.Reserve(1); status != statusMisuse {
		t.Fatalf("second reserve: %v", status)
	}
	if status := writer.Write(nil); status != statusMisuse {
		t.Fatalf("write with reservation: %v", status)
	}
	_, tag := q.result(span.pos)
	atomic.StoreUint64(tag, 0)
	if err := writer.Abort(span); !errors.Is(err, ErrRecovered) {
		t.Fatalf("recovered abort: %v", err)
	}

	writer.closed = true
	if status := writer.Write(nil); status != statusMisuse {
		t.Fatalf("closed write: %v", status)
	}
	writer.closed = false
	writer.pos = invalidPos
	writer.fullThreshold = 0
	if status := writer.fullStatus(0); status != statusFull {
		t.Fatalf("full status: %v", status)
	}
	writer.fullStreak = 1
	writer.fullThreshold = 2
	if status := writer.fullStatus(0); status != statusFull || writer.fullThreshold != 4 {
		t.Fatalf("threshold status: %v threshold=%d", status, writer.fullThreshold)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	reader, err := q.newReader()
	if err != nil {
		t.Fatal(err)
	}
	atomic.StoreUint64(q.claim(reader.rd), packrecord(64, 7, 0))
	if _, _, err := reader.Peek(); !errors.Is(err, ErrFormat) {
		t.Fatalf("invalid state: %v", err)
	}
	atomic.StoreUint64(q.claim(reader.rd), freeWord(reader.rd))
	reader.peekExtent = 64
	reader.rd = math.MaxUint64 - 31
	if err := reader.Pop(); !errors.Is(err, ErrPositionExhausted) {
		t.Fatalf("overflowing pop: %v", err)
	}
	reader.peekExtent = 0
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSPSCInternalColdPaths(t *testing.T) {
	q, err := CreateSPSC(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	writer, err := q.newWriter()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.newWriter(); !errors.Is(err, ErrBusy) {
		t.Fatalf("second writer: %v", err)
	}
	if _, status := writer.Reserve(-1); status != statusTooLarge {
		t.Fatalf("negative reserve: %v", status)
	}
	writer.tail = math.MaxUint64 - 31
	if _, status := writer.Reserve(1); status != statusPositionExhausted {
		t.Fatalf("overflow reserve: %v", status)
	}
	writer.tail = 0
	writer.closed = true
	if status := writer.Write(nil); status != statusMisuse {
		t.Fatalf("closed write: %v", status)
	}
	writer.closed = false
	writer.fullThreshold = 0
	if status := writer.fullStatus(0); status != statusFull {
		t.Fatalf("full status: %v", status)
	}
	span, status := writer.Reserve(1)
	if status != statusOK {
		t.Fatal(status)
	}
	if err := writer.Commit(span, -1); !errors.Is(err, ErrMisuse) {
		t.Fatalf("negative commit: %v", err)
	}
	if err := writer.Abort(span); err != nil {
		t.Fatal(err)
	}

	reader, err := q.newReader()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.newReader(); !errors.Is(err, ErrBusy) {
		t.Fatalf("second reader: %v", err)
	}
	reader.closed = true
	if _, _, err := reader.Peek(); !errors.Is(err, ErrMisuse) {
		t.Fatalf("closed peek: %v", err)
	}
	reader.closed = false
	reader.peekExtent = 64
	reader.rd = math.MaxUint64 - 31
	if err := reader.Pop(); !errors.Is(err, ErrPositionExhausted) {
		t.Fatalf("overflowing pop: %v", err)
	}
	reader.peekExtent = 0
	reader.rd = 0
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
}
