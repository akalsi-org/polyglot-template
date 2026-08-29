//go:build linux && (amd64 || arm64)

package journal

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/akalsi-org/polyglot-template/go/lib/hostcpu"
	"github.com/akalsi-org/polyglot-template/go/lib/internal/orderedatomic"
)

// Archiver is the single OS-thread-bound durable archive writer.
// Use NewArchiver to create an Archiver. Do not copy an Archiver value.
type Archiver struct {
	_          noCopy
	self       *Archiver
	journal    *Journal
	region     *ringRegion
	identity   hostcpu.ThreadIdentity
	tid        hostcpu.ThreadId
	generation uint32
	config     ArchiveConfig
	lock       archiveFile
	ops        archiveFileOps
	reclaim    ringCursor
	scan       ringCursor
	proof      durableProof
	records    []archiveRecord
	payload    []byte
	occupied   uint64
	started    time.Time
	scratch    archiveBatchScratch
	closed     atomic.Bool
}

// NewArchiver validates the persisted sizing contract.
// It completes archive and cursor recovery before it returns a live Archiver.
// It then acquires the archiver role and locks the goroutine to its OS thread.
// NewArchiver can block on the archive lock and recovery I/O.
func (journal *Journal) NewArchiver(config ArchiveConfig) (*Archiver, error) {
	if !journal.valid() {
		return nil, ErrMisuse
	}
	if config.Directory == "" {
		return nil, fmt.Errorf("%w: archive directory", ErrFormat)
	}
	directory, err := canonicalArchiveDirectory(config.Directory)
	if err != nil {
		return nil, err
	}
	config.Directory = directory
	journal.life.Lock()
	journal.reapDeadArchiverLocked()
	if journal.closing || journal.region == nil {
		journal.life.Unlock()
		return nil, ErrClosed
	}
	if journal.archiverAttaching || journal.archiver != nil {
		journal.life.Unlock()
		return nil, ErrBusy
	}
	if err := validateArchiveSizing(journal.region, config.Sizing); err != nil {
		journal.life.Unlock()
		return nil, err
	}
	journal.archiverAttaching = true
	journal.life.Unlock()
	attaching := true
	defer func() {
		if attaching {
			journal.life.Lock()
			journal.archiverAttaching = false
			journal.life.Unlock()
		}
	}()

	ops := journalArchiveFileOps
	if err := createArchiveDirectory(ops, directory, 0o700); err != nil {
		return nil, err
	}
	lock, err := openArchiveLock(ops, directory)
	if err != nil {
		return nil, err
	}
	closeLock := true
	defer func() {
		if closeLock {
			_ = lock.Close()
		}
	}()

	runtime.LockOSThread()
	unlockThread := true
	defer func() {
		if unlockThread {
			runtime.UnlockOSThread()
		}
	}()
	identity := journalCurrentThreadIdentity()
	if err := lockArchiveExclusive(ops, lock); err != nil {
		return nil, err
	}
	recoveryErr := recoverArchive(journal, config, ops)
	unlockErr := unlockArchive(ops, lock)
	if recoveryErr != nil || unlockErr != nil {
		return nil, errors.Join(recoveryErr, unlockErr)
	}

	journal.life.Lock()
	defer journal.life.Unlock()
	journal.reapDeadProducerLocked()
	journal.reapDeadArchiverLocked()
	if journal.closing || journal.region == nil {
		return nil, ErrClosed
	}
	if journal.archiver != nil {
		return nil, ErrBusy
	}
	if journal.needsRecoveryLocked() {
		return nil, ErrRecoveryRequired
	}
	if err := validateArchiveSizing(journal.region, config.Sizing); err != nil {
		return nil, err
	}
	tid := identity.ThreadId()
	generation, previousOwner, err := acquireArchiverRole(journal.region, tid)
	if err != nil {
		return nil, err
	}
	if uint32(previousOwner) != 0 {
		restoreArchiverRole(journal.region, tid, generation, previousOwner)
		journal.region.setRecoveryRequired()
		return nil, ErrRecoveryRequired
	}
	if journal.region.recoveryRequired() {
		releaseArchiverRole(journal.region, tid, generation)
		return nil, ErrRecoveryRequired
	}
	durable, ok := journal.region.snapshotDurable()
	if !ok || !validCursorShape(durable) {
		releaseArchiverRole(journal.region, tid, generation)
		journal.region.setRecoveryRequired()
		return nil, ErrRecoveryRequired
	}
	initialPayloadBytes := min(
		config.Sizing.BatchMaxOccupiedBytes,
		config.Sizing.ZstdBlockBytes,
		uint64(64<<10),
	)
	initialRecords := initialPayloadBytes / uint64(JournalGrain)
	archiver := &Archiver{
		journal: journal, region: journal.region, identity: identity, tid: tid,
		generation: generation, config: config, lock: lock, ops: ops,
		reclaim: durable, scan: durable,
		records: make([]archiveRecord, 0, int(initialRecords)),
		payload: make([]byte, 0, int(initialPayloadBytes)),
	}
	archiver.self = archiver
	journal.archiver = archiver
	journal.archiverAttaching = false
	attaching = false
	closeLock = false
	unlockThread = false
	return archiver, nil
}

