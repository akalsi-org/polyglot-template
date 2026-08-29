//go:build linux && (amd64 || arm64)

package journal

import (
	"bytes"
	"context"
	"errors"
	"hash/crc32"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/akalsi-org/polyglot-template/go/lib/internal/orderedatomic"
)

func newTestArchiver(t *testing.T, journal *Journal, directory string) *Archiver {
	t.Helper()
	archiver, err := journal.NewArchiver(ArchiveConfig{
		Directory: directory,
		Sizing:    journalTestSizing(journal.region.maxRecord),
	})
	if err != nil {
		t.Fatalf("NewArchiver: %v", err)
	}
	t.Cleanup(func() {
		if err := archiver.Close(); err != nil {
			t.Errorf("Archiver.Close: %v", err)
		}
	})
	return archiver
}

func drainTestBatch(t *testing.T, archiver *Archiver, now time.Time) ArchiveProgress {
	t.Helper()
	progress, committed, err := archiver.DrainOnce(now)
	if err != nil {
		t.Fatalf("first DrainOnce: %v", err)
	}
	if committed {
		return progress
	}
	progress, committed, err = archiver.DrainOnce(now.Add(archiver.config.Sizing.BatchMaxAge))
	if err != nil || !committed {
		t.Fatalf("age DrainOnce = (%+v, %v, %v)", progress, committed, err)
	}
	return progress
}

func TestBatchFilenameRoundTrip(t *testing.T) {
	for _, sequence := range []Sequence{1, 2, 42, JournalMaxSequence} {
		name, err := batchFilename(sequence)
		if err != nil {
			t.Fatal(err)
		}
		if len(name) != len("batch-")+20+len(".jrn.zst") {
			t.Fatalf("filename length = %d", len(name))
		}
		got, ok := parseBatchFilename(name)
		if !ok || got != sequence {
			t.Fatalf("parseBatchFilename(%q) = (%d, %v)", name, got, ok)
		}
	}
	for _, name := range []string{"batch-1.jrn.zst", "batch-00000000000000000000.jrn.zst", "batch-00000000000000000001.tmp", "other"} {
		if _, ok := parseBatchFilename(name); ok {
			t.Fatalf("parseBatchFilename(%q) succeeded", name)
		}
	}
}

func TestArchiveBatchRoundTripAndChecksums(t *testing.T) {
	journal := newTestJournal(t, 1<<16, 4096)
	producer := newTestProducer(t, journal)
	payloads := [][]byte{[]byte("alpha"), bytes.Repeat([]byte("beta"), 40), nil}
	for _, payload := range payloads {
		if _, err := producer.Write(payload); err != nil {
			t.Fatal(err)
		}
	}
	durable, ok := journal.region.snapshotDurable()
	if !ok {
		t.Fatal("durable snapshot failed")
	}
	publish, ok := journal.region.snapshotPublish()
	if !ok {
		t.Fatal("publish snapshot failed")
	}
	proof, err := journal.region.beginDurableProof(durable, publish)
	if err != nil {
		t.Fatal(err)
	}
	var records []ringRecord
	for proof.to != publish {
		record, present, err := journal.region.recordAt(proof.to, publish)
		if err != nil || !present {
			t.Fatalf("recordAt = (%v, %v)", present, err)
		}
		if err := proof.include(record); err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	blockBytes := uint64(BatchRecordHeaderSize) + 164
	file, want, err := buildArchiveBatch(journal.region.ringEpoch(), proof, records, blockBytes, 1, time.Unix(0, 123456789))
	if err != nil {
		t.Fatal(err)
	}
	maximumFileBytes, err := MaximumArchiveBatchFileBytes(journalTestSizing(journal.region.maxRecord))
	if err != nil {
		t.Fatal(err)
	}
	if uint64(len(file)) > maximumFileBytes {
		t.Fatalf("batch file bytes = %d, sizing bound %d", len(file), maximumFileBytes)
	}
	got, err := parseArchiveBatch(file, want.filename, want.epoch)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("batch metadata differs\ngot  %+v\nwant %+v", got, want)
	}
	if got.blockCount != 3 {
		t.Fatalf("block count = %d, want three independent blocks", got.blockCount)
	}
	firstBlock := int(BatchHeaderSize + BatchBlockHeaderSize)
	if len(file) < firstBlock+4 || !bytes.Equal(file[firstBlock:firstBlock+4], []byte{0x28, 0xb5, 0x2f, 0xfd}) {
		t.Fatalf("first block is not a Zstandard frame: %x", file[firstBlock:firstBlock+4])
	}
	footer := file[len(file)-int(BatchFooterSize):]
	if get32(file, BatchHeaderCRC32COffset) != crc32c(file[:BatchHeaderSize], BatchHeaderCRC32COffset) ||
		get32(footer, BatchFooterCRC32COffset) != crc32c(footer, BatchFooterCRC32COffset) {
		t.Fatal("batch fixed-object CRC32C differs")
	}
	corruptions := []struct {
		name   string
		offset int
	}{
		{name: "header", offset: int(BatchHeaderReservedOffset)},
		{name: "block header", offset: int(BatchHeaderSize + BatchBlockFlagsOffset)},
		{name: "compressed payload", offset: firstBlock + 4},
		{name: "index", offset: int(want.indexOffset + uint64(BatchIndexReservedOffset))},
		{name: "footer", offset: len(file) - int(BatchFooterSize) + int(BatchFooterReservedOffset)},
	}
	for _, corruption := range corruptions {
		t.Run(corruption.name, func(t *testing.T) {
			broken := append([]byte(nil), file...)
			broken[corruption.offset] ^= 1
			if _, err := parseArchiveBatch(broken, want.filename, want.epoch); err == nil {
				t.Fatal("corrupt batch succeeded")
			}
		})
	}
}

