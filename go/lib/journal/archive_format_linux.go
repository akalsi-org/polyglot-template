//go:build linux && (amd64 || arm64)

package journal

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
)

const (
	archiveHeadName      = "archive.head"
	archiveHeadTempName  = "archive.head.tmp"
	archiveLockName      = "archive.lock"
	retentionFloorName   = "retention.floor"
	batchFilenamePrefix  = "batch-"
	batchFilenameSuffix  = ".jrn.zst"
	batchSequenceDigits  = 20
	batchTemporarySuffix = ".tmp"
)

var archiveCRC32CTable = crc32.MakeTable(crc32.Castagnoli)

type archiveBatchMeta struct {
	epoch             RingEpoch
	id                BatchID
	first             ringCursor
	next              ringCursor
	recordCount       uint64
	occupiedBytes     uint64
	uncompressedBytes uint64
	blockMaxBytes     uint64
	blockCount        uint32
	createdUnixNanos  int64
	indexOffset       uint64
	filename          string
	fileBytes         uint64
	fileSHA256        [sha256.Size]byte
	contentSHA256     [sha256.Size]byte
	recordsSHA256     [sha256.Size]byte
}

type archiveHead struct {
	epoch       RingEpoch
	latest      BatchID
	next        ringCursor
	batchBytes  uint64
	batchSHA256 [sha256.Size]byte
	filename    string
}

type archiveBlockIndex struct {
	first             Sequence
	end               Sequence
	fileOffset        uint64
	compressedBytes   uint64
	uncompressedBytes uint64
	payloadCRC32C     uint32
}

type archiveRecord struct {
	cursor       ringCursor
	next         ringCursor
	extent       uint64
	payloadStart int
	payloadBytes int
	payload      []byte
}

type archiveBatchScratch struct {
	block      []byte
	compressed []byte
	indexes    []archiveBlockIndex
}

func batchFilename(first Sequence) (string, error) {
	if first == 0 || first > JournalMaxSequence {
		return "", ErrFormat
	}
	return fmt.Sprintf("%s%020d%s", batchFilenamePrefix, first, batchFilenameSuffix), nil
}

func parseBatchFilename(name string) (Sequence, bool) {
	if len(name) != len(batchFilenamePrefix)+batchSequenceDigits+len(batchFilenameSuffix) ||
		!strings.HasPrefix(name, batchFilenamePrefix) || !strings.HasSuffix(name, batchFilenameSuffix) {
		return 0, false
	}
	digits := name[len(batchFilenamePrefix) : len(batchFilenamePrefix)+batchSequenceDigits]
	value, err := strconv.ParseUint(digits, 10, 64)
	if err != nil || value == 0 || value > uint64(JournalMaxSequence) {
		return 0, false
	}
	return Sequence(value), true
}

func crc32c(bytes []byte, fieldOffset uint32) uint32 {
	hash := crc32.New(archiveCRC32CTable)
	_, _ = hash.Write(bytes[:fieldOffset])
	_, _ = hash.Write([]byte{0, 0, 0, 0})
	_, _ = hash.Write(bytes[fieldOffset+4:])
	return hash.Sum32()
}

func validCRC32C(bytes []byte, fieldOffset uint32) bool {
	return len(bytes) >= int(fieldOffset)+4 && get32(bytes, fieldOffset) == crc32c(bytes, fieldOffset)
}

func putCRC32C(bytes []byte, fieldOffset uint32) {
	put32(bytes, fieldOffset, 0)
	put32(bytes, fieldOffset, crc32.Checksum(bytes, archiveCRC32CTable))
}

type zeroRange struct {
	start int64
	end   int64
}

func sha256ReaderAt(reader io.ReaderAt, size int64, zeroes ...zeroRange) ([sha256.Size]byte, error) {
	var result [sha256.Size]byte
	if size < 0 {
		return result, ErrFormat
	}
	hash := sha256.New()
	buffer := make([]byte, 64<<10)
	for offset := int64(0); offset < size; {
		length := int64(len(buffer))
		if length > size-offset {
			length = size - offset
		}
		chunk := buffer[:length]
		if _, err := reader.ReadAt(chunk, offset); err != nil && !errors.Is(err, io.EOF) {
			return result, err
		}
		for _, span := range zeroes {
			start := span.start
			if start < offset {
				start = offset
			}
			end := span.end
			if end > offset+length {
				end = offset + length
			}
			if start < end {
				clear(chunk[start-offset : end-offset])
			}
		}
		_, _ = hash.Write(chunk)
		offset += length
	}
	copy(result[:], hash.Sum(nil))
	return result, nil
}

func batchContentSHA256(reader io.ReaderAt, size int64) ([sha256.Size]byte, error) {
	footerOffset := size - int64(BatchFooterSize)
	if footerOffset < int64(BatchHeaderSize) {
		return [sha256.Size]byte{}, ErrFormat
	}
	return sha256ReaderAt(
		reader,
		size,
		zeroRange{start: footerOffset + int64(BatchFooterCRC32COffset), end: footerOffset + int64(BatchFooterCRC32COffset) + 4},
		zeroRange{start: footerOffset + int64(BatchFooterContentSHA256Offset), end: footerOffset + int64(BatchFooterContentSHA256Offset) + sha256.Size},
	)
}

func writeArchiveRecordHash(hash io.Writer, sequence Sequence, payload []byte) {
	var header [BatchRecordHeaderSize]byte
	put64(header[:], 0, uint64(sequence))
	put32(header[:], 8, uint32(len(payload)))
	put32(header[:], 12, crc32.Checksum(payload, archiveCRC32CTable))
	_, _ = hash.Write(header[:])
	_, _ = hash.Write(payload)
}

func writeArchiveBytes(file archiveFile, bytes []byte) error {
	for len(bytes) != 0 {
		written, err := file.Write(bytes)
		if err != nil {
			return err
		}
		if written <= 0 || written > len(bytes) {
			return io.ErrShortWrite
		}
		bytes = bytes[written:]
	}
	return nil
}

