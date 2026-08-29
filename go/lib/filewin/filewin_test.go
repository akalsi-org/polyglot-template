//go:build linux && (amd64 || arm64)

package filewin

import (
	"bytes"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/akalsi-org/polyglot-template/go/lib/atmc"
)

func testConfig(path string) Config {
	return Config{
		Path:       path,
		Reserve:    1 << 30,
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
	committed := atmc.LoadAcquireU64(file.committed())
	if committed < wrote {
		t.Fatalf("committed %d < data end %d", committed, wrote)
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

func TestHeaderAndDataMapsAreSeparate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log")
	file, err := Create(testConfig(path))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	if len(file.header) != int(headerSize) {
		t.Fatalf("header map %d", len(file.header))
	}
	if uint64(len(file.data)) != file.reserve {
		t.Fatalf("data map %d want reserve %d", len(file.data), file.reserve)
	}
	headInfo, err := os.Stat(path + headSuffix)
	if err != nil {
		t.Fatal(err)
	}
	if headInfo.Size() != int64(headerSize) {
		t.Fatalf("head file size %d want %d", headInfo.Size(), headerSize)
	}
	dataInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if dataInfo.Size()%int64(file.page) != 0 {
		t.Fatalf("data file size %d is not page aligned", dataInfo.Size())
	}
	if uint64(dataInfo.Size()) >= file.reserve {
		t.Fatalf("file %d should start far below its reservation %d", dataInfo.Size(), file.reserve)
	}
}

func TestGrowthNeedsNoRemap(t *testing.T) {
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
	reader, err := file.AttachReader()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })

	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	mapped := &file.data[0]
	payload := bytes.Repeat([]byte("z"), 4096)
	var wrote uint64
	for i := 0; i < 1024; i++ {
		if err := writer.Write(payload); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
		wrote += uint64(len(payload))
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() <= before.Size() {
		t.Fatalf("file did not grow: %d -> %d", before.Size(), after.Size())
	}
	if mapped != &file.data[0] {
		t.Fatal("mapping moved during growth")
	}
	var got uint64
	for got < wrote {
		chunk, err := reader.Peek(1 << 16)
		if err != nil {
			t.Fatal(err)
		}
		if len(chunk) == 0 {
			t.Fatal("peek empty before end")
		}
		got += uint64(len(chunk))
		if err := reader.Advance(uint64(len(chunk))); err != nil {
			t.Fatal(err)
		}
	}
}

func TestHeadInSharedMemoryIsSymlinked(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log")
	cfg := testConfig(path)
	cfg.HeadInSharedMemory = true
	file, err := Create(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		target, _ := os.Readlink(path + headSuffix)
		_ = file.Close()
		if target != "" {
			_ = os.Remove(target)
		}
	})
	target, err := os.Readlink(path + headSuffix)
	if err != nil {
		t.Fatalf("head is not a symlink: %v", err)
	}
	if filepath.Dir(target) != sharedMemoryDir {
		t.Fatalf("head target %q is not under %s", target, sharedMemoryDir)
	}
	info, err := os.Stat(target)
	if err != nil || info.Size() != int64(headerSize) {
		t.Fatalf("head target stat = %v, %v", info, err)
	}
	writer, err := file.AttachWriter()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = writer.Close() })
	if err := writer.Write([]byte("shm-head")); err != nil {
		t.Fatal(err)
	}
	reader, err := file.AttachReader()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	got, err := reader.Peek(32)
	if err != nil || string(got) != "shm-head" {
		t.Fatalf("Peek = %q, %v", got, err)
	}
}

func TestCursorsSitOnSeparateCacheLines(t *testing.T) {
	lines := map[int]string{}
	for name, off := range map[string]int{
		"writePos":   offWritePos,
		"writeGen":   offWriteGen,
		"committed":  offCommitted,
		"syncedPos":  offSyncedPos,
		"evictedPos": offEvictedPos,
	} {
		owner := map[string]string{
			"writePos": "writer", "writeGen": "writer",
			"committed": "helper", "syncedPos": "helper", "evictedPos": "helper",
		}[name]
		line := off / cacheLine
		if prev, ok := lines[line]; ok && prev != owner {
			t.Fatalf("%s shares cache line %d with %s", name, line, prev)
		}
		lines[line] = owner
	}
	if offWritePos/cacheLine == 0 {
		t.Fatal("writer cursor must not share the identity line")
	}
}

