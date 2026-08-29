package journal

const (
	// JournalMagic is the journal-ring format-v1 magic, "pgt_jrn1".
	JournalMagic uint64 = 0x7067745f6a726e31
	// JournalFormatVersion is the only journal-ring version defined here.
	JournalFormatVersion uint32 = 1
	// JournalHeaderSize is the fixed journal-ring header size.
	JournalHeaderSize uint32 = 4096
	// JournalEndianMarker identifies little-endian serialized fields.
	JournalEndianMarker uint32 = 0x01020304
	// JournalGrain is the payload allocation grain.
	JournalGrain uint32 = 64
	// JournalTagSize is one tag-plane entry size.
	JournalTagSize uint32 = 8
	// JournalDescriptorSize is one descriptor-plane entry size.
	JournalDescriptorSize uint32 = 8
	// JournalSentinelBytes keeps full and empty cursor states distinct.
	JournalSentinelBytes uint64 = uint64(JournalGrain)
	// JournalArenaAlignment is the format-v1 payload arena file alignment.
	JournalArenaAlignment uint64 = 64 << 10
	// JournalMaxCapacity is the largest payload capacity supported by the mapper.
	// Its control bytes plus two payload mappings fit signed int and uintptr limits.
	JournalMaxCapacity uint64 = 1 << 61
	// JournalMaxSequence reserves the high tag bit for publication state.
	JournalMaxSequence Sequence = (1 << 63) - 1
)

const (
	// JournalFlagRecoveryRequired blocks role attachment until recovery commits.
	JournalFlagRecoveryRequired uint32 = 1 << 0
	// JournalKnownFlags contains every format-v1 header flag.
	JournalKnownFlags uint32 = JournalFlagRecoveryRequired
)

const (
	JournalHeaderMagicOffset              uint32 = 0
	JournalHeaderVersionOffset            uint32 = 8
	JournalHeaderSizeOffset               uint32 = 12
	JournalHeaderEndianOffset             uint32 = 16
	JournalHeaderGrainOffset              uint32 = 20
	JournalHeaderCapacityOffset           uint32 = 24
	JournalHeaderMaxRecordBytesOffset     uint32 = 32
	JournalHeaderRingEpochOffset          uint32 = 40
	JournalHeaderCreatorPIDOffset         uint32 = 56
	JournalHeaderFlagsOffset              uint32 = 60
	JournalHeaderPIDNamespaceDevOffset    uint32 = 64
	JournalHeaderPIDNamespaceInodeOffset  uint32 = 72
	JournalHeaderProducerTIDOffset        uint32 = 80
	JournalHeaderProducerGenerationOffset uint32 = 84
	JournalHeaderArchiverTIDOffset        uint32 = 88
	JournalHeaderArchiverGenerationOffset uint32 = 92
	JournalHeaderPublishLockOffset        uint32 = 96
	JournalHeaderPublishSequenceOffset    uint32 = 104
	JournalHeaderPublishPositionOffset    uint32 = 112
	JournalHeaderDurableLockOffset        uint32 = 128
	JournalHeaderDurableSequenceOffset    uint32 = 136
	JournalHeaderDurablePositionOffset    uint32 = 144
	JournalHeaderRequiredCapacityOffset   uint32 = 152
	JournalHeaderSizingDigestOffset       uint32 = 160
	JournalHeaderReservedOffset           uint32 = 192
)

const (
	// SizingConfigMagic identifies the capacity-proof digest schema, "pgt_jsz1".
	SizingConfigMagic        uint64 = 0x7067745f6a737a31
	SizingConfigPreimageSize uint32 = 128
	SizingConfigDigestSize   uint32 = 32
)

const (
	SizingConfigMagicOffset                   uint32 = 0
	SizingConfigMaxRecordBytesOffset          uint32 = 8
	SizingConfigRequiredCapacityOffset        uint32 = 16
	SizingConfigBatchMaxOccupiedBytesOffset   uint32 = 24
	SizingConfigBatchMaxAgeNanosOffset        uint32 = 32
	SizingConfigZstdBlockBytesOffset          uint32 = 40
	SizingConfigPeakOccupiedRateOffset        uint32 = 48
	SizingConfigPeakArchiveRateOffset         uint32 = 56
	SizingConfigMinArchiveRateOffset          uint32 = 64
	SizingConfigMinObserverReplayRateOffset   uint32 = 72
	SizingConfigBurstOccupiedBytesOffset      uint32 = 80
	SizingConfigMaxArchiveStallNanosOffset    uint32 = 88
	SizingConfigMaxSyncLatencyNanosOffset     uint32 = 96
	SizingConfigMaxRecoveryScanBytesOffset    uint32 = 104
	SizingConfigMaxCatchUpNanosOffset         uint32 = 112
	SizingConfigMaxObserverBacklogBytesOffset uint32 = 120
	SizingConfigReservedOffset                uint32 = 128
)

