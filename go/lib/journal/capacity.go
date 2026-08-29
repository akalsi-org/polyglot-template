package journal

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"math"
	"math/big"
	"time"
)

var (
	errZeroBound           = errors.New("a required bound is zero")
	errNegativeDuration    = errors.New("a duration bound is negative")
	errArchiveRate         = errors.New("minimum archive rate must exceed peak archive rate")
	errObserverRate        = errors.New("minimum observer replay rate must exceed peak archive rate")
	errRecordBatchFit      = errors.New("maximum record extent exceeds the batch bound")
	errRecordBlockFit      = errors.New("maximum encoded record exceeds the Zstandard block bound")
	errCatchUpBacklog      = errors.New("observer backlog is required with a catch-up deadline")
	errCatchUpDeadline     = errors.New("observer catch-up time exceeds the configured deadline")
	errCalculationOverflow = errors.New("capacity calculation exceeds format limits")
	errRequestedCapacity   = errors.New("requested capacity is below the required capacity")
	errRecoveryScanBatch   = errors.New("maximum recovery scan bytes cannot cover one maximum archive batch file")
	errZstdBlockMemory     = errors.New("Zstandard block bytes exceed the supported memory bound")
)

// RequiredCapacity returns the minimum power-of-two payload capacity.
// It fails closed when any required bound is absent or does not converge.
func RequiredCapacity(maxRecordBytes uint64, sizing SizingConfig) (uint64, error) {
	err := convergenceInputs(maxRecordBytes, sizing)
	if err != nil {
		return 0, err
	}

	maxExtent, ok := align64(maxRecordBytes)
	if !ok {
		return 0, convergenceFailure(ConvergenceOverflow, maxRecordBytes, 0, sizing, errCalculationOverflow)
	}
	if maxExtent > JournalDescriptorMaxExtent*uint64(JournalGrain) || maxRecordBytes > JournalDescriptorMaxLength {
		return 0, convergenceFailure(ConvergenceRecordFit, maxRecordBytes, maxExtent, sizing, ErrTooLarge)
	}
	if maxExtent > sizing.BatchMaxOccupiedBytes {
		return 0, convergenceFailure(ConvergenceRecordFit, maxRecordBytes, maxExtent, sizing, errRecordBatchFit)
	}
	encodedRecordBytes, carry := add64(maxRecordBytes, uint64(BatchRecordHeaderSize))
	if carry || encodedRecordBytes > sizing.ZstdBlockBytes {
		return 0, convergenceFailure(ConvergenceRecordFit, maxRecordBytes, maxExtent, sizing, errRecordBlockFit)
	}
	maximumBatchFileBytes, reason, cause := maximumArchiveBatchFileBytes(sizing)
	if cause != nil {
		return 0, convergenceFailure(reason, maxRecordBytes, maxExtent, sizing, cause)
	}
	if sizing.Bounds.MaxRecoveryScanBytes < maximumBatchFileBytes {
		err := convergenceFailure(ConvergenceInvalidBound, maxRecordBytes, maxExtent, sizing, errRecoveryScanBatch)
		err.MaximumArchiveBatchFileBytes = maximumBatchFileBytes
		return 0, err
	}

	bounds := sizing.Bounds
	archiveMargin := bounds.MinArchiveBytesPerSecond - bounds.PeakArchiveBytesPerSecond
	observerMargin := bounds.MinObserverReplayBytesPerSecond - bounds.PeakArchiveBytesPerSecond
	stallBytes, ok := occupiedDuringStop(bounds)
	if !ok {
		return 0, convergenceFailure(ConvergenceOverflow, maxRecordBytes, maxExtent, sizing, errCalculationOverflow)
	}

	base, ok := checkedSum(
		sizing.BatchMaxOccupiedBytes,
		stallBytes,
		bounds.BurstOccupiedBytes,
		maxExtent,
		JournalSentinelBytes,
	)
	if !ok {
		return 0, convergenceFailure(ConvergenceOverflow, maxRecordBytes, maxExtent, sizing, errCalculationOverflow)
	}
	aligned, ok := align64(base)
	if !ok {
		return 0, convergenceFailure(ConvergenceOverflow, maxRecordBytes, maxExtent, sizing, errCalculationOverflow)
	}
	required, ok := nextPowerOfTwo(aligned)
	if !ok || required > JournalMaxCapacity {
		return 0, convergenceFailure(ConvergenceOverflow, maxRecordBytes, maxExtent, sizing, errCalculationOverflow)
	}

	var observerCatchUp time.Duration
	if bounds.MaxCatchUp != 0 {
		nanos, ok := ceilMulDiv(bounds.MaxObserverBacklogBytes, uint64(time.Second), observerMargin)
		if !ok || nanos > math.MaxInt64 {
			return 0, convergenceFailure(ConvergenceOverflow, maxRecordBytes, maxExtent, sizing, errCalculationOverflow)
		}
		observerCatchUp = time.Duration(nanos)
		if observerCatchUp > bounds.MaxCatchUp {
			err := convergenceFailure(ConvergenceObserverDeadline, maxRecordBytes, maxExtent, sizing, errCatchUpDeadline)
			err.ArchiveRateMargin = archiveMargin
			err.ObserverRateMargin = observerMargin
			err.StallOccupiedBytes = stallBytes
			err.RequiredCapacity = required
			err.ObserverCatchUpNanos = int64(observerCatchUp)
			return 0, err
		}
	}

	return required, nil
}

