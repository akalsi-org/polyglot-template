//go:build linux && (amd64 || arm64)

package mpsc

import (
	"errors"
	"runtime"
	"testing"

	"github.com/akalsi-org/polyglot-template/go/lib/hostcpu"
)

func onOtherThread(fn func() error) error {
	result := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		result <- fn()
	}()
	return <-result
}

func requireThreadMisuse(t *testing.T, name string, fn func() error) {
	t.Helper()
	if err := onOtherThread(fn); !errors.Is(err, ErrMisuse) {
		t.Fatalf("%s: %v", name, err)
	}
}

func TestMPSCOperationsEnforceThreadOwnership(t *testing.T) {
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

	requireThreadMisuse(t, "Reserve", func() error {
		_, err := producer.Reserve(1)
		return err
	})
	requireThreadMisuse(t, "Write", func() error { return producer.Write([]byte("x")) })

	span, err := producer.Reserve(1)
	if err != nil {
		t.Fatal(err)
	}
	requireThreadMisuse(t, "Commit", func() error { return producer.Commit(span, 1) })
	span.Bytes()[0] = 'a'
	if err := producer.Commit(span, 1); err != nil {
		t.Fatal(err)
	}

	span, err = producer.Reserve(1)
	if err != nil {
		t.Fatal(err)
	}
	requireThreadMisuse(t, "Abort", func() error { return producer.Abort(span) })
	if err := producer.Abort(span); err != nil {
		t.Fatal(err)
	}

	requireThreadMisuse(t, "Peek", func() error {
		_, _, err := consumer.Peek()
		return err
	})
	if _, ok, err := consumer.Peek(); err != nil || !ok {
		t.Fatalf("owner Peek: ok=%t err=%v", ok, err)
	}
	requireThreadMisuse(t, "Pop", consumer.Pop)
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

func TestThreadIdentityRejectsMatchingReusedTID(t *testing.T) {
	q, err := CreateMPSC(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	producer, err := q.NewProducer()
	if err != nil {
		t.Fatal(err)
	}
	originalTID := producer.core.tid
	err = onOtherThread(func() error {
		producer.core.tid = hostcpu.CurrentThreadId()
		return producer.Write(nil)
	})
	producer.core.tid = originalTID
	if !errors.Is(err, ErrMisuse) {
		t.Fatalf("matching reused TID: %v", err)
	}
	if err := producer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSPSCOperationsEnforceThreadOwnership(t *testing.T) {
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

	requireThreadMisuse(t, "Reserve", func() error {
		_, err := producer.Reserve(1)
		return err
	})
	requireThreadMisuse(t, "Write", func() error { return producer.Write([]byte("x")) })

	span, err := producer.Reserve(1)
	if err != nil {
		t.Fatal(err)
	}
	requireThreadMisuse(t, "Commit", func() error { return producer.Commit(span, 1) })
	span.Bytes()[0] = 'a'
	if err := producer.Commit(span, 1); err != nil {
		t.Fatal(err)
	}

	span, err = producer.Reserve(1)
	if err != nil {
		t.Fatal(err)
	}
	requireThreadMisuse(t, "Abort", func() error { return producer.Abort(span) })
	if err := producer.Abort(span); err != nil {
		t.Fatal(err)
	}

	requireThreadMisuse(t, "Peek", func() error {
		_, _, err := consumer.Peek()
		return err
	})
	if _, ok, err := consumer.Peek(); err != nil || !ok {
		t.Fatalf("owner Peek: ok=%t err=%v", ok, err)
	}
	requireThreadMisuse(t, "Pop", consumer.Pop)
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