func writeArchiveBatch(
	file archiveFile,
	epoch RingEpoch,
	proof durableProof,
	records []archiveRecord,
	blockBytes uint64,
	level int,
	now time.Time,
	scratch *archiveBatchScratch,
) (archiveBatchMeta, error) {
	if file == nil || scratch == nil || allZero(epoch[:]) || proof.region == nil || proof.from == proof.to ||
		!proof.verified || len(records) == 0 || blockBytes == 0 || blockBytes > uint64(math.MaxInt) {
		return archiveBatchMeta{}, ErrFormat
	}
	if proof.from != records[0].cursor || proof.to != records[len(records)-1].next ||
		uint64(len(records)) != uint64(proof.to.sequence-proof.from.sequence) {
		return archiveBatchMeta{}, ErrFormat
	}
	filename, err := batchFilename(proof.from.sequence)
	if err != nil {
		return archiveBatchMeta{}, err
	}
	options := []zstd.EOption{zstd.WithEncoderCRC(false), zstd.WithEncoderConcurrency(1)}
	if level != 0 {
		options = append(options, zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(level)))
	}
	encoder, err := zstd.NewWriter(nil, options...)
	if err != nil {
		return archiveBatchMeta{}, fmt.Errorf("create Zstandard encoder: %w", err)
	}
	defer encoder.Close()
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return archiveBatchMeta{}, err
	}
	if err := writeArchiveBytes(file, make([]byte, BatchHeaderSize)); err != nil {
		return archiveBatchMeta{}, err
	}
	scratch.block = scratch.block[:0]
	scratch.compressed = scratch.compressed[:0]
	scratch.indexes = scratch.indexes[:0]
	var blockFirst Sequence
	var blockRecords uint32
	var totalUncompressed uint64
	var occupied uint64
	recordsHash := sha256.New()
	fileOffset := uint64(BatchHeaderSize)
	flushBlock := func(end Sequence) error {
		if blockRecords == 0 {
			return nil
		}
		scratch.compressed = encoder.EncodeAll(scratch.block, scratch.compressed[:0])
		header := make([]byte, BatchBlockHeaderSize)
		put64(header, BatchBlockMagicOffset, BatchBlockMagic)
		put32(header, BatchBlockVersionOffset, BatchFormatVersion)
		put32(header, BatchBlockHeaderSizeOffset, BatchBlockHeaderSize)
		put64(header, BatchBlockFirstSequenceOffset, uint64(blockFirst))
		put64(header, BatchBlockEndSequenceOffset, uint64(end))
		put32(header, BatchBlockRecordCountOffset, blockRecords)
		put64(header, BatchBlockUncompressedBytesOffset, uint64(len(scratch.block)))
		put64(header, BatchBlockCompressedBytesOffset, uint64(len(scratch.compressed)))
		payloadCRC := crc32.Checksum(scratch.block, archiveCRC32CTable)
		put32(header, BatchBlockPayloadCRC32COffset, payloadCRC)
		putCRC32C(header, BatchBlockHeaderCRC32COffset)
		if err := writeArchiveBytes(file, header); err != nil {
			return err
		}
		if err := writeArchiveBytes(file, scratch.compressed); err != nil {
			return err
		}
		scratch.indexes = append(scratch.indexes, archiveBlockIndex{
			first: blockFirst, end: end, fileOffset: fileOffset,
			compressedBytes: uint64(len(scratch.compressed)), uncompressedBytes: uint64(len(scratch.block)),
			payloadCRC32C: payloadCRC,
		})
		fileOffset += uint64(BatchBlockHeaderSize) + uint64(len(scratch.compressed))
		totalUncompressed += uint64(len(scratch.block))
		scratch.block = scratch.block[:0]
		blockRecords = 0
		return nil
	}
	expected := proof.from
	for _, record := range records {
		if record.cursor != expected || record.next.sequence != record.cursor.sequence+1 ||
			record.next.position <= record.cursor.position || record.extent != record.next.position-record.cursor.position ||
			uint64(len(record.payload)) > math.MaxUint32 {
			return archiveBatchMeta{}, ErrFormat
		}
		expected = record.next
		encodedBytes := uint64(BatchRecordHeaderSize) + uint64(len(record.payload))
		if encodedBytes > blockBytes || encodedBytes > uint64(math.MaxInt) {
			return archiveBatchMeta{}, ErrTooLarge
		}
		if blockRecords != 0 && (uint64(len(scratch.block))+encodedBytes > blockBytes || blockRecords == math.MaxUint32) {
			if err := flushBlock(record.cursor.sequence); err != nil {
				return archiveBatchMeta{}, err
			}
		}
		if blockRecords == 0 {
			blockFirst = record.cursor.sequence
		}
		start := len(scratch.block)
		scratch.block = append(scratch.block, make([]byte, BatchRecordHeaderSize)...)
		put64(scratch.block[start:], 0, uint64(record.cursor.sequence))
		put32(scratch.block[start:], 8, uint32(len(record.payload)))
		put32(scratch.block[start:], 12, crc32.Checksum(record.payload, archiveCRC32CTable))
		scratch.block = append(scratch.block, record.payload...)
		writeArchiveRecordHash(recordsHash, record.cursor.sequence, record.payload)
		blockRecords++
		occupied += record.extent
	}
	if err := flushBlock(proof.to.sequence); err != nil {
		return archiveBatchMeta{}, err
	}
	if expected != proof.to || len(scratch.indexes) == 0 || len(scratch.indexes) > math.MaxUint32 {
		return archiveBatchMeta{}, ErrFormat
	}
	indexOffset := fileOffset
	for _, index := range scratch.indexes {
		entry := make([]byte, BatchIndexEntrySize)
		put64(entry, BatchIndexFirstSequenceOffset, uint64(index.first))
		put64(entry, BatchIndexEndSequenceOffset, uint64(index.end))
		put64(entry, BatchIndexFileOffsetOffset, index.fileOffset)
		put64(entry, BatchIndexCompressedBytesOffset, index.compressedBytes)
		put64(entry, BatchIndexUncompressedBytesOffset, index.uncompressedBytes)
		put32(entry, BatchIndexBlockCRC32COffset, index.payloadCRC32C)
		if err := writeArchiveBytes(file, entry); err != nil {
			return archiveBatchMeta{}, err
		}
		fileOffset += uint64(BatchIndexEntrySize)
	}
	meta := archiveBatchMeta{
		epoch: epoch, id: BatchID(proof.from.sequence), first: proof.from, next: proof.to,
		recordCount: uint64(len(records)), occupiedBytes: occupied, uncompressedBytes: totalUncompressed,
		blockMaxBytes: blockBytes, blockCount: uint32(len(scratch.indexes)), createdUnixNanos: now.UnixNano(),
		indexOffset: indexOffset, filename: filename,
	}
	copy(meta.recordsSHA256[:], recordsHash.Sum(nil))
	footer := make([]byte, BatchFooterSize)
	encodeBatchFooter(footer, meta)
	if err := writeArchiveBytes(file, footer); err != nil {
		return archiveBatchMeta{}, err
	}
	fileOffset += uint64(BatchFooterSize)
	header := make([]byte, BatchHeaderSize)
	encodeBatchHeader(header, meta)
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return archiveBatchMeta{}, err
	}
	if err := writeArchiveBytes(file, header); err != nil {
		return archiveBatchMeta{}, err
	}
	contentSHA, err := batchContentSHA256(file, int64(fileOffset))
	if err != nil {
		return archiveBatchMeta{}, err
	}
	meta.contentSHA256 = contentSHA
	copy(footer[BatchFooterContentSHA256Offset:], contentSHA[:])
	putCRC32C(footer, BatchFooterCRC32COffset)
	if _, err := file.Seek(int64(fileOffset)-int64(BatchFooterSize), io.SeekStart); err != nil {
		return archiveBatchMeta{}, err
	}
	if err := writeArchiveBytes(file, footer); err != nil {
		return archiveBatchMeta{}, err
	}
	meta.fileBytes = fileOffset
	meta.fileSHA256, err = sha256ReaderAt(file, int64(fileOffset))
	if err != nil {
		return archiveBatchMeta{}, err
	}
	return meta, nil
}

