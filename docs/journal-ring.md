# Journal ring version 1

This document is the normative journal-ring and archive-batch version-one contract.

The component has one producer and one archiver.

The component permits any number of passive observers.

Observers never change reclamation or producer capacity.

The existing MPSC and SPSC format-v4 bytes are unrelated formats.

An implementation must reject format-v4 bytes with `ErrFormat`.

## 1. Durability boundary

A producer commit makes a record visible through shared memory.

A producer commit does not make a record power-loss durable.

The archiver makes one complete batch durable before it advances the durable cursor.

A power failure can lose the current uncut ring tail.

The durable cursor is the exclusive archive frontier.

The reclaim cursor always equals the durable cursor.

The producer can reuse only bytes before the reclaim cursor.

The archiver uses a private scan cursor while it creates a batch.

An archive failure resets the scan cursor to the reclaim cursor.

An archive failure does not release ring capacity.

## 2. Identity, sequences, and cursors

`RingEpoch` is a collision-resistant 128-bit ring and archive identity.

`Sequence` is a nonzero, monotonic `uint64` ordinal.

Sequence one identifies the first committed record.

Sequence zero is invalid.

The maximum sequence is `(1<<63)-1`.

The producer returns `ErrPositionExhausted` before sequence or logical-position wrap.

`BatchID` equals the first sequence in one durability batch.

A batch covers the half-open sequence range `[First, End)`.

Every cursor identifies the next record to read.

An internal cursor contains the ring epoch, next sequence, and next logical position.

A public `Cursor` exposes only the ring epoch and next sequence.

Logical positions are internal format values.

Adjacent batches must have equal preceding end and following start cursors.

## 3. Journal-ring serialized layout

All integer fields use little-endian encoding.

The format magic is `0x7067745f6a726e31`, named `pgt_jrn1`.

The format version is one.

The fixed header size is 4096 bytes.

The payload grain is 64 bytes.

The payload capacity is a power of two.

The mapping provides a mirrored payload view for contiguous wraparound access.

The metadata uses one 8-byte tag and one 8-byte descriptor per payload grain.

The tag plane follows the fixed header.

The descriptor plane follows the tag plane.

The payload arena starts at the next 64 KiB boundary after the descriptor plane.

Bytes between the descriptor plane and the payload arena must contain zero.

This fixed alignment makes the arena offset identical on 4 KiB and 64 KiB hosts.

For capacity `C`, each metadata plane contains `C/64` entries.

Reserved version-one bytes must contain zero.

An attaching implementation must reject a nonzero reserved byte with `ErrFormat`.

### 3.1 Header offsets

| Offset | Width | Field |
| ---: | ---: | --- |
| 0 | 8 | Magic |
| 8 | 4 | Version |
| 12 | 4 | Header size |
| 16 | 4 | Endian marker `0x01020304` |
| 20 | 4 | Grain size |
| 24 | 8 | Payload capacity |
| 32 | 8 | Maximum record bytes |
| 40 | 16 | Ring epoch |
| 56 | 4 | Creator PID |
| 60 | 4 | Format flags |
| 64 | 8 | PID namespace device |
| 72 | 8 | PID namespace inode |
| 80 | 4 | Producer TID |
| 84 | 4 | Producer generation |
| 88 | 4 | Archiver TID |
| 92 | 4 | Archiver generation |
| 96 | 8 | Publish cursor seqlock |
| 104 | 8 | Publish next sequence |
| 112 | 8 | Publish next logical position |
| 120 | 8 | Reserved zero |
| 128 | 8 | Durable cursor seqlock |
| 136 | 8 | Durable next sequence |
| 144 | 8 | Durable next logical position |
| 152 | 8 | Required capacity |
| 160 | 32 | Archive sizing SHA-256 |
| 192 | 3904 | Reserved zero |

A cursor writer sets its seqlock to an odd value before field updates.

The writer publishes an even seqlock value after both fields are complete.

A reader accepts a cursor only when two seqlock reads match one even value.

Format flag bit zero is `JournalFlagRecoveryRequired`.

Unknown format flag bits make attachment fail with `ErrFormat`.

The recovery flag is shared and mutable.

A detected dead owner or owner-free cursor failure sets the flag atomically.

Every attached handle observes the same flag.

