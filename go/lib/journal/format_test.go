package journal

import (
	"encoding/binary"
	"testing"
)

func TestRingFormatV1Layout(t *testing.T) {
	fields := []struct {
		name   string
		offset uint32
		want   uint32
	}{
		{"magic", JournalHeaderMagicOffset, 0},
		{"version", JournalHeaderVersionOffset, 8},
		{"header size", JournalHeaderSizeOffset, 12},
		{"endian", JournalHeaderEndianOffset, 16},
		{"grain", JournalHeaderGrainOffset, 20},
		{"capacity", JournalHeaderCapacityOffset, 24},
		{"maximum record", JournalHeaderMaxRecordBytesOffset, 32},
		{"ring epoch", JournalHeaderRingEpochOffset, 40},
		{"creator PID", JournalHeaderCreatorPIDOffset, 56},
		{"PID namespace device", JournalHeaderPIDNamespaceDevOffset, 64},
		{"PID namespace inode", JournalHeaderPIDNamespaceInodeOffset, 72},
		{"producer TID", JournalHeaderProducerTIDOffset, 80},
		{"producer generation", JournalHeaderProducerGenerationOffset, 84},
		{"archiver TID", JournalHeaderArchiverTIDOffset, 88},
		{"archiver generation", JournalHeaderArchiverGenerationOffset, 92},
		{"publish seqlock", JournalHeaderPublishLockOffset, 96},
		{"publish sequence", JournalHeaderPublishSequenceOffset, 104},
		{"publish position", JournalHeaderPublishPositionOffset, 112},
		{"durable seqlock", JournalHeaderDurableLockOffset, 128},
		{"durable sequence", JournalHeaderDurableSequenceOffset, 136},
		{"durable position", JournalHeaderDurablePositionOffset, 144},
		{"required capacity", JournalHeaderRequiredCapacityOffset, 152},
		{"sizing digest", JournalHeaderSizingDigestOffset, 160},
		{"reserved", JournalHeaderReservedOffset, 192},
	}
	for _, field := range fields {
		if field.offset != field.want {
			t.Errorf("%s offset = %d, want %d", field.name, field.offset, field.want)
		}
	}
	if JournalHeaderSize != 4096 || JournalGrain != 64 || JournalTagSize != 8 || JournalDescriptorSize != 8 || JournalArenaAlignment != 64<<10 {
		t.Fatalf("unexpected ring geometry: header=%d grain=%d tag=%d descriptor=%d alignment=%d", JournalHeaderSize, JournalGrain, JournalTagSize, JournalDescriptorSize, JournalArenaAlignment)
	}
}

func TestRingFormatV1GoldenPrefix(t *testing.T) {
	got := make([]byte, 40)
	binary.LittleEndian.PutUint64(got[JournalHeaderMagicOffset:], JournalMagic)
	binary.LittleEndian.PutUint32(got[JournalHeaderVersionOffset:], JournalFormatVersion)
	binary.LittleEndian.PutUint32(got[JournalHeaderSizeOffset:], JournalHeaderSize)
	binary.LittleEndian.PutUint32(got[JournalHeaderEndianOffset:], JournalEndianMarker)
	binary.LittleEndian.PutUint32(got[JournalHeaderGrainOffset:], JournalGrain)
	binary.LittleEndian.PutUint64(got[JournalHeaderCapacityOffset:], 1<<20)
	binary.LittleEndian.PutUint64(got[JournalHeaderMaxRecordBytesOffset:], 4096)
	want := []byte{
		0x31, 0x6e, 0x72, 0x6a, 0x5f, 0x74, 0x67, 0x70,
		0x01, 0x00, 0x00, 0x00, 0x00, 0x10, 0x00, 0x00,
		0x04, 0x03, 0x02, 0x01, 0x40, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x10, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x10, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	}
	if string(got) != string(want) {
		t.Fatalf("ring prefix = %x, want %x", got, want)
	}
}