func validateArchiveSizing(region *ringRegion, sizing SizingConfig) error {
	if region == nil || !region.hasValidSizingContract() {
		return ErrFormat
	}
	required, err := RequiredCapacity(region.maxRecord, sizing)
	if err != nil {
		return err
	}
	storedRequired := get64(region.control, JournalHeaderRequiredCapacityOffset)
	storedDigest := region.control[JournalHeaderSizingDigestOffset : JournalHeaderSizingDigestOffset+SizingConfigDigestSize]
	digest := sizingConfigDigest(region.maxRecord, required, sizing)
	if storedRequired != required || !equalBytes(storedDigest, digest[:]) {
		failure := convergenceFailure(ConvergenceInvalidBound, region.maxRecord, 0, sizing, errors.New("archive sizing digest differs from journal"))
		failure.RequiredCapacity = required
		return failure
	}
	return nil
}

func equalBytes(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	var difference byte
	for index := range left {
		difference |= left[index] ^ right[index]
	}
	return difference == 0
}

func acquireArchiverRole(region *ringRegion, tid hostcpu.ThreadId) (uint32, uint64, error) {
	return acquireSharedRole(region, JournalHeaderArchiverTIDOffset, tid)
}

func releaseArchiverRole(region *ringRegion, tid hostcpu.ThreadId, generation uint32) bool {
	return releaseSharedRole(region, JournalHeaderArchiverTIDOffset, tid, generation)
}

func restoreArchiverRole(region *ringRegion, tid hostcpu.ThreadId, generation uint32, previous uint64) bool {
	return restoreSharedRole(region, JournalHeaderArchiverTIDOffset, tid, generation, previous)
}

func (archiver *Archiver) valid() bool { return archiver != nil && archiver.self == archiver }

func (archiver *Archiver) ownsRole() bool {
	if archiver.region == nil {
		return false
	}
	want := uint64(archiver.generation)<<32 | uint64(uint32(archiver.tid))
	return orderedatomic.LoadAcquire64(archiver.region.ptr64(JournalHeaderArchiverTIDOffset)) == want
}

func (archiver *Archiver) checkOperation() error {
	if !archiver.valid() {
		return ErrMisuse
	}
	if archiver.closed.Load() || archiver.region == nil {
		return ErrClosed
	}
	if journalCurrentThreadIdentity() != archiver.identity {
		return ErrMisuse
	}
	if archiver.region.recoveryRequired() {
		return ErrRecoveryRequired
	}
	if !archiver.ownsRole() {
		return ErrRecovered
	}
	return nil
}

// DrainOnce copies committed records into one private pending batch.
// It commits the batch only when the occupied-byte or age condition becomes true.
// The Boolean result reports whether this call committed one durability batch.
// DrainOnce can block on the archive lock, compression, fdatasync, rename, and directory fsync.
func (archiver *Archiver) DrainOnce(now time.Time) (ArchiveProgress, bool, error) {
	if err := archiver.checkOperation(); err != nil {
		return ArchiveProgress{}, false, err
	}
	if now.IsZero() {
		return ArchiveProgress{}, false, fmt.Errorf("%w: archive time", ErrFormat)
	}
	if err := lockArchiveExclusive(archiver.ops, archiver.lock); err != nil {
		archiver.resetPending()
		return ArchiveProgress{}, false, err
	}
	progress, committed, operationErr := archiver.drainLocked(now)
	unlockErr := unlockArchive(archiver.ops, archiver.lock)
	if operationErr != nil {
		archiver.resetPending()
	}
	if operationErr != nil || unlockErr != nil {
		return progress, committed, errors.Join(operationErr, unlockErr)
	}
	return progress, committed, nil
}