func buildArchiveBatch(epoch RingEpoch, proof durableProof, records []ringRecord, blockBytes uint64, level int, now time.Time) ([]byte, archiveBatchMeta, error) {
	if allZero(epoch[:]) || proof.region == nil || proof.from == proof.to || !proof.verified || len(records) == 0 || blockBytes == 0 || blockBytes > uint64(math.MaxInt) {
		return nil, archiveBatchMeta{}, ErrFormat
	}
	if proof.from.sequence != records[0].cursor.sequence || proof.from.position != records[0].cursor.position ||
		proof.to != records[len(records)-1].next || uint64(len(records)) != uint64(proof.to.sequence-proof.from.sequence) {
		return nil, archiveBatchMeta{}, ErrFormat
	}
	filename, err := batchFilename(proof.from.sequence)
	if err != nil {
		return nil, archiveBatchMeta{}, err
	}
	options := []zstd.EOption{zstd.WithEncoderCRC(false), zstd.WithEncoderConcurrency(1)}
	if level != 0 {
		options = append(options, zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(level)))
	}
	encoder, err := zstd.NewWriter(nil, options...)
	if err != nil {
		return nil, archiveBatchMeta{}, fmt.Errorf("create Zstandard encoder: %w", err)
	}
	defer encoder.Close()

	var output bytes.Buffer
	output.Grow(int(BatchHeaderSize))
	output.Write(make([]byte, BatchHeaderSize))
	indexes := make([]archiveBlockIndex, 0, len(records))
	var block []byte
	var blockFirst Sequence
	var blockRecords uint32
	var totalUncompressed uint64
	var occupied uint64
	recordsHash := sha256.New()

	flushBlock := func(end Sequence) error {
		if blockRecords == 0 {
			return nil
		}
		compressed := encoder.EncodeAll(block, nil)
		header := make([]byte, BatchBlockHeaderSize)
		put64(header, BatchBlockMagicOffset, BatchBlockMagic)
		put32(header, BatchBlockVersionOffset, BatchFormatVersion)
		put32(header, BatchBlockHeaderSizeOffset, BatchBlockHeaderSize)
		put64(header, BatchBlockFirstSequenceOffset, uint64(blockFirst))
		put64(header, BatchBlockEndSequenceOffset, uint64(end))
		put32(header, BatchBlockRecordCountOffset, blockRecords)
		put64(header, BatchBlockUncompressedBytesOffset, uint64(len(block)))
		put64(header, BatchBlockCompressedBytesOffset, uint64(len(compressed)))
		payloadCRC := crc32.Checksum(block, archiveCRC32CTable)
		put32(header, BatchBlockPayloadCRC32COffset, payloadCRC)
		putCRC32C(header, BatchBlockHeaderCRC32COffset)
		offset := uint64(output.Len())
		output.Write(header)
		output.Write(compressed)
		indexes = append(indexes, archiveBlockIndex{
			first:             blockFirst,
			end:               end,
			fileOffset:        offset,
			compressedBytes:   uint64(len(compressed)),
			uncompressedBytes: uint64(len(block)),
			payloadCRC32C:     payloadCRC,
		})
		totalUncompressed += uint64(len(block))
		block = block[:0]
		blockRecords = 0
		return nil
	}

	expectedRecordCursor := proof.from
	for _, record := range records {
		if record.region != proof.region || record.cursor != expectedRecordCursor || record.cursor.sequence == 0 || record.length != uint32(len(record.bytes)) {
			return nil, archiveBatchMeta{}, ErrFormat
		}
		expectedRecordCursor = record.next
		encodedBytes := uint64(BatchRecordHeaderSize) + uint64(record.length)
		if encodedBytes > blockBytes || encodedBytes > uint64(math.MaxInt) {
			return nil, archiveBatchMeta{}, ErrTooLarge
		}
		if blockRecords != 0 && (uint64(len(block))+encodedBytes > blockBytes || blockRecords == math.MaxUint32) {
			if err := flushBlock(record.cursor.sequence); err != nil {
				return nil, archiveBatchMeta{}, err
			}
		}
		if blockRecords == 0 {
			blockFirst = record.cursor.sequence
			initialCapacity := blockBytes
			if initialCapacity > 64<<10 {
				initialCapacity = 64 << 10
			}
			block = make([]byte, 0, int(initialCapacity))
		}
		start := len(block)
		block = append(block, make([]byte, BatchRecordHeaderSize)...)
		put64(block[start:], 0, uint64(record.cursor.sequence))
		put32(block[start:], 8, record.length)
		put32(block[start:], 12, crc32.Checksum(record.bytes, archiveCRC32CTable))
		block = append(block, record.bytes...)
		writeArchiveRecordHash(recordsHash, record.cursor.sequence, record.bytes)
		blockRecords++
		occupied += record.extent
	}
	if err := flushBlock(proof.to.sequence); err != nil {
		return nil, archiveBatchMeta{}, err
	}
	if len(indexes) == 0 || len(indexes) > math.MaxUint32 {
		return nil, archiveBatchMeta{}, ErrFormat
	}
	indexOffset := uint64(output.Len())
	for _, index := range indexes {
		entry := make([]byte, BatchIndexEntrySize)
		put64(entry, BatchIndexFirstSequenceOffset, uint64(index.first))
		put64(entry, BatchIndexEndSequenceOffset, uint64(index.end))
		put64(entry, BatchIndexFileOffsetOffset, index.fileOffset)
		put64(entry, BatchIndexCompressedBytesOffset, index.compressedBytes)
		put64(entry, BatchIndexUncompressedBytesOffset, index.uncompressedBytes)
		put32(entry, BatchIndexBlockCRC32COffset, index.payloadCRC32C)
		output.Write(entry)
	}

	meta := archiveBatchMeta{
		epoch:             epoch,
		id:                BatchID(proof.from.sequence),
		first:             proof.from,
		next:              proof.to,
		recordCount:       uint64(len(records)),
		occupiedBytes:     occupied,
		uncompressedBytes: totalUncompressed,
		blockMaxBytes:     blockBytes,
		blockCount:        uint32(len(indexes)),
		createdUnixNanos:  now.UnixNano(),
		indexOffset:       indexOffset,
		filename:          filename,
	}
	copy(meta.recordsSHA256[:], recordsHash.Sum(nil))
	header := output.Bytes()[:BatchHeaderSize]
	encodeBatchHeader(header, meta)
	footer := make([]byte, BatchFooterSize)
	encodeBatchFooter(footer, meta)
	output.Write(footer)
	file := output.Bytes()
	footerOffset := len(file) - int(BatchFooterSize)
	contentSHA, err := batchContentSHA256(bytes.NewReader(file), int64(len(file)))
	if err != nil {
		return nil, archiveBatchMeta{}, err
	}
	meta.contentSHA256 = contentSHA
	copy(file[footerOffset+int(BatchFooterContentSHA256Offset):], meta.contentSHA256[:])
	putCRC32C(file[footerOffset:], BatchFooterCRC32COffset)
	meta.fileBytes = uint64(len(file))
	meta.fileSHA256 = sha256.Sum256(file)
	return file, meta, nil
}

func encodeBatchHeader(header []byte, meta archiveBatchMeta) {
	clear(header)
	put64(header, BatchHeaderMagicOffset, BatchMagic)
	put32(header, BatchHeaderVersionOffset, BatchFormatVersion)
	put32(header, BatchHeaderSizeOffset, BatchHeaderSize)
	put32(header, BatchHeaderEndianOffset, JournalEndianMarker)
	put32(header, BatchHeaderCodecOffset, BatchCodecZstandard)
	copy(header[BatchHeaderRingEpochOffset:], meta.epoch[:])
	put64(header, BatchHeaderBatchIDOffset, uint64(meta.id))
	put64(header, BatchHeaderFirstSequenceOffset, uint64(meta.first.sequence))
	put64(header, BatchHeaderEndSequenceOffset, uint64(meta.next.sequence))
	put64(header, BatchHeaderFirstPositionOffset, meta.first.position)
	put64(header, BatchHeaderNextPositionOffset, meta.next.position)
	put64(header, BatchHeaderRecordCountOffset, meta.recordCount)
	put64(header, BatchHeaderOccupiedBytesOffset, meta.occupiedBytes)
	put64(header, BatchHeaderUncompressedBytesOffset, meta.uncompressedBytes)
	put64(header, BatchHeaderBlockMaxBytesOffset, meta.blockMaxBytes)
	put32(header, BatchHeaderBlockCountOffset, meta.blockCount)
	put64(header, BatchHeaderCreatedUnixNanosOffset, uint64(meta.createdUnixNanos))
	putCRC32C(header, BatchHeaderCRC32COffset)
}

