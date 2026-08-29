//go:build linux && (amd64 || arm64)

package journal

import (
	"errors"
	"testing"

	"github.com/akalsi-org/polyglot-template/go/lib/internal/orderedatomic"
)

func TestDescriptorHelpers(t *testing.T) {
	word, ok := packDescriptor(65, 2, 0)
	if !ok {
		t.Fatal("packDescriptor rejected a valid descriptor")
	}
	if word != 65|uint64(2)<<32 {
		t.Fatalf("descriptor = %#x, want %#x", word, 65|uint64(2)<<32)
	}
	length, grains, flags, ok := unpackDescriptor(word)
	if !ok || length != 65 || grains != 2 || flags != 0 {
		t.Fatalf("unpackDescriptor = (%d, %d, %d, %v)", length, grains, flags, ok)
	}
	if _, ok := packDescriptor(65, 1, 0); ok {
		t.Fatal("packDescriptor accepted length beyond extent")
	}
	if _, ok := packDescriptor(0, 0, 0); ok {
		t.Fatal("packDescriptor accepted zero extent")
	}
}

func TestZeroAndMaximumRecordSequences(t *testing.T) {
	const (
		capacity = uint64(1 << 16)
		maximum  = uint64(1<<14) - JournalSentinelBytes
	)
	journal := newTestJournal(t, capacity, maximum)
	producer := newTestProducer(t, journal)
	zero, err := producer.Reserve(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(zero.Bytes()) != 0 || cap(zero.Bytes()) != 0 || zero.extent != uint64(JournalGrain) {
		t.Fatalf("zero span = len %d cap %d extent %d", len(zero.Bytes()), cap(zero.Bytes()), zero.extent)
	}
	sequence, err := producer.Commit(zero, 0)
	if err != nil || sequence != 1 {
		t.Fatalf("zero Commit = (%d, %v), want (1, nil)", sequence, err)
	}
	firstEnd := ringCursor{sequence: 2, position: uint64(JournalGrain)}
	proof, err := journal.region.beginDurableProof(ringCursor{sequence: 1}, firstEnd)
	if err != nil {
		t.Fatal(err)
	}
	record, found, err := journal.region.recordAt(ringCursor{sequence: 1}, firstEnd)
	if err != nil || !found {
		t.Fatalf("zero record = (%+v, %v, %v)", record, found, err)
	}
	if err := proof.include(record); err != nil {
		t.Fatal(err)
	}
	if err := journal.region.advanceDurable(proof); err != nil {
		t.Fatalf("advance zero record: %v", err)
	}
	maximumSpan, err := producer.Reserve(maximum)
	if err != nil {
		t.Fatal(err)
	}
	if uint64(len(maximumSpan.Bytes())) != maximum || len(maximumSpan.Bytes()) != cap(maximumSpan.Bytes()) {
		t.Fatalf("maximum span = len %d cap %d", len(maximumSpan.Bytes()), cap(maximumSpan.Bytes()))
	}
	sequence, err = producer.Commit(maximumSpan, maximum)
	if err != nil || sequence != 2 {
		t.Fatalf("maximum Commit = (%d, %v), want (2, nil)", sequence, err)
	}
}

func TestRecordAtAndDurableAdvancement(t *testing.T) {
	journal := newTestJournal(t, 1<<16, 4096)
	producer := newTestProducer(t, journal)
	firstSequence, err := producer.Write([]byte("first"))
	if err != nil || firstSequence != 1 {
		t.Fatalf("first Write = (%d, %v)", firstSequence, err)
	}
	secondSpan, err := producer.Reserve(65)
	if err != nil {
		t.Fatal(err)
	}
	copy(secondSpan.Bytes(), "second")
	secondSequence, err := producer.Commit(secondSpan, 6)
	if err != nil || secondSequence != 2 {
		t.Fatalf("second Commit = (%d, %v)", secondSequence, err)
	}
	publish, ok := journal.region.snapshotPublish()
	if !ok || publish != (ringCursor{sequence: 3, position: 128}) {
		t.Fatalf("publish = %+v, ok=%v", publish, ok)
	}
	first, found, err := journal.region.recordAt(ringCursor{sequence: 1}, publish)
	if err != nil || !found || string(first.bytes) != "first" || first.extent != 64 {
		t.Fatalf("first record = %+v, found=%v, err=%v", first, found, err)
	}
	second, found, err := journal.region.recordAt(first.next, publish)
	if err != nil || !found || string(second.bytes) != "second" || second.extent != 64 {
		t.Fatalf("second record = %+v, found=%v, err=%v", second, found, err)
	}
	if _, found, err := journal.region.recordAt(publish, publish); err != nil || found {
		t.Fatalf("record at frontier = found %v, err %v", found, err)
	}
	from := ringCursor{sequence: 1}
	firstProof, err := journal.region.beginDurableProof(from, publish)
	if err != nil {
		t.Fatal(err)
	}
	if err := firstProof.include(first); err != nil {
		t.Fatal(err)
	}
	staleProof := firstProof
	if err := journal.region.advanceDurable(firstProof); err != nil {
		t.Fatalf("advanceDurable: %v", err)
	}
	if durable, ok := journal.region.snapshotDurable(); !ok || durable != first.next {
		t.Fatalf("durable = %+v, ok=%v", durable, ok)
	}
	if err := journal.region.advanceDurable(staleProof); !errors.Is(err, ErrRecovered) {
		t.Fatalf("stale advanceDurable = %v, want ErrRecovered", err)
	}
	forgedProof := durableProof{region: journal.region, from: first.next, to: ringCursor{sequence: 3, position: 128}, publish: publish}
	if err := journal.region.advanceDurable(forgedProof); !errors.Is(err, ErrFormat) {
		t.Fatalf("forged advanceDurable = %v, want ErrFormat", err)
	}
	secondProof, err := journal.region.beginDurableProof(first.next, publish)
	if err != nil {
		t.Fatal(err)
	}
	if err := secondProof.include(second); err != nil {
		t.Fatal(err)
	}
	if err := journal.region.advanceDurable(secondProof); err != nil {
		t.Fatalf("second advanceDurable: %v", err)
	}
}

func TestRecordValidationRejectsLengthAboveConfiguredMaximum(t *testing.T) {
	journal := newTestJournal(t, 1<<16, 64)
	word, ok := packDescriptor(65, 2, 0)
	if !ok {
		t.Fatal("packDescriptor rejected forged test descriptor")
	}
	orderedatomic.StoreRelaxed64(journal.region.descriptor(0), word)
	orderedatomic.StoreRelease64(journal.region.tag(64), StableTag(1))
	orderedatomic.StoreRelease64(journal.region.tag(0), StableTag(1))
	publish := ringCursor{sequence: 2, position: 128}
	if _, _, err := journal.region.recordAt(ringCursor{sequence: 1}, publish); !errors.Is(err, ErrFormat) {
		t.Fatalf("recordAt oversized configured record = %v, want ErrFormat", err)
	}
	if _, err := journal.region.scanStableFrom(ringCursor{sequence: 1}); !errors.Is(err, ErrFormat) {
		t.Fatalf("scanStableFrom oversized configured record = %v, want ErrFormat", err)
	}
}

func TestRecordValidationRejectsExtentAboveConfiguredMaximum(t *testing.T) {
	journal := newTestJournal(t, 1<<16, 65)
	word, ok := packDescriptor(1, 3, 0)
	if !ok {
		t.Fatal("packDescriptor rejected forged extent")
	}
	orderedatomic.StoreRelaxed64(journal.region.descriptor(0), word)
	for grain := uint64(0); grain < 3; grain++ {
		orderedatomic.StoreRelease64(journal.region.tag(grain*uint64(JournalGrain)), StableTag(1))
	}
	publish := ringCursor{sequence: 2, position: 192}
	if _, _, err := journal.region.recordAt(ringCursor{sequence: 1}, publish); !errors.Is(err, ErrFormat) {
		t.Fatalf("recordAt oversized configured extent = %v, want ErrFormat", err)
	}
	if _, err := journal.region.scanStableFrom(ringCursor{sequence: 1}); !errors.Is(err, ErrFormat) {
		t.Fatalf("scanStableFrom oversized configured extent = %v, want ErrFormat", err)
	}
}

func TestRecordAtRejectsContinuationDescriptor(t *testing.T) {
	journal := newTestJournal(t, 1<<16, 4096)
	producer := newTestProducer(t, journal)
	span, err := producer.Reserve(65)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := producer.Commit(span, 65); err != nil {
		t.Fatal(err)
	}
	publish, _ := journal.region.snapshotPublish()
	orderedatomic.StoreRelaxed64(journal.region.descriptor(64), 1)
	if _, _, err := journal.region.recordAt(ringCursor{sequence: 1}, publish); !errors.Is(err, ErrFormat) {
		t.Fatalf("recordAt continuation descriptor = %v, want ErrFormat", err)
	}
}

func TestRecordAtDetectsContinuationOverwrite(t *testing.T) {
	journal := newTestJournal(t, 1<<16, 4096)
	producer := newTestProducer(t, journal)
	span, err := producer.Reserve(65)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := producer.Commit(span, 65); err != nil {
		t.Fatal(err)
	}
	publish, _ := journal.region.snapshotPublish()
	orderedatomic.StoreRelease64(journal.region.tag(64), WritingTag(1))
	if _, _, err := journal.region.recordAt(ringCursor{sequence: 1}, publish); !errors.Is(err, ErrOverwritten) {
		t.Fatalf("recordAt overwritten continuation = %v, want ErrOverwritten", err)
	}
	if _, err := journal.region.scanStableFrom(ringCursor{sequence: 1}); !errors.Is(err, ErrFormat) {
		t.Fatalf("scanStableFrom overwritten continuation = %v, want ErrFormat", err)
	}
}

func TestRingEpochCopiesHeaderBytes(t *testing.T) {
	journal := newTestJournal(t, 1<<16, 4096)
	epoch := journal.region.ringEpoch()
	if allZero(epoch[:]) {
		t.Fatal("ring epoch is zero")
	}
	epoch[0] ^= 0xff
	if epoch == journal.region.ringEpoch() {
		t.Fatal("ringEpoch returned aliased storage")
	}
}