func (archiver *Archiver) drainLocked(now time.Time) (ArchiveProgress, bool, error) {
	durable, durableOK := archiver.region.snapshotDurable()
	publish, publishOK := archiver.region.snapshotPublish()
	if !durableOK || !publishOK || !archiver.region.validCursorPair(durable, publish) {
		archiver.region.setRecoveryRequired()
		return ArchiveProgress{}, false, ErrRecoveryRequired
	}
	if durable != archiver.reclaim {
		if len(archiver.records) != 0 || archiver.scan != archiver.reclaim {
			return ArchiveProgress{}, false, ErrRecovered
		}
		archiver.reclaim = durable
		archiver.scan = durable
	}
	if len(archiver.records) == 0 {
		if progress, adopted, err := archiver.adoptOwnedOrphanLocked(durable, publish); err != nil || adopted {
			return progress, adopted, err
		}
		if durable == publish {
			return ArchiveProgress{}, false, nil
		}
		proof, err := archiver.region.beginDurableProof(durable, publish)
		if err != nil {
			return ArchiveProgress{}, false, err
		}
		archiver.proof = proof
	} else {
		if archiver.proof.from != durable || archiver.proof.to != archiver.scan {
			return ArchiveProgress{}, false, ErrRecovered
		}
		archiver.proof.publish = publish
	}

	cutByBytes := false
	for archiver.scan != publish {
		record, present, err := archiver.region.recordAt(archiver.scan, publish)
		if err != nil {
			return ArchiveProgress{}, false, err
		}
		if !present {
			break
		}
		if record.extent > archiver.config.Sizing.BatchMaxOccupiedBytes {
			return ArchiveProgress{}, false, ErrTooLarge
		}
		if archiver.occupied > archiver.config.Sizing.BatchMaxOccupiedBytes-record.extent {
			cutByBytes = true
			break
		}
		if err := archiver.proof.include(record); err != nil {
			return ArchiveProgress{}, false, err
		}
		payloadStart := len(archiver.payload)
		archiver.payload = append(archiver.payload, record.bytes...)
		archiver.records = append(archiver.records, archiveRecord{
			cursor:       record.cursor,
			next:         record.next,
			extent:       record.extent,
			payloadStart: payloadStart,
			payloadBytes: len(record.bytes),
		})
		archiver.occupied += record.extent
		archiver.scan = record.next
		if len(archiver.records) == 1 {
			archiver.started = now
		}
		if archiver.occupied == archiver.config.Sizing.BatchMaxOccupiedBytes {
			cutByBytes = true
			break
		}
	}
	if len(archiver.records) == 0 {
		return ArchiveProgress{}, false, nil
	}
	cutByAge := now.Sub(archiver.started) >= archiver.config.Sizing.BatchMaxAge
	if !cutByBytes && !cutByAge {
		return ArchiveProgress{}, false, nil
	}
	meta, err := archiver.publishPendingLocked(now)
	if err != nil {
		return ArchiveProgress{}, false, err
	}
	if err := archiver.region.advanceDurable(archiver.proof); err != nil {
		return ArchiveProgress{}, false, err
	}
	progress := archiveProgressForBatch(meta)
	archiver.finishPending(meta.next)
	return progress, true, nil
}

func archiveProgressForBatch(meta archiveBatchMeta) ArchiveProgress {
	return ArchiveProgress{
		Batch: meta.id, First: meta.first.sequence, End: meta.next.sequence,
		DurableBytes: meta.occupiedBytes,
	}
}