func encodeBatchFooter(footer []byte, meta archiveBatchMeta) {
	clear(footer)
	put64(footer, BatchFooterMagicOffset, BatchFooterMagic)
	put32(footer, BatchFooterVersionOffset, BatchFormatVersion)
	put32(footer, BatchFooterSizeOffset, BatchFooterSize)
	copy(footer[BatchFooterRingEpochOffset:], meta.epoch[:])
	put64(footer, BatchFooterBatchIDOffset, uint64(meta.id))
	put64(footer, BatchFooterFirstSequenceOffset, uint64(meta.first.sequence))
	put64(footer, BatchFooterEndSequenceOffset, uint64(meta.next.sequence))
	put64(footer, BatchFooterFirstPositionOffset, meta.first.position)
	put64(footer, BatchFooterNextPositionOffset, meta.next.position)
	put64(footer, BatchFooterRecordCountOffset, meta.recordCount)
	put64(footer, BatchFooterOccupiedBytesOffset, meta.occupiedBytes)
	put64(footer, BatchFooterUncompressedBytesOffset, meta.uncompressedBytes)
	put64(footer, BatchFooterIndexOffsetOffset, meta.indexOffset)
	put32(footer, BatchFooterIndexCountOffset, meta.blockCount)
	put32(footer, BatchFooterIndexEntrySizeOffset, BatchIndexEntrySize)
	put64(footer, BatchFooterNextSequenceOffset, uint64(meta.next.sequence))
}

func readArchiveBatch(path string, expectedEpoch RingEpoch) (archiveBatchMeta, error) {
	return readArchiveBatchWithOps(path, expectedEpoch, math.MaxUint64, uint64(math.MaxInt), defaultArchiveFileOps())
}

func readArchiveBatchWithOps(
	path string,
	expectedEpoch RingEpoch,
	maximumBytes uint64,
	maximumBlockBytes uint64,
	ops archiveFileOps,
) (archiveBatchMeta, error) {
	return readArchiveBatchWithRecoveryBoundOps(
		path,
		expectedEpoch,
		maximumBytes,
		maximumBlockBytes,
		math.MaxUint64,
		ops,
	)
}

