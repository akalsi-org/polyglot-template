//go:build linux && (amd64 || arm64)

package mpsc

import (
	"math"
	"sync/atomic"

	"github.com/akalsi-org/polyglot-template/go/lib/hostcpu"
)

type noCopy struct{}

func (*noCopy) Lock()   {}
func (*noCopy) Unlock() {}

type localHandle interface {
	reapDead() bool
}

func threadIdentityDead(identity hostcpu.ThreadIdentity) bool {
	return identity != (hostcpu.ThreadIdentity{}) && !threadAliveProbe(identity.ThreadId())
}

//go:noinline
func statusError(status status) error {
	switch status {
	case statusOK:
		return nil
	case statusFull:
		return ErrFull
	case statusReaderDead:
		return ErrReaderDead
	case statusTooLarge:
		return ErrTooLarge
	case statusNoSlot:
		return ErrNoWriterSlot
	case statusPositionExhausted:
		return ErrPositionExhausted
	case statusContended:
		return ErrContended
	case statusMisuse:
		return ErrMisuse
	case statusRecovered:
		return ErrRecovered
	default:
		return ErrMisuse
	}
}

// DupFD duplicates the MPSC backing file descriptor.
// The caller owns the returned descriptor.
func (q *MPSC) DupFD() (int, error) { return q.dupFD() }

// DupFD duplicates the SPSC backing file descriptor.
// The caller owns the returned descriptor.
func (q *SPSC) DupFD() (int, error) { return q.dupFD() }

var nextSpanOwner atomic.Uint64

func newSpanOwner() uint64 {
	owner := nextSpanOwner.Add(1)
	if owner == 0 {
		owner = nextSpanOwner.Add(1)
	}
	return owner
}

// WriteSpan is a writable zero-copy reservation.
// Commit, Abort, or producer Close invalidates it.
type WriteSpan struct {
	bytes []byte
	pos   uint64
	need  uint64
	token uint64
	owner uint64
}

// Bytes returns the writable reserved payload.
func (s WriteSpan) Bytes() []byte { return s.bytes }

// ReadSpan is a read-only zero-copy record view.
// Pop or consumer Close invalidates it.
type ReadSpan struct {
	bytes []byte
}

// Len returns the committed payload length.
func (s ReadSpan) Len() int { return len(s.bytes) }

// CopyTo copies the payload into dst and returns the copied byte count.
func (s ReadSpan) CopyTo(dst []byte) int { return copy(dst, s.bytes) }

// UnsafeBytes returns a zero-copy read-only payload view.
// The caller must not mutate the returned bytes.
func (s ReadSpan) UnsafeBytes() []byte { return s.bytes }

// MPSCProducer is one OS-thread-bound MPSC producer handle.
// Its zero value is not usable.
type MPSCProducer struct {
	_        noCopy
	self     *MPSCProducer
	core     *mpscWriter
	identity hostcpu.ThreadIdentity
	busy     atomic.Int32
	closed   atomic.Bool
	token    uint64
	owner    uint64
}

// NewProducer attaches a producer and locks its goroutine to an OS thread.
func (q *MPSC) NewProducer() (*MPSCProducer, error) {
	if !q.valid() {
		return nil, ErrMisuse
	}
	q.life.Lock()
	defer q.life.Unlock()
	if q.closing {
		return nil, ErrClosed
	}
	core, err := q.newWriter()
	if err != nil {
		return nil, err
	}
	producer := &MPSCProducer{core: core, identity: core.identity, owner: newSpanOwner()}
	producer.self = producer
	q.handles[producer] = struct{}{}
	q.active.Add(1)
	return producer, nil
}

func (p *MPSCProducer) valid() bool { return p != nil && p.self == p }

// Reserve returns a writable span or a retryable admission error.
func (p *MPSCProducer) Reserve(n uint64) (WriteSpan, error) {
	if !p.valid() || p.core == nil || hostcpu.CurrentThreadIdentity() != p.identity || p.closed.Load() || p.core.pos != invalidPos {
		return WriteSpan{}, ErrMisuse
	}
	if n > uint64(math.MaxInt) {
		return WriteSpan{}, ErrTooLarge
	}
	span, status := p.core.Reserve(int(n))
	if status != statusOK {
		return WriteSpan{}, statusError(status)
	}
	p.token++
	if p.token == 0 {
		p.token++
	}
	return WriteSpan{bytes: span.bytes, pos: span.pos, need: span.need, token: p.token, owner: p.owner}, nil
}

