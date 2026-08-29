//go:build linux && (amd64 || arm64)

package filewin

import (
	"errors"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/akalsi-org/polyglot-template/go/lib/atmc"
	"github.com/akalsi-org/polyglot-template/go/lib/uring"
)

const (
	// ringEntries sizes the submission queue. It only has to hold the few
	// operations one housekeeping round stages.
	ringEntries = 64
	// growTag identifies a staged file growth in a ring completion.
	growTag = 1
)

// Writer appends into the mapped log.
//
// Reserve and Commit never map, unmap, or advise. The file's housekeeping,
// which is preallocating ahead, establishing page-table entries, releasing
// them behind, starting writeback, and dropping page cache, is staged on the
// writer's own ring and picked up by the kernel. There are no helper threads
// and no futex handshakes: the writer tees the work up and reaps completions
// at its own chunk boundaries.
type Writer struct {
	file *File
	pos  uint64
	ring *uring.Ring

	committed uint64
	populated uint64
	dropped   uint64
	synced    uint64
	evicted   uint64
	nextWork  uint64
	growing   uint64
	closed    bool

	Waits      atomic.Uint64
	WaitNs     atomic.Uint64
	Grows      atomic.Uint64
	GrowNs     atomic.Uint64
	Populates  atomic.Uint64
	Dontneed   atomic.Uint64
	Syncs      atomic.Uint64
	Evicts     atomic.Uint64
	Reaped     atomic.Uint64
	Dropped    atomic.Uint64
	SyncErrors atomic.Uint64
	GrowErrors atomic.Uint64
}

// AttachWriter binds a writer to the file.
func (f *File) AttachWriter() (*Writer, error) {
	if f == nil || f.closed {
		return nil, ErrClosed
	}
	w := &Writer{file: f}
	w.pos = atmc.LoadAcquireU64(f.writePos())
	w.committed = atmc.LoadAcquireU64(f.committed())
	w.populated = w.pos
	w.dropped = 0
	w.synced = w.pos
	w.evicted = 0
	w.nextWork = w.pos
	// A ring is an optimisation, not a requirement. Without one the same
	// operations run synchronously, which is correct but costs the writer
	// the tens of microseconds each call takes.
	if r, err := uring.New(ringEntries); err == nil {
		r.OnComplete = w.onComplete
		w.ring = r
	}
	f.addLocalWriter()
	return w, nil
}

// Reserve returns a contiguous writable slice of n bytes.
func (w *Writer) Reserve(n uint64) ([]byte, error) {
	if w == nil || w.closed || w.file.closed {
		return nil, ErrClosed
	}
	if n == 0 || n > w.file.maxReserve {
		return nil, ErrTooLarge
	}
	if w.pos+n > w.file.reserve {
		return nil, ErrFull
	}
	if w.pos+n > w.committed {
		// The file is short. This is the one piece of housekeeping the
		// writer cannot outrun, so it waits here. Growing an extent at a
		// time keeps it rare.
		if err := w.growTo(w.pos + n); err != nil {
			return nil, err
		}
	}
	return w.file.data[w.pos : w.pos+n], nil
}

