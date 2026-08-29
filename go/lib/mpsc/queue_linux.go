//go:build linux && (amd64 || arm64)

package mpsc

import (
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/akalsi-org/polyglot-template/go/lib/hostcpu"
)

type status uint8

const (
	statusOK status = iota
	statusFull
	statusReaderDead
	statusTooLarge
	statusNoSlot
	statusPositionExhausted
	statusContended
	statusMisuse
	statusRecovered
)

var (
	// ErrBusy reports that a live handle owns the requested role.
	ErrBusy = errors.New("mpsc: role is owned by a live thread")
	// ErrMisuse reports an invalid handle, thread, span, or operation sequence.
	ErrMisuse = errors.New("mpsc: invalid handle operation")
	// ErrContended reports retryable bounded MPSC admission contention.
	ErrContended = errors.New("mpsc: admission contended")
	// ErrRecovered reports that reader recovery invalidated a producer span.
	ErrRecovered = errors.New("mpsc: reservation recovered")
)

const (
	stateFree    uint64 = 0
	stateClaimed uint64 = 1
	stateCleared uint64 = 2
	stateAborted uint64 = 4
	stateMask           = 7
	invalidPos          = ^uint64(0)
)

func freeWord(pos uint64) uint64 { return (((pos >> 6) & ((1 << 58) - 1)) << 3) }
func extentOf(w uint64) uint64   { return ((w >> 3) & ((1 << 26) - 1)) * cacheLine }
func tidOf(w uint64) uint32      { return uint32((w >> 35) & ((1 << 22) - 1)) }
func packrecord(extent uint64, state uint64, tid uint32) uint64 {
	return state | ((extent/cacheLine)&((1<<26)-1))<<3 | (uint64(tid)&((1<<22)-1))<<35
}
func withState(w, state uint64) uint64 { return (w &^ stateMask) | state }
func tagBase(pos uint64) uint64        { return ((pos >> 6) & ((1 << 58) - 1)) << 2 }
func reservationTag(pos uint64) uint64 { return tagBase(pos) | 1 }
func committingTag(pos uint64) uint64  { return tagBase(pos) | 2 }
func commitTag(pos uint64) uint64      { return tagBase(pos) | 3 }
func extentFor(n uint64, grain uint64) (uint64, bool) {
	if n > maxExtent {
		return 0, false
	}
	e := (n + grain - 1) &^ (grain - 1)
	if e == 0 {
		e = grain
	}
	return e, e <= maxExtent
}

var (
	threadAliveProbe      = hostcpu.ThreadAlive
	procfsValidationProbe = hostcpu.ValidateProcfsPIDNamespace
)

type reservation struct {
	bytes []byte
	pos   uint64
	need  uint64
}

func (r reservation) Bytes() []byte { return r.bytes }

type record struct {
	bytes  []byte
	pos    uint64
	extent uint64
}

func (r record) Bytes() []byte { return r.bytes }

// MPSC owns an attached many-producer, single-consumer shared queue.
type mpscHot struct {
	hint, readPos             *uint64
	readerTid                 *uint32
	claimBase, resultBase     unsafe.Pointer
	arenaBase                 unsafe.Pointer
	capacity, planeMask       uint64
	grainMask, maxNeed        uint64
	planeShift, planeGrain    uint
	claimStride, resultStride uintptr
}

func bindMPSCHot(r *region) mpscHot {
	base := unsafe.Pointer(&r.control[0])
	return mpscHot{
		hint:         (*uint64)(unsafe.Add(base, controlSize)),
		readPos:      (*uint64)(unsafe.Add(base, controlSize+128)),
		readerTid:    (*uint32)(unsafe.Add(base, 28)),
		claimBase:    unsafe.Add(base, r.claimBase()),
		resultBase:   unsafe.Add(base, r.resultBase()),
		arenaBase:    unsafe.Pointer(&r.mirroredArena[0]),
		capacity:     r.capacity,
		planeMask:    r.planeMask,
		grainMask:    uint64(r.planeGrain - 1),
		maxNeed:      r.capacity - uint64(r.planeGrain),
		planeShift:   r.planeShift,
		planeGrain:   uint(r.planeGrain),
		claimStride:  uintptr(r.claimStride),
		resultStride: uintptr(r.resultStride),
	}
}

