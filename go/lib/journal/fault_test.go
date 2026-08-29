//go:build linux && (amd64 || arm64)

package journal

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/akalsi-org/polyglot-template/go/lib/hostcpu"
	"github.com/akalsi-org/polyglot-template/go/lib/internal/orderedatomic"
)

const journalFaultChildEnv = "PGT_JOURNAL_FAULT_CHILD"

func TestJournalProducerFaultChild(t *testing.T) {
	mode := os.Getenv(journalFaultChildEnv)
	if mode == "" {
		return
	}
	journal, err := Attach(3)
	if err != nil {
		t.Fatal(err)
	}
	producer, err := journal.AttachProducer()
	if err != nil {
		t.Fatal(err)
	}
	switch mode {
	case "owner":
	case "reservation":
		span, err := producer.Reserve(128)
		if err != nil {
			t.Fatal(err)
		}
		copy(span.Bytes(), "reserved")
	case "stable-tail":
		span, err := producer.Reserve(128)
		if err != nil {
			t.Fatal(err)
		}
		copy(span.Bytes(), "stable")
		descriptor, ok := packDescriptor(6, 1, 0)
		if !ok {
			t.Fatal("packDescriptor rejected stable-tail record")
		}
		orderedatomic.StoreRelaxed64(journal.region.descriptor(span.position), descriptor)
		orderedatomic.StoreRelease64(journal.region.tag(span.position), StableTag(span.sequence))
	default:
		t.Fatalf("unknown child mode %q", mode)
	}
	fmt.Println("ready")
	if _, err := bufio.NewReader(os.Stdin).ReadByte(); err != nil {
		os.Exit(0)
	}
}

type journalFaultChild struct {
	command *exec.Cmd
	input   io.WriteCloser
	stderr  bytes.Buffer
}

func startJournalFaultChild(t *testing.T, journal *Journal, mode string) *journalFaultChild {
	t.Helper()
	fd, err := journal.DupFD()
	if err != nil {
		t.Fatal(err)
	}
	file := os.NewFile(uintptr(fd), "journal-fault-region")
	command := exec.Command(os.Args[0], "-test.run=^TestJournalProducerFaultChild$")
	command.Env = append(os.Environ(), journalFaultChildEnv+"="+mode)
	command.ExtraFiles = []*os.File{file}
	stdin, err := command.StdinPipe()
	if err != nil {
		file.Close()
		t.Fatal(err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		stdin.Close()
		file.Close()
		t.Fatal(err)
	}
	child := &journalFaultChild{command: command, input: stdin}
	command.Stderr = &child.stderr
	if err := command.Start(); err != nil {
		stdin.Close()
		file.Close()
		t.Fatal(err)
	}
	file.Close()
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "ready" {
		_ = command.Process.Kill()
		_ = command.Wait()
		t.Fatalf("fault child %q readiness = %q, %v: %s", mode, line, err, child.stderr.String())
	}
	return child
}

func (child *journalFaultChild) kill(t *testing.T) {
	t.Helper()
	if child.command.ProcessState != nil {
		return
	}
	if err := child.command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := child.command.Wait(); err == nil {
		t.Fatal("killed fault child exited successfully")
	}
	_ = child.input.Close()
}

func waitForStoppedProcess(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	path := fmt.Sprintf("/proc/%d/status", pid)
	for {
		status, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(status), "\n") {
			if strings.HasPrefix(line, "State:") && strings.Contains(line, "T (stopped)") {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("process %d did not stop", pid)
		}
		runtime.Gosched()
	}
}

func TestRealDeadProducerReservationRequiresRecovery(t *testing.T) {
	journal := newTestJournal(t, 1<<16, 4096)
	child := startJournalFaultChild(t, journal, "reservation")
	child.kill(t)
	fd, err := journal.DupFD()
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fd)
	attached, err := Attach(fd)
	if err != nil {
		t.Fatal(err)
	}
	defer attached.Close()
	if !attached.NeedsRecovery() {
		t.Fatal("dead reserved producer did not require recovery")
	}
	if producer, err := attached.AttachProducer(); producer != nil || !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("AttachProducer = (%v, %v), want nil ErrRecoveryRequired", producer, err)
	}
	durable, durableOK := attached.region.snapshotDurable()
	publish, publishOK := attached.region.snapshotPublish()
	if !durableOK || !publishOK {
		t.Fatal("dead reservation left an unstable cursor")
	}
	if err := attached.completeCursorRecovery(durable, publish); err != nil {
		t.Fatal(err)
	}
	replacement, err := attached.AttachProducer()
	if err != nil {
		t.Fatal(err)
	}
	if err := replacement.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRealDeadProducerStableTailRequiresRecovery(t *testing.T) {
	journal := newTestJournal(t, 1<<16, 4096)
	child := startJournalFaultChild(t, journal, "stable-tail")
	child.kill(t)
	fd, err := journal.DupFD()
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fd)
	attached, err := Attach(fd)
	if err != nil {
		t.Fatal(err)
	}
	defer attached.Close()
	if !attached.NeedsRecovery() {
		t.Fatal("dead producer with a stable tail did not require recovery")
	}
	publish, ok := attached.region.snapshotPublish()
	if !ok || publish != (ringCursor{sequence: 1}) {
		t.Fatalf("stored publish = %+v, ok=%v", publish, ok)
	}
	durable, ok := attached.region.snapshotDurable()
	if !ok {
		t.Fatal("durable cursor is unstable")
	}
	derived, err := attached.recoverPublishCursor(durable)
	if err != nil {
		t.Fatal(err)
	}
	if derived != (ringCursor{sequence: 2, position: uint64(JournalGrain)}) {
		t.Fatalf("derived publish = %+v", derived)
	}
	if !attached.NeedsRecovery() {
		t.Fatal("tail scan cleared recovery without archive reconciliation")
	}
}

func TestStoppedLiveProducerDoesNotRequireRecovery(t *testing.T) {
	journal := newTestJournal(t, 1<<16, 4096)
	child := startJournalFaultChild(t, journal, "owner")
	defer child.kill(t)
	owner := orderedatomic.LoadAcquire64(journal.region.ptr64(JournalHeaderProducerTIDOffset))
	tid := hostcpu.ThreadId(uint32(owner))
	if tid == 0 {
		t.Fatal("child did not publish a producer owner")
	}
	if err := child.command.Process.Signal(syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	waitForStoppedProcess(t, child.command.Process.Pid)
	if !hostcpu.ThreadAlive(tid) {
		t.Fatalf("stopped producer thread %d appears dead", tid)
	}
	fd, err := journal.DupFD()
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fd)
	attached, err := Attach(fd)
	if err != nil {
		t.Fatal(err)
	}
	defer attached.Close()
	if attached.NeedsRecovery() {
		t.Fatal("stopped live producer required recovery")
	}
	if producer, err := attached.AttachProducer(); producer != nil || !errors.Is(err, ErrBusy) {
		t.Fatalf("AttachProducer = (%v, %v), want nil ErrBusy", producer, err)
	}
}
