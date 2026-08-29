//go:build linux && (amd64 || arm64)

package journal

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"github.com/akalsi-org/polyglot-template/go/lib/hostcpu"
	"github.com/akalsi-org/polyglot-template/go/lib/internal/orderedatomic"
	"github.com/akalsi-org/polyglot-template/go/lib/internal/shmregion"
)

const minimumJournalCapacity uint64 = JournalArenaAlignment

type noCopy struct{}

func (*noCopy) Lock()   {}
func (*noCopy) Unlock() {}

type ringCursor struct {
	sequence Sequence
	position uint64
}

type ownerState uint8

const (
	ownerFree ownerState = iota
	ownerLive
	ownerDead
)

type ringRegion struct {
	mapping         *shmregion.Mapping
	control         []byte
	mirroredArena   []byte
	capacity        uint64
	maxRecord       uint64
	maxRecordExtent uint64
	tagBase         uintptr
	descriptorBase  uintptr
}

// Journal owns one attached journal-ring mapping.
// Use Create or Attach to create a Journal. Do not copy Journal values.
type Journal struct {
	_                 noCopy
	self              *Journal
	region            *ringRegion
	life              sync.Mutex
	closing           bool
	archiverAttaching bool
	producer          *Producer
	archiver          *Archiver
}

// PIDNamespaceError reports a journal created in a different PID namespace.
type PIDNamespaceError struct {
	CreatorDev uint64
	CreatorIno uint64
	CurrentDev uint64
	CurrentIno uint64
}

func (err *PIDNamespaceError) Error() string {
	return fmt.Sprintf(
		"%v: creator device %d inode %d, current device %d inode %d",
		ErrPIDNamespace, err.CreatorDev, err.CreatorIno, err.CurrentDev, err.CurrentIno,
	)
}

func (err *PIDNamespaceError) Unwrap() error { return ErrPIDNamespace }

var (
	journalRandomRead            = rand.Read
	journalInitializeHeader      = initializeJournalHeader
	journalUnlink                = syscall.Unlink
	journalProcfsValidationProbe = hostcpu.ValidateProcfsPIDNamespace
	journalThreadAliveProbe      = hostcpu.ThreadAlive
	journalCurrentThreadIdentity = hostcpu.CurrentThreadIdentity
	journalThreadIdentityReused  = func(owner, current hostcpu.ThreadIdentity) bool {
		return owner.ThreadId() == current.ThreadId() && owner != current
	}
)

// Create creates and initializes a journal-ring mapping.
func Create(cfg Config) (*Journal, error) {
	if err := journalProcfsValidationProbe(); err != nil {
		return nil, err
	}
	requiredCapacity, err := RequiredCapacity(cfg.MaxRecordBytes, cfg.Sizing)
	if err != nil {
		return nil, err
	}
	if cfg.Capacity < requiredCapacity {
		failure := convergenceFailure(ConvergenceInvalidBound, cfg.MaxRecordBytes, 0, cfg.Sizing, errRequestedCapacity)
		failure.RequiredCapacity = requiredCapacity
		return nil, failure
	}
	capacity, err := normalizeJournalCapacity(cfg.Capacity)
	if err != nil {
		return nil, err
	}
	if err := validateMaxRecord(capacity, cfg.MaxRecordBytes); err != nil {
		return nil, err
	}
	sizingDigest := sizingConfigDigest(cfg.MaxRecordBytes, requiredCapacity, cfg.Sizing)
	layout, err := journalLayout(capacity)
	if err != nil {
		return nil, err
	}
	options, err := journalMappingOptions(cfg, layout)
	if err != nil {
		return nil, err
	}
	mapping, err := shmregion.Create(options)
	if err != nil {
		return nil, err
	}
	region := bindRingRegion(mapping, capacity, cfg.MaxRecordBytes)
	if err := journalInitializeHeader(
		region.control,
		capacity,
		cfg.MaxRecordBytes,
		requiredCapacity,
		sizingDigest,
	); err != nil {
		return nil, cleanupFailedJournalCreate(mapping, options, err)
	}
	journal := &Journal{region: region}
	journal.self = journal
	return journal, nil
}