func (h mpscHot) cell(pos uint64) uintptr {
	return uintptr((pos >> h.planeShift) & h.planeMask)
}

func (h mpscHot) validExtent(extent uint64) bool {
	return extent != 0 && extent&h.grainMask == 0 && extent <= h.maxNeed
}
func (h mpscHot) claim(pos uint64) *uint64 {
	return (*uint64)(unsafe.Add(h.claimBase, h.cell(pos)*h.claimStride))
}
func (h mpscHot) result(pos uint64) (*uint64, *uint64) {
	base := unsafe.Add(h.resultBase, h.cell(pos)*h.resultStride)
	return (*uint64)(base), (*uint64)(unsafe.Add(base, 8))
}
func (h mpscHot) payload(pos, n uint64) []byte {
	start := pos & (h.capacity - 1)
	return unsafe.Slice((*byte)(unsafe.Add(h.arenaBase, uintptr(start))), int(n))
}

type MPSC struct {
	_       noCopy
	self    *MPSC
	r       *region
	hot     mpscHot
	life    sync.Mutex
	closing bool
	active  atomic.Int32
	handles map[localHandle]struct{}
}

// CreateMPSC creates a format-v4 MPSC queue.
func CreateMPSC(cfg Config) (*MPSC, error) {
	if cfg.Capacity == 0 {
		cfg.Capacity = 1 << 20
	}
	geometry, ok := mpscGeometry(cfg.MPSCLayout)
	if !ok {
		return nil, ErrFormat
	}
	r, err := createRegion(cfg, geometry.claimStride, geometry.resultStride, geometry.grain)
	if err != nil {
		return nil, err
	}
	q := &MPSC{r: r, hot: bindMPSCHot(r), handles: make(map[localHandle]struct{})}
	q.self = q
	return q, nil
}

// AttachMPSC attaches to a format-v4 MPSC queue through fd.
func AttachMPSC(fd int) (*MPSC, error) {
	r, err := attachRegion(fd)
	if err != nil {
		return nil, err
	}
	if !isMPSCGeometry(r.claimStride, r.resultStride, r.planeGrain) {
		r.close()
		return nil, ErrFormat
	}
	q := &MPSC{r: r, hot: bindMPSCHot(r), handles: make(map[localHandle]struct{})}
	q.self = q
	return q, nil
}

func (q *MPSC) valid() bool { return q != nil && q.self == q }

func (q *MPSC) ownerAlive(tid hostcpu.ThreadId) bool {
	return tid != 0 && threadAliveProbe(tid)
}

