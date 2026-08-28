# Go Shared-Memory Queues

Status: current implementation and verification reference for shared-memory format version 4.

## Scope

`go/lib/mpsc` implements Linux shared-memory queues without CGO.

The package supports these queue types:

- SPSC has one producer and one consumer.
- MPSC has many producers and one consumer.

The package supports `amd64` and `arm64`.

The package creates and attaches format version 4 only.

The queue format is an internal repository boundary.
An incompatible format change requires a new version and an attachment compatibility plan.

## Public API

The primary constructors are:

```go
func CreateMPSC(cfg Config) (*MPSC, error)
func AttachMPSC(fd int) (*MPSC, error)
func CreateSPSC(cfg Config) (*SPSC, error)
func AttachSPSC(fd int) (*SPSC, error)
```

A zero capacity selects 1 MiB.
Creation normalizes the capacity before it initializes the region.
Attachment duplicates the supplied descriptor and owns the duplicate.

Both queue types provide `Capacity`, `DupFD`, and `Close`.
`DupFD` returns an owned close-on-exec descriptor.
Queue `Close` returns `ErrBusy` while a handle remains active.
A successful queue `Close` unmaps the region and closes its owned descriptor.

MPSC provides these handle constructors:

```go
func (q *MPSC) NewProducer() (*MPSCProducer, error)
func (q *MPSC) AttachConsumer() (*MPSCConsumer, error)
```

SPSC provides these handle constructors:

```go
func (q *SPSC) AttachProducer() (*SPSCProducer, error)
func (q *SPSC) AttachConsumer() (*SPSCConsumer, error)
```

Producer handles provide these operations:

```go
Reserve(n uint64) (WriteSpan, error)
Commit(span WriteSpan, actual uint64) error
Abort(span WriteSpan) error
Write(src []byte) error
Close() error
```

Consumer handles provide these operations:

```go
Peek() (ReadSpan, bool, error)
Pop() error
Close() error
```

`Write` combines reserve, copy, and commit.
`Peek` returns `ok == false` when no committed record is available.
A second `Peek` before `Pop` returns `ErrMisuse`.
A `Pop` without an outstanding `Peek` returns `ErrMisuse`.
Zero-value handles are not usable and return `ErrMisuse`.
A nil handle can be closed.

Queue and handle values must not be copied after initialization.
Each value contains a `noCopy` marker for `go vet` and a runtime self-identity guard.
A copied queue or handle returns `ErrMisuse` before it changes ownership state.
`Capacity` returns zero for a copied queue because it cannot return an error.
A copied queue cannot unmap the original queue region.
A copied handle cannot release ownership or decrement the queue handle count.

## Thread ownership

Each handle locks its attaching goroutine with `runtime.LockOSThread`.
The handle records a process-local `hostcpu.ThreadIdentity`.
This identity contains the Linux ThreadID and the Go runtime thread token.
The attaching goroutine must use and close that handle.
The lock consumes one OS thread until handle `Close` succeeds.

Every handle operation verifies the complete recorded thread identity.
This rule covers `Reserve`, `Commit`, `Abort`, `Write`, `Peek`, `Pop`, and `Close`.
An identity mismatch returns `ErrMisuse`.
A reused ThreadID cannot reactivate a retained handle on another runtime thread.
A wrong-thread `Close` does not close the handle.
The owner thread can retry `Close`.
A successful handle `Close` calls `runtime.UnlockOSThread`.

The shared header records consumer ownership.
A second consumer receives `ErrBusy` while the recorded thread is live.
A dead consumer owner can be replaced with a compare-and-swap operation.

Thread liveness uses `sched_getscheduler` and `/proc/<tid>/stat`.
`ESRCH` proves that the thread is dead.
`ENOENT` while opening the validated procfs stat path proves owner death immediately.
The code does not re-probe the scheduler after `ENOENT`, because the kernel can reuse the TID.
The `Z` and `X` process states also prove death.
An ambiguous syscall, read, or parse result is treated as live.
A stopped thread remains live and is not recovered.

Each queue keeps a registry of its local handles.
Queue `Close` checks registered handles while it holds the queue lifetime lock.
It invalidates and removes a handle only after its recorded thread identity is proven dead.
It also releases shared producer or consumer ownership when that ownership still belongs to the dead handle.
MPSC reservations remain available to the existing reader recovery protocol.