func (archiver *Archiver) publishPendingLocked(now time.Time) (archiveBatchMeta, error) {
	filename, err := batchFilename(archiver.proof.from.sequence)
	if err != nil {
		return archiveBatchMeta{}, err
	}
	if err := archiver.preparePendingPayloads(); err != nil {
		return archiveBatchMeta{}, err
	}
	finalPath := filepath.Join(archiver.config.Directory, filename)
	tempPath := fmt.Sprintf("%s.%d.%d%s", finalPath, archiver.tid, archiver.generation, batchTemporarySuffix)
	var written archiveBatchMeta
	err = publishDurableTemp(
		archiver.ops,
		tempPath,
		finalPath,
		durableTempOptions{operation: "batch", noReplace: true, dataOnly: true},
		func(file archiveFile) error {
			var writeErr error
			written, writeErr = writeArchiveBatch(
				file,
				archiver.region.ringEpoch(),
				archiver.proof,
				archiver.records,
				archiver.config.Sizing.ZstdBlockBytes,
				archiver.config.ZstdLevel,
				now,
				&archiver.scratch,
			)
			return writeErr
		},
		func(file archiveFile) error {
			info, statErr := file.Stat()
			if statErr != nil {
				return statErr
			}
			validated, parseErr := parseArchiveBatchReaderBounded(
				file,
				uint64(info.Size()),
				filename,
				archiver.region.ringEpoch(),
				archiver.config.Sizing.ZstdBlockBytes,
			)
			if parseErr != nil {
				return parseErr
			}
			if validated != written {
				return ErrFormat
			}
			return nil
		},
	)
	if err != nil {
		return archiveBatchMeta{}, err
	}
	directory, err := archiver.ops.openDir(archiver.config.Directory)
	if err != nil {
		return archiveBatchMeta{}, fmt.Errorf("open archive directory: %w", err)
	}
	defer directory.Close()
	if err := archiver.ops.fsync(directory); err != nil {
		return archiveBatchMeta{}, fmt.Errorf("sync batch directory entry: %w", err)
	}
	if err := publishArchiveHeadLocked(
		archiver.config.Directory,
		archiveHeadForBatch(written),
		directory,
		archiver.ops,
	); err != nil {
		return archiveBatchMeta{}, err
	}
	return written, nil
}

func publishArchiveHeadLocked(
	directory string,
	head archiveHead,
	directoryFile archiveFile,
	ops archiveFileOps,
) error {
	bytes, err := encodeArchiveHead(head)
	if err != nil {
		return err
	}
	tempPath := filepath.Join(directory, archiveHeadTempName)
	finalPath := filepath.Join(directory, archiveHeadName)
	if err := publishDurableTemp(
		ops,
		tempPath,
		finalPath,
		durableTempOptions{operation: "archive head"},
		func(file archiveFile) error { return ops.writeAll(file, bytes) },
		func(file archiveFile) error {
			read := make([]byte, ArchiveHeadSize)
			if _, readErr := file.ReadAt(read, 0); readErr != nil {
				return readErr
			}
			parsed, parseErr := parseArchiveHead(read, head.epoch)
			if parseErr != nil {
				return parseErr
			}
			if parsed != head {
				return ErrFormat
			}
			return nil
		},
	); err != nil {
		return err
	}
	if err := ops.fsync(directoryFile); err != nil {
		return fmt.Errorf("sync archive head directory entry: %w", err)
	}
	return nil
}

