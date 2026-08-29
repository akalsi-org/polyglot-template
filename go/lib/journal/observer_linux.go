//go:build linux && (amd64 || arm64)

package journal

import (
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"

	"github.com/akalsi-org/polyglot-template/go/lib/internal/orderedatomic"
)

// ArchivedRecord is one decompressed archive payload.
// Bytes aliases caller-provided storage.
type ArchivedRecord struct {
	Sequence Sequence
	Batch    BatchID
	Bytes    []byte
}

// LiveView is a zero-allocation live-ring borrow.
// UnsafeBytes remains valid until Advance, failed Validate, Resync, or Close.
type LiveView struct {
	observer    *Observer
	token       uint64
	sequence    Sequence
	position    uint64
	extent      uint64
	expectedTag uint64
	bytes       []byte
	validated   bool
}

// Observer replays durable batches and inspects live ring records.
// Do not copy Observer values. OpenObserver does not pin the OS thread.
type Observer struct {
	_            noCopy
	self         *Observer
	journal      *Journal
	region       *ringRegion
	ops          archiveFileOps
	directory    string
	epoch        RingEpoch
	state        ObserverState
	next         ringCursor
	batch        BatchID
	batchFile    archiveFile
	batchMeta    archiveBatchMeta
	pending      []byte
	pendingSeq   Sequence
	pendingBatch BatchID
	liveToken    uint64
	live         LiveView
	closed       atomic.Bool
}

// Sequence returns the live record sequence.
func (view LiveView) Sequence() Sequence { return view.sequence }

// UnsafeBytes returns mapped live payload bytes.
func (view LiveView) UnsafeBytes() []byte { return view.bytes }

// OpenObserver creates a passive observer.
// It can block on the shared archive lock and batch open I/O.
func (journal *Journal) OpenObserver(config ObserverConfig) (*Observer, error) {
	if !journal.valid() {
		return nil, ErrMisuse
	}
	if journal.closing || journal.region == nil {
		return nil, ErrClosed
	}
	directory, err := canonicalArchiveDirectory(config.ArchiveDirectory)
	if err != nil {
		return nil, err
	}
	observer := &Observer{
		journal:   journal,
		region:    journal.region,
		ops:       journalArchiveFileOps,
		directory: directory,
		epoch:     journal.region.ringEpoch(),
	}
	observer.self = observer
	if err := observer.openStart(config.Start); err != nil {
		_ = observer.Close()
		return nil, err
	}
	return observer, nil
}

func (observer *Observer) valid() bool { return observer != nil && observer.self == observer }

// State returns the current observer state.
func (observer *Observer) State() ObserverState {
	if !observer.valid() || observer.closed.Load() {
		return ObserverClosed
	}
	return observer.state
}

func (observer *Observer) openStart(start ObserverStart) error {
	lock, err := openArchiveLock(observer.ops, observer.directory)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := lockArchiveShared(observer.ops, lock); err != nil {
		return err
	}
	defer unlockArchive(observer.ops, lock)
	floor, floorPresent, err := readRetentionFloor(observer.directory, observer.epoch)
	if err != nil {
		return err
	}
	head, headPresent, err := readArchiveHead(observer.directory, observer.epoch)
	if err != nil {
		return err
	}
	if start.Batch == nil {
		if headPresent {
			return ErrBatchBoundary
		}
		durable, ok := observer.region.snapshotDurable()
		if !ok || !validCursorShape(durable) {
			return ErrRecoveryRequired
		}
		observer.next = durable
		observer.state = ObserverLive
		return nil
	}
	requested := *start.Batch
	if requested == 0 {
		return ErrBatchBoundary
	}
	newest := requested
	if headPresent {
		newest = head.latest
	}
	if floorPresent && requested < floor {
		return &NotRetainedError{Requested: requested, OldestRetained: floor, NewestDurable: newest}
	}
	return observer.openBatchLocked(requested)
}

