//go:build linux && (amd64 || arm64)

package journal

import (
	"errors"
	"runtime"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/akalsi-org/polyglot-template/go/lib/hostcpu"
	"github.com/akalsi-org/polyglot-template/go/lib/internal/orderedatomic"
)

func completeTestCursorRecovery(t *testing.T, journal *Journal, durable ringCursor) ringCursor {
	t.Helper()
	recovery, err := journal.beginCursorRecovery()
	if err != nil {
		t.Fatal(err)
	}
	defer recovery.abort()
	if err := recovery.repairDurable(durable); err != nil {
		t.Fatal(err)
	}
	publish, err := recovery.recoverPublish(durable)
	if err != nil {
		t.Fatal(err)
	}
	if err := recovery.complete(durable, publish); err != nil {
		t.Fatal(err)
	}
	return publish
}

func TestCursorSeqlockPublishesCompletePairs(t *testing.T) {
	journal := newTestJournal(t, 1<<16, 4096)
	const updates = 200000
	var done atomic.Bool
	failures := make(chan ringCursor, 1)
	go func() {
		for index := uint64(1); index <= updates; index++ {
			publishCursor(journal.region, JournalHeaderPublishLockOffset, JournalHeaderPublishSequenceOffset, JournalHeaderPublishPositionOffset, ringCursor{
				sequence: Sequence(index + 1),
				position: index * uint64(JournalGrain),
			})
		}
		done.Store(true)
	}()
	for !done.Load() {
		cursor, ok := journal.region.snapshotPublish()
		if !ok {
			runtime.Gosched()
			continue
		}
		if cursor.position != (uint64(cursor.sequence)-1)*uint64(JournalGrain) {
			failures <- cursor
			break
		}
	}
	for !done.Load() {
		runtime.Gosched()
	}
	select {
	case cursor := <-failures:
		t.Fatalf("torn cursor = %+v", cursor)
	default:
	}
}

func TestDurableSeqlockPublishesCompletePairs(t *testing.T) {
	journal := newTestJournal(t, 1<<16, 4096)
	const updates = 200000
	var done atomic.Bool
	failures := make(chan ringCursor, 1)
	go func() {
		from := ringCursor{sequence: 1}
		for index := uint64(1); index <= updates; index++ {
			to := ringCursor{sequence: Sequence(index + 1), position: index * uint64(JournalGrain)}
			proof := durableProof{region: journal.region, from: from, to: to, publish: to, verified: true}
			if err := journal.region.advanceDurable(proof); err != nil {
				failures <- ringCursor{}
				break
			}
			from = to
		}
		done.Store(true)
	}()
	for !done.Load() {
		cursor, ok := journal.region.snapshotDurable()
		if !ok {
			runtime.Gosched()
			continue
		}
		if cursor.position != (uint64(cursor.sequence)-1)*uint64(JournalGrain) {
			failures <- cursor
			break
		}
	}
	for !done.Load() {
		runtime.Gosched()
	}
	select {
	case cursor := <-failures:
		t.Fatalf("torn durable cursor = %+v", cursor)
	default:
	}
}

func TestRecoveryCursorRestorationPublishesCompletePairs(t *testing.T) {
	journal := newTestJournal(t, 1<<16, 4096)
	first := [6]uint64{
		0, 2, uint64(JournalGrain),
		0, 2, uint64(JournalGrain),
	}
	second := [6]uint64{
		0, 3, 2 * uint64(JournalGrain),
		0, 3, 2 * uint64(JournalGrain),
	}
	journal.region.restoreCursorWords(first)
	const restorations = 200000
	var done atomic.Bool
	failures := make(chan ringCursor, 1)
	go func() {
		for index := 0; index < restorations; index++ {
			words := first
			if index&1 != 0 {
				words = second
			}
			journal.region.restoreCursorWords(words)
		}
		done.Store(true)
	}()
	for !done.Load() {
		publish, publishOK := journal.region.snapshotPublish()
		if publishOK && publish.position !=
			(uint64(publish.sequence)-1)*uint64(JournalGrain) {
			failures <- publish
			break
		}
		durable, durableOK := journal.region.snapshotDurable()
		if durableOK && durable.position !=
			(uint64(durable.sequence)-1)*uint64(JournalGrain) {
			failures <- durable
			break
		}
	}
	for !done.Load() {
		runtime.Gosched()
	}
	select {
	case cursor := <-failures:
		t.Fatalf("torn restored cursor = %+v", cursor)
	default:
	}
}

