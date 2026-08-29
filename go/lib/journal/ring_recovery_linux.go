//go:build linux && (amd64 || arm64)

package journal

import (
	"runtime"

	"github.com/akalsi-org/polyglot-template/go/lib/hostcpu"
	"github.com/akalsi-org/polyglot-template/go/lib/internal/orderedatomic"
)

type cursorRecovery struct {
	_                     noCopy
	self                  *cursorRecovery
	journal               *Journal
	identity              hostcpu.ThreadIdentity
	tid                   hostcpu.ThreadId
	archiverGeneration    uint32
	producerGeneration    uint32
	previousArchiverOwner uint64
	previousProducerOwner uint64
	originalCursorWord    [6]uint64
	mutated               bool
	active                bool
}

func (journal *Journal) beginCursorRecovery() (*cursorRecovery, error) {
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
	unlockJournal := true
	defer func() {
		if unlockJournal {
			journal.life.Unlock()
		}
	}()
	if journal.closing || journal.region == nil {
		return nil, ErrClosed
	}
	if !journal.region.recoveryRequired() {
		return nil, ErrMisuse
	}
	journal.region.setRecoveryRequired()
	if journal.producer != nil {
		return nil, ErrBusy
	}
	tid := identity.ThreadId()
	archiverGeneration, previousArchiverOwner, err := acquireSharedRole(
		journal.region,
		JournalHeaderArchiverTIDOffset,
		tid,
	)
	if err != nil {
		return nil, err
	}
	producerGeneration, previousProducerOwner, err := acquireSharedRole(
		journal.region,
		JournalHeaderProducerTIDOffset,
		tid,
	)
	if err != nil {
		restoreSharedRole(
			journal.region,
			JournalHeaderArchiverTIDOffset,
			tid,
			archiverGeneration,
			previousArchiverOwner,
		)
		return nil, err
	}
	if !journal.region.recoveryRequired() {
		restoreSharedRole(
			journal.region,
			JournalHeaderProducerTIDOffset,
			tid,
			producerGeneration,
			previousProducerOwner,
		)
		restoreSharedRole(
			journal.region,
			JournalHeaderArchiverTIDOffset,
			tid,
			archiverGeneration,
			previousArchiverOwner,
		)
		return nil, ErrRecovered
	}
	recovery := &cursorRecovery{
		journal:               journal,
		identity:              identity,
		tid:                   tid,
		archiverGeneration:    archiverGeneration,
		producerGeneration:    producerGeneration,
		previousArchiverOwner: previousArchiverOwner,
		previousProducerOwner: previousProducerOwner,
		active:                true,
	}
	recovery.self = recovery
	recovery.originalCursorWord = journal.region.cursorWords()
	unlockThread = false
	unlockJournal = false
	return recovery, nil
}

func (recovery *cursorRecovery) valid() bool {
	return recovery != nil && recovery.self == recovery && recovery.active &&
		recovery.journal != nil &&
		journalCurrentThreadIdentity() == recovery.identity
}

func (region *ringRegion) cursorWords() [6]uint64 {
	return [6]uint64{
		orderedatomic.LoadAcquire64(region.ptr64(JournalHeaderPublishLockOffset)),
		orderedatomic.LoadRelaxed64(region.ptr64(JournalHeaderPublishSequenceOffset)),
		orderedatomic.LoadRelaxed64(region.ptr64(JournalHeaderPublishPositionOffset)),
		orderedatomic.LoadAcquire64(region.ptr64(JournalHeaderDurableLockOffset)),
		orderedatomic.LoadRelaxed64(region.ptr64(JournalHeaderDurableSequenceOffset)),
		orderedatomic.LoadRelaxed64(region.ptr64(JournalHeaderDurablePositionOffset)),
	}
}

func (region *ringRegion) restoreCursorWords(words [6]uint64) {
	publishCursor(
		region,
		JournalHeaderPublishLockOffset,
		JournalHeaderPublishSequenceOffset,
		JournalHeaderPublishPositionOffset,
		ringCursor{sequence: Sequence(words[1]), position: words[2]},
	)
	publishCursor(
		region,
		JournalHeaderDurableLockOffset,
		JournalHeaderDurableSequenceOffset,
		JournalHeaderDurablePositionOffset,
		ringCursor{sequence: Sequence(words[4]), position: words[5]},
	)
}