Linux can reuse a numeric ThreadID before queue `Close` observes the death.
A reused ThreadID is live, so the queue cannot prove that the original handle thread is dead.
In that case, queue `Close` returns `ErrBusy` instead of risking recovery of a live thread.
Explicit handle `Close` remains the only guaranteed local cleanup path.

## PID namespace boundary

Linux ThreadIDs have meaning only inside one PID namespace.
Format version 4 records the creator PID namespace identity.
The identity is `/proc/self/ns/pid` `stat.Dev` and `stat.Ino`.

Attachment reads this identity from the header before it creates the complete mapping.
It compares both values with the attaching process identity.
A mismatch returns `*PIDNamespaceError`.
`errors.Is(err, ErrPIDNamespace)` recognizes this error.
`errors.As` exposes both creator and current identity values.

The header also records the creator PID.
The namespace check uses the `Dev` and `Ino` pair, not the numeric PID.
This check prevents recovery from testing an unrelated ThreadID in another namespace.

Creation and attachment also call `hostcpu.ValidateProcfsPIDNamespace`.
This check compares `/proc/1/ns/pid` with `/proc/self/ns/pid`.
A validation failure returns the hostcpu error before writable mapping or ownership changes.
The default constructors never create a queue with liveness recovery disabled.

## Mapping ownership

Creation owns the backing descriptor and all mappings until queue `Close` succeeds.
Attachment duplicates the caller descriptor before it accepts ownership.
The caller keeps ownership of the supplied descriptor.

The mapper reserves one virtual range with `PROT_NONE`.
The range contains the control area and two adjacent payload views.
The control area maps once.
The payload file range maps twice at adjacent virtual addresses.
Both payload views refer to the same file offset.

A record can cross the logical ring boundary.
The mirrored views still expose that record as one contiguous byte slice.
Queue `Close` unmaps the complete reservation.

Attachment first maps one page as read-only.
It validates magic before it validates the version.
It then validates namespace identity, geometry, capacity, and exact file size.
Validation does not mutate the region.
Invalid geometry returns `ErrFormat`.
A recognized header with another version returns `ErrFormatVersion`.

## Backing stores

`Config.Backend` selects one backing store.

| Backend | Creation policy | Lifetime policy |
|---|---|---|
| `BackendMemfd` | Creates an anonymous sealed memory file. | The descriptor owns the object lifetime. |
| `BackendSHM` | Creates an exclusive file under `/dev/shm`. | Queue close does not unlink the name. |
| `BackendFile` | Creates an exclusive file at `Config.Name`. | Queue close does not unlink the path. |

`BackendMemfd` uses `MFD_CLOEXEC | MFD_ALLOW_SEALING`.
Creation adds grow, shrink, and seal seals after sizing.