func (archiver *Archiver) adoptOwnedOrphanLocked(
	durable ringCursor,
	publish ringCursor,
) (ArchiveProgress, bool, error) {
	filename, err := batchFilename(durable.sequence)
	if err != nil {
		return ArchiveProgress{}, false, err
	}
	path := filepath.Join(archiver.config.Directory, filename)
	maximumBatchFileBytes, err := MaximumArchiveBatchFileBytes(archiver.config.Sizing)
	if err != nil {
		return ArchiveProgress{}, false, err
	}
	meta, err := readArchiveBatchWithOps(
		path,
		archiver.region.ringEpoch(),
		maximumBatchFileBytes,
		archiver.config.Sizing.ZstdBlockBytes,
		archiver.ops,
	)
	if errors.Is(err, os.ErrNotExist) {
		return ArchiveProgress{}, false, nil
	}
	if err != nil {
		return ArchiveProgress{}, false, err
	}
	if meta.first != durable || meta.next.sequence > publish.sequence || meta.next.position > publish.position {
		return ArchiveProgress{}, false, archiveCorrupt(path, ErrFormat)
	}
	proof, recordsSHA256, err := archiveRingRecordsSHA256(
		archiver.region,
		durable,
		meta.next,
		publish,
	)
	if err != nil {
		return ArchiveProgress{}, false, archiveCorrupt(path, err)
	}
	if recordsSHA256 != meta.recordsSHA256 {
		return ArchiveProgress{}, false, archiveCorrupt(path, ErrFormat)
	}
	head, headPresent, err := readArchiveHeadWithOps(archiver.config.Directory, meta.epoch, archiver.ops)
	if err != nil {
		return ArchiveProgress{}, false, err
	}
	if headPresent {
		if head.latest == meta.id {
			if head != archiveHeadForBatch(meta) {
				return ArchiveProgress{}, false, archiveCorrupt(filepath.Join(archiver.config.Directory, archiveHeadName), ErrFormat)
			}
		} else if head.next != durable || Sequence(head.latest) >= durable.sequence {
			return ArchiveProgress{}, false, ErrArchiveLost
		}
	}
	directory, err := archiver.ops.openDir(archiver.config.Directory)
	if err != nil {
		return ArchiveProgress{}, false, err
	}
	defer directory.Close()
	if err := archiver.ops.fsync(directory); err != nil {
		return ArchiveProgress{}, false, fmt.Errorf("sync orphan batch directory entry: %w", err)
	}
	if err := publishArchiveHeadLocked(
		archiver.config.Directory,
		archiveHeadForBatch(meta),
		directory,
		archiver.ops,
	); err != nil {
		return ArchiveProgress{}, false, err
	}
	if err := archiver.region.advanceDurable(proof); err != nil {
		return ArchiveProgress{}, false, err
	}
	archiver.finishPending(meta.next)
	return archiveProgressForBatch(meta), true, nil
}

func archiveRingRecordsSHA256(
	region *ringRegion,
	from ringCursor,
	to ringCursor,
	publish ringCursor,
) (durableProof, [sha256.Size]byte, error) {
	proof, err := region.beginDurableProof(from, publish)
	if err != nil {
		return durableProof{}, [sha256.Size]byte{}, err
	}
	recordsHash := sha256.New()
	for proof.to != to {
		record, present, err := region.recordAt(proof.to, publish)
		if err != nil {
			return durableProof{}, [sha256.Size]byte{}, err
		}
		if !present || record.next.sequence > to.sequence || record.next.position > to.position {
			return durableProof{}, [sha256.Size]byte{}, ErrFormat
		}
		if err := proof.include(record); err != nil {
			return durableProof{}, [sha256.Size]byte{}, err
		}
		writeArchiveRecordHash(recordsHash, record.cursor.sequence, record.bytes)
	}
	var digest [sha256.Size]byte
	copy(digest[:], recordsHash.Sum(nil))
	return proof, digest, nil
}

func (archiver *Archiver) preparePendingPayloads() error {
	for index := range archiver.records {
		record := &archiver.records[index]
		if record.payloadStart < 0 || record.payloadBytes < 0 ||
			record.payloadStart > len(archiver.payload)-record.payloadBytes {
			return ErrFormat
		}
		record.payload = archiver.payload[record.payloadStart : record.payloadStart+record.payloadBytes]
	}
	return nil
}

func (archiver *Archiver) finishPending(cursor ringCursor) {
	for index := range archiver.records {
		archiver.records[index].payload = nil
	}
	archiver.records = archiver.records[:0]
	archiver.payload = archiver.payload[:0]
	archiver.occupied = 0
	archiver.started = time.Time{}
	archiver.proof = durableProof{}
	archiver.reclaim = cursor
	archiver.scan = cursor
}

func (archiver *Archiver) resetPending() {
	durable, ok := archiver.region.snapshotDurable()
	if !ok {
		return
	}
	archiver.finishPending(durable)
}

func (archiver *Archiver) nextRunDelay(now time.Time) time.Duration {
	if len(archiver.records) != 0 {
		remaining := archiver.config.Sizing.BatchMaxAge - now.Sub(archiver.started)
		if remaining <= 0 {
			return 0
		}
		return remaining
	}
	delay := archiver.config.Sizing.BatchMaxAge
	if delay <= 0 || delay > time.Millisecond {
		delay = time.Millisecond
	}
	return delay
}

