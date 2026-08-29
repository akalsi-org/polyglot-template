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
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/akalsi-org/polyglot-template/go/lib/internal/orderedatomic"
)

const journalObserverChildEnv = "PGT_JOURNAL_OBSERVER_CHILD"
const journalObserverArchiveEnv = "PGT_JOURNAL_OBSERVER_ARCHIVE"

func TestObserverChild(t *testing.T) {
	mode := os.Getenv(journalObserverChildEnv)
	if mode == "" {
		return
	}
	journal, err := Attach(3)
	if err != nil {
		t.Fatal(err)
	}
	observer, err := journal.OpenObserver(ObserverConfig{
		ArchiveDirectory: os.Getenv(journalObserverArchiveEnv),
		Start:            ObserverStart{Batch: batchID(1)},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer observer.Close()
	switch mode {
	case "replay":
		buffer := make([]byte, 64)
		for {
			record, ok, err := observer.ReplayNext(buffer)
			if err != nil {
				t.Fatal(err)
			}
			if !ok {
				break
			}
			fmt.Printf("replay %d %s\n", record.Sequence, string(record.Bytes))
		}
	case "live":
		buffer := make([]byte, 64)
		for {
			_, ok, err := observer.ReplayNext(buffer)
			if err != nil {
				t.Fatal(err)
			}
			if !ok {
				break
			}
		}
		fmt.Println("ready")
		if _, err := bufio.NewReader(os.Stdin).ReadByte(); err != nil && !errors.Is(err, io.EOF) {
			t.Fatal(err)
		}
		for {
			view, ok, err := observer.NextLive()
			if err != nil {
				t.Fatal(err)
			}
			if !ok {
				break
			}
			payload := append([]byte(nil), view.UnsafeBytes()...)
			if err := view.Validate(); err != nil {
				t.Fatal(err)
			}
			fmt.Printf("live %d %s\n", view.Sequence(), payload)
			if err := observer.Advance(view); err != nil {
				t.Fatal(err)
			}
		}
	default:
		t.Fatalf("unknown child mode %q", mode)
	}
}

func batchID(sequence Sequence) *BatchID {
	id := BatchID(sequence)
	return &id
}

func TestObserverReplaysArchivedRecords(t *testing.T) {
	journal := newTestJournal(t, 1<<16, 4096)
	producer := newTestProducer(t, journal)
	directory := t.TempDir()
	archiver := newTestArchiver(t, journal, directory)
	payloads := [][]byte{[]byte("one"), []byte("two"), []byte("three")}
	for _, payload := range payloads {
		if _, err := producer.Write(payload); err != nil {
			t.Fatal(err)
		}
	}
	progress := drainTestBatch(t, archiver, time.Unix(1, 0))
	observer, err := journal.OpenObserver(ObserverConfig{ArchiveDirectory: directory, Start: ObserverStart{Batch: &progress.Batch}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = observer.Close() })
	buffer := make([]byte, 8)
	for index, payload := range payloads {
		record, ok, err := observer.ReplayNext(buffer)
		if err != nil || !ok {
			t.Fatalf("record %d = (%v, %v, %v)", index, record, ok, err)
		}
		if record.Sequence != Sequence(index+1) || !bytes.Equal(record.Bytes, payload) {
			t.Fatalf("record %+v want seq %d payload %q", record, index+1, payload)
		}
	}
	if _, ok, err := observer.ReplayNext(buffer); err != nil || ok {
		t.Fatalf("extra replay = (%v, %v)", ok, err)
	}
	if observer.State() != ObserverHandoff && observer.State() != ObserverLive {
		t.Fatalf("state = %v", observer.State())
	}
}

func TestObserverReplayBufferTooSmall(t *testing.T) {
	journal := newTestJournal(t, 1<<16, 4096)
	producer := newTestProducer(t, journal)
	directory := t.TempDir()
	archiver := newTestArchiver(t, journal, directory)
	if _, err := producer.Write([]byte("abcdef")); err != nil {
		t.Fatal(err)
	}
	progress := drainTestBatch(t, archiver, time.Unix(1, 0))
	observer, err := journal.OpenObserver(ObserverConfig{ArchiveDirectory: directory, Start: ObserverStart{Batch: &progress.Batch}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = observer.Close() })
	_, _, err = observer.ReplayNext(make([]byte, 2))
	var tooSmall *BufferTooSmallError
	if !errors.As(err, &tooSmall) || tooSmall.Required != 6 {
		t.Fatalf("ReplayNext = %v", err)
	}
	record, ok, err := observer.ReplayNext(make([]byte, 6))
	if err != nil || !ok || string(record.Bytes) != "abcdef" {
		t.Fatalf("retry = (%+v, %v, %v)", record, ok, err)
	}
}

func TestObserverLiveAfterHandoff(t *testing.T) {
	journal := newTestJournal(t, 1<<16, 4096)
	producer := newTestProducer(t, journal)
	directory := t.TempDir()
	archiver := newTestArchiver(t, journal, directory)
	if _, err := producer.Write([]byte("old")); err != nil {
		t.Fatal(err)
	}
	progress := drainTestBatch(t, archiver, time.Unix(1, 0))
	if _, err := producer.Write([]byte("live")); err != nil {
		t.Fatal(err)
	}
	observer, err := journal.OpenObserver(ObserverConfig{ArchiveDirectory: directory, Start: ObserverStart{Batch: &progress.Batch}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = observer.Close() })
	buffer := make([]byte, 8)
	if _, ok, err := observer.ReplayNext(buffer); err != nil || !ok {
		t.Fatalf("replay = (%v, %v)", ok, err)
	}
	if _, ok, err := observer.ReplayNext(buffer); err != nil || ok {
		t.Fatalf("replay drain = (%v, %v)", ok, err)
	}
	view, ok, err := observer.NextLive()
	if err != nil || !ok || string(view.UnsafeBytes()) != "live" {
		t.Fatalf("NextLive = (%+v, %v, %v)", view, ok, err)
	}
	if err := view.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := observer.Advance(view); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := observer.NextLive(); err != nil || ok {
		t.Fatalf("empty live = (%v, %v)", ok, err)
	}
}

func TestObserverDetectsLiveOverwrite(t *testing.T) {
	journal := newTestJournal(t, 1<<16, 4096)
	producer := newTestProducer(t, journal)
	directory := t.TempDir()
	observer, err := journal.OpenObserver(ObserverConfig{ArchiveDirectory: directory})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = observer.Close() })
	if _, err := producer.Write([]byte("live")); err != nil {
		t.Fatal(err)
	}
	view, ok, err := observer.NextLive()
	if err != nil || !ok {
		t.Fatalf("NextLive = (%v, %v)", ok, err)
	}
	orderedatomic.StoreRelaxed64(journal.region.tag(view.position), WritingTag(view.Sequence()))
	if err := view.Validate(); !errors.Is(err, ErrOverwritten) {
		t.Fatalf("Validate = %v", err)
	}
	if observer.State() != ObserverResyncRequired {
		t.Fatalf("state = %v", observer.State())
	}
}