// Commit publishes actual bytes from span.
func (p *MPSCProducer) Commit(span WriteSpan, actual uint64) error {
	if !p.valid() || p.core == nil || hostcpu.CurrentThreadIdentity() != p.identity || p.closed.Load() || actual > uint64(math.MaxInt) {
		return ErrMisuse
	}
	if span.token == 0 || span.token != p.token || span.owner != p.owner {
		return ErrMisuse
	}
	return p.core.Commit(reservation{bytes: span.bytes, pos: span.pos, need: span.need}, int(actual))
}

// Abort discards span without publication.
func (p *MPSCProducer) Abort(span WriteSpan) error {
	if !p.valid() || p.core == nil || hostcpu.CurrentThreadIdentity() != p.identity || p.closed.Load() {
		return ErrMisuse
	}
	if span.token == 0 || span.token != p.token || span.owner != p.owner {
		return ErrMisuse
	}
	return p.core.Abort(reservation{pos: span.pos, need: span.need})
}

// Write reserves, copies, and commits one record.
func (p *MPSCProducer) Write(src []byte) error {
	if p == nil || p.self != p || p.core == nil {
		return ErrMisuse
	}
	return mpscWrite(p, src)
}

//go:noinline
func mpscWrite(producer *MPSCProducer, src []byte) error {
	if hostcpu.CurrentThreadIdentity() != producer.identity {
		return ErrMisuse
	}
	status := producer.core.Write(src)
	if status == statusOK {
		return nil
	}
	return statusError(status)
}

// Close auto-aborts a live span and releases the producer.
func (p *MPSCProducer) Close() error {
	if p == nil {
		return nil
	}
	if !p.valid() || p.core == nil || hostcpu.CurrentThreadIdentity() != p.identity {
		return ErrMisuse
	}
	if p.closed.Load() {
		return nil
	}
	if !p.busy.CompareAndSwap(0, 1) {
		return ErrMisuse
	}
	defer p.busy.Store(0)
	q := p.core.q
	q.life.Lock()
	defer q.life.Unlock()
	if err := p.core.Close(); err != nil {
		return err
	}
	p.token++
	p.closed.Store(true)
	delete(q.handles, p)
	q.active.Add(-1)
	return nil
}

func (p *MPSCProducer) reapDead() bool {
	if !p.valid() || p.core == nil || p.closed.Load() || !threadIdentityDead(p.identity) {
		return false
	}
	p.core.closed = true
	p.core.pos = invalidPos
	p.token++
	p.closed.Store(true)
	p.identity = hostcpu.ThreadIdentity{}
	p.self = nil
	return true
}

// MPSCConsumer is the single OS-thread-bound MPSC consumer handle.
// Its zero value is not usable.
type MPSCConsumer struct {
	_        noCopy
	self     *MPSCConsumer
	core     *mpscReader
	identity hostcpu.ThreadIdentity
	busy     atomic.Int32
	closed   atomic.Bool
}

// AttachConsumer attaches the single consumer.
func (q *MPSC) AttachConsumer() (*MPSCConsumer, error) {
	if !q.valid() {
		return nil, ErrMisuse
	}
	q.life.Lock()
	defer q.life.Unlock()
	if q.closing {
		return nil, ErrClosed
	}
	core, err := q.newReader()
	if err != nil {
		return nil, err
	}
	consumer := &MPSCConsumer{core: core, identity: core.identity}
	consumer.self = consumer
	q.handles[consumer] = struct{}{}
	q.active.Add(1)
	return consumer, nil
}

func (c *MPSCConsumer) valid() bool { return c != nil && c.self == c }

// Peek returns the next committed record without advancing the queue.
func (c *MPSCConsumer) Peek() (ReadSpan, bool, error) {
	return mpscPeek(c)
}

//go:noinline
func mpscPeek(consumer *MPSCConsumer) (ReadSpan, bool, error) {
	if !consumer.valid() || consumer.core == nil || hostcpu.CurrentThreadIdentity() != consumer.identity {
		return ReadSpan{}, false, ErrMisuse
	}
	span, ok, err := consumer.core.Peek()
	if err != nil || !ok {
		return ReadSpan{}, false, err
	}
	return ReadSpan{bytes: span.bytes}, true, nil
}

// Pop advances past the outstanding peek and invalidates its span.
func (c *MPSCConsumer) Pop() error {
	if c == nil || c.self != c || c.core == nil {
		return ErrMisuse
	}
	return mpscPop(c)
}

//go:noinline
func mpscPop(consumer *MPSCConsumer) error {
	if hostcpu.CurrentThreadIdentity() != consumer.identity {
		return ErrMisuse
	}
	return consumer.core.Pop()
}

