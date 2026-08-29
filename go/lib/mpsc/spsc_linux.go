//go:build linux && (amd64 || arm64)

package mpsc

import (
	"runtime"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/akalsi-org/polyglot-template/go/lib/hostcpu"
)

type spscHot struct {
	tail, readPos          *uint64
	writerBitmap           *uint64
	readerTid, writerOwner *uint32
	generation             *uint32
	slotBase, arenaBase    unsafe.Pointer
	capacity, planeMask    uint64
	planeShift             uint
	slotStride             uintptr
}

func bindSPSCHot(r *region) spscHot {
	base := unsafe.Pointer(&r.control[0])
	return spscHot{
		tail:         (*uint64)(unsafe.Add(base, controlSize)),
		readPos:      (*uint64)(unsafe.Add(base, controlSize+128)),
		writerBitmap: (*uint64)(unsafe.Add(base, controlSize+shardSize)),
		readerTid:    (*uint32)(unsafe.Add(base, 28)),
		writerOwner:  (*uint32)(unsafe.Add(base, controlSize+shardSize+8)),
		generation:   (*uint32)(unsafe.Add(base, controlSize+shardSize+12)),
		slotBase:     unsafe.Add(base, r.claimBase()),
		arenaBase:    unsafe.Pointer(&r.mirroredArena[0]),
		capacity:     r.capacity,
		planeMask:    r.planeMask,
		planeShift:   r.planeShift,
		slotStride:   uintptr(r.claimStride),
	}
}

func (h spscHot) slot(pos uint64) *uint64 {
	cell := uintptr((pos >> h.planeShift) & h.planeMask)
	return (*uint64)(unsafe.Add(h.slotBase, cell*h.slotStride))
}

func (h spscHot) payload(pos, n uint64) []byte {
	start := pos & (h.capacity - 1)
	return unsafe.Slice((*byte)(unsafe.Add(h.arenaBase, uintptr(start))), int(n))
}

// SPSC owns an attached single-producer, single-consumer shared queue.
type SPSC struct {
	_       noCopy
	self    *SPSC
	r       *region
	hot     spscHot
	life    sync.Mutex
	closing bool
	active  atomic.Int32
	handles map[localHandle]struct{}
}

// CreateSPSC creates a format-v4 SPSC queue.
func CreateSPSC(cfg Config) (*SPSC, error) {
	if cfg.Capacity == 0 {
		cfg.Capacity = 1 << 20
	}
	r, err := createRegion(cfg, 8, 0, 64)
	if err != nil {
		return nil, err
	}
	q := &SPSC{r: r, hot: bindSPSCHot(r), handles: make(map[localHandle]struct{})}
	q.self = q
	return q, nil
}

// AttachSPSC attaches to a format-v4 SPSC queue through fd.
func AttachSPSC(fd int) (*SPSC, error) {
	r, err := attachRegion(fd)
	if err != nil {
		return nil, err
	}
	if r.claimStride != 8 || r.resultStride != 0 || r.planeGrain != 64 {
		r.close()
		return nil, ErrFormat
	}
	q := &SPSC{r: r, hot: bindSPSCHot(r), handles: make(map[localHandle]struct{})}
	q.self = q
	return q, nil
}
func (q *SPSC) valid() bool { return q != nil && q.self == q }

func (q *SPSC) ownerAlive(tid hostcpu.ThreadId) bool {
	return tid != 0 && threadAliveProbe(tid)
}

func (q *SPSC) dupFD() (int, error) {
	if q == nil {
		return -1, ErrClosed
	}
	if !q.valid() {
		return -1, ErrMisuse
	}
	q.life.Lock()
	defer q.life.Unlock()
	if q.closing || q.r == nil {
		return -1, ErrClosed
	}
	return q.r.fdDup()
}

// Capacity returns the normalized payload capacity in bytes.
func (q *SPSC) Capacity() uint64 {
	if !q.valid() || q.r == nil {
		return 0
	}
	return q.r.capacity
}

func (q *SPSC) reapDeadHandles() {
	for handle := range q.handles {
		if handle.reapDead() {
			delete(q.handles, handle)
			q.active.Add(-1)
		}
	}
}

