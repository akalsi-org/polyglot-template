//go:build linux && (amd64 || arm64)

package journal

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/akalsi-org/polyglot-template/go/lib/hostcpu"
	"github.com/akalsi-org/polyglot-template/go/lib/internal/orderedatomic"
)

var _ interface {
	NewProducer() (*Producer, error)
	AttachProducer() (*Producer, error)
} = (*Journal)(nil)

func reflectJournalCopy[T any](source *T) *T {
	copy := reflect.New(reflect.TypeOf(source).Elem())
	copy.Elem().Set(reflect.ValueOf(source).Elem())
	return copy.Interface().(*T)
}

func journalTestSizing(maximum uint64) SizingConfig {
	extent, _ := journalExtent(maximum)
	sizing := SizingConfig{
		BatchMaxOccupiedBytes: extent,
		BatchMaxAge:           time.Second,
		ZstdBlockBytes:        maximum + uint64(BatchRecordHeaderSize),
		Bounds: Bounds{
			PeakOccupiedBytesPerSecond:      1,
			PeakArchiveBytesPerSecond:       1,
			MinArchiveBytesPerSecond:        ^uint64(0),
			MinObserverReplayBytesPerSecond: 2,
			BurstOccupiedBytes:              uint64(JournalGrain),
			MaxArchiveStall:                 time.Nanosecond,
			MaxSyncLatency:                  time.Nanosecond,
		},
	}
	maximumBatchFileBytes, err := MaximumArchiveBatchFileBytes(sizing)
	if err != nil {
		panic(err)
	}
	sizing.Bounds.MaxRecoveryScanBytes = maximumBatchFileBytes
	return sizing
}

