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
	ringEntries = 256
	// growTag identifies a staged file growth in a ring completion.
	growTag = 1
	// populateCatchUp is how many rounds' worth of page-table work one round
	// may stage. One round's worth keeps pace; the rest closes a gap.
	populateCatchUp = 2
	// crossPatience is how long a producer waits for a staged growth before
	// doing it itself. It must exceed helperIdleNs by a wide margin.
	crossPatience = 2 * time.Millisecond
	// helperIdleNs is how long the helper sleeps with nothing to do. The
	// runway is an extent, so this only has to be far below the time the
	// producer takes to cross one.
	helperIdleNs = 20 * time.Microsecond
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
	// Producer state. Only the calling goroutine touches these.
	file      *File
	pos       uint64
	committed uint64 // cached file length; the shared one is authoritative
	closed    bool

	// Growth crosses between the two, and is the only thing that does.
	//
	// The producer stages a growth long before it needs the space and
	// numbers it. The submitting goroutine reaps the completion and records
	// the number it has reached. Neither writes the other's word: growSeq
	// and growTarget belong to the producer, growDone to the submitter. The
	// producer waits only when it reaches the end of the length it has
	// published, which with an extent staged ahead it should not.
	growSeq    uint64
	growTarget uint64
	growDone   atomic.Uint64

	// Staged by the producer, submitted by the other goroutine.
	ring     *uring.Ring
	nextWork uint64

	// Housekeeping cursors. Producer only; it stages, it does not submit.
	populated uint64
	dropped   uint64
	synced    uint64
	evicted   uint64

	stop       atomic.Bool
	helperDone chan struct{}

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
	// SelfGrows counts growths the producer had to perform itself.
	SelfGrows atomic.Uint64
	// WorkNs is time the producer itself spent staging housekeeping.
	WorkNs                                                      atomic.Uint64
	Works                                                       atomic.Uint64
	GrowStageNs, PopStageNs, DropStageNs, SyncStageNs, SubmitNs atomic.Uint64
	ReapNs                                                      atomic.Uint64
}

// AttachWriter binds a writer to the file.
func (f *File) AttachWriter() (*Writer, error) {
	if f == nil || f.closed {
		return nil, ErrClosed
	}
	w := &Writer{file: f}
	w.pos = atmc.LoadAcquireU64(f.writePos())
	w.committed = atmc.LoadAcquireU64(f.committed())
	w.growTarget = w.committed
	w.populated = w.pos
	w.dropped = 0
	w.synced = w.pos
	w.evicted = 0
	w.nextWork = w.pos
	// A ring is an optimisation, not a requirement. Without one the same
	// operations run synchronously, which is correct but costs the writer
	// the tens of microseconds each call takes.
	if r, err := uring.New(ringEntries); err == nil {
		r.Async = true
		r.OnComplete = w.onComplete
		w.ring = r
	}
	f.addLocalWriter()
	if w.ring != nil {
		w.helperDone = make(chan struct{})
		go w.runSubmitter()
	}
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
	// Two separate things happen here, and only the first can block.
	//
	// Crossing the end of the published length needs the growth covering it
	// to have finished, so wait for that. With the next extent staged half an
	// extent ago this is already true and the wait is a load.
	if w.pos+n > w.committed {
		if err := w.crossInto(w.pos + n); err != nil {
			return nil, err
		}
	}
	// Then stage the next growth, early, so the crossing after this one is
	// also already done when it arrives. This never waits.
	if w.pos+w.file.growAhead > w.growTarget {
		w.stageGrowth()
	}
	return w.file.data[w.pos : w.pos+n], nil
}