A SHM name must be a non-empty simple name.
The package rejects `..`, `/`, and `\` in a SHM name.
SHM creation always uses exclusive creation.
It never overwrites an existing object.

A file backend requires a non-empty path.
The default file policy uses exclusive creation.
`AllowOverwrite` selects truncation and destroys existing contents.
`AllowOverwrite` applies only to `BackendFile`.

Creation always calls `ftruncate`.
Creation calls `fallocate` unless `DisablePreallocate` is true.
Disabling preallocation does not disable file sizing.

## Format version 4

The fixed values are:

- Magic is `0x7067745f6d707363`.
- Version is 4.
- A cache line is 64 bytes.
- The control header is 128 bytes.
- Per-shard metadata is 256 bytes.
- The current shard count is exactly one.
- The maximum extent is `((1<<26)-1) * 64` bytes.

The common header contains these fields:

| Offset | Size | Meaning |
|---:|---:|---|
| 0 | 8 | Magic |
| 8 | 4 | Format version |
| 12 | 4 | Shard count |
| 16 | 8 | Normalized capacity |
| 24 | 4 | Creator PID |
| 28 | 4 | Consumer ThreadID |
| 32 | 4 | Claim or slot stride |
| 36 | 4 | Result stride |
| 40 | 4 | Admission grain |
| 48 | 8 | PID namespace device |
| 56 | 8 | PID namespace inode |

Capacity is at least one operating-system page.
Creation rounds capacity up to a power of two.
The geometry must contain an integral number of grains.

## MPSC layouts

MPSC supports three metadata layouts.

| Layout | Claim stride | Result stride | Grain |
|---|---:|---:|---:|
| `MPSCCompact` | 8 bytes | 16 bytes | 64 bytes |
| `MPSCPadded64` | 64 bytes | 64 bytes | 64 bytes |
| `MPSCPadded256` | 64 bytes | 64 bytes | 256 bytes |

`MPSCCompact` uses the least metadata.
It permits more cache-line sharing between metadata cells.
`MPSCPadded64` isolates claim and result cells at cache-line strides.
`MPSCPadded256` also increases the allocation grain.
The larger grain increases internal payload slack.

`MPSCLayoutExtent` returns the canonical grain and rounded extent for a payload size.
`SPSCExtent` returns the rounded SPSC extent.
Both helpers use the same checked extent logic as queue admission.

The logical cell calculation is:

```text
cell = (logical_position / grain) & (capacity/grain - 1)
```

The claim plane records state, extent, producer ThreadID, and position generation.
The result plane records committed length and a publication tag.
The two-plane design separates admission traffic from publication traffic.

## Span ownership and lifetime

`WriteSpan.Bytes` returns the writable reserved view.
The returned slice capacity equals its length.
The caller cannot extend the slice into reservation slack.

Each write span contains a producer-owner token and an operation token.
`Commit` and `Abort` require both tokens to match.
Cross-handle, cross-queue, stale, and completed spans return `ErrMisuse`.

A write span remains valid until one of these events:

- `Commit` succeeds.
- `Abort` succeeds.
- Producer `Close` succeeds.

Producer `Close` automatically aborts an outstanding reservation.

`ReadSpan` provides `Len`, `CopyTo`, and `UnsafeBytes`.
`UnsafeBytes` returns a zero-copy read-only view by contract.
A read span remains valid until `Pop` or consumer `Close`.
Consumer `Close` does not pop an outstanding record.
A later consumer sees that record again.

Go cannot revoke a copied mapped slice.
The caller must stop using a span after its invalidation event.

## MPSC algorithm

### Claim encoding

The claim word uses these states:

- `stateFree` is 0.
- `stateClaimed` is 1.
- `stateCleared` is 2.
- `stateAborted` is 4.

Bits 3 through 28 contain the extent in 64-byte units.
Bits 35 through 56 contain the producer ThreadID.
The ThreadID field is limited to 22 bits.
A separate generation value binds the cell to its logical position.

A valid extent is aligned to the selected grain.
A valid extent is no larger than `capacity - grain`.
MPSC always keeps one grain unavailable.
The sentinel grain distinguishes full storage from reusable storage.

### Reservation

`Reserve` rounds the requested length up to the layout grain.
A zero-length record consumes one grain.

The fast path performs these steps:

1. It reads the shared admission hint with acquire ordering.
2. It checks logical-position overflow and cached capacity.
3. It claims the expected free cell with compare-and-swap.
4. It initializes the successor free cell.
5. It writes the reservation tag.
6. It publishes `stateCleared` with a release store.
7. It publishes the new hint with a release store.
8. It returns the requested payload view.

The slow path scans claim cells and skips valid existing extents.
One attempt makes at most one ring of hops.
One reservation makes at most four attempts.
Changing or invalid metadata stops that scan attempt.
The operation returns `ErrContended` after the bounded attempts fail.

### Commit and abort

The result tag has three phases:

- Phase 1 is reserved.
- Phase 2 is committing.
- Phase 3 is committed.

`Commit` first changes the reservation tag to the committing tag.
This compare-and-swap fences the producer against consumer recovery.
The producer writes the actual length with a relaxed store.
It publishes the committed tag with a release store.
Only the committed tag publishes the payload.

A short MPSC commit does not reclaim unused reserved extent.
The consumer retires the full reserved extent.

`Abort` changes the reservation tag to zero.
It then publishes `stateAborted` with a release store.
If recovery changed the tag first, `Abort` returns `ErrRecovered`.

### Consumption

The consumer examines the claim at its logical read position.
`stateFree` means that no record is available.
The consumer skips `stateAborted` and advances by its extent.
A matching committed tag makes a `stateCleared` record visible.

The consumer validates state, extent, length, and position arithmetic before mutation.
Invalid shared metadata returns `ErrFormat` or `ErrPositionExhausted`.

`Pop` advances by the complete reserved extent.
It publishes the new read position with a release store.
MPSC preserves record order for each producer.
Records from different producers can interleave.

## MPSC recovery generation protocol

The consumer delays liveness syscalls on an incomplete record.
It starts with 128 repeated observations of the same busy position.
A live producer doubles the next threshold.
The maximum threshold is 65,536 observations.

If the producer is dead, the consumer re-reads the claim and tag.
This second read prevents mutation after a concurrent state change.

For `stateClaimed`, recovery performs these steps:

1. It clears the result tag.
2. It initializes the successor free cell.
3. It changes the claim to `stateAborted` with compare-and-swap.

For `stateCleared`, recovery performs these steps:

1. It reads the result tag with acquire ordering.
2. It preserves a matching committed tag.
3. It changes another matching generation tag to zero.
4. It changes the claim to `stateAborted`.

A committing tag is not a publication point.
It gives no payload visibility guarantee.
A completed commit remains deliverable after producer death.
An incomplete dead-producer reservation becomes aborted.

A stale producer cannot publish after successful recovery.
Its later `Commit` or `Abort` returns `ErrRecovered`.
Generation tests cover cell reuse and stale reservation actions.

## SPSC algorithm

SPSC uses 64-byte grains and 8-byte length slots.
It has no MPSC result plane.
Shared state includes tail, read position, producer bitmap, owner ThreadID, and producer generation.

### Producer ownership and generation

Producer attachment first claims the owner ThreadID with compare-and-swap.
It then sets the single writer bitmap bit.
A dead owner or an orphaned bitmap permits takeover.
Takeover also requires `readPos == tail`.
The queue must be drained before a new producer takes ownership.

Producer detach marks the live owner as detaching before it clears the bitmap.
The detaching state retains the owner ThreadID for liveness checks.
Detach clears the owner only after it clears the bitmap.
A live detaching owner rejects takeover during both detach phases.
A dead detaching owner permits crash recovery.

Successful takeover replaces the owner and increments the shared generation.
Each producer records its attachment generation.
`Reserve`, `Commit`, `Abort`, and `Write` reject a stale generation.
A stale producer cannot publish after takeover.
A stale producer also cannot clear the successor's ownership.

### Reservation and publication

`Reserve` rounds the requested length up to 64 bytes.
SPSC can reserve the complete capacity.
It refreshes the shared read position with an acquire load when cached space is insufficient.

`Commit` writes the actual length with a relaxed store.
It advances the local tail by the actual committed extent.
It then publishes the shared tail with a release store.
A short SPSC commit immediately reclaims unused reservation slack.

`Abort` clears only the local outstanding reservation.
It does not publish data or consume capacity.

The consumer refreshes the shared tail with an acquire load.
That acquire makes the slot length and payload visible.
The consumer can then read the slot length with a relaxed load.
`Pop` publishes the new read position with a release store.

## Memory ordering contract

The implementation separates data access from publication operations.
Relaxed operations do not publish payload data.

### MPSC edges

- Claim compare-and-swap acquires a reusable claim cell.
- The `stateCleared` release publishes reservation metadata to recovery.
- The hint release publishes completed admission metadata.
- The committed-tag release publishes the payload and length.
- The consumer committed-tag acquire makes the payload and length visible.
- The read-position release publishes retired capacity to producers.
- Recovery acquire loads determine whether a committed record exists.

### SPSC edges

- The tail release publishes the payload and slot length.
- The tail acquire makes the payload and slot length visible.
- The read-position release publishes retired capacity.
- The producer read-position acquire permits safe capacity reuse.
- Bitmap, owner, and generation atomics order producer takeover.

## Architecture primitives

The ordered atomic wrappers state the exact intent at each call site.
The implementation uses assembly where Go does not expose the required weaker operation.
All assembly functions use `NOSPLIT`.
Pointer arguments use `//go:noescape`.