func (observer *Observer) openBatchLocked(id BatchID) error {
	name, err := batchFilename(Sequence(id))
	if err != nil {
		return err
	}
	path := filepath.Join(observer.directory, name)
	file, err := observer.ops.openFile(path, os.O_RDONLY, 0)
	if errors.Is(err, os.ErrNotExist) {
		floor, floorPresent, floorErr := readRetentionFloor(observer.directory, observer.epoch)
		if floorErr != nil {
			return floorErr
		}
		head, headPresent, headErr := readArchiveHead(observer.directory, observer.epoch)
		if headErr != nil {
			return headErr
		}
		newest := id
		if headPresent {
			newest = head.latest
		}
		if floorPresent && id < floor {
			return &NotRetainedError{Requested: id, OldestRetained: floor, NewestDurable: newest}
		}
		return ErrBatchBoundary
	}
	if err != nil {
		return err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return err
	}
	meta, err := parseArchiveBatchReaderBounded(file, uint64(info.Size()), name, observer.epoch, ^uint64(0))
	if err != nil {
		_ = file.Close()
		return archiveCorrupt(path, err)
	}
	observer.closeBatch()
	observer.batchFile = file
	observer.batchMeta = meta
	observer.batch = meta.id
	observer.next = meta.first
	observer.state = ObserverReplay
	observer.pending = nil
	return nil
}

func (observer *Observer) closeBatch() {
	if observer.batchFile != nil {
		_ = observer.batchFile.Close()
		observer.batchFile = nil
	}
	observer.batchMeta = archiveBatchMeta{}
	observer.pending = nil
}

func (observer *Observer) checkRead() error {
	if !observer.valid() {
		return ErrMisuse
	}
	if observer.closed.Load() || observer.journal == nil || observer.region == nil || observer.journal.closing {
		return ErrClosed
	}
	if observer.state == ObserverResyncRequired {
		return ErrResyncRequired
	}
	return nil
}

func (observer *Observer) requireResync(err error) error {
	observer.state = ObserverResyncRequired
	observer.invalidateLive()
	observer.closeBatch()
	if err == nil {
		return ErrOverwritten
	}
	return err
}

func (observer *Observer) invalidateLive() {
	observer.liveToken++
	if observer.liveToken == 0 {
		observer.liveToken++
	}
	observer.live = LiveView{}
}

// ReplayNext copies the next archived payload into dst.
func (observer *Observer) ReplayNext(dst []byte) (ArchivedRecord, bool, error) {
	if err := observer.checkRead(); err != nil {
		return ArchivedRecord{}, false, err
	}
	if observer.state != ObserverReplay {
		return ArchivedRecord{}, false, nil
	}
	if observer.pending != nil {
		if uint64(len(dst)) < uint64(len(observer.pending)) {
			return ArchivedRecord{}, false, &BufferTooSmallError{Required: uint64(len(observer.pending))}
		}
		n := copy(dst, observer.pending)
		record := ArchivedRecord{Sequence: observer.pendingSeq, Batch: observer.pendingBatch, Bytes: dst[:n]}
		observer.pending = nil
		observer.next.sequence++
		if observer.next.sequence == observer.batchMeta.next.sequence {
			observer.next = observer.batchMeta.next
			if err := observer.advanceAfterBatch(); err != nil {
				return ArchivedRecord{}, false, err
			}
		}
		return record, true, nil
	}
	var found bool
	var payload []byte
	err := forEachArchiveRecord(observer.batchFile, observer.batchMeta, observer.next.sequence, func(sequence Sequence, bytes []byte) error {
		if found {
			return errStopReplay
		}
		if sequence != observer.next.sequence {
			return ErrFormat
		}
		copied := append([]byte(nil), bytes...)
		payload = copied
		observer.pendingSeq = sequence
		observer.pendingBatch = observer.batch
		found = true
		return errStopReplay
	})
	if err != nil && !errors.Is(err, errStopReplay) {
		return ArchivedRecord{}, false, err
	}
	if !found {
		if err := observer.advanceAfterBatch(); err != nil {
			return ArchivedRecord{}, false, err
		}
		return ArchivedRecord{}, false, nil
	}
	observer.pending = payload
	if uint64(len(dst)) < uint64(len(payload)) {
		return ArchivedRecord{}, false, &BufferTooSmallError{Required: uint64(len(payload))}
	}
	n := copy(dst, payload)
	observer.pending = nil
	observer.next.sequence++
	if observer.next.sequence == observer.batchMeta.next.sequence {
		observer.next = observer.batchMeta.next
		if err := observer.advanceAfterBatch(); err != nil {
			return ArchivedRecord{}, false, err
		}
	}
	return ArchivedRecord{Sequence: observer.pendingSeq, Batch: observer.pendingBatch, Bytes: dst[:n]}, true, nil
}

