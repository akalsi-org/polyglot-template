//go:build linux && (amd64 || arm64)

package filewin

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/akalsi-org/polyglot-template/go/lib/internal/orderedatomic"
)

func testConfig(path string) Config {
	return Config{
		Path:       path,
		Window:     1 << 20,
		Extent:     1 << 20,
		Ahead:      64 << 10,
		MaxReserve: 4096,
	}
}

func TestWriterGrowsLinearlyAndReaderSeesBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log")
	file, err := Create(testConfig(path))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	writer, err := file.AttachWriter()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = writer.Close() })
	payload := bytes.Repeat([]byte("abcd"), 1024)
	var wrote uint64
	for i := 0; i < 400; i++ {
		if err := writer.Write(payload); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
		wrote += uint64(len(payload))
	}
	committed := orderedatomic.LoadAcquire64(file.committed())
	if committed < headerSize+wrote {
		t.Fatalf("committed %d < data end %d", committed, headerSize+wrote)
	}
	if committed < headerSize+2*file.extent {
		t.Fatalf("expected linear extent growth, committed=%d extent=%d", committed, file.extent)
	}
	reader, err := file.AttachReader()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	got := make([]byte, 0, wrote)
	for uint64(len(got)) < wrote {
		chunk, err := reader.Peek(1 << 16)
		if err != nil {
			t.Fatal(err)
		}
		if len(chunk) == 0 {
			t.Fatal("peek empty before end")
		}
		got = append(got, chunk...)
		if err := reader.Advance(uint64(len(chunk))); err != nil {
			t.Fatal(err)
		}
	}
	if !bytes.Equal(got[:len(payload)], payload) || !bytes.Equal(got[len(got)-len(payload):], payload) {
		t.Fatal("payload mismatch")
	}
}

func TestOpenAttachesExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log")
	file, err := Create(testConfig(path))
	if err != nil {
		t.Fatal(err)
	}
	writer, err := file.AttachWriter()
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Write([]byte("hello-window")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	reopen, err := Open(path, testConfig(path))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopen.Close() })
	reader, err := reopen.AttachReader()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	got, err := reader.Peek(32)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello-window" {
		t.Fatalf("got %q", got)
	}
}