// MaximumArchiveBatchFileBytes returns a conservative format-v1 file-size bound.
// Archive recovery can use this result as its minimum per-file scan allowance.
func MaximumArchiveBatchFileBytes(sizing SizingConfig) (uint64, error) {
	maximum, reason, cause := maximumArchiveBatchFileBytes(sizing)
	if cause != nil {
		return 0, convergenceFailure(reason, 0, 0, sizing, cause)
	}
	return maximum, nil
}

func maximumArchiveBatchFileBytes(sizing SizingConfig) (uint64, ConvergenceReason, error) {
	if sizing.BatchMaxOccupiedBytes == 0 || sizing.ZstdBlockBytes == 0 {
		return 0, ConvergenceInvalidBound, errZeroBound
	}
	maximumRecords := sizing.BatchMaxOccupiedBytes / uint64(JournalGrain)
	if maximumRecords == 0 {
		return 0, ConvergenceRecordFit, errRecordBatchFit
	}
	recordHeaderBytes, ok := checkedMul(maximumRecords, uint64(BatchRecordHeaderSize))
	if !ok {
		return 0, ConvergenceOverflow, errCalculationOverflow
	}
	uncompressedBytes, carry := add64(sizing.BatchMaxOccupiedBytes, recordHeaderBytes)
	if carry {
		return 0, ConvergenceOverflow, errCalculationOverflow
	}
	zstdPayloadBytes, ok := checkedMul(uncompressedBytes, 2)
	if !ok {
		return 0, ConvergenceOverflow, errCalculationOverflow
	}
	zstdFrameBytes, ok := checkedMul(maximumRecords, 64)
	if !ok {
		return 0, ConvergenceOverflow, errCalculationOverflow
	}
	zstdBytes, carry := add64(zstdPayloadBytes, zstdFrameBytes)
	if carry {
		return 0, ConvergenceOverflow, errCalculationOverflow
	}
	blockHeaderBytes, ok := checkedMul(maximumRecords, uint64(BatchBlockHeaderSize))
	if !ok {
		return 0, ConvergenceOverflow, errCalculationOverflow
	}
	indexBytes, ok := checkedMul(maximumRecords, uint64(BatchIndexEntrySize))
	if !ok {
		return 0, ConvergenceOverflow, errCalculationOverflow
	}
	maximum, ok := checkedSum(
		uint64(BatchHeaderSize),
		zstdBytes,
		blockHeaderBytes,
		indexBytes,
		uint64(BatchFooterSize),
	)
	if !ok {
		return 0, ConvergenceOverflow, errCalculationOverflow
	}
	return maximum, 0, nil
}