func cleanupFailedJournalCreate(
	mapping *shmregion.Mapping,
	options shmregion.CreateOptions,
	cause error,
) error {
	closeErr := mapping.Close()
	var unlinkErr error
	switch {
	case options.Backend == shmregion.BackendSHM:
		unlinkErr = journalUnlink("/dev/shm/" + options.Name)
	case options.Backend == shmregion.BackendFile && !options.AllowOverwrite:
		unlinkErr = journalUnlink(options.Name)
	}
	if closeErr == nil && unlinkErr == nil {
		return cause
	}
	return errors.Join(cause, closeErr, unlinkErr)
}

// Attach attaches to a format-v1 journal through fd.
// Attach does not take ownership of fd.
func Attach(fd int) (*Journal, error) {
	if err := journalProcfsValidationProbe(); err != nil {
		return nil, err
	}
	page := os.Getpagesize()
	probe := make([]byte, page)
	fileSize, err := shmregion.Probe(fd, probe)
	if errors.Is(err, shmregion.ErrProbeTooSmall) {
		return nil, ErrFormat
	}
	if err != nil {
		return nil, err
	}
	capacity, maxRecord, err := validateJournalHeader(probe)
	if err != nil {
		return nil, err
	}
	layout, err := journalLayout(capacity)
	if err != nil || fileSize != layout.ControlBytes+layout.ArenaBytes {
		return nil, ErrFormat
	}
	mapping, err := shmregion.Attach(fd, layout)
	if errors.Is(err, syscall.EINVAL) {
		return nil, ErrFormat
	}
	if err != nil {
		return nil, err
	}
	region := bindRingRegion(mapping, capacity, maxRecord)
	if !region.hasValidFlags() || !region.hasValidSharedOwners() || !region.hasZeroLayoutPadding() {
		_ = mapping.Close()
		return nil, ErrFormat
	}
	journal := &Journal{region: region}
	journal.self = journal
	journal.initializeRecoveryState()
	return journal, nil
}

func (journal *Journal) valid() bool { return journal != nil && journal.self == journal }

// NeedsRecovery reports whether archive-backed cursor repair must complete.
func (journal *Journal) NeedsRecovery() bool {
	if !journal.valid() {
		return false
	}
	journal.life.Lock()
	defer journal.life.Unlock()
	return journal.needsRecoveryLocked()
}

func (journal *Journal) initializeRecoveryState() {
	journal.life.Lock()
	defer journal.life.Unlock()
	journal.needsRecoveryLocked()
}

func (journal *Journal) needsRecoveryLocked() bool {
	if journal.closing || journal.region == nil || len(journal.region.control) < int(JournalHeaderSize) {
		return false
	}
	if journal.region.recoveryRequired() {
		return true
	}
	producerOwner, archiverOwner := journal.region.sharedOwners()
	producerState := classifyOwnerWord(producerOwner)
	archiverState := classifyOwnerWord(archiverOwner)
	if producerState == ownerDead || archiverState == ownerDead {
		journal.region.setRecoveryRequired()
		return true
	}
	if !journal.region.needsCursorRecovery() {
		return false
	}
	if producerState == ownerLive || archiverState == ownerLive {
		return false
	}
	journal.region.setRecoveryRequired()
	return true
}

// Capacity returns the normalized payload capacity in bytes.
func (journal *Journal) Capacity() uint64 {
	if !journal.valid() || journal.region == nil {
		return 0
	}
	return journal.region.capacity
}

// DupFD duplicates the journal backing file descriptor.
// The caller owns the returned descriptor.
func (journal *Journal) DupFD() (int, error) {
	if journal == nil {
		return -1, ErrClosed
	}
	if !journal.valid() {
		return -1, ErrMisuse
	}
	journal.life.Lock()
	defer journal.life.Unlock()
	if journal.closing || journal.region == nil {
		return -1, ErrClosed
	}
	return journal.region.mapping.DupFD()
}

// Close unmaps the journal after all local role handles close.
func (journal *Journal) Close() error {
	if journal == nil {
		return nil
	}
	if !journal.valid() {
		return ErrMisuse
	}
	journal.life.Lock()
	defer journal.life.Unlock()
	journal.reapDeadProducerLocked()
	journal.reapDeadArchiverLocked()
	if journal.archiverAttaching || journal.producer != nil || journal.archiver != nil {
		return ErrBusy
	}
	if journal.closing {
		return nil
	}
	journal.closing = true
	if journal.region == nil {
		return nil
	}
	return journal.region.close()
}

