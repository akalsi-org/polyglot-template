//go:build linux && (amd64 || arm64)

package journal

import (
	"math"
	"runtime"
	"sync/atomic"

	"github.com/akalsi-org/polyglot-template/go/lib/hostcpu"
	"github.com/akalsi-org/polyglot-template/go/lib/internal/orderedatomic"
)

var nextJournalSpanOwner atomic.Uint64

func newJournalSpanOwner() uint64 {
	owner := nextJournalSpanOwner.Add(1)
	if owner == 0 {
		owner = nextJournalSpanOwner.Add(1)
	}
	return owner
}

// WriteSpan is a writable zero-copy journal reservation.
// Commit, Abort, or Producer.Close invalidates the span.
type WriteSpan struct {
	bytes      []byte
	position   uint64
	extent     uint64
	maximum    uint64
	sequence   Sequence
	generation uint32
	token      uint64
	owner      uint64
}

// Bytes returns the writable reserved payload.
// The returned slice remains valid until Commit, Abort, or Producer.Close.
func (span WriteSpan) Bytes() []byte { return span.bytes }

// Producer is the single OS-thread-bound journal producer handle.
// Use NewProducer to create a Producer. Do not copy Producer values.
type Producer struct {
	_               noCopy
	self            *Producer
	journal         *Journal
	region          *ringRegion
	identity        hostcpu.ThreadIdentity
	tid             hostcpu.ThreadId
	generation      uint32
	sequence        Sequence
	position        uint64
	durable         ringCursor
	reserved        bool
	reservedAt      uint64
	reservedExtent  uint64
	reservedMaximum uint64
	token           uint64
	owner           uint64
	closed          atomic.Bool
}

// NewProducer acquires the producer role and locks the goroutine to its OS thread.
func (journal *Journal) NewProducer() (*Producer, error) {
	if !journal.valid() {
		return nil, ErrMisuse
	}
	runtime.LockOSThread()
	unlockThread := true
	defer func() {
		if unlockThread {
			runtime.UnlockOSThread()
		}
	}()
	identity := journalCurrentThreadIdentity()
	journal.life.Lock()
	defer journal.life.Unlock()
	journal.reapDeadProducerLocked()
	if journal.closing || journal.region == nil {
		return nil, ErrClosed
	}
	if journal.producer != nil {
		return nil, ErrBusy
	}
	if journal.needsRecoveryLocked() {
		return nil, ErrRecoveryRequired
	}
	if !journal.region.hasValidSizingContract() {
		return nil, ErrFormat
	}
	tid := identity.ThreadId()
	generation, previousOwner, err := acquireProducerRole(journal.region, tid)
	if err != nil {
		return nil, err
	}
	if uint32(previousOwner) != 0 {
		restoreProducerRole(journal.region, tid, generation, previousOwner)
		journal.region.setRecoveryRequired()
		return nil, ErrRecoveryRequired
	}
	if journal.region.recoveryRequired() {
		releaseProducerRole(journal.region, tid, generation)
		return nil, ErrRecoveryRequired
	}
	_, archiverOwner := journal.region.sharedOwners()
	archiverState := classifyOwnerWord(archiverOwner)
	if archiverState == ownerDead {
		releaseProducerRole(journal.region, tid, generation)
		journal.region.setRecoveryRequired()
		return nil, ErrRecoveryRequired
	}
	cursor, publishOK := journal.region.snapshotPublish()
	durable, durableOK := journal.region.snapshotDurable()
	if !publishOK || !durableOK {
		releaseProducerRole(journal.region, tid, generation)
		if archiverState == ownerLive {
			return nil, ErrBusy
		}
		journal.region.setRecoveryRequired()
		return nil, ErrRecoveryRequired
	}
	if !journal.region.validCursorPair(durable, cursor) {
		releaseProducerRole(journal.region, tid, generation)
		journal.region.setRecoveryRequired()
		return nil, ErrRecoveryRequired
	}
	producer := &Producer{
		journal:    journal,
		region:     journal.region,
		identity:   identity,
		tid:        tid,
		generation: generation,
		sequence:   cursor.sequence,
		position:   cursor.position,
		durable:    durable,
		owner:      newJournalSpanOwner(),
	}
	producer.self = producer
	journal.producer = producer
	unlockThread = false
	return producer, nil
}

// AttachProducer creates a Producer through NewProducer.
func (journal *Journal) AttachProducer() (*Producer, error) {
	return journal.NewProducer()
}

func (producer *Producer) valid() bool { return producer != nil && producer.self == producer }