func readArchiveBatchWithRecoveryBoundOps(
	path string,
	expectedEpoch RingEpoch,
	maximumBytes uint64,
	maximumBlockBytes uint64,
	remainingRecoveryBytes uint64,
	ops archiveFileOps,
) (archiveBatchMeta, error) {
	file, err := ops.openFile(path, os.O_RDONLY, 0)
	if err != nil {
		return archiveBatchMeta{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return archiveBatchMeta{}, err
	}
	minimum := uint64(BatchHeaderSize) + uint64(BatchBlockHeaderSize) + uint64(BatchIndexEntrySize) + uint64(BatchFooterSize)
	if !info.Mode().IsRegular() || info.Size() < 0 || uint64(info.Size()) < minimum || uint64(info.Size()) > maximumBytes {
		return archiveBatchMeta{}, archiveCorrupt(path, ErrFormat)
	}
	if uint64(info.Size()) > remainingRecoveryBytes {
		return archiveBatchMeta{}, fmt.Errorf(
			"%w: archive recovery scan exceeds its configured byte bound",
			ErrConvergence,
		)
	}
	meta, err := parseArchiveBatchReaderBounded(file, uint64(info.Size()), filepath.Base(path), expectedEpoch, maximumBlockBytes)
	if err != nil {
		return archiveBatchMeta{}, archiveCorrupt(path, err)
	}
	return meta, nil
}

func readArchiveObject(reader io.ReaderAt, offset uint64, size uint32) ([]byte, error) {
	if offset > math.MaxInt64 || uint64(size) > math.MaxInt64-offset {
		return nil, ErrFormat
	}
	bytes := make([]byte, size)
	if _, err := reader.ReadAt(bytes, int64(offset)); err != nil {
		return nil, err
	}
	return bytes, nil
}

func parseArchiveBatchReader(
	reader io.ReaderAt,
	fileBytes uint64,
	name string,
	expectedEpoch RingEpoch,
) (archiveBatchMeta, error) {
	return parseArchiveBatchReaderBounded(reader, fileBytes, name, expectedEpoch, uint64(math.MaxInt))
}

func parseArchiveBatchReaderBounded(
	reader io.ReaderAt,
	fileBytes uint64,
	name string,
	expectedEpoch RingEpoch,
	maximumBlockBytes uint64,
) (archiveBatchMeta, error) {
	minimum := uint64(BatchHeaderSize) + uint64(BatchBlockHeaderSize) + uint64(BatchIndexEntrySize) + uint64(BatchFooterSize)
	if reader == nil || fileBytes < minimum || fileBytes > math.MaxInt64 {
		return archiveBatchMeta{}, ErrFormat
	}
	header, err := readArchiveObject(reader, 0, BatchHeaderSize)
	if err != nil {
		return archiveBatchMeta{}, err
	}
	footerOffset := fileBytes - uint64(BatchFooterSize)
	footer, err := readArchiveObject(reader, footerOffset, BatchFooterSize)
	if err != nil {
		return archiveBatchMeta{}, err
	}
	if get64(header, BatchHeaderMagicOffset) != BatchMagic || get64(footer, BatchFooterMagicOffset) != BatchFooterMagic {
		return archiveBatchMeta{}, ErrFormat
	}
	if get32(header, BatchHeaderVersionOffset) != BatchFormatVersion || get32(footer, BatchFooterVersionOffset) != BatchFormatVersion {
		return archiveBatchMeta{}, ErrFormatVersion
	}
	if get32(header, BatchHeaderSizeOffset) != BatchHeaderSize || get32(header, BatchHeaderEndianOffset) != JournalEndianMarker ||
		get32(header, BatchHeaderCodecOffset) != BatchCodecZstandard || get32(header, BatchHeaderFlagsOffset) != 0 ||
		get32(footer, BatchFooterSizeOffset) != BatchFooterSize || get32(footer, BatchFooterFlagsOffset) != 0 ||
		get32(footer, BatchFooterIndexEntrySizeOffset) != BatchIndexEntrySize ||
		!allZero(header[BatchHeaderReservedOffset:]) || !allZero(footer[BatchFooterReservedOffset:]) ||
		!validCRC32C(header, BatchHeaderCRC32COffset) || !validCRC32C(footer, BatchFooterCRC32COffset) {
		return archiveBatchMeta{}, ErrFormat
	}
	var epoch RingEpoch
	copy(epoch[:], header[BatchHeaderRingEpochOffset:BatchHeaderRingEpochOffset+16])
	if allZero(epoch[:]) || (!allZero(expectedEpoch[:]) && epoch != expectedEpoch) ||
		!bytes.Equal(footer[BatchFooterRingEpochOffset:BatchFooterRingEpochOffset+16], epoch[:]) {
		return archiveBatchMeta{}, ErrFormat
	}
	meta := archiveBatchMeta{
		epoch:             epoch,
		id:                BatchID(get64(header, BatchHeaderBatchIDOffset)),
		first:             ringCursor{sequence: Sequence(get64(header, BatchHeaderFirstSequenceOffset)), position: get64(header, BatchHeaderFirstPositionOffset)},
		next:              ringCursor{sequence: Sequence(get64(header, BatchHeaderEndSequenceOffset)), position: get64(header, BatchHeaderNextPositionOffset)},
		recordCount:       get64(header, BatchHeaderRecordCountOffset),
		occupiedBytes:     get64(header, BatchHeaderOccupiedBytesOffset),
		uncompressedBytes: get64(header, BatchHeaderUncompressedBytesOffset),
		blockMaxBytes:     get64(header, BatchHeaderBlockMaxBytesOffset),
		blockCount:        get32(header, BatchHeaderBlockCountOffset),
		createdUnixNanos:  int64(get64(header, BatchHeaderCreatedUnixNanosOffset)),
		indexOffset:       get64(footer, BatchFooterIndexOffsetOffset),
		filename:          name,
		fileBytes:         fileBytes,
	}
	copy(meta.contentSHA256[:], footer[BatchFooterContentSHA256Offset:BatchFooterContentSHA256Offset+sha256.Size])
	parsedName, ok := parseBatchFilename(name)
	if !ok || BatchID(parsedName) != meta.id || meta.id != BatchID(meta.first.sequence) ||
		meta.recordCount == 0 || meta.blockCount == 0 || meta.blockMaxBytes == 0 || meta.blockMaxBytes > maximumBlockBytes ||
		meta.next.sequence <= meta.first.sequence || uint64(meta.next.sequence-meta.first.sequence) != meta.recordCount ||
		!validCursorShape(meta.first) || !validCursorShape(meta.next) || meta.next.position <= meta.first.position ||
		meta.occupiedBytes != meta.next.position-meta.first.position ||
		get64(footer, BatchFooterBatchIDOffset) != uint64(meta.id) ||
		get64(footer, BatchFooterFirstSequenceOffset) != uint64(meta.first.sequence) ||
		get64(footer, BatchFooterEndSequenceOffset) != uint64(meta.next.sequence) ||
		get64(footer, BatchFooterFirstPositionOffset) != meta.first.position ||
		get64(footer, BatchFooterNextPositionOffset) != meta.next.position ||
		get64(footer, BatchFooterRecordCountOffset) != meta.recordCount ||
		get64(footer, BatchFooterOccupiedBytesOffset) != meta.occupiedBytes ||
		get64(footer, BatchFooterUncompressedBytesOffset) != meta.uncompressedBytes ||
		get32(footer, BatchFooterIndexCountOffset) != meta.blockCount ||
		get64(footer, BatchFooterNextSequenceOffset) != uint64(meta.next.sequence) {
		return archiveBatchMeta{}, ErrFormat
	}
	indexBytes := uint64(meta.blockCount) * uint64(BatchIndexEntrySize)
	if meta.indexOffset < uint64(BatchHeaderSize) || meta.indexOffset > footerOffset ||
		indexBytes > footerOffset-meta.indexOffset || meta.indexOffset+indexBytes != footerOffset {
		return archiveBatchMeta{}, ErrFormat
	}
	contentSHA, err := batchContentSHA256(reader, int64(fileBytes))
	if err != nil || contentSHA != meta.contentSHA256 {
		return archiveBatchMeta{}, ErrFormat
	}
	meta.fileSHA256, err = sha256ReaderAt(reader, int64(fileBytes))
	if err != nil {
		return archiveBatchMeta{}, err
	}
	decoderMemory := meta.blockMaxBytes
	if decoderMemory < 1<<20 {
		decoderMemory = 1 << 20
	}
	if decoderMemory > math.MaxUint64-(1<<20) {
		return archiveBatchMeta{}, ErrFormat
	}
	decoder, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(decoderMemory+(1<<20)))
	if err != nil {
		return archiveBatchMeta{}, err
	}
	defer decoder.Close()
	expectedSequence := meta.first.sequence
	expectedBlockOffset := uint64(BatchHeaderSize)
	recordsHash := sha256.New()
	var totalRecords uint64
	var totalUncompressed uint64
	for blockIndex := uint32(0); blockIndex < meta.blockCount; blockIndex++ {
		entryOffset := meta.indexOffset + uint64(blockIndex)*uint64(BatchIndexEntrySize)
		entry, err := readArchiveObject(reader, entryOffset, BatchIndexEntrySize)
		if err != nil {
			return archiveBatchMeta{}, err
		}
		index := archiveBlockIndex{
			first:             Sequence(get64(entry, BatchIndexFirstSequenceOffset)),
			end:               Sequence(get64(entry, BatchIndexEndSequenceOffset)),
			fileOffset:        get64(entry, BatchIndexFileOffsetOffset),
			compressedBytes:   get64(entry, BatchIndexCompressedBytesOffset),
			uncompressedBytes: get64(entry, BatchIndexUncompressedBytesOffset),
			payloadCRC32C:     get32(entry, BatchIndexBlockCRC32COffset),
		}
		if get32(entry, BatchIndexReservedOffset) != 0 || index.first != expectedSequence || index.end <= index.first ||
			index.fileOffset != expectedBlockOffset || index.uncompressedBytes == 0 || index.uncompressedBytes > meta.blockMaxBytes ||
			index.compressedBytes == 0 || index.compressedBytes > uint64(math.MaxInt) ||
			index.fileOffset > meta.indexOffset-uint64(BatchBlockHeaderSize) {
			return archiveBatchMeta{}, ErrFormat
		}
		blockHeaderEnd := index.fileOffset + uint64(BatchBlockHeaderSize)
		if index.compressedBytes > meta.indexOffset-blockHeaderEnd {
			return archiveBatchMeta{}, ErrFormat
		}
		compressedEnd := blockHeaderEnd + index.compressedBytes
		blockHeader, err := readArchiveObject(reader, index.fileOffset, BatchBlockHeaderSize)
		if err != nil {
			return archiveBatchMeta{}, err
		}
		if get64(blockHeader, BatchBlockMagicOffset) != BatchBlockMagic || get32(blockHeader, BatchBlockVersionOffset) != BatchFormatVersion ||
			get32(blockHeader, BatchBlockHeaderSizeOffset) != BatchBlockHeaderSize || get32(blockHeader, BatchBlockFlagsOffset) != 0 ||
			Sequence(get64(blockHeader, BatchBlockFirstSequenceOffset)) != index.first ||
			Sequence(get64(blockHeader, BatchBlockEndSequenceOffset)) != index.end ||
			uint64(get32(blockHeader, BatchBlockRecordCountOffset)) != uint64(index.end-index.first) ||
			get64(blockHeader, BatchBlockUncompressedBytesOffset) != index.uncompressedBytes ||
			get64(blockHeader, BatchBlockCompressedBytesOffset) != index.compressedBytes ||
			get32(blockHeader, BatchBlockPayloadCRC32COffset) != index.payloadCRC32C ||
			!validCRC32C(blockHeader, BatchBlockHeaderCRC32COffset) {
			return archiveBatchMeta{}, ErrFormat
		}
		compressed := make([]byte, index.compressedBytes)
		if _, err := reader.ReadAt(compressed, int64(blockHeaderEnd)); err != nil {
			return archiveBatchMeta{}, err
		}
		decoded, err := decoder.DecodeAll(compressed, make([]byte, 0, index.uncompressedBytes))
		if err != nil || uint64(len(decoded)) != index.uncompressedBytes || crc32.Checksum(decoded, archiveCRC32CTable) != index.payloadCRC32C {
			if err == nil {
				err = ErrFormat
			}
			return archiveBatchMeta{}, err
		}
		cursor := 0
		for sequence := index.first; sequence < index.end; sequence++ {
			if len(decoded)-cursor < int(BatchRecordHeaderSize) {
				return archiveBatchMeta{}, ErrFormat
			}
			record := decoded[cursor:]
			if Sequence(get64(record, 0)) != sequence {
				return archiveBatchMeta{}, ErrFormat
			}
			length := uint64(get32(record, 8))
			if length > uint64(len(decoded)-cursor-int(BatchRecordHeaderSize)) {
				return archiveBatchMeta{}, ErrFormat
			}
			payload := record[BatchRecordHeaderSize : uint64(BatchRecordHeaderSize)+length]
			if crc32.Checksum(payload, archiveCRC32CTable) != get32(record, 12) {
				return archiveBatchMeta{}, ErrFormat
			}
			writeArchiveRecordHash(recordsHash, sequence, payload)
			cursor += int(BatchRecordHeaderSize) + int(length)
		}
		if cursor != len(decoded) {
			return archiveBatchMeta{}, ErrFormat
		}
		totalRecords += uint64(index.end - index.first)
		if totalUncompressed > math.MaxUint64-index.uncompressedBytes {
			return archiveBatchMeta{}, ErrFormat
		}
		totalUncompressed += index.uncompressedBytes
		expectedSequence = index.end
		expectedBlockOffset = compressedEnd
	}
	if expectedSequence != meta.next.sequence || expectedBlockOffset != meta.indexOffset ||
		totalRecords != meta.recordCount || totalUncompressed != meta.uncompressedBytes {
		return archiveBatchMeta{}, ErrFormat
	}
	copy(meta.recordsSHA256[:], recordsHash.Sum(nil))
	return meta, nil
}