func normalizeJournalCapacity(requested uint64) (uint64, error) {
	if requested == 0 || requested > JournalMaxCapacity {
		return 0, fmt.Errorf("%w: capacity", ErrFormat)
	}
	capacity := requested
	if capacity < minimumJournalCapacity {
		capacity = minimumJournalCapacity
	}
	var ok bool
	capacity, ok = nextPowerOfTwo(capacity)
	if !ok {
		return 0, fmt.Errorf("%w: capacity", ErrFormat)
	}
	if capacity < requested || capacity > JournalMaxCapacity || capacity > uint64(^uint(0)>>1) {
		return 0, fmt.Errorf("%w: capacity", ErrFormat)
	}
	return capacity, nil
}

func validateMaxRecord(capacity, maximum uint64) error {
	if maximum == 0 {
		return fmt.Errorf("%w: maximum record bytes", ErrFormat)
	}
	if maximum > JournalDescriptorMaxLength {
		return ErrTooLarge
	}
	extent, ok := journalExtent(maximum)
	if !ok || extent > capacity-JournalSentinelBytes {
		return ErrTooLarge
	}
	return nil
}

func journalExtent(length uint64) (uint64, bool) {
	if length > JournalDescriptorMaxLength || length > ^uint64(0)-(uint64(JournalGrain)-1) {
		return 0, false
	}
	extent := (length + uint64(JournalGrain) - 1) &^ (uint64(JournalGrain) - 1)
	if extent == 0 {
		extent = uint64(JournalGrain)
	}
	return extent, extent/uint64(JournalGrain) <= JournalDescriptorMaxExtent
}

func journalLayout(capacity uint64) (shmregion.Layout, error) {
	return journalLayoutForPage(capacity, uint64(os.Getpagesize()))
}

func journalLayoutForPage(capacity, pageBytes uint64) (shmregion.Layout, error) {
	if pageBytes == 0 || pageBytes&(pageBytes-1) != 0 ||
		JournalArenaAlignment%pageBytes != 0 ||
		capacity < minimumJournalCapacity || capacity > JournalMaxCapacity ||
		capacity&(capacity-1) != 0 || capacity%pageBytes != 0 {
		return shmregion.Layout{}, ErrFormat
	}
	metadataBytes := capacity / uint64(JournalGrain) * (uint64(JournalTagSize) + uint64(JournalDescriptorSize))
	if metadataBytes > ^uint64(0)-uint64(JournalHeaderSize) {
		return shmregion.Layout{}, ErrFormat
	}
	usedControlBytes := uint64(JournalHeaderSize) + metadataBytes
	if usedControlBytes > ^uint64(0)-(JournalArenaAlignment-1) {
		return shmregion.Layout{}, ErrFormat
	}
	controlBytes := (usedControlBytes + JournalArenaAlignment - 1) &^ (JournalArenaAlignment - 1)
	return shmregion.Layout{ControlBytes: controlBytes, ArenaBytes: capacity}, nil
}

func journalMappingOptions(cfg Config, layout shmregion.Layout) (shmregion.CreateOptions, error) {
	var backend shmregion.Backend
	switch cfg.Backend {
	case BackendMemfd:
		backend = shmregion.BackendMemfd
	case BackendSHM:
		if cfg.Name == "" || strings.Contains(cfg.Name, "..") || strings.ContainsAny(cfg.Name, "/\\") {
			return shmregion.CreateOptions{}, fmt.Errorf("%w: shared memory name", ErrFormat)
		}
		backend = shmregion.BackendSHM
	case BackendFile:
		if cfg.Name == "" {
			return shmregion.CreateOptions{}, fmt.Errorf("%w: file path", ErrFormat)
		}
		backend = shmregion.BackendFile
	default:
		return shmregion.CreateOptions{}, fmt.Errorf("%w: backend", ErrFormat)
	}
	return shmregion.CreateOptions{
		Backend:            backend,
		Name:               cfg.Name,
		Layout:             layout,
		DisablePreallocate: cfg.DisablePreallocate,
		AllowOverwrite:     cfg.AllowOverwrite,
		MemfdName:          "pgt-journal",
	}, nil
}

