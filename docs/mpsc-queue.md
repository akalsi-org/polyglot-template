# Variable-Length Byte Queue — Algorithm Specification

Two variants, allocation-free after construction, over a mirrored (doubly
mapped) ring so a record crossing the physical end is still one contiguous span.

| variant | producers | ordering | arbitration | recovery |
|---|---|---|---|---|
| `MpscRing<>` | many | total across all writers | CAS on a Claim cell | reader-only, on proven death |
| `SpscRing<>` | one | per-writer FIFO | none (wait-free) | none needed |

**The in-band queue and its sharded compositions have been removed.** That queue
placed its 8-byte descriptor at the head of the record, so the descriptor shared
a cache line with the first 56 payload bytes. The claim CAS wanted that line
exclusive, the owner's payload writes wanted it exclusive, and the reader's
commit poll pulled it shared — three actors, three access patterns, one line,
per record. Measured on
Zen 5 it lost ~80% of throughput across the 2→4 writer step and reached 0.04 IPC
at eight writers, with 33 coherence misses per record.

That was not fixable by tuning the claim path. A test-and-test-and-set filter
removed 42% of the failed locked RMWs and made throughput *worse*; per-line
ablations of the successor prefetch and the reader poll were worth ~5% each,
against a 70–80% cliff. The residual cost was the line sharing itself.

## Properties

* Variable-length records, in-place: `reserve(n)` returns a span the caller
  writes directly, `commit(span, actual_n)` publishes any prefix of it.
* Fail-fast admission. `reserve` declines atomically on capacity exhaustion
  before any irreversible state change.
* Exact recovery of a dead writer's reservation. `{position, extent, owner,
  state}` is durable before any later record depends on it.
* Reader-only recovery, gated on **proven** death via `/proc/<tid>/stat`.
  Writers never help, stamp, or abort another writer's record. Time is never
  evidence of death; there are no timeouts in the protocol.
* Arbitrary short commit: reserve N, commit any n ≤ N. The reader delivers n
  bytes and advances by the full reserved extent.
* Per-writer FIFO; `MpscRing<>` additionally gives a total order.

## Memory layout

One `memfd`, sealed `F_SEAL_SHRINK | F_SEAL_GROW | F_SEAL_SEAL` before any descriptor
is shared, and `fallocate`d at construction so no first-touch fault can `SIGBUS` or
allocate on the hot path.

```
  memfd                          virtual address space
  ------                         ---------------------
  control page  --- mmap x1 -->  control          (canonical alias only)
  arena (N)     --+- mmap ---->  arena     base + 0
                  +- mmap ---->  arena'    base + N     <- same pages