func TestSizingConfigFormatV1Layout(t *testing.T) {
	if SizingConfigPreimageSize != 128 || SizingConfigDigestSize != 32 {
		t.Fatalf("unexpected sizing format: preimage=%d digest=%d", SizingConfigPreimageSize, SizingConfigDigestSize)
	}
	fields := []struct {
		name string
		got  uint32
		want uint32
	}{
		{"magic", SizingConfigMagicOffset, 0},
		{"maximum record", SizingConfigMaxRecordBytesOffset, 8},
		{"required capacity", SizingConfigRequiredCapacityOffset, 16},
		{"batch maximum", SizingConfigBatchMaxOccupiedBytesOffset, 24},
		{"batch maximum age", SizingConfigBatchMaxAgeNanosOffset, 32},
		{"block maximum", SizingConfigZstdBlockBytesOffset, 40},
		{"occupied rate", SizingConfigPeakOccupiedRateOffset, 48},
		{"archive peak", SizingConfigPeakArchiveRateOffset, 56},
		{"archive minimum", SizingConfigMinArchiveRateOffset, 64},
		{"observer minimum", SizingConfigMinObserverReplayRateOffset, 72},
		{"burst", SizingConfigBurstOccupiedBytesOffset, 80},
		{"stall", SizingConfigMaxArchiveStallNanosOffset, 88},
		{"sync", SizingConfigMaxSyncLatencyNanosOffset, 96},
		{"recovery", SizingConfigMaxRecoveryScanBytesOffset, 104},
		{"catch up", SizingConfigMaxCatchUpNanosOffset, 112},
		{"backlog", SizingConfigMaxObserverBacklogBytesOffset, 120},
		{"reserved", SizingConfigReservedOffset, 128},
	}
	for _, field := range fields {
		if field.got != field.want {
			t.Errorf("%s offset = %d, want %d", field.name, field.got, field.want)
		}
	}
}

func TestBatchFormatV1Layout(t *testing.T) {
	if BatchHeaderSize != 4096 || BatchBlockHeaderSize != 64 || BatchIndexEntrySize != 48 || BatchFooterSize != 256 || BatchRecordHeaderSize != 16 {
		t.Fatalf("unexpected batch geometry: header=%d block=%d index=%d footer=%d record=%d", BatchHeaderSize, BatchBlockHeaderSize, BatchIndexEntrySize, BatchFooterSize, BatchRecordHeaderSize)
	}
	fields := []struct {
		name string
		got  uint32
		want uint32
	}{
		{"header epoch", BatchHeaderRingEpochOffset, 24},
		{"header first sequence", BatchHeaderFirstSequenceOffset, 48},
		{"header next position", BatchHeaderNextPositionOffset, 72},
		{"header CRC", BatchHeaderCRC32COffset, 128},
		{"block first sequence", BatchBlockFirstSequenceOffset, 16},
		{"block compressed bytes", BatchBlockCompressedBytesOffset, 48},
		{"block header CRC", BatchBlockHeaderCRC32COffset, 60},
		{"index file offset", BatchIndexFileOffsetOffset, 16},
		{"index block CRC", BatchIndexBlockCRC32COffset, 40},
		{"footer epoch", BatchFooterRingEpochOffset, 16},
		{"footer next position", BatchFooterNextPositionOffset, 64},
		{"footer index offset", BatchFooterIndexOffsetOffset, 96},
		{"footer next sequence", BatchFooterNextSequenceOffset, 112},
		{"footer CRC", BatchFooterCRC32COffset, 124},
		{"footer SHA-256", BatchFooterContentSHA256Offset, 128},
		{"footer reserved", BatchFooterReservedOffset, 160},
	}
	for _, field := range fields {
		if field.got != field.want {
			t.Errorf("%s offset = %d, want %d", field.name, field.got, field.want)
		}
	}
}

func TestCatalogFormatV1Layout(t *testing.T) {
	if ArchiveHeadSize != 4096 || ArchiveHeadFilenameBytes != 64 || RetentionFloorSize != 4096 {
		t.Fatalf("unexpected catalog geometry: head=%d filename=%d floor=%d", ArchiveHeadSize, ArchiveHeadFilenameBytes, RetentionFloorSize)
	}
	if ArchiveHeadNextSequenceOffset != 48 || ArchiveHeadNextPositionOffset != 56 || ArchiveHeadFilenameOffset != 108 || ArchiveHeadCRC32COffset != 172 {
		t.Fatal("archive head offsets changed")
	}
	if RetentionFloorOldestBatchIDOffset != 40 || RetentionFloorCRC32COffset != 48 || RetentionFloorReservedOffset != 52 {
		t.Fatal("retention floor offsets changed")
	}
}

func TestPublicationTags(t *testing.T) {
	for _, sequence := range []Sequence{1, 2, JournalMaxSequence} {
		stable := StableTag(sequence)
		writing := WritingTag(sequence)
		if stable == 0 || stable&1 != 0 {
			t.Fatalf("stable tag for %d = %#x", sequence, stable)
		}
		if writing != stable|1 || writing&1 == 0 {
			t.Fatalf("writing tag for %d = %#x", sequence, writing)
		}
	}
}
