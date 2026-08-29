//go:build linux && (amd64 || arm64)

package journal

import (
	"errors"
	"syscall"
	"testing"

	"github.com/akalsi-org/polyglot-template/go/lib/internal/orderedatomic"
)

func TestAttachRejectsUnknownJournalFlag(t *testing.T) {
	journal := newTestJournal(t, 1<<16, 4096)
	put32(
		journal.region.control,
		JournalHeaderFlagsOffset,
		JournalKnownFlags<<1,
	)
	fd, err := journal.DupFD()
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fd)
	attached, err := Attach(fd)
	if attached != nil || !errors.Is(err, ErrFormat) {
		t.Fatalf("Attach = (%v, %v), want nil ErrFormat", attached, err)
	}
}

func TestAttachAllowsRecoveryFlag(t *testing.T) {
	journal := newTestJournal(t, 1<<16, 4096)
	journal.region.setRecoveryRequired()
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
		t.Fatal("attached handle did not preserve shared recovery")
	}
}

func TestSharedRecoveryFlagIsVisibleAcrossHandles(t *testing.T) {
	journal := newTestJournal(t, 1<<16, 4096)
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

	journal.region.setRecoveryRequired()
	if !first.NeedsRecovery() || !second.NeedsRecovery() {
		t.Fatal("an attached handle did not observe shared recovery")
	}
	recovery, err := first.beginCursorRecovery()
	if err != nil {
		t.Fatal(err)
	}
	producerOwner, archiverOwner := first.region.sharedOwners()
	if uint32(producerOwner) == 0 || uint32(archiverOwner) == 0 {
		recovery.abort()
		t.Fatalf("recovery owners = (%#x, %#x), want both roles", producerOwner, archiverOwner)
	}
	if !second.NeedsRecovery() {
		recovery.abort()
		t.Fatal("temporary recovery ownership cleared shared recovery")
	}
	if err := recovery.abort(); err != nil {
		t.Fatal(err)
	}
	if !second.NeedsRecovery() {
		t.Fatal("aborted recovery cleared shared recovery")
	}

	durable := ringCursor{sequence: 1}
	if err := first.completeCursorRecovery(durable, durable); err != nil {
		t.Fatal(err)
	}
	producerOwner, archiverOwner = first.region.sharedOwners()
	if uint32(producerOwner) != 0 || uint32(archiverOwner) != 0 {
		t.Fatalf("completed recovery owners = (%#x, %#x), want owner-free roles", producerOwner, archiverOwner)
	}
	if first.region.recoveryRequired() || second.NeedsRecovery() {
		t.Fatal("completed recovery did not clear shared recovery")
	}
}

func TestAttachProducerInvalidCursorSetsSharedRecovery(t *testing.T) {
	journal := newTestJournal(t, 1<<16, 4096)
	publishCursor(
		journal.region,
		JournalHeaderPublishLockOffset,
		JournalHeaderPublishSequenceOffset,
		JournalHeaderPublishPositionOffset,
		ringCursor{sequence: 2, position: 8192},
	)
	producer, err := journal.AttachProducer()
	if producer != nil || !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("AttachProducer = (%v, %v), want nil ErrRecoveryRequired", producer, err)
	}
	if !journal.region.recoveryRequired() {
		t.Fatal("invalid cursor pair did not set shared recovery")
	}
	recovery, err := journal.beginCursorRecovery()
	if err != nil {
		t.Fatalf("beginCursorRecovery: %v", err)
	}
	if err := recovery.abort(); err != nil {
		t.Fatal(err)
	}
}

func TestLiveArchiverBlocksRecoveryWithoutOwnerMutation(t *testing.T) {
	journal := newTestJournal(t, 1<<16, 4096)
	identity := journalCurrentThreadIdentity()
	owner := uint64(7)<<32 | uint64(uint32(identity.ThreadId()))
	orderedatomic.StoreRelease64(
		journal.region.ptr64(JournalHeaderArchiverTIDOffset),
		owner,
	)
	journal.region.setRecoveryRequired()
	recovery, err := journal.beginCursorRecovery()
	if recovery != nil || !errors.Is(err, ErrBusy) {
		t.Fatalf("beginCursorRecovery = (%v, %v), want nil ErrBusy", recovery, err)
	}
	got := orderedatomic.LoadAcquire64(
		journal.region.ptr64(JournalHeaderArchiverTIDOffset),
	)
	if got != owner {
		t.Fatalf("archiver owner = %#x, want %#x", got, owner)
	}
	orderedatomic.StoreRelease64(
		journal.region.ptr64(JournalHeaderArchiverTIDOffset),
		uint64(7)<<32,
	)
}
