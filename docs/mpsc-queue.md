# Variable-Length MPSC Byte Queue — Algorithm Specification

A many-writer, single-reader queue of variable-length byte records in shared memory,
delivering a single totally-ordered stream, tolerant of writers stopped or killed at
any instruction, with **no timeout constant anywhere**.

Status: revision 3. Revision 1 had five critical defects found by adversarial analysis;
r2 fixed them and was verified clean; r3 adds the two-phase API and the fixes from a
third pass. Not implemented. A bounded model check is in progress.

**Scope: the reader busy-polls.** Blocking, sleeping, and wakeup are out of scope —
see [Notification](#notification-out-of-scope).

## Properties

| Property | Value |
|---|---|
| Writers | Many, cross-process, mortal |
| Readers | One, busy-polling |
| Ordering | Total order over all records |
| Writer progress | Lock-free (claim may retry) |
| Reader progress | Wait-free except at a stalled record |
| Allocation | None after construction |
| Recovery | **Reader-only**, gated on proven thread death, never a timeout |
| Max record | Remaining ring capacity |
| Target | Linux, x86-64 |
| Backends | memfd, shm_open, tmpfs, file — all descriptor-based |
| Deployment | Process-local or cross-process, same protocol |

Lock-free rather than wait-free is deliberate: records may be as large as the free
space, so admission must be able to *decline*, and only a conditional atomic can.
See [Why not fetch_add](#why-not-fetch_add).

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

Eight bytes at the head of every record slot, 64-byte aligned. `state` occupies the
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

**`FREE` must be 0 deliberately.** An untouched arena word is all zeros, which then
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

## Why writers stamp their successor

The claim CAS requires the slot to read `FREE(p)`, so stale data must be replaced.
On the next lap a descriptor position lands in the middle of the previous lap's
*payload*, not on a previous descriptor, because variable-length records give different
boundaries every lap. Reader-side zeroing of retired descriptors is therefore useless.

The alternative — the reader memsets the whole consumed region, as Agrona's
`ManyToOneRingBuffer` does — costs a full write pass over every byte read. Each writer
stamping its successor costs 8 bytes at a known boundary. The price is that claims form
a causal chain: a writer cannot claim until its predecessor has claimed *and* stamped.

## Writer

The API is two-phase. A single-shot `write()` cannot express a short commit — the
caller must be able to reserve an upper bound, discover the actual length while
filling, and commit less. Reservation state is writer-private, and a writer may hold
at most one reservation at a time.

```c
struct Reservation {           // writer-private, thread-local
    uint64_t p;                // claimed position; INVALID when not held
    uint64_t need;             // reservation extent
    uint64_t n;                // requested payload length
    uint64_t word;             // the claim word we wrote (carries our tid)
};

std::span<std::byte> reserve(size_t n);   // claim, stamp successor, vouch
void commit(size_t actual_n);             // actual_n <= n; publishes
void abort();                             // publishes ABORTED, no payload

bool write(const void *data, size_t n) {  // convenience: reserve + copy + commit
    auto s = reserve(n);
    if (s.empty()) return false;
    memcpy(s.data(), data, n);
    commit(n);
    return true;
}
```

**The span returned by `reserve` is valid only until `commit` or `abort`.** The reader
may overwrite those bytes once it has passed the record, and `read_pos` publication is
what licenses that — so a caller must finish writing before committing, and must not
retain the span afterwards. Symmetrically on the read side, the span from `peek` is
valid only until `pop`.

### Misuse guards

Two of these are silent-corruption paths reachable by ordinary caller bugs, so they are
correctness guards rather than hardening, and belong in the code from day one. All are
thread-local checks with no shared-state cost.

```c
void commit(size_t actual_n) {
    assert(res.p != INVALID);                          // (b) double commit
    assert(gettid() == tid_of(res.word));              // (e) fork / wrong thread
    assert(align64(8 + actual_n) <= res.need);         // (c) grown commit
    ...
    res.p = INVALID;                                   // poison
}
```

| Misuse | Consequence | Guard |
|---|---|---|
| (a) never commits, thread lives | queue wedges — reader waits forever at CLEARED+alive | RAII reservation whose destructor aborts. **Self-healing if the thread instead exits**: the tid goes dead and the reader recovers. Only an immortal thread that never commits wedges the queue, and the no-timeout doctrine makes that indistinguishable from slow user code by design. |
| (b) commit twice | **silent corruption** — after the ring wraps, the second store smashes a live later-lap descriptor. This is the r1 delayed-stamper defect resurrected through the API. | poison `res.p`, assert |
| (c) `actual_n > n` | **silent corruption** — commit stores a grown size with no trailer, so `p + used` is a fictitious boundary and walkers diverge | assert against `res.need` |
| (d) reserve twice | first reservation orphaned in CLEARED with a live owner — wedge, no corruption | assert `res.p == INVALID` |
| (e) commit after `fork()` | child inherits the mapping *and* the thread-local reservation; both may commit | assert `gettid()` matches; `pthread_atfork` child handler clears `res` |
| (f) writes span after commit | arbitrary corruption — those bytes may be another record's payload *or its descriptor* | contract only; not catchable |

```c
std::span<std::byte> reserve(size_t n) {
    size_t need = align64(8 + n);
    uint64_t p, d, mine;

    for (;;) {
        p = write_hint.load(acquire);           // release/acquire — see ordering
        unsigned hops = 0;

        for (;;) {                              // walk to the true frontier
            if (++hops > N / 64) goto restart;  // bounded: stale walk
            d = desc[p].load(acquire);
            if (state_of(d) == FREE) {
                if (free_pos(d) != p) goto restart;   // recycled slot, stale walk
                break;                                // frontier
            }
            if (state_of(d) == CLAIMED) {       // owner alive, or reader will recover
                policy.on_busy(hops);
                goto restart;
            }
            p += extent_of(d);                  // vouched by I2
        }

        if (p < read_pos_cached) goto restart;  // stale walk, NOT full
        if (p + need + 64 - read_pos_cached > N) {
            read_pos_cached = read_pos.load(acquire);
            if (p < read_pos_cached) goto restart;
            if (p + need + 64 - read_pos_cached > N) {
                policy.on_full(0);
                // full is normal backpressure; a DEAD reader means it will never
                // drain, so report that distinctly rather than looping forever
                if (!thread_alive(reader_tid)) return {};   // + set errno/flag
                return {};                      // genuinely full, fail fast
            }
        }

        // claim and identify in ONE atomic. Expected value carries p, so a stale
        // claimant from an earlier lap fails deterministically. See Lemma 1: a
        // successful CAS proves p is the frontier, however the walk computed it.
        uint64_t expected = free_word(p);
        mine = pack(need >> 6, 0, CLAIMED, my_tid);
        if (desc[p].compare_exchange_strong(expected, mine, acquire, relaxed)) break;
        policy.on_contended(0);
    restart:;
    }

    uint64_t q = p + need;

    // stamp the successor as FREE(q). CAS from observed — belt and braces; by
    // Lemma 2 no concurrent mutator of this slot can exist.
    uint64_t g = desc[q].load(relaxed);
    desc[q].compare_exchange_strong(g, free_word(q), relaxed, relaxed);

    desc[p].store(with_state(mine, CLEARED), release);   // vouch (I2)
    write_hint.store(q, release);                        // release — see ordering

    res = { p, need, mine };                             // writer-private
    return { base + ((p + 8) & mask), n };
}

void commit(size_t actual_n) {
    size_t used = align64(8 + actual_n);
    // Trailer FIRST, then shrink size (I1 as refinement). Both are published by
    // the commit release below, so no observer sees a shrunk size without a trailer.
    if (used < res.need)
        desc[res.p + used].store(
            pack((res.need - used) >> 6, 0, ABORTED, my_tid), relaxed);
    desc[res.p].store(pack(used >> 6, actual_n & 63, COMMITTED, my_tid), release);
}

void abort(void) {
    desc[res.p].store(with_state(res.word, ABORTED), release);
}
```

`abort()` is safe with no trailer: the whole reservation stays one ABORTED record of
its original extent, and the successor at `p + need` was already stamped during
`reserve`. Death during abort converges from both sides — the store either landed
(ABORTED, reader pops) or did not (CLEARED with a dead owner, reader recovers to the
identical word).

**The reserve-to-commit window is a queue-wide head-of-line pin.** The reader cannot
pass `p`, so consumption halts and admission continues only until the ring fills behind
`read_pos`. User-code duration between `reserve` and `commit` therefore bounds queue
availability for *every* participant. This is a contract on callers, not something the
queue can defend against — the no-timeout doctrine means a slow writer and a hung
writer are deliberately indistinguishable.

Writers do **not** help. On reaching a `CLAIMED` descriptor a writer backs off and
retries, whether the owner is alive or dead. Recovery belongs to the reader alone.

### Why recovery is reader-only

Revision 1 let any writer help, and it was unsound in a way no per-word CAS discipline
fixes. A helper's license is "the owner is dead" — a permanent fact — while its
authority over the successor slot expires the moment anyone else completes recovery. A
helper stopped before loading the successor slot resumes, loads a *live* claim placed
there by a later writer, and stamps over it. `CAS`-from-observed does not help: it
guards against a stale expected value, not against a fresh load in the wrong era.

Reader-only recovery has exactly one actor. A stalled reader freezes `read_pos`, and by
I3 nothing it might touch can be recycled — so even the reader's own delayed stamp is
safe.

The cost: a dead writer stalls all writers until the reader drains to that record. With
a deep backlog that is the whole backlog. Acceptable because recovery is cold and
head-of-line blocking is already inherent to the total-order requirement — but it is a
real latency consequence, not a free simplification.

## Reader

```c
span peek(void) {
    uint64_t d = desc[rd].load(acquire);        // pairs with commit's release
    switch (state_of(d)) {
    case FREE:      return {};                  // frontier
    case ABORTED:   pop(); return peek();
    case COMMITTED: return { base + ((rd + 8) & mask), committed_len(d) };
    case CLAIMED:
    case CLEARED:
        if (thread_alive(tid_of(d))) { policy.on_busy(0); return {}; }
        // MUST re-read. Between the load of d and the liveness check the owner
        // can have finished the protocol AND died, so d is a stale snapshot.
        // A dead owner's word is frozen — no writer helps, and no one can claim
        // a slot that does not read FREE — so this re-read is final.
        d = desc[rd].load(acquire);
        if (state_of(d) == CLAIMED || state_of(d) == CLEARED)
            recover(rd, d);
        return peek();                          // re-dispatch on the fresh value
    }
}

void recover(uint64_t p, uint64_t d) {
    if (state_of(d) == CLAIMED) {               // owner never stamped its successor
        uint64_t q = p + extent_of(d);          // exact boundary, from I1
        uint64_t g = desc[q].load(relaxed);
        bool ok = desc[q].compare_exchange_strong(g, free_word(q), relaxed, relaxed);
        assert(ok);                             // Lemma 2: no concurrent mutator
    }
    // release: a walker's acquire-load of ABORTED must observe the stamp above
    desc[p].store(with_state(d, ABORTED), release);
}

void pop(void) {
    rd += extent_of(desc[rd].load(relaxed));    // rd is reader-private
    read_pos.store(rd, release);                // license to overwrite
}
```

`rd` is the reader's private cursor; `read_pos` is its shared publication. Publish per
batch rather than per record — it cuts the reader's RFO rate and the writers'
`read_pos_cached` refresh traffic.

**`recover` must never act on a descriptor value read before the liveness check.** The
owner can complete the whole protocol and die in that gap, so a snapshot taken while it
was in flight may describe a record that is now COMMITTED — and stamping ABORTED over it
destroys a delivered-but-unread record, while stamping its computed successor can land
`FREE(q)` on a live claim placed there by a later writer. Re-read after the liveness
check and dispatch on the fresh value. This is safe because a dead owner's word is
frozen: no writer helps, and no one can claim a slot that does not read FREE.

Lemma 2's premise is "the owner is uncommitted" — it must be evaluated at the moment of
the action, not at the moment of the snapshot. Revision 3 evaluated it at the snapshot
and was wrong; a bounded model check found it in 337 states after three inspection
passes had missed it.

**`recover` must stamp the successor before stamping ABORTED, and the ABORTED store
must be release.** Stamping ABORTED alone makes I2 lie: ABORTED promises the successor
is trustworthy while it still holds stale payload, and the next walker advances into
garbage, misparses a length, and may claim or deliver an arbitrary range. A non-release
store reintroduces the same failure on any weakly-ordered target.

A `CLEARED` record needs no successor stamp — its owner already did that — so recovery
there is the ABORTED store alone.

### Liveness check

```c
bool thread_alive(uint32_t tid);   // /proc/<tid>/stat, state field
```

**Zombies must count as dead.** `tgkill(tgid, tid, 0)` succeeds for a zombie group
leader and `/proc/<tid>` exists for one, so a writer whose process died mid-claim but
was never reaped reads as alive forever — the reader backs off forever and the queue
hangs with no thread stopped. Parse the state field and treat `Z` and `X` as dead.
Exited *threads* inside a live process are auto-reaped, so the pure-thread case is
safe either way.

`tgkill` remains a valid fast path for the process-local configuration, where the
zombie case cannot arise.

**An OS-level ordering assumption, stated because it is invisible to every formal
tool used here.** Recovery is sound only if a dead owner's final stores are visible
to the recovering reader before the liveness check reports death. Under the C++
model that edge does not exist: `thread_alive()` is a syscall, not a synchronizing
operation, so nothing in the language orders the owner's release stores against the
reader's observation of `Z`/`X` in `/proc`. The guarantee comes from the kernel —
its exit path drains the dying task's stores before the task becomes reapable. That
holds on Linux, but it is an assumption about the operating system, not a derivation
from the memory model, and no amount of model checking here can validate it. A port
to another OS must re-establish it.

### Reader death and restart

**Death is defined behavior.** No reader means no progress, so the ring fills and
writers take backpressure — capacity checks fail, `reserve` returns empty, callers
handle it. Nothing corrupts. A reader dying inside `recover()` between the successor
stamp and the ABORTED store leaves an inert state; one dying inside `pop` loses only an
unpublished private cursor.

**Restart needs no new state, because the reader keeps none.** `rd` is a cache of
`read_pos`, which lives in the control page; the descriptor chain is ground truth; and
`recover()` is idempotent, so a half-finished recovery is simply re-run — the stamp CAS
observes `FREE(q)` and rewrites the identical value, then ABORTED lands.

```c
bool attach_reader(void) {
    uint32_t cur = reader_tid.load(acquire);
    if (cur != NONE && thread_alive(cur)) return false;   // a live reader exists
    if (!reader_tid.compare_exchange_strong(cur, gettid(), acq_rel, relaxed))
        return false;                                     // lost the race
    rd = read_pos.load(acquire);                          // resume exactly here
    return true;
}
```

Two properties make this sufficient. **Exactly one reader** is enforced by the CAS —
concurrent claimants race and one wins. **Takeover requires proven death**, the same
`/proc/<tid>` check used everywhere else, so a merely-stopped reader is never displaced;
and since a dead thread never resumes, no generation counter is needed to fence it.

**This matters for IPC, not for process-local.** Process-local, a dead reader thread
inside a live process can be replaced by another thread of that process — useful, but
the common failure is the whole process dying, which takes the writers and the memfd
with it and leaves nothing to restart into. Cross-process is where restart earns its
keep: the writers are separate processes that keep running and keep buffering, the
reader process crashes, and a fresh reader attaches and resumes without losing what
accumulated in between.

Obtaining the fd is a deployment question, not a queue one: while any writer lives the
memfd lives, so a successor gets it by `SCM_RIGHTS` re-donation, `/proc/<pid>/fd/<n>`,
or a named backend. If every peer died there is nothing to preserve anyway.

This is the one place a **named backend** pays for its lost seals: `shm_open` gives a
successor a path it can open unaided, with no dependence on a surviving writer or a
supervisor holding the fd. Weigh that against `F_SEAL_SHRINK`, which only memfd offers.

**Delivery becomes at-least-once across a restart.** A reader that died between `peek`
and `pop` has already handed the record to the application, and the successor resumes at
`read_pos` and redelivers it. Applications needing exactly-once must be idempotent, or
pop before processing and accept at-most-once instead.

### Reader backoff

Under a total order the reader cannot skip, so on reaching an in-flight record it polls
that exact descriptor — which shares a line with the payload its owner is writing. Each
poll downgrades the line M→S and each payload store re-upgrades it. **Exponential
backoff is not optional**; without it one slow writer costs the queue far more than its
own latency.

### Traversal cost

Walking by inline `size` is not a pointer chase: the address sequence is monotone
sequential, so the hardware stream prefetcher covers the dependency chain. Measured on
Zen 5, cross-core with lines Modified in the writer's cache: dependent walk
5.02 ns/record vs 5.24 for precomputed offsets. A shuffled chase is 13.8 ns/rec warm
and 153 ns/rec from DRAM. Software-prefetch and two-pass header/payload variants both
measured *worse*; do not build them.

## Memory ordering

Ordering strength and coherence traffic are independent. A relaxed RMW still acquires
its line exclusively. On x86-64 most of this table costs nothing at runtime; it is
written against the C++ model so the code is correct rather than accidentally correct.

| Op | Order | Why not weaker |
|---|---|---|
| claim `CAS(desc[p], FREE(p)→CLAIMED)` | **acquire** / relaxed fail | Stops the payload memcpy hoisting above the claim. |
| stamp `CAS(desc[q], g→FREE(q))` | **relaxed** | Publishes no data; the successor only needs to observe it. |
| vouch `desc[p] = CLEARED` | **release** | Publishes the stamp above. |
| trailer `desc[p+used] = ABORTED` | **relaxed** | Published by the commit release that follows. No path reaches `p+used` except through an acquire of the commit word. |
| commit `desc[p] = COMMITTED` | **release** | Publishes the payload. Irreducible. |
| walk load `desc[p]` | **acquire** | Reading CLEARED, you rely on seeing the successor stamp. |
| `write_hint` store / load | **release** / **acquire** | See below. |
| `read_pos` store (reader) | **release** | Licenses overwrite of bytes it may still be reading. |
| `read_pos` load (writer) | **acquire** | Pairs with the above. |
| recover stamp `CAS(desc[q], …)` | **relaxed** | Published by the ABORTED release that follows. |
| recover `desc[p] = ABORTED` | **release** | Publishes the stamp above. |

**`write_hint` needs release/acquire, not relaxed.** Revision 1 claimed the hint "can
only regress, never overshoot" — false in the model it claimed to target. A release
store does not stop a *later* relaxed store from becoming visible first, so on a weakly
ordered target another writer can observe the new hint while the vouch and the
successor stamp are both still invisible, then dereference stale bytes as a descriptor.
Free on x86-64.

Hint *regression* is genuinely bounded, by I3: a writer stalled before its hint store
has an uncommitted record, so the reader is pinned and the ring cannot wrap, so a late
stale store always lands within the live window.

**Never continue a walk from the claim CAS's failure value.** It is read relaxed; a
`CLEARED` word observed without acquire does not guarantee visibility of the successor
stamp. Restart the walk, which re-reads with acquire.

Exclusive line acquisitions per record: claim CAS (1), successor stamp (1, different
line), `write_hint` store (1, the most contended line in the design), plus payload. The
vouch and commit stores hit a line held M from the claim, so they are free unless the
reader polled it in between.

## Policies

Compile-time template parameters with empty defaults, never virtual — an unused hook
must vanish entirely.

```cpp
struct DefaultPolicy {
    // wait policies — called with the iteration count, so a backoff ladder
    // needs no state of its own
    void on_empty(unsigned iter)     noexcept {}  // reader: nothing to read
    void on_busy(unsigned iter)      noexcept {}  // reader/writer: record in flight
    void on_full(unsigned iter)      noexcept {}  // writer: no capacity
    void on_contended(unsigned iter) noexcept {}  // writer: lost the claim CAS

    // event hooks — fire and forget, must not block
    void on_reclaim(uint64_t pos, uint32_t tid, uint64_t bytes) noexcept {}
    void on_abort(uint64_t pos, uint64_t bytes)                 noexcept {}
    void on_wrap(uint64_t lap)                                  noexcept {}

    // instrumentation
    void on_claim(uint64_t pos, uint32_t size, unsigned hops)   noexcept {}
    void on_commit(uint64_t pos, uint32_t size)                 noexcept {}
};
```

**`on_empty` and `on_busy` must stay separate.** *Empty* means no data exists — the
frontier is reached and the wait may be arbitrarily long. *Busy* means a record is
in flight and will arrive when its writer commits.

Under the two-phase API the in-flight window is **caller-controlled**, not bounded by
the queue: a writer may hold a reservation across arbitrary user code, so `on_busy` can
legitimately last milliseconds. It therefore wants a full backoff ladder that may end
in sleeping, not a pure spin. Its default cannot be truly empty either — it carries the
mandatory reader backoff and should at minimum issue a `pause`.

`on_reclaim` fires exactly when a writer was found dead and its record recovered. That
is otherwise invisible, and a queue quietly recovering from dying writers is something
to alert on.

## Instrumentation

The policy type is the instrumentation seam — an instrumented build is a different
policy, not a different queue. `hops` is the cheapest actionable metric: it measures how
stale `write_hint` is, and therefore how to tune its update frequency.

| Tier | Mechanism | Hot-path cost | Perturbs? |
|---|---|---|---|
| 0 production | empty hooks | zero | no |
| 1 counters | thread-local increments | ~1 cycle | no |
| 2 timing | `rdtsc`, sampled 1-in-N | ~20-30 cycles/sample | **yes** |
| 3 tracing | per-record to a side buffer | high | yes |
| 4 hardware | `perf` / PEBS / `perf c2c` | zero | no |

**Never a shared counter.** A contended atomic increment costs 50-100 ns — more than
the claim CAS it measures — and manufactures the coherence traffic the design avoids.
Software counters must be thread-local and aggregated at teardown.

Stamping a timestamp between claim and commit **lengthens the in-flight window**, which
is what determines how often the reader hits a busy record and how long the successor
stalls. Tier-2 numbers are an upper bound on latency and a lower bound on throughput.

`perf c2c` is the right tool for the known coherence hazards: it reports HITM per cache
line *and per offset within the line*, so it shows whether a descriptor is ping-ponging
against its own payload and how hot `write_hint` really is.

### What to measure

1. **Does clear-forward's causal chain beat `fetch_add`?** Build both claim paths behind
   the policy and A/B at fixed record size. `fetch_add` caps near 10-20 M claims/s from
   coherence alone.
2. **How stale is `write_hint`?** Tier-1 hop histogram.
3. **Does the reader's in-flight poll hurt?** `perf c2c`, with and without backoff.
4. **Real full-rate** against padding waste at the actual record-size distribution.

## Notification (out of scope)

The reader busy-polls. If a blocking mode is added, notification should be an injected
policy object — the Aeron `IdleStrategy` / Disruptor `WaitStrategy` pattern — so the
busy-poll deployment keeps a `write` path that ends at the commit store.

One hazard to record now. A writer that publishes and then checks whether the reader is
asleep, against a reader that marks itself asleep and then checks whether the queue is
empty, is the store-buffering litmus shape: both sides can read stale values, the reader
sleeps, and the record is never delivered. It requires a **standalone
`atomic_thread_fence(seq_cst)`** on both sides. It cannot be satisfied by a nearby
locked RMW — a seq_cst RMW on one location orders nothing about a relaxed load of
another, and on AArch64 that formulation emits no barrier at all.

Gate the wake behind a `compare_exchange` so exactly one writer issues the syscall.
Wake latency is dominated by CPU idle-state exit (~1-3 µs from C1, ~50-200 µs from C6),
not the syscall (~100-300 ns), so capping C-state depth via `/dev/cpu_dma_latency`
matters more than anything in the queue.

## Why not fetch_add

**Admission.** `fetch_add` cannot decline. Once it lands the region is irrevocably part
of the byte sequence — `fetch_sub` is unsound because later claims are already
positioned after yours. Over-claim can be made safe by pre-paying headroom of
`(W-1) × max_record`, but `max_record` here is the remaining capacity, so the headroom
required is the whole ring.

**Identity.** `fetch_add` returns a position; stamping the descriptor is a separate
store. Between them the position exists with nothing identifying its owner — and under
a total order the reader cannot skip it. That is the failure that forces a timeout.

CAS-on-descriptor makes claim and identity one atomic, so an unattributed hole cannot
exist. Wait-freedom is the price.

Wait-free is not the same as non-serializing: every `lock xadd` on a shared cursor is
serialized by coherence at ~50-100 ns per migration, capping a shared cursor near
10-20 M claims/s regardless of instruction. The chain here is *causally* serialized,
which is stronger — benchmark it against that ceiling.

## Invariants to check

**Safety**

- S1 No two live claims cover overlapping byte ranges.
- S2 A stamp never overwrites a live claim.
- S3 The reader never observes a partially written payload.
- S4 Every boundary any actor computes is a real record boundary.
- S5 Recovery never acts on behalf of a live thread.
- S6 Every boundary derivable from any observed `size` is real (I1).
- S7 `write_hint` never exceeds the true frontier.
- S8 A record is delivered at most once, in claim order.

**Liveness**

- L1 If all writers are alive, some writer eventually claims.
- L2 If a writer dies mid-protocol, the reader eventually recovers it and the queue
     accepts writes again — **conditional on reader liveness**, which reader-only
     recovery makes explicit.
- L3 A committed record is eventually delivered, unless the reader dies.

**Crash model.** Any writer may stop between any two steps, permanently (killed) or for
an unbounded finite time (stopped). Only the permanent case may be recovered from. A
killed thread has no pending stores; a stopped thread resumes with stale registers and
executes its next instruction late.

## Verification status

**Exhaustive (strong claim).** Spin, 2 writers + 1 reader, 4 slots, records of 1-2
slots, commit / abort / short-commit all nondeterministic, death possible at every step
boundary, ring wrapping twice. **207,093,640 states, 468M transitions, zero errors**,
no invalid end states. S1-S5, S7, S8 all hold.

The coverage listing is worth more than the pass: the only unreached code is the two
`assert(false)` arms — the writer's successor-stamp CAS failure and `recover`'s CAS
failure. **Spin proved those CASes never fail**, which is Lemma 2 established as
unreachability rather than by argument.

Liveness, exhaustive at 2 writers x 1 op under weak fairness: no fair non-progress
cycle, and every fair execution terminates with the queue drained. This subsumes L2 —
fairness forces recovery, so dead writers are always recovered.

Fault injection validates the model has teeth. Three r1 defects re-introduced one at a
time were each detected: `FREE` as plain zero (lap ABA, depth 101), `recover` stamping
without stamping the successor (I2 violation, depth 143), and writers helping (a
fresh-observed CAS landing FREE on a live claim, depth 271). A model that only ever
passes proves nothing; this one demonstrably sees the defect classes that matter.

**Bounded (weaker claim).** 3 writers x 2 ops ran 199M states with no errors before
being killed at 12.5 GB — that configuration does not fit exhaustively on the machine
used. Report it as bounded, never as exhaustive.

**Not covered.** Spin is sequentially consistent, so **none of this validates the
memory-ordering table**. That rests on the per-operation justifications and on compiled
evidence (GCC 13.3 `-O2`: `atomic_thread_fence(seq_cst)` lowers to `lock or [rsp],0`
on x86-64 and `dmb ish` on AArch64). Closing it properly wants a weak-memory tool —
GenMC, Nidhugg, or CBMC — or herd7 for individual litmus shapes. Also outside every
tool used: the OS-level ordering assumption documented under Liveness.

## Known gaps

- **tid reuse** is assumed not to occur.
- **Reclaim is FIFO-only.** Contiguous variable-length records can only be freed by
  advancing `read_pos` in order. Blocks any future multi-reader or out-of-order
  consumption.
- **tid reuse** is assumed not to occur. The `/proc/<tid>` check is tid-only, so any
  process recycling that tid blocks recovery — a false-*alive*, which is the safe
  direction, but it hangs rather than corrupting.
- **All peers must share a PID namespace** for the `/proc` liveness check.
- **`FALLOC_FL_PUNCH_HOLE` is not blocked** by the applied seals — only `F_SEAL_WRITE`
  blocks it and that cannot be used here. A peer punching holes causes silent zeroing
  rather than `SIGBUS`. Out of scope under the cooperative trust model, but it should be
  stated rather than implied to be sealed.

## Revision history

**r5** — bounded model check (Spin, 2 writers, 4 slots, ring wrapping twice, death
possible at every step boundary) found a defect three inspection passes had missed:
`recover()` acted on the descriptor value snapshotted by `peek()` before the liveness
check, so an owner that completed and died in that gap had its COMMITTED record stamped
ABORTED, or had a later writer's live claim at its computed successor stamped FREE. Fixed
by re-reading after the liveness check and dispatching on the fresh value. Lemma 2
annotated: its premise must be evaluated at the moment of the action, not at an earlier
snapshot. All three deliberately re-introduced r1 defects were detected by the same
model, so the clean runs carry weight.

**r4** — reader death reclassified from unhandled gap to defined behavior (no reader
means no progress means backpressure, which is correct), and reader **restart** shown to
need no new state: the reader keeps none that isn't in the control page, and `recover()`
is idempotent, so a successor reads `read_pos` and resumes. Added `reader_tid` as both
the takeover word and the way a back-pressured writer distinguishes a dead reader from a
slow one. Restart is scoped to the IPC deployment, where writers outlive the reader.

**r3b** — third analysis pass: the reserve/commit split introduced no protocol defect,
but Lemma 3 as first written was too strong (its happens-before chain has a genuine gap
for a writer that dies mid-flight; that corner is closed by the crash-model axiom
instead, and restart-on-mismatch is load-bearing for progress rather than defense in
depth). Double commit and grown commit identified as silent-corruption paths and given
mandatory asserts. I3 restated for arbitrary-duration windows; Lemmas 1 and 2 rescoped;
trailer ordering corrected to relaxed in the table to match the code; `on_busy`
doctrine corrected now that the in-flight window is caller-controlled.

**r3** — verification pass on r2 found no critical defect; these are the two
underspecifications it did find, plus hardening. Two-phase `reserve`/`commit`/`abort`
API, without which `actual_n` had no source and the short-commit trailer was again dead
code; state encodings pinned numerically with `FREE = 0` chosen deliberately; the three
lemmas stated explicitly as the model-check targets; span-lifetime contract stated;
`recover`'s stamp CAS asserted.

**r2** — fixes from adversarial analysis of r1: FREE carries its position (closes
claim-CAS lap ABA from a resumed stopped writer); recovery restricted to the reader
(closes delayed-helper stamp races that no per-word CAS discipline could fix);
`recover()` specified, stamping successor before a release ABORTED (closes an I2
violation that let walkers advance into garbage); `write_hint` release/acquire (r1's
"can only regress" claim was false); descriptor re-laid out with a 6-bit remainder and
22-bit tid so record lengths are representable at all, and short commit made reachable
by writing the trailer before shrinking `size`; I1 restated as boundary-refinement; I3
stated; zombie-aware liveness; bounded walk with stale-walk restart replacing an
insufficient snapshot guard.