func (q *MPSC) dupFD() (int, error) {
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
func (q *MPSC) Capacity() uint64 {
	if !q.valid() || q.r == nil {
		return 0
	}
	return q.r.capacity
}

func (q *MPSC) reapDeadHandles() {
	for handle := range q.handles {
		if handle.reapDead() {
			delete(q.handles, handle)
			q.active.Add(-1)
		}
	}
}

// Close unmaps the queue after all handles close.
func (q *MPSC) Close() error {
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
func (q *MPSC) newWriter() (*mpscWriter, error) {
	if !q.valid() {
		return nil, ErrMisuse
	}
	if q.r == nil || q.r.closed {
		return nil, ErrClosed
	}
	runtime.LockOSThread()
	identity := hostcpu.CurrentThreadIdentity()
	return &mpscWriter{q: q, hot: q.hot, identity: identity, tid: identity.ThreadId(), pos: invalidPos, fullThreshold: 128}, nil
}
func (q *MPSC) newReader() (*mpscReader, error) {
	if !q.valid() {
		return nil, ErrMisuse
	}
	if q.r == nil || q.r.closed {
		return nil, ErrClosed
	}
	runtime.LockOSThread()
	identity := hostcpu.CurrentThreadIdentity()
	self := identity.ThreadId()
	p := q.hot.readerTid
	cur := loadAcquire32(p)
	if cur == uint32(self) {
		runtime.UnlockOSThread()
		return nil, ErrBusy
	}
	if cur != uint32(self) {
		if q.ownerAlive(hostcpu.ThreadId(cur)) {
			runtime.UnlockOSThread()
			return nil, ErrBusy
		}
		if !compareAndSwap32(p, cur, uint32(self)) {
			runtime.UnlockOSThread()
			return nil, ErrBusy
		}
	}
	return &mpscReader{q: q, hot: q.hot, identity: identity, tid: self, rd: loadAcquire64(q.hot.readPos), busyPos: invalidPos, busyThreshold: 128}, nil
}

func (q *MPSC) ptr32(off uintptr) *uint32   { return (*uint32)(unsafe.Pointer(&q.r.control[off])) }
func (q *MPSC) ptr64(off uintptr) *uint64   { return (*uint64)(unsafe.Pointer(&q.r.control[off])) }
func (q *MPSC) planeCell(pos uint64) uint64 { return uint64(q.hot.cell(pos)) }
func (q *MPSC) claim(pos uint64) *uint64    { return q.hot.claim(pos) }
func (q *MPSC) result(pos uint64) (*uint64, *uint64) {
	return q.hot.result(pos)
}
func (q *MPSC) payload(pos, n uint64) []byte { return q.hot.payload(pos, n) }

type mpscWriter struct {
	q                                   *MPSC
	hot                                 mpscHot
	identity                            hostcpu.ThreadIdentity
	tid                                 hostcpu.ThreadId
	pos, need, maximum, word, readCache uint64
	status                              status
	fullStreak, fullThreshold           uint32
	closed                              bool
}

func (w *mpscWriter) Reserve(n int) (reservation, status) {
	return w.reserveOwned(n)
}

//go:noinline
func (w *mpscWriter) reserveOwned(n int) (reservation, status) {
	if hostcpu.CurrentThreadIdentity() != w.identity {
		return reservation{}, statusMisuse
	}
	return w.reserveFast(n)
}

//go:noinline
func (w *mpscWriter) reserveFast(n int) (reservation, status) {
	if w.closed || w.pos != invalidPos || n < 0 {
		return w.reserveSlow(n)
	}
	grain := uint64(w.hot.planeGrain)
	need, ok := extentFor(uint64(n), grain)
	if !ok || need > w.hot.capacity-grain {
		return w.reserveSlow(n)
	}
	p := loadAcquire64(w.hot.hint)
	if p > ^uint64(0)-need-grain || p < w.readCache || p+need+grain-w.readCache > w.hot.capacity {
		return w.reserveSlow(n)
	}
	word := freeWord(p)
	mine := packrecord(need, stateClaimed, uint32(w.tid))
	if !compareAndSwapAcquire64(w.hot.claim(p), word, mine) {
		return w.reserveSlow(n)
	}
	storeRelaxed64(w.hot.claim(p+need), freeWord(p+need))
	_, tag := w.hot.result(p)
	storeRelaxed64(tag, reservationTag(p))
	storeRelease64(w.hot.claim(p), withState(mine, stateCleared))
	storeRelease64(w.hot.hint, p+need)
	w.pos, w.need, w.maximum, w.word, w.status = p, need, uint64(n), mine, statusOK
	w.fullStreak = 0
	return reservation{bytes: w.hot.payload(p, uint64(n)), pos: p, need: need}, w.status
}

//go:noinline
func (w *mpscWriter) reserveSlow(n int) (reservation, status) {
	if w.closed || w.pos != invalidPos {
		w.status = statusMisuse
		return reservation{}, w.status
	}
	if n < 0 {
		w.status = statusTooLarge
		return reservation{}, w.status
	}
	grain := uint64(w.hot.planeGrain)
	need, ok := extentFor(uint64(n), grain)
	if !ok || need > w.hot.capacity-grain {
		w.status = statusTooLarge
		return reservation{}, w.status
	}
	cap := w.hot.capacity
	maxHops := cap / grain
	for attempt := 0; attempt < 4; attempt++ {
		p := loadAcquire64(w.hot.hint)
		if p > ^uint64(0)-need-grain {
			w.status = statusPositionExhausted
			return reservation{}, w.status
		}
		prevClaimed := false
		for hops := uint64(0); ; hops++ {
			if hops >= maxHops {
				break
			}
			if p > ^uint64(0)-need-grain {
				w.status = statusPositionExhausted
				return reservation{}, w.status
			}
			word := loadAcquire64(w.hot.claim(p))
			state := word & stateMask
			if state == stateFree {
				if word != freeWord(p) || prevClaimed {
					break
				}
				if p < w.readCache {
					break
				}
				if p+need+grain-w.readCache > cap {
					previousRead := w.readCache
					w.readCache = loadAcquire64(w.hot.readPos)
					if w.readCache != previousRead {
						w.fullStreak = 0
					}
					if p < w.readCache {
						break
					}
					if p+need+grain-w.readCache > cap {
						w.status = w.fullStatus(hostcpu.ThreadId(loadAcquire32(w.hot.readerTid)))
						return reservation{}, w.status
					}
				}
				mine := packrecord(need, stateClaimed, uint32(w.tid))
				if !compareAndSwapAcquire64(w.hot.claim(p), word, mine) {
					continue
				}
				storeRelaxed64(w.hot.claim(p+need), freeWord(p+need))
				_, tag := w.hot.result(p)
				// The Claim release publishes this relaxed reservation tag to recovery.
				storeRelaxed64(tag, reservationTag(p))
				storeRelease64(w.hot.claim(p), withState(mine, stateCleared))
				storeRelease64(w.hot.hint, p+need)
				w.pos, w.need, w.maximum, w.word, w.status = p, need, uint64(n), mine, statusOK
				w.fullStreak = 0
				r := reservation{bytes: w.hot.payload(p, uint64(n)), pos: p, need: need}
				return r, w.status
			}
			prevClaimed = state == stateClaimed
			e := extentOf(word)
			if !w.hot.validExtent(e) {
				break
			}
			if p > ^uint64(0)-e {
				w.status = statusPositionExhausted
				return reservation{}, w.status
			}
			p += e
		}
	}
	w.status = statusContended
	return reservation{}, w.status
}
func (w *mpscWriter) fullStatus(reader hostcpu.ThreadId) status {
	if w.fullThreshold == 0 {
		w.fullThreshold = 128
	}
	if w.fullStreak != 0 {
		w.fullStreak++
		if w.fullStreak < w.fullThreshold {
			return statusFull
		}
		w.fullStreak = 0
		if w.fullThreshold < 65536 {
			w.fullThreshold *= 2
		}
	}
	if reader != 0 && !w.q.ownerAlive(reader) {
		return statusReaderDead
	}
	w.fullStreak = 1
	return statusFull
}

// The reservation-tag CAS fences stale producers before metadata writes.
// The final tag release publishes the payload and relaxed length together.
func (w *mpscWriter) Commit(r reservation, actual int) error {
	return w.commitOwned(r, actual)
}

//go:noinline
func (w *mpscWriter) commitOwned(r reservation, actual int) error {
	if hostcpu.CurrentThreadIdentity() != w.identity {
		return ErrMisuse
	}
	return w.commitFast(r, actual)
}

//go:noinline
func (w *mpscWriter) commitFast(r reservation, actual int) error {
	if w.closed || w.pos != r.pos || w.need != r.need || actual < 0 || uint64(actual) > w.maximum {
		return commitMisuse()
	}
	length, tag := w.hot.result(w.pos)
	if !compareAndSwap64(tag, reservationTag(w.pos), committingTag(w.pos)) {
		return w.commitRecovered()
	}
	storeRelaxed64(length, uint64(actual))
	storeRelease64(tag, commitTag(w.pos))
	w.pos = invalidPos
	return nil
}

//go:noinline
func commitMisuse() error {
	return ErrMisuse
}

//go:noinline
func (w *mpscWriter) commitRecovered() error {
	w.pos = invalidPos
	return ErrRecovered
}
func (w *mpscWriter) Abort(r reservation) error {
	return w.abortOwned(r)
}

//go:noinline
func (w *mpscWriter) abortOwned(r reservation) error {
	if hostcpu.CurrentThreadIdentity() != w.identity {
		return ErrMisuse
	}
	if w.closed || w.pos != r.pos || w.need != r.need {
		return ErrMisuse
	}
	_, tag := w.hot.result(w.pos)
	if !compareAndSwap64(tag, reservationTag(w.pos), 0) {
		w.pos = invalidPos
		return ErrRecovered
	}
	storeRelease64(w.hot.claim(w.pos), withState(w.word, stateAborted))
	w.pos = invalidPos
	return nil
}

func (w *mpscWriter) Write(p []byte) status {
	return w.writeOwned(p)
}

//go:noinline
func (w *mpscWriter) writeOwned(p []byte) status {
	if hostcpu.CurrentThreadIdentity() != w.identity {
		return statusMisuse
	}
	return w.writeFast(p)
}

//go:noinline
func (w *mpscWriter) writeFast(p []byte) status {
	r, s := w.reserveFast(len(p))
	if s != statusOK {
		return s
	}
	copy(r.bytes, p)
	length, tag := w.hot.result(w.pos)
	if !compareAndSwap64(tag, reservationTag(w.pos), committingTag(w.pos)) {
		w.pos = invalidPos
		return statusRecovered
	}
	storeRelaxed64(length, uint64(len(p)))
	storeRelease64(tag, commitTag(w.pos))
	w.pos = invalidPos
	return statusOK
}
func (w *mpscWriter) Close() error {
	if w.closed {
		return nil
	}
	if hostcpu.CurrentThreadIdentity() != w.identity {
		return ErrMisuse
	}
	if w.pos != invalidPos {
		_ = w.Abort(reservation{pos: w.pos, need: w.need})
	}
	w.closed = true
	runtime.UnlockOSThread()
	return nil
}

type mpscReader struct {
	q                         *MPSC
	hot                       mpscHot
	identity                  hostcpu.ThreadIdentity
	tid                       hostcpu.ThreadId
	rd, peekExtent, busyPos   uint64
	busyStreak, busyThreshold uint32
	closed                    bool
}

func (r *mpscReader) Peek() (record, bool, error) {
	return r.peekOwned()
}

//go:noinline
func (r *mpscReader) peekOwned() (record, bool, error) {
	if hostcpu.CurrentThreadIdentity() != r.identity {
		return record{}, false, ErrMisuse
	}
	return r.peekEmptyFast()
}

//go:noinline
func (r *mpscReader) peekEmptyFast() (record, bool, error) {
	word := loadAcquire64(r.hot.claim(r.rd))
	if !r.closed && r.peekExtent == 0 && word&stateMask == stateFree {
		return record{}, false, nil
	}
	return r.peekFast(word)
}

//go:noinline
func (r *mpscReader) peekFast(word uint64) (record, bool, error) {
	if r.closed || r.peekExtent != 0 {
		return r.peekSlow()
	}
	if word&stateMask != stateCleared {
		return r.peekSlow()
	}
	extent := extentOf(word)
	if !r.hot.validExtent(extent) || r.rd > ^uint64(0)-extent {
		return r.peekSlow()
	}
	length, tag := r.hot.result(r.rd)
	if loadAcquire64(tag) != commitTag(r.rd) {
		return r.peekSlow()
	}
	n := loadRelaxed64(length)
	if n > extent || n > maxExtent {
		return r.peekSlow()
	}
	r.peekExtent = extent
	return record{bytes: r.hot.payload(r.rd, n), pos: r.rd, extent: extent}, true, nil
}

//go:noinline
func (r *mpscReader) peekSlow() (record, bool, error) {
	if r.closed || r.peekExtent != 0 {
		return record{}, false, ErrMisuse
	}
	for {
		w := loadAcquire64(r.hot.claim(r.rd))
		state := w & stateMask
		if state == stateFree {
			return record{}, false, nil
		}
		if state != stateClaimed && state != stateCleared && state != stateAborted {
			return record{}, false, ErrFormat
		}
		extent := extentOf(w)
		if !r.hot.validExtent(extent) {
			return record{}, false, ErrFormat
		}
		if r.rd > ^uint64(0)-extent {
			return record{}, false, ErrPositionExhausted
		}
		if state == stateAborted {
			r.rd += extent
			storeRelease64(r.hot.readPos, r.rd)
			continue
		}
		length, tag := r.hot.result(r.rd)
		if loadAcquire64(tag) == commitTag(r.rd) {
			n := loadRelaxed64(length)
			if n > extent || n > maxExtent {
				return record{}, false, ErrFormat
			}
			r.peekExtent = extent
			return record{bytes: r.hot.payload(r.rd, n), pos: r.rd, extent: extent}, true, nil
		}
		if r.busyPos != r.rd {
			r.busyPos = r.rd
			r.busyStreak = 0
			r.busyThreshold = 128
		}
		r.busyStreak++
		if r.busyStreak < r.busyThreshold {
			return record{}, false, nil
		}
		r.busyStreak = 0
		if r.q.ownerAlive(hostcpu.ThreadId(tidOf(w))) {
			if r.busyThreshold < 65536 {
				r.busyThreshold *= 2
			}
			return record{}, false, nil
		}
		if loadAcquire64(tag) == commitTag(r.rd) {
			continue
		}
		fresh := loadAcquire64(r.hot.claim(r.rd))
		fs := fresh & stateMask
		if fs == stateFree {
			continue
		}
		if fs != stateClaimed && fs != stateCleared && fs != stateAborted {
			return record{}, false, ErrFormat
		}
		freshExtent := extentOf(fresh)
		if !r.hot.validExtent(freshExtent) {
			return record{}, false, ErrFormat
		}
		if r.rd > ^uint64(0)-freshExtent {
			return record{}, false, ErrPositionExhausted
		}
		// A commit tag acquires payload publication. A committing tag has no visibility guarantee.
		freshTag := loadAcquire64(tag)
		if freshTag == commitTag(r.rd) {
			continue
		}
		if fs == stateClaimed {
			storeRelaxed64(tag, 0)
			storeRelaxed64(r.hot.claim(r.rd+freshExtent), freeWord(r.rd+freshExtent))
			compareAndSwap64(r.hot.claim(r.rd), fresh, withState(fresh, stateAborted))
			continue
		}
		if fs == stateCleared {
			if !compareAndSwap64(tag, freshTag, 0) {
				continue
			}
			compareAndSwap64(r.hot.claim(r.rd), fresh, withState(fresh, stateAborted))
			continue
		}
	}
}
func (r *mpscReader) Pop() error {
	return r.popOwned()
}

//go:noinline
func (r *mpscReader) popOwned() error {
	if hostcpu.CurrentThreadIdentity() != r.identity {
		return ErrMisuse
	}
	return r.popFast()
}

//go:noinline
func (r *mpscReader) popFast() error {
	if r.closed || r.peekExtent == 0 || r.rd > ^uint64(0)-r.peekExtent {
		return r.popSlow()
	}
	r.rd += r.peekExtent
	r.peekExtent = 0
	storeRelease64(r.hot.readPos, r.rd)
	return nil
}

//go:noinline
func (r *mpscReader) popSlow() error {
	if r.closed || r.peekExtent == 0 {
		return ErrMisuse
	}
	return ErrPositionExhausted
}
func (r *mpscReader) Close() error {
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