A stable even cursor lock does not clear the flag.

Only a completed recovery transaction can clear the flag.

The transaction clears the flag after cursor publication and shared owner cleanup.

### 3.2 Descriptor encoding

Only the start-grain descriptor is meaningful.

Bits 0 through 31 contain the payload length.

Bits 32 through 55 contain the occupied extent in grains.

Bits 56 through 63 contain version-one flags.

Continuation descriptors must contain zero in version one.

The extent includes at least one grain, including for an empty payload.

The payload length cannot exceed `2^32-1` bytes.

The extent cannot exceed `2^24-1` grains.

## 4. Publication and overwrite detection

Each covered physical grain has one 64-bit tag.

A zero tag means that the grain was never published.

The stable tag is `sequence << 1`.

The writing tag is `(sequence << 1) | 1`.

A stable start tag is the record publication linearization point.

`Reserve` checks capacity against the durable cursor position.

`Reserve` writes the odd writing tag to every target grain.

`Reserve` then executes a full store barrier.

On ARM64, the barrier must use an explicit DMB-based primitive.

The barrier must complete before any descriptor or payload write.

A release store alone does not satisfy this pre-overwrite requirement.

`Commit` writes the final start descriptor.

`Commit` publishes stable continuation tags before the start tag.

`Commit` publishes the stable start tag last with release ordering.

`Commit` then advances the publish cursor with a seqlock release update.

`Abort` leaves the publish cursor unchanged.

`Abort` permits reuse of the same unpublished sequence.

A live reader acquire-loads the expected stable start tag.

The reader validates the descriptor, cursor arithmetic, and every continuation tag.

The reader can then expose a capacity-capped mapped slice.

`LiveView.Validate` acquire-loads every covered tag again.

Validation succeeds only when every tag still equals the captured stable tag.

A mismatch returns `ErrOverwritten` and requires resynchronization.

## 5. Ownership and progress

Exactly one producer can own the producer role.

Exactly one archiver can own the archiver role.

Producer and archiver operations can run concurrently.

Observers can run concurrently with both roles.

Producer and archiver handles bind to their attaching `hostcpu.ThreadIdentity`.

An implementation must reject wrong-thread operations with `ErrMisuse`.

A takeover requires a proven-dead owner and a matching PID namespace.

Shared owner metadata cannot distinguish a reused Linux TID.

A reused TID can conservatively cause `ErrBusy` while the unrelated thread remains alive.

Process-local `ThreadIdentity` checks still remove authority from a stale local handle.

A takeover increments the role generation.

A stale role handle returns `ErrRecovered`.

Recovery acquires the archiver role before the producer role.

Recovery holds both roles through durable repair, stable-tail derivation, commit, or abort.

A failed second acquisition restores the first owner word.

An aborted recovery restores both original owner words.

A successful recovery releases both owner roles before it clears the shared flag.

The component makes no general lock-free or wait-free guarantee.

Archive I/O and cold liveness checks can block.

## 6. Capacity and convergence

`Config.Sizing` contains the required `SizingConfig` capacity proof.

`ArchiveConfig.Sizing` must equal the stored capacity proof.

`SizingConfig` contains `BatchMaxOccupiedBytes`, `BatchMaxAge`, `ZstdBlockBytes`, and `Bounds`.

`RequiredCapacity` accepts `maxRecordBytes` and one `SizingConfig` value.

All occupied rates include 64-byte record-extent rounding.

`R` is `PeakOccupiedBytesPerSecond`.

`W` is `PeakArchiveBytesPerSecond`.

`A` is `MinArchiveBytesPerSecond`.

`P` is `MinObserverReplayBytesPerSecond`.

`B` is `BatchMaxOccupiedBytes`.

`E` is the maximum 64-byte-rounded record extent.

`T` is the maximum archive stop interval.

Use this equation for `T`:

```text
T = MaxArchiveStall + MaxSyncLatency + MaxRecoveryScanBytes/A
```

`MaxSyncLatency` includes batch sync, both directory syncs, and archive-head sync and rename.

Use this capacity equation:

```text
required = nextPowerOfTwo(align64(B + ceil(R*T) + BurstOccupiedBytes + E + 64))
```

The final 64 bytes are the sentinel grain.

`RequiredCapacity` must use checked integer arithmetic.