func sizingConfigDigest(maxRecordBytes, requiredCapacity uint64, sizing SizingConfig) [sha256.Size]byte {
	var preimage [SizingConfigPreimageSize]byte
	order := binary.LittleEndian
	order.PutUint64(preimage[SizingConfigMagicOffset:], SizingConfigMagic)
	order.PutUint64(preimage[SizingConfigMaxRecordBytesOffset:], maxRecordBytes)
	order.PutUint64(preimage[SizingConfigRequiredCapacityOffset:], requiredCapacity)
	order.PutUint64(preimage[SizingConfigBatchMaxOccupiedBytesOffset:], sizing.BatchMaxOccupiedBytes)
	order.PutUint64(preimage[SizingConfigBatchMaxAgeNanosOffset:], uint64(sizing.BatchMaxAge))
	order.PutUint64(preimage[SizingConfigZstdBlockBytesOffset:], sizing.ZstdBlockBytes)
	order.PutUint64(preimage[SizingConfigPeakOccupiedRateOffset:], sizing.Bounds.PeakOccupiedBytesPerSecond)
	order.PutUint64(preimage[SizingConfigPeakArchiveRateOffset:], sizing.Bounds.PeakArchiveBytesPerSecond)
	order.PutUint64(preimage[SizingConfigMinArchiveRateOffset:], sizing.Bounds.MinArchiveBytesPerSecond)
	order.PutUint64(preimage[SizingConfigMinObserverReplayRateOffset:], sizing.Bounds.MinObserverReplayBytesPerSecond)
	order.PutUint64(preimage[SizingConfigBurstOccupiedBytesOffset:], sizing.Bounds.BurstOccupiedBytes)
	order.PutUint64(preimage[SizingConfigMaxArchiveStallNanosOffset:], uint64(sizing.Bounds.MaxArchiveStall))
	order.PutUint64(preimage[SizingConfigMaxSyncLatencyNanosOffset:], uint64(sizing.Bounds.MaxSyncLatency))
	order.PutUint64(preimage[SizingConfigMaxRecoveryScanBytesOffset:], sizing.Bounds.MaxRecoveryScanBytes)
	order.PutUint64(preimage[SizingConfigMaxCatchUpNanosOffset:], uint64(sizing.Bounds.MaxCatchUp))
	order.PutUint64(preimage[SizingConfigMaxObserverBacklogBytesOffset:], sizing.Bounds.MaxObserverBacklogBytes)
	return sha256.Sum256(preimage[:])
}

func convergenceInputs(maxRecordBytes uint64, sizing SizingConfig) *ConvergenceError {
	bounds := sizing.Bounds
	if maxRecordBytes == 0 || sizing.BatchMaxOccupiedBytes == 0 || sizing.BatchMaxAge == 0 || sizing.ZstdBlockBytes == 0 ||
		bounds.PeakOccupiedBytesPerSecond == 0 || bounds.PeakArchiveBytesPerSecond == 0 ||
		bounds.MinArchiveBytesPerSecond == 0 || bounds.MinObserverReplayBytesPerSecond == 0 ||
		bounds.BurstOccupiedBytes == 0 || bounds.MaxRecoveryScanBytes == 0 ||
		bounds.MaxArchiveStall == 0 || bounds.MaxSyncLatency == 0 {
		return convergenceFailure(ConvergenceInvalidBound, maxRecordBytes, 0, sizing, errZeroBound)
	}
	if sizing.BatchMaxAge < 0 || bounds.MaxArchiveStall < 0 || bounds.MaxSyncLatency < 0 || bounds.MaxCatchUp < 0 {
		return convergenceFailure(ConvergenceInvalidBound, maxRecordBytes, 0, sizing, errNegativeDuration)
	}
	if sizing.ZstdBlockBytes > uint64(math.MaxInt) {
		return convergenceFailure(ConvergenceInvalidBound, maxRecordBytes, 0, sizing, errZstdBlockMemory)
	}
	if bounds.MinArchiveBytesPerSecond <= bounds.PeakArchiveBytesPerSecond {
		return convergenceFailure(ConvergenceArchiveRate, maxRecordBytes, 0, sizing, errArchiveRate)
	}
	if bounds.MinObserverReplayBytesPerSecond <= bounds.PeakArchiveBytesPerSecond {
		return convergenceFailure(ConvergenceObserverRate, maxRecordBytes, 0, sizing, errObserverRate)
	}
	if bounds.MaxCatchUp != 0 && bounds.MaxObserverBacklogBytes == 0 {
		return convergenceFailure(ConvergenceInvalidBound, maxRecordBytes, 0, sizing, errCatchUpBacklog)
	}
	return nil
}