func acquireSharedRole(
	region *ringRegion,
	offset uint32,
	tid hostcpu.ThreadId,
) (uint32, uint64, error) {
	owner := region.ptr64(offset)
	for attempts := 0; attempts < 64; attempts++ {
		current := orderedatomic.LoadAcquire64(owner)
		currentTID := hostcpu.ThreadId(uint32(current))
		generation := uint32(current >> 32)
		if currentTID != 0 && journalThreadAliveProbe(currentTID) {
			return 0, 0, ErrBusy
		}
		if generation == math.MaxUint32 {
			return 0, 0, ErrPositionExhausted
		}
		generation++
		if generation == 0 {
			generation = 1
		}
		next := uint64(generation)<<32 | uint64(uint32(tid))
		if orderedatomic.CompareAndSwap64(owner, current, next) {
			return generation, current, nil
		}
		orderedatomic.Relax()
	}
	return 0, 0, ErrBusy
}

func releaseSharedRole(
	region *ringRegion,
	offset uint32,
	tid hostcpu.ThreadId,
	generation uint32,
) bool {
	owner := region.ptr64(offset)
	current := uint64(generation)<<32 | uint64(uint32(tid))
	return orderedatomic.CompareAndSwap64(owner, current, uint64(generation)<<32)
}

func restoreSharedRole(
	region *ringRegion,
	offset uint32,
	tid hostcpu.ThreadId,
	generation uint32,
	previous uint64,
) bool {
	owner := region.ptr64(offset)
	current := uint64(generation)<<32 | uint64(uint32(tid))
	return orderedatomic.CompareAndSwap64(owner, current, previous)
}

func acquireProducerRole(region *ringRegion, tid hostcpu.ThreadId) (uint32, uint64, error) {
	return acquireSharedRole(region, JournalHeaderProducerTIDOffset, tid)
}

func releaseProducerRole(region *ringRegion, tid hostcpu.ThreadId, generation uint32) bool {
	return releaseSharedRole(region, JournalHeaderProducerTIDOffset, tid, generation)
}

func restoreProducerRole(region *ringRegion, tid hostcpu.ThreadId, generation uint32, previous uint64) bool {
	return restoreSharedRole(region, JournalHeaderProducerTIDOffset, tid, generation, previous)
}

func (producer *Producer) ownsRole() bool {
	if producer.region == nil {
		return false
	}
	want := uint64(producer.generation)<<32 | uint64(uint32(producer.tid))
	return orderedatomic.LoadAcquire64(producer.region.ptr64(JournalHeaderProducerTIDOffset)) == want
}

func (producer *Producer) checkOperation() error {
	if !producer.valid() {
		return ErrMisuse
	}
	if producer.closed.Load() || producer.region == nil {
		return ErrClosed
	}
	if journalCurrentThreadIdentity() != producer.identity {
		return ErrMisuse
	}
	if producer.region.recoveryRequired() {
		return ErrRecoveryRequired
	}
	if !producer.ownsRole() {
		return ErrRecovered
	}
	return nil
}

// Reserve returns a writable zero-copy span.
// Reserve reclaims capacity only from the durable cursor.
func (producer *Producer) Reserve(length uint64) (WriteSpan, error) {
	if err := producer.checkOperation(); err != nil {
		return WriteSpan{}, err
	}
	return producer.reserve(length)
}

func (producer *Producer) reserve(length uint64) (WriteSpan, error) {
	if producer.reserved {
		return WriteSpan{}, ErrMisuse
	}
	if length > producer.region.maxRecord || length > uint64(math.MaxInt) {
		return WriteSpan{}, ErrTooLarge
	}
	extent, ok := journalExtent(length)
	if !ok || extent > producer.region.capacity-JournalSentinelBytes {
		return WriteSpan{}, ErrTooLarge
	}
	if producer.sequence == 0 || producer.sequence > JournalMaxSequence {
		return WriteSpan{}, ErrPositionExhausted
	}
	if producer.position > math.MaxUint64-extent {
		return WriteSpan{}, ErrPositionExhausted
	}
	if err := producer.ensureCapacity(extent); err != nil {
		return WriteSpan{}, err
	}
	writingTag := WritingTag(producer.sequence)
	grains := extent / uint64(JournalGrain)
	producer.region.fillTagsRelaxed(producer.position, grains, writingTag)
	orderedatomic.StoreBarrier()
	producer.region.fillDescriptorsRelaxed(
		producer.position+uint64(JournalGrain),
		grains-1,
		0,
	)
	producer.token++
	if producer.token == 0 {
		producer.token++
	}
	producer.reserved = true
	producer.reservedAt = producer.position
	producer.reservedExtent = extent
	producer.reservedMaximum = length
	return WriteSpan{
		bytes:      producer.region.payload(producer.position, length),
		position:   producer.position,
		extent:     extent,
		maximum:    length,
		sequence:   producer.sequence,
		generation: producer.generation,
		token:      producer.token,
		owner:      producer.owner,
	}, nil
}

func (producer *Producer) ensureCapacity(extent uint64) error {
	available, err := producer.hasCapacity(producer.durable, extent)
	if err != nil {
		return err
	}
	if available {
		return nil
	}
	durable, ok := producer.region.snapshotDurable()
	if !ok {
		_, archiverOwner := producer.region.sharedOwners()
		if ownerWordIsLive(archiverOwner) {
			return ErrBusy
		}
		producer.region.setRecoveryRequired()
		return ErrRecoveryRequired
	}
	available, err = producer.hasCapacity(durable, extent)
	if err != nil {
		return err
	}
	producer.durable = durable
	if !available {
		return ErrFull
	}
	return nil
}