func TestArchiveHeadAndRetentionFloorValidation(t *testing.T) {
	epoch := RingEpoch{1, 2, 3}
	name, _ := batchFilename(7)
	head := archiveHead{
		epoch:       epoch,
		latest:      7,
		next:        ringCursor{sequence: 9, position: 256},
		batchBytes:  8192,
		batchSHA256: [32]byte{9},
		filename:    name,
	}
	encoded, err := encodeArchiveHead(head)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := parseArchiveHead(encoded, epoch)
	if err != nil || decoded != head {
		t.Fatalf("parseArchiveHead = (%+v, %v)", decoded, err)
	}
	encoded[ArchiveHeadReservedOffset] = 1
	if _, err := parseArchiveHead(encoded, epoch); err == nil {
		t.Fatal("nonzero archive head reserved byte succeeded")
	}
	floor := make([]byte, RetentionFloorSize)
	put64(floor, RetentionFloorMagicOffset, RetentionFloorMagic)
	put32(floor, RetentionFloorVersionOffset, RetentionFloorFormatVersion)
	put32(floor, RetentionFloorSizeOffset, RetentionFloorSize)
	put32(floor, RetentionFloorEndianOffset, JournalEndianMarker)
	copy(floor[RetentionFloorRingEpochOffset:], epoch[:])
	put64(floor, RetentionFloorOldestBatchIDOffset, 7)
	putCRC32C(floor, RetentionFloorCRC32COffset)
	if got, err := parseRetentionFloor(floor, epoch); err != nil || got != 7 {
		t.Fatalf("parseRetentionFloor = (%d, %v)", got, err)
	}
	floor[RetentionFloorReservedOffset] = 1
	if _, err := parseRetentionFloor(floor, epoch); err == nil {
		t.Fatal("nonzero retention floor reserved byte succeeded")
	}
}

func TestArchiverPublishesBatchHeadThenDurable(t *testing.T) {
	journal := newTestJournal(t, 1<<16, 4096)
	producer := newTestProducer(t, journal)
	if _, err := producer.Write([]byte("durable payload")); err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	archiver := newTestArchiver(t, journal, directory)
	var events []string
	base := archiver.ops
	ops := base
	batchWritten := false
	ops.openFile = func(path string, flags int, mode os.FileMode) (archiveFile, error) {
		file, err := base.openFile(path, flags, mode)
		if err != nil {
			return nil, err
		}
		if strings.Contains(filepath.Base(path), batchFilenameSuffix) {
			return &faultArchiveFile{
				archiveFile: file,
				writeEvent: func() {
					if !batchWritten {
						batchWritten = true
						events = append(events, "batch-write")
					}
				},
			}, nil
		}
		return file, nil
	}
	ops.writeAll = func(file archiveFile, bytes []byte) error {
		if len(bytes) == int(ArchiveHeadSize) {
			events = append(events, "head-write")
		} else {
			events = append(events, "batch-write")
		}
		return base.writeAll(file, bytes)
	}
	ops.fdatasync = func(file archiveFile) error {
		events = append(events, "batch-fdatasync")
		return base.fdatasync(file)
	}
	ops.renameNoReplace = func(oldPath, newPath string) error {
		events = append(events, "batch-rename")
		return base.renameNoReplace(oldPath, newPath)
	}
	ops.rename = func(oldPath, newPath string) error {
		events = append(events, "head-rename")
		return base.rename(oldPath, newPath)
	}
	ops.fsync = func(file archiveFile) error {
		info, err := file.Stat()
		if err == nil && info.IsDir() {
			events = append(events, "directory-fsync")
		} else {
			events = append(events, "head-fsync")
		}
		return base.fsync(file)
	}
	archiver.ops = ops
	progress := drainTestBatch(t, archiver, time.Unix(0, 1))
	if progress.Batch != 1 || progress.First != 1 || progress.End != 2 || progress.DurableBytes != uint64(JournalGrain) {
		t.Fatalf("DrainOnce progress = %+v", progress)
	}
	wantEvents := []string{"batch-write", "batch-fdatasync", "batch-rename", "directory-fsync", "head-write", "head-fsync", "head-rename", "directory-fsync"}
	if !reflect.DeepEqual(events, wantEvents) {
		t.Fatalf("events = %v, want %v", events, wantEvents)
	}
	durable, ok := journal.region.snapshotDurable()
	if !ok || durable.sequence != 2 || durable.position != uint64(JournalGrain) {
		t.Fatalf("durable = %+v, ok=%v", durable, ok)
	}
	head, present, err := readArchiveHead(directory, journal.region.ringEpoch())
	if err != nil || !present || head.next != durable {
		t.Fatalf("archive head = (%+v, %v, %v)", head, present, err)
	}
	meta, err := readArchiveBatch(filepath.Join(directory, head.filename), head.epoch)
	if err != nil || !archiveHeadMatchesBatch(head, meta) {
		t.Fatalf("archive batch = (%+v, %v)", meta, err)
	}
}

type faultArchiveFile struct {
	archiveFile
	writeErr   error
	readAtErr  error
	closeErr   error
	writeEvent func()
}

func (file *faultArchiveFile) Write(bytes []byte) (int, error) {
	if file.writeErr != nil {
		return 0, file.writeErr
	}
	if file.writeEvent != nil {
		file.writeEvent()
	}
	return file.archiveFile.Write(bytes)
}

func (file *faultArchiveFile) ReadAt(bytes []byte, offset int64) (int, error) {
	if file.readAtErr != nil {
		return 0, file.readAtErr
	}
	return file.archiveFile.ReadAt(bytes, offset)
}

func (file *faultArchiveFile) Close() error {
	baseErr := file.archiveFile.Close()
	return errors.Join(baseErr, file.closeErr)
}