On `amd64`:

- `cpuRelax` uses `PAUSE`.
- Relaxed and release stores use aligned `MOVL` or `MOVQ`.
- Loads and read-modify-write operations use `sync/atomic`.
- The x86 memory model supplies the required release behavior for these stores.

On `arm64`:

- `cpuRelax` uses `YIELD`.
- Relaxed loads use `MOVWU` or `MOVD`.
- Acquire loads use `LDARW` or `LDAR`.
- Release stores use `STLRW` or `STLR`.
- General compare-and-swap uses acquire-release exclusive loops.
- Acquire-only successful compare-and-swap uses `LDXR`, `STXR`, and `DMB ISHLD`.
- Fetch-or and fetch-add use acquire-release exclusive loops.
- Fetch-and uses a release exclusive loop.

`SpinWait` executes one architecture processor hint.
It does not yield the Go scheduler.
It does not block, allocate, or inspect queue state.
Internal bounded pauses use 1, 2, 4, and 8 processor hints.

## Progress and retry results

The queues make no general lock-free or wait-free guarantee.
Cold liveness checks and kernel operations can block.

`ErrFull` is a retryable capacity result.
`ErrContended` is a retryable MPSC admission result.
The MPSC scan and retry count are bounded for one call.

`ErrReaderDead` means that a full queue has a proven-dead consumer.
It requires caller recovery or consumer replacement.
It is not a normal full result.