```

`N` is a power of two and a multiple of the page size. Arena addressing is
`base + (pos & (N-1))`, so any record of length `<= N` is contiguous across the wrap.

**Two unwrapped positions differing by a multiple of `N` are the same physical word.**
This is the source of revision 1's worst defect and must be kept in mind everywhere.

**Control words live in the control page only.** Never place an atomic in the mirrored
region: the two aliases are *different locations* to the compiler, which may reorder or
combine relaxed accesses that touch the same physical word. A miscompilation hazard,
not a coherence one.

Control page, each cursor padded to 128 bytes:

```
magic, version, N, tgid          read-only after init
reader_tid  (u32)                written once when the reader attaches
write_hint  (u64)                writer-written, hint only, never authoritative
read_pos    (u64)                reader-written only
```

`reader_tid` does two jobs, both using the same exact-liveness check as everywhere else
rather than a timeout. A writer facing persistent backpressure can distinguish a dead
reader from a slow one, turning an indefinite retry loop into a definite error. And it
is the takeover word for [reader restart](#reader-death-and-restart). Checked only on
the full path, never on the hot path.

The coherence granule on x86-64 is 64 bytes, so 64 would prevent true false sharing —
but Intel's L2 adjacent-line prefetcher pairs lines, and there are only two hot
cursors, so 128 is free here. Per-record padding is not, which is why the record
granule stays at 64.

Positions are **unwrapped monotonic u64**. Masking happens only at address computation.

## Storage backends

**Every configuration needs a file descriptor, including the process-local one.** You
cannot map anonymous memory twice: `MAP_ANONYMOUS` always produces fresh zero pages and
`mremap` moves rather than duplicates. There is no "mmap-based" variant distinct from a
descriptor-based one — only where the descriptor comes from.

| Backend | Reachable by | Sealable | Outlives creator | Use for |
|---|---|---|---|---|
| `memfd_create` | fd passing, inheritance | **yes** | while any fd open | IPC with trusted peers; default |
| `shm_open` | name under `/dev/shm` | no | until `shm_unlink` | reader restart, named discovery |
| `open` on tmpfs | path | no | until unlink | as above, explicit path |
| `open` on disk | path | no | reboot | forensics only |
| `memfd` + `MFD_HUGETLB` | as memfd | yes | | large `N`, fewer TLB entries |

Only memfd supports seals — `F_ADD_SEALS` requires `memfd_create` with
`MFD_ALLOW_SEALING`. Every named backend forgoes `F_SEAL_SHRINK` protection against a
peer's `ftruncate`, which is the most common way a shared-memory queue dies in
production. That is the price of being nameable, and naming is also what would fix
reader restart.

A disk-backed file is almost always wrong: `MAP_SHARED` on a real filesystem means
continuous writeback for data nobody intends to persist.

The backend supplies `{fd, size, capabilities}`; one shared mapping routine performs
the `PROT_NONE` reservation and the two `MAP_FIXED` calls for all of them.

### Process-local vs IPC

The ring, protocol, descriptor layout, and mapping are identical. Two things differ:

**Rendezvous.** Process-local shares the fd by inheritance. IPC passes it over a unix
socket with `SCM_RIGHTS`, or opens a name.

**Liveness identity.** `tgkill` needs `tgid` and `tid`; the descriptor has room for
one id. Thread ids are unique **within a PID namespace**, and `/proc/<tid>` exists for
any thread, not only group leaders — so a bare `tid` suffices, with recovery reading
`/proc/<tid>/stat`. Process-local can use `tgkill` as a fast path. All peers must share
a PID namespace.

## Alignment

Record extents are `align64(8 + n)`; record starts, and therefore descriptors, are
64-byte aligned.

| payload | extent | waste |
|---|---|---|
| 32 B | 64 B | 50% |
| 56 B | 64 B | **12.5%** |
| 64 B | 128 B | 50% |
| 120 B | 128 B | **6%** |
| 1 KiB | 1088 B | 6% |

Payloads of `64k − 8` pack perfectly. Padding is dead ring capacity, not merely
per-record overhead — it shortens the wrap interval and raises the full-rate.

The alignment eliminates false sharing between *adjacent* records. It does not
eliminate sharing between a descriptor and its own payload; see
[Reader backoff](#reader-backoff).

## Descriptor

Eight bytes, holding a record's state, extent, and owner. In `MpscRing<>` this
word is a **Claim cell** in the control plane; in `SpscRing<>` it is the in-band record
header. The encoding is shared because the recovery and lap-ABA arguments are the
same in both. `state` occupies the
low bits in every encoding so it can be decoded before anything else.

```
  bits 0..2     state    FREE=0 | CLAIMED=1 | CLEARED=2 | COMMITTED=3 | ABORTED=4

  state == FREE:
    bits  3..60   pos >> 6      the position this slot IS, unwrapped
    bits 61..63   reserved

  otherwise:
    bits  3..28   size          reservation extent, 64-byte units (4 GiB)
    bits 29..34   remainder     committed length mod 64
    bits 35..56   tid           owning thread (22 bits; PID_MAX_LIMIT is 2^22)
    bits 57..63   reserved