func forEachArchiveRecord(
	reader io.ReaderAt,
	meta archiveBatchMeta,
	start Sequence,
	visit func(Sequence, []byte) error,
) error {
	if reader == nil || start < meta.first.sequence || start > meta.next.sequence || visit == nil {
		return ErrFormat
	}
	if start == meta.next.sequence {
		return nil
	}
	decoderMemory := meta.blockMaxBytes
	if decoderMemory < 1<<20 {
		decoderMemory = 1 << 20
	}
	decoder, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(decoderMemory+(1<<20)))
	if err != nil {
		return err
	}
	defer decoder.Close()
	for blockIndex := uint32(0); blockIndex < meta.blockCount; blockIndex++ {
		entryOffset := meta.indexOffset + uint64(blockIndex)*uint64(BatchIndexEntrySize)
		entry, err := readArchiveObject(reader, entryOffset, BatchIndexEntrySize)
		if err != nil {
			return err
		}
		first := Sequence(get64(entry, BatchIndexFirstSequenceOffset))
		end := Sequence(get64(entry, BatchIndexEndSequenceOffset))
		fileOffset := get64(entry, BatchIndexFileOffsetOffset)
		compressedBytes := get64(entry, BatchIndexCompressedBytesOffset)
		uncompressedBytes := get64(entry, BatchIndexUncompressedBytesOffset)
		payloadCRC := get32(entry, BatchIndexBlockCRC32COffset)
		if end <= start {
			continue
		}
		compressed := make([]byte, compressedBytes)
		if _, err := reader.ReadAt(compressed, int64(fileOffset+uint64(BatchBlockHeaderSize))); err != nil {
			return err
		}
		decoded, err := decoder.DecodeAll(compressed, make([]byte, 0, uncompressedBytes))
		if err != nil || uint64(len(decoded)) != uncompressedBytes || crc32.Checksum(decoded, archiveCRC32CTable) != payloadCRC {
			if err == nil {
				err = ErrFormat
			}
			return err
		}
		cursor := 0
		for sequence := first; sequence < end; sequence++ {
			if len(decoded)-cursor < int(BatchRecordHeaderSize) {
				return ErrFormat
			}
			length := uint64(get32(decoded[cursor:], 8))
			if length > uint64(len(decoded)-cursor-int(BatchRecordHeaderSize)) {
				return ErrFormat
			}
			payload := decoded[cursor+int(BatchRecordHeaderSize) : cursor+int(BatchRecordHeaderSize)+int(length)]
			if sequence >= start {
				if err := visit(sequence, payload); err != nil {
					return err
				}
			}
			cursor += int(BatchRecordHeaderSize) + int(length)
		}
	}
	return nil
}