// Run drains batches until context cancellation or an archive error.
// Run reuses one timer while it waits for records or an age condition.
func (archiver *Archiver) Run(ctx context.Context) error {
	if ctx == nil {
		return ErrMisuse
	}
	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		now := timeNow()
		_, committed, err := archiver.DrainOnce(now)
		if err != nil {
			return err
		}
		if committed {
			continue
		}
		delay := archiver.nextRunDelay(now)
		if delay <= 0 {
			continue
		}
		timer.Reset(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// Close discards the private pending batch and leaves durability unchanged.
// Close releases the archiver role and its archive lock descriptor.
// Close can block on the journal lifecycle lock and file close.
func (archiver *Archiver) Close() error {
	if archiver == nil {
		return nil
	}
	if !archiver.valid() {
		return ErrMisuse
	}
	if archiver.closed.Load() {
		return nil
	}
	if journalCurrentThreadIdentity() != archiver.identity {
		return ErrMisuse
	}
	journal := archiver.journal
	journal.life.Lock()
	defer journal.life.Unlock()
	archiver.finishPending(archiver.reclaim)
	released := releaseArchiverRole(archiver.region, archiver.tid, archiver.generation)
	archiver.closed.Store(true)
	if journal.archiver == archiver {
		journal.archiver = nil
	}
	closeErr := archiver.lock.Close()
	archiver.lock = nil
	runtime.UnlockOSThread()
	if !released {
		return errors.Join(ErrRecovered, closeErr)
	}
	return closeErr
}

func (archiver *Archiver) reapDead() bool {
	if !archiver.valid() || archiver.closed.Load() || archiver.identity == (hostcpu.ThreadIdentity{}) {
		return false
	}
	current := journalCurrentThreadIdentity()
	tidReused := journalThreadIdentityReused(archiver.identity, current)
	if !tidReused && journalThreadAliveProbe(archiver.identity.ThreadId()) {
		return false
	}
	archiver.closed.Store(true)
	archiver.finishPending(archiver.reclaim)
	_ = archiver.lock.Close()
	archiver.lock = nil
	archiver.identity = hostcpu.ThreadIdentity{}
	archiver.self = nil
	return true
}

type namedArchiveBatch struct {
	sequence Sequence
	name     string
}

func recoverArchive(journal *Journal, config ArchiveConfig, ops archiveFileOps) error {
	directory := config.Directory
	epoch := journal.region.ringEpoch()
	entries, err := ops.readDir(directory)
	if err != nil {
		return err
	}
	removedTemp := false
	batches := make([]namedArchiveBatch, 0)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if name == archiveHeadTempName || isArchiveBatchTemporary(name) {
			if err := ops.remove(filepath.Join(directory, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("remove archive temporary file: %w", err)
			}
			removedTemp = true
			continue
		}
		if sequence, ok := parseBatchFilename(name); ok {
			batches = append(batches, namedArchiveBatch{sequence: sequence, name: name})
		}
	}
	if removedTemp {
		directoryFile, err := ops.openDir(directory)
		if err != nil {
			return err
		}
		syncErr := ops.fsync(directoryFile)
		closeErr := directoryFile.Close()
		if syncErr != nil || closeErr != nil {
			return errors.Join(syncErr, closeErr)
		}
	}
	sort.Slice(batches, func(left, right int) bool { return batches[left].sequence < batches[right].sequence })
	head, headPresent, err := readArchiveHeadWithOps(directory, epoch, ops)
	if err != nil {
		return err
	}
	floor, floorPresent, err := readRetentionFloorWithOps(directory, epoch, ops)
	if err != nil {
		return err
	}
	if len(batches) == 0 {
		if headPresent || floorPresent {
			return ErrArchiveLost
		}
		return reconcileArchiveCursor(journal, ringCursor{sequence: 1})
	}

	start := 0
	if floorPresent {
		for start < len(batches) && BatchID(batches[start].sequence) < floor {
			start++
		}
		if start == len(batches) || BatchID(batches[start].sequence) != floor {
			return ErrArchiveGap
		}
	} else if batches[0].sequence != 1 {
		return ErrArchiveGap
	}
	if headPresent && floorPresent && floor > head.latest {
		return ErrArchiveGap
	}
	if start == len(batches) {
		return ErrArchiveGap
	}

	maximumBatchFileBytes, err := MaximumArchiveBatchFileBytes(config.Sizing)
	if err != nil {
		return err
	}
	remaining := config.Sizing.Bounds.MaxRecoveryScanBytes
	var previous archiveBatchMeta
	var latest archiveBatchMeta
	headFound := false
	adopted := false
	for index := start; index < len(batches); index++ {
		batch := batches[index]
		if index != start && batch.sequence != previous.next.sequence {
			return ErrArchiveGap
		}
		path := filepath.Join(directory, batch.name)
		meta, err := readArchiveBatchWithRecoveryBoundOps(
			path,
			epoch,
			maximumBatchFileBytes,
			config.Sizing.ZstdBlockBytes,
			remaining,
			ops,
		)
		if err != nil {
			return err
		}
		if index != start {
			if meta.first.sequence != previous.next.sequence {
				return ErrArchiveGap
			}
			if meta.first.position != previous.next.position {
				return archiveCorrupt(path, ErrFormat)
			}
		}
		scanOrphan := !headPresent || headFound
		if headPresent {
			if meta.id == head.latest {
				if headFound || head != archiveHeadForBatch(meta) {
					return archiveCorrupt(filepath.Join(directory, archiveHeadName), ErrFormat)
				}
				headFound = true
			} else if headFound {
				adopted = true
			} else if meta.id > head.latest {
				return ErrArchiveGap
			}
		}
		remaining -= meta.fileBytes
		if scanOrphan {
			if err := verifyArchiveBatchAgainstRing(journal.region, meta); err != nil {
				return archiveCorrupt(path, err)
			}
		}
		previous = meta
		latest = meta
	}
	if headPresent && !headFound {
		return ErrArchiveLost
	}
	if !headPresent {
		adopted = true
	}
	if adopted {
		directoryFile, err := ops.openDir(directory)
		if err != nil {
			return err
		}
		defer directoryFile.Close()
		if err := ops.fsync(directoryFile); err != nil {
			return fmt.Errorf("sync orphan batch directory entry: %w", err)
		}
		if err := publishArchiveHeadLocked(directory, archiveHeadForBatch(latest), directoryFile, ops); err != nil {
			return err
		}
	}
	return reconcileArchiveCursor(journal, latest.next)
}

func verifyArchiveBatchAgainstRing(region *ringRegion, meta archiveBatchMeta) error {
	_, digest, err := archiveRingRecordsSHA256(
		region,
		meta.first,
		meta.next,
		meta.next,
	)
	if err != nil {
		return err
	}
	if digest != meta.recordsSHA256 {
		return ErrFormat
	}
	return nil
}

func isArchiveBatchTemporary(name string) bool {
	return strings.HasPrefix(name, batchFilenamePrefix) &&
		strings.Contains(name, batchFilenameSuffix) &&
		strings.HasSuffix(name, batchTemporarySuffix)
}

func readArchiveHeadWithOps(
	directory string,
	epoch RingEpoch,
	ops archiveFileOps,
) (archiveHead, bool, error) {
	path := filepath.Join(directory, archiveHeadName)
	bytes, err := ops.readFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return archiveHead{}, false, nil
	}
	if err != nil {
		return archiveHead{}, false, err
	}
	head, err := parseArchiveHead(bytes, epoch)
	if err != nil {
		return archiveHead{}, false, archiveCorrupt(path, err)
	}
	return head, true, nil
}

func reconcileArchiveCursor(journal *Journal, disk ringCursor) error {
	shared, sharedOK := journal.region.snapshotDurable()
	needsRecovery := journal.region.recoveryRequired()
	if sharedOK && validCursorShape(shared) {
		if shared == disk && !needsRecovery {
			return nil
		}
		if cursorStrictlyAfter(shared, disk) ||
			(shared != disk && !cursorStrictlyAfter(disk, shared)) {
			return ErrArchiveLost
		}
	}
	journal.region.setRecoveryRequired()
	recovery, err := journal.beginCursorRecovery()
	if err != nil {
		return err
	}
	defer recovery.abort()
	if err := recovery.repairDurable(disk); err != nil {
		return err
	}
	publish, err := recovery.recoverPublish(disk)
	if err != nil {
		return err
	}
	return recovery.complete(disk, publish)
}

func cursorStrictlyAfter(left, right ringCursor) bool {
	return left.sequence > right.sequence && left.position > right.position
}
