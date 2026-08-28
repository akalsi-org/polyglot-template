//go:build linux && (amd64 || arm64)

package mpsc

import (
	"errors"
	"sync/atomic"
	"testing"
)

func TestMPSCCommitDetectsRecoveredReservation(t *testing.T) {
	q, err := CreateMPSC(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	w, err := q.newWriter()
	if err != nil {
		t.Fatal(err)
	}
	span, status := w.Reserve(8)
	if status != statusOK {
		t.Fatal(status)
	}
	length, tag := q.result(span.pos)
	if got := atomic.LoadUint64(tag); got != reservationTag(span.pos) {
		t.Fatalf("reservation tag: %#x", got)
	}
	atomic.StoreUint64(length, 41)
	atomic.StoreUint64(tag, 0)
	if err := w.Commit(span, 8); !errors.Is(err, ErrRecovered) {
		t.Fatalf("commit: %v", err)
	}
	if got := atomic.LoadUint64(length); got != 41 {
		t.Fatalf("stale commit changed length: %d", got)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestMPSCDeadCommittingClearedRecordIsAborted(t *testing.T) {
	q, err := CreateMPSC(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	consumer, err := q.AttachConsumer()
	if err != nil {
		t.Fatal(err)
	}
	atomic.StoreUint64(q.claim(0), packrecord(64, stateCleared, deadTid(t)))
	length, tag := q.result(0)
	atomic.StoreUint64(length, 37)
	atomic.StoreUint64(tag, committingTag(0))
	consumer.core.busyPos = 0
	consumer.core.busyThreshold = 1
	if record, ok, err := consumer.Peek(); err != nil || ok || record.UnsafeBytes() != nil {
		t.Fatalf("peek: %v %v", ok, err)
	}
	if got := atomic.LoadUint64(tag); got != 0 {
		t.Fatalf("recovered tag: %#x", got)
	}
	if got := atomic.LoadUint64(q.claim(0)) & stateMask; got != stateAborted {
		t.Fatalf("claim state: %d", got)
	}
	if err := consumer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestMPSCRecoveryInvalidatesReservationTag(t *testing.T) {
	q, err := CreateMPSC(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	consumer, err := q.AttachConsumer()
	if err != nil {
		t.Fatal(err)
	}
	atomic.StoreUint64(q.claim(0), packrecord(64, stateCleared, deadTid(t)))
	_, tag := q.result(0)
	atomic.StoreUint64(tag, reservationTag(0))
	consumer.core.busyPos = 0
	consumer.core.busyThreshold = 1
	if record, ok, err := consumer.Peek(); err != nil || ok || record.UnsafeBytes() != nil {
		t.Fatalf("peek: %v %v", ok, err)
	}
	if got := atomic.LoadUint64(tag); got != 0 {
		t.Fatalf("recovered tag: %#x", got)
	}
	if err := consumer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestMPSCRejectsForgedSharedMetadata(t *testing.T) {
	tests := []struct {
		name   string
		layout MPSCLayout
		extent uint64
		length uint64
	}{
		{name: "zero extent", extent: 0},
		{name: "extent exceeds admission limit", extent: 4096},
		{name: "length exceeds extent", extent: 64, length: 65},
		{name: "length exceeds maximum", extent: 64, length: maxExtent + 1},
		{name: "extent is not plane aligned", layout: MPSCPadded256, extent: 64},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q, err := CreateMPSC(Config{Capacity: 4096, Backend: BackendMemfd, MPSCLayout: tt.layout})
			if err != nil {
				t.Fatal(err)
			}
			consumer, err := q.AttachConsumer()
			if err != nil {
				t.Fatal(err)
			}
			atomic.StoreUint64(q.claim(0), packrecord(tt.extent, stateCleared, deadTid(t)))
			length, tag := q.result(0)
			atomic.StoreUint64(length, tt.length)
			atomic.StoreUint64(tag, commitTag(0))
			if _, _, err := consumer.Peek(); !errors.Is(err, ErrFormat) {
				t.Fatalf("peek: %v", err)
			}
			if got := atomic.LoadUint64(q.ptr64(controlSize + 128)); got != 0 {
				t.Fatalf("read position mutated: %d", got)
			}
			if err := consumer.Close(); err != nil {
				t.Fatal(err)
			}
			if err := q.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMPSCRejectsReadPositionOverflowBeforeMutation(t *testing.T) {
	q, err := CreateMPSC(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	rd := ^uint64(0) - 31
	atomic.StoreUint64(q.ptr64(controlSize+128), rd)
	atomic.StoreUint64(q.claim(rd), packrecord(64, stateAborted, deadTid(t)))
	consumer, err := q.AttachConsumer()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := consumer.Peek(); !errors.Is(err, ErrPositionExhausted) {
		t.Fatalf("peek: %v", err)
	}
	if got := atomic.LoadUint64(q.ptr64(controlSize + 128)); got != rd {
		t.Fatalf("read position mutated: %d", got)
	}
	if err := consumer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestMPSCFirstProvenFullCheckDetectsDeadReader(t *testing.T) {
	q, err := CreateMPSC(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	atomic.StoreUint32(q.ptr32(28), deadTid(t))
	first, err := q.newWriter()
	if err != nil {
		t.Fatal(err)
	}
	span, status := first.Reserve(int(q.Capacity() - uint64(q.r.planeGrain)))
	if status != statusOK {
		t.Fatal(status)
	}
	second, err := q.newWriter()
	if err != nil {
		t.Fatal(err)
	}
	if _, status := second.Reserve(1); status != statusReaderDead {
		t.Fatalf("first full status: %v", status)
	}
	if err := first.Abort(span); err != nil {
		t.Fatal(err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	atomic.StoreUint32(q.ptr32(28), 0)
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
}