// Close releases consumer ownership without advancing a live span.
func (c *MPSCConsumer) Close() error {
	if c == nil {
		return nil
	}
	if !c.valid() || c.core == nil || hostcpu.CurrentThreadIdentity() != c.identity {
		return ErrMisuse
	}
	if c.closed.Load() {
		return nil
	}
	if !c.busy.CompareAndSwap(0, 1) {
		return ErrMisuse
	}
	defer c.busy.Store(0)
	q := c.core.q
	q.life.Lock()
	defer q.life.Unlock()
	if err := c.core.Close(); err != nil {
		return err
	}
	c.closed.Store(true)
	delete(q.handles, c)
	q.active.Add(-1)
	return nil
}

func (c *MPSCConsumer) reapDead() bool {
	if !c.valid() || c.core == nil || c.closed.Load() || !threadIdentityDead(c.identity) {
		return false
	}
	compareAndSwap32(c.core.hot.readerTid, uint32(c.core.tid), 0)
	c.core.peekExtent = 0
	c.core.closed = true
	c.closed.Store(true)
	c.identity = hostcpu.ThreadIdentity{}
	c.self = nil
	return true
}

// SPSCProducer is the single OS-thread-bound SPSC producer handle.
// Its zero value is not usable.
type SPSCProducer struct {
	_        noCopy
	self     *SPSCProducer
	core     *spscWriter
	identity hostcpu.ThreadIdentity
	busy     atomic.Int32
	closed   atomic.Bool
	token    uint64
	owner    uint64
}

// AttachProducer attaches the single producer.
func (q *SPSC) AttachProducer() (*SPSCProducer, error) {
	if !q.valid() {
		return nil, ErrMisuse
	}
	q.life.Lock()
	defer q.life.Unlock()
	if q.closing {
		return nil, ErrClosed
	}
	core, err := q.newWriter()
	if err != nil {
		return nil, err
	}
	producer := &SPSCProducer{core: core, identity: core.identity, owner: newSpanOwner()}
	producer.self = producer
	q.handles[producer] = struct{}{}
	q.active.Add(1)
	return producer, nil
}

func (p *SPSCProducer) valid() bool { return p != nil && p.self == p }

// Reserve returns a writable span or a retryable admission error.
func (p *SPSCProducer) Reserve(n uint64) (WriteSpan, error) {
	if !p.valid() || p.core == nil || hostcpu.CurrentThreadIdentity() != p.identity || p.closed.Load() || p.core.pos != invalidPos {
		return WriteSpan{}, ErrMisuse
	}
	if n > uint64(math.MaxInt) {
		return WriteSpan{}, ErrTooLarge
	}
	span, status := p.core.Reserve(int(n))
	if status != statusOK {
		return WriteSpan{}, statusError(status)
	}
	p.token++
	if p.token == 0 {
		p.token++
	}
	return WriteSpan{bytes: span.bytes, pos: span.pos, need: span.need, token: p.token, owner: p.owner}, nil
}

// Commit publishes actual bytes and reclaims unused reservation slack.
func (p *SPSCProducer) Commit(span WriteSpan, actual uint64) error {
	if !p.valid() || p.core == nil || hostcpu.CurrentThreadIdentity() != p.identity || p.closed.Load() || actual > uint64(math.MaxInt) {
		return ErrMisuse
	}
	if span.token == 0 || span.token != p.token || span.owner != p.owner {
		return ErrMisuse
	}
	return p.core.Commit(reservation{bytes: span.bytes, pos: span.pos, need: span.need}, int(actual))
}

// Abort discards span without publication.
func (p *SPSCProducer) Abort(span WriteSpan) error {
	if !p.valid() || p.core == nil || hostcpu.CurrentThreadIdentity() != p.identity || p.closed.Load() {
		return ErrMisuse
	}
	if span.token == 0 || span.token != p.token || span.owner != p.owner {
		return ErrMisuse
	}
	return p.core.Abort(reservation{pos: span.pos, need: span.need})
}

// Write reserves, copies, and commits one record.
func (p *SPSCProducer) Write(src []byte) error {
	if p == nil || p.self != p || p.core == nil {
		return ErrMisuse
	}
	return spscWrite(p, src)
}

//go:noinline
func spscWrite(producer *SPSCProducer, src []byte) error {
	if hostcpu.CurrentThreadIdentity() != producer.identity {
		return ErrMisuse
	}
	status := producer.core.Write(src)
	if status == statusOK {
		return nil
	}
	return statusError(status)
}