func TestArchiverFailureNeverAdvancesDurable(t *testing.T) {
	steps := []string{
		"batch-write", "batch-validate", "batch-fdatasync", "batch-close", "batch-rename",
		"directory-one", "head-create", "head-write", "head-fsync", "head-close",
		"head-rename", "directory-two",
	}
	for _, step := range steps {
		t.Run(step, func(t *testing.T) {
			journal := newTestJournal(t, 1<<16, 4096)
			producer := newTestProducer(t, journal)
			if _, err := producer.Write([]byte("pending")); err != nil {
				t.Fatal(err)
			}
			archiver := newTestArchiver(t, journal, t.TempDir())
			now := time.Unix(0, 1)
			if _, committed, err := archiver.DrainOnce(now); err != nil || committed {
				t.Fatalf("prime DrainOnce = (%v, %v)", committed, err)
			}
			base := archiver.ops
			ops := base
			failure := errors.New("injected archive failure")
			var directorySyncs int
			ops.openFile = func(path string, flags int, mode os.FileMode) (archiveFile, error) {
				isHead := filepath.Base(path) == archiveHeadTempName
				if step == "head-create" && isHead {
					return nil, failure
				}
				file, err := base.openFile(path, flags, mode)
				if err != nil {
					return nil, err
				}
				fault := &faultArchiveFile{archiveFile: file}
				if !isHead {
					if step == "batch-write" {
						fault.writeErr = failure
					}
					if step == "batch-validate" {
						fault.readAtErr = failure
					}
					if step == "batch-close" {
						fault.closeErr = failure
					}
				} else if step == "head-close" {
					fault.closeErr = failure
				}
				return fault, nil
			}
			ops.writeAll = func(file archiveFile, bytes []byte) error {
				if step == "head-write" && len(bytes) == int(ArchiveHeadSize) {
					return failure
				}
				return base.writeAll(file, bytes)
			}
			ops.fdatasync = func(file archiveFile) error {
				if step == "batch-fdatasync" {
					return failure
				}
				return base.fdatasync(file)
			}
			ops.renameNoReplace = func(oldPath, newPath string) error {
				if step == "batch-rename" {
					return failure
				}
				return base.renameNoReplace(oldPath, newPath)
			}
			ops.rename = func(oldPath, newPath string) error {
				if step == "head-rename" && filepath.Base(newPath) == archiveHeadName {
					return failure
				}
				return base.rename(oldPath, newPath)
			}
			ops.fsync = func(file archiveFile) error {
				info, _ := file.Stat()
				if info != nil && info.IsDir() {
					directorySyncs++
					if (step == "directory-one" && directorySyncs == 1) ||
						(step == "directory-two" && directorySyncs == 2) {
						return failure
					}
				} else if step == "head-fsync" {
					return failure
				}
				return base.fsync(file)
			}
			archiver.ops = ops
			progress, committed, err := archiver.DrainOnce(now.Add(time.Second))
			if committed || progress != (ArchiveProgress{}) || !errors.Is(err, failure) {
				t.Fatalf("DrainOnce = (%+v, %v, %v), want injected failure", progress, committed, err)
			}
			durable, ok := journal.region.snapshotDurable()
			if !ok || durable != (ringCursor{sequence: 1}) || archiver.scan != durable {
				t.Fatalf("durable=%+v scan=%+v ok=%v", durable, archiver.scan, ok)
			}
			archiver.ops = base
			if progress := drainTestBatch(t, archiver, now.Add(2*time.Second)); progress.Batch != 1 {
				t.Fatalf("retry progress = %+v", progress)
			}
			durable, ok = journal.region.snapshotDurable()
			if !ok || durable.sequence != 2 {
				t.Fatalf("retry durable=%+v ok=%v", durable, ok)
			}
		})
	}
}

func TestArchiverAdoptsOrphanAndRepairsDiskAheadCursor(t *testing.T) {
	journal := newTestJournal(t, 1<<16, 4096)
	producer := newTestProducer(t, journal)
	if _, err := producer.Write([]byte("orphan")); err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	archiver := newTestArchiver(t, journal, directory)
	base := archiver.ops
	ops := base
	ops.openFile = func(path string, flags int, mode os.FileMode) (archiveFile, error) {
		if filepath.Base(path) == archiveHeadTempName {
			return nil, errors.New("crash before head")
		}
		return base.openFile(path, flags, mode)
	}
	archiver.ops = ops
	now := time.Unix(0, 1)
	if _, committed, err := archiver.DrainOnce(now); err != nil || committed {
		t.Fatalf("prime DrainOnce = (%v, %v)", committed, err)
	}
	if _, _, err := archiver.DrainOnce(now.Add(time.Second)); err == nil {
		t.Fatal("DrainOnce succeeded")
	}
	if err := archiver.Close(); err != nil {
		t.Fatal(err)
	}
	if err := producer.Close(); err != nil {
		t.Fatal(err)
	}
	recovered, err := journal.NewArchiver(ArchiveConfig{Directory: directory, Sizing: journalTestSizing(journal.region.maxRecord)})
	if err != nil {
		t.Fatalf("NewArchiver recovery: %v", err)
	}
	defer recovered.Close()
	durable, ok := journal.region.snapshotDurable()
	if !ok || durable.sequence != 2 {
		t.Fatalf("recovered durable = %+v, ok=%v", durable, ok)
	}
	head, present, err := readArchiveHead(directory, journal.region.ringEpoch())
	if err != nil || !present || head.latest != 1 || head.next != durable {
		t.Fatalf("adopted head = (%+v, %v, %v)", head, present, err)
	}
}

