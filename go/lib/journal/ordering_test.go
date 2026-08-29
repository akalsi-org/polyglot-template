//go:build linux && (amd64 || arm64)

package journal

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"github.com/akalsi-org/polyglot-template/go/lib/internal/orderedatomic"
)

const journalOrderingRecordBytes = 128

func journalOrderingCount(t *testing.T) int {
	t.Helper()
	text := os.Getenv("PGT_JOURNAL_ORDERING_RECORDS")
	if text == "" {
		return 5000
	}
	count, err := strconv.Atoi(text)
	if err != nil || count <= 0 {
		t.Fatalf("invalid PGT_JOURNAL_ORDERING_RECORDS: %q", text)
	}
	return count
}

func fillJournalOrderingRecord(bytes []byte, sequence Sequence) {
	binary.LittleEndian.PutUint64(bytes, uint64(sequence))
	for offset := 8; offset < len(bytes); offset++ {
		bytes[offset] = byte(uint64(sequence)*131 + uint64(offset)*29)
	}
}

func validateJournalOrderingRecord(bytes []byte, sequence Sequence) error {
	if len(bytes) != journalOrderingRecordBytes {
		return fmt.Errorf("sequence %d length %d", sequence, len(bytes))
	}
	if got := Sequence(binary.LittleEndian.Uint64(bytes)); got != sequence {
		return fmt.Errorf("sequence field %d, want %d", got, sequence)
	}
	for offset := 8; offset < len(bytes); offset++ {
		want := byte(uint64(sequence)*131 + uint64(offset)*29)
		if bytes[offset] != want {
			return fmt.Errorf("sequence %d byte %d is %d, want %d", sequence, offset, bytes[offset], want)
		}
	}
	return nil
}