func (producer *Producer) hasCapacity(durable ringCursor, extent uint64) (bool, error) {
	publish := ringCursor{sequence: producer.sequence, position: producer.position}
	if !producer.region.validCursorPair(durable, publish) {
		producer.region.setRecoveryRequired()
		return false, ErrRecoveryRequired
	}
	used := producer.position - durable.position
	return extent+JournalSentinelBytes <= producer.region.capacity-used, nil
}

// Commit publishes actual bytes from span and returns its sequence.
func (producer *Producer) Commit(span WriteSpan, actual uint64) (Sequence, error) {
	if err := producer.checkOperation(); err != nil {
		return 0, err
	}
	return producer.commit(span, actual)
}

func (producer *Producer) commit(span WriteSpan, actual uint64) (Sequence, error) {
	if !producer.matches(span) || actual > span.maximum || actual > uint64(math.MaxUint32) {
		return 0, ErrMisuse
	}
	actualExtent, ok := journalExtent(actual)
	if !ok || actualExtent > span.extent {
		return 0, ErrFormat
	}
	descriptor, ok := packDescriptor(uint32(actual), uint32(actualExtent/uint64(JournalGrain)), 0)
	if !ok {
		return 0, ErrFormat
	}
	orderedatomic.StoreRelaxed64(producer.region.descriptor(span.position), descriptor)
	stableTag := StableTag(span.sequence)
	grains := actualExtent / uint64(JournalGrain)
	producer.region.fillTagsRelaxed(
		span.position+uint64(JournalGrain),
		grains-1,
		stableTag,
	)
	orderedatomic.StoreRelease64(producer.region.tag(span.position), stableTag)
	next := ringCursor{sequence: span.sequence + 1, position: span.position + actualExtent}
	publishCursor(producer.region, JournalHeaderPublishLockOffset, JournalHeaderPublishSequenceOffset, JournalHeaderPublishPositionOffset, next)
	producer.sequence = next.sequence
	producer.position = next.position
	producer.invalidateReservation()
	return span.sequence, nil
}

// Abort discards span without changing the publish cursor.
// The next Reserve reuses the same tentative sequence.
func (producer *Producer) Abort(span WriteSpan) error {
	if err := producer.checkOperation(); err != nil {
		return err
	}
	if !producer.matches(span) {
		return ErrMisuse
	}
	producer.invalidateReservation()
	return nil
}

func (producer *Producer) matches(span WriteSpan) bool {
	return producer.reserved &&
		span.owner == producer.owner && span.token != 0 && span.token == producer.token &&
		span.generation == producer.generation && span.sequence == producer.sequence &&
		span.position == producer.reservedAt && span.extent == producer.reservedExtent &&
		span.maximum == producer.reservedMaximum
}

func (producer *Producer) invalidateReservation() {
	producer.reserved = false
	producer.reservedAt = 0
	producer.reservedExtent = 0
	producer.reservedMaximum = 0
	producer.token++
	if producer.token == 0 {
		producer.token++
	}
}

// Write copies and publishes one record.
func (producer *Producer) Write(source []byte) (Sequence, error) {
	if err := producer.checkOperation(); err != nil {
		return 0, err
	}
	span, err := producer.reserve(uint64(len(source)))
	if err != nil {
		return 0, err
	}
	copy(span.bytes, source)
	return producer.commit(span, uint64(len(source)))
}

// Close auto-aborts a reservation and releases the producer role.
func (producer *Producer) Close() error {
	if producer == nil {
		return nil
	}
	if !producer.valid() {
		return ErrMisuse
	}
	if producer.closed.Load() {
		return nil
	}
	if journalCurrentThreadIdentity() != producer.identity {
		return ErrMisuse
	}
	journal := producer.journal
	journal.life.Lock()
	defer journal.life.Unlock()
	if producer.reserved {
		producer.invalidateReservation()
	}
	released := releaseProducerRole(producer.region, producer.tid, producer.generation)
	producer.closed.Store(true)
	if journal.producer == producer {
		journal.producer = nil
	}
	runtime.UnlockOSThread()
	if !released {
		return ErrRecovered
	}
	return nil
}

func (producer *Producer) reapDead() bool {
	if !producer.valid() || producer.closed.Load() || producer.identity == (hostcpu.ThreadIdentity{}) {
		return false
	}
	current := journalCurrentThreadIdentity()
	tidReused := journalThreadIdentityReused(producer.identity, current)
	if !tidReused && journalThreadAliveProbe(producer.identity.ThreadId()) {
		return false
	}
	producer.closed.Store(true)
	producer.invalidateReservation()
	producer.identity = hostcpu.ThreadIdentity{}
	producer.self = nil
	return true
}