```

`pos >> 6` needs exactly 58 bits for a 64-bit position space, and bits 3..60 provide
exactly 58. No truncation, so `FREE(p) != FREE(p + kN)` unconditionally.

**The numeric state values are ABI and must be pinned**, not left to an enum's
declaration order — this word is shared between separately-compiled processes. Do not
express it as a C bitfield either; bit order and allocation within bitfields are
implementation-defined. Use an explicit `uint64_t` with pack/unpack helpers.

**`FREE` must be 0 deliberately.** An untouched plane or arena word is all zeros, which then
decodes as `FREE(0)` — harmless, because any claimant at `p != 0` mismatches and
restarts. Had `ABORTED` been 0, an untouched word would decode as a zero-extent
ABORTED and a walker reaching one would advance by zero, an infinite loop caught only
by the hop bound. No legitimate descriptor has extent 0 (`need >= 64`, and a trailer's
remainder is either 0 or `>= 64`), so `extent == 0` may be asserted.

### FREE carries its own position

**A slot is claimable iff it reads exactly `FREE(p)` for the position `p` the claimant
believes it is at.** Not "iff it is zero."

This is the fix for revision 1's worst defect. A writer stopped between its capacity
check and its claim CAS has claimed nothing, so nothing pins the reader and the ring
wraps freely underneath it. On resuming, a zero-expecting CAS would succeed against a
*different lap's* slot that some other writer had just cleared, and the stale writer
would then clear at an arbitrary offset and memcpy over live records. Encoding the
position in the FREE word makes that CAS fail deterministically: `FREE(p)` can never
equal `FREE(p + kN)`.

It also closes the "stale payload might coincidentally look claimable" hazard. With
zero as the predicate, eight zero payload bytes were enough; now a full 64-bit
accidental collision is required.

Construction stamps `desc[0] = FREE(0)`. Every other slot is only ever reached via a
stamp.

### Invariants

**I1 — boundaries only refine.** `size` may shrink on commit (short commit, below) but
every boundary derivable from any observed `size` is a real record boundary. A walker
holding the pre-commit size computes `p + reserved`, which is real because the trailer
ends there; a walker holding the post-commit size computes `p + used`, which is real
because the trailer starts there. This replaces revision 1's stronger and incorrect
"size is immutable" claim, which made the short-commit trailer unreachable dead code.

**I2 — state vouches for the successor.**

```
CLAIMED    successor slot NOT yet stamped — may hold stale payload
CLEARED    successor slot stamped FREE(succ) or validly claimed
COMMITTED  payload complete (implies CLEARED)
ABORTED    record void, skip it (implies CLEARED)
```

Without I2 a walker reaching a non-FREE word cannot distinguish a valid claim from
stale payload. Any transition into ABORTED, including the reader's recovery, **must
stamp the successor first** — otherwise ABORTED lies about its successor and the next
walker advances into garbage.

**I3 — an uncommitted record's slots cannot be recycled, for as long as it stays
uncommitted.** An uncommitted record pins the reader at or before it, so `read_pos`
freezes, and the capacity bound then keeps every claim and every stamp strictly below
`read_pos + N`. Therefore a writer's descriptor, its reservation interior (including
the future trailer position), and its payload bytes are all unrecyclable across the
entire window from claim to commit-or-abort, and its later stores remain safe.

The proof never depended on that window being short — only on the record being
uncommitted. This matters because the two-phase API puts **arbitrary user code** in
that window.

This was implicit in revision 1 and is load-bearing for the safety of every plain store
the owner makes, and for reader-only recovery. It is stated because it is easy to
violate accidentally when changing the capacity check.

### Three lemmas

Essentially the whole safety argument rests on these. They are the right targets for a
model check.

**Lemma 1 — the FREE word is self-certifying.** The value `FREE(q)` can only ever be
written *before* `q` is first claimed — by the owner's successor stamp, by recovery's
stamp, or by both, which are idempotent within that window. Once `q` is claimed the
word becomes CLAIMED and that slot's next FREE stamp carries `q + kN`, so `FREE(q)`
never recurs. Therefore at any instant at most one *reachable* slot reads `FREE(x)` for
`x` equal to its own true position — the frontier.

("Reachable" is doing real work in one corner: while `desc[p]` is CLAIMED with a dead
owner and recovery has stamped `q` but not yet stored ABORTED, `FREE(q)` exists, but no
walker can reach `q` — `p` blocks the chain, and the hint never reached `q` because the
dead owner's hint store is sequenced after the vouch it never performed.)

The consequence is strong enough to make S1 nearly self-evident: **a successful claim
CAS proves by content alone that `p` was the frontier**, regardless of how the walk
computed `p`. A walker that wandered through recycled territory and by luck arrived at
the true frontier makes a *correct* claim, because validity depends only on the content
match and the capacity check, both verified at claim time. The walk is a heuristic for
finding `p`; it is never trusted.

**Lemma 2 — while the owner is alive, no other actor may write its slots.** Take a
writer anywhere between its claim and its commit-or-abort — a window that spans
arbitrary user code.

*Evaluate the premise at the moment of the action, never at the moment of an earlier
snapshot.* An owner observed in-flight may have committed and died before the acting
thread proceeds, at which point the pinning argument no longer applies and its slots are
recyclable. This is not a subtlety of the lemma; it is the one way it has actually been
misapplied. It is uncommitted, so by I3 the reader is pinned at or before `p`,
`read_pos` is frozen there, and every claim and stamp is capped strictly below
`read_pos + N <= p + N < q + N`. Two halves follow: `q`'s slot cannot be reached by
*recycling* (any position `q + kN`, `k >= 1`, is beyond the cap), and `q` itself cannot
be reached in the current lap because `desc[p]` reads CLAIMED and blocks every walker.

Other actors *load* these words constantly; the lemma is about writes. And if the owner
dies, the reader legitimately writes them — that is recovery, and it is sound precisely
because a dead thread issues no further stores.

**Lemma 3 — a hint value observed from a live writer never points at a recycled slot.**
Recycling slot `v` requires a claim at `>= v + N`, which requires `read_pos >= v + 64`,
which means the reader consumed `v`'s record. Any writer able to claim in that range
read that `read_pos` via acquire from the reader's release store, which the reader
issued after acquiring **that hint-storer's own commit or abort** — and its hint store
is sequenced before that. So once recycling is possible, `v` is no longer the visible
hint value.

**The chain only closes for writers that reach commit or abort.** If the hint-storer
dies in between, the reader passes its record via `recover()`: the ABORTED word the
reader acquires was written by the reader itself, and the CLEARED word it read was
released by the *vouch*, which is sequenced **before** the hint store. No
happens-before edge from the dead writer's hint store exists at all. That corner is
closed instead by the crash-model axiom — a killed thread has no pending stores, so its
death totalizes them before anything recovery does afterwards.

Consequence: **restart-on-FREE-mismatch is load-bearing for progress**, not merely
defense in depth. Safety never depends on it — Lemma 1 makes a claim from garbage
territory impossible regardless — but in the dead-mid-flight-writer corner it is what
lets a walker resynchronize.

## Two-plane protocol

Three planes, three different lines, three different actors:

```
   Claim[cell]  8B   arbitration + recoverable ownership   writers race here
   Result[cell] 16B  completion + actual length            owner writes once,
                                                           reader polls here
   payload           data                                  owner writes,
                                                           reader reads