const (
	// JournalDescriptorLengthBits stores a uint32 payload length.
	JournalDescriptorLengthBits = 32
	// JournalDescriptorExtentBits stores a 24-bit grain count.
	JournalDescriptorExtentBits = 24
	// JournalDescriptorFlagsBits stores eight format-v1 flags.
	JournalDescriptorFlagsBits        = 8
	JournalDescriptorMaxLength uint64 = (1 << JournalDescriptorLengthBits) - 1
	JournalDescriptorMaxExtent uint64 = (1 << JournalDescriptorExtentBits) - 1
	JournalDescriptorMaxFlags  uint64 = (1 << JournalDescriptorFlagsBits) - 1
)

const (
	// BatchMagic is the archive-batch format-v1 magic, "pgt_jbt1".
	BatchMagic uint64 = 0x7067745f6a627431
	// BatchFooterMagic is the batch-footer format-v1 magic, "pgt_jbf1".
	BatchFooterMagic uint64 = 0x7067745f6a626631
	// BatchBlockMagic is the block-header format-v1 magic, "pgt_jbk1".
	BatchBlockMagic       uint64 = 0x7067745f6a626b31
	BatchFormatVersion    uint32 = 1
	BatchHeaderSize       uint32 = 4096
	BatchBlockHeaderSize  uint32 = 64
	BatchIndexEntrySize   uint32 = 48
	BatchFooterSize       uint32 = 256
	BatchRecordHeaderSize uint32 = 16
	BatchCodecZstandard   uint32 = 1
)

const (
	BatchHeaderMagicOffset             uint32 = 0
	BatchHeaderVersionOffset           uint32 = 8
	BatchHeaderSizeOffset              uint32 = 12
	BatchHeaderEndianOffset            uint32 = 16
	BatchHeaderCodecOffset             uint32 = 20
	BatchHeaderRingEpochOffset         uint32 = 24
	BatchHeaderBatchIDOffset           uint32 = 40
	BatchHeaderFirstSequenceOffset     uint32 = 48
	BatchHeaderEndSequenceOffset       uint32 = 56
	BatchHeaderFirstPositionOffset     uint32 = 64
	BatchHeaderNextPositionOffset      uint32 = 72
	BatchHeaderRecordCountOffset       uint32 = 80
	BatchHeaderOccupiedBytesOffset     uint32 = 88
	BatchHeaderUncompressedBytesOffset uint32 = 96
	BatchHeaderBlockMaxBytesOffset     uint32 = 104
	BatchHeaderBlockCountOffset        uint32 = 112
	BatchHeaderFlagsOffset             uint32 = 116
	BatchHeaderCreatedUnixNanosOffset  uint32 = 120
	BatchHeaderCRC32COffset            uint32 = 128
	BatchHeaderReservedOffset          uint32 = 132
)

const (
	BatchBlockMagicOffset             uint32 = 0
	BatchBlockVersionOffset           uint32 = 8
	BatchBlockHeaderSizeOffset        uint32 = 12
	BatchBlockFirstSequenceOffset     uint32 = 16
	BatchBlockEndSequenceOffset       uint32 = 24
	BatchBlockRecordCountOffset       uint32 = 32
	BatchBlockFlagsOffset             uint32 = 36
	BatchBlockUncompressedBytesOffset uint32 = 40
	BatchBlockCompressedBytesOffset   uint32 = 48
	BatchBlockPayloadCRC32COffset     uint32 = 56
	BatchBlockHeaderCRC32COffset      uint32 = 60
)

const (
	BatchIndexFirstSequenceOffset     uint32 = 0
	BatchIndexEndSequenceOffset       uint32 = 8
	BatchIndexFileOffsetOffset        uint32 = 16
	BatchIndexCompressedBytesOffset   uint32 = 24
	BatchIndexUncompressedBytesOffset uint32 = 32
	BatchIndexBlockCRC32COffset       uint32 = 40
	BatchIndexReservedOffset          uint32 = 44
)