// Commit publishes n bytes from the last reservation.
func (w *Writer) Commit(n uint64) error {
	if w == nil || w.closed || w.file.closed {
		return ErrClosed
	}
	next := w.pos + n
	// The only thing a commit publishes. The housekeeping helper reads this
	// and decides what to stage; the producer never stages anything itself,
	// so a commit costs one release store however much work is due.
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

// stageGrowth asks for the next extent. It never waits for the answer.
func (w *Writer) stageGrowth() {
	if w.growSeq != w.growDone.Load() {
		// One outstanding at a time, so a completion number means exactly
		// one length and the producer never has to match them up.
		return
	}
	want := alignUp(w.growTarget+w.file.growAhead, w.file.extent)
	if want > w.file.reserve {
		want = w.file.reserve
	}
	if want <= w.growTarget {
		return
	}
	if w.ring == nil {
		if err := w.allocate(w.growTarget, want-w.growTarget); err != nil {
			return
		}
		w.growTarget = want
		w.growSeq++
		w.growDone.Store(w.growSeq)
		return
	}
	if !w.ring.Fallocate(w.file.fd, 0, int64(w.growTarget), int64(want-w.growTarget)) {
		return
	}
	w.growSeq++
	w.ring.Tag(w.growSeq)
	w.growTarget = want
}

// crossInto waits for the growth that covers need, then publishes the length.
//
// The publish is the producer's, not the helper's. A length the producer has
// not seen completed is a length it must not write into, and a length written
// by anyone else is a word with two writers.
func (w *Writer) crossInto(need uint64) error {
	if w.growTarget < need {
		w.stageGrowth()
	}
	// Wait in time, not in iterations. The other goroutine sleeps when idle,
	// so the producer has to be willing to wait longer than that sleep or it
	// will give up on a growth that was about to land.
	t0 := time.Now()
	deadline := t0.Add(crossPatience)
	for spun := false; ; spun = true {
		if w.growDone.Load() >= w.growSeq && w.growTarget >= need {
			w.committed = w.growTarget
			atmc.StoreReleaseU64(w.file.committed(), w.committed)
			w.Grows.Add(1)
			// Count a wait only when there actually was one. Crossing a
			// growth that already completed is the expected case, and
			// counting it makes the metric say the opposite of the truth.
			if spun {
				w.Waits.Add(1)
				w.WaitNs.Add(uint64(time.Since(t0).Nanoseconds()))
			}
			return nil
		}
		if time.Now().After(deadline) {
			break
		}
		atmc.Relax()
	}
	// The submitting goroutine is not keeping up. Grow here rather than
	// stall without bound, and count it: this is not an error, it is the
	// producer doing work it meant to hand off.
	want := alignUp(need+w.file.growAhead, w.file.extent)
	if want > w.file.reserve {
		want = w.file.reserve
	}
	if err := w.allocate(w.committed, want-w.committed); err != nil {
		return err
	}
	w.growTarget = want
	w.committed = want
	w.growDone.Store(w.growSeq)
	atmc.StoreReleaseU64(w.file.committed(), want)
	w.Grows.Add(1)
	w.SelfGrows.Add(1)
	return nil
}

// allocate preallocates a range. fallocate without FALLOC_FL_KEEP_SIZE both
// allocates the blocks and extends the file, so no truncate is needed.
// Allocating is the point: a store into an unwritten extent would fault on
// the writer instead of here.
func (w *Writer) allocate(off, length uint64) error {
	if length == 0 {
		return nil
	}
	// Deliberately the plain syscall, not the ring.
	//
	// Draining the ring to wait for a staged allocation waits for every
	// other operation in it too, and those include the eviction chain, which
	// waits on the device. One growth would then stop the whole housekeeping
	// pipeline rather than just itself. The ring also reports a completion
	// result the caller must inspect, and this path needs an error return,
	// which the syscall already gives.
	err := fallocate(w.file.fd, int64(off), int64(length))
	if errors.Is(err, syscall.ENOSYS) || errors.Is(err, syscall.EOPNOTSUPP) {
		err = syscall.Ftruncate(w.file.fd, int64(off+length))
	}
	return err
}

// runSubmitter makes the system call the producer must not.
//
// Everything else lives on the producer: it decides what is due and stages it
// straight into the submission ring, which costs a handful of memory writes.
// Only io_uring_enter is expensive, and on the measured kernel it costs what
// the staged work costs, because the kernel runs advisory memory work during
// the submitting call rather than on a worker. Timing the parts of one round
// gave 0.3us to reap, 0.2 to stage a populate, 0.1 each to stage a drop and a
// writeback, and 572us to submit.
//
// So the producer stages and this submits. The two never write the same word:
// staging owns the submission ring and its tail, this owns the completion
// ring's head and the call.
func (w *Writer) runSubmitter() {
	defer close(w.helperDone)
	for !w.stop.Load() && !w.file.closed {
		if w.ring.Pending() != 0 {
			if err := w.ring.Submit(); err != nil {
				w.SyncErrors.Add(1)
			}
		}
		w.ring.Reap()
		// Sleep only with nothing to submit and nothing outstanding. Sleeping
		// while work is pending lets the producer fill the submission ring,
		// and a full ring drops the very work that keeps it ahead.
		if w.ring.Pending() == 0 && w.ring.Inflight() == 0 {
			time.Sleep(helperIdleNs)
			continue
		}
		atmc.Relax()
	}
	_ = w.ring.Submit()
	w.ring.Reap()
}

// work stages one round of housekeeping and reports whether it found any.
// It never waits for the kernel.
// onComplete runs on the submitting goroutine for every completion it reaps.
func (w *Writer) onComplete(tag uint64, res int32) {
	if tag == 0 {
		return
	}
	if res < 0 {
		// The allocation failed. Leave the watermark where it is, so the
		// producer waits and then grows the file itself.
		w.GrowErrors.Add(1)
		return
	}
	w.growDone.Store(tag)
}

func (w *Writer) work() {
	t0 := time.Now()
	defer func() { w.WorkNs.Add(uint64(time.Since(t0).Nanoseconds())) }()
	w.nextWork = w.pos + w.file.workBytes
	w.Works.Add(1)
	s := time.Now()
	w.populateAhead()
	w.PopStageNs.Add(uint64(time.Since(s).Nanoseconds()))
	s = time.Now()
	w.dropBehind()
	w.DropStageNs.Add(uint64(time.Since(s).Nanoseconds()))
	s = time.Now()
	w.syncBehind()
	w.evictBehind()
	w.SyncStageNs.Add(uint64(time.Since(s).Nanoseconds()))
	if w.ring != nil {
		w.Dropped.Store(w.ring.Dropped)
	}
}

// populateAhead establishes writable page-table entries ahead of the cursor,
// so the writer does not fault on first touch. A missed populate costs a
// fault, never correctness, so it is staged and never waited on.
func (w *Writer) populateAhead() {
	if w.file.noPopulate {
		return
	}
	pos := w.pos
	target := alignDown(pos+w.file.ahead, w.file.page)
	if target > w.growTarget {
		target = alignDown(w.growTarget, w.file.page)
	}
	if target <= w.populated || target-w.populated < w.file.populateChunk {
		return
	}
	// Stage at most one round's worth. A round runs every WorkBytes, so
	// staging that much keeps pace with the writer exactly, and the margin
	// lets it close a gap without ever emptying the whole backlog into one
	// submission. Draining the backlog in one round is what makes this work
	// bursty: the same total, delivered in a spike the ring and the kernel
	// workers then have to absorb.
	if limit := w.populated + w.file.workBytes*populateCatchUp; target > limit {
		target = limit
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
	// Same bound as populateAhead, and for the same reason.
	if limit := w.dropped + w.file.workBytes*populateCatchUp; target > limit {
		target = limit
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
	w.stop.Store(true)
	if w.helperDone != nil {
		<-w.helperDone
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
