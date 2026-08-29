//go:build linux && (amd64 || arm64)

package filewin

import (
	"syscall"

	"github.com/akalsi-org/polyglot-template/go/lib/internal/orderedatomic"
)

// Reader maps a contiguous window around read_pos.
type Reader struct {
	file     *File
	mapOff   uint64
	mapBytes []byte
	pos      uint64
}

// AttachReader maps a data window at the current read cursor.
func (f *File) AttachReader() (*Reader, error) {
	if f == nil || f.closed {
		return nil, ErrClosed
	}
	r := &Reader{file: f}
	r.pos = orderedatomic.LoadAcquire64(f.readPos())
	if r.pos == 0 {
		r.pos = 0
	}
	if err := r.remap(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *Reader) remap() error {
	if r.mapBytes != nil {
		_ = syscall.Munmap(r.mapBytes)
		r.mapBytes = nil
	}
	fileOff := alignDown(r.file.dataFileOff(r.pos), r.file.page)
	mapped, err := mapRange(r.file.fd, fileOff, int(r.file.window), syscall.PROT_READ|syscall.PROT_WRITE)
	if err != nil {
		return err
	}
	r.mapOff = fileOff
	r.mapBytes = mapped
	return nil
}

// Peek returns committed bytes from the current read cursor.
func (r *Reader) Peek(max uint64) ([]byte, error) {
	if r == nil || r.file.closed {
		return nil, ErrClosed
	}
	write := orderedatomic.LoadAcquire64(r.file.writePos())
	if r.pos == write {
		return nil, nil
	}
	if r.pos > write {
		return nil, ErrFormat
	}
	n := write - r.pos
	if n > max && max != 0 {
		n = max
	}
	if r.file.dataFileOff(r.pos)+n > r.mapOff+uint64(len(r.mapBytes)) {
		if err := r.remap(); err != nil {
			return nil, err
		}
	}
	off := r.file.dataFileOff(r.pos) - r.mapOff
	if off+n > uint64(len(r.mapBytes)) {
		n = uint64(len(r.mapBytes)) - off
	}
	return r.mapBytes[off : off+n], nil
}

// Advance publishes a new read cursor and may drop pages behind the window.
func (r *Reader) Advance(n uint64) error {
	if r == nil || r.file.closed {
		return ErrClosed
	}
	next := r.pos + n
	prevChunk := r.pos / chunkBytes
	orderedatomic.StoreRelease64(r.file.readPos(), next)
	dropTo := next
	if dropTo > r.file.window {
		dropFile := r.file.dataFileOff(dropTo - r.file.window)
		if dropFile >= r.mapOff {
			local := dropFile - r.mapOff
			if local > 0 && local <= uint64(len(r.mapBytes)) {
				madvise(r.mapBytes[:local], madvDont)
			}
		}
	}
	r.pos = next
	if next/chunkBytes != prevChunk {
		orderedatomic.FetchAddAcqRel32(r.file.readGen(), 1)
		futexWake(r.file.readGen())
	}
	return nil
}

// Close unmaps the reader window.
func (r *Reader) Close() error {
	if r == nil || r.mapBytes == nil {
		return nil
	}
	err := syscall.Munmap(r.mapBytes)
	r.mapBytes = nil
	return err
}