// Close unmaps the queue after all handles close.
func (q *SPSC) Close() error {
	if q == nil {
		return nil
	}
	if !q.valid() {
		return ErrMisuse
	}
	q.life.Lock()
	defer q.life.Unlock()
	q.reapDeadHandles()
	if q.active.Load() != 0 {
		return ErrBusy
	}
	q.closing = true
	return q.r.close()
}
func (q *SPSC) ptr32(off uintptr) *uint32    { return (*uint32)(unsafe.Pointer(&q.r.control[off])) }
func (q *SPSC) ptr64(off uintptr) *uint64    { return (*uint64)(unsafe.Pointer(&q.r.control[off])) }
func (q *SPSC) slot(pos uint64) *uint64      { return q.hot.slot(pos) }
func (q *SPSC) payload(pos, n uint64) []byte { return q.hot.payload(pos, n) }

const spscWriterDetaching uint32 = 1 << 31

func spscWriterTid(owner uint32) hostcpu.ThreadId {
	return hostcpu.ThreadId(owner &^ spscWriterDetaching)
}

func (q *SPSC) newWriter() (*spscWriter, error) {
	if !q.valid() {
		return nil, ErrMisuse
	}
	if q.r == nil || q.r.closed {
		return nil, ErrClosed
	}
	runtime.LockOSThread()
	identity := hostcpu.CurrentThreadIdentity()
	tid := identity.ThreadId()
	owner := q.hot.writerOwner
	cur := loadAcquire32(owner)
	takeover := cur != 0 || loadAcquire64(q.hot.writerBitmap)&1 != 0
	if q.ownerAlive(spscWriterTid(cur)) {
		runtime.UnlockOSThread()
		return nil, ErrBusy
	}
	if takeover && loadAcquire64(q.hot.readPos) != loadAcquire64(q.hot.tail) {
		runtime.UnlockOSThread()
		return nil, ErrBusy
	}
	if !compareAndSwap32(owner, cur, uint32(tid)) {
		runtime.UnlockOSThread()
		return nil, ErrBusy
	}
	fetchOrAcqRel64(q.hot.writerBitmap, 1)
	if takeover {
		fetchAddAcqRel32(q.hot.generation, 1)
	}
	generation := loadAcquire32(q.hot.generation)
	return &spscWriter{q: q, hot: q.hot, identity: identity, tid: tid, generation: generation, tail: loadAcquire64(q.hot.tail), readCache: loadAcquire64(q.hot.readPos), pos: invalidPos, fullThreshold: 1}, nil
}
func (q *SPSC) newReader() (*spscReader, error) {
	if !q.valid() {
		return nil, ErrMisuse
	}
	if q.r == nil || q.r.closed {
		return nil, ErrClosed
	}
	runtime.LockOSThread()
	identity := hostcpu.CurrentThreadIdentity()
	tid := identity.ThreadId()
	p := q.hot.readerTid
	cur := loadAcquire32(p)
	if cur == uint32(tid) {
		runtime.UnlockOSThread()
		return nil, ErrBusy
	}
	if cur != uint32(tid) {
		if q.ownerAlive(hostcpu.ThreadId(cur)) {
			runtime.UnlockOSThread()
			return nil, ErrBusy
		}
		if !compareAndSwap32(p, cur, uint32(tid)) {
			runtime.UnlockOSThread()
			return nil, ErrBusy
		}
	}
	rd := loadAcquire64(q.hot.readPos)
	return &spscReader{q: q, hot: q.hot, identity: identity, tid: tid, rd: rd, tailCache: rd}, nil
}

type spscWriter struct {
	q                                     *SPSC
	hot                                   spscHot
	identity                              hostcpu.ThreadIdentity
	tid                                   hostcpu.ThreadId
	tail, readCache, pos, need, maximum   uint64
	status                                status
	generation, fullStreak, fullThreshold uint32
	closed                                bool
}

func (w *spscWriter) ownsWriter() bool {
	return loadAcquire32(w.hot.writerOwner) == uint32(w.tid) &&
		loadAcquire32(w.hot.generation) == w.generation
}

func (w *spscWriter) Reserve(n int) (reservation, status) {
	return w.reserveOwned(n)
}