// Close auto-aborts a live span and releases the producer.
func (p *SPSCProducer) Close() error {
	if p == nil {
		return nil
	}
	if !p.valid() || p.core == nil || hostcpu.CurrentThreadIdentity() != p.identity {
		return ErrMisuse
	}
	if p.closed.Load() {
		return nil
	}
	if !p.busy.CompareAndSwap(0, 1) {
		return ErrMisuse
	}
	defer p.busy.Store(0)
	q := p.core.q
	q.life.Lock()
	defer q.life.Unlock()
	if err := p.core.Close(); err != nil {
		return err
	}
	p.token++
	p.closed.Store(true)
	delete(q.handles, p)
	q.active.Add(-1)
	return nil
}

func (p *SPSCProducer) reapDead() bool {
	if !p.valid() || p.core == nil || p.closed.Load() || !threadIdentityDead(p.identity) {
		return false
	}
	if p.core.ownsWriter() {
		detaching := uint32(p.core.tid) | spscWriterDetaching
		if compareAndSwap32(p.core.hot.writerOwner, uint32(p.core.tid), detaching) {
			fetchAndRelease64(p.core.hot.writerBitmap, ^uint64(1))
			compareAndSwap32(p.core.hot.writerOwner, detaching, 0)
		}
	}
	p.core.pos = invalidPos
	p.core.closed = true
	p.token++
	p.closed.Store(true)
	p.identity = hostcpu.ThreadIdentity{}
	p.self = nil
	return true
}

// SPSCConsumer is the single OS-thread-bound SPSC consumer handle.
// Its zero value is not usable.
type SPSCConsumer struct {
	_        noCopy
	self     *SPSCConsumer
	core     *spscReader
	identity hostcpu.ThreadIdentity
	busy     atomic.Int32
	closed   atomic.Bool
}

// AttachConsumer attaches the single consumer.
func (q *SPSC) AttachConsumer() (*SPSCConsumer, error) {
	if !q.valid() {
		return nil, ErrMisuse
	}
	q.life.Lock()
	defer q.life.Unlock()
	if q.closing {
		return nil, ErrClosed
	}
	core, err := q.newReader()
	if err != nil {
		return nil, err
	}
	consumer := &SPSCConsumer{core: core, identity: core.identity}
	consumer.self = consumer
	q.handles[consumer] = struct{}{}
	q.active.Add(1)
	return consumer, nil
}

func (c *SPSCConsumer) valid() bool { return c != nil && c.self == c }

// Peek returns the next committed record without advancing the queue.
func (c *SPSCConsumer) Peek() (ReadSpan, bool, error) {
	return spscPeek(c)
}

//go:noinline
func spscPeek(consumer *SPSCConsumer) (ReadSpan, bool, error) {
	if !consumer.valid() || consumer.core == nil || hostcpu.CurrentThreadIdentity() != consumer.identity {
		return ReadSpan{}, false, ErrMisuse
	}
	span, ok, err := consumer.core.Peek()
	if err != nil || !ok {
		return ReadSpan{}, false, err
	}
	return ReadSpan{bytes: span.bytes}, true, nil
}

// Pop advances past the outstanding peek and invalidates its span.
func (c *SPSCConsumer) Pop() error {
	if c == nil || c.self != c || c.core == nil {
		return ErrMisuse
	}
	return spscPop(c)
}

//go:noinline
func spscPop(consumer *SPSCConsumer) error {
	if hostcpu.CurrentThreadIdentity() != consumer.identity {
		return ErrMisuse
	}
	return consumer.core.Pop()
}

// Close releases consumer ownership without advancing a live span.
func (c *SPSCConsumer) Close() error {
	if c == nil {
		return nil
	}
	if !c.valid() || c.core == nil || hostcpu.CurrentThreadIdentity() != c.identity {
		return ErrMisuse
	}
	if c.closed.Load() {
		return nil
	}
	if !c.busy.CompareAndSwap(0, 1) {
		return ErrMisuse
	}
	defer c.busy.Store(0)
	q := c.core.q
	q.life.Lock()
	defer q.life.Unlock()
	if err := c.core.Close(); err != nil {
		return err
	}
	c.closed.Store(true)
	delete(q.handles, c)
	q.active.Add(-1)
	return nil
}

func (c *SPSCConsumer) reapDead() bool {
	if !c.valid() || c.core == nil || c.closed.Load() || !threadIdentityDead(c.identity) {
		return false
	}
	compareAndSwap32(c.core.hot.readerTid, uint32(c.core.tid), 0)
	c.core.peekExtent = 0
	c.core.closed = true
	c.closed.Store(true)
	c.identity = hostcpu.ThreadIdentity{}
	c.self = nil
	return true
}