func TestObserverCrossProcessReplayAndLive(t *testing.T) {
	journal := newTestJournal(t, 1<<16, 4096)
	producer := newTestProducer(t, journal)
	directory := t.TempDir()
	archiver := newTestArchiver(t, journal, directory)
	if _, err := producer.Write([]byte("alpha")); err != nil {
		t.Fatal(err)
	}
	_ = drainTestBatch(t, archiver, time.Unix(1, 0))
	replayOut := runObserverChild(t, journal, directory, "replay", false)
	if !strings.Contains(replayOut, "replay 1 alpha") {
		t.Fatalf("replay child output %q", replayOut)
	}
	if _, err := producer.Write([]byte("beta")); err != nil {
		t.Fatal(err)
	}
	liveOut := runObserverChild(t, journal, directory, "live", true)
	if !strings.Contains(liveOut, "live 2 beta") {
		t.Fatalf("live child output %q", liveOut)
	}
}

func runObserverChild(t *testing.T, journal *Journal, directory, mode string, signal bool) string {
	t.Helper()
	fd, err := journal.DupFD()
	if err != nil {
		t.Fatal(err)
	}
	file := os.NewFile(uintptr(fd), "journal-observer-region")
	command := exec.Command(os.Args[0], "-test.run=^TestObserverChild$")
	command.Env = append(os.Environ(), journalObserverChildEnv+"="+mode, journalObserverArchiveEnv+"="+directory)
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
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		stdin.Close()
		file.Close()
		t.Fatal(err)
	}
	file.Close()
	reader := bufio.NewReader(stdout)
	var output strings.Builder
	if signal {
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				_ = command.Process.Kill()
				_ = command.Wait()
				t.Fatalf("wait ready: %v stderr %s", err, stderr.String())
			}
			output.WriteString(line)
			if strings.TrimSpace(line) == "ready" {
				break
			}
		}
		if _, err := stdin.Write([]byte{'\n'}); err != nil {
			t.Fatal(err)
		}
	}
	_ = stdin.Close()
	rest, _ := io.ReadAll(reader)
	output.Write(rest)
	if err := command.Wait(); err != nil {
		t.Fatalf("child: %v stderr %s stdout %s", err, stderr.String(), output.String())
	}
	return output.String()
}

func TestObserverNilStartRequiresEmptyArchive(t *testing.T) {
	journal := newTestJournal(t, 1<<16, 4096)
	producer := newTestProducer(t, journal)
	directory := t.TempDir()
	archiver := newTestArchiver(t, journal, directory)
	if _, err := producer.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	_ = drainTestBatch(t, archiver, time.Unix(1, 0))
	_, err := journal.OpenObserver(ObserverConfig{ArchiveDirectory: directory})
	if !errors.Is(err, ErrBatchBoundary) {
		t.Fatalf("OpenObserver = %v", err)
	}
}

func TestObserverOpenUsesAbsoluteArchivePath(t *testing.T) {
	journal := newTestJournal(t, 1<<16, 4096)
	directory := t.TempDir()
	relative := filepath.Base(directory)
	parent := filepath.Dir(directory)
	working, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(parent); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(working) })
	observer, err := journal.OpenObserver(ObserverConfig{ArchiveDirectory: relative})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = observer.Close() })
	if observer.directory != directory && observer.directory != parent+"/"+relative {
		if _, err := os.Stat(observer.directory); err != nil {
			t.Fatalf("directory %q: %v", observer.directory, err)
		}
	}
}