// A reader must leave no trace in shared memory. The writer scales to any
// number of readers only because it never learns that one attached.
func TestReadersWriteNothingShared(t *testing.T) {
	dir := t.TempDir()
	file, err := Create(testConfig(filepath.Join(dir, "log")))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	writer, err := file.AttachWriter()
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if err := writer.Write(bytes.Repeat([]byte{7}, 4096)); err != nil {
		t.Fatal(err)
	}

	// Only the writer and its helpers own header bytes: the identity line,
	// the writer line, and the helper line. Everything else must stay zero
	// however many readers come and go.
	tail := func() []byte {
		return append(append([]byte(nil), file.header[offReserved2:offReserved2+cacheLine]...),
			file.header[4*cacheLine:]...)
	}
	before := tail()
	readers := make([]*Reader, 0, 64)
	for i := 0; i < 64; i++ {
		r, err := file.AttachReader()
		if err != nil {
			t.Fatalf("reader %d did not attach: %v", i, err)
		}
		readers = append(readers, r)
	}
	for _, r := range readers {
		if err := r.Seek(0); err != nil {
			t.Fatal(err)
		}
		span, err := r.Peek(0)
		if err != nil {
			t.Fatal(err)
		}
		if err := r.Advance(uint64(len(span))); err != nil {
			t.Fatal(err)
		}
	}
	if !bytes.Equal(before, tail()) {
		t.Fatal("attaching and reading changed shared header bytes")
	}
	for _, r := range readers {
		if err := r.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if !bytes.Equal(before, tail()) {
		t.Fatal("closing readers changed shared header bytes")
	}
	for i, b := range before {
		if b != 0 {
			t.Fatalf("reader-visible header byte %d is not zero: %d", i, b)
		}
	}
}

// The writer retains a fixed window and nothing else can hold it, so memory
// stays bounded no matter how far behind a reader falls.
func TestRetentionFollowsTheWriterAlone(t *testing.T) {
	dir := t.TempDir()
	const retain = 1 << 20
	cfg := testConfig(filepath.Join(dir, "log"))
	cfg.RetainBytes = retain
	file, err := Create(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	writer, err := file.AttachWriter()
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	stalled, err := file.AttachReader()
	if err != nil {
		t.Fatal(err)
	}
	defer stalled.Close()

	payload := bytes.Repeat([]byte{3}, 4096)
	for i := 0; i < 8*retain/len(payload); i++ {
		if err := writer.Write(payload); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if writer.Dontneed.Load() > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if writer.Dontneed.Load() == 0 {
		t.Fatal("the writer never released history past the retention window")
	}
	// The stalled reader lost no data. Released pages are a residency
	// decision, and the bytes are still in the file.
	if err := stalled.Seek(0); err != nil {
		t.Fatal(err)
	}
	span, err := stalled.Peek(uint64(len(payload)))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(span, payload) {
		t.Fatal("history released from memory did not read back correctly")
	}
}

// A reader must never read past the write cursor. The cursor never exceeds
// the file's published length, so this is also what keeps a reader off the
// pages beyond the end of the file, where a touch would raise SIGBUS.
func TestReaderStopsAtTheWriteCursor(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig(filepath.Join(dir, "log"))
	cfg.Reserve = 1 << 30
	file, err := Create(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	reader, err := file.AttachReader()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	if committed := atmc.LoadAcquireU64(file.committed()); committed >= file.reserve {
		t.Fatal("the file already covers the whole reservation")
	}
	// Seeking past the cursor is a misuse, not a way to read the file end.
	if err := reader.Seek(1 << 20); err == nil {
		t.Fatal("Seek past the write cursor was allowed")
	}
	if span, err := reader.Peek(0); err != nil || len(span) != 0 {
		t.Fatalf("read past the write cursor: %d bytes, %v", len(span), err)
	}
	if err := reader.Wait(); err != nil {
		t.Fatal(err)
	}
}

// The writer must never believe the file is longer than it is. Publishing a
// length whose blocks do not exist makes the next store run past the end of
// the file, and a store past the end of a mapping raises SIGBUS, which kills
// the process. A staged allocation has not happened yet, so only a completed
// one may move the length.
func TestCommittedLengthNeverExceedsTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log")
	cfg := testConfig(path)
	cfg.Extent = 1 << 20
	file, err := Create(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	writer, err := file.AttachWriter()
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()

	payload := bytes.Repeat([]byte{6}, 4096)
	for i := 0; i < 4096; i++ {
		if err := writer.Write(payload); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
		if i%64 != 0 {
			continue
		}
		var st syscall.Stat_t
		if err := syscall.Stat(path, &st); err != nil {
			t.Fatal(err)
		}
		committed := atmc.LoadAcquireU64(file.committed())
		if committed > uint64(st.Size) {
			t.Fatalf("committed %d exceeds file size %d after %d writes",
				committed, st.Size, i)
		}
		if writer.Pos() > uint64(st.Size) {
			t.Fatalf("write cursor %d is past the end of the file %d",
				writer.Pos(), st.Size)
		}
	}
	if got := writer.GrowErrors.Load(); got != 0 {
		t.Fatalf("%d growth failures on a healthy filesystem", got)
	}
}