func TestAttachMarksDeadSharedOwnersForRecovery(t *testing.T) {
	for _, test := range []struct {
		name   string
		offset uint32
	}{
		{name: "producer", offset: JournalHeaderProducerTIDOffset},
		{name: "archiver", offset: JournalHeaderArchiverTIDOffset},
	} {
		t.Run(test.name, func(t *testing.T) {
			journal := newTestJournal(t, 1<<16, 4096)
			const tid = hostcpu.ThreadId(424242)
			owner := uint64(7)<<32 | uint64(uint32(tid))
			orderedatomic.StoreRelease64(journal.region.ptr64(test.offset), owner)
			originalAlive := journalThreadAliveProbe
			journalThreadAliveProbe = func(candidate hostcpu.ThreadId) bool { return candidate != tid }
			t.Cleanup(func() { journalThreadAliveProbe = originalAlive })
			fd, err := journal.DupFD()
			if err != nil {
				t.Fatal(err)
			}
			defer syscall.Close(fd)
			attached, err := Attach(fd)
			if err != nil {
				t.Fatal(err)
			}
			defer attached.Close()
			if !attached.NeedsRecovery() {
				t.Fatal("dead shared owner did not require recovery")
			}
			if producer, err := attached.AttachProducer(); producer != nil || !errors.Is(err, ErrRecoveryRequired) {
				t.Fatalf("AttachProducer = (%v, %v), want nil ErrRecoveryRequired", producer, err)
			}
			if got := orderedatomic.LoadAcquire64(journal.region.ptr64(test.offset)); got != owner {
				t.Fatalf("dead owner = %#x, want preserved %#x", got, owner)
			}
		})
	}
}

func TestAttachRequiresRecoveryForDeadOwnerWithStableTailBeyondCursor(t *testing.T) {
	journal := newTestJournal(t, 1<<16, 4096)
	producer := newTestProducer(t, journal)
	if _, err := producer.Write([]byte("stable")); err != nil {
		t.Fatal(err)
	}
	publishCursor(journal.region, JournalHeaderPublishLockOffset, JournalHeaderPublishSequenceOffset, JournalHeaderPublishPositionOffset, ringCursor{sequence: 1})
	originalAlive := journalThreadAliveProbe
	journalThreadAliveProbe = func(candidate hostcpu.ThreadId) bool { return candidate != producer.tid }
	t.Cleanup(func() { journalThreadAliveProbe = originalAlive })
	fd, err := journal.DupFD()
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fd)
	attached, err := Attach(fd)
	if err != nil {
		t.Fatal(err)
	}
	defer attached.Close()
	if !attached.NeedsRecovery() {
		t.Fatal("even stale publish cursor did not require recovery")
	}
	if replacement, err := attached.AttachProducer(); replacement != nil || !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("AttachProducer = (%v, %v), want nil ErrRecoveryRequired", replacement, err)
	}
	publish, ok := journal.region.snapshotPublish()
	if !ok || publish != (ringCursor{sequence: 1}) {
		t.Fatalf("publish changed before task18 recovery = %+v, ok=%v", publish, ok)
	}
	if err := producer.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestAttachAllowsStableOwnerZeroRing(t *testing.T) {
	journal := newTestJournal(t, 1<<16, 4096)
	fd, err := journal.DupFD()
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fd)
	attached, err := Attach(fd)
	if err != nil {
		t.Fatal(err)
	}
	defer attached.Close()
	if attached.NeedsRecovery() {
		t.Fatal("stable owner-zero ring requires recovery")
	}
	producer, err := attached.AttachProducer()
	if err != nil {
		t.Fatal(err)
	}
	if err := producer.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestAttachMarksOddPublishCursorForRecovery(t *testing.T) {
	journal := newTestJournal(t, 1<<16, 4096)
	orderedatomic.StoreRelease64(journal.region.ptr64(JournalHeaderPublishLockOffset), 1)
	fd, err := journal.DupFD()
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fd)
	attached, err := Attach(fd)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer attached.Close()
	if !attached.NeedsRecovery() {
		t.Fatal("Attach did not mark an odd publish cursor")
	}
	if _, err := attached.AttachProducer(); !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("AttachProducer = %v, want ErrRecoveryRequired", err)
	}
	durable := ringCursor{sequence: 1}
	publish := completeTestCursorRecovery(t, attached, durable)
	if publish != durable {
		t.Fatalf("recovered publish = %+v, want %+v", publish, durable)
	}
	if attached.NeedsRecovery() {
		t.Fatal("completed recovery remained required")
	}
	producer, err := attached.AttachProducer()
	if err != nil {
		t.Fatalf("AttachProducer after recovery: %v", err)
	}
	if err := producer.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestTransientLiveOwnerCursorDoesNotSetSharedRecovery(t *testing.T) {
	journal := newTestJournal(t, 1<<16, 4096)
	producer := newTestProducer(t, journal)
	orderedatomic.StoreRelease64(journal.region.ptr64(JournalHeaderPublishLockOffset), 1)
	fd, err := journal.DupFD()
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fd)
	attached, err := Attach(fd)
	if err != nil {
		t.Fatal(err)
	}
	defer attached.Close()
	if attached.NeedsRecovery() {
		t.Fatal("live producer cursor update set shared recovery")
	}
	if attached.region.recoveryRequired() {
		t.Fatal("live producer cursor update latched the shared flag")
	}
	orderedatomic.StoreRelease64(journal.region.ptr64(JournalHeaderPublishLockOffset), 2)
	if err := producer.Close(); err != nil {
		t.Fatal(err)
	}
	if attached.NeedsRecovery() {
		t.Fatal("clean producer close left transient recovery state set")
	}
}