func bindRingRegion(mapping *shmregion.Mapping, capacity, maxRecord uint64) *ringRegion {
	tagBytes := capacity / uint64(JournalGrain) * uint64(JournalTagSize)
	maxRecordExtent, _ := journalExtent(maxRecord)
	return &ringRegion{
		mapping:         mapping,
		control:         mapping.Control(),
		mirroredArena:   mapping.Arena(),
		capacity:        capacity,
		maxRecord:       maxRecord,
		maxRecordExtent: maxRecordExtent,
		tagBase:         uintptr(JournalHeaderSize),
		descriptorBase:  uintptr(uint64(JournalHeaderSize) + tagBytes),
	}
}

func (region *ringRegion) hasZeroLayoutPadding() bool {
	metadataBytes := region.capacity / uint64(JournalGrain) *
		(uint64(JournalTagSize) + uint64(JournalDescriptorSize))
	paddingStart := uint64(JournalHeaderSize) + metadataBytes
	return paddingStart <= uint64(len(region.control)) &&
		allZero(region.control[paddingStart:])
}

func initializeJournalHeader(header []byte, capacity, maxRecord, requiredCapacity uint64, sizingDigest [SizingConfigDigestSize]byte) error {
	namespace, err := hostcpu.CurrentPIDNamespaceIdentity()
	if err != nil {
		return err
	}
	var epoch RingEpoch
	for allZero(epoch[:]) {
		if _, err := journalRandomRead(epoch[:]); err != nil {
			return fmt.Errorf("create ring epoch: %w", err)
		}
	}
	put64(header, JournalHeaderMagicOffset, JournalMagic)
	put32(header, JournalHeaderVersionOffset, JournalFormatVersion)
	put32(header, JournalHeaderSizeOffset, JournalHeaderSize)
	put32(header, JournalHeaderEndianOffset, JournalEndianMarker)
	put32(header, JournalHeaderGrainOffset, JournalGrain)
	put64(header, JournalHeaderCapacityOffset, capacity)
	put64(header, JournalHeaderMaxRecordBytesOffset, maxRecord)
	copy(header[JournalHeaderRingEpochOffset:JournalHeaderRingEpochOffset+16], epoch[:])
	put32(header, JournalHeaderCreatorPIDOffset, uint32(os.Getpid()))
	put64(header, JournalHeaderPIDNamespaceDevOffset, namespace.Dev)
	put64(header, JournalHeaderPIDNamespaceInodeOffset, namespace.Ino)
	put64(header, JournalHeaderPublishSequenceOffset, 1)
	put64(header, JournalHeaderDurableSequenceOffset, 1)
	put64(header, JournalHeaderRequiredCapacityOffset, requiredCapacity)
	copy(header[JournalHeaderSizingDigestOffset:JournalHeaderSizingDigestOffset+SizingConfigDigestSize], sizingDigest[:])
	return nil
}

func validateJournalHeader(header []byte) (uint64, uint64, error) {
	if len(header) < int(JournalHeaderSize) {
		return 0, 0, ErrFormat
	}
	magic := get64(header, JournalHeaderMagicOffset)
	version := get32(header, JournalHeaderVersionOffset)
	if magic != JournalMagic {
		return 0, 0, ErrFormat
	}
	if version != JournalFormatVersion {
		return 0, 0, fmt.Errorf("%w: got %d", ErrFormatVersion, version)
	}
	if get32(header, JournalHeaderSizeOffset) != JournalHeaderSize ||
		get32(header, JournalHeaderEndianOffset) != JournalEndianMarker ||
		get32(header, JournalHeaderGrainOffset) != JournalGrain ||
		get32(header, JournalHeaderCreatorPIDOffset) == 0 ||
		get32(header, JournalHeaderFlagsOffset)&^JournalKnownFlags != 0 ||
		allZero(header[JournalHeaderRingEpochOffset:JournalHeaderRingEpochOffset+16]) {
		return 0, 0, ErrFormat
	}
	capacity := get64(header, JournalHeaderCapacityOffset)
	maximum := get64(header, JournalHeaderMaxRecordBytesOffset)
	if _, err := journalLayout(capacity); err != nil {
		return 0, 0, ErrFormat
	}
	if err := validateMaxRecord(capacity, maximum); err != nil {
		return 0, 0, ErrFormat
	}
	if !validSizingContractBytes(header, capacity) {
		return 0, 0, ErrFormat
	}
	if !allZero(header[120:128]) || !allZero(header[JournalHeaderReservedOffset:JournalHeaderSize]) {
		return 0, 0, ErrFormat
	}
	currentNamespace, err := hostcpu.CurrentPIDNamespaceIdentity()
	if err != nil {
		return 0, 0, err
	}
	creatorNamespace := hostcpu.PIDNamespaceIdentity{
		Dev: get64(header, JournalHeaderPIDNamespaceDevOffset),
		Ino: get64(header, JournalHeaderPIDNamespaceInodeOffset),
	}
	if creatorNamespace != currentNamespace {
		return 0, 0, &PIDNamespaceError{
			CreatorDev: creatorNamespace.Dev,
			CreatorIno: creatorNamespace.Ino,
			CurrentDev: currentNamespace.Dev,
			CurrentIno: currentNamespace.Ino,
		}
	}
	return capacity, maximum, nil
}

