//go:build linux && (amd64 || arm64)

package filewin

import (
	"sync/atomic"

	"github.com/akalsi-org/polyglot-template/go/lib/atmc"
	"github.com/akalsi-org/polyglot-template/go/lib/uring"
)

// Reader consumes the mapped log.
//
// A reader is anonymous. It claims nothing, publishes nothing, and the writer
// never learns that it exists, so any number of readers may attach from any
// number of processes. The cost of that freedom is that no reader holds
// history: the writer retains RetainBytes and releases the rest. A reader
// further behind than that still reads correct bytes, because the bytes stay
// in the file. It reads them from the device instead of from memory.
//
// Peek and Advance never map, unmap, or advise. A live reader polls the
// write cursor. The writer does not wake it. Page-table entries behind the
// cursor are released on the reader's own ring, so a reader that trails a
// very large file still holds a small resident set.
type Reader struct {
	file *File
	pos  uint64
	// limit is this reader's snapshot of the write cursor.
	// The reader drains to it before it loads the shared cursor again.
	limit uint64

	ring      *uring.Ring
	dropped   uint64
	populated uint64
	closed    bool

	Empty     atomic.Uint64
	Dontneed  atomic.Uint64
	Populates atomic.Uint64
	Reaped    atomic.Uint64
}

// AttachReader returns a reader positioned at the start of the log.
// Use Seek with WritePos to follow only new records instead.
func (f *File) AttachReader() (*Reader, error) {
	if f == nil || f.closed {
		return nil, ErrClosed
	}
	r := &Reader{file: f}
	r.dropped = 0
	r.populated = 0
	// A ring is an optimisation. Without one the reader releases its own
	// history synchronously, which is correct but costs it the tens of
	// microseconds each madvise takes.
	if ring, err := uring.New(ringEntries); err == nil {
		ring.Async = true
		r.ring = ring
	}
	return r, nil
}

// Seek moves the cursor to an absolute log position.
func (r *Reader) Seek(pos uint64) error {
	if r == nil || r.file == nil || r.closed || r.file.closed {
		return ErrClosed
	}
	// The write cursor never exceeds the file's published length, so a
	// cursor-bounded reader can never touch a page past the end of the file
	// and raise SIGBUS.
	if pos > atmc.LoadAcquireU64(r.file.writePos()) {
		return ErrMisuse
	}
	r.pos = pos
	r.limit = pos
	r.dropped = alignDown(pos, r.file.readerDropChunk)
	return nil
}

// Pos returns this reader's own cursor. Nobody else can observe it.
func (r *Reader) Pos() uint64 {
	if r == nil {
		return 0
	}
	return r.pos
}

// Peek returns committed bytes from the current read cursor.
//
// It loads the shared write cursor once and then drains everything up to that
// snapshot without loading it again. The writer stores that cursor on every
// commit, so each load by each reader is a line the writer must later
// invalidate. Reading to a snapshot makes a reader that fell behind quiet: it
// costs one shared load per batch instead of one per record, and the further
// behind it is, the less it interferes with the writer.
//
// The returned slice aliases the mapping. It stays valid until this reader
// has advanced RetainBytes past it.
func (r *Reader) Peek(max uint64) ([]byte, error) {
	if r == nil || r.file == nil || r.closed || r.file.closed {
		return nil, ErrClosed
	}
	if r.pos >= r.limit {
		r.limit = atmc.LoadAcquireU64(r.file.writePos())
		if r.pos == r.limit {
			r.Empty.Add(1)
			return nil, nil
		}
	}
	if r.pos > r.limit || r.limit > r.file.reserve {
		return nil, ErrFormat
	}
	n := r.limit - r.pos
	if n > max && max != 0 {
		n = max
	}
	return r.file.data[r.pos : r.pos+n], nil
}

// Advance moves this reader's cursor. It writes nothing shared.
func (r *Reader) Advance(n uint64) error {
	if r == nil || r.file == nil || r.closed || r.file.closed {
		return ErrClosed
	}
	r.pos += n
	if r.pos-r.dropped >= r.file.readerDropChunk {
		r.housekeep()
	}
	return nil
}

// housekeep establishes page-table entries ahead of this reader and releases
// them behind it, in one submission.
//
// A reader in the writer's own process rarely faults, because the writer has
// already installed the entries it is about to read. A reader in another
// process has its own page tables and faults on every page. Establishing them
// ahead replaces those faults with one call over many pages.
//
// It asks for read entries, not write ones. The page is already in the page
// cache, put there by the writer, so this only has to map it: there is no
// allocation and no zeroing, which is what makes it much cheaper than the
// write-side populate and safe to do in a batch.
func (r *Reader) housekeep() {
	if r.ring != nil {
		r.Reaped.Add(uint64(r.ring.Reap()))
	}
	// Release before establishing, not after. Both go into one submission
	// and the kernel runs them in that order, so the pages this reader has
	// finished with are free to back the ones it is about to need. The other
	// order peaks at holding both sets at once.
	r.dropBehind()
	r.populateAhead()
	if r.ring != nil {
		_ = r.ring.Submit()
	}
}

func (r *Reader) populateAhead() {
	if r.file.noReaderPopulate {
		return
	}
	chunk := r.file.readerDropChunk
	target := alignDown(r.pos+r.file.readerAhead, r.file.page)
	// Never past what the writer has published: the pages beyond it may not
	// be in the file yet, and touching those is fatal rather than slow.
	//
	// Read the cursor rather than the reader's snapshot of it. Advance calls
	// this having just consumed up to that snapshot, so the snapshot equals
	// the cursor and clamping to it would leave nothing ahead to establish.
	limit := alignDown(atmc.LoadAcquireU64(r.file.writePos()), r.file.page)
	if target > limit {
		target = limit
	}
	if r.populated < r.pos {
		r.populated = alignDown(r.pos, r.file.page)
	}
	if target <= r.populated || target-r.populated < chunk {
		return
	}
	start := r.populated
	for start < target {
		end := start + chunk
		if end > target {
			end = target
		}
		if r.ring == nil {
			madvise(r.file.data[start:end], madvPopulateRead)
		} else if !r.ring.Madvise(r.file.data[start:end], madvPopulateRead) {
			break
		}
		r.Populates.Add(1)
		start = end
	}
	r.populated = start
}

// dropBehind releases page-table entries behind this reader's cursor, so a
// reader trailing a very large file still holds a small resident set. The
// bytes stay in the page cache, which the writer process owns.
//
// The work is staged on the reader's own ring and never waited on: a missed
// drop costs memory, never correctness.
func (r *Reader) dropBehind() {
	chunk := r.file.readerDropChunk
	if r.pos < chunk {
		return
	}
	target := alignDown(r.pos-chunk, chunk)
	if target <= r.dropped {
		return
	}
	start := r.dropped
	for start < target {
		end := start + chunk
		if end > target {
			end = target
		}
		if r.ring == nil {
			madvise(r.file.data[start:end], madvDont)
		} else if !r.ring.Madvise(r.file.data[start:end], madvDont) {
			break
		}
		r.Dontneed.Add(1)
		start = end
	}
	r.dropped = start
}

// Close releases the reader. The mapping belongs to the File.
func (r *Reader) Close() error {
	if r == nil || r.file == nil || r.closed {
		return nil
	}
	r.closed = true
	var err error
	if r.ring != nil {
		err = r.ring.Close()
		r.ring = nil
	}
	r.file = nil
	return err
}
