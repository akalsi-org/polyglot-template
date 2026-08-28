//go:build linux && (amd64 || arm64)

package mpsc

import (
	"errors"
	"reflect"
	"runtime"
	"testing"

	"github.com/akalsi-org/polyglot-template/go/lib/hostcpu"
)

func reflectCopy[T any](source *T) *T {
	copy := reflect.New(reflect.TypeOf(source).Elem())
	copy.Elem().Set(reflect.ValueOf(source).Elem())
	return copy.Interface().(*T)
}

func TestPublicMPSCAPIAndBorrowGuards(t *testing.T) {
	q, err := CreateMPSC(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	producer, err := q.NewProducer()
	if err != nil {
		t.Fatal(err)
	}
	consumer, err := q.AttachConsumer()
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := producer.Reserve(8)
	if err != nil {
		t.Fatal(err)
	}
	copy(reservation.Bytes(), "payload!")
	if err := q.Close(); !errors.Is(err, ErrBusy) {
		t.Fatalf("close with borrow: %v", err)
	}
	if err := producer.Commit(reservation, 7); err != nil {
		t.Fatal(err)
	}
	record, ok, err := consumer.Peek()
	if err != nil || !ok {
		t.Fatalf("peek: %v %v", ok, err)
	}
	if string(record.UnsafeBytes()) != "payload" {
		t.Fatalf("got %q", record.UnsafeBytes())
	}
	if _, _, err := consumer.Peek(); !errors.Is(err, ErrMisuse) {
		t.Fatalf("second peek: %v", err)
	}
	if err := consumer.Pop(); err != nil {
		t.Fatal(err)
	}
	if err := consumer.Pop(); !errors.Is(err, ErrMisuse) {
		t.Fatalf("double pop: %v", err)
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

func TestMPSCWriteRejectsInvalidOperationSequence(t *testing.T) {
	q, err := CreateMPSC(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	producer, err := q.NewProducer()
	if err != nil {
		t.Fatal(err)
	}
	span, err := producer.Reserve(1)
	if err != nil {
		t.Fatal(err)
	}
	if err := producer.Write(nil); !errors.Is(err, ErrMisuse) {
		t.Fatalf("write with reservation: %v", err)
	}
	if err := producer.Abort(span); err != nil {
		t.Fatal(err)
	}
	if err := producer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := producer.Write(nil); !errors.Is(err, ErrMisuse) {
		t.Fatalf("write after close: %v", err)
	}
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPublicSPSCAPI(t *testing.T) {
	q, err := CreateSPSC(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	producer, err := q.AttachProducer()
	if err != nil {
		t.Fatal(err)
	}
	consumer, err := q.AttachConsumer()
	if err != nil {
		t.Fatal(err)
	}
	if err := producer.Write(nil); err != nil {
		t.Fatal(err)
	}
	record, ok, err := consumer.Peek()
	if err != nil || !ok || record.UnsafeBytes() == nil || len(record.UnsafeBytes()) != 0 {
		t.Fatalf("zero record: %v %v", ok, err)
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

func TestSpansBindToOriginatingHandle(t *testing.T) {
	q, err := CreateMPSC(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	first, err := q.NewProducer()
	if err != nil {
		t.Fatal(err)
	}
	second, err := q.NewProducer()
	if err != nil {
		t.Fatal(err)
	}
	span, err := first.Reserve(8)
	if err != nil {
		t.Fatal(err)
	}
	if cap(span.Bytes()) != len(span.Bytes()) {
		t.Fatalf("write span capacity: got %d want %d", cap(span.Bytes()), len(span.Bytes()))
	}
	if err := second.Commit(span, 8); !errors.Is(err, ErrMisuse) {
		t.Fatalf("cross-handle commit: %v", err)
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
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestZeroValueHandlesRejectOperations(t *testing.T) {
	var producer MPSCProducer
	if _, err := producer.Reserve(1); !errors.Is(err, ErrMisuse) {
		t.Fatalf("zero producer Reserve: %v", err)
	}
	if err := producer.Close(); !errors.Is(err, ErrMisuse) {
		t.Fatalf("zero producer Close: %v", err)
	}
	var consumer MPSCConsumer
	if _, _, err := consumer.Peek(); !errors.Is(err, ErrMisuse) {
		t.Fatalf("zero consumer Peek: %v", err)
	}
	if err := consumer.Close(); !errors.Is(err, ErrMisuse) {
		t.Fatalf("zero consumer Close: %v", err)
	}
}

func TestCopiedQueuesRejectOperations(t *testing.T) {
	m, err := CreateMPSC(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	mCopy := reflectCopy(m)
	if mCopy.Capacity() != 0 {
		t.Fatal("copied MPSC reported capacity")
	}
	if _, err := mCopy.NewProducer(); !errors.Is(err, ErrMisuse) {
		t.Fatalf("copied MPSC producer: %v", err)
	}
	if err := mCopy.Close(); !errors.Is(err, ErrMisuse) {
		t.Fatalf("copied MPSC close: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := CreateSPSC(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	sCopy := reflectCopy(s)
	if sCopy.Capacity() != 0 {
		t.Fatal("copied SPSC reported capacity")
	}
	if _, err := sCopy.AttachProducer(); !errors.Is(err, ErrMisuse) {
		t.Fatalf("copied SPSC producer: %v", err)
	}
	if err := sCopy.Close(); !errors.Is(err, ErrMisuse) {
		t.Fatalf("copied SPSC close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCopiedHandlesCannotReleaseOriginalOwnership(t *testing.T) {
	m, err := CreateMPSC(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	mp, err := m.NewProducer()
	if err != nil {
		t.Fatal(err)
	}
	mc, err := m.AttachConsumer()
	if err != nil {
		t.Fatal(err)
	}
	if err := reflectCopy(mp).Close(); !errors.Is(err, ErrMisuse) {
		t.Fatalf("copied MPSC producer close: %v", err)
	}
	if err := reflectCopy(mc).Close(); !errors.Is(err, ErrMisuse) {
		t.Fatalf("copied MPSC consumer close: %v", err)
	}
	if got := m.active.Load(); got != 2 {
		t.Fatalf("MPSC active after copied closes: %d", got)
	}
	if err := mc.Close(); err != nil {
		t.Fatal(err)
	}
	if err := mp.Close(); err != nil {
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
	sc, err := s.AttachConsumer()
	if err != nil {
		t.Fatal(err)
	}
	if err := reflectCopy(sp).Close(); !errors.Is(err, ErrMisuse) {
		t.Fatalf("copied SPSC producer close: %v", err)
	}
	if err := reflectCopy(sc).Close(); !errors.Is(err, ErrMisuse) {
		t.Fatalf("copied SPSC consumer close: %v", err)
	}
	if got := s.active.Load(); got != 2 {
		t.Fatalf("SPSC active after copied closes: %d", got)
	}
	if err := sc.Close(); err != nil {
		t.Fatal(err)
	}
	if err := sp.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestQueueCloseReapsProvenDeadLocalHandles(t *testing.T) {
	oldProbe := threadAliveProbe
	threadAliveProbe = func(hostcpu.ThreadId) bool { return false }
	defer func() { threadAliveProbe = oldProbe }()

	t.Run("MPSC producer", func(t *testing.T) {
		q, err := CreateMPSC(testConfig())
		if err != nil {
			t.Fatal(err)
		}
		producer, err := q.NewProducer()
		if err != nil {
			t.Fatal(err)
		}
		runtime.UnlockOSThread()
		if err := q.Close(); err != nil {
			t.Fatal(err)
		}
		if err := producer.Write(nil); !errors.Is(err, ErrMisuse) {
			t.Fatalf("reaped producer write: %v", err)
		}
	})

	t.Run("MPSC consumer", func(t *testing.T) {
		q, err := CreateMPSC(testConfig())
		if err != nil {
			t.Fatal(err)
		}
		consumer, err := q.AttachConsumer()
		if err != nil {
			t.Fatal(err)
		}
		runtime.UnlockOSThread()
		if err := q.Close(); err != nil {
			t.Fatal(err)
		}
		if _, _, err := consumer.Peek(); !errors.Is(err, ErrMisuse) {
			t.Fatalf("reaped consumer peek: %v", err)
		}
	})

	t.Run("SPSC producer", func(t *testing.T) {
		q, err := CreateSPSC(testConfig())
		if err != nil {
			t.Fatal(err)
		}
		producer, err := q.AttachProducer()
		if err != nil {
			t.Fatal(err)
		}
		runtime.UnlockOSThread()
		if err := q.Close(); err != nil {
			t.Fatal(err)
		}
		if err := producer.Write(nil); !errors.Is(err, ErrMisuse) {
			t.Fatalf("reaped producer write: %v", err)
		}
	})

	t.Run("SPSC consumer", func(t *testing.T) {
		q, err := CreateSPSC(testConfig())
		if err != nil {
			t.Fatal(err)
		}
		consumer, err := q.AttachConsumer()
		if err != nil {
			t.Fatal(err)
		}
		runtime.UnlockOSThread()
		if err := q.Close(); err != nil {
			t.Fatal(err)
		}
		if _, _, err := consumer.Peek(); !errors.Is(err, ErrMisuse) {
			t.Fatalf("reaped consumer peek: %v", err)
		}
	})
}

func TestMPSCLayouts(t *testing.T) {
	for _, layout := range []MPSCLayout{MPSCCompact, MPSCPadded64, MPSCPadded256} {
		t.Run(string(rune('0'+layout)), func(t *testing.T) {
			cfg := testConfig()
			cfg.MPSCLayout = layout
			q, err := CreateMPSC(cfg)
			if err != nil {
				t.Fatal(err)
			}
			producer, err := q.NewProducer()
			if err != nil {
				t.Fatal(err)
			}
			consumer, err := q.AttachConsumer()
			if err != nil {
				t.Fatal(err)
			}
			if err := producer.Write([]byte("x")); err != nil {
				t.Fatal(err)
			}
			record, ok, err := consumer.Peek()
			if err != nil || !ok || string(record.UnsafeBytes()) != "x" {
				t.Fatalf("peek: %v %v", ok, err)
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
		})
	}
}