Producers delay repeated consumer liveness checks.
MPSC starts after 128 unchanged observations.
SPSC starts after one unchanged observation.
A live consumer doubles the next threshold up to 65,536.
A changed read position resets the observation state.

Other public errors are:

| Error | Meaning |
|---|---|
| `ErrTooLarge` | The requested record cannot fit the queue or format. |
| `ErrPositionExhausted` | A logical 64-bit position would overflow. |
| `ErrNoWriterSlot` | No producer ownership slot is available. |
| `ErrBusy` | A live handle owns the role, or queue close has active handles. |
| `ErrMisuse` | A handle, thread, span, or operation order is invalid. |
| `ErrRecovered` | Consumer recovery invalidated an MPSC reservation. |
| `ErrFormat` | Shared metadata or geometry is invalid. |
| `ErrFormatVersion` | The magic is valid, but the version is unsupported. |
| `ErrPIDNamespace` | Creator and attaching processes use different PID namespaces. |
| `ErrClosed` | The queue is closed or closing. |

## Allocation and copy contract

These steady-state operations allocate zero Go heap objects:

- `Reserve`
- `Commit`
- `Abort`
- `Write`
- `Peek`
- `Pop`

Allocation tests enforce zero allocations for complete MPSC and SPSC record cycles.
`Write` copies one payload into mapped storage.
`CopyTo` copies one payload out of mapped storage.
Span access avoids payload copies.

Cold operations can allocate or enter the kernel.
These operations include creation, attachment, handle attachment, mapping, and descriptor duplication.
PID namespace and thread liveness inspection are also cold operations.

The inlining check protects small public wrappers and core dispatch operations.
Slow paths and error conversion use `//go:noinline` where needed.

## Performance campaigns

### Current Go campaign

`go/bench/mpsc` contains three benchmark programs:

- `bench_queue` runs concrete SPSC and MPSC queue cases.
- `run_campaign` randomizes and records campaign executions.
- `summarize_campaign` calculates medians, quartiles, and baseline changes.

The campaign records the benchmark path and SHA-256 digest.
It also records Go, kernel, host, CPU list, duration, payload, and random seed.
It records variants, writer counts, consumer modes, compiler flags, and topology policy.
Counter runs are separate from throughput runs.

The authoritative fast-ThreadID campaign output is [`benchmarks/go-mpsc-threadid-final.jsonl`](benchmarks/go-mpsc-threadid-final.jsonl).
The syscall-based diagnostic remains at [`benchmarks/go-mpsc-syscall-threadid-diagnostic.jsonl`](benchmarks/go-mpsc-syscall-threadid-diagnostic.jsonl).
Do not use the diagnostic campaign for current performance conclusions.

The final campaign used the guarded Go 1.27 runtime `m.procid` ThreadID path.
The ThreadID path passed 16,384 syscall equivalence checks before this campaign.
Its microbenchmark measured 1.995 ns/op, compared with 40.99 ns/op for the syscall path.

The campaign used 11 randomized process runs per cell, a 2-second duration, a 56-byte payload, and metadata-only consumption.
It pinned one benchmark thread to each selected physical core on CPUs `0,2,4,6,8,10,12,14,16,18,20,22`.
The benchmark used `spin-wait-safety-yield-4096` and verified the topology.
The binary SHA-256 was `ba1cd9887bc7a07407d692f4df760778124e2c7d3c60d8ee5fea208ebe304730`.

The measured medians and interquartile ranges were:

| Queue | Writers | Median Mrec/s | IQR Mrec/s |
|---|---:|---:|---:|
| MPSC compact | 1 | 21.574 | 21.372–21.690 |
| MPSC compact | 2 | 20.557 | 20.144–20.794 |
| MPSC compact | 4 | 12.613 | 11.702–13.236 |
| MPSC compact | 8 | 7.178 | 6.906–7.328 |
| SPSC | 1 | 67.025 | 66.392–67.723 |

The repository does not retain the pre-ThreadID scalar-gate distribution.
Therefore, this campaign does not claim a measured difference from that gate.

