//go:build linux && (amd64 || arm64)

package mpsc

import (
	"errors"
	"sync/atomic"
	"syscall"
	"testing"
)

func TestSPSCShortCommitReusesReservationSlack(t *testing.T) {
	q, err := CreateSPSC(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	writer, err := q.newWriter()
	if err != nil {
		t.Fatal(err)
	}
	reader, err := q.newReader()
	if err != nil {
		t.Fatal(err)
	}

	first, status := writer.Reserve(int(q.Capacity()))
	if status != statusOK {
		t.Fatal(status)
	}
	first.Bytes()[0] = 7
	if err := writer.Commit(first, 1); err != nil {
		t.Fatal(err)
	}
	second, status := writer.Reserve(int(q.Capacity() - cacheLine))
	if status != statusOK {
		t.Fatalf("reserve reclaimed capacity: %v", status)
	}
	if err := writer.Abort(second); err != nil {
		t.Fatal(err)
	}
	record, ok, err := reader.Peek()
	if err != nil || !ok || len(record.Bytes()) != 1 || record.Bytes()[0] != 7 {
		t.Fatalf("peek short commit: ok=%v len=%d err=%v", ok, len(record.Bytes()), err)
	}
	if err := reader.Pop(); err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSPSCStaleProducerGenerationFencesReserveAndCommit(t *testing.T) {
	q, err := CreateSPSC(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	writer, err := q.newWriter()
	if err != nil {
		t.Fatal(err)
	}
	generation := q.ptr32(controlSize + shardSize + 12)

	atomic.AddUint32(generation, 1)
	if _, status := writer.Reserve(1); status == statusOK {
		t.Fatal("stale producer reserved")
	}
	atomic.StoreUint32(generation, writer.generation)
	span, status := writer.Reserve(1)
	if status != statusOK {
		t.Fatal(status)
	}
	atomic.AddUint32(generation, 1)
	if err := writer.Commit(span, 1); !errors.Is(err, ErrMisuse) {
		t.Fatalf("stale commit: %v", err)
	}
	if got := atomic.LoadUint64(q.ptr64(controlSize)); got != writer.tail {
		t.Fatalf("stale commit advanced tail to %d", got)
	}
	atomic.StoreUint32(generation, writer.generation)
	if err := writer.Abort(span); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSPSCWriterRequiresSharedOwnerAndGeneration(t *testing.T) {
	q, err := CreateSPSC(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	writer, err := q.newWriter()
	if err != nil {
		t.Fatal(err)
	}
	owner := q.ptr32(controlSize + shardSize + 8)
	storeRelease32(owner, 0)
	if _, status := writer.Reserve(1); status != statusMisuse {
		t.Fatalf("reserve without shared owner: %v", status)
	}
	if status := writer.Write(nil); status != statusMisuse {
		t.Fatalf("write without shared owner: %v", status)
	}
	storeRelease32(owner, uint32(writer.tid))
	span, status := writer.Reserve(1)
	if status != statusOK {
		t.Fatal(status)
	}
	storeRelease32(owner, 0)
	if err := writer.Commit(span, 1); !errors.Is(err, ErrMisuse) {
		t.Fatalf("commit without shared owner: %v", err)
	}
	if err := writer.Abort(span); !errors.Is(err, ErrMisuse) {
		t.Fatalf("abort without shared owner: %v", err)
	}
	storeRelease32(owner, uint32(writer.tid))
	if err := writer.Abort(span); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSPSCIncompleteOwnerPublicationAdvancesGeneration(t *testing.T) {
	q, err := CreateSPSC(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	bitmap := q.ptr64(controlSize + shardSize)
	generation := q.ptr32(controlSize + shardSize + 12)
	atomic.StoreUint64(bitmap, 1)
	before := atomic.LoadUint32(generation)
	writer, err := q.newWriter()
	if err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadUint32(generation); got != before+1 {
		t.Fatalf("generation: got %d want %d", got, before+1)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSPSCDetachInterleavingRejectsOverlappingWriters(t *testing.T) {
	q, err := CreateSPSC(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	owner := q.ptr32(controlSize + shardSize + 8)
	bitmap := q.ptr64(controlSize + shardSize)
	detaching := uint32(syscall.Gettid()) | spscWriterDetaching

	atomic.StoreUint32(owner, detaching)
	atomic.StoreUint64(bitmap, 1)
	if _, err := q.newWriter(); !errors.Is(err, ErrBusy) {
		t.Fatalf("attach before bitmap clear: %v", err)
	}
	atomic.StoreUint64(bitmap, 0)
	if _, err := q.newWriter(); !errors.Is(err, ErrBusy) {
		t.Fatalf("attach after bitmap clear: %v", err)
	}
	atomic.StoreUint32(owner, 0)
	writer, err := q.newWriter()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.newWriter(); !errors.Is(err, ErrBusy) {
		t.Fatalf("overlapping attach after detach: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSPSCDeadDetachingWriterCanBeTakenOver(t *testing.T) {
	q, err := CreateSPSC(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	owner := q.ptr32(controlSize + shardSize + 8)
	generation := q.ptr32(controlSize + shardSize + 12)
	atomic.StoreUint32(owner, deadTid(t)|spscWriterDetaching)
	before := atomic.LoadUint32(generation)
	writer, err := q.newWriter()
	if err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadUint32(generation); got != before+1 {
		t.Fatalf("generation: got %d want %d", got, before+1)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSPSCReaderRejectsForgedMetadata(t *testing.T) {
	for _, test := range []struct {
		name   string
		length uint64
		tail   uint64
	}{
		{name: "derived extent exceeds capacity", length: 4097, tail: 8192},
		{name: "derived extent exceeds published tail", length: 65, tail: 64},
	} {
		t.Run(test.name, func(t *testing.T) {
			q, err := CreateSPSC(Config{Capacity: 4096})
			if err != nil {
				t.Fatal(err)
			}
			reader, err := q.newReader()
			if err != nil {
				t.Fatal(err)
			}
			atomic.StoreUint64(q.ptr64(controlSize), test.tail)
			atomic.StoreUint64(q.slot(reader.rd), test.length)
			if _, ok, err := reader.Peek(); ok || !errors.Is(err, ErrFormat) {
				t.Fatalf("peek forged metadata: ok=%v err=%v", ok, err)
			}
			if err := reader.Close(); err != nil {
				t.Fatal(err)
			}
			if err := q.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSPSCWrappedShortRecordUsesMirroredArena(t *testing.T) {
	q, err := CreateSPSC(Config{Capacity: 4096})
	if err != nil {
		t.Fatal(err)
	}
	writer, err := q.newWriter()
	if err != nil {
		t.Fatal(err)
	}
	reader, err := q.newReader()
	if err != nil {
		t.Fatal(err)
	}
	prefix, status := writer.Reserve(4032)
	if status != statusOK {
		t.Fatal(status)
	}
	if err := writer.Commit(prefix, 4032); err != nil {
		t.Fatal(err)
	}
	record, ok, err := reader.Peek()
	if err != nil || !ok {
		t.Fatalf("prefix peek: ok=%v err=%v", ok, err)
	}
	if err := reader.Pop(); err != nil {
		t.Fatal(err)
	}

	span, status := writer.Reserve(128)
	if status != statusOK {
		t.Fatal(status)
	}
	for i := range 65 {
		span.Bytes()[i] = byte(i)
	}
	if err := writer.Commit(span, 65); err != nil {
		t.Fatal(err)
	}
	record, ok, err = reader.Peek()
	if err != nil || !ok || len(record.Bytes()) != 65 {
		t.Fatalf("wrapped peek: ok=%v len=%d err=%v", ok, len(record.Bytes()), err)
	}
	for i, got := range record.Bytes() {
		if got != byte(i) {
			t.Fatalf("wrapped byte %d: got %d", i, got)
		}
	}
	if err := reader.Pop(); err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSPSCReserveReportsMisuseStatus(t *testing.T) {
	q, err := CreateSPSC(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	writer, err := q.newWriter()
	if err != nil {
		t.Fatal(err)
	}
	span, status := writer.Reserve(1)
	if status != statusOK {
		t.Fatal(status)
	}
	if _, status := writer.Reserve(1); status != statusMisuse {
		t.Fatalf("live reservation status: %v", status)
	}
	if err := writer.Abort(span); err != nil {
		t.Fatal(err)
	}
	atomic.AddUint32(q.ptr32(controlSize+shardSize+12), 1)
	if _, status := writer.Reserve(1); status != statusMisuse {
		t.Fatalf("stale generation status: %v", status)
	}
	atomic.StoreUint32(q.ptr32(controlSize+shardSize+12), writer.generation)
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSPSCFirstProvenFullChecksReaderLiveness(t *testing.T) {
	q, err := CreateSPSC(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	writer, err := q.newWriter()
	if err != nil {
		t.Fatal(err)
	}
	reader, err := q.newReader()
	if err != nil {
		t.Fatal(err)
	}
	span, status := writer.Reserve(int(q.Capacity()))
	if status != statusOK {
		t.Fatal(status)
	}
	if err := writer.Commit(span, int(q.Capacity())); err != nil {
		t.Fatal(err)
	}
	readerOwner := q.ptr32(28)
	atomic.StoreUint32(readerOwner, deadTid(t))
	if _, status := writer.Reserve(1); status != statusReaderDead {
		t.Fatalf("first full observation: %v", status)
	}
	atomic.StoreUint32(readerOwner, uint32(reader.tid))
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
}