var errStopReplay = errors.New("journal: stop archive replay")

func (observer *Observer) advanceAfterBatch() error {
	nextID := BatchID(observer.next.sequence)
	name, err := batchFilename(observer.next.sequence)
	if err != nil {
		return err
	}
	path := filepath.Join(observer.directory, name)
	lock, err := openArchiveLock(observer.ops, observer.directory)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := lockArchiveShared(observer.ops, lock); err != nil {
		return err
	}
	file, openErr := observer.ops.openFile(path, os.O_RDONLY, 0)
	unlockErr := unlockArchive(observer.ops, lock)
	if unlockErr != nil {
		if openErr == nil {
			_ = file.Close()
		}
		return unlockErr
	}
	if openErr == nil {
		_ = file.Close()
		return observer.openBatchLocked(nextID)
	}
	if !errors.Is(openErr, os.ErrNotExist) {
		return openErr
	}
	observer.closeBatch()
	observer.state = ObserverHandoff
	return nil
}

// NextLive returns the next live ring record when the observer has caught the durable frontier.
func (observer *Observer) NextLive() (LiveView, bool, error) {
	if err := observer.checkRead(); err != nil {
		return LiveView{}, false, err
	}
	if observer.state == ObserverReplay {
		return LiveView{}, false, nil
	}
	durable, durableOK := observer.region.snapshotDurable()
	publish, publishOK := observer.region.snapshotPublish()
	if !durableOK || !publishOK {
		return LiveView{}, false, ErrBusy
	}
	if observer.next.sequence < durable.sequence {
		record, stable, err := observer.region.readStableRecord(observer.next)
		if err != nil {
			return LiveView{}, false, observer.requireResync(err)
		}
		if !stable {
			return LiveView{}, false, observer.requireResync(ErrOverwritten)
		}
		return observer.liveView(record), true, nil
	}
	if observer.next.sequence == publish.sequence {
		observer.state = ObserverLive
		return LiveView{}, false, nil
	}
	if observer.next.sequence > publish.sequence {
		return LiveView{}, false, observer.requireResync(ErrFormat)
	}
	record, stable, err := observer.region.readStableRecord(observer.next)
	if err != nil {
		return LiveView{}, false, observer.requireResync(err)
	}
	if !stable {
		return LiveView{}, false, observer.requireResync(ErrOverwritten)
	}
	observer.state = ObserverLive
	return observer.liveView(record), true, nil
}

func (observer *Observer) liveView(record ringRecord) LiveView {
	observer.invalidateLive()
	view := LiveView{
		observer:    observer,
		token:       observer.liveToken,
		sequence:    record.cursor.sequence,
		position:    record.cursor.position,
		extent:      record.extent,
		expectedTag: StableTag(record.cursor.sequence),
		bytes:       record.bytes,
	}
	observer.live = view
	return view
}

