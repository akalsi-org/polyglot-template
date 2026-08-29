//go:build linux && (amd64 || arm64)

package mpsc

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/akalsi-org/polyglot-template/go/lib/hostcpu"
)

func TestMPSCFaultChild(t *testing.T) {
	mode := os.Getenv("PGT_MPSC_FAULT_CHILD")
	if mode == "" {
		return
	}
	q, err := AttachMPSC(3)
	if err != nil {
		t.Fatal(err)
	}
	if mode == "peek" {
		consumer, err := q.AttachConsumer()
		if err != nil {
			t.Fatal(err)
		}
		record, ok, err := consumer.Peek()
		if err != nil || !ok || string(record.UnsafeBytes()) != "survives" {
			t.Fatalf("peek: %v %v", ok, err)
		}
		fmt.Println("ready")
		select {}
	}
	producer, err := q.NewProducer()
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := producer.Reserve(8)
	if err != nil {
		t.Fatal(err)
	}
	copy(reservation.Bytes(), "survives")
	if mode == "commit" {
		if err := producer.Commit(reservation, 8); err != nil {
			t.Fatal(err)
		}
	}
	fmt.Println("ready")
	select {}
}

func startFaultChild(t *testing.T, q *MPSC, mode string) *exec.Cmd {
	fd, err := q.DupFD()
	if err != nil {
		t.Fatal(err)
	}
	file := os.NewFile(uintptr(fd), "mpsc-region")
	cmd := exec.Command(os.Args[0], "-test.run=TestMPSCFaultChild")
	cmd.ExtraFiles = []*os.File{file}
	cmd.Env = append(os.Environ(), "PGT_MPSC_FAULT_CHILD="+mode)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	file.Close()
	scanner := bufio.NewScanner(stdout)
	if !scanner.Scan() || scanner.Text() != "ready" {
		t.Fatalf("child did not become ready: %q", scanner.Text())
	}
	return cmd
}

func TestKilledWriterProcessRecovery(t *testing.T) {
	q, err := CreateMPSC(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	consumer, err := q.AttachConsumer()
	if err != nil {
		t.Fatal(err)
	}
	cmd := startFaultChild(t, q, "reserve")
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	for i := 0; i < 140; i++ {
		record, ok, err := consumer.Peek()
		if err != nil || ok || record.UnsafeBytes() != nil {
			t.Fatalf("peek %d: %v %v", i, ok, err)
		}
	}
	if err := consumer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCommittedThenKilledProcessIsDelivered(t *testing.T) {
	q, err := CreateMPSC(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	consumer, err := q.AttachConsumer()
	if err != nil {
		t.Fatal(err)
	}
	cmd := startFaultChild(t, q, "commit")
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	record, ok, err := consumer.Peek()
	if err != nil || !ok || string(record.UnsafeBytes()) != "survives" {
		t.Fatalf("peek: %v %v", ok, err)
	}
	if err := consumer.Pop(); err != nil {
		t.Fatal(err)
	}
	if err := consumer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestStoppedWriterProcessIsNotRecovered(t *testing.T) {
	q, err := CreateMPSC(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	consumer, err := q.AttachConsumer()
	if err != nil {
		t.Fatal(err)
	}
	cmd := startFaultChild(t, q, "reserve")
	if err := cmd.Process.Signal(syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 260; i++ {
		record, ok, err := consumer.Peek()
		if err != nil || ok || record.UnsafeBytes() != nil {
			t.Fatalf("peek %d: %v %v", i, ok, err)
		}
	}
	if err := cmd.Process.Signal(syscall.SIGCONT); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	for i := 0; i < 140; i++ {
		_, _, _ = consumer.Peek()
	}
	if err := consumer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
}

func deadTid(t *testing.T) uint32 {
	cmd := exec.Command("/bin/true")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	tid := uint32(cmd.Process.Pid)
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if hostcpu.ThreadAlive(hostcpu.ThreadId(tid)) {
		t.Fatalf("reaped Tid %d is alive", tid)
	}
	return tid
}

func TestDeadWriterRecovery(t *testing.T) {
	q, err := CreateMPSC(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	consumer, err := q.AttachConsumer()
	if err != nil {
		t.Fatal(err)
	}
	word := packrecord(64, stateCleared, deadTid(t))
	atomic.StoreUint64(q.claim(0), word)
	atomic.StoreUint64(q.claim(64), freeWord(64))
	for i := 0; i < 130; i++ {
		if record, ok, err := consumer.Peek(); err != nil || ok || record.UnsafeBytes() != nil {
			t.Fatalf("peek %d: %v %v", i, ok, err)
		}
	}
	if got := atomic.LoadUint64(q.ptr64(controlSize + 128)); got != 64 {
		t.Fatalf("read position: %d", got)
	}
	if err := consumer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCommittedThenDeadIsDelivered(t *testing.T) {
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
	q.payload(0, 1)[0] = 7
	atomic.StoreUint64(length, 1)
	atomic.StoreUint64(tag, commitTag(0))
	record, ok, err := consumer.Peek()
	if err != nil || !ok || record.UnsafeBytes()[0] != 7 {
		t.Fatalf("peek: %v %v", ok, err)
	}
	if err := consumer.Pop(); err != nil {
		t.Fatal(err)
	}
	if err := consumer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestStoppedLiveWriterIsNotRecovered(t *testing.T) {
	q, err := CreateMPSC(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	consumer, err := q.AttachConsumer()
	if err != nil {
		t.Fatal(err)
	}
	word := packrecord(64, stateCleared, uint32(hostcpu.CurrentThreadId()))
	atomic.StoreUint64(q.claim(0), word)
	for i := 0; i < 260; i++ {
		if record, ok, err := consumer.Peek(); err != nil || ok || record.UnsafeBytes() != nil {
			t.Fatalf("peek %d: %v %v", i, ok, err)
		}
	}
	if got := atomic.LoadUint64(q.claim(0)); got != word {
		t.Fatalf("live claim changed: %#x", got)
	}
	atomic.StoreUint64(q.claim(0), withState(word, stateAborted))
	atomic.StoreUint64(q.claim(64), freeWord(64))
	if _, _, err := consumer.Peek(); err != nil {
		t.Fatal(err)
	}
	if err := consumer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPadded256Wrap(t *testing.T) {
	q, err := CreateMPSC(Config{Capacity: 4096, MPSCLayout: MPSCPadded256})
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
	for i := 0; i < 40; i++ {
		payload := make([]byte, 200)
		payload[0] = byte(i)
		if err := producer.Write(payload); err != nil {
			t.Fatal(err)
		}
		record, ok, err := consumer.Peek()
		if err != nil || !ok || record.UnsafeBytes()[0] != byte(i) {
			t.Fatalf("record %d: %v %v", i, ok, err)
		}
		if err := consumer.Pop(); err != nil {
			t.Fatal(err)
		}
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

func TestCloseExcludesNewHandles(t *testing.T) {
	q, err := CreateMPSC(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	producer, err := q.NewProducer()
	if err != nil {
		t.Fatal(err)
	}
	if err := q.Close(); !errors.Is(err, ErrBusy) {
		t.Fatalf("close with producer: %v", err)
	}
	if err := producer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := q.NewProducer(); !errors.Is(err, ErrClosed) {
		t.Fatalf("new producer after close: %v", err)
	}
}