func TestArchiverDetectsArchiveLost(t *testing.T) {
	journal := newTestJournal(t, 1<<16, 4096)
	producer := newTestProducer(t, journal)
	if _, err := producer.Write([]byte("lost")); err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	archiver := newTestArchiver(t, journal, directory)
	drainTestBatch(t, archiver, time.Unix(0, 1))
	if err := archiver.Close(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() == archiveHeadName || strings.HasPrefix(entry.Name(), batchFilenamePrefix) {
			if err := os.Remove(filepath.Join(directory, entry.Name())); err != nil {
				t.Fatal(err)
			}
		}
	}
	if replacement, err := journal.NewArchiver(ArchiveConfig{Directory: directory, Sizing: journalTestSizing(journal.region.maxRecord)}); replacement != nil || !errors.Is(err, ErrArchiveLost) {
		t.Fatalf("NewArchiver = (%v, %v), want ErrArchiveLost", replacement, err)
	}
}

func TestArchiverRejectsSizingBeforeRoleAcquisition(t *testing.T) {
	journal := newTestJournal(t, 1<<16, 4096)
	config := journalTestSizing(journal.region.maxRecord)
	config.BatchMaxAge++
	archiver, err := journal.NewArchiver(ArchiveConfig{Directory: t.TempDir(), Sizing: config})
	if archiver != nil || !errors.Is(err, ErrConvergence) {
		t.Fatalf("NewArchiver = (%v, %v)", archiver, err)
	}
	if owner := orderedatomic.LoadAcquire64(journal.region.ptr64(JournalHeaderArchiverTIDOffset)); owner != 0 {
		t.Fatalf("archiver owner = %#x after sizing failure", owner)
	}
}

func TestArchiverThreadAndGenerationOwnership(t *testing.T) {
	journal := newTestJournal(t, 1<<16, 4096)
	archiver := newTestArchiver(t, journal, t.TempDir())
	if duplicate, err := journal.NewArchiver(ArchiveConfig{Directory: t.TempDir(), Sizing: journalTestSizing(journal.region.maxRecord)}); duplicate != nil || !errors.Is(err, ErrBusy) {
		t.Fatalf("duplicate NewArchiver = (%v, %v)", duplicate, err)
	}
	wrongThread := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		_, _, err := archiver.DrainOnce(time.Unix(0, 1))
		wrongThread <- err
	}()
	if err := <-wrongThread; !errors.Is(err, ErrMisuse) {
		t.Fatalf("wrong-thread DrainOnce = %v", err)
	}
	copied := reflectJournalCopy(archiver)
	if _, _, err := copied.DrainOnce(time.Unix(0, 1)); !errors.Is(err, ErrMisuse) {
		t.Fatalf("copied DrainOnce = %v", err)
	}
	owner := uint64(archiver.generation+1)<<32 | uint64(uint32(archiver.tid))
	orderedatomic.StoreRelease64(journal.region.ptr64(JournalHeaderArchiverTIDOffset), owner)
	if _, _, err := archiver.DrainOnce(time.Unix(0, 1)); !errors.Is(err, ErrRecovered) {
		t.Fatalf("stale DrainOnce = %v", err)
	}
	orderedatomic.StoreRelease64(journal.region.ptr64(JournalHeaderArchiverTIDOffset), uint64(archiver.generation)<<32|uint64(uint32(archiver.tid)))
}

func TestArchiverRoleRejectsLiveOwnerAndGenerationExhaustion(t *testing.T) {
	journal := newTestJournal(t, 1<<16, 4096)
	tid := journalCurrentThreadIdentity().ThreadId()
	liveOwner := uint64(7)<<32 | uint64(uint32(tid))
	orderedatomic.StoreRelease64(journal.region.ptr64(JournalHeaderArchiverTIDOffset), liveOwner)
	if _, _, err := acquireArchiverRole(journal.region, tid); !errors.Is(err, ErrBusy) {
		t.Fatalf("live-owner acquisition = %v", err)
	}
	exhaustedOwner := uint64(math.MaxUint32) << 32
	orderedatomic.StoreRelease64(journal.region.ptr64(JournalHeaderArchiverTIDOffset), exhaustedOwner)
	if _, _, err := acquireArchiverRole(journal.region, tid); !errors.Is(err, ErrPositionExhausted) {
		t.Fatalf("exhausted acquisition = %v", err)
	}
	if got := orderedatomic.LoadAcquire64(journal.region.ptr64(JournalHeaderArchiverTIDOffset)); got != exhaustedOwner {
		t.Fatalf("owner changed to %#x", got)
	}
	orderedatomic.StoreRelease64(journal.region.ptr64(JournalHeaderArchiverTIDOffset), 0)
}

func TestArchiveRecoveryRejectsGapAfterHead(t *testing.T) {
	journal := newTestJournal(t, 1<<16, 4096)
	producer := newTestProducer(t, journal)
	if _, err := producer.Write([]byte("first")); err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	archiver := newTestArchiver(t, journal, directory)
	drainTestBatch(t, archiver, time.Unix(0, 1))
	if err := archiver.Close(); err != nil {
		t.Fatal(err)
	}
	if err := producer.Close(); err != nil {
		t.Fatal(err)
	}
	gapName, _ := batchFilename(3)
	if err := os.WriteFile(filepath.Join(directory, gapName), []byte("later batch"), 0o600); err != nil {
		t.Fatal(err)
	}
	if replacement, err := journal.NewArchiver(ArchiveConfig{Directory: directory, Sizing: journalTestSizing(journal.region.maxRecord)}); replacement != nil || !errors.Is(err, ErrArchiveGap) {
		t.Fatalf("NewArchiver = (%v, %v), want ErrArchiveGap", replacement, err)
	}
}