// Validate confirms the borrowed live bytes were not overwritten.
func (view LiveView) Validate() error {
	if view.observer == nil || !view.observer.valid() || view.observer.closed.Load() {
		return ErrMisuse
	}
	if view.token != view.observer.liveToken || view.observer.live.token != view.token {
		return ErrMisuse
	}
	orderedatomic.LoadBarrier()
	grains := view.extent / uint64(JournalGrain)
	for grain := uint64(0); grain < grains; grain++ {
		if orderedatomic.LoadAcquire64(view.observer.region.tag(view.position+grain*uint64(JournalGrain))) != view.expectedTag {
			return view.observer.requireResync(ErrOverwritten)
		}
	}
	view.observer.live.validated = true
	return nil
}

// Advance consumes a validated live view.
func (observer *Observer) Advance(view LiveView) error {
	if err := observer.checkRead(); err != nil {
		if observer != nil && observer.state == ObserverResyncRequired {
			return err
		}
		return err
	}
	if view.observer != observer || view.token != observer.liveToken || !observer.live.validated || view.sequence != observer.next.sequence {
		return ErrMisuse
	}
	observer.next.sequence = view.sequence + 1
	observer.next.position = view.position + view.extent
	observer.invalidateLive()
	return nil
}

// Resync restarts replay from the retained batch that contains the expected sequence.
func (observer *Observer) Resync() error {
	if !observer.valid() {
		return ErrMisuse
	}
	if observer.closed.Load() {
		return ErrClosed
	}
	if observer.state != ObserverResyncRequired {
		return ErrMisuse
	}
	lock, err := openArchiveLock(observer.ops, observer.directory)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := lockArchiveShared(observer.ops, lock); err != nil {
		return err
	}
	id, err := observer.findBatchContaining(observer.next.sequence)
	unlockErr := unlockArchive(observer.ops, lock)
	if err != nil {
		return err
	}
	if unlockErr != nil {
		return unlockErr
	}
	expected := observer.next.sequence
	if err := observer.openBatchLocked(id); err != nil {
		return err
	}
	if expected < observer.batchMeta.first.sequence || expected > observer.batchMeta.next.sequence {
		return ErrFormat
	}
	observer.next.sequence = expected
	observer.state = ObserverReplay
	observer.pending = nil
	observer.invalidateLive()
	return nil
}

func (observer *Observer) findBatchContaining(sequence Sequence) (BatchID, error) {
	floor, floorPresent, err := readRetentionFloor(observer.directory, observer.epoch)
	if err != nil {
		return 0, err
	}
	head, headPresent, err := readArchiveHead(observer.directory, observer.epoch)
	if err != nil {
		return 0, err
	}
	newest := BatchID(sequence)
	if headPresent {
		newest = head.latest
	}
	if floorPresent && sequence < Sequence(floor) {
		return 0, &NotRetainedError{Requested: BatchID(sequence), OldestRetained: floor, NewestDurable: newest}
	}
	entries, err := observer.ops.readDir(observer.directory)
	if err != nil {
		return 0, err
	}
	var match BatchID
	found := false
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		id, ok := parseBatchFilename(entry.Name())
		if !ok {
			continue
		}
		if sequence < id {
			continue
		}
		path := filepath.Join(observer.directory, entry.Name())
		meta, err := readArchiveBatch(path, observer.epoch)
		if err != nil {
			continue
		}
		if sequence >= meta.first.sequence && sequence < meta.next.sequence {
			match = meta.id
			found = true
			break
		}
	}
	if !found {
		if floorPresent {
			return 0, &NotRetainedError{Requested: BatchID(sequence), OldestRetained: floor, NewestDurable: newest}
		}
		return 0, ErrArchiveGap
	}
	return match, nil
}

// Close releases observer batch files.
func (observer *Observer) Close() error {
	if observer == nil {
		return nil
	}
	if !observer.valid() {
		return ErrMisuse
	}
	if observer.closed.Load() {
		return nil
	}
	observer.closed.Store(true)
	observer.state = ObserverClosed
	observer.invalidateLive()
	observer.closeBatch()
	observer.self = nil
	return nil
}