// Commit publishes n bytes from the last reservation.
func (w *Writer) Commit(n uint64) error {
	if w == nil || w.closed || w.file.closed {
		return ErrClosed
	}
	next := w.pos + n
	atmc.StoreReleaseU64(w.file.writePos(), next)
	w.pos = next
	if next >= w.nextWork {
		w.work()
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

// Pos returns the writer's own cursor.
func (w *Writer) Pos() uint64 {
	if w == nil {
		return 0
	}
	return w.pos
}

// growTo extends the file so a reservation ending at need fits.
// It waits, because the writer cannot store into a page the file does not
// have. Every other operation this writer stages is advisory and does not.
func (w *Writer) growTo(need uint64) error {
	want := need + w.file.ahead + w.file.maxReserve
	if want > w.file.reserve {
		want = w.file.reserve
	}
	want = alignUp(want, w.file.extent)
	if want > w.file.reserve {
		want = w.file.reserve
	}
	t0 := time.Now()
	if err := w.allocate(w.committed, want-w.committed, true); err != nil {
		return err
	}
	w.committed = want
	atmc.StoreReleaseU64(w.file.committed(), want)
	w.Grows.Add(1)
	w.GrowNs.Add(uint64(time.Since(t0).Nanoseconds()))
	w.Waits.Add(1)
	w.WaitNs.Add(uint64(time.Since(t0).Nanoseconds()))
	return nil
}

// allocate preallocates a range. fallocate without FALLOC_FL_KEEP_SIZE both
// allocates the blocks and extends the file, so no truncate is needed.
// Allocating is the point: a store into an unwritten extent would fault on
// the writer instead of here.
func (w *Writer) allocate(off, length uint64, wait bool) error {
	if length == 0 {
		return nil
	}
	if w.ring != nil && wait {
		if w.ring.Fallocate(w.file.fd, 0, int64(off), int64(length)) {
			if err := w.ring.Submit(); err == nil {
				w.ring.Drain()
				return nil
			}
		}
	}
	err := fallocate(w.file.fd, int64(off), int64(length))
	if errors.Is(err, syscall.ENOSYS) || errors.Is(err, syscall.EOPNOTSUPP) {
		err = syscall.Ftruncate(w.file.fd, int64(off+length))
	}
	return err
}

// work stages one round of housekeeping. It never waits for the kernel.
func (w *Writer) work() {
	w.nextWork = w.pos + w.file.workBytes
	if w.ring != nil {
		w.Reaped.Add(uint64(w.ring.Reap()))
	}
	w.growAhead()
	w.populateAhead()
	w.dropBehind()
	w.syncBehind()
	w.evictBehind()
	if w.ring != nil {
		if err := w.ring.Submit(); err != nil {
			w.SyncErrors.Add(1)
		}
		w.Dropped.Store(w.ring.Dropped)
	}
}

// growAhead keeps the file ahead of the cursor so Reserve rarely waits.
func (w *Writer) growAhead() {
	want := w.pos + w.file.ahead + w.file.maxReserve
	if want <= w.committed {
		return
	}
	if want > w.file.reserve {
		want = w.file.reserve
	}
	want = alignUp(want, w.file.extent)
	if want > w.file.reserve {
		want = w.file.reserve
	}
	if want <= w.committed {
		return
	}
	if w.ring == nil {
		if err := w.allocate(w.committed, want-w.committed, false); err == nil {
			w.publishCommitted(want)
		}
		return
	}
	if w.growing != 0 {
		// One staged grow at a time. Until it completes the file is still
		// short, and Reserve must keep believing that.
		return
	}
	if !w.ring.Fallocate(w.file.fd, 0, int64(w.committed), int64(want-w.committed)) {
		return
	}
	w.ring.Tag(growTag)
	// Do NOT publish the new length here. A staged operation has not
	// happened yet, and it can still fail, most obviously with ENOSPC. A
	// writer that believed the file had grown would store past the end of
	// it and take SIGBUS. The completion handler publishes instead.
	w.growing = want
}

// publishCommitted records a file length whose blocks now exist.
func (w *Writer) publishCommitted(length uint64) {
	if length <= w.committed {
		return
	}
	w.committed = length
	atmc.StoreReleaseU64(w.file.committed(), length)
	w.Grows.Add(1)
}

// onComplete runs for every completion this writer's ring reaps.
func (w *Writer) onComplete(tag uint64, res int32) {
	if tag != growTag {
		return
	}
	want := w.growing
	w.growing = 0
	if res < 0 {
		// The allocation failed. Leave the file length alone. Reserve then
		// retries synchronously and surfaces the error to the caller.
		w.GrowErrors.Add(1)
		return
	}
	w.publishCommitted(want)
}

// populateAhead establishes writable page-table entries ahead of the cursor,
// so the writer does not fault on first touch. A missed populate costs a
// fault, never correctness, so it is staged and never waited on.
func (w *Writer) populateAhead() {
	target := alignDown(w.pos+w.file.ahead, w.file.page)
	if target > w.committed {
		target = alignDown(w.committed, w.file.page)
	}
	if target <= w.populated || target-w.populated < w.file.populateChunk {
		return
	}
	start := w.populated
	for start < target {
		end := start + w.file.populateChunk
		if end > target {
			end = target
		}
		if w.ring == nil {
			madvise(w.file.data[start:end], madvPopulateWrite)
		} else if !w.ring.Madvise(w.file.data[start:end], madvPopulateWrite) {
			break
		}
		w.Populates.Add(1)
		start = end
	}
	w.populated = start
}

// dropBehind releases page-table entries behind the retention window.
func (w *Writer) dropBehind() {
	floor := w.file.dropFloor()
	chunk := w.file.dropChunk
	if floor < chunk {
		return
	}
	target := alignDown(floor, chunk)
	if target <= w.dropped || target-w.dropped < chunk {
		return
	}
	start := w.dropped
	for start < target {
		end := start + chunk
		if end > target {
			end = target
		}
		if w.ring == nil {
			madvise(w.file.data[start:end], madvDont)
		} else if !w.ring.Madvise(w.file.data[start:end], madvDont) {
			break
		}
		w.Dontneed.Add(1)
		start = end
	}
	w.dropped = start
}

// syncBehind starts writeback for committed bytes and never waits for it.
func (w *Writer) syncBehind() {
	if w.file.disableWriteback {
		return
	}
	target := alignDown(w.pos, w.file.page)
	if target <= w.synced || target-w.synced < w.file.syncBytes {
		return
	}
	if w.ring == nil {
		if err := syncFileRange(w.file.fd, int64(w.synced), int64(target-w.synced), syncFileRangeWrite); err != nil {
			w.SyncErrors.Add(1)
			return
		}
	} else if !w.ring.SyncFileRange(w.file.fd, int64(w.synced), int64(target-w.synced), syncFileRangeWrite) {
		return
	}
	w.Syncs.Add(1)
	w.synced = target
	atmc.StoreReleaseU64(w.file.syncedPos(), target)
}

// evictBehind drops page-cache pages for history nobody needs. Without this
// the page cache grows until the kernel must reclaim it, and reclaim stalls
// the writer with major faults.
//
// Writeback must finish before the drop, or the drop keeps the dirty pages
// and achieves nothing. The two are staged as a linked pair, so the kernel
// sequences them and the writer never waits for the device.
func (w *Writer) evictBehind() {
	if w.file.disableWriteback || w.file.disableEvict {
		return
	}
	floor := w.file.dropFloor()
	if floor < w.file.keepCached {
		return
	}
	target := alignDown(floor-w.file.keepCached, w.file.page)
	if target <= w.evicted || target-w.evicted < w.file.evictChunk {
		return
	}
	if target-w.evicted > w.file.evictChunk {
		target = w.evicted + w.file.evictChunk
	}
	off, length := int64(w.evicted), int64(target-w.evicted)
	flags := syncFileRangeWaitBefore | syncFileRangeWrite | syncFileRangeWaitAfter
	if w.ring == nil {
		if err := syncFileRange(w.file.fd, off, length, flags); err != nil {
			w.SyncErrors.Add(1)
			return
		}
		if err := fadvise(w.file.fd, off, length, fadviseDontneed); err != nil {
			w.SyncErrors.Add(1)
			return
		}
	} else {
		if !w.ring.SyncFileRange(w.file.fd, off, length, flags) {
			return
		}
		w.ring.Link()
		if !w.ring.Fadvise(w.file.fd, off, length, fadviseDontneed) {
			return
		}
	}
	w.Evicts.Add(1)
	w.evicted = target
	atmc.StoreReleaseU64(w.file.evictedPos(), target)
}

// Close releases the writer. The mapping belongs to the File.
func (w *Writer) Close() error {
	if w == nil || w.closed {
		return nil
	}
	w.closed = true
	var err error
	if w.ring != nil {
		err = w.ring.Close()
		w.ring = nil
	}
	w.file.removeLocalWriter()
	return err
}