func newTestJournal(t *testing.T, capacity, maximum uint64) *Journal {
	t.Helper()
	journal, err := Create(Config{
		Capacity:           capacity,
		MaxRecordBytes:     maximum,
		Sizing:             journalTestSizing(maximum),
		Backend:            BackendMemfd,
		DisablePreallocate: true,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() {
		if err := journal.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return journal
}

func newTestProducer(t *testing.T, journal *Journal) *Producer {
	t.Helper()
	producer, err := journal.AttachProducer()
	if err != nil {
		t.Fatalf("AttachProducer: %v", err)
	}
	t.Cleanup(func() {
		if err := producer.Close(); err != nil {
			t.Errorf("Producer.Close: %v", err)
		}
	})
	return producer
}

func TestCreateAttachAndGoldenPlanes(t *testing.T) {
	journal := newTestJournal(t, 1<<16, 4096)
	if journal.Capacity() != 1<<16 {
		t.Fatalf("Capacity = %d, want %d", journal.Capacity(), 1<<16)
	}
	if journal.region.tagBase != uintptr(JournalHeaderSize) {
		t.Fatalf("tag base = %d, want %d", journal.region.tagBase, JournalHeaderSize)
	}
	wantDescriptor := uintptr(JournalHeaderSize) + uintptr((1<<16)/JournalGrain*JournalTagSize)
	if journal.region.descriptorBase != wantDescriptor {
		t.Fatalf("descriptor base = %d, want %d", journal.region.descriptorBase, wantDescriptor)
	}
	layout, err := journalLayout(journal.Capacity())
	if err != nil {
		t.Fatal(err)
	}
	wantControl := JournalArenaAlignment
	if layout.ControlBytes != wantControl {
		t.Fatalf("control bytes = %d, want %d", layout.ControlBytes, wantControl)
	}
	fd, err := journal.DupFD()
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fd)
	flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), syscall.F_GETFD, 0)
	if errno != 0 || flags&syscall.FD_CLOEXEC == 0 {
		t.Fatalf("duplicated descriptor flags = %#x, errno %v", flags, errno)
	}
	if _, err := syscall.Seek(fd, 17, 0); err != nil {
		t.Fatal(err)
	}
	attached, err := Attach(fd)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer attached.Close()
	if attached.Capacity() != journal.Capacity() {
		t.Fatalf("attached capacity = %d, want %d", attached.Capacity(), journal.Capacity())
	}
	if offset, err := syscall.Seek(fd, 0, 1); err != nil || offset != 17 {
		t.Fatalf("descriptor offset = %d, err %v, want 17", offset, err)
	}
	attached.region.mirroredArena[0] = 0x5a
	if attached.region.mirroredArena[attached.Capacity()] != 0x5a {
		t.Fatal("attached arena is not mirrored")
	}
	if err := attached.Close(); err != nil {
		t.Fatal(err)
	}
	if attached.region.control != nil || attached.region.mirroredArena != nil {
		t.Fatal("Close retained borrowed mapping slices")
	}
	if err := attached.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestJournalLayoutIsHostPageIndependent(t *testing.T) {
	page4K, err := journalLayoutForPage(1<<20, 4<<10)
	if err != nil {
		t.Fatal(err)
	}
	page64K, err := journalLayoutForPage(1<<20, 64<<10)
	if err != nil {
		t.Fatal(err)
	}
	if page4K != page64K {
		t.Fatalf("4 KiB layout = %+v, 64 KiB layout = %+v", page4K, page64K)
	}
	if page4K.ControlBytes != 5*(64<<10) || page4K.ArenaBytes != 1<<20 {
		t.Fatalf("layout = %+v, want 320 KiB control and 1 MiB arena", page4K)
	}
}

func TestCreatedHeaderGoldenValues(t *testing.T) {
	journal := newTestJournal(t, 1<<20, 4096)
	header := journal.region.control[:JournalHeaderSize]
	if get64(header, JournalHeaderMagicOffset) != JournalMagic ||
		get32(header, JournalHeaderVersionOffset) != JournalFormatVersion ||
		get32(header, JournalHeaderSizeOffset) != JournalHeaderSize ||
		get32(header, JournalHeaderEndianOffset) != JournalEndianMarker ||
		get32(header, JournalHeaderGrainOffset) != JournalGrain ||
		get64(header, JournalHeaderCapacityOffset) != 1<<20 ||
		get64(header, JournalHeaderMaxRecordBytesOffset) != 4096 {
		t.Fatalf("created header prefix = %x", header[:40])
	}
	if get32(header, JournalHeaderCreatorPIDOffset) != uint32(os.Getpid()) {
		t.Fatalf("creator PID = %d, want %d", get32(header, JournalHeaderCreatorPIDOffset), os.Getpid())
	}
	if get64(header, JournalHeaderPublishSequenceOffset) != 1 || get64(header, JournalHeaderDurableSequenceOffset) != 1 {
		t.Fatalf("initial sequences = publish %d durable %d", get64(header, JournalHeaderPublishSequenceOffset), get64(header, JournalHeaderDurableSequenceOffset))
	}
	sizing := journalTestSizing(4096)
	required, err := RequiredCapacity(4096, sizing)
	if err != nil {
		t.Fatal(err)
	}
	if got := get64(header, JournalHeaderRequiredCapacityOffset); got != required {
		t.Fatalf("required capacity = %d, want %d", got, required)
	}
	digest := sizingConfigDigest(4096, required, sizing)
	if got := header[JournalHeaderSizingDigestOffset : JournalHeaderSizingDigestOffset+SizingConfigDigestSize]; string(got) != string(digest[:]) {
		t.Fatalf("sizing digest = %x, want %x", got, digest)
	}
	if !allZero(header[120:128]) || !allZero(header[JournalHeaderReservedOffset:]) {
		t.Fatal("created header contains nonzero reserved bytes")
	}
	if !allZero(journal.region.control[journal.region.tagBase:journal.region.descriptorBase]) {
		t.Fatal("created tag plane is not zero")
	}
}

func TestCreateRetriesAllZeroRingEpoch(t *testing.T) {
	originalRead := journalRandomRead
	calls := 0
	journalRandomRead = func(bytes []byte) (int, error) {
		calls++
		clear(bytes)
		if calls > 1 {
			for index := range bytes {
				bytes[index] = byte(index + 1)
			}
		}
		return len(bytes), nil
	}
	t.Cleanup(func() { journalRandomRead = originalRead })
	journal := newTestJournal(t, 1<<16, 4096)
	if calls != 2 {
		t.Fatalf("random read calls = %d, want 2", calls)
	}
	if allZero(journal.region.control[JournalHeaderRingEpochOffset : JournalHeaderRingEpochOffset+16]) {
		t.Fatal("created ring epoch remained zero")
	}
}

func TestCreateFailureRemovesExclusiveBackingForRetry(t *testing.T) {
	failure := errors.New("injected journal header failure")
	for _, backend := range []Backend{BackendSHM, BackendFile} {
		for _, injection := range []string{"random", "header"} {
			name := fmt.Sprintf("%d/%s", backend, injection)
			t.Run(name, func(t *testing.T) {
				config := Config{
					Capacity:           1 << 16,
					MaxRecordBytes:     4096,
					Sizing:             journalTestSizing(4096),
					Backend:            backend,
					DisablePreallocate: true,
				}
				var path string
				if backend == BackendSHM {
					config.Name = fmt.Sprintf(
						"pgt-journal-create-%d-%d",
						os.Getpid(),
						time.Now().UnixNano(),
					)
					path = "/dev/shm/" + config.Name
				} else {
					path = t.TempDir() + "/journal"
					config.Name = path
				}
				t.Cleanup(func() { _ = os.Remove(path) })

				originalRead := journalRandomRead
				originalInitialize := journalInitializeHeader
				if injection == "random" {
					journalRandomRead = func([]byte) (int, error) {
						return 0, failure
					}
				} else {
					journalInitializeHeader = func(
						[]byte,
						uint64,
						uint64,
						uint64,
						[SizingConfigDigestSize]byte,
					) error {
						return failure
					}
				}
				journal, err := Create(config)
				journalRandomRead = originalRead
				journalInitializeHeader = originalInitialize
				if journal != nil || !errors.Is(err, failure) {
					t.Fatalf("failed Create = (%v, %v), want nil injected failure", journal, err)
				}
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("failed Create retained exclusive backing: %v", err)
				}
				journal, err = Create(config)
				if err != nil {
					t.Fatalf("retry Create: %v", err)
				}
				if err := journal.Close(); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestCreateFailurePreservesOverwriteBacking(t *testing.T) {
	path := t.TempDir() + "/journal"
	if err := os.WriteFile(path, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("injected journal header failure")
	originalInitialize := journalInitializeHeader
	journalInitializeHeader = func(
		[]byte,
		uint64,
		uint64,
		uint64,
		[SizingConfigDigestSize]byte,
	) error {
		return failure
	}
	config := Config{
		Capacity:           1 << 16,
		MaxRecordBytes:     4096,
		Sizing:             journalTestSizing(4096),
		Backend:            BackendFile,
		Name:               path,
		DisablePreallocate: true,
		AllowOverwrite:     true,
	}
	journal, err := Create(config)
	journalInitializeHeader = originalInitialize
	if journal != nil || !errors.Is(err, failure) {
		t.Fatalf("failed Create = (%v, %v), want nil injected failure", journal, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("failed overwrite Create removed backing: %v", err)
	}
	journal, err = Create(config)
	if err != nil {
		t.Fatalf("retry overwrite Create: %v", err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCreateFailureJoinsCleanupError(t *testing.T) {
	path := t.TempDir() + "/journal"
	createFailure := errors.New("injected journal header failure")
	cleanupFailure := errors.New("injected unlink failure")
	originalInitialize := journalInitializeHeader
	originalUnlink := journalUnlink
	journalInitializeHeader = func(
		[]byte,
		uint64,
		uint64,
		uint64,
		[SizingConfigDigestSize]byte,
	) error {
		return createFailure
	}
	journalUnlink = func(string) error { return cleanupFailure }
	journal, err := Create(Config{
		Capacity:           1 << 16,
		MaxRecordBytes:     4096,
		Sizing:             journalTestSizing(4096),
		Backend:            BackendFile,
		Name:               path,
		DisablePreallocate: true,
	})
	journalInitializeHeader = originalInitialize
	journalUnlink = originalUnlink
	if journal != nil || !errors.Is(err, createFailure) ||
		!errors.Is(err, cleanupFailure) {
		t.Fatalf(
			"failed Create = (%v, %v), want joined create and cleanup failures",
			journal,
			err,
		)
	}
	if removeErr := os.Remove(path); removeErr != nil {
		t.Fatal(removeErr)
	}
}

func TestCreateRoundsCapacityAndValidatesMaximum(t *testing.T) {
	journal := newTestJournal(t, 20000, 64)
	if journal.Capacity() != minimumJournalCapacity {
		t.Fatalf("Capacity = %d, want %d", journal.Capacity(), minimumJournalCapacity)
	}
	if _, err := Create(Config{Capacity: 1 << 16, Backend: BackendMemfd}); !errors.Is(err, ErrConvergence) {
		t.Fatalf("missing sizing error = %v, want ErrConvergence", err)
	}
	sizing := journalTestSizing(64)
	sizing.BatchMaxOccupiedBytes = JournalDescriptorMaxLength + 1
	sizing.ZstdBlockBytes = JournalDescriptorMaxLength + 1 + uint64(BatchRecordHeaderSize)
	if _, err := Create(Config{Capacity: 1 << 16, MaxRecordBytes: JournalDescriptorMaxLength + 1, Sizing: sizing, Backend: BackendMemfd}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversize maximum error = %v, want ErrTooLarge", err)
	}
}

func TestCreateRejectsCapacityBelowSizingRequirement(t *testing.T) {
	sizing := journalTestSizing(4096)
	required, err := RequiredCapacity(4096, sizing)
	if err != nil {
		t.Fatal(err)
	}
	path := t.TempDir() + "/journal"
	journal, err := Create(Config{
		Capacity:       required - 1,
		MaxRecordBytes: 4096,
		Sizing:         sizing,
		Backend:        BackendFile,
		Name:           path,
	})
	if journal != nil || !errors.Is(err, ErrConvergence) {
		t.Fatalf("Create = (%v, %v), want nil ErrConvergence", journal, err)
	}
	var convergence *ConvergenceError
	if !errors.As(err, &convergence) || convergence.RequiredCapacity != required {
		t.Fatalf("convergence error = %#v, want required capacity %d", err, required)
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("undersized Create changed backing path: %v", statErr)
	}
}

func TestAttachRejectsInvalidSizingContract(t *testing.T) {
	tests := []struct {
		name   string
		mutate func([]byte)
	}{
		{
			name: "zero requirement",
			mutate: func(header []byte) {
				put64(header, JournalHeaderRequiredCapacityOffset, 0)
			},
		},
		{
			name: "requirement above capacity",
			mutate: func(header []byte) {
				put64(header, JournalHeaderRequiredCapacityOffset, 1<<17)
			},
		},
		{
			name: "zero digest",
			mutate: func(header []byte) {
				clear(header[JournalHeaderSizingDigestOffset : JournalHeaderSizingDigestOffset+SizingConfigDigestSize])
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			journal := newTestJournal(t, 1<<16, 4096)
			test.mutate(journal.region.control)
			fd, err := journal.DupFD()
			if err != nil {
				t.Fatal(err)
			}
			defer syscall.Close(fd)
			attached, err := Attach(fd)
			if attached != nil || !errors.Is(err, ErrFormat) {
				t.Fatalf("Attach = (%v, %v), want nil ErrFormat", attached, err)
			}
		})
	}
}

func TestAttachProducerRejectsInvalidSizingContract(t *testing.T) {
	journal := newTestJournal(t, 1<<16, 4096)
	put64(journal.region.control, JournalHeaderRequiredCapacityOffset, 0)
	producer, err := journal.AttachProducer()
	if producer != nil || !errors.Is(err, ErrFormat) {
		t.Fatalf("AttachProducer = (%v, %v), want nil ErrFormat", producer, err)
	}
}

func TestReserveCommitPublishesAllGrains(t *testing.T) {
	journal := newTestJournal(t, 1<<16, 4096)
	producer := newTestProducer(t, journal)
	span, err := producer.Reserve(129)
	if err != nil {
		t.Fatal(err)
	}
	for grain := uint64(0); grain < 3; grain++ {
		tag := orderedatomic.LoadAcquire64(journal.region.tag(grain * uint64(JournalGrain)))
		if tag != WritingTag(1) {
			t.Fatalf("writing tag %d = %#x, want %#x", grain, tag, WritingTag(1))
		}
		if descriptor := orderedatomic.LoadAcquire64(journal.region.descriptor(grain * uint64(JournalGrain))); descriptor != 0 {
			t.Fatalf("descriptor %d = %#x, want zero", grain, descriptor)
		}
	}
	for index := range span.Bytes() {
		span.Bytes()[index] = byte(index)
	}
	if _, err := producer.Commit(span, 129); err != nil {
		t.Fatal(err)
	}
	for grain := uint64(0); grain < 3; grain++ {
		tag := orderedatomic.LoadAcquire64(journal.region.tag(grain * uint64(JournalGrain)))
		if tag != StableTag(1) {
			t.Fatalf("stable tag %d = %#x, want %#x", grain, tag, StableTag(1))
		}
	}
	descriptor := orderedatomic.LoadAcquire64(journal.region.descriptor(0))
	wantDescriptor := uint64(129) | uint64(3)<<JournalDescriptorLengthBits
	if descriptor != wantDescriptor {
		t.Fatalf("descriptor = %#x, want %#x", descriptor, wantDescriptor)
	}
	publish, ok := journal.region.snapshotPublish()
	if !ok || publish != (ringCursor{sequence: 2, position: 192}) {
		t.Fatalf("publish cursor = %+v, ok=%v", publish, ok)
	}
}

func TestShortCommitReclaimsReservationSlack(t *testing.T) {
	journal := newTestJournal(t, 1<<16, 4096)
	producer := newTestProducer(t, journal)
	const staleDescriptor = uint64(0xfeedface)
	orderedatomic.StoreRelaxed64(journal.region.descriptor(0), staleDescriptor)
	span, err := producer.Reserve(129)
	if err != nil {
		t.Fatal(err)
	}
	if got := orderedatomic.LoadRelaxed64(journal.region.descriptor(0)); got != staleDescriptor {
		t.Fatalf("Reserve cleared start descriptor = %#x, want %#x", got, staleDescriptor)
	}
	for grain := uint64(1); grain < 3; grain++ {
		if got := orderedatomic.LoadRelaxed64(journal.region.descriptor(grain * uint64(JournalGrain))); got != 0 {
			t.Fatalf("continuation descriptor %d = %#x, want zero", grain, got)
		}
	}
	if _, err := producer.Commit(span, 1); err != nil {
		t.Fatal(err)
	}
	publish, ok := journal.region.snapshotPublish()
	if !ok || publish != (ringCursor{sequence: 2, position: 64}) {
		t.Fatalf("short publish = %+v, ok=%v", publish, ok)
	}
	length, grains, flags, ok := unpackDescriptor(orderedatomic.LoadRelaxed64(journal.region.descriptor(0)))
	if !ok || length != 1 || grains != 1 || flags != 0 {
		t.Fatalf("short descriptor = (%d, %d, %d, %v)", length, grains, flags, ok)
	}
	if got := orderedatomic.LoadAcquire64(journal.region.tag(0)); got != StableTag(1) {
		t.Fatalf("short start tag = %#x, want %#x", got, StableTag(1))
	}
	for grain := uint64(1); grain < 3; grain++ {
		if got := orderedatomic.LoadAcquire64(journal.region.tag(grain * uint64(JournalGrain))); got != WritingTag(1) {
			t.Fatalf("unused tag %d = %#x, want %#x", grain, got, WritingTag(1))
		}
	}
	next, err := producer.Reserve(129)
	if err != nil {
		t.Fatal(err)
	}
	if next.position != 64 {
		t.Fatalf("next reservation position = %d, want 64", next.position)
	}
	if _, err := producer.Commit(next, 129); err != nil {
		t.Fatal(err)
	}
	publish, ok = journal.region.snapshotPublish()
	if !ok || publish != (ringCursor{sequence: 3, position: 256}) {
		t.Fatalf("reused publish = %+v, ok=%v", publish, ok)
	}
}

func TestAbortReusesTentativeSequenceAndPosition(t *testing.T) {
	journal := newTestJournal(t, 1<<16, 4096)
	producer := newTestProducer(t, journal)
	first, err := producer.Reserve(128)
	if err != nil {
		t.Fatal(err)
	}
	if err := producer.Abort(first); err != nil {
		t.Fatal(err)
	}
	second, err := producer.Reserve(1)
	if err != nil {
		t.Fatal(err)
	}
	if second.sequence != first.sequence || second.position != first.position {
		t.Fatalf("second reservation = sequence %d position %d, want %d and %d", second.sequence, second.position, first.sequence, first.position)
	}
	if _, err := producer.Commit(second, 1); err != nil {
		t.Fatal(err)
	}
	if tag := orderedatomic.LoadAcquire64(journal.region.tag(64)); tag != WritingTag(1) {
		t.Fatalf("abandoned grain tag = %#x, want %#x", tag, WritingTag(1))
	}
}

func TestWrapUsesMirroredArena(t *testing.T) {
	journal := newTestJournal(t, 1<<14, 256)
	producer := newTestProducer(t, journal)
	producer.position = journal.Capacity() - 64
	producer.sequence = Sequence(producer.position/256 + 1)
	producer.durable = ringCursor{sequence: producer.sequence, position: producer.position}
	publishCursor(journal.region, JournalHeaderPublishLockOffset, JournalHeaderPublishSequenceOffset, JournalHeaderPublishPositionOffset, ringCursor{sequence: producer.sequence, position: producer.position})
	publishCursor(journal.region, JournalHeaderDurableLockOffset, JournalHeaderDurableSequenceOffset, JournalHeaderDurablePositionOffset, ringCursor{sequence: producer.sequence, position: producer.position})
	span, err := producer.Reserve(256)
	if err != nil {
		t.Fatal(err)
	}
	for index := range span.Bytes() {
		span.Bytes()[index] = byte(index)
	}
	if _, err := producer.Commit(span, 256); err != nil {
		t.Fatal(err)
	}
	for _, position := range []uint64{
		journal.Capacity() - 64,
		journal.Capacity(),
		journal.Capacity() + 64,
		journal.Capacity() + 128,
	} {
		if tag := orderedatomic.LoadAcquire64(journal.region.tag(position)); tag != StableTag(span.sequence) {
			t.Fatalf("wrapped tag at %d = %#x, want %#x", position, tag, StableTag(span.sequence))
		}
	}
	for _, position := range []uint64{
		journal.Capacity(),
		journal.Capacity() + 64,
		journal.Capacity() + 128,
	} {
		if descriptor := orderedatomic.LoadRelaxed64(journal.region.descriptor(position)); descriptor != 0 {
			t.Fatalf("wrapped continuation descriptor at %d = %#x, want zero", position, descriptor)
		}
	}
	for index := 0; index < 192; index++ {
		if journal.region.mirroredArena[index] != byte(index+64) {
			t.Fatalf("wrapped byte %d = %d, want %d", index, journal.region.mirroredArena[index], byte(index+64))
		}
	}
}

func TestCapacityUsesOnlyDurableCursor(t *testing.T) {
	journal := newTestJournal(t, 1<<14, 4096)
	producer := newTestProducer(t, journal)
	records := int((journal.Capacity() - JournalSentinelBytes) / 4096)
	for index := 0; index < records; index++ {
		span, err := producer.Reserve(4096)
		if err != nil {
			t.Fatalf("Reserve %d: %v", index, err)
		}
		if _, err := producer.Commit(span, 4096); err != nil {
			t.Fatalf("Commit %d: %v", index, err)
		}
	}
	if _, err := producer.Reserve(4096); !errors.Is(err, ErrFull) {
		t.Fatalf("Reserve at durable frontier = %v, want ErrFull", err)
	}
	publishCursor(journal.region, JournalHeaderDurableLockOffset, JournalHeaderDurableSequenceOffset, JournalHeaderDurablePositionOffset, ringCursor{sequence: 2, position: 4096})
	if _, err := producer.Reserve(4096); err != nil {
		t.Fatalf("Reserve after durable advance: %v", err)
	}
}

func TestReserveMarksPostAttachCursorCorruptionForRecovery(t *testing.T) {
	created := newTestJournal(t, 1<<16, 4096)
	fd, err := created.DupFD()
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fd)
	journal, err := Attach(fd)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := journal.Close(); closeErr != nil {
			t.Errorf("attached Close: %v", closeErr)
		}
	})
	producer := newTestProducer(t, journal)
	producer.position = journal.Capacity() - JournalSentinelBytes
	producer.sequence = Sequence((producer.position+4095)/4096 + 1)
	publishCursor(journal.region, JournalHeaderPublishLockOffset, JournalHeaderPublishSequenceOffset, JournalHeaderPublishPositionOffset, ringCursor{sequence: producer.sequence, position: producer.position})
	publishCursor(journal.region, JournalHeaderDurableLockOffset, JournalHeaderDurableSequenceOffset, JournalHeaderDurablePositionOffset, ringCursor{sequence: producer.sequence, position: 64})
	if _, err := producer.Reserve(1); !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("Reserve with malformed refreshed durable cursor = %v, want ErrRecoveryRequired", err)
	}
	if !journal.region.recoveryRequired() {
		t.Fatal("malformed refreshed durable cursor did not set shared recovery")
	}
}

func TestReserveMarksMalformedCachedCursorForRecovery(t *testing.T) {
	journal := newTestJournal(t, 1<<16, 4096)
	producer := newTestProducer(t, journal)
	producer.durable = ringCursor{sequence: 2, position: uint64(JournalGrain)}
	if _, err := producer.Reserve(1); !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("Reserve with malformed cached durable cursor = %v, want ErrRecoveryRequired", err)
	}
	if !journal.region.recoveryRequired() {
		t.Fatal("malformed cached durable cursor did not set shared recovery")
	}
}

func TestReservePersistentCursorFailureMarksRecovery(t *testing.T) {
	journal := newTestJournal(t, 1<<16, 4096)
	producer := newTestProducer(t, journal)
	producer.position = journal.Capacity() - JournalSentinelBytes
	producer.sequence = Sequence((producer.position+4095)/4096 + 1)
	publishCursor(journal.region, JournalHeaderPublishLockOffset, JournalHeaderPublishSequenceOffset, JournalHeaderPublishPositionOffset, ringCursor{sequence: producer.sequence, position: producer.position})
	orderedatomic.StoreRelease64(journal.region.ptr64(JournalHeaderDurableLockOffset), 1)
	if _, err := producer.Reserve(1); !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("Reserve with persistent odd durable lock = %v, want ErrRecoveryRequired", err)
	}
	if !journal.region.recoveryRequired() {
		t.Fatal("persistent cursor failure did not mark recovery")
	}
	orderedatomic.StoreRelease64(journal.region.ptr64(JournalHeaderDurableLockOffset), 2)
	if _, err := producer.Reserve(1); !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("Reserve after durable lock repair = %v, want ErrRecoveryRequired", err)
	}
	if !journal.region.recoveryRequired() {
		t.Fatal("ordinary cursor repair cleared shared recovery")
	}
}

func TestAttachProducerDoesNotLatchTransientCursorUpdate(t *testing.T) {
	journal := newTestJournal(t, 1<<16, 4096)
	archiverOwner := uint64(1)<<32 | uint64(uint32(journalCurrentThreadIdentity().ThreadId()))
	orderedatomic.StoreRelease64(journal.region.ptr64(JournalHeaderArchiverTIDOffset), archiverOwner)
	orderedatomic.StoreRelease64(journal.region.ptr64(JournalHeaderDurableLockOffset), 1)
	if producer, err := journal.AttachProducer(); producer != nil || !errors.Is(err, ErrBusy) {
		t.Fatalf("AttachProducer with live archiver update = (%v, %v), want nil ErrBusy", producer, err)
	}
	if journal.region.recoveryRequired() {
		t.Fatal("live archiver update set shared recovery")
	}
	orderedatomic.StoreRelease64(journal.region.ptr64(JournalHeaderDurableLockOffset), 2)
	orderedatomic.StoreRelease64(journal.region.ptr64(JournalHeaderArchiverTIDOffset), uint64(1)<<32)
	producer, err := journal.AttachProducer()
	if err != nil {
		t.Fatalf("AttachProducer after durable update: %v", err)
	}
	if err := producer.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestReserveUsesCachedDurableCursorOnFastPath(t *testing.T) {
	journal := newTestJournal(t, 1<<16, 4096)
	producer := newTestProducer(t, journal)
	publishCursor(journal.region, JournalHeaderDurableLockOffset, JournalHeaderDurableSequenceOffset, JournalHeaderDurablePositionOffset, ringCursor{sequence: producer.sequence, position: 64})
	span, err := producer.Reserve(1)
	if err != nil {
		t.Fatalf("Reserve with sufficient cached capacity: %v", err)
	}
	if err := producer.Abort(span); err != nil {
		t.Fatal(err)
	}
}

func TestReserveRejectsSequenceAndPositionExhaustion(t *testing.T) {
	journal := newTestJournal(t, 1<<16, 4096)
	producer := newTestProducer(t, journal)
	producer.sequence = JournalMaxSequence + 1
	if _, err := producer.Reserve(1); !errors.Is(err, ErrPositionExhausted) {
		t.Fatalf("sequence exhaustion = %v, want ErrPositionExhausted", err)
	}
	producer.sequence = 1
	producer.position = ^uint64(0) - 63
	publishCursor(journal.region, JournalHeaderDurableLockOffset, JournalHeaderDurableSequenceOffset, JournalHeaderDurablePositionOffset, ringCursor{sequence: 1, position: producer.position})
	if _, err := producer.Reserve(65); !errors.Is(err, ErrPositionExhausted) {
		t.Fatalf("position exhaustion = %v, want ErrPositionExhausted", err)
	}
}

func TestCopiedValuesAndSpanReuseFail(t *testing.T) {
	journal := newTestJournal(t, 1<<16, 4096)
	journalCopy := reflectJournalCopy(journal)
	if journalCopy.Capacity() != 0 {
		t.Fatalf("copied Journal Capacity = %d, want zero", journalCopy.Capacity())
	}
	if _, err := journalCopy.DupFD(); !errors.Is(err, ErrMisuse) {
		t.Fatalf("copied Journal DupFD = %v, want ErrMisuse", err)
	}
	producer := newTestProducer(t, journal)
	producerCopy := reflectJournalCopy(producer)
	if _, err := producerCopy.Reserve(1); !errors.Is(err, ErrMisuse) {
		t.Fatalf("copied Producer Reserve = %v, want ErrMisuse", err)
	}
	span, err := producer.Reserve(1)
	if err != nil {
		t.Fatal(err)
	}
	spanCopy := span
	if _, err := producer.Commit(span, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := producer.Commit(spanCopy, 1); !errors.Is(err, ErrMisuse) {
		t.Fatalf("reused span Commit = %v, want ErrMisuse", err)
	}
}

func TestWrongThreadOperationFails(t *testing.T) {
	journal := newTestJournal(t, 1<<16, 4096)
	producer := newTestProducer(t, journal)
	result := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		_, err := producer.Reserve(1)
		result <- err
	}()
	if err := <-result; !errors.Is(err, ErrMisuse) {
		t.Fatalf("wrong-thread Reserve = %v, want ErrMisuse", err)
	}
	span, err := producer.Reserve(1)
	if err != nil {
		t.Fatalf("owner Reserve after misuse: %v", err)
	}
	if err := producer.Abort(span); err != nil {
		t.Fatal(err)
	}
}

func TestReusedTIDInvalidatesLocalHandle(t *testing.T) {
	journal := newTestJournal(t, 1<<16, 4096)
	producer, err := journal.AttachProducer()
	if err != nil {
		t.Fatal(err)
	}
	originalReused := journalThreadIdentityReused
	originalAlive := journalThreadAliveProbe
	journalThreadIdentityReused = func(owner, current hostcpu.ThreadIdentity) bool { return true }
	journalThreadAliveProbe = func(tid hostcpu.ThreadId) bool { return true }
	t.Cleanup(func() {
		journalThreadIdentityReused = originalReused
		journalThreadAliveProbe = originalAlive
	})
	journal.life.Lock()
	journal.reapDeadProducerLocked()
	journal.life.Unlock()
	runtime.UnlockOSThread()
	if journal.producer != nil {
		t.Fatal("reaped producer remained attached")
	}
	owner := orderedatomic.LoadAcquire64(journal.region.ptr64(JournalHeaderProducerTIDOffset))
	wantOwner := uint64(producer.generation)<<32 | uint64(uint32(producer.tid))
	if owner != wantOwner {
		t.Fatalf("reaped owner = %#x, want dead-owner marker %#x", owner, wantOwner)
	}
	if !journal.region.recoveryRequired() {
		t.Fatal("reaped producer did not require recovery")
	}
	if producer.valid() {
		t.Fatal("recovered producer remained valid")
	}
}

func TestDeadOwnerTakeoverIncrementsGeneration(t *testing.T) {
	journal := newTestJournal(t, 1<<16, 4096)
	producer, err := journal.AttachProducer()
	if err != nil {
		t.Fatal(err)
	}
	originalProbe := journalThreadAliveProbe
	journalThreadAliveProbe = func(tid hostcpu.ThreadId) bool { return false }
	t.Cleanup(func() { journalThreadAliveProbe = originalProbe })
	generation, previousOwner, err := acquireProducerRole(journal.region, producer.tid)
	if err != nil {
		t.Fatalf("acquireProducerRole: %v", err)
	}
	if generation != producer.generation+1 {
		t.Fatalf("generation = %d, want %d", generation, producer.generation+1)
	}
	wantPrevious := uint64(producer.generation)<<32 | uint64(uint32(producer.tid))
	if previousOwner != wantPrevious {
		t.Fatalf("previous owner = %#x, want %#x", previousOwner, wantPrevious)
	}
	if _, err := producer.Reserve(1); !errors.Is(err, ErrRecovered) {
		t.Fatalf("stale Reserve = %v, want ErrRecovered", err)
	}
	if !releaseProducerRole(journal.region, producer.tid, generation) {
		t.Fatal("release takeover role failed")
	}
	if err := producer.Close(); !errors.Is(err, ErrRecovered) {
		t.Fatalf("stale Close = %v, want ErrRecovered", err)
	}
}

func TestProcfsValidationFailurePreventsCreate(t *testing.T) {
	originalProbe := journalProcfsValidationProbe
	journalProcfsValidationProbe = func() error { return hostcpu.ErrProcfsPIDNamespace }
	t.Cleanup(func() { journalProcfsValidationProbe = originalProbe })
	journal, err := Create(Config{Capacity: 1 << 16, MaxRecordBytes: 64, Backend: BackendMemfd})
	if journal != nil || !errors.Is(err, hostcpu.ErrProcfsPIDNamespace) {
		t.Fatalf("Create = (%v, %v), want nil procfs error", journal, err)
	}
}

func TestDeadProducerRequiresArchiveBackedStableTailRecovery(t *testing.T) {
	journal := newTestJournal(t, 1<<16, 4096)
	first, err := journal.AttachProducer()
	if err != nil {
		t.Fatal(err)
	}
	if sequence, err := first.Write([]byte("committed")); err != nil || sequence != 1 {
		t.Fatalf("Write = (%d, %v), want (1, nil)", sequence, err)
	}
	publishCursor(journal.region, JournalHeaderPublishLockOffset, JournalHeaderPublishSequenceOffset, JournalHeaderPublishPositionOffset, ringCursor{sequence: 1})
	originalAlive := journalThreadAliveProbe
	journalThreadAliveProbe = func(hostcpu.ThreadId) bool { return false }
	t.Cleanup(func() { journalThreadAliveProbe = originalAlive })
	second, err := journal.AttachProducer()
	if second != nil || !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("AttachProducer after dead owner = (%v, %v), want nil ErrRecoveryRequired", second, err)
	}
	runtime.UnlockOSThread()
	publish, ok := journal.region.snapshotPublish()
	if !ok || publish != (ringCursor{sequence: 1}) {
		t.Fatalf("publish changed before archive recovery = %+v, ok=%v", publish, ok)
	}
	owner := orderedatomic.LoadAcquire64(journal.region.ptr64(JournalHeaderProducerTIDOffset))
	if uint32(owner) == 0 {
		t.Fatal("dead producer marker was cleared before recovery")
	}
}

func TestReleasedRoleIncrementsNextGeneration(t *testing.T) {
	journal := newTestJournal(t, 1<<16, 4096)
	first := newTestProducer(t, journal)
	generation := first.generation
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := journal.AttachProducer()
	if err != nil {
		t.Fatal(err)
	}
	if second.generation != generation+1 {
		t.Fatalf("generation = %d, want %d", second.generation, generation+1)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestWrongThreadMisusePrecedesRecoveryState(t *testing.T) {
	journal := newTestJournal(t, 1<<16, 4096)
	producer := newTestProducer(t, journal)
	journal.region.setRecoveryRequired()
	result := make(chan error, 1)
	go func() {
		_, err := producer.Reserve(1)
		result <- err
	}()
	if err := <-result; !errors.Is(err, ErrMisuse) {
		t.Fatalf("wrong-thread Reserve during recovery = %v, want ErrMisuse", err)
	}
	journal.region.clearRecoveryRequired()
}

func TestGenerationChangeInvalidatesProducer(t *testing.T) {
	journal := newTestJournal(t, 1<<16, 4096)
	producer := newTestProducer(t, journal)
	owner := journal.region.ptr64(JournalHeaderProducerTIDOffset)
	orderedatomic.StoreRelease64(owner, uint64(producer.generation+1)<<32|uint64(uint32(producer.tid)))
	if _, err := producer.Reserve(1); !errors.Is(err, ErrRecovered) {
		t.Fatalf("Reserve after generation change = %v, want ErrRecovered", err)
	}
	if err := producer.Close(); !errors.Is(err, ErrRecovered) {
		t.Fatalf("Close after generation change = %v, want ErrRecovered", err)
	}
}

func TestAttachRejectsWrongBackingSize(t *testing.T) {
	journal, err := Create(Config{
		Capacity:           1 << 16,
		MaxRecordBytes:     4096,
		Sizing:             journalTestSizing(4096),
		Backend:            BackendFile,
		Name:               t.TempDir() + "/journal",
		DisablePreallocate: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	fd, err := journal.DupFD()
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fd)
	layout, err := journalLayout(journal.Capacity())
	if err != nil {
		t.Fatal(err)
	}
	originalSize := int64(layout.ControlBytes + layout.ArenaBytes)
	if err := syscall.Ftruncate(fd, originalSize+int64(os.Getpagesize())); err != nil {
		t.Fatal(err)
	}
	defer syscall.Ftruncate(fd, originalSize)
	if _, err := Attach(fd); !errors.Is(err, ErrFormat) {
		t.Fatalf("Attach oversized backing = %v, want ErrFormat", err)
	}
}

func TestAttachRejectsFormatV4AndReservedBytes(t *testing.T) {
	journal := newTestJournal(t, 1<<16, 4096)
	fd, err := journal.DupFD()
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fd)
	originalMagic := get64(journal.region.control, JournalHeaderMagicOffset)
	put64(journal.region.control, JournalHeaderMagicOffset, 0x7067745f6d707363)
	put32(journal.region.control, JournalHeaderVersionOffset, 4)
	if _, err := Attach(fd); !errors.Is(err, ErrFormat) {
		t.Fatalf("Attach format v4 = %v, want ErrFormat", err)
	}
	put64(journal.region.control, JournalHeaderMagicOffset, originalMagic)
	put32(journal.region.control, JournalHeaderVersionOffset, JournalFormatVersion)
	journal.region.control[JournalHeaderReservedOffset] = 1
	if _, err := Attach(fd); !errors.Is(err, ErrFormat) {
		t.Fatalf("Attach reserved byte = %v, want ErrFormat", err)
	}
	journal.region.control[JournalHeaderReservedOffset] = 0
}

func TestAttachRejectsNonzeroLayoutPadding(t *testing.T) {
	journal := newTestJournal(t, 1<<16, 4096)
	metadataBytes := journal.Capacity() / uint64(JournalGrain) *
		uint64(JournalTagSize+JournalDescriptorSize)
	paddingStart := uint64(JournalHeaderSize) + metadataBytes
	if paddingStart >= uint64(len(journal.region.control)) {
		t.Fatal("test journal has no layout padding")
	}
	journal.region.control[paddingStart] = 1
	fd, err := journal.DupFD()
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fd)
	if _, err := Attach(fd); !errors.Is(err, ErrFormat) {
		t.Fatalf("Attach nonzero layout padding = %v, want ErrFormat", err)
	}
	journal.region.control[paddingStart] = 0
}

func TestAttachValidatesSharedOwnerAfterMapping(t *testing.T) {
	journal := newTestJournal(t, 1<<16, 4096)
	orderedatomic.StoreRelease64(journal.region.ptr64(JournalHeaderProducerTIDOffset), 42)
	fd, err := journal.DupFD()
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fd)
	if _, err := Attach(fd); !errors.Is(err, ErrFormat) {
		t.Fatalf("Attach invalid shared owner = %v, want ErrFormat", err)
	}
	orderedatomic.StoreRelease64(journal.region.ptr64(JournalHeaderProducerTIDOffset), 0)
}

func TestJournalCrossProcessVisibility(t *testing.T) {
	if os.Getenv("PGT_JOURNAL_CHILD") == "1" {
		fd := 3
		journal, err := Attach(fd)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Attach: %v\n", err)
			os.Exit(2)
		}
		defer journal.Close()
		publish, ok := journal.region.snapshotPublish()
		if !ok || publish.sequence != 2 || string(journal.region.payload(0, 5)) != "child" {
			fmt.Fprintf(os.Stderr, "publish=%+v ok=%v payload=%q\n", publish, ok, journal.region.payload(0, 5))
			os.Exit(3)
		}
		os.Exit(0)
	}
	journal := newTestJournal(t, 1<<16, 4096)
	producer := newTestProducer(t, journal)
	if _, err := producer.Write([]byte("child")); err != nil {
		t.Fatal(err)
	}
	fd, err := journal.DupFD()
	if err != nil {
		t.Fatal(err)
	}
	file := os.NewFile(uintptr(fd), "journal")
	defer file.Close()
	command := exec.Command(os.Args[0], "-test.run=^TestJournalCrossProcessVisibility$")
	command.Env = append(os.Environ(), "PGT_JOURNAL_CHILD=1")
	command.ExtraFiles = []*os.File{file}
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("child: %v\n%s", err, output)
	}
}

func TestWriteChecksIdentityOnceAndMatchesReserveCommit(t *testing.T) {
	journal := newTestJournal(t, 1<<16, 64)
	producer := newTestProducer(t, journal)
	originalIdentity := journalCurrentThreadIdentity
	t.Cleanup(func() { journalCurrentThreadIdentity = originalIdentity })
	calls := 0
	journalCurrentThreadIdentity = func() hostcpu.ThreadIdentity {
		calls++
		return originalIdentity()
	}
	sequence, err := producer.Write([]byte("direct"))
	journalCurrentThreadIdentity = originalIdentity
	if err != nil || sequence != 1 {
		t.Fatalf("Write = (%d, %v), want (1, nil)", sequence, err)
	}
	if calls != 1 {
		t.Fatalf("Write identity checks = %d, want 1", calls)
	}
	publish, ok := journal.region.snapshotPublish()
	if !ok {
		t.Fatal("publish cursor is unstable")
	}
	record, found, err := journal.region.recordAt(ringCursor{sequence: 1}, publish)
	if err != nil || !found || string(record.bytes) != "direct" || record.extent != uint64(JournalGrain) {
		t.Fatalf("direct record = %+v, found=%v, err=%v", record, found, err)
	}
}

func TestProducerHotPathsAllocateZero(t *testing.T) {
	journal := newTestJournal(t, 1<<16, 64)
	producer := newTestProducer(t, journal)
	allocations := testing.AllocsPerRun(100, func() {
		span, err := producer.Reserve(1)
		if err != nil {
			panic(err)
		}
		span.Bytes()[0] = 7
		if _, err := producer.Commit(span, 1); err != nil {
			panic(err)
		}
		publishCursor(journal.region, JournalHeaderDurableLockOffset, JournalHeaderDurableSequenceOffset, JournalHeaderDurablePositionOffset, ringCursor{sequence: producer.sequence, position: producer.position})
	})
	if allocations != 0 {
		t.Fatalf("Reserve/Commit allocations = %f, want zero", allocations)
	}
	allocations = testing.AllocsPerRun(100, func() {
		span, err := producer.Reserve(1)
		if err != nil {
			panic(err)
		}
		if err := producer.Abort(span); err != nil {
			panic(err)
		}
	})
	if allocations != 0 {
		t.Fatalf("Reserve/Abort allocations = %f, want zero", allocations)
	}
	payload := []byte{9}
	allocations = testing.AllocsPerRun(100, func() {
		if _, err := producer.Write(payload); err != nil {
			panic(err)
		}
		publishCursor(journal.region, JournalHeaderDurableLockOffset, JournalHeaderDurableSequenceOffset, JournalHeaderDurablePositionOffset, ringCursor{sequence: producer.sequence, position: producer.position})
	})
	if allocations != 0 {
		t.Fatalf("Write allocations = %f, want zero", allocations)
	}
}