```

`cells = capacity / 64`, so the Claim plane can never be exhausted before the
byte ring is: every record occupies at least one 64-byte grain. The slot plane
therefore needs no backpressure of its own — the byte capacity check remains the
sole admission gate, and the slot index is `ordinal % cells`.

Both planes live in the Region's **singly-mapped control area**, never in a
mirrored arena. Claim and Result cells are atomics, and the two aliases of a
mirrored arena are different locations to the compiler, which may reorder or
coalesce relaxed accesses that in fact touch one physical word. That is a
miscompilation hazard, not a coherence one, and no amount of correct atomic code
repairs it.

Metadata cost is 8 + 16 = **24 bytes per 64 bytes of ring, 37.5%**.

### Writer

```
  walk Claim cells from write_hint          [acquire]
  find the cell reading exactly FREE(p)
  CAS  FREE(p) -> CLAIMED(extent, owner)    [acquire success / relaxed failure]
  promote successor: Claim[q] = FREE(q)     [relaxed]   exclusive by Lemma 2
  vouch: Claim[p] = CLEARED                 [release]   publishes the promotion
  ... caller writes payload ...
  Result[p].len = n                         [relaxed]
  Result[p].tag = commitTag(p)              [release]   publishes the payload
```

### Reader

```
  load Claim[rd_]                           [acquire]
  FREE     -> frontier, nothing to read
  ABORTED  -> skip its extent
  CLAIMED/CLEARED -> load Result[rd_].tag   [acquire]
        tag == commitTag(rd_) -> deliver payload[rd_ .. rd_+len]
        otherwise             -> spaced liveness check (see Recovery)