func parseArchiveBatch(file []byte, name string, expectedEpoch RingEpoch) (archiveBatchMeta, error) {
	minimum := int(BatchHeaderSize + BatchBlockHeaderSize + BatchIndexEntrySize + BatchFooterSize)
	if len(file) < minimum {
		return archiveBatchMeta{}, ErrFormat
	}
	header := file[:BatchHeaderSize]
	footerOffset := len(file) - int(BatchFooterSize)
	footer := file[footerOffset:]
	if get64(header, BatchHeaderMagicOffset) != BatchMagic || get64(footer, BatchFooterMagicOffset) != BatchFooterMagic {
		return archiveBatchMeta{}, ErrFormat
	}
	if get32(header, BatchHeaderVersionOffset) != BatchFormatVersion || get32(footer, BatchFooterVersionOffset) != BatchFormatVersion {
		return archiveBatchMeta{}, ErrFormatVersion
	}
	if get32(header, BatchHeaderSizeOffset) != BatchHeaderSize || get32(header, BatchHeaderEndianOffset) != JournalEndianMarker ||
		get32(header, BatchHeaderCodecOffset) != BatchCodecZstandard || get32(header, BatchHeaderFlagsOffset) != 0 ||
		get32(footer, BatchFooterSizeOffset) != BatchFooterSize || get32(footer, BatchFooterFlagsOffset) != 0 ||
		get32(footer, BatchFooterIndexEntrySizeOffset) != BatchIndexEntrySize ||
		!allZero(header[BatchHeaderReservedOffset:]) || !allZero(footer[BatchFooterReservedOffset:]) ||
		!validCRC32C(header, BatchHeaderCRC32COffset) || !validCRC32C(footer, BatchFooterCRC32COffset) {
		return archiveBatchMeta{}, ErrFormat
	}
	var epoch RingEpoch
	copy(epoch[:], header[BatchHeaderRingEpochOffset:BatchHeaderRingEpochOffset+16])
	if allZero(epoch[:]) || (!allZero(expectedEpoch[:]) && epoch != expectedEpoch) ||
		!bytes.Equal(footer[BatchFooterRingEpochOffset:BatchFooterRingEpochOffset+16], epoch[:]) {
		return archiveBatchMeta{}, ErrFormat
	}
	meta := archiveBatchMeta{
		epoch:             epoch,
		id:                BatchID(get64(header, BatchHeaderBatchIDOffset)),
		first:             ringCursor{sequence: Sequence(get64(header, BatchHeaderFirstSequenceOffset)), position: get64(header, BatchHeaderFirstPositionOffset)},
		next:              ringCursor{sequence: Sequence(get64(header, BatchHeaderEndSequenceOffset)), position: get64(header, BatchHeaderNextPositionOffset)},
		recordCount:       get64(header, BatchHeaderRecordCountOffset),
		occupiedBytes:     get64(header, BatchHeaderOccupiedBytesOffset),
		uncompressedBytes: get64(header, BatchHeaderUncompressedBytesOffset),
		blockMaxBytes:     get64(header, BatchHeaderBlockMaxBytesOffset),
		blockCount:        get32(header, BatchHeaderBlockCountOffset),
		createdUnixNanos:  int64(get64(header, BatchHeaderCreatedUnixNanosOffset)),
		indexOffset:       get64(footer, BatchFooterIndexOffsetOffset),
		filename:          name,
		fileBytes:         uint64(len(file)),
		fileSHA256:        sha256.Sum256(file),
	}
	copy(meta.contentSHA256[:], footer[BatchFooterContentSHA256Offset:BatchFooterContentSHA256Offset+sha256.Size])
	parsedName, ok := parseBatchFilename(name)
	if !ok || BatchID(parsedName) != meta.id || meta.id != BatchID(meta.first.sequence) ||
		meta.recordCount == 0 || meta.blockCount == 0 || meta.blockMaxBytes == 0 || meta.blockMaxBytes > uint64(math.MaxInt) ||
		meta.next.sequence <= meta.first.sequence || uint64(meta.next.sequence-meta.first.sequence) != meta.recordCount ||
		!validCursorShape(meta.first) || !validCursorShape(meta.next) || meta.next.position <= meta.first.position ||
		meta.occupiedBytes != meta.next.position-meta.first.position ||
		get64(footer, BatchFooterBatchIDOffset) != uint64(meta.id) ||
		get64(footer, BatchFooterFirstSequenceOffset) != uint64(meta.first.sequence) ||
		get64(footer, BatchFooterEndSequenceOffset) != uint64(meta.next.sequence) ||
		get64(footer, BatchFooterFirstPositionOffset) != meta.first.position ||
		get64(footer, BatchFooterNextPositionOffset) != meta.next.position ||
		get64(footer, BatchFooterRecordCountOffset) != meta.recordCount ||
		get64(footer, BatchFooterOccupiedBytesOffset) != meta.occupiedBytes ||
		get64(footer, BatchFooterUncompressedBytesOffset) != meta.uncompressedBytes ||
		get32(footer, BatchFooterIndexCountOffset) != meta.blockCount ||
		get64(footer, BatchFooterNextSequenceOffset) != uint64(meta.next.sequence) {
		return archiveBatchMeta{}, ErrFormat
	}
	indexBytes := uint64(meta.blockCount) * uint64(BatchIndexEntrySize)
	if meta.indexOffset < uint64(BatchHeaderSize) || meta.indexOffset > uint64(footerOffset) ||
		indexBytes > uint64(footerOffset)-meta.indexOffset || meta.indexOffset+indexBytes != uint64(footerOffset) {
		return archiveBatchMeta{}, ErrFormat
	}
	contentSHA, err := batchContentSHA256(bytes.NewReader(file), int64(len(file)))
	if err != nil || contentSHA != meta.contentSHA256 {
		return archiveBatchMeta{}, ErrFormat
	}
	decoderMemory := meta.blockMaxBytes
	if decoderMemory < 1<<20 {
		decoderMemory = 1 << 20
	}
	if decoderMemory > math.MaxUint64-(1<<20) {
		return archiveBatchMeta{}, ErrFormat
	}
	decoderMemory += 1 << 20
	decoder, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(decoderMemory))
	if err != nil {
		return archiveBatchMeta{}, err
	}
	defer decoder.Close()
	expectedSequence := meta.first.sequence
	expectedBlockOffset := uint64(BatchHeaderSize)
	recordsHash := sha256.New()
	var totalRecords uint64
	var totalUncompressed uint64
	for blockIndex := uint32(0); blockIndex < meta.blockCount; blockIndex++ {
		entryOffset := meta.indexOffset + uint64(blockIndex)*uint64(BatchIndexEntrySize)
		entry := file[entryOffset : entryOffset+uint64(BatchIndexEntrySize)]
		index := archiveBlockIndex{
			first:             Sequence(get64(entry, BatchIndexFirstSequenceOffset)),
			end:               Sequence(get64(entry, BatchIndexEndSequenceOffset)),
			fileOffset:        get64(entry, BatchIndexFileOffsetOffset),
			compressedBytes:   get64(entry, BatchIndexCompressedBytesOffset),
			uncompressedBytes: get64(entry, BatchIndexUncompressedBytesOffset),
			payloadCRC32C:     get32(entry, BatchIndexBlockCRC32COffset),
		}
		if get32(entry, BatchIndexReservedOffset) != 0 || index.first != expectedSequence || index.end <= index.first ||
			index.fileOffset != expectedBlockOffset || index.uncompressedBytes == 0 || index.uncompressedBytes > meta.blockMaxBytes ||
			index.compressedBytes == 0 || index.fileOffset > meta.indexOffset-uint64(BatchBlockHeaderSize) {
			return archiveBatchMeta{}, ErrFormat
		}
		blockHeaderEnd := index.fileOffset + uint64(BatchBlockHeaderSize)
		if index.compressedBytes > meta.indexOffset-blockHeaderEnd {
			return archiveBatchMeta{}, ErrFormat
		}
		compressedEnd := blockHeaderEnd + index.compressedBytes
		if compressedEnd > meta.indexOffset {
			return archiveBatchMeta{}, ErrFormat
		}
		blockHeader := file[index.fileOffset:blockHeaderEnd]
		if get64(blockHeader, BatchBlockMagicOffset) != BatchBlockMagic || get32(blockHeader, BatchBlockVersionOffset) != BatchFormatVersion ||
			get32(blockHeader, BatchBlockHeaderSizeOffset) != BatchBlockHeaderSize || get32(blockHeader, BatchBlockFlagsOffset) != 0 ||
			Sequence(get64(blockHeader, BatchBlockFirstSequenceOffset)) != index.first ||
			Sequence(get64(blockHeader, BatchBlockEndSequenceOffset)) != index.end ||
			uint64(get32(blockHeader, BatchBlockRecordCountOffset)) != uint64(index.end-index.first) ||
			get64(blockHeader, BatchBlockUncompressedBytesOffset) != index.uncompressedBytes ||
			get64(blockHeader, BatchBlockCompressedBytesOffset) != index.compressedBytes ||
			get32(blockHeader, BatchBlockPayloadCRC32COffset) != index.payloadCRC32C ||
			!validCRC32C(blockHeader, BatchBlockHeaderCRC32COffset) {
			return archiveBatchMeta{}, ErrFormat
		}
		decoded, err := decoder.DecodeAll(file[blockHeaderEnd:compressedEnd], make([]byte, 0, index.uncompressedBytes))
		if err != nil || uint64(len(decoded)) != index.uncompressedBytes || crc32.Checksum(decoded, archiveCRC32CTable) != index.payloadCRC32C {
			if err == nil {
				err = ErrFormat
			}
			return archiveBatchMeta{}, err
		}
		cursor := 0
		for sequence := index.first; sequence < index.end; sequence++ {
			if len(decoded)-cursor < int(BatchRecordHeaderSize) {
				return archiveBatchMeta{}, ErrFormat
			}
			record := decoded[cursor:]
			if Sequence(get64(record, 0)) != sequence {
				return archiveBatchMeta{}, ErrFormat
			}
			length := uint64(get32(record, 8))
			if length > uint64(len(decoded)-cursor-int(BatchRecordHeaderSize)) {
				return archiveBatchMeta{}, ErrFormat
			}
			payload := record[BatchRecordHeaderSize : uint64(BatchRecordHeaderSize)+length]
			if crc32.Checksum(payload, archiveCRC32CTable) != get32(record, 12) {
				return archiveBatchMeta{}, ErrFormat
			}
			writeArchiveRecordHash(recordsHash, sequence, payload)
			cursor += int(BatchRecordHeaderSize) + int(length)
		}
		if cursor != len(decoded) {
			return archiveBatchMeta{}, ErrFormat
		}
		totalRecords += uint64(index.end - index.first)
		if totalUncompressed > math.MaxUint64-index.uncompressedBytes {
			return archiveBatchMeta{}, ErrFormat
		}
		totalUncompressed += index.uncompressedBytes
		expectedSequence = index.end
		expectedBlockOffset = compressedEnd
	}
	if expectedSequence != meta.next.sequence || expectedBlockOffset != meta.indexOffset ||
		totalRecords != meta.recordCount || totalUncompressed != meta.uncompressedBytes {
		return archiveBatchMeta{}, ErrFormat
	}
	copy(meta.recordsSHA256[:], recordsHash.Sum(nil))
	return meta, nil
}