//go:noinline
func (w *spscWriter) reserveOwned(n int) (reservation, status) {
	if hostcpu.CurrentThreadIdentity() != w.identity {
		return reservation{}, statusMisuse
	}
	if w.closed || !w.ownsWriter() || w.pos != invalidPos {
		w.status = statusMisuse
		return reservation{}, w.status
	}
	if n < 0 {
		w.status = statusTooLarge
		return reservation{}, w.status
	}
	need, ok := extentFor(uint64(n), 64)
	if !ok || need > w.hot.capacity {
		w.status = statusTooLarge
		return reservation{}, w.status
	}
	if w.tail > ^uint64(0)-need {
		w.status = statusPositionExhausted
		return reservation{}, w.status
	}
	if w.tail+need-w.readCache > w.hot.capacity {
		previousRead := w.readCache
		w.readCache = loadAcquire64(w.hot.readPos)
		if w.readCache != previousRead {
			w.fullStreak = 0
			w.fullThreshold = 1
		}
		if w.tail+need-w.readCache > w.hot.capacity {
			w.status = w.fullStatus(hostcpu.ThreadId(loadRelaxed32(w.hot.readerTid)))
			return reservation{}, w.status
		}
	}
	w.pos, w.need, w.maximum, w.status = w.tail, need, uint64(n), statusOK
	w.fullStreak = 0
	return reservation{bytes: w.hot.payload(w.pos, uint64(n)), pos: w.pos, need: need}, w.status
}
func (w *spscWriter) fullStatus(reader hostcpu.ThreadId) status {
	w.fullStreak++
	if w.fullThreshold == 0 {
		w.fullThreshold = 1
	}
	if w.fullStreak < w.fullThreshold {
		return statusFull
	}
	if w.fullThreshold < 65536 {
		w.fullThreshold *= 2
	}
	if reader != 0 && !w.q.ownerAlive(reader) {
		return statusReaderDead
	}
	return statusFull
}
func (w *spscWriter) Commit(r reservation, actual int) error {
	return w.commitOwned(r, actual)
}

//go:noinline
func (w *spscWriter) commitOwned(r reservation, actual int) error {
	if hostcpu.CurrentThreadIdentity() != w.identity {
		return ErrMisuse
	}
	if w.closed || !w.ownsWriter() || w.pos != r.pos || w.need != r.need || actual < 0 || uint64(actual) > w.maximum {
		return ErrMisuse
	}
	actualExtent, ok := extentFor(uint64(actual), 64)
	if !ok || actualExtent > w.need || w.pos > ^uint64(0)-actualExtent {
		return ErrMisuse
	}
	storeRelaxed64(w.hot.slot(w.pos), uint64(actual))
	w.tail = w.pos + actualExtent
	storeRelease64(w.hot.tail, w.tail)
	w.pos = invalidPos
	return nil
}
func (w *spscWriter) Abort(r reservation) error {
	return w.abortOwned(r)
}

//go:noinline
func (w *spscWriter) abortOwned(r reservation) error {
	if hostcpu.CurrentThreadIdentity() != w.identity {
		return ErrMisuse
	}
	if w.closed || !w.ownsWriter() || w.pos != r.pos || w.need != r.need {
		return ErrMisuse
	}
	w.pos = invalidPos
	return nil
}

func (w *spscWriter) Write(p []byte) status {
	return w.writeOwned(p)
}

//go:noinline
func (w *spscWriter) writeOwned(p []byte) status {
	if hostcpu.CurrentThreadIdentity() != w.identity {
		return statusMisuse
	}
	return w.writeFast(p)
}

//go:noinline
func (w *spscWriter) writeFast(p []byte) status {
	if w.closed || w.pos != invalidPos || !w.ownsWriter() {
		return w.writeSlow(p)
	}
	n := uint64(len(p))
	need, ok := extentFor(n, 64)
	if !ok || need > w.hot.capacity || w.tail > ^uint64(0)-need || w.tail+need-w.readCache > w.hot.capacity {
		return w.writeSlow(p)
	}
	copy(w.hot.payload(w.tail, n), p)
	storeRelaxed64(w.hot.slot(w.tail), n)
	w.tail += need
	storeRelease64(w.hot.tail, w.tail)
	return statusOK
}

