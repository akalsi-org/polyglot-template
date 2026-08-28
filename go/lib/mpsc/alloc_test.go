//go:build linux && (amd64 || arm64)

package mpsc

import "testing"

func TestMPSCHotPathAllocations(t *testing.T) {
	q, err := CreateMPSC(Config{Capacity: 1 << 20})
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
	var writeSpan WriteSpan
	var operationErr error
	allocs := testing.AllocsPerRun(1000, func() {
		writeSpan, operationErr = producer.Reserve(1)
		if operationErr == nil {
			operationErr = producer.Commit(writeSpan, 1)
		}
	})
	if operationErr != nil {
		t.Fatal(operationErr)
	}
	if allocs != 0 {
		t.Fatalf("Reserve/Commit allocations: %g", allocs)
	}
	var ok bool
	allocs = testing.AllocsPerRun(1000, func() {
		_, ok, operationErr = consumer.Peek()
		if operationErr == nil && ok {
			operationErr = consumer.Pop()
		}
	})
	if operationErr != nil || !ok {
		t.Fatalf("Peek/Pop: %v %v", ok, operationErr)
	}
	if allocs != 0 {
		t.Fatalf("Peek/Pop allocations: %g", allocs)
	}
	payload := []byte{1}
	allocs = testing.AllocsPerRun(1000, func() {
		operationErr = producer.Write(payload)
		if operationErr == nil {
			_, ok, operationErr = consumer.Peek()
		}
		if operationErr == nil && ok {
			operationErr = consumer.Pop()
		}
	})
	if operationErr != nil || !ok {
		t.Fatalf("Write/Peek/Pop: %v %v", ok, operationErr)
	}
	if allocs != 0 {
		t.Fatalf("Write allocations: %g", allocs)
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

func TestSPSCHotPathAllocations(t *testing.T) {
	q, err := CreateSPSC(Config{Capacity: 1 << 20})
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
	var writeSpan WriteSpan
	var operationErr error
	allocs := testing.AllocsPerRun(1000, func() {
		writeSpan, operationErr = producer.Reserve(1)
		if operationErr == nil {
			operationErr = producer.Commit(writeSpan, 1)
		}
	})
	if operationErr != nil {
		t.Fatal(operationErr)
	}
	if allocs != 0 {
		t.Fatalf("Reserve/Commit allocations: %g", allocs)
	}
	var ok bool
	allocs = testing.AllocsPerRun(1000, func() {
		_, ok, operationErr = consumer.Peek()
		if operationErr == nil && ok {
			operationErr = consumer.Pop()
		}
	})
	if operationErr != nil || !ok {
		t.Fatalf("Peek/Pop: %v %v", ok, operationErr)
	}
	if allocs != 0 {
		t.Fatalf("Peek/Pop allocations: %g", allocs)
	}
	payload := []byte{1}
	allocs = testing.AllocsPerRun(1000, func() {
		operationErr = producer.Write(payload)
		if operationErr == nil {
			_, ok, operationErr = consumer.Peek()
		}
		if operationErr == nil && ok {
			operationErr = consumer.Pop()
		}
	})
	if operationErr != nil || !ok {
		t.Fatalf("Write/Peek/Pop: %v %v", ok, operationErr)
	}
	if allocs != 0 {
		t.Fatalf("Write allocations: %g", allocs)
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
