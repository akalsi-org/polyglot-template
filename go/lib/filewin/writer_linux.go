//go:build linux && (amd64 || arm64)

package filewin

import (
	"sync/atomic"
	"syscall"

	"github.com/akalsi-org/polyglot-template/go/lib/internal/orderedatomic"
)

const chunkBytes = 2 << 20

// Writer appends into the file window.
// AttachWriter starts an unpinned helper that grows the file linearly and prefaults.
type Writer struct {
	file     *File
	mapOff   uint64
	mapBytes []byte
	pos      uint64
	stop     atomic.Bool
	helper   chan struct{}
}

// AttachWriter maps a data window and starts the growth helper.
func (f *File) AttachWriter() (*Writer, error) {
	if f == nil || f.closed {
		return nil, ErrClosed
	}
	w := &Writer{file: f, helper: make(chan struct{})}
	w.pos = orderedatomic.LoadAcquire64(f.writePos())
	if err := w.remap(); err != nil {
		return nil, err
	}
	go w.runHelper()
	return w, nil
}

func (w *Writer) remap() error {
	if w.mapBytes != nil {
		_ = syscall.Munmap(w.mapBytes)
		w.mapBytes = nil
	}
	pos := w.pos
	fileOff := alignDown(w.file.dataFileOff(pos), w.file.page)
	mapped, err := mapRange(w.file.fd, fileOff, int(w.file.window), syscall.PROT_READ|syscall.PROT_WRITE)
	if err != nil {
		return err
	}
	w.mapOff = fileOff
	w.mapBytes = mapped
	return nil
}

func (w *Writer) covers(pos, n uint64) bool {
	start := w.file.dataFileOff(pos)
	end := start + n
	return start >= w.mapOff && end <= w.mapOff+uint64(len(w.mapBytes))
}

func (w *Writer) waitCommitted(need uint64) error {
	for !w.stop.Load() {
		if orderedatomic.LoadAcquire64(w.file.committed()) >= need {
			return nil
		}
		futexWait(w.file.writeGen(), orderedatomic.LoadRelaxed32(w.file.writeGen()))
	}
	return ErrClosed
}

// Reserve returns a contiguous writable slice of n bytes.
func (w *Writer) Reserve(n uint64) ([]byte, error) {
	if w == nil || w.file.closed || w.stop.Load() {
		return nil, ErrClosed
	}
	if n == 0 || n > w.file.maxReserve {
		return nil, ErrTooLarge
	}
	need := w.file.dataFileOff(w.pos + n + w.file.ahead)
	if err := w.waitCommitted(need); err != nil {
		return nil, err
	}
	if !w.covers(w.pos, n) {
		if err := w.remap(); err != nil {
			return nil, err
		}
	}
	if !w.covers(w.pos, n) {
		return nil, ErrTooLarge
	}
	off := w.file.dataFileOff(w.pos) - w.mapOff
	return w.mapBytes[off : off+n], nil
}

// Commit publishes n bytes from the last reservation.
func (w *Writer) Commit(n uint64) error {
	if w == nil || w.file.closed {
		return ErrClosed
	}
	next := w.pos + n
	prevChunk := w.pos / chunkBytes
	orderedatomic.StoreRelease64(w.file.writePos(), next)
	w.pos = next
	if next/chunkBytes != prevChunk {
		orderedatomic.FetchAddAcqRel32(w.file.writeGen(), 1)
		futexWake(w.file.writeGen())
	}
	return nil
}

// Write copies src and commits it.
func (w *Writer) Write(src []byte) error {
	span, err := w.Reserve(uint64(len(src)))
	if err != nil {
		return err
	}
	copy(span, src)
	return w.Commit(uint64(len(src)))
}

func (w *Writer) runHelper() {
	defer close(w.helper)
	for !w.stop.Load() {
		pos := orderedatomic.LoadAcquire64(w.file.writePos())
		want := w.file.dataFileOff(pos) + w.file.window
		if extra := w.file.dataFileOff(pos) + w.file.ahead + w.file.maxReserve; extra > want {
			want = extra
		}
		committed := orderedatomic.LoadAcquire64(w.file.committed())
		grew := false
		for committed < want && !w.stop.Load() {
			next := committed + w.file.extent
			if err := syscall.Ftruncate(w.file.fd, int64(next)); err != nil {
				break
			}
			_ = fallocate(w.file.fd, int64(committed), int64(w.file.extent))
			orderedatomic.StoreRelease64(w.file.committed(), next)
			committed = next
			grew = true
		}
		if grew {
			futexWake(w.file.writeGen())
		}
		if w.mapBytes != nil {
			posFile := w.file.dataFileOff(pos)
			if posFile >= w.mapOff {
				local := posFile - w.mapOff
				end := local + w.file.ahead
				if end > uint64(len(w.mapBytes)) {
					end = uint64(len(w.mapBytes))
				}
				if end > local {
					madvise(w.mapBytes[local:end], madvWill)
				}
			}
		}
		if !grew && committed >= want {
			futexWait(w.file.writeGen(), orderedatomic.LoadRelaxed32(w.file.writeGen()))
		}
	}
}

// Close stops the helper and unmaps the writer window.
func (w *Writer) Close() error {
	if w == nil {
		return nil
	}
	w.stop.Store(true)
	futexWake(w.file.writeGen())
	<-w.helper
	if w.mapBytes != nil {
		err := syscall.Munmap(w.mapBytes)
		w.mapBytes = nil
		return err
	}
	return nil
}