func (recovery *cursorRecovery) repairDurable(cursor ringCursor) error {
	if !recovery.valid() {
		return ErrMisuse
	}
	if !validCursorShape(cursor) {
		return ErrFormat
	}
	recovery.mutated = true
	publishCursor(
		recovery.journal.region,
		JournalHeaderDurableLockOffset,
		JournalHeaderDurableSequenceOffset,
		JournalHeaderDurablePositionOffset,
		cursor,
	)
	return nil
}

func (recovery *cursorRecovery) recoverPublish(durable ringCursor) (ringCursor, error) {
	if !recovery.valid() {
		return ringCursor{}, ErrMisuse
	}
	current, ok := recovery.journal.region.snapshotDurable()
	if !ok || current != durable {
		return ringCursor{}, ErrRecovered
	}
	return recovery.journal.region.scanStableFrom(durable)
}

func (recovery *cursorRecovery) complete(durable, publish ringCursor) error {
	if !recovery.valid() {
		return ErrMisuse
	}
	journal := recovery.journal
	current, ok := journal.region.snapshotDurable()
	if !ok || current != durable {
		return ErrRecovered
	}
	derived, err := journal.region.scanStableFrom(durable)
	if err != nil {
		return err
	}
	if derived != publish || !journal.region.validCursorPair(durable, publish) {
		return ErrFormat
	}
	recovery.mutated = true
	publishCursor(
		journal.region,
		JournalHeaderPublishLockOffset,
		JournalHeaderPublishSequenceOffset,
		JournalHeaderPublishPositionOffset,
		publish,
	)
	if journal.region.needsCursorRecovery() {
		return ErrFormat
	}
	if !recovery.finish(true) {
		return ErrRecovered
	}
	return nil
}

func (recovery *cursorRecovery) abort() error {
	if recovery == nil || !recovery.active {
		return nil
	}
	if !recovery.valid() {
		return ErrMisuse
	}
	if !recovery.finish(false) {
		return ErrRecovered
	}
	return nil
}

func (recovery *cursorRecovery) finish(completed bool) bool {
	journal := recovery.journal
	rolesClean := true
	if completed {
		producerReleased := releaseSharedRole(
			journal.region,
			JournalHeaderProducerTIDOffset,
			recovery.tid,
			recovery.producerGeneration,
		)
		archiverReleased := releaseSharedRole(
			journal.region,
			JournalHeaderArchiverTIDOffset,
			recovery.tid,
			recovery.archiverGeneration,
		)
		rolesClean = producerReleased && archiverReleased
		if rolesClean {
			journal.region.clearRecoveryRequired()
		}
	} else {
		if recovery.mutated {
			journal.region.restoreCursorWords(recovery.originalCursorWord)
		}
		producerRestored := restoreSharedRole(
			journal.region,
			JournalHeaderProducerTIDOffset,
			recovery.tid,
			recovery.producerGeneration,
			recovery.previousProducerOwner,
		)
		archiverRestored := restoreSharedRole(
			journal.region,
			JournalHeaderArchiverTIDOffset,
			recovery.tid,
			recovery.archiverGeneration,
			recovery.previousArchiverOwner,
		)
		rolesClean = producerRestored && archiverRestored
	}
	recovery.active = false
	journal.life.Unlock()
	runtime.UnlockOSThread()
	return rolesClean
}

func (journal *Journal) recoverPublishCursor(durable ringCursor) (ringCursor, error) {
	recovery, err := journal.beginCursorRecovery()
	if err != nil {
		return ringCursor{}, err
	}
	defer recovery.abort()
	return recovery.recoverPublish(durable)
}

func (journal *Journal) completeCursorRecovery(durable, publish ringCursor) error {
	recovery, err := journal.beginCursorRecovery()
	if err != nil {
		return err
	}
	defer recovery.abort()
	return recovery.complete(durable, publish)
}

func (region *ringRegion) scanStableFrom(cursor ringCursor) (ringCursor, error) {
	if !validCursorShape(cursor) {
		return ringCursor{}, ErrFormat
	}
	maximumRecords := region.capacity / uint64(JournalGrain)
	for records := uint64(0); records < maximumRecords; records++ {
		if cursor.sequence > JournalMaxSequence {
			return cursor, nil
		}
		record, stable, err := region.readStableRecord(cursor)
		if err != nil {
			return ringCursor{}, ErrFormat
		}
		if !stable {
			return cursor, nil
		}
		cursor = record.next
	}
	return ringCursor{}, ErrFormat
}