```

The reader touches the payload line **only after** acquiring the tag. In `in-band queue`
it polled that line throughout the owner's window.

### Walking past an in-flight record is legal

In `in-band queue`, a walker meeting `CLAIMED` had to restart: the successor slot might
still hold arbitrary payload bytes, which can decode as anything. Here the
successor is a Claim cell, and the only values it can ever hold are well-formed
claim words — this lap's or an earlier lap's. A walker advancing on a stale word
wanders, but cannot make a bad claim, because only the true frontier cell reads
exactly `freeWord(p)` for the `p` the walker holds. Wandering is bounded by the
hop cap and ends in a restart.

This is why a live writer holding a reservation across arbitrary user code no
longer stalls every other writer — only the reader. It is also where the fairness
difference comes from: in `in-band queue` a writer behind an in-flight record stayed
behind, giving min/max records-per-writer of 0.32 at two writers; two-plane
measures 0.96 on the same pinning.

**A walker may not CLAIM the position immediately following a record it observed
`kClaimed`.** It may walk past it; it may not take its successor.

That single rule is what lets reader recovery promote the successor
unconditionally. Recovery fires on a record still `kClaimed` with a dead owner —
and such a record has been `kClaimed` since its claim CAS, so no walker can ever
have observed it otherwise, so nobody can have claimed its successor. There is
nothing at `q` to destroy and, crucially, nothing to CLASSIFY. It restores the
invariant the in-band ring got by blocking the walk entirely, without blocking
the walk.

It is nearly free because `kClaimed` is only the promote-then-vouch window inside
`finishClaim` — two stores, closed before `reserve()` returns. The long,
caller-controlled window is `kCleared`. The one case that blocks for real is a
DEAD owner, which is exactly when blocking is correct; the stall is then bounded
by recovery rather than permanent.

The fast path needs no check of its own: the write hint is published AFTER the
vouch, so reaching a position from the hint already implies its predecessor was
vouched.

State transitions are monotonic (`FREE → kClaimed → kCleared → kAborted`), so a
claim licensed by observing a non-`kClaimed` predecessor stays licensed.

WHAT THIS REPLACED, recorded so it is not reintroduced. Recovery originally
promoted unconditionally with the false comment "nobody else can have written
cell(q)". Three attempts to make recovery CLASSIFY the successor instead all
failed: a **lap-parity bit** (cells interior to a large record are skipped for
whole laps, so a stale word can be two or more laps old — and no counter width
fixes it, since a stable record layout skips the same cells forever); a
**reader-stamped retirement marker** (sound, but the stamp lands on the writers'
active cache line — 22.7 misses/record at four writers — and it broke the walk,
which advances THROUGH retired cells using their extents); and
**chain-following** from `q` (a stale chain with uniform extents aliases the live
one exactly and terminates on the real frontier). The common error was inferring
a word's lap from a word that does not encode its position. Removing the need to
classify was the answer.

### Recovery

```
  Result tag absent -> space liveness checks by OBSERVATION COUNT, never time
        threadAlive(owner)  alive -> keep waiting, widening the spacing
                            dead  -> RE-READ Result first
                                       committed -> deliver it, never recover
                                       in flight -> if CLAIMED:
                                                    promote successor q
                                                    Claim[p] = ABORTED [release]