func encodeArchiveHead(head archiveHead) ([]byte, error) {
	if allZero(head.epoch[:]) || head.latest == 0 || !validCursorShape(head.next) || head.batchBytes == 0 ||
		allZero(head.batchSHA256[:]) || len(head.filename) == 0 || len(head.filename) > int(ArchiveHeadFilenameBytes) {
		return nil, ErrFormat
	}
	first, ok := parseBatchFilename(head.filename)
	if !ok || BatchID(first) != head.latest {
		return nil, ErrFormat
	}
	bytes := make([]byte, ArchiveHeadSize)
	put64(bytes, ArchiveHeadMagicOffset, ArchiveHeadMagic)
	put32(bytes, ArchiveHeadVersionOffset, ArchiveHeadFormatVersion)
	put32(bytes, ArchiveHeadSizeOffset, ArchiveHeadSize)
	put32(bytes, ArchiveHeadEndianOffset, JournalEndianMarker)
	copy(bytes[ArchiveHeadRingEpochOffset:], head.epoch[:])
	put64(bytes, ArchiveHeadLatestBatchIDOffset, uint64(head.latest))
	put64(bytes, ArchiveHeadNextSequenceOffset, uint64(head.next.sequence))
	put64(bytes, ArchiveHeadNextPositionOffset, head.next.position)
	put64(bytes, ArchiveHeadBatchBytesOffset, head.batchBytes)
	copy(bytes[ArchiveHeadBatchSHA256Offset:], head.batchSHA256[:])
	put32(bytes, ArchiveHeadFilenameLengthOffset, uint32(len(head.filename)))
	copy(bytes[ArchiveHeadFilenameOffset:], head.filename)
	putCRC32C(bytes, ArchiveHeadCRC32COffset)
	return bytes, nil
}

func parseArchiveHead(bytes []byte, expectedEpoch RingEpoch) (archiveHead, error) {
	if len(bytes) != int(ArchiveHeadSize) || get64(bytes, ArchiveHeadMagicOffset) != ArchiveHeadMagic {
		return archiveHead{}, ErrFormat
	}
	if get32(bytes, ArchiveHeadVersionOffset) != ArchiveHeadFormatVersion {
		return archiveHead{}, ErrFormatVersion
	}
	length := get32(bytes, ArchiveHeadFilenameLengthOffset)
	if get32(bytes, ArchiveHeadSizeOffset) != ArchiveHeadSize || get32(bytes, ArchiveHeadEndianOffset) != JournalEndianMarker ||
		get32(bytes, ArchiveHeadFlagsOffset) != 0 || length == 0 || length > ArchiveHeadFilenameBytes ||
		!allZero(bytes[ArchiveHeadFilenameOffset+length:ArchiveHeadFilenameOffset+ArchiveHeadFilenameBytes]) ||
		!allZero(bytes[ArchiveHeadReservedOffset:]) || !validCRC32C(bytes, ArchiveHeadCRC32COffset) {
		return archiveHead{}, ErrFormat
	}
	var head archiveHead
	copy(head.epoch[:], bytes[ArchiveHeadRingEpochOffset:ArchiveHeadRingEpochOffset+16])
	head.latest = BatchID(get64(bytes, ArchiveHeadLatestBatchIDOffset))
	head.next = ringCursor{sequence: Sequence(get64(bytes, ArchiveHeadNextSequenceOffset)), position: get64(bytes, ArchiveHeadNextPositionOffset)}
	head.batchBytes = get64(bytes, ArchiveHeadBatchBytesOffset)
	copy(head.batchSHA256[:], bytes[ArchiveHeadBatchSHA256Offset:ArchiveHeadBatchSHA256Offset+sha256.Size])
	head.filename = string(bytes[ArchiveHeadFilenameOffset : ArchiveHeadFilenameOffset+length])
	first, ok := parseBatchFilename(head.filename)
	if allZero(head.epoch[:]) || (!allZero(expectedEpoch[:]) && head.epoch != expectedEpoch) || !ok ||
		BatchID(first) != head.latest || !validCursorShape(head.next) || head.batchBytes == 0 || allZero(head.batchSHA256[:]) {
		return archiveHead{}, ErrFormat
	}
	return head, nil
}

func readArchiveHead(directory string, epoch RingEpoch) (archiveHead, bool, error) {
	return readArchiveHeadWithOps(directory, epoch, defaultArchiveFileOps())
}

func parseRetentionFloor(bytes []byte, expectedEpoch RingEpoch) (BatchID, error) {
	if len(bytes) != int(RetentionFloorSize) || get64(bytes, RetentionFloorMagicOffset) != RetentionFloorMagic {
		return 0, ErrFormat
	}
	if get32(bytes, RetentionFloorVersionOffset) != RetentionFloorFormatVersion {
		return 0, ErrFormatVersion
	}
	var epoch RingEpoch
	copy(epoch[:], bytes[RetentionFloorRingEpochOffset:RetentionFloorRingEpochOffset+16])
	floor := BatchID(get64(bytes, RetentionFloorOldestBatchIDOffset))
	if get32(bytes, RetentionFloorSizeOffset) != RetentionFloorSize || get32(bytes, RetentionFloorEndianOffset) != JournalEndianMarker ||
		get32(bytes, RetentionFloorFlagsOffset) != 0 || allZero(epoch[:]) || (!allZero(expectedEpoch[:]) && epoch != expectedEpoch) ||
		floor == 0 || Sequence(floor) > JournalMaxSequence || !allZero(bytes[RetentionFloorReservedOffset:]) ||
		!validCRC32C(bytes, RetentionFloorCRC32COffset) {
		return 0, ErrFormat
	}
	return floor, nil
}

func readRetentionFloor(directory string, epoch RingEpoch) (BatchID, bool, error) {
	return readRetentionFloorWithOps(directory, epoch, defaultArchiveFileOps())
}

func readRetentionFloorWithOps(
	directory string,
	epoch RingEpoch,
	ops archiveFileOps,
) (BatchID, bool, error) {
	path := filepath.Join(directory, retentionFloorName)
	bytes, err := ops.readFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	floor, err := parseRetentionFloor(bytes, epoch)
	if err != nil {
		return 0, false, archiveCorrupt(path, err)
	}
	return floor, true, nil
}

func archiveCorrupt(path string, cause error) error {
	return &ArchiveCorruptError{Path: path, Cause: cause}
}

func archiveHeadForBatch(meta archiveBatchMeta) archiveHead {
	return archiveHead{
		epoch:       meta.epoch,
		latest:      meta.id,
		next:        meta.next,
		batchBytes:  meta.fileBytes,
		batchSHA256: meta.fileSHA256,
		filename:    meta.filename,
	}
}

func archiveHeadMatchesBatch(head archiveHead, meta archiveBatchMeta) bool {
	return head.epoch == meta.epoch && head.latest == meta.id && head.next == meta.next &&
		head.batchBytes == meta.fileBytes && head.batchSHA256 == meta.fileSHA256 && head.filename == meta.filename
}
