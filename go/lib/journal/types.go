package journal

import "time"

// Sequence identifies one committed record within a ring epoch.
// Zero is invalid. Sequence one identifies the first committed record.
type Sequence uint64

// BatchID identifies a durability batch by its first sequence.
type BatchID Sequence

// RingEpoch identifies one journal-ring instance and its archive.
type RingEpoch [16]byte

// Cursor identifies the next record to read.
// The logical ring position is intentionally not public.
type Cursor struct {
	RingEpoch RingEpoch
	Sequence  Sequence
}

// Backend selects the journal region backing store.
type Backend uint8

const (
	// BackendMemfd uses an anonymous sealed memory file.
	BackendMemfd Backend = iota
	// BackendSHM creates an exclusive named file under /dev/shm.
	BackendSHM
	// BackendFile creates an exclusive file at Config.Name.
	BackendFile
)

// Config controls journal-ring creation.
type Config struct {
	// Capacity is the requested occupied-byte capacity.
	// Creation rounds this value up to a supported power of two.
	Capacity uint64
	// MaxRecordBytes is the maximum payload length.
	MaxRecordBytes uint64
	// Sizing contains the required archive capacity proof.
	Sizing SizingConfig
	// Backend selects the region backing store.
	Backend Backend
	// Name is the BackendSHM name or BackendFile path.
	Name string
	// DisablePreallocate skips physical backing allocation when true.
	DisablePreallocate bool
	// AllowOverwrite lets BackendFile truncate an existing file.
	AllowOverwrite bool
}

// SizingConfig contains the persisted archive capacity proof inputs.
type SizingConfig struct {
	// BatchMaxOccupiedBytes cuts a batch after this occupied-byte bound.
	BatchMaxOccupiedBytes uint64
	// BatchMaxAge cuts a nonempty batch after this age.
	BatchMaxAge time.Duration
	// ZstdBlockBytes limits each uncompressed Zstandard block.
	ZstdBlockBytes uint64
	// Bounds contains startup capacity and convergence assumptions.
	Bounds Bounds
}

// ArchiveProgress describes one committed durability batch.
type ArchiveProgress struct {
	// Batch identifies the committed batch by its first sequence.
	Batch BatchID
	// First is the first sequence in the committed batch.
	First Sequence
	// End is the exclusive end sequence in the committed batch.
	End Sequence
	// DurableBytes is the occupied ring byte count made durable.
	DurableBytes uint64
}

// ArchiveConfig controls durable batch creation.
type ArchiveConfig struct {
	// Directory contains immutable batch files and archive metadata.
	Directory string
	// ZstdLevel selects the configured Zstandard compression level.
	ZstdLevel int
	// Sizing must match the journal-ring capacity proof.
	Sizing SizingConfig
}

// Bounds contains fail-closed capacity and convergence assumptions.
// Byte rates use occupied ring bytes or uncompressed archive bytes as named.
type Bounds struct {
	PeakOccupiedBytesPerSecond      uint64
	PeakArchiveBytesPerSecond       uint64
	MinArchiveBytesPerSecond        uint64
	MinObserverReplayBytesPerSecond uint64
	BurstOccupiedBytes              uint64
	MaxArchiveStall                 time.Duration
	MaxSyncLatency                  time.Duration
	MaxRecoveryScanBytes            uint64
	// MaxCatchUp is optional. Zero disables the global observer time bound.
	MaxCatchUp time.Duration
	// MaxObserverBacklogBytes is required when MaxCatchUp is nonzero.
	MaxObserverBacklogBytes uint64
}

// ObserverStart selects an exact retained batch.
// A nil Batch is valid only when no durable batch exists.
type ObserverStart struct {
	Batch *BatchID
}

// ObserverConfig controls archive replay and live-ring handoff.
type ObserverConfig struct {
	ArchiveDirectory string
	Start            ObserverStart
}

// ObserverState identifies one observer state.
type ObserverState uint8

const (
	ObserverReplay ObserverState = iota
	ObserverHandoff
	ObserverLive
	ObserverResyncRequired
	ObserverClosed
)