```

The re-read after the liveness verdict is mandatory: between the first load and
`/proc` check the owner can have committed and died, so stale Claim and Result
snapshots must not be recovered. A committed record is delivered.

For an in-flight `kClaimed` record, recovery promotes `q = p + extent`
unconditionally before marking `p` aborted. This is safe because a walker may
walk past a `kClaimed` record but may not claim its immediate successor. A dead
record recovered while still `kClaimed` has held that state since its claim CAS,
so no writer can own `q`: it is either already `freeWord(q)` from the owner's
promotion or a stale word left when the owner died before promoting. The
successor-blocking invariant removes any need to classify `q` using lap parity,
retirement markers, or chain following.

## Memory ordering

| operation | order |
|---|---|
| claim CAS `FREE(p) -> CLAIMED` | acquire success, relaxed failure |
| successor promotion `Claim[q] = FREE(q)` | relaxed (exclusive, publishes no payload) |
| vouch `Claim[p] = CLEARED` | release (publishes the promotion) |
| Claim walk / reader load | acquire |
| `Result.len` | relaxed (published by the tag) |
| `Result.tag` | release / acquire |
| recovery `ABORTED` | release |
| `write_hint` | release / acquire |
| `read_pos` | reader release / writer acquire |

## Cross-process attach

Both planes are sized into the same memfd as the arena, covered by the same
`ftruncate`, `fallocate`, and seals. `Control` records the plane strides so an
attaching process can reconstruct the layout from the descriptor alone.
`MpscRing::attach(fd)` refuses a region whose cell layout differs from the
binary's — a compact binary attaching to a padded region would index the Claim
plane with the wrong stride and read a neighbouring record's ownership word.

The public ring `attach()` functions duplicate the descriptor and leave the
caller's fd untouched on both success and failure. The lower-level
`Region::attach()` takes ownership on success and leaves the fd untouched on
failure. Probing for the right ring layout is therefore safe.

## Measurement

Zen 5 (Ryzen AI 9 HX 370), 56-byte payloads, 1 MiB capacity, pinned, 25
interleaved independent processes per point, medians:

| writers | 1 | 2 | 4 | 8 | 16 |
|---|---:|---:|---:|---:|---:|
| `MpscRing<>` Mrec/s | 41.9 | 39.9 | 36.8 | 15.5 | 9.7 |
| misses / record | 1.3 | 1.4 | 1.7 | 3.2 | 5.3 |
| IPC | 1.30 | 1.35 | 1.35 | 1.28 | 1.33 |
| fairness (min/max) | 1.00 | 0.96 | 0.82 | 0.50 | 0.79 |

It does not scale *up* — one total order still means one successful claim per
record — but it degrades gracefully instead of collapsing.

`MpscRing<DefaultPolicy, true>` (one cell per 64-byte line, 200% metadata) is **slower**
above two writers: spreading the working set across more lines costs more than
the false sharing it removes. Compact is the default; padded is retained as a
measurement control.

### Method

* The replicate is a **process**, not an in-process loop: launch captures
  scheduler, thermal, allocator, and ASLR variation instead of averaging it away.
* Configurations are interleaved in shuffled order within each round.
* Coherence counters come from `perf_event_open` on each writer thread, enabled
  around the timed window only. Measured non-perturbing (throughput with and
  without counters is indistinguishable). `perf c2c` is unavailable on this host:
  it needs AMD IBS precise memory sampling and no `ibs_op` PMU is exposed under
  WSL2, so attribution is per-thread plus ablation, never per-address.
* Valgrind cannot substitute for the PMU here — it serialises threads, so
  cross-core ownership transfer never occurs and cachegrind models no coherence
  protocol at all.
* **Always read the fairness ratio next to a throughput number.** A private-hint
  experiment once looked 6x faster at four writers purely because it starved every
  writer but one, and total throughput reported that as a win.
* Machine load is the dominant error term: a campaign run while other processes
  compiled was wrong by 3.5x and inverted a headline result.

## Verification status

* `mpsc_ring_test.cc` — differential against a mutex-guarded reference with short
  commits, multi-writer integrity and per-writer FIFO, zero-length records, full
  boundary, abort, in-flight non-blocking, reader restart, live-reader
  displacement. Both cell layouts instantiated everywhere.
* `mpsc_ring_fault_test.cc` — fork-based: killed mid-reservation, recovery
  promotes the successor cell, committed-then-dead delivered rather than
  reclaimed, repeated kills across wraps, stopped writer never reclaimed,
  stopped writer blocks the reader but not other writers, cross-process attach by
  fd, layout-mismatch refusal.
* `region_test.cc`, `spsc_test.cc` — mapping, control-area layout including the
  shared writer registry, backends, descriptor encoding; SPSC short commit,
  boundary sizes, and single-writer enforcement.
* `ordering_mutants.sh` — release→relaxed mutants of the vouch and the Result-tag
  publication. **x86-64 is TSO, so these mutants are byte-identical there**; only
  the aarch64 CI leg can adjudicate them, and qemu-user is blind (host TSO).
* `docs/verification/queue_model.py` — dependency-free bounded state exploration
  of both surviving protocols. The current bounds explore 998 SPSC states and
  3,483 MPSC states and reject four SPSC and eight MPSC negative controls,
  including wrong-lap claims, successor-before-vouch, stale-tag recovery,
  publish-before-descriptor, missing generation fencing, and missing capacity
  slack. See `docs/verification/queue-model.md` for the exact bounds and
  assumptions.
* Break-first discipline: a test is not trusted until it has been shown to fail
  against a deliberately broken implementation, built from a single snapshot copy
  so a concurrent edit cannot shift the baseline.
* **ThreadSanitizer: clean**, 17 cases / 3,055,434 assertions, 3/3 repeat runs,
  with NO suppression file. The suppressions that `in-band queue` required covered one
  class -- a stale walker's atomic probe landing on bytes concurrently serving as
  another record's payload, which is inherent to an in-band descriptor. This
  design has no in-band descriptor, so that class is structurally absent and the
  suppression file has been deleted rather than carried forward: its rule was a
  broad substring match on `reserve`, which would have silently hidden a genuine
  race in the surviving hot path.
  Two caveats on that result. It requires the system toolchain -- the pinned musl
  GCC has no libtsan -- so it validates a different compiler's codegen than
  production. And it covers the in-process suites only; fork-based fault
  injection does not mix with TSan, so the writer-death paths are NOT covered by
  it.
* Empirical stress, all clean: 64 writers on 24 cores, 256-byte capacity forcing
  constant wraps, adversarial size mixes, zero-length-heavy, slow and pausing
  readers, ASan+UBSan, and two-CPU `taskset` contention.

## Known gaps

* **The executable model is bounded evidence, not an unbounded proof.** It uses a
  sequentially-consistent transition abstraction with explicit publication
  ordering, a four-grain ring, two MPSC writers, finite protocol depth, and no
  integer wrap. It does not prove the full C++ weak-memory state space, Linux IPC
  atomic ABI assumptions, PID-namespace death detection, or scheduler fairness.
  Native AArch64 ordering-mutant runs remain required evidence.
* **Resolved defect: recovery now relies on successor blocking.** A walker may walk past `kClaimed` but cannot claim its immediate successor, so `MpscRing::recover()` in `mpsc_ring.hh` can promote that successor unconditionally before marking the dead record aborted.
* `threadAlive()` (`liveness.hh`) safely defaults to returning `true` (ALIVE) on resource errors (such as `EMFILE`, `ENFILE`, `EACCES`, or `EINTR` retries), adhering to the false-ALIVE safety doctrine. `existenceProbeSaysGone()` (`sched_getscheduler`, no fd required) returning `ESRCH` and `ENOENT` (from `/proc/<tid>/stat`) are the only conditions that prove a thread is DEAD.
* Writer-death paths are covered by fork-based tests but NOT by TSan or by the
  in-process stress suite, which is why writer-death recovery paths require standalone
  fork-based testing.
* `sharded` composition is not wired up; `MpscRing` is a standalone
  single-shard ring.
* The mutant that overlays the planes on the payload arena is caught by hanging
  rather than by a clean assertion — a weaker signal than the others.
* Fairness figures are confounded by this being a heterogeneous CPU (4x Zen 5 +
  8x Zen 5c), so writers land on cores with different peak throughput. The
  comparisons are controlled (same pinning); the absolute values should not be
  over-read.
