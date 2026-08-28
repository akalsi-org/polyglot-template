//go:build linux && (amd64 || arm64)

package mpsc

import (
	"errors"
	"testing"
)

func TestStatusErrorCoversAllValues(t *testing.T) {
	cases := []struct {
		status status
		want   error
	}{
		{statusOK, nil},
		{statusFull, ErrFull},
		{statusReaderDead, ErrReaderDead},
		{statusTooLarge, ErrTooLarge},
		{statusNoSlot, ErrNoWriterSlot},
		{statusPositionExhausted, ErrPositionExhausted},
		{statusContended, ErrContended},
		{statusMisuse, ErrMisuse},
		{statusRecovered, ErrRecovered},
		{status(255), ErrMisuse},
	}
	for _, tc := range cases {
		got := statusError(tc.status)
		if !errors.Is(got, tc.want) || (tc.want == nil && got != nil) {
			t.Fatalf("status %d: got %v want %v", tc.status, got, tc.want)
		}
	}
}

func TestMPSCCloseCleansLostSpans(t *testing.T) {
	q, err := CreateMPSC(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	producer, err := q.NewProducer()
	if err != nil {
		t.Fatal(err)
	}
	span, err := producer.Reserve(8)
	if err != nil {
		t.Fatal(err)
	}
	if err := producer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := producer.Commit(span, 8); !errors.Is(err, ErrMisuse) {
		t.Fatalf("commit after close: %v", err)
	}
	consumer, err := q.AttachConsumer()
	if err != nil {
		t.Fatal(err)
	}
	if record, ok, err := consumer.Peek(); err != nil || ok || record.Len() != 0 {
		t.Fatalf("aborted record: %v %v", ok, err)
	}
	if err := consumer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestMPSCConsumerCloseRedeliversLostSpan(t *testing.T) {
	q, err := CreateMPSC(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	producer, err := q.NewProducer()
	if err != nil {
		t.Fatal(err)
	}
	if err := producer.Write([]byte("again")); err != nil {
		t.Fatal(err)
	}
	consumer, err := q.AttachConsumer()
	if err != nil {
		t.Fatal(err)
	}
	span, ok, err := consumer.Peek()
	if err != nil || !ok {
		t.Fatalf("peek: %v %v", ok, err)
	}
	if err := consumer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := consumer.Pop(); !errors.Is(err, ErrMisuse) {
		t.Fatalf("pop after close: %v", err)
	}
	consumer, err = q.AttachConsumer()
	if err != nil {
		t.Fatal(err)
	}
	if err := consumer.Pop(); !errors.Is(err, ErrMisuse) {
		t.Fatalf("pop span from previous consumer: %v", err)
	}
	span, ok, err = consumer.Peek()
	if err != nil || !ok || string(span.UnsafeBytes()) != "again" {
		t.Fatalf("redelivery: %v %v", ok, err)
	}
	if err := consumer.Pop(); err != nil {
		t.Fatal(err)
	}
	if err := consumer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := producer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSPSCCloseCleansLostSpans(t *testing.T) {
	q, err := CreateSPSC(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	producer, err := q.AttachProducer()
	if err != nil {
		t.Fatal(err)
	}
	span, err := producer.Reserve(8)
	if err != nil {
		t.Fatal(err)
	}
	if err := producer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := producer.Commit(span, 8); !errors.Is(err, ErrMisuse) {
		t.Fatalf("commit after close: %v", err)
	}
	consumer, err := q.AttachConsumer()
	if err != nil {
		t.Fatal(err)
	}
	if record, ok, err := consumer.Peek(); err != nil || ok || record.Len() != 0 {
		t.Fatalf("aborted record: %v %v", ok, err)
	}
	if err := consumer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSPSCConsumerCloseRedeliversLostSpan(t *testing.T) {
	q, err := CreateSPSC(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	producer, err := q.AttachProducer()
	if err != nil {
		t.Fatal(err)
	}
	if err := producer.Write([]byte("again")); err != nil {
		t.Fatal(err)
	}
	consumer, err := q.AttachConsumer()
	if err != nil {
		t.Fatal(err)
	}
	span, ok, err := consumer.Peek()
	if err != nil || !ok {
		t.Fatalf("peek: %v %v", ok, err)
	}
	if err := consumer.Close(); err != nil {
		t.Fatal(err)
	}
	consumer, err = q.AttachConsumer()
	if err != nil {
		t.Fatal(err)
	}
	if err := consumer.Pop(); !errors.Is(err, ErrMisuse) {
		t.Fatalf("pop span from previous consumer: %v", err)
	}
	span, ok, err = consumer.Peek()
	if err != nil || !ok || string(span.UnsafeBytes()) != "again" {
		t.Fatalf("redelivery: %v %v", ok, err)
	}
	if err := consumer.Pop(); err != nil {
		t.Fatal(err)
	}
	if err := consumer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := producer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCloseRetryAfterWrongThreadError(t *testing.T) {
	wrongThread := func(closeFn func() error) error {
		result := make(chan error, 1)
		go func() { result <- closeFn() }()
		return <-result
	}

	m, err := CreateMPSC(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	mp, err := m.NewProducer()
	if err != nil {
		t.Fatal(err)
	}
	if err := wrongThread(mp.Close); !errors.Is(err, ErrMisuse) {
		t.Fatalf("MPSC producer wrong thread: %v", err)
	}
	if err := mp.Close(); err != nil {
		t.Fatal(err)
	}
	mc, err := m.AttachConsumer()
	if err != nil {
		t.Fatal(err)
	}
	if err := wrongThread(mc.Close); !errors.Is(err, ErrMisuse) {
		t.Fatalf("MPSC consumer wrong thread: %v", err)
	}
	if err := mc.Close(); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := CreateSPSC(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	sp, err := s.AttachProducer()
	if err != nil {
		t.Fatal(err)
	}
	if err := wrongThread(sp.Close); !errors.Is(err, ErrMisuse) {
		t.Fatalf("SPSC producer wrong thread: %v", err)
	}
	if err := sp.Close(); err != nil {
		t.Fatal(err)
	}
	sc, err := s.AttachConsumer()
	if err != nil {
		t.Fatal(err)
	}
	if err := wrongThread(sc.Close); !errors.Is(err, ErrMisuse) {
		t.Fatalf("SPSC consumer wrong thread: %v", err)
	}
	if err := sc.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestZeroValueSPSCHandlesRejectOperations(t *testing.T) {
	var producer SPSCProducer
	if _, err := producer.Reserve(1); !errors.Is(err, ErrMisuse) {
		t.Fatalf("Reserve: %v", err)
	}
	if err := producer.Commit(WriteSpan{}, 0); !errors.Is(err, ErrMisuse) {
		t.Fatalf("Commit: %v", err)
	}
	if err := producer.Abort(WriteSpan{}); !errors.Is(err, ErrMisuse) {
		t.Fatalf("Abort: %v", err)
	}
	if err := producer.Write(nil); !errors.Is(err, ErrMisuse) {
		t.Fatalf("Write: %v", err)
	}
	if err := producer.Close(); !errors.Is(err, ErrMisuse) {
		t.Fatalf("Close: %v", err)
	}
	var consumer SPSCConsumer
	if _, _, err := consumer.Peek(); !errors.Is(err, ErrMisuse) {
		t.Fatalf("Peek: %v", err)
	}
	if err := consumer.Pop(); !errors.Is(err, ErrMisuse) {
		t.Fatalf("Pop: %v", err)
	}
	if err := consumer.Close(); !errors.Is(err, ErrMisuse) {
		t.Fatalf("Close: %v", err)
	}
}

func TestSpansAreBoundToExactHandlesAndQueues(t *testing.T) {
	m1, err := CreateMPSC(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	m2, err := CreateMPSC(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	mp1, err := m1.NewProducer()
	if err != nil {
		t.Fatal(err)
	}
	mp2, err := m2.NewProducer()
	if err != nil {
		t.Fatal(err)
	}
	mspan, err := mp1.Reserve(4)
	if err != nil {
		t.Fatal(err)
	}
	if err := mp2.Commit(mspan, 4); !errors.Is(err, ErrMisuse) {
		t.Fatalf("cross-queue MPSC commit: %v", err)
	}
	if err := mp1.Abort(mspan); err != nil {
		t.Fatal(err)
	}
	if err := mp1.Close(); err != nil {
		t.Fatal(err)
	}
	if err := mp2.Close(); err != nil {
		t.Fatal(err)
	}

	s1, err := CreateSPSC(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	s2, err := CreateSPSC(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	sp1, err := s1.AttachProducer()
	if err != nil {
		t.Fatal(err)
	}
	sp2, err := s2.AttachProducer()
	if err != nil {
		t.Fatal(err)
	}
	sspan, err := sp1.Reserve(4)
	if err != nil {
		t.Fatal(err)
	}
	if err := sp2.Abort(sspan); !errors.Is(err, ErrMisuse) {
		t.Fatalf("cross-queue SPSC abort: %v", err)
	}
	if err := sp1.Abort(sspan); err != nil {
		t.Fatal(err)
	}
	if err := sp1.Close(); err != nil {
		t.Fatal(err)
	}
	if err := sp2.Close(); err != nil {
		t.Fatal(err)
	}

	mp1, err = m1.NewProducer()
	if err != nil {
		t.Fatal(err)
	}
	mp2, err = m2.NewProducer()
	if err != nil {
		t.Fatal(err)
	}
	if err := mp1.Write([]byte("one")); err != nil {
		t.Fatal(err)
	}
	if err := mp2.Write([]byte("two")); err != nil {
		t.Fatal(err)
	}
	mc1, err := m1.AttachConsumer()
	if err != nil {
		t.Fatal(err)
	}
	mc2, err := m2.AttachConsumer()
	if err != nil {
		t.Fatal(err)
	}
	_, ok, err := mc1.Peek()
	if err != nil || !ok {
		t.Fatalf("MPSC queue one peek: %v %v", ok, err)
	}
	if err := mc2.Pop(); !errors.Is(err, ErrMisuse) {
		t.Fatalf("cross-queue MPSC pop: %v", err)
	}
	if err := mc1.Pop(); err != nil {
		t.Fatal(err)
	}
	_, ok, err = mc2.Peek()
	if err != nil || !ok {
		t.Fatalf("MPSC queue two peek: %v %v", ok, err)
	}
	if err := mc2.Pop(); err != nil {
		t.Fatal(err)
	}
	if err := mc1.Close(); err != nil {
		t.Fatal(err)
	}
	if err := mc2.Close(); err != nil {
		t.Fatal(err)
	}
	if err := mp1.Close(); err != nil {
		t.Fatal(err)
	}
	if err := mp2.Close(); err != nil {
		t.Fatal(err)
	}

	sp1, err = s1.AttachProducer()
	if err != nil {
		t.Fatal(err)
	}
	sp2, err = s2.AttachProducer()
	if err != nil {
		t.Fatal(err)
	}
	if err := sp1.Write([]byte("one")); err != nil {
		t.Fatal(err)
	}
	if err := sp2.Write([]byte("two")); err != nil {
		t.Fatal(err)
	}
	sc1, err := s1.AttachConsumer()
	if err != nil {
		t.Fatal(err)
	}
	sc2, err := s2.AttachConsumer()
	if err != nil {
		t.Fatal(err)
	}
	_, ok, err = sc1.Peek()
	if err != nil || !ok {
		t.Fatalf("SPSC queue one peek: %v %v", ok, err)
	}
	if err := sc2.Pop(); !errors.Is(err, ErrMisuse) {
		t.Fatalf("cross-queue SPSC pop: %v", err)
	}
	if err := sc1.Pop(); err != nil {
		t.Fatal(err)
	}
	_, ok, err = sc2.Peek()
	if err != nil || !ok {
		t.Fatalf("SPSC queue two peek: %v %v", ok, err)
	}
	if err := sc2.Pop(); err != nil {
		t.Fatal(err)
	}
	if err := sc1.Close(); err != nil {
		t.Fatal(err)
	}
	if err := sc2.Close(); err != nil {
		t.Fatal(err)
	}
	if err := sp1.Close(); err != nil {
		t.Fatal(err)
	}
	if err := sp2.Close(); err != nil {
		t.Fatal(err)
	}

	if err := m1.Close(); err != nil {
		t.Fatal(err)
	}
	if err := m2.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s2.Close(); err != nil {
		t.Fatal(err)
	}
}