func occupiedDuringStop(bounds Bounds) (uint64, bool) {
	durationNanos, carry := add64(uint64(bounds.MaxArchiveStall), uint64(bounds.MaxSyncLatency))
	if carry {
		return 0, false
	}

	// Calculate ceil(R*(duration/A-independent + recovery/A)) exactly.
	// The common denominator is 1e9*A.
	r := new(big.Int).SetUint64(bounds.PeakOccupiedBytesPerSecond)
	a := new(big.Int).SetUint64(bounds.MinArchiveBytesPerSecond)
	billion := new(big.Int).SetUint64(uint64(time.Second))

	numerator := new(big.Int).Mul(new(big.Int).Set(r), new(big.Int).SetUint64(durationNanos))
	numerator.Mul(numerator, a)
	recovery := new(big.Int).Mul(new(big.Int).Set(r), new(big.Int).SetUint64(bounds.MaxRecoveryScanBytes))
	recovery.Mul(recovery, billion)
	numerator.Add(numerator, recovery)
	denominator := new(big.Int).Mul(a, billion)

	quotient, remainder := new(big.Int).QuoRem(numerator, denominator, new(big.Int))
	if remainder.Sign() != 0 {
		quotient.Add(quotient, big.NewInt(1))
	}
	if !quotient.IsUint64() {
		return 0, false
	}
	return quotient.Uint64(), true
}

func convergenceFailure(reason ConvergenceReason, maxRecordBytes, maxExtent uint64, sizing SizingConfig, cause error) *ConvergenceError {
	bounds := sizing.Bounds
	return &ConvergenceError{
		Reason:                          reason,
		MaxRecordBytes:                  maxRecordBytes,
		MaxRecordExtent:                 maxExtent,
		BatchMaxOccupiedBytes:           sizing.BatchMaxOccupiedBytes,
		BatchMaxAgeNanos:                int64(sizing.BatchMaxAge),
		ZstdBlockBytes:                  sizing.ZstdBlockBytes,
		PeakOccupiedBytesPerSecond:      bounds.PeakOccupiedBytesPerSecond,
		PeakArchiveBytesPerSecond:       bounds.PeakArchiveBytesPerSecond,
		MinArchiveBytesPerSecond:        bounds.MinArchiveBytesPerSecond,
		MinObserverReplayBytesPerSecond: bounds.MinObserverReplayBytesPerSecond,
		BurstOccupiedBytes:              bounds.BurstOccupiedBytes,
		MaxArchiveStallNanos:            int64(bounds.MaxArchiveStall),
		MaxSyncLatencyNanos:             int64(bounds.MaxSyncLatency),
		MaxRecoveryScanBytes:            bounds.MaxRecoveryScanBytes,
		MaxCatchUpNanos:                 int64(bounds.MaxCatchUp),
		MaxObserverBacklogBytes:         bounds.MaxObserverBacklogBytes,
		Cause:                           cause,
	}
}

func align64(value uint64) (uint64, bool) {
	if value > math.MaxUint64-(uint64(JournalGrain)-1) {
		return 0, false
	}
	return (value + uint64(JournalGrain) - 1) &^ (uint64(JournalGrain) - 1), true
}

func nextPowerOfTwo(value uint64) (uint64, bool) {
	if value == 0 {
		return 0, false
	}
	if value&(value-1) == 0 {
		return value, true
	}
	if value > 1<<63 {
		return 0, false
	}
	value--
	value |= value >> 1
	value |= value >> 2
	value |= value >> 4
	value |= value >> 8
	value |= value >> 16
	value |= value >> 32
	return value + 1, true
}

func checkedSum(values ...uint64) (uint64, bool) {
	var sum uint64
	for _, value := range values {
		var carry bool
		sum, carry = add64(sum, value)
		if carry {
			return 0, false
		}
	}
	return sum, true
}

func add64(left, right uint64) (uint64, bool) {
	sum := left + right
	return sum, sum < left
}

func checkedMul(left, right uint64) (uint64, bool) {
	if left != 0 && right > math.MaxUint64/left {
		return 0, false
	}
	return left * right, true
}

func ceilMulDiv(left, right, divisor uint64) (uint64, bool) {
	if divisor == 0 {
		return 0, false
	}
	numerator := new(big.Int).Mul(new(big.Int).SetUint64(left), new(big.Int).SetUint64(right))
	quotient, remainder := new(big.Int).QuoRem(numerator, new(big.Int).SetUint64(divisor), new(big.Int))
	if remainder.Sign() != 0 {
		quotient.Add(quotient, big.NewInt(1))
	}
	if !quotient.IsUint64() {
		return 0, false
	}
	return quotient.Uint64(), true
}