func validSizingContractBytes(header []byte, capacity uint64) bool {
	if len(header) < int(JournalHeaderSizingDigestOffset+SizingConfigDigestSize) {
		return false
	}
	required := get64(header, JournalHeaderRequiredCapacityOffset)
	digest := header[JournalHeaderSizingDigestOffset : JournalHeaderSizingDigestOffset+SizingConfigDigestSize]
	return required != 0 && required <= capacity && !allZero(digest)
}

func (region *ringRegion) hasValidSizingContract() bool {
	return region != nil && validSizingContractBytes(region.control, region.capacity)
}

func (region *ringRegion) sharedOwners() (uint64, uint64) {
	return orderedatomic.LoadAcquire64(region.ptr64(JournalHeaderProducerTIDOffset)),
		orderedatomic.LoadAcquire64(region.ptr64(JournalHeaderArchiverTIDOffset))
}

func (region *ringRegion) hasValidSharedOwners() bool {
	if region == nil || len(region.control) < int(JournalHeaderSize) {
		return false
	}
	producer, archiver := region.sharedOwners()
	return validOwnerWord(producer) && validOwnerWord(archiver)
}

func validOwnerWord(owner uint64) bool {
	return uint32(owner) == 0 || uint32(owner>>32) != 0
}

func (region *ringRegion) hasDeadSharedOwner() bool {
	if region == nil || len(region.control) < int(JournalHeaderSize) {
		return false
	}
	producer, archiver := region.sharedOwners()
	return ownerWordIsDead(producer) || ownerWordIsDead(archiver)
}

func classifyOwnerWord(owner uint64) ownerState {
	tid := hostcpu.ThreadId(uint32(owner))
	if tid == 0 {
		return ownerFree
	}
	if journalThreadAliveProbe(tid) {
		return ownerLive
	}
	return ownerDead
}

func ownerWordIsDead(owner uint64) bool {
	return classifyOwnerWord(owner) == ownerDead
}

func ownerWordIsLive(owner uint64) bool {
	return classifyOwnerWord(owner) == ownerLive
}

func (region *ringRegion) close() error {
	if region == nil || region.mapping == nil {
		return nil
	}
	err := region.mapping.Close()
	region.control = nil
	region.mirroredArena = nil
	return err
}

func (region *ringRegion) ptr64(offset uint32) *uint64 {
	return (*uint64)(unsafe.Pointer(&region.control[offset]))
}

func (region *ringRegion) flags() *uint32 {
	return (*uint32)(unsafe.Pointer(&region.control[JournalHeaderFlagsOffset]))
}

func (region *ringRegion) hasValidFlags() bool {
	return orderedatomic.LoadAcquire32(region.flags())&^JournalKnownFlags == 0
}

func (region *ringRegion) recoveryRequired() bool {
	return orderedatomic.LoadAcquire32(region.flags())&JournalFlagRecoveryRequired != 0
}

func (region *ringRegion) setRecoveryRequired() {
	flags := region.flags()
	for {
		current := orderedatomic.LoadAcquire32(flags)
		if current&JournalFlagRecoveryRequired != 0 ||
			orderedatomic.CompareAndSwap32(flags, current, current|JournalFlagRecoveryRequired) {
			return
		}
		orderedatomic.Relax()
	}
}

func (region *ringRegion) clearRecoveryRequired() {
	flags := region.flags()
	for {
		current := orderedatomic.LoadAcquire32(flags)
		if current&JournalFlagRecoveryRequired == 0 ||
			orderedatomic.CompareAndSwap32(flags, current, current&^JournalFlagRecoveryRequired) {
			return
		}
		orderedatomic.Relax()
	}
}