//go:noinline
func (w *spscWriter) writeSlow(p []byte) status {
	r, s := w.Reserve(len(p))
	if s != statusOK {
		return s
	}
	copy(r.bytes, p)
	if w.Commit(r, len(p)) != nil {
		return statusMisuse
	}
	return statusOK
}
func (w *spscWriter) Close() error {
	if w.closed {
		return nil
	}
	if hostcpu.CurrentThreadIdentity() != w.identity {
		return ErrMisuse
	}
	if w.ownsWriter() {
		if w.pos != invalidPos {
			_ = w.Abort(reservation{pos: w.pos, need: w.need})
		}
		detaching := uint32(w.tid) | spscWriterDetaching
		if !compareAndSwap32(w.hot.writerOwner, uint32(w.tid), detaching) {
			return ErrMisuse
		}
		fetchAndRelease64(w.hot.writerBitmap, ^uint64(1))
		if !compareAndSwap32(w.hot.writerOwner, detaching, 0) {
			return ErrMisuse
		}
	}
	w.closed = true
	runtime.UnlockOSThread()
	return nil
}

type spscReader struct {
	q                         *SPSC
	hot                       spscHot
	identity                  hostcpu.ThreadIdentity
	tid                       hostcpu.ThreadId
	rd, tailCache, peekExtent uint64
	closed                    bool
}

func (r *spscReader) Peek() (record, bool, error) {
	return r.peekOwned()
}

//go:noinline
func (r *spscReader) peekOwned() (record, bool, error) {
	if hostcpu.CurrentThreadIdentity() != r.identity {
		return record{}, false, ErrMisuse
	}
	return r.peekFast()
}

//go:noinline
func (r *spscReader) peekFast() (record, bool, error) {
	if !r.closed && r.peekExtent == 0 && r.rd < r.tailCache {
		n := loadRelaxed64(r.hot.slot(r.rd))
		extent, ok := extentFor(n, 64)
		if ok && extent <= r.hot.capacity && r.rd <= ^uint64(0)-extent && extent <= r.tailCache-r.rd {
			r.peekExtent = extent
			return record{bytes: r.hot.payload(r.rd, n), pos: r.rd, extent: extent}, true, nil
		}
	}
	return r.peekSlow()
}

//go:noinline
func (r *spscReader) peekSlow() (record, bool, error) {
	if r.closed || r.peekExtent != 0 {
		return record{}, false, ErrMisuse
	}
	if r.rd >= r.tailCache {
		r.tailCache = loadAcquire64(r.hot.tail)
		if r.rd >= r.tailCache {
			return record{}, false, nil
		}
	}
	n := loadRelaxed64(r.hot.slot(r.rd))
	extent, ok := extentFor(n, 64)
	if !ok || extent > r.hot.capacity || r.rd > ^uint64(0)-extent || extent > r.tailCache-r.rd {
		return record{}, false, ErrFormat
	}
	r.peekExtent = extent
	return record{bytes: r.hot.payload(r.rd, n), pos: r.rd, extent: extent}, true, nil
}
func (r *spscReader) Pop() error {
	return r.popOwned()
}

//go:noinline
func (r *spscReader) popOwned() error {
	if hostcpu.CurrentThreadIdentity() != r.identity {
		return ErrMisuse
	}
	if r.closed || r.peekExtent == 0 {
		return ErrMisuse
	}
	if r.rd > ^uint64(0)-r.peekExtent {
		return ErrPositionExhausted
	}
	r.rd += r.peekExtent
	r.peekExtent = 0
	storeRelease64(r.hot.readPos, r.rd)
	return nil
}
func (r *spscReader) Close() error {
	if r.closed {
		return nil
	}
	if hostcpu.CurrentThreadIdentity() != r.identity {
		return ErrMisuse
	}
	if !compareAndSwap32(r.hot.readerTid, uint32(r.tid), 0) {
		return ErrMisuse
	}
	r.peekExtent = 0
	r.closed = true
	runtime.UnlockOSThread()
	return nil
}