func TestFailedRecoveryCompletionPreservesDeadOwner(t *testing.T) {
	journal := newTestJournal(t, 1<<16, 4096)
	const tid = hostcpu.ThreadId(424242)
	owner := uint64(7)<<32 | uint64(uint32(tid))
	orderedatomic.StoreRelease64(journal.region.ptr64(JournalHeaderProducerTIDOffset), owner)
	originalAlive := journalThreadAliveProbe
	journalThreadAliveProbe = func(candidate hostcpu.ThreadId) bool { return candidate != tid }
	t.Cleanup(func() { journalThreadAliveProbe = originalAlive })
	fd, err := journal.DupFD()
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fd)
	attached, err := Attach(fd)
	if err != nil {
		t.Fatal(err)
	}
	defer attached.Close()
	durable := ringCursor{sequence: 1}
	invalidPublish := ringCursor{sequence: 2, position: uint64(JournalGrain)}
	if err := attached.completeCursorRecovery(durable, invalidPublish); !errors.Is(err, ErrFormat) {
		t.Fatalf("completeCursorRecovery = %v, want ErrFormat", err)
	}
	if got := orderedatomic.LoadAcquire64(journal.region.ptr64(JournalHeaderProducerTIDOffset)); got != owner {
		t.Fatalf("owner after failed completion = %#x, want %#x", got, owner)
	}
	if !attached.NeedsRecovery() {
		t.Fatal("failed completion cleared recovery state")
	}
}

func TestCompletedRecoveryClearsOtherAttachedHandle(t *testing.T) {
	journal := newTestJournal(t, 1<<16, 4096)
	orderedatomic.StoreRelease64(journal.region.ptr64(JournalHeaderPublishLockOffset), 1)
	fd, err := journal.DupFD()
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fd)
	first, err := Attach(fd)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := Attach(fd)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if !first.NeedsRecovery() || !second.NeedsRecovery() {
		t.Fatal("attached handles did not preserve the recovery state")
	}
	durable := ringCursor{sequence: 1}
	completeTestCursorRecovery(t, first, durable)
	if second.NeedsRecovery() {
		t.Fatal("completed shared recovery left another handle blocked")
	}
	producer, err := second.AttachProducer()
	if err != nil {
		t.Fatalf("AttachProducer after shared recovery: %v", err)
	}
	if err := producer.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestAttachMarksOddDurableCursorForRecovery(t *testing.T) {
	journal := newTestJournal(t, 1<<16, 4096)
	orderedatomic.StoreRelease64(journal.region.ptr64(JournalHeaderDurableLockOffset), 3)
	orderedatomic.StoreRelaxed64(journal.region.ptr64(JournalHeaderDurableSequenceOffset), 9)
	fd, err := journal.DupFD()
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fd)
	attached, err := Attach(fd)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer attached.Close()
	if !attached.NeedsRecovery() {
		t.Fatal("Attach did not mark an odd durable cursor")
	}
	completeTestCursorRecovery(t, attached, ringCursor{sequence: 1})
}

func TestAttachMarksInconsistentCursorPairForRecovery(t *testing.T) {
	journal := newTestJournal(t, 1<<16, 4096)
	publishCursor(journal.region, JournalHeaderDurableLockOffset, JournalHeaderDurableSequenceOffset, JournalHeaderDurablePositionOffset, ringCursor{sequence: 2, position: 64})
	fd, err := journal.DupFD()
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fd)
	attached, err := Attach(fd)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer attached.Close()
	if !attached.NeedsRecovery() {
		t.Fatal("Attach did not mark an inconsistent cursor pair")
	}
	durable := ringCursor{sequence: 1}
	completeTestCursorRecovery(t, attached, durable)
	if attached.NeedsRecovery() {
		t.Fatal("completed cursor repair remained latched")
	}
}

func TestCursorPairReservesSentinelCapacity(t *testing.T) {
	durable := ringCursor{sequence: 1}
	publish := ringCursor{sequence: 2, position: 1 << 16}
	maximum := uint64(1<<16) - JournalSentinelBytes
	if validCursorPair(durable, publish, 1<<16, maximum) {
		t.Fatal("cursor pair accepted a full-capacity distance")
	}
	publish.position -= JournalSentinelBytes
	if !validCursorPair(durable, publish, 1<<16, maximum) {
		t.Fatal("cursor pair rejected the maximum usable distance")
	}
}