const (
	BatchFooterMagicOffset             uint32 = 0
	BatchFooterVersionOffset           uint32 = 8
	BatchFooterSizeOffset              uint32 = 12
	BatchFooterRingEpochOffset         uint32 = 16
	BatchFooterBatchIDOffset           uint32 = 32
	BatchFooterFirstSequenceOffset     uint32 = 40
	BatchFooterEndSequenceOffset       uint32 = 48
	BatchFooterFirstPositionOffset     uint32 = 56
	BatchFooterNextPositionOffset      uint32 = 64
	BatchFooterRecordCountOffset       uint32 = 72
	BatchFooterOccupiedBytesOffset     uint32 = 80
	BatchFooterUncompressedBytesOffset uint32 = 88
	BatchFooterIndexOffsetOffset       uint32 = 96
	BatchFooterIndexCountOffset        uint32 = 104
	BatchFooterIndexEntrySizeOffset    uint32 = 108
	BatchFooterNextSequenceOffset      uint32 = 112
	BatchFooterFlagsOffset             uint32 = 120
	BatchFooterCRC32COffset            uint32 = 124
	BatchFooterContentSHA256Offset     uint32 = 128
	BatchFooterReservedOffset          uint32 = 160
)

const (
	// ArchiveHeadMagic is the archive-head format-v1 magic, "pgt_jhd1".
	ArchiveHeadMagic                uint64 = 0x7067745f6a686431
	ArchiveHeadFormatVersion        uint32 = 1
	ArchiveHeadSize                 uint32 = 4096
	ArchiveHeadFilenameBytes        uint32 = 64
	ArchiveHeadMagicOffset          uint32 = 0
	ArchiveHeadVersionOffset        uint32 = 8
	ArchiveHeadSizeOffset           uint32 = 12
	ArchiveHeadEndianOffset         uint32 = 16
	ArchiveHeadFlagsOffset          uint32 = 20
	ArchiveHeadRingEpochOffset      uint32 = 24
	ArchiveHeadLatestBatchIDOffset  uint32 = 40
	ArchiveHeadNextSequenceOffset   uint32 = 48
	ArchiveHeadNextPositionOffset   uint32 = 56
	ArchiveHeadBatchBytesOffset     uint32 = 64
	ArchiveHeadBatchSHA256Offset    uint32 = 72
	ArchiveHeadFilenameLengthOffset uint32 = 104
	ArchiveHeadFilenameOffset       uint32 = 108
	ArchiveHeadCRC32COffset         uint32 = 172
	ArchiveHeadReservedOffset       uint32 = 176
)

const (
	// RetentionFloorMagic is the retention-floor format-v1 magic, "pgt_jrf1".
	RetentionFloorMagic               uint64 = 0x7067745f6a726631
	RetentionFloorFormatVersion       uint32 = 1
	RetentionFloorSize                uint32 = 4096
	RetentionFloorMagicOffset         uint32 = 0
	RetentionFloorVersionOffset       uint32 = 8
	RetentionFloorSizeOffset          uint32 = 12
	RetentionFloorEndianOffset        uint32 = 16
	RetentionFloorFlagsOffset         uint32 = 20
	RetentionFloorRingEpochOffset     uint32 = 24
	RetentionFloorOldestBatchIDOffset uint32 = 40
	RetentionFloorCRC32COffset        uint32 = 48
	RetentionFloorReservedOffset      uint32 = 52
)

func packDescriptor(length uint32, extentGrains uint32, flags uint8) (uint64, bool) {
	if extentGrains == 0 || uint64(extentGrains) > JournalDescriptorMaxExtent || uint64(length) > uint64(extentGrains)*uint64(JournalGrain) {
		return 0, false
	}
	word := uint64(length)
	word |= uint64(extentGrains) << JournalDescriptorLengthBits
	word |= uint64(flags) << (JournalDescriptorLengthBits + JournalDescriptorExtentBits)
	return word, true
}

func unpackDescriptor(word uint64) (length uint32, extentGrains uint32, flags uint8, ok bool) {
	length = uint32(word)
	extentGrains = uint32((word >> JournalDescriptorLengthBits) & JournalDescriptorMaxExtent)
	flags = uint8(word >> (JournalDescriptorLengthBits + JournalDescriptorExtentBits))
	ok = extentGrains != 0 && uint64(length) <= uint64(extentGrains)*uint64(JournalGrain)
	return length, extentGrains, flags, ok
}

// StableTag returns the stable publication tag for sequence.
func StableTag(sequence Sequence) uint64 { return uint64(sequence) << 1 }

// WritingTag returns the overwrite-in-progress tag for sequence.
func WritingTag(sequence Sequence) uint64 { return StableTag(sequence) | 1 }