The maximum format-v1 capacity is `2^61` bytes.

The control mapping plus both payload mappings must fit `int` and `uintptr`.

`RequiredCapacity` must reject a result above this mapper limit.

`RequiredCapacity` must reject a missing required bound.

`MaxRecoveryScanBytes` must cover one maximum archive batch file.

`MaximumArchiveBatchFileBytes` returns this conservative file-size bound.

Let `M = floor(B/64)` be the maximum record and block count.

Let `U = B + M*16` include one record header per maximum record.

Let `Z = 2*U + M*64` be the conservative Zstandard output bound.

Use this exact maximum archive batch file equation:

```text
maximum batch file = 4096 + Z + M*64 + M*48 + 256
```

The final terms cover block headers, maximum index entries, and the footer.

Reject `MaxRecoveryScanBytes` below this result with `ConvergenceInvalidBound`.

Zero `MaxCatchUp` is the only optional zero bound.

`RequiredCapacity` must reject `A <= W`.

`RequiredCapacity` must reject `P <= W`.

The maximum record must fit one batch and one configured Zstandard block.

When `MaxCatchUp` is nonzero, `MaxObserverBacklogBytes` must be nonzero.

Use this observer catch-up equation:

```text
observer catch-up = MaxObserverBacklogBytes/(P-W)
```

Reject an observer catch-up result greater than `MaxCatchUp`.

An equality with `MaxCatchUp` is valid.

Every sizing failure returns `*ConvergenceError`.

`errors.Is(err, ErrConvergence)` must succeed for every sizing failure.

Startup must not attach any role after sizing validation fails.

Creation computes the required capacity before it creates the shared region.

Creation rejects a requested capacity below the computed requirement.

Creation stores the computed requirement at header offset 152.

Creation stores the sizing digest at header offsets 160 through 191.

Attachment rejects a zero requirement or a requirement above the actual capacity.

Attachment rejects an all-zero sizing digest.

Archiver attachment recomputes the digest from its supplied `SizingConfig`.

A digest mismatch returns `ErrConvergence`.

### 6.1 Sizing digest preimage

The sizing digest is SHA-256 over a fixed 128-byte preimage.

All preimage integers use little-endian encoding.

The preimage uses this exact layout:

| Offset | Width | Field |
| ---: | ---: | --- |
| 0 | 8 | Magic `pgt_jsz1` |
| 8 | 8 | Maximum record bytes |
| 16 | 8 | Required capacity |
| 24 | 8 | Batch maximum occupied bytes |
| 32 | 8 | Batch maximum age nanoseconds |
| 40 | 8 | Zstandard block bytes |
| 48 | 8 | Peak occupied bytes per second |
| 56 | 8 | Peak archive bytes per second |
| 64 | 8 | Minimum archive bytes per second |
| 72 | 8 | Minimum observer replay bytes per second |
| 80 | 8 | Burst occupied bytes |
| 88 | 8 | Maximum archive stall nanoseconds |
| 96 | 8 | Maximum sync latency nanoseconds |
| 104 | 8 | Maximum recovery scan bytes |
| 112 | 8 | Maximum catch-up nanoseconds |
| 120 | 8 | Maximum observer backlog bytes |

The preimage has zero reserved bytes.

Durations must be nonnegative before conversion to unsigned nanoseconds.

The digest binds every input and the calculated requirement to one ring instance.

## 7. Archive batch format

Each durability batch uses one immutable compressed file.

Version one has no segment-aggregation layer.

The filename is `batch-<20-digit-first-sequence>.jrn.zst`.

The codec is the pinned, vendored pure-Go Zstandard implementation.

Codec identifier one means Zstandard.

The file contains a fixed header, compressed blocks, an index, and a fixed footer.

The fixed batch header is 4096 bytes.

Each compressed block has a 64-byte header.

Each index entry is 48 bytes.

The fixed footer is 256 bytes.

Each record header is 16 bytes.

### 7.1 Batch header offsets