The older tracked SPSC reference was approximately 64 million records per second.
The final SPSC median is 4.7 percent above that reference.
A same-host C++ observation measured 98.844 million records per second.
The final SPSC median is 32.2 percent below that observation.
The repository does not track the current native raw distribution.
Therefore, the final campaign does not establish current native parity.

Against the historical native medians, MPSC1 is 48.5 percent lower. MPSC2 is 48.5 percent lower. MPSC4 is 65.7 percent lower. MPSC8 is 53.7 percent lower.
The migration accepts this lower MPSC throughput to remove the native implementation boundary.
The campaigns used different run counts, so the preserved raw output remains the authoritative evidence.

### Historical MPSC baseline

The last committed native campaign used these conditions:

- Ryzen AI 9 HX 370.
- A 56-byte payload.
- A 1 MiB capacity.
- Pinned CPUs.
- Twenty-five interleaved processes per point.
- Median throughput.

Its two-plane MPSC results were:

| Writers | 1 | 2 | 4 | 8 | 16 |
|---:|---:|---:|---:|---:|---:|
| Throughput, million records/second | 41.9 | 39.9 | 36.8 | 15.5 | 9.7 |

The two-plane design improved the removed in-band design by these ratios:

| Writers | 1 | 2 | 4 | 8 | 16 |
|---:|---:|---:|---:|---:|---:|
| Improvement | 1.02x | 1.48x | 2.51x | 3.29x | 3.46x |

The historical paced-load campaign carried 4 million records per second at eight writers.
Fairness was 1.000 at that load.
Usable aggregate capacity was approximately 8 million records per second.
Saturated fairness was 0.36.
The campaign found no congestion collapse above the usable-capacity point.

These values describe the retired native implementation.
They provide a baseline only and do not prove current Go performance.

## Ordering mutation campaign

`go/lib/mpsc/ordering_mutants.sh` applies five ordering mutations:

1. It changes the ARM64 release store from `STLR` to `MOVD`.
2. It changes the ARM64 acquire load from `LDAR` to `MOVD`.
3. It changes MPSC committed-tag publication to a relaxed store.
4. It changes consumer committed-tag observation to a relaxed load.
5. It changes recovery tag observation to a relaxed load.

The script runs a clean baseline before each campaign.
It applies each mutation exactly once.
It rejects a mutation that does not compile.
The default campaign runs each mutant 20 times with 20,000 records.
It reports `CAUGHT`, `NOT-CAUGHT`, or `INVALID`.

Strict adjudication is valid only on native ARM64.
A strict run fails on another architecture.
A non-strict non-ARM64 run is a control and does not adjudicate weak ordering.

The cross-process publication test uses four producer processes.
It validates declared length, checksum, payload bytes, and per-producer sequence.
A progress deadline catches stalls.

## Verification and limits

The test suite covers these contracts:

- Public API sequencing and span misuse.
- ThreadID checks on every operation.
- Queue and handle cleanup.
- Backend overwrite and preallocation policies.
- PID namespace mismatch handling.
- Mirrored mappings and boundary-crossing records.
- MPSC generation recovery and stale producer actions.
- SPSC takeover generation and short-commit semantics.
- Position and format validation before mutation.
- Zero-allocation steady-state record cycles.
- Cross-process payload publication and producer order.

The ordering campaign has architecture limits.
Release-to-relaxed mutations can produce identical instructions on `amd64`.
The x86 TSO model can hide a weakened source-level ordering rule.
ThreadSanitizer does not prove a weak-ordering contract without a data race.
Native ARM64 remains the required ordering-mutant adjudication platform.
QEMU user mode does not replace native ARM64 memory-order testing.

The retired bounded model checked a simplified queue protocol.
It did not model the Go runtime, compiler, scheduler, allocator, syscalls, or mappings.
Those deleted model files do not verify format version 4.

Performance verification also has limits.
Machine load and CPU topology can dominate small differences.
Campaigns must pin producers and consumers to non-sibling CPUs.
Throughput must be read with fairness.
A high aggregate rate can hide producer starvation.
PMU counter runs explain a throughput result but do not replace it.
Heterogeneous core types can confound fairness comparisons.

## Verification commands

Run the queue tests through the pinned repository toolchain:

```bash
./repo.sh buck2 test //go/lib/mpsc:mpsc_test
```

Run the ordering mutation campaign:

```bash
./repo.sh exec go/lib/mpsc/ordering_mutants.sh
```

Use strict mutation adjudication only on native ARM64.
Run benchmark campaigns through the targets under `go/bench/mpsc`.
Preserve raw JSONL output with the benchmark binary digest and campaign provenance.
