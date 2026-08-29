package journal

import (
	"errors"
	"fmt"
)

var (
	// ErrFull reports a retryable producer capacity shortage.
	ErrFull = errors.New("journal: full")
	// ErrTooLarge reports a record or configured unit that exceeds format limits.
	ErrTooLarge = errors.New("journal: value too large")
	// ErrPositionExhausted reports sequence or logical-position exhaustion.
	ErrPositionExhausted = errors.New("journal: position exhausted")
	// ErrBusy reports a role or archive lock owned by a live process.
	ErrBusy = errors.New("journal: busy")
	// ErrMisuse reports an invalid handle, thread, view, or operation order.
	ErrMisuse = errors.New("journal: invalid operation")
	// ErrRecovered reports a stale handle generation after takeover.
	ErrRecovered = errors.New("journal: handle recovered")
	// ErrRecoveryRequired reports a ring that needs archive-backed cursor repair.
	ErrRecoveryRequired = errors.New("journal: recovery required")
	// ErrClosed reports an operation on a closed object.
	ErrClosed = errors.New("journal: closed")
	// ErrFormat reports invalid journal metadata or bytes.
	ErrFormat = errors.New("journal: invalid format")
	// ErrFormatVersion reports a recognized unsupported journal version.
	ErrFormatVersion = errors.New("journal: unsupported format version")
	// ErrPIDNamespace reports a creator PID namespace mismatch.
	ErrPIDNamespace = errors.New("journal: PID namespace differs")
	// ErrConvergence reports an invalid or unprovable startup bound.
	ErrConvergence = errors.New("journal: convergence is not proved")
	// ErrArchiveCorrupt reports failed archive validation.
	ErrArchiveCorrupt = errors.New("journal: archive corrupt")
	// ErrArchiveGap reports a missing batch at or above the retention floor.
	ErrArchiveGap = errors.New("journal: archive gap")
	// ErrArchiveLost reports shared durability beyond verified disk durability.
	ErrArchiveLost = errors.New("journal: durable archive lost")
	// ErrBatchBoundary reports a start sequence that is not a retained batch ID.
	ErrBatchBoundary = errors.New("journal: sequence is not a batch boundary")
	// ErrNotRetained reports a batch below the external retention floor.
	ErrNotRetained = errors.New("journal: batch is not retained")
	// ErrOverwritten reports failed live-view generation validation.
	ErrOverwritten = errors.New("journal: live record overwritten")
	// ErrResyncRequired reports an observer that requires explicit resync.
	ErrResyncRequired = errors.New("journal: observer resync required")
	// ErrBufferTooSmall reports insufficient caller replay storage.
	ErrBufferTooSmall = errors.New("journal: replay buffer too small")
)

// ConvergenceReason identifies one stable sizing failure category.
type ConvergenceReason uint8

const (
	ConvergenceInvalidBound ConvergenceReason = iota + 1
	ConvergenceArchiveRate
	ConvergenceObserverRate
	ConvergenceRecordFit
	ConvergenceObserverDeadline
	ConvergenceOverflow
)

func (reason ConvergenceReason) String() string {
	switch reason {
	case ConvergenceInvalidBound:
		return "invalid bound"
	case ConvergenceArchiveRate:
		return "archive rate does not converge"
	case ConvergenceObserverRate:
		return "observer rate does not converge"
	case ConvergenceRecordFit:
		return "maximum record does not fit"
	case ConvergenceObserverDeadline:
		return "observer catch-up exceeds deadline"
	case ConvergenceOverflow:
		return "calculation overflow"
	default:
		return "unknown convergence failure"
	}
}

// ConvergenceError reports all sizing inputs and calculated outputs.
type ConvergenceError struct {
	Reason ConvergenceReason

	MaxRecordBytes        uint64
	MaxRecordExtent       uint64
	BatchMaxOccupiedBytes uint64
	BatchMaxAgeNanos      int64
	ZstdBlockBytes        uint64

	PeakOccupiedBytesPerSecond      uint64
	PeakArchiveBytesPerSecond       uint64
	MinArchiveBytesPerSecond        uint64
	MinObserverReplayBytesPerSecond uint64
	BurstOccupiedBytes              uint64
	MaxArchiveStallNanos            int64
	MaxSyncLatencyNanos             int64
	MaxRecoveryScanBytes            uint64
	MaxCatchUpNanos                 int64
	MaxObserverBacklogBytes         uint64

	ArchiveRateMargin            uint64
	ObserverRateMargin           uint64
	StallOccupiedBytes           uint64
	MaximumArchiveBatchFileBytes uint64
	RequiredCapacity             uint64
	ObserverCatchUpNanos         int64

	Cause error
}

// Error returns a diagnostic sizing failure.
func (err *ConvergenceError) Error() string {
	if err == nil {
		return ErrConvergence.Error()
	}
	if err.Cause != nil {
		return fmt.Sprintf("%v: %s: %v", ErrConvergence, err.Reason, err.Cause)
	}
	return fmt.Sprintf("%v: %s", ErrConvergence, err.Reason)
}

// Unwrap identifies the stable sizing category and optional cause.
func (err *ConvergenceError) Unwrap() []error {
	if err == nil || err.Cause == nil {
		return []error{ErrConvergence}
	}
	return []error{ErrConvergence, err.Cause}
}

// NotRetainedError reports the current external retention range.
type NotRetainedError struct {
	Requested      BatchID
	OldestRetained BatchID
	NewestDurable  BatchID
}

func (err *NotRetainedError) Error() string {
	return fmt.Sprintf("%v: requested %d, oldest retained %d, newest durable %d", ErrNotRetained, err.Requested, err.OldestRetained, err.NewestDurable)
}

func (err *NotRetainedError) Unwrap() error { return ErrNotRetained }

// BufferTooSmallError reports the required replay payload storage.
type BufferTooSmallError struct {
	Required uint64
}

func (err *BufferTooSmallError) Error() string {
	return fmt.Sprintf("%v: need %d bytes", ErrBufferTooSmall, err.Required)
}

func (err *BufferTooSmallError) Unwrap() error { return ErrBufferTooSmall }

// ArchiveCorruptError reports a failed archive object validation.
type ArchiveCorruptError struct {
	Path  string
	Cause error
}

func (err *ArchiveCorruptError) Error() string {
	if err.Cause == nil {
		return fmt.Sprintf("%v: %s", ErrArchiveCorrupt, err.Path)
	}
	return fmt.Sprintf("%v: %s: %v", ErrArchiveCorrupt, err.Path, err.Cause)
}

func (err *ArchiveCorruptError) Unwrap() []error {
	if err.Cause == nil {
		return []error{ErrArchiveCorrupt}
	}
	return []error{ErrArchiveCorrupt, err.Cause}
}