func TestCursorPairValidatesRecordGeometry(t *testing.T) {
	durable := ringCursor{sequence: 1}
	if validCursorPair(
		durable,
		ringCursor{sequence: 100, position: uint64(JournalGrain)},
		1<<16,
		4096,
	) {
		t.Fatal("cursor pair accepted too many records for its position distance")
	}
	if validCursorPair(
		durable,
		ringCursor{sequence: 2, position: 8192},
		1<<16,
		4096,
	) {
		t.Fatal("cursor pair accepted a record above the maximum extent")
	}
}

func TestRecoveryRejectsUnalignedCursor(t *testing.T) {
	journal := newTestJournal(t, 1<<16, 4096)
	journal.region.setRecoveryRequired()
	recovery, err := journal.beginCursorRecovery()
	if err != nil {
		t.Fatal(err)
	}
	defer recovery.abort()
	if err := recovery.repairDurable(ringCursor{sequence: 2, position: 65}); !errors.Is(err, ErrFormat) {
		t.Fatalf("repairDurable = %v, want ErrFormat", err)
	}
}

func TestRecoveryExcludesSharedProducerOwner(t *testing.T) {
	journal := newTestJournal(t, 1<<16, 4096)
	fd, err := journal.DupFD()
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fd)
	attached, err := Attach(fd)
	if err != nil {
		t.Fatal(err)
	}
	defer attached.Close()
	producer := newTestProducer(t, journal)
	attached.region.setRecoveryRequired()
	archiverOwner := orderedatomic.LoadAcquire64(
		attached.region.ptr64(JournalHeaderArchiverTIDOffset),
	)
	if recovery, err := attached.beginCursorRecovery(); recovery != nil || !errors.Is(err, ErrBusy) {
		t.Fatalf("beginCursorRecovery = (%v, %v), want nil ErrBusy", recovery, err)
	}
	if got := orderedatomic.LoadAcquire64(
		attached.region.ptr64(JournalHeaderArchiverTIDOffset),
	); got != archiverOwner {
		t.Fatalf("archiver owner after producer contention = %#x, want %#x", got, archiverOwner)
	}
	if err := producer.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCursorRecoveryHoldsSharedExclusionAcrossPhases(t *testing.T) {
	journal := newTestJournal(t, 1<<16, 4096)
	publishCursor(
		journal.region,
		JournalHeaderDurableLockOffset,
		JournalHeaderDurableSequenceOffset,
		JournalHeaderDurablePositionOffset,
		ringCursor{sequence: 2, position: uint64(JournalGrain)},
	)
	fd, err := journal.DupFD()
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fd)
	first, err := Attach(fd)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	recovery, err := first.beginCursorRecovery()
	if err != nil {
		t.Fatal(err)
	}
	if err := recovery.repairDurable(ringCursor{sequence: 1}); err != nil {
		recovery.abort()
		t.Fatal(err)
	}
	second, err := Attach(fd)
	if err != nil {
		recovery.abort()
		t.Fatal(err)
	}
	defer second.Close()
	if producer, err := second.AttachProducer(); producer != nil || !errors.Is(err, ErrRecoveryRequired) {
		recovery.abort()
		t.Fatalf("AttachProducer during recovery = (%v, %v), want nil ErrRecoveryRequired", producer, err)
	}
	if err := recovery.abort(); err != nil {
		t.Fatal(err)
	}
	if !second.NeedsRecovery() {
		t.Fatal("aborted recovery did not restore shared recovery evidence")
	}
}

func TestRecoveryScanFindsStableCommittedTail(t *testing.T) {
	journal := newTestJournal(t, 1<<16, 4096)
	producer := newTestProducer(t, journal)
	if _, err := producer.Write([]byte("one")); err != nil {
		t.Fatal(err)
	}
	if _, err := producer.Write([]byte("two")); err != nil {
		t.Fatal(err)
	}
	if err := producer.Close(); err != nil {
		t.Fatal(err)
	}
	orderedatomic.StoreRelease64(journal.region.ptr64(JournalHeaderPublishLockOffset), 5)
	fd, err := journal.DupFD()
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fd)
	attached, err := Attach(fd)
	if err != nil {
		t.Fatal(err)
	}
	defer attached.Close()
	durable := ringCursor{sequence: 1}
	publish := completeTestCursorRecovery(t, attached, durable)
	if publish != (ringCursor{sequence: 3, position: 128}) {
		t.Fatalf("recovered publish = %+v", publish)
	}
}