func TestJournalRecordPublicationOrdering(t *testing.T) {
	journal := newTestJournal(t, 1<<16, journalOrderingRecordBytes)
	producer := newTestProducer(t, journal)
	records := journalOrderingCount(t)
	failures := make(chan error, 1)
	var consumed atomic.Uint64

	go func() {
		cursor := ringCursor{sequence: 1}
		for int(cursor.sequence) <= records {
			publish, ok := journal.region.snapshotPublish()
			if !ok || cursor == publish {
				runtime.Gosched()
				continue
			}
			proof, err := journal.region.beginDurableProof(cursor, publish)
			if err != nil {
				failures <- err
				return
			}
			for cursor != publish {
				record, found, err := journal.region.recordAt(cursor, publish)
				if err != nil || !found {
					failures <- fmt.Errorf("record %d: found=%v: %w", cursor.sequence, found, err)
					return
				}
				if err := validateJournalOrderingRecord(record.bytes, cursor.sequence); err != nil {
					failures <- err
					return
				}
				if err := proof.include(record); err != nil {
					failures <- err
					return
				}
				cursor = record.next
			}
			if err := journal.region.advanceDurable(proof); err != nil {
				failures <- err
				return
			}
			consumed.Store(uint64(cursor.sequence - 1))
		}
		failures <- nil
	}()

	var bytes [journalOrderingRecordBytes]byte
	progressDeadline := time.Now().Add(10 * time.Second)
	for sequence := Sequence(1); int(sequence) <= records; {
		fillJournalOrderingRecord(bytes[:], sequence)
		_, err := producer.Write(bytes[:])
		if errors.Is(err, ErrFull) {
			select {
			case failure := <-failures:
				if failure != nil {
					t.Fatal(failure)
				}
				t.Fatalf("reader stopped after %d records", consumed.Load())
			default:
			}
			if time.Now().After(progressDeadline) {
				t.Fatalf("no publication progress after sequence %d", sequence)
			}
			runtime.Gosched()
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		sequence++
		progressDeadline = time.Now().Add(10 * time.Second)
	}
	if err := <-failures; err != nil {
		t.Fatal(err)
	}
}

type journalInvalidationAttempt struct {
	cursor  ringCursor
	publish ringCursor
	value   uint64
}

func TestJournalReservationInvalidationOrdering(t *testing.T) {
	journal := newTestJournal(t, 1<<16, 8)
	producer := newTestProducer(t, journal)
	descriptor, ok := packDescriptor(8, 1, 0)
	if !ok {
		t.Fatal("packDescriptor rejected ordering descriptor")
	}
	payload := (*uint64)(unsafe.Pointer(&journal.region.mirroredArena[0]))
	attempts := make(chan journalInvalidationAttempt)
	results := make(chan error)
	go func() {
		for attempt := range attempts {
			for orderedatomic.LoadRelaxed64(payload) != attempt.value {
				runtime.Gosched()
			}
			_, found, err := journal.region.recordAt(attempt.cursor, attempt.publish)
			if err == nil && found {
				results <- fmt.Errorf("record %d remained stable after payload overwrite", attempt.cursor.sequence)
				continue
			}
			if !errors.Is(err, ErrOverwritten) {
				results <- fmt.Errorf("record %d validation = found %v: %w", attempt.cursor.sequence, found, err)
				continue
			}
			results <- nil
		}
	}()
	defer close(attempts)

	for iteration := 1; iteration <= journalOrderingCount(t); iteration++ {
		oldSequence := Sequence(iteration)
		newSequence := oldSequence + 1
		oldPosition := uint64(iteration-1) * journal.Capacity()
		newPosition := oldPosition + journal.Capacity()
		oldValue := uint64(oldSequence)*0x9e3779b97f4a7c15 + 1
		newValue := uint64(newSequence)*0x9e3779b97f4a7c15 + 1
		orderedatomic.StoreRelaxed64(payload, oldValue)
		orderedatomic.StoreRelaxed64(journal.region.descriptor(oldPosition), descriptor)
		orderedatomic.StoreRelease64(journal.region.tag(oldPosition), StableTag(oldSequence))
		producer.sequence = newSequence
		producer.position = newPosition
		producer.durable = ringCursor{sequence: newSequence, position: newPosition}
		attempts <- journalInvalidationAttempt{
			cursor:  ringCursor{sequence: oldSequence, position: oldPosition},
			publish: ringCursor{sequence: newSequence, position: oldPosition + uint64(JournalGrain)},
			value:   newValue,
		}
		span, err := producer.Reserve(8)
		if err != nil {
			t.Fatal(err)
		}
		orderedatomic.StoreRelaxed64((*uint64)(unsafe.Pointer(&span.Bytes()[0])), newValue)
		if err := <-results; err != nil {
			t.Fatal(err)
		}
		if err := producer.Abort(span); err != nil {
			t.Fatal(err)
		}
	}
}

func TestJournalRecordRevalidationOrdering(t *testing.T) {
	journal := newTestJournal(t, 1<<16, 16)
	producer := newTestProducer(t, journal)
	oldDescriptor, ok := packDescriptor(8, 1, 0)
	if !ok {
		t.Fatal("packDescriptor rejected old descriptor")
	}
	ready := make(chan ringCursor)
	start := make(chan struct{})
	results := make(chan error)
	go func() {
		for cursor := range ready {
			publish := ringCursor{sequence: cursor.sequence + 1, position: cursor.position + uint64(JournalGrain)}
			start <- struct{}{}
			for orderedatomic.LoadAcquire64(journal.region.tag(cursor.position)) == StableTag(cursor.sequence) {
				record, found, err := journal.region.recordAt(cursor, publish)
				if err == nil && found && record.length != 8 {
					results <- fmt.Errorf("record %d length %d under old tag", cursor.sequence, record.length)
					goto next
				}
				if err != nil && !errors.Is(err, ErrOverwritten) {
					results <- err
					goto next
				}
			}
			results <- nil
		next:
		}
	}()
	defer close(ready)

	for iteration := 1; iteration <= journalOrderingCount(t); iteration++ {
		oldSequence := Sequence(iteration)
		newSequence := oldSequence + 1
		oldPosition := uint64(iteration-1) * journal.Capacity()
		newPosition := oldPosition + journal.Capacity()
		orderedatomic.StoreRelaxed64(journal.region.descriptor(oldPosition), oldDescriptor)
		orderedatomic.StoreRelease64(journal.region.tag(oldPosition), StableTag(oldSequence))
		producer.sequence = newSequence
		producer.position = newPosition
		producer.durable = ringCursor{sequence: newSequence, position: newPosition}
		ready <- ringCursor{sequence: oldSequence, position: oldPosition}
		<-start
		span, err := producer.Reserve(16)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := producer.Commit(span, 16); err != nil {
			t.Fatal(err)
		}
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
}