func (region *ringRegion) tag(position uint64) *uint64 {
	cell := (position / uint64(JournalGrain)) & (region.capacity/uint64(JournalGrain) - 1)
	return (*uint64)(unsafe.Add(unsafe.Pointer(&region.control[0]), region.tagBase+uintptr(cell)*uintptr(JournalTagSize)))
}

func (region *ringRegion) descriptor(position uint64) *uint64 {
	cell := (position / uint64(JournalGrain)) & (region.capacity/uint64(JournalGrain) - 1)
	return (*uint64)(unsafe.Add(unsafe.Pointer(&region.control[0]), region.descriptorBase+uintptr(cell)*uintptr(JournalDescriptorSize)))
}

func (region *ringRegion) fillTagsRelaxed(position, grains, value uint64) {
	region.fillPlaneRelaxed(
		region.tagBase,
		uintptr(JournalTagSize),
		position,
		grains,
		value,
	)
}

func (region *ringRegion) fillDescriptorsRelaxed(position, grains, value uint64) {
	region.fillPlaneRelaxed(
		region.descriptorBase,
		uintptr(JournalDescriptorSize),
		position,
		grains,
		value,
	)
}

func (region *ringRegion) fillPlaneRelaxed(
	base,
	entrySize uintptr,
	position,
	grains,
	value uint64,
) {
	if grains == 0 {
		return
	}
	cells := region.capacity / uint64(JournalGrain)
	start := (position / uint64(JournalGrain)) & (cells - 1)
	first := min(grains, cells-start)
	firstPointer := (*uint64)(unsafe.Add(
		unsafe.Pointer(&region.control[0]),
		base+uintptr(start)*entrySize,
	))
	orderedatomic.FillRelaxed64(firstPointer, uintptr(first), value)
	if first == grains {
		return
	}
	secondPointer := (*uint64)(unsafe.Add(unsafe.Pointer(&region.control[0]), base))
	orderedatomic.FillRelaxed64(secondPointer, uintptr(grains-first), value)
}

func (region *ringRegion) payload(position, length uint64) []byte {
	start := position & (region.capacity - 1)
	return unsafe.Slice((*byte)(unsafe.Add(unsafe.Pointer(&region.mirroredArena[0]), uintptr(start))), int(length))
}

func (region *ringRegion) snapshotPublish() (ringCursor, bool) {
	return snapshotCursor(region, JournalHeaderPublishLockOffset, JournalHeaderPublishSequenceOffset, JournalHeaderPublishPositionOffset)
}

func (region *ringRegion) snapshotDurable() (ringCursor, bool) {
	return snapshotCursor(region, JournalHeaderDurableLockOffset, JournalHeaderDurableSequenceOffset, JournalHeaderDurablePositionOffset)
}

func (region *ringRegion) needsCursorRecovery() bool {
	for attempt := 0; attempt < 1024; attempt++ {
		publish, publishOK := snapshotCursorOnce(region, JournalHeaderPublishLockOffset, JournalHeaderPublishSequenceOffset, JournalHeaderPublishPositionOffset)
		durable, durableOK := snapshotCursorOnce(region, JournalHeaderDurableLockOffset, JournalHeaderDurableSequenceOffset, JournalHeaderDurablePositionOffset)
		if publishOK && durableOK && region.validCursorPair(durable, publish) {
			return false
		}
		if attempt < 64 {
			orderedatomic.Relax()
		} else {
			runtime.Gosched()
		}
	}
	return true
}

func (region *ringRegion) validCursorPair(durable, publish ringCursor) bool {
	return validCursorPairWithExtent(
		durable,
		publish,
		region.capacity,
		region.maxRecordExtent,
	)
}

func validCursorPair(durable, publish ringCursor, capacity, maxRecord uint64) bool {
	maximumExtent, ok := journalExtent(maxRecord)
	return ok && validCursorPairWithExtent(durable, publish, capacity, maximumExtent)
}

func validCursorPairWithExtent(durable, publish ringCursor, capacity, maximumExtent uint64) bool {
	if !validCursorShape(publish) || !validCursorShape(durable) ||
		durable.sequence > publish.sequence || durable.position > publish.position ||
		publish.position-durable.position >= capacity {
		return false
	}
	sequenceDelta := uint64(publish.sequence - durable.sequence)
	positionDelta := publish.position - durable.position
	if (sequenceDelta == 0) != (positionDelta == 0) ||
		sequenceDelta > ^uint64(0)/uint64(JournalGrain) ||
		positionDelta < sequenceDelta*uint64(JournalGrain) {
		return false
	}
	return maximumExtent != 0 &&
		(sequenceDelta > ^uint64(0)/maximumExtent ||
			positionDelta <= sequenceDelta*maximumExtent)
}