func TestArchiveRecoveryRejectsTornOrphanTail(t *testing.T) {
	journal := newTestJournal(t, 1<<16, 4096)
	producer := newTestProducer(t, journal)
	directory := t.TempDir()
	archiver := newTestArchiver(t, journal, directory)
	if _, err := producer.Write([]byte("first")); err != nil {
		t.Fatal(err)
	}
	drainTestBatch(t, archiver, time.Unix(0, 1))
	if _, err := producer.Write([]byte("torn orphan")); err != nil {
		t.Fatal(err)
	}
	base := archiver.ops
	ops := base
	ops.openFile = func(path string, flags int, mode os.FileMode) (archiveFile, error) {
		if filepath.Base(path) == archiveHeadTempName {
			return nil, errors.New("crash before second head")
		}
		return base.openFile(path, flags, mode)
	}
	archiver.ops = ops
	now := time.Unix(0, int64(2*time.Second))
	if _, committed, err := archiver.DrainOnce(now); err != nil || committed {
		t.Fatalf("prime second DrainOnce = (%v, %v)", committed, err)
	}
	if _, _, err := archiver.DrainOnce(now.Add(time.Second)); err == nil {
		t.Fatal("second DrainOnce succeeded")
	}
	orphanName, _ := batchFilename(2)
	orphanPath := filepath.Join(directory, orphanName)
	file, err := os.OpenFile(orphanPath, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteAt([]byte{0xff}, int64(BatchHeaderReservedOffset)); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := archiver.Close(); err != nil {
		t.Fatal(err)
	}
	if err := producer.Close(); err != nil {
		t.Fatal(err)
	}
	if replacement, err := journal.NewArchiver(ArchiveConfig{Directory: directory, Sizing: journalTestSizing(journal.region.maxRecord)}); replacement != nil || !errors.Is(err, ErrArchiveCorrupt) {
		t.Fatalf("NewArchiver = (%v, %v), want ErrArchiveCorrupt", replacement, err)
	}
}

func TestArchiverRunStopsWithContext(t *testing.T) {
	journal := newTestJournal(t, 1<<16, 4096)
	archiver := newTestArchiver(t, journal, t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := archiver.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v", err)
	}
}

func TestArchiveLockSharedAndExclusiveProtocol(t *testing.T) {
	ops := defaultArchiveFileOps()
	directory := t.TempDir()
	first, err := openArchiveLock(ops, directory)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := openArchiveLock(ops, directory)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if err := lockArchiveShared(ops, first); err != nil {
		t.Fatal(err)
	}
	if err := ops.flock(second, syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		t.Fatalf("second shared lock: %v", err)
	}
	if err := unlockArchive(ops, second); err != nil {
		t.Fatal(err)
	}
	if err := ops.flock(second, syscall.LOCK_EX|syscall.LOCK_NB); !errors.Is(err, syscall.EWOULDBLOCK) {
		t.Fatalf("exclusive lock with shared holder = %v", err)
	}
	if err := unlockArchive(ops, first); err != nil {
		t.Fatal(err)
	}
	if err := ops.flock(second, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("exclusive lock after release: %v", err)
	}
	_ = unlockArchive(ops, second)
}

func TestRecordCRC32CUsesCastagnoli(t *testing.T) {
	payload := []byte("123456789")
	if got := crc32.Checksum(payload, archiveCRC32CTable); got != 0xe3069283 {
		t.Fatalf("CRC32C = %#x", got)
	}
}

type closeErrorArchiveFile struct {
	archiveFile
	err error
}

func (file *closeErrorArchiveFile) Close() error { return file.err }

func TestArchiverRetainsPrivateScanUntilAgeCut(t *testing.T) {
	journal := newTestJournal(t, 1<<16, 4096)
	producer := newTestProducer(t, journal)
	archiver := newTestArchiver(t, journal, t.TempDir())
	now := time.Unix(10, 0)
	if _, err := producer.Write([]byte("first")); err != nil {
		t.Fatal(err)
	}
	progress, committed, err := archiver.DrainOnce(now)
	if err != nil || committed || progress != (ArchiveProgress{}) {
		t.Fatalf("first DrainOnce = (%+v, %v, %v)", progress, committed, err)
	}
	if archiver.reclaim != (ringCursor{sequence: 1}) || archiver.scan.sequence != 2 || len(archiver.records) != 1 {
		t.Fatalf("pending state = reclaim %+v, scan %+v, records %d", archiver.reclaim, archiver.scan, len(archiver.records))
	}
	if _, err := producer.Write([]byte("second")); err != nil {
		t.Fatal(err)
	}
	progress, committed, err = archiver.DrainOnce(now.Add(time.Millisecond))
	if err != nil || committed || progress != (ArchiveProgress{}) || archiver.scan.sequence != 3 || len(archiver.records) != 2 {
		t.Fatalf("second DrainOnce = (%+v, %v, %v), scan %+v, records %d", progress, committed, err, archiver.scan, len(archiver.records))
	}
	progress, committed, err = archiver.DrainOnce(now.Add(time.Second))
	if err != nil || !committed {
		t.Fatalf("age DrainOnce = (%+v, %v, %v)", progress, committed, err)
	}
	if progress != (ArchiveProgress{Batch: 1, First: 1, End: 3, DurableBytes: 2 * uint64(JournalGrain)}) {
		t.Fatalf("progress = %+v", progress)
	}
}

func TestArchiverCutsAtOccupiedByteBound(t *testing.T) {
	journal := newTestJournal(t, 1<<16, 4096)
	producer := newTestProducer(t, journal)
	archiver := newTestArchiver(t, journal, t.TempDir())
	payload := bytes.Repeat([]byte{0x5a}, int(journal.region.maxRecord))
	if _, err := producer.Write(payload); err != nil {
		t.Fatal(err)
	}
	progress, committed, err := archiver.DrainOnce(time.Unix(10, 0))
	if err != nil || !committed {
		t.Fatalf("DrainOnce = (%+v, %v, %v)", progress, committed, err)
	}
	if progress.DurableBytes != archiver.config.Sizing.BatchMaxOccupiedBytes || progress.Batch != 1 {
		t.Fatalf("progress = %+v", progress)
	}
}

func TestArchiverPropagatesUnlockError(t *testing.T) {
	journal := newTestJournal(t, 1<<16, 4096)
	archiver := newTestArchiver(t, journal, t.TempDir())
	base := archiver.ops
	failure := errors.New("unlock failure")
	ops := base
	ops.flock = func(file archiveFile, operation int) error {
		if operation == syscall.LOCK_UN {
			return failure
		}
		return base.flock(file, operation)
	}
	archiver.ops = ops
	_, committed, err := archiver.DrainOnce(time.Unix(10, 0))
	archiver.ops = base
	if committed || !errors.Is(err, failure) {
		t.Fatalf("DrainOnce = (%v, %v)", committed, err)
	}
}

func TestArchiverClosePropagatesFileCloseError(t *testing.T) {
	journal := newTestJournal(t, 1<<16, 4096)
	archiver := newTestArchiver(t, journal, t.TempDir())
	failure := errors.New("close failure")
	original := archiver.lock
	archiver.lock = &closeErrorArchiveFile{archiveFile: original, err: failure}
	if err := archiver.Close(); !errors.Is(err, failure) {
		t.Fatalf("Close = %v", err)
	}
	if err := original.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestArchiveNoReplacePreservesExistingFile(t *testing.T) {
	directory := t.TempDir()
	temporary := filepath.Join(directory, "temporary")
	final := filepath.Join(directory, "final")
	if err := os.WriteFile(temporary, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(final, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := renameArchiveNoReplace(temporary, final); !errors.Is(err, os.ErrExist) && !errors.Is(err, syscall.EEXIST) {
		t.Fatalf("renameArchiveNoReplace = %v", err)
	}
	bytes, err := os.ReadFile(final)
	if err != nil || string(bytes) != "old" {
		t.Fatalf("final = %q, %v", bytes, err)
	}
}

func TestArchiveNoReplaceProcessCollision(t *testing.T) {
	if os.Getenv("JOURNAL_ARCHIVE_COLLISION_HELPER") != "" {
		directory := os.Getenv("JOURNAL_ARCHIVE_COLLISION_DIRECTORY")
		temporary := filepath.Join(directory, "temporary-"+os.Getenv("JOURNAL_ARCHIVE_COLLISION_HELPER"))
		if err := os.WriteFile(temporary, []byte(os.Getenv("JOURNAL_ARCHIVE_COLLISION_HELPER")), 0o600); err != nil {
			os.Exit(2)
		}
		if err := renameArchiveNoReplace(temporary, filepath.Join(directory, "final")); err != nil && !errors.Is(err, syscall.EEXIST) {
			os.Exit(3)
		}
		return
	}
	directory := t.TempDir()
	commands := make([]*exec.Cmd, 2)
	for index, value := range []string{"first", "second"} {
		commands[index] = exec.Command(os.Args[0], "-test.run=^TestArchiveNoReplaceProcessCollision$")
		commands[index].Env = append(os.Environ(),
			"JOURNAL_ARCHIVE_COLLISION_HELPER="+value,
			"JOURNAL_ARCHIVE_COLLISION_DIRECTORY="+directory,
		)
		if err := commands[index].Start(); err != nil {
			t.Fatal(err)
		}
	}
	for _, command := range commands {
		if err := command.Wait(); err != nil {
			t.Fatal(err)
		}
	}
	bytes, err := os.ReadFile(filepath.Join(directory, "final"))
	if err != nil || (string(bytes) != "first" && string(bytes) != "second") {
		t.Fatalf("final = %q, %v", bytes, err)
	}
}

func TestNewArchiverCanonicalizesRelativeDirectory(t *testing.T) {
	original, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Chdir(original); err != nil {
			t.Errorf("restore working directory: %v", err)
		}
	}()
	journal := newTestJournal(t, 1<<16, 4096)
	producer := newTestProducer(t, journal)
	archiver := newTestArchiver(t, journal, "archive")
	if !filepath.IsAbs(archiver.config.Directory) {
		t.Fatalf("directory = %q", archiver.config.Directory)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if _, err := producer.Write(bytes.Repeat([]byte{1}, int(journal.region.maxRecord))); err != nil {
		t.Fatal(err)
	}
	progress, committed, err := archiver.DrainOnce(time.Unix(10, 0))
	if err != nil || !committed || progress.Batch != 1 {
		t.Fatalf("DrainOnce = (%+v, %v, %v)", progress, committed, err)
	}
	if _, err := os.Stat(filepath.Join(root, "archive", archiveHeadName)); err != nil {
		t.Fatal(err)
	}
}

func TestNewArchiverRemovesAllMatchingTemporaryFiles(t *testing.T) {
	journal := newTestJournal(t, 1<<16, 4096)
	directory := t.TempDir()
	names := []string{
		archiveHeadTempName,
		"batch-00000000000000000001.jrn.zst.1.1.tmp",
		"batch-00000000000000000002.jrn.zst.tmp",
	}
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(directory, name), []byte("temporary"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	archiver := newTestArchiver(t, journal, directory)
	_ = archiver
	for _, name := range names {
		if _, err := os.Stat(filepath.Join(directory, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("temporary %q remains: %v", name, err)
		}
	}
}

func TestReadArchiveBatchRejectsNonRegularFile(t *testing.T) {
	directory := t.TempDir()
	name, _ := batchFilename(1)
	path := filepath.Join(directory, name)
	if err := os.Symlink("missing", path); err != nil {
		t.Fatal(err)
	}
	if _, err := readArchiveBatchWithOps(path, RingEpoch{1}, 1<<20, 1<<20, defaultArchiveFileOps()); err == nil {
		t.Fatal("readArchiveBatchWithOps accepted a nonregular file")
	}
}

func TestParseArchiveBatchRejectsMaliciousBlockBoundBeforeAllocation(t *testing.T) {
	file := make([]byte, BatchHeaderSize+BatchBlockHeaderSize+BatchIndexEntrySize+BatchFooterSize)
	put64(file, BatchHeaderMagicOffset, BatchMagic)
	put32(file, BatchHeaderVersionOffset, BatchFormatVersion)
	put32(file, BatchHeaderSizeOffset, BatchHeaderSize)
	put32(file, BatchHeaderEndianOffset, JournalEndianMarker)
	put32(file, BatchHeaderCodecOffset, BatchCodecZstandard)
	epoch := RingEpoch{1}
	copy(file[BatchHeaderRingEpochOffset:], epoch[:])
	put64(file, BatchHeaderBatchIDOffset, 1)
	put64(file, BatchHeaderFirstSequenceOffset, 1)
	put64(file, BatchHeaderEndSequenceOffset, 2)
	put64(file, BatchHeaderNextPositionOffset, uint64(JournalGrain))
	put64(file, BatchHeaderRecordCountOffset, 1)
	put64(file, BatchHeaderOccupiedBytesOffset, uint64(JournalGrain))
	put64(file, BatchHeaderBlockMaxBytesOffset, math.MaxUint64)
	put32(file, BatchHeaderBlockCountOffset, 1)
	putCRC32C(file[:BatchHeaderSize], BatchHeaderCRC32COffset)
	name, _ := batchFilename(1)
	if _, err := parseArchiveBatchReader(bytes.NewReader(file), uint64(len(file)), name, RingEpoch{1}); err == nil {
		t.Fatal("parseArchiveBatchReader accepted a malicious block bound")
	}
}

func writeTestRetentionFloor(t *testing.T, directory string, epoch RingEpoch, floor BatchID) {
	t.Helper()
	bytes := make([]byte, RetentionFloorSize)
	put64(bytes, RetentionFloorMagicOffset, RetentionFloorMagic)
	put32(bytes, RetentionFloorVersionOffset, RetentionFloorFormatVersion)
	put32(bytes, RetentionFloorSizeOffset, RetentionFloorSize)
	put32(bytes, RetentionFloorEndianOffset, JournalEndianMarker)
	copy(bytes[RetentionFloorRingEpochOffset:], epoch[:])
	put64(bytes, RetentionFloorOldestBatchIDOffset, uint64(floor))
	putCRC32C(bytes, RetentionFloorCRC32COffset)
	if err := os.WriteFile(filepath.Join(directory, retentionFloorName), bytes, 0o600); err != nil {
		t.Fatal(err)
	}
}

func createTestArchiveBatches(t *testing.T, count int) (*Journal, string) {
	t.Helper()
	journal := newTestJournal(t, 1<<16, 4096)
	producer := newTestProducer(t, journal)
	directory := t.TempDir()
	archiver := newTestArchiver(t, journal, directory)
	for index := 0; index < count; index++ {
		payload := bytes.Repeat([]byte{byte(index + 1)}, int(journal.region.maxRecord))
		if _, err := producer.Write(payload); err != nil {
			t.Fatal(err)
		}
		progress, committed, err := archiver.DrainOnce(time.Unix(int64(index+1), 0))
		if err != nil || !committed || progress.Batch != BatchID(index+1) {
			t.Fatalf("batch %d DrainOnce = (%+v, %v, %v)", index+1, progress, committed, err)
		}
	}
	if err := archiver.Close(); err != nil {
		t.Fatal(err)
	}
	if err := producer.Close(); err != nil {
		t.Fatal(err)
	}
	return journal, directory
}

func TestArchiveRecoveryValidatesRetainedInteriorGap(t *testing.T) {
	journal, directory := createTestArchiveBatches(t, 3)
	writeTestRetentionFloor(t, directory, journal.region.ringEpoch(), 1)
	name, _ := batchFilename(2)
	if err := os.Remove(filepath.Join(directory, name)); err != nil {
		t.Fatal(err)
	}
	archiver, err := journal.NewArchiver(ArchiveConfig{Directory: directory, Sizing: journalTestSizing(journal.region.maxRecord)})
	if archiver != nil || !errors.Is(err, ErrArchiveGap) {
		t.Fatalf("NewArchiver = (%v, %v), want ErrArchiveGap", archiver, err)
	}
}

func TestArchiveRecoveryValidatesRetainedPrefixCorruption(t *testing.T) {
	journal, directory := createTestArchiveBatches(t, 3)
	writeTestRetentionFloor(t, directory, journal.region.ringEpoch(), 1)
	name, _ := batchFilename(1)
	file, err := os.OpenFile(filepath.Join(directory, name), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteAt([]byte{1}, int64(BatchHeaderReservedOffset)); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	archiver, err := journal.NewArchiver(ArchiveConfig{Directory: directory, Sizing: journalTestSizing(journal.region.maxRecord)})
	if archiver != nil || !errors.Is(err, ErrArchiveCorrupt) {
		t.Fatalf("NewArchiver = (%v, %v), want ErrArchiveCorrupt", archiver, err)
	}
}

func TestArchiveRecoveryBoundsRetainedHistoryBytes(t *testing.T) {
	journal, directory := createTestArchiveBatches(t, 3)
	sizing := journalTestSizing(journal.region.maxRecord)
	firstName, _ := batchFilename(1)
	firstInfo, err := os.Stat(filepath.Join(directory, firstName))
	if err != nil {
		t.Fatal(err)
	}
	sizing.Bounds.MaxRecoveryScanBytes = uint64(firstInfo.Size())
	err = recoverArchive(
		journal,
		ArchiveConfig{Directory: directory, Sizing: sizing},
		defaultArchiveFileOps(),
	)
	if !errors.Is(err, ErrConvergence) {
		t.Fatalf("recoverArchive = %v, want ErrConvergence", err)
	}
}

func TestArchiveRecoveryBoundsCumulativeOrphanBytes(t *testing.T) {
	journal, directory := createTestArchiveBatches(t, 3)
	sizing := journalTestSizing(journal.region.maxRecord)
	firstName, _ := batchFilename(1)
	first, err := readArchiveBatch(
		filepath.Join(directory, firstName),
		journal.region.ringEpoch(),
	)
	if err != nil {
		t.Fatal(err)
	}
	headBytes, err := encodeArchiveHead(archiveHeadForBatch(first))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, archiveHeadName), headBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	firstInfo, err := os.Stat(filepath.Join(directory, firstName))
	if err != nil {
		t.Fatal(err)
	}
	secondName, _ := batchFilename(2)
	thirdName, _ := batchFilename(3)
	secondInfo, err := os.Stat(filepath.Join(directory, secondName))
	if err != nil {
		t.Fatal(err)
	}
	thirdInfo, err := os.Stat(filepath.Join(directory, thirdName))
	if err != nil {
		t.Fatal(err)
	}
	sizing.Bounds.MaxRecoveryScanBytes = uint64(firstInfo.Size() + max(secondInfo.Size(), thirdInfo.Size()))
	err = recoverArchive(journal, ArchiveConfig{Directory: directory, Sizing: sizing}, defaultArchiveFileOps())
	if !errors.Is(err, ErrConvergence) {
		t.Fatalf("recoverArchive = %v, want ErrConvergence", err)
	}
}

func TestNewArchiverSyncsArchiveParentDirectory(t *testing.T) {
	journal := newTestJournal(t, 1<<16, 4096)
	root := t.TempDir()
	directory := filepath.Join(root, "new", "archive")
	base := journalArchiveFileOps
	ops := base
	var opened []string
	var syncs int
	ops.openDir = func(path string) (archiveFile, error) {
		opened = append(opened, filepath.Clean(path))
		return base.openDir(path)
	}
	ops.fsync = func(file archiveFile) error {
		syncs++
		return base.fsync(file)
	}
	journalArchiveFileOps = ops
	defer func() { journalArchiveFileOps = base }()
	archiver, err := journal.NewArchiver(ArchiveConfig{Directory: directory, Sizing: journalTestSizing(journal.region.maxRecord)})
	if err != nil {
		t.Fatal(err)
	}
	defer archiver.Close()
	if len(opened) == 0 || opened[0] != filepath.Dir(directory) || syncs < 3 {
		t.Fatalf("opened = %v, syncs = %d", opened, syncs)
	}
}

func TestArchiverRejectsOrphanWhosePayloadDiffersFromRing(t *testing.T) {
	journal := newTestJournal(t, 1<<16, 4096)
	producer := newTestProducer(t, journal)
	directory := t.TempDir()
	archiver := newTestArchiver(t, journal, directory)
	if _, err := producer.Write(bytes.Repeat([]byte{0x11}, int(journal.region.maxRecord))); err != nil {
		t.Fatal(err)
	}
	base := archiver.ops
	ops := base
	failure := errors.New("head failure")
	ops.openFile = func(path string, flags int, mode os.FileMode) (archiveFile, error) {
		if filepath.Base(path) == archiveHeadTempName {
			return nil, failure
		}
		return base.openFile(path, flags, mode)
	}
	archiver.ops = ops
	if _, committed, err := archiver.DrainOnce(time.Unix(10, 0)); committed || !errors.Is(err, failure) {
		t.Fatalf("DrainOnce = (%v, %v)", committed, err)
	}
	archiver.ops = base
	journal.region.payload(0, 1)[0] ^= 1
	if _, committed, err := archiver.DrainOnce(time.Unix(11, 0)); committed || !errors.Is(err, ErrArchiveCorrupt) {
		t.Fatalf("orphan DrainOnce = (%v, %v)", committed, err)
	}
}

func TestArchiveRecoveryRejectsOrphanWhosePayloadDiffersFromRing(t *testing.T) {
	journal := newTestJournal(t, 1<<16, 4096)
	producer := newTestProducer(t, journal)
	directory := t.TempDir()
	archiver := newTestArchiver(t, journal, directory)
	if _, err := producer.Write(bytes.Repeat([]byte{0x22}, int(journal.region.maxRecord))); err != nil {
		t.Fatal(err)
	}
	base := archiver.ops
	ops := base
	failure := errors.New("head failure")
	ops.openFile = func(path string, flags int, mode os.FileMode) (archiveFile, error) {
		if filepath.Base(path) == archiveHeadTempName {
			return nil, failure
		}
		return base.openFile(path, flags, mode)
	}
	archiver.ops = ops
	if _, committed, err := archiver.DrainOnce(time.Unix(10, 0)); committed || !errors.Is(err, failure) {
		t.Fatalf("DrainOnce = (%v, %v)", committed, err)
	}
	archiver.ops = base
	if err := archiver.Close(); err != nil {
		t.Fatal(err)
	}
	if err := producer.Close(); err != nil {
		t.Fatal(err)
	}
	journal.region.payload(0, 1)[0] ^= 1
	recovered, err := journal.NewArchiver(ArchiveConfig{
		Directory: directory,
		Sizing:    journalTestSizing(journal.region.maxRecord),
	})
	if recovered != nil || !errors.Is(err, ErrArchiveCorrupt) {
		t.Fatalf("NewArchiver = (%v, %v), want ErrArchiveCorrupt", recovered, err)
	}
}