| Offset | Width | Field |
| ---: | ---: | --- |
| 0 | 8 | Batch magic `pgt_jbt1` |
| 8 | 4 | Version |
| 12 | 4 | Header size |
| 16 | 4 | Endian marker |
| 20 | 4 | Codec identifier |
| 24 | 16 | Ring epoch |
| 40 | 8 | Batch ID |
| 48 | 8 | First sequence |
| 56 | 8 | Exclusive end sequence |
| 64 | 8 | First logical position |
| 72 | 8 | Next logical position |
| 80 | 8 | Record count |
| 88 | 8 | Occupied ring bytes |
| 96 | 8 | Uncompressed encoded bytes |
| 104 | 8 | Maximum uncompressed block bytes |
| 112 | 4 | Block count |
| 116 | 4 | Flags |
| 120 | 8 | Creation time in Unix nanoseconds |
| 128 | 4 | Header CRC32C |
| 132 | 3964 | Reserved zero |

The header CRC32C covers the complete header with its CRC field zeroed.

### 7.2 Record and block encoding

Each uncompressed record contains these fields in order:

```text
uint64 sequence
uint32 payload length
uint32 payload CRC32C
payload bytes
```

The record stream contains no alignment padding.

A block never splits one encoded record.

A block header uses these offsets:

| Offset | Width | Field |
| ---: | ---: | --- |
| 0 | 8 | Block magic `pgt_jbk1` |
| 8 | 4 | Version |
| 12 | 4 | Block header size |
| 16 | 8 | First sequence |
| 24 | 8 | Exclusive end sequence |
| 32 | 4 | Record count |
| 36 | 4 | Flags |
| 40 | 8 | Uncompressed bytes |
| 48 | 8 | Compressed bytes |
| 56 | 4 | Uncompressed block CRC32C |
| 60 | 4 | Header CRC32C |

The block header CRC32C covers the header with its header CRC field zeroed.

### 7.3 Block index

Each index entry uses these offsets:

| Offset | Width | Field |
| ---: | ---: | --- |
| 0 | 8 | First sequence |
| 8 | 8 | Exclusive end sequence |
| 16 | 8 | Block header file offset |
| 24 | 8 | Compressed bytes |
| 32 | 8 | Uncompressed bytes |
| 40 | 4 | Uncompressed block CRC32C |
| 44 | 4 | Reserved zero |

Index ranges must be consecutive and complete.

### 7.4 Batch footer offsets

| Offset | Width | Field |
| ---: | ---: | --- |
| 0 | 8 | Footer magic `pgt_jbf1` |
| 8 | 4 | Version |
| 12 | 4 | Footer size |
| 16 | 16 | Ring epoch |
| 32 | 8 | Batch ID |
| 40 | 8 | First sequence |
| 48 | 8 | Exclusive end sequence |
| 56 | 8 | First logical position |
| 64 | 8 | Next logical position |
| 72 | 8 | Record count |
| 80 | 8 | Occupied ring bytes |
| 88 | 8 | Uncompressed encoded bytes |
| 96 | 8 | Index file offset |
| 104 | 4 | Index count |
| 108 | 4 | Index entry size |
| 112 | 8 | Next sequence |
| 120 | 4 | Flags |
| 124 | 4 | Footer CRC32C |
| 128 | 32 | Content SHA-256 |
| 160 | 96 | Reserved zero |

The footer cursor is `{ring epoch, next sequence, next logical position}`.

The content SHA-256 covers all file bytes with the digest and footer CRC fields zeroed.

The footer CRC32C covers the completed footer with only its CRC field zeroed.

The archive head stores a separate SHA-256 of the complete final file.

## 8. Archive publication

The archive directory contains `archive.lock`.

An archiver takes the exclusive lock during recovery and publication.

For each batch, the archiver performs this order:

1. Write a same-directory temporary batch file.
2. Complete all blocks, the index, and the footer.
3. Call `fdatasync` on the temporary file.
4. Rename the file to its final batch name.
5. Call `fsync` on the archive directory.
6. Create `archive.head.tmp` exclusively.
7. Write the new fixed archive head.
8. Sync and rename it to `archive.head`.
9. Call `fsync` on the archive directory again.
10. Advance the shared durable cursor with release ordering.
11. Permit producer reuse through the new cursor.

The batch footer and archive head must contain identical exclusive frontier cursors.

A crash can leave an orphan final batch.

A head file must never reference missing or unsynchronized batch bytes.

Recovery ignores temporary files.

Recovery validates the head and its referenced batch.

