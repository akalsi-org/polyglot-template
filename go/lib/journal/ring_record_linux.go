//go:build linux && (amd64 || arm64)

package journal

import (
	"math"

	"github.com/akalsi-org/polyglot-template/go/lib/internal/orderedatomic"
)

type ringRecord struct {
	region *ringRegion
	cursor ringCursor
	next   ringCursor
	length uint32
	extent uint64
	flags  uint8
	bytes  []byte
}

type durableProof struct {
	region   *ringRegion
	from     ringCursor
	to       ringCursor
	publish  ringCursor
	verified bool
}

func (region *ringRegion) ringEpoch() RingEpoch {
	var epoch RingEpoch
	copy(epoch[:], region.control[JournalHeaderRingEpochOffset:JournalHeaderRingEpochOffset+16])
	return epoch
}

func (region *ringRegion) recordAt(cursor ringCursor, publish ringCursor) (ringRecord, bool, error) {
	if cursor == publish {
		return ringRecord{}, false, nil
	}
	if cursor.sequence == 0 || cursor.sequence > JournalMaxSequence ||
		cursor.sequence >= publish.sequence || cursor.position >= publish.position {
		return ringRecord{}, false, ErrFormat
	}
	record, stable, err := region.readStableRecord(cursor)
	if err != nil {
		return ringRecord{}, false, err
	}
	if !stable {
		return ringRecord{}, false, ErrOverwritten
	}
	if record.next.sequence > publish.sequence || record.next.position > publish.position ||
		(record.next.sequence == publish.sequence) != (record.next.position == publish.position) {
		return ringRecord{}, false, ErrFormat
	}
	return record, true, nil
}

func (region *ringRegion) readStableRecord(cursor ringCursor) (ringRecord, bool, error) {
	if cursor.sequence == 0 || cursor.sequence > JournalMaxSequence ||
		cursor.position%uint64(JournalGrain) != 0 {
		return ringRecord{}, false, ErrFormat
	}
	expectedTag := StableTag(cursor.sequence)
	if orderedatomic.LoadAcquire64(region.tag(cursor.position)) != expectedTag {
		return ringRecord{}, false, nil
	}
	word := orderedatomic.LoadRelaxed64(region.descriptor(cursor.position))
	length, extentGrains, flags, ok := unpackDescriptor(word)
	if !ok || flags != 0 || uint64(length) > region.maxRecord {
		return ringRecord{}, false, ErrFormat
	}
	extent := uint64(extentGrains) * uint64(JournalGrain)
	if extent > region.maxRecordExtent ||
		extent > region.capacity-JournalSentinelBytes ||
		cursor.position > math.MaxUint64-extent {
		return ringRecord{}, false, ErrFormat
	}
	for grain := uint64(1); grain < uint64(extentGrains); grain++ {
		position := cursor.position + grain*uint64(JournalGrain)
		if orderedatomic.LoadAcquire64(region.tag(position)) != expectedTag {
			return ringRecord{}, false, ErrOverwritten
		}
		if orderedatomic.LoadRelaxed64(region.descriptor(position)) != 0 {
			return ringRecord{}, false, ErrFormat
		}
	}
	orderedatomic.LoadBarrier()
	if orderedatomic.LoadAcquire64(region.tag(cursor.position)) != expectedTag {
		return ringRecord{}, false, ErrOverwritten
	}
	next := ringCursor{sequence: cursor.sequence + 1, position: cursor.position + extent}
	return ringRecord{
		region: region,
		cursor: cursor,
		next:   next,
		length: length,
		extent: extent,
		flags:  flags,
		bytes:  region.payload(cursor.position, uint64(length)),
	}, true, nil
}

func (region *ringRegion) beginDurableProof(from, publish ringCursor) (durableProof, error) {
	if !region.validCursorPair(from, publish) {
		return durableProof{}, ErrFormat
	}
	return durableProof{region: region, from: from, to: from, publish: publish}, nil
}

func (proof *durableProof) include(record ringRecord) error {
	if proof == nil || proof.region == nil || record.region != proof.region || record.cursor != proof.to ||
		record.next.sequence > proof.publish.sequence || record.next.position > proof.publish.position ||
		(record.next.sequence == proof.publish.sequence) != (record.next.position == proof.publish.position) {
		return ErrFormat
	}
	proof.to = record.next
	proof.verified = true
	return nil
}

func (region *ringRegion) advanceDurable(proof durableProof) error {
	if proof.region != region || (proof.from != proof.to && !proof.verified) || !region.validCursorPair(proof.from, proof.to) ||
		proof.to.sequence > proof.publish.sequence || proof.to.position > proof.publish.position ||
		(proof.to.sequence == proof.publish.sequence) != (proof.to.position == proof.publish.position) {
		return ErrFormat
	}
	current, ok := region.snapshotDurable()
	if !ok || current != proof.from {
		return ErrRecovered
	}
	if proof.from == proof.to {
		return nil
	}
	lock := region.ptr64(JournalHeaderDurableLockOffset)
	version := orderedatomic.LoadAcquire64(lock)
	if version > math.MaxUint64-2 {
		return ErrFormat
	}
	if version&1 != 0 ||
		Sequence(orderedatomic.LoadRelaxed64(region.ptr64(JournalHeaderDurableSequenceOffset))) != proof.from.sequence ||
		orderedatomic.LoadRelaxed64(region.ptr64(JournalHeaderDurablePositionOffset)) != proof.from.position ||
		!orderedatomic.CompareAndSwap64(lock, version, version+1) {
		return ErrRecovered
	}
	orderedatomic.StoreBarrier()
	orderedatomic.StoreRelaxed64(region.ptr64(JournalHeaderDurableSequenceOffset), uint64(proof.to.sequence))
	orderedatomic.StoreRelaxed64(region.ptr64(JournalHeaderDurablePositionOffset), proof.to.position)
	orderedatomic.StoreRelease64(lock, version+2)
	return nil
}