func validCursorShape(cursor ringCursor) bool {
	if cursor.sequence == 0 || cursor.sequence > JournalMaxSequence+1 || cursor.position%uint64(JournalGrain) != 0 {
		return false
	}
	return (cursor.sequence == 1) == (cursor.position == 0)
}

func snapshotCursor(region *ringRegion, lockOffset, sequenceOffset, positionOffset uint32) (ringCursor, bool) {
	return snapshotCursorBytes(region.control, lockOffset, sequenceOffset, positionOffset)
}

func snapshotCursorOnce(region *ringRegion, lockOffset, sequenceOffset, positionOffset uint32) (ringCursor, bool) {
	return snapshotCursorOnceBytes(region.control, lockOffset, sequenceOffset, positionOffset)
}

func snapshotCursorBytes(header []byte, lockOffset, sequenceOffset, positionOffset uint32) (ringCursor, bool) {
	for attempts := 0; attempts < 1024; attempts++ {
		cursor, ok := snapshotCursorOnceBytes(header, lockOffset, sequenceOffset, positionOffset)
		if ok {
			return cursor, true
		}
		orderedatomic.Relax()
	}
	return ringCursor{}, false
}

func snapshotCursorOnceBytes(header []byte, lockOffset, sequenceOffset, positionOffset uint32) (ringCursor, bool) {
	if len(header) < int(positionOffset)+8 {
		return ringCursor{}, false
	}
	base := unsafe.Pointer(&header[0])
	lock := (*uint64)(unsafe.Add(base, uintptr(lockOffset)))
	before := orderedatomic.LoadAcquire64(lock)
	if before&1 != 0 {
		return ringCursor{}, false
	}
	nextSequence := orderedatomic.LoadRelaxed64((*uint64)(unsafe.Add(base, uintptr(sequenceOffset))))
	nextPosition := orderedatomic.LoadRelaxed64((*uint64)(unsafe.Add(base, uintptr(positionOffset))))
	orderedatomic.LoadBarrier()
	after := orderedatomic.LoadAcquire64(lock)
	if before != after || after&1 != 0 {
		return ringCursor{}, false
	}
	return ringCursor{sequence: Sequence(nextSequence), position: nextPosition}, true
}

func publishCursor(region *ringRegion, lockOffset, sequenceOffset, positionOffset uint32, cursor ringCursor) {
	lock := region.ptr64(lockOffset)
	version := orderedatomic.LoadRelaxed64(lock)
	if version&1 != 0 {
		version++
	}
	orderedatomic.StoreRelease64(lock, version+1)
	orderedatomic.StoreBarrier()
	orderedatomic.StoreRelaxed64(region.ptr64(sequenceOffset), uint64(cursor.sequence))
	orderedatomic.StoreRelaxed64(region.ptr64(positionOffset), cursor.position)
	orderedatomic.StoreRelease64(lock, version+2)
}

func (journal *Journal) reapDeadProducerLocked() {
	if journal.producer != nil && journal.producer.reapDead() {
		journal.producer = nil
		journal.region.setRecoveryRequired()
	}
}

func (journal *Journal) reapDeadArchiverLocked() {
	if journal.archiver != nil && journal.archiver.reapDead() {
		journal.archiver = nil
		journal.region.setRecoveryRequired()
	}
}

func allZero(bytes []byte) bool {
	for _, value := range bytes {
		if value != 0 {
			return false
		}
	}
	return true
}

func get32(bytes []byte, offset uint32) uint32 {
	return binary.LittleEndian.Uint32(bytes[offset : offset+4])
}

func get64(bytes []byte, offset uint32) uint64 {
	return binary.LittleEndian.Uint64(bytes[offset : offset+8])
}

func put32(bytes []byte, offset uint32, value uint32) {
	binary.LittleEndian.PutUint32(bytes[offset:offset+4], value)
}

func put64(bytes []byte, offset uint32, value uint64) {
	binary.LittleEndian.PutUint64(bytes[offset:offset+8], value)
}