Recovery adopts only valid, contiguous orphan batches after the head.

A missing interior batch returns `ErrArchiveGap`.

Disk ahead of shared state can advance shared state after complete validation.

Shared state ahead of verified disk returns `ErrArchiveLost`.

## 9. Archive head format

`archive.head` has a fixed size of 4096 bytes.

Its magic is `pgt_jhd1`.

| Offset | Width | Field |
| ---: | ---: | --- |
| 0 | 8 | Magic |
| 8 | 4 | Version |
| 12 | 4 | File size |
| 16 | 4 | Endian marker |
| 20 | 4 | Flags |
| 24 | 16 | Ring epoch |
| 40 | 8 | Latest batch ID |
| 48 | 8 | Durable next sequence |
| 56 | 8 | Durable next logical position |
| 64 | 8 | Complete batch file bytes |
| 72 | 32 | Complete-file SHA-256 |
| 104 | 4 | Filename length |
| 108 | 64 | Final batch filename bytes |
| 172 | 4 | Head CRC32C |
| 176 | 3920 | Reserved zero |

The filename is UTF-8 and has no terminating zero in its declared length.

Unused filename bytes must contain zero.

The head CRC32C covers the complete file with its CRC field zeroed.

## 10. External retention floor

The component does not schedule or perform automatic deletion.

External policy owns retention.

The archive directory contains `retention.floor`.

The floor file has a fixed size of 4096 bytes.

Its magic is `pgt_jrf1`.

| Offset | Width | Field |
| ---: | ---: | --- |
| 0 | 8 | Magic |
| 8 | 4 | Version |
| 12 | 4 | File size |
| 16 | 4 | Endian marker |
| 20 | 4 | Flags |
| 24 | 16 | Ring epoch |
| 40 | 8 | Oldest retained batch ID |
| 48 | 4 | Floor CRC32C |
| 52 | 4044 | Reserved zero |

The floor CRC32C covers the complete file with its CRC field zeroed.

External retention takes the exclusive `archive.lock` before a floor update or deletion.

External retention first writes and syncs a higher floor atomically.

External retention then syncs the archive directory.

External retention deletes only complete batches below the floor.

External retention syncs the archive directory after deletion.

An observer takes the shared lock while it opens the selected batch file.

The observer releases the lock after the open operation succeeds.

The open file descriptor pins the batch bytes across later unlink operations.

A new open below the floor returns `*NotRetainedError`.

A missing batch at or above the floor returns `ErrArchiveGap`.

## 11. Observer contract

An observer can start only at an exact retained `BatchID`.

When durable batches exist, a nil start is invalid.

When no durable batch exists, a nil start selects the oldest live-ring cursor.

Replay copies one decompressed payload into caller storage.

Insufficient storage returns `*BufferTooSmallError` without cursor advancement.

An observer replays through one fixed archive-head frontier.

The observer uses the batch footer cursor for live handoff.

The observer must not use an unrelated current publish tail.

If the durable frontier advances, the observer replays the new durable batches first.

If the handoff slot was overwritten, the observer requires resynchronization.

`NextLive` returns a zero-allocation `LiveView` value.

`UnsafeBytes` returns a read-only mapped slice.

The caller must stage all calculations in private storage.

The caller must not publish irreversible effects before `Validate` succeeds.

A failed validation invalidates the view and returns `ErrOverwritten`.

A failed validation changes the observer to `ObserverResyncRequired`.

Later read calls return `ErrResyncRequired` until explicit resynchronization.

Successful resynchronization provides at-least-once delivery.

Exactly-once effects require an application transaction.

## 12. Error stability and evolution

Callers can branch on the exported sentinel errors.

Free-form error text is not stable.

Typed errors must unwrap to their documented sentinel.

System-call, path, compression, and checksum causes remain available for diagnostics.

`ErrFull` is retryable without a state change.

Archive I/O errors are retryable only when the cause permits retry.

`ErrOverwritten` requires explicit observer resynchronization.

`ErrArchiveGap`, `ErrArchiveLost`, and `ErrFormat` require operator action.

Version-one readers reject unknown versions.

Version-one readers reject nonzero reserved fields and unknown flag bits.

A future incompatible meaning requires a new format version.

A future format must not reinterpret retained version-one bytes.
