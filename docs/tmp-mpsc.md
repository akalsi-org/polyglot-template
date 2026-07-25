# Variable-Length MPSC Byte Queue Over Mirrored Memory

Status: **design proposal only.** Nothing described here is implemented, and no
`repo.sh` command builds or verifies it. This document is a hand-off record of a
design conversation; it is deliberately named `tmp-` and is not listed in
[DESIGN-README.md](DESIGN-README.md). Promote it to `proposals/` under a real
name before treating any part of it as a commitment.

## Scope

A multi-producer, single-consumer queue of **variable-length byte records**,
usable both within one process and across processes, built on a memory region
mapped twice so that record wraparound is invisible.

Target intent captured from the requirements discussion:

| Axis | Decision | Rationale |
| --- | --- | --- |
| Trust model | Cooperative but crash-prone peers | Trusted code; processes may be OOM-killed or `SIGSTOP`ped. No payload validation; crash containment required. |
| Producer count | Either `< 10` or `100..1000` | Both regimes must work from one implementation. |
| Ordering | Per-shard FIFO, no global FIFO | See [Ordering](#ordering-decision). Recoverable approximately via `rdtsc` stamps. |
| Priority | Low latency over throughput | Drives the arbitration and notification choices. |
| Language | C++ core in `cpp/lib` | Matches the existing lane; Go/Python/TS wrap later. |

## Why Mirroring

Map an `N`-byte region twice, back to back, at `base` and `base + N`. For any
offset `o < N` and length `l <= N`, the bytes `[base + o, base + o + l)` are
valid and contiguous even when `o + l > N`.

```
   primary mapping            mirror mapping
   +-----------------------+-----------------------+
   | 0 1 2 .. 12 13 14 15  | 0 1 2 .. 12 13 14 15  |
   +-----------------------+-----------------------+
                  \--------+------/
                  one contiguous 6-byte span at base+13
                   ^ physically the same pages ^

   plain ring would need:  [13,14,15] + [0,1,2]  -> two spans, or a pad record
```

`N` must be a multiple of the page size (2 MiB if hugepages are wanted) and a
power of two so `pos & (N - 1)` masks.

The payoff is specific to **variable-length** records: no padding records, no
bipartite split, no "record does not fit before the end, skip to zero" logic.
`reserve(n)` always returns one `span`, and `peek()` always returns one `span`.
That single property is the entire justification for the mapping complexity. If
it is not worth it for the eventual call sites, do not build this.

### Aliasing rule

Two virtual addresses now alias each page.

```
   payload bytes  ->  either alias is fine   (memcpy through base + (pos & mask))
   control words  ->  canonical alias ONLY   (all atomics, no exceptions)
```

Aliased atomics do work on mainstream hardware, but relying on it defeats any
ability to reason about the code. Treat the restriction as absolute.

## Topology

```
  1000 producers                 K=64 shards               1 consumer
  --------------                 -----------               ----------

  P0   -+
  P1    +--hash--> +----------+
  ...   |          | shard 0  |--+
  P15  -+          +----------+  |
                                 |
  P16  -+          +----------+  |     +-----------------+
  ...   +--hash--> | shard 1  |--+---->|  sweep all K    |--> application
  P31  -+          +----------+  |     |  (prefetched)   |
                                 |     +-----------------+
  ...              ...           |
                                 |
  P984 -+          +----------+  |
  ...   +--hash--> |shard K-1 |--+
  P999 -+          +----------+

  producer-side contention: ~16 producers/shard   (cheap CAS)
  consumer-side scan:       64 cursors            (4 KB, prefetchable)
```

`K` is the count of independent rings. **`K` is tuned to the consumer's sweep
budget, not to the producer count.** `K = P` for small `P` degenerates to pure
per-producer SPSC with zero producer contention; `K = 1` degenerates to a single
shared ticket ring. `K = 64` is the proposed default for the large-`P` regime.

Sizing rule: choose `K` so the expected records-per-shard-per-sweep is at least
about 8, which is where the fixed per-shard cache misses stop mattering:

```
   K  <=  arrival_rate * target_sweep_latency / 8
```

That number should be measured before the recovery machinery in
[Crash containment](#crash-containment) is written, because it determines
whether the reaper is needed at all.

## Backing Store And Mapping

One `memfd`. Control region mapped once; every shard arena mapped twice,
adjacently.

```
  memfd file offsets                        virtual address space
  ------------------                        ---------------------

  0    +-------------+                      +-------------+  CTL
       |  control    | ---- mmap x1 ------> |  control    |
  C    +-------------+                      +-------------+
       |             |                      +-------------+  base+0
       |  shard 0    | --+-- mmap --------> |  shard 0    |
       |  (N bytes)  |   |                  +-------------+  base+N
  C+N  +-------------+   +-- mmap --------> |  shard 0'   |  <- same pages
       |             |                      +-------------+  base+2N
       |  shard 1    | --+-- mmap --------> |  shard 1    |
       |  (N bytes)  |   |                  +-------------+  base+3N
  C+2N +-------------+   +-- mmap --------> |  shard 1'   |
       |     ...     |                      +-------------+
```

Both mappings of a shard are `MAP_FIXED` into one pre-reserved
`PROT_NONE | MAP_ANONYMOUS | MAP_NORESERVE` hole of `2 * K * N`. The reservation
is what makes the pair race-free: without it another thread's unrelated `mmap`
can land in the gap between the two `MAP_FIXED` calls.

Mapping is **lazy** — a shard is mapped on first use, so the small-`P`
configuration never pays for the large one. Two VMAs per shard; adjacent VMAs
with differing file offsets do not merge. At `K = 1000` that is 2000 VMAs,
comfortably under the 65530 `max_map_count` default.

Seals are applied before any descriptor is shared:

```
   F_SEAL_SHRINK   <- without this, a crashing peer's ftruncate
   F_SEAL_GROW        gives the consumer a SIGBUS
   F_SEAL_SEAL
```

`F_SEAL_SHRINK` matters even inside the trust boundary: the failure mode is a
*crashing* peer, not a hostile one, and it is the most common way a shared-memory
queue dies in production.

Rendezvous: pass the descriptor over a unix socket with `SCM_RIGHTS`. A
`shm_open` + `shm_unlink` backend behind the same interface covers named
discovery and non-Linux portability, at the cost of losing seals.

Non-Linux, if it ever matters: macOS needs `mach_vm_remap` or
`shm_open`; Windows needs `VirtualAlloc2` placeholder splitting plus
`MapViewOfFile3`, with 64 KiB allocation granularity. **Recommendation: make
mirroring a hard construction requirement and refuse to construct where it is
unavailable.** A non-mirrored fallback would force the consumer API to return an
iovec pair instead of a single span, and that change propagates into every call
site.

## Layout And Framing

### Shard control block

```
   byte
   offset
   +-------------------------------------------------+
 0 | tail  (u64)          | ....... pad ............ |  written by producers
   +-------------------------------------------------+  <- line boundary
64 | head  (u64)          | ....... pad ............ |  written by consumer ONLY
   +-------------------------------------------------+  <- line boundary
128| cached_head, owner_pid, generation, ...         |  cold
   +-------------------------------------------------+
```

Pad to 128 bytes, not 64: x86 adjacent-line prefetch and Apple M-series both
pull the sibling line.

### Global control page

```
   +-------------------------------------------------+
   | magic | version | N | K | max_record | ...       |  read-only after init
   +-------------------------------------------------+
   | consumer_state (u32) | ....... pad ............ |  READ-MOSTLY
   +-------------------------------------------------+   ^ this line sits in
                                                          S state on all cores
```

Attach must verify magic and version and refuse on mismatch. The read-mostly
property of `consumer_state` is load-bearing; see [Sleep protocol](#sleep-protocol).

### Record framing

```
   +--------------------------------+----------------------+
   | header (u64)                   | payload (len bytes)  |
   +--------------------------------+----------------------+
     |
     +- bits  0..31  len
        bits 32..62  lap        published iff lap == expected(pos)
        bit     63   kind       data | abandoned

   publish  = single release store of the whole word
   consume  = single acquire load of the whole word, then read payload
```

Records aligned to 8 or 16 bytes.

The rejected alternative is a **global commit counter**, where each producer
spins until `commit == my_start` before advancing it. It makes the consumer
trivial but serializes producers at commit time, reintroducing exactly the
head-of-line blocking the design is trying to avoid. The per-record lap flag is
strictly better for MPSC.

### Memory ordering

```
   reserve            relaxed RMW
   publish header     release
   consumer header    acquire
   consumer head      release
   producer's read of cached head   acquire
```

## Ordering Decision

Global FIFO across producers is **given up**. The reasoning:

- A shared cursor is a single cache line arbitrating RMW traffic from up to 1000
  cores. Producer latency degrades roughly linearly in contenders; at `P = 1000`
  it is a retry storm.
- Under a shared cursor, a producer killed between reserve and commit wedges the
  consumer behind a hole, and recovery must distinguish "slow" from "dead". That
  is the least-tested code in the system, on the critical path of every message.

If cross-producer ordering is later needed, recover it with a **per-record
`rdtsc` stamp**, not a sequence number. A shared monotonic counter would
reintroduce precisely the contended atomic being eliminated; invariant TSC is
coherent across cores on any plausible deployment target and costs ~20 cycles.

**Honest cost:** capacity is partitioned. Memory is `K * N`, and one bursting
producer cannot borrow idle capacity from the other shards. *If the real load
shape is "one producer occasionally floods", this design is wrong and should be
revisited before implementation starts.*

## Producer Hot Path

```
   +------------------------------------------------+
   | 1. shard = hash(tid) & (K-1)                   |  no shared state
   +------------------------------------------------+
   | 2. CAS shard.tail  (~16 contenders - cheap)    |  own shard's line
   +------------------------------------------------+
   | 3. memcpy payload  -> base + (pos & mask)      |  mirrored, contiguous
   +------------------------------------------------+
   | 4. release-store header (publish)              |
   +------------------------------------------------+
   | 5. lock xchg shard.tail                        |  own line, uncontended
   |      ^ doubles as the seq_cst fence for step 6 |  <- the trick
   +------------------------------------------------+
   | 6. load consumer_state (relaxed)               |  shared, but READ-only
   |      if SLEEPING -> futex_wake                 |  almost never taken
   +------------------------------------------------+

   shared-line traffic per enqueue:  one load of a clean line.  Not an RMW.
```

Step 5 exists because step 6 needs a full barrier (see
[Sleep protocol](#sleep-protocol)) and a standalone `mfence` costs 20-30 cycles.
Publishing the shard's own tail with `lock xchg` supplies the barrier as a side
effect, on an uncontended line the producer owns.

The barrier **cannot** be elided conditionally. Checking "was this shard already
non-empty, so the consumer cannot be asleep?" races with the consumer draining
the shard concurrently, so the elision is unsound.

## Consumer Sweep

There is no non-empty bitmap. It was an artifact of `K = P` and is actively
harmful once `K` is small:

- A dense bitmap (1000 bits = 2 lines) makes every producer RMW a shared line on
  each empty-to-non-empty edge. That is the same contention the sharding removed,
  relocated. Making the bit-set *relaxed* does not help — memory ordering and
  coherence traffic are independent, and a relaxed `fetch_or` still acquires the
  line exclusively.
- A cache-aligned flag per producer fixes the producer side but forces the
  consumer to read 1000 lines (64 KiB) to learn what is active, which is the
  exact cost the bitmap existed to avoid.

With `K = 64` the scan is cheap enough to do unconditionally:

```
   for round = 0,1,2,...
   +----------------------------------------------------------+
   |  prefetcht0 &shard[i+8].tail  for i..i+8   (run ahead)    |
   |                                                            |
   |  shard:   0    1    2    3    4    5   ...  63             |
   |  tail:   +--+ +--+ +--+ +--+ +--+ +--+      +--+          |
   |          |##| |  | |##| |##| |  | |##|  ..  |  |          |
   |          +--+ +--+ +--+ +--+ +--+ +--+      +--+          |
   |           |         |    |         |                      |
   |         drain     drain drain    drain   (batch each)     |
   |                                                            |
   |  start index rotates each round -> no starvation of high i |
   +----------------------------------------------------------+

   64 independent, statically-known misses
        serialized : 64 * 200 ns  ~= 13 us
        16-deep MLP: ~4 round trips ~= 800 ns    <- prefetch is load-bearing
```

The 64 addresses are known *before* any of them is touched. That is what
separates this sweep from pointer chasing, and it is why an unconditional scan
beats a hint structure that would cost every producer an exclusive-state RMW.

Rotating the start index each round is not optional: a fixed-order scan starves
high-index shards under sustained overload.

### Known weakness

Compared with a single shared ring, this scatters records across `K` buffers, so
the consumer loses sequential prefetch locality — roughly 2 misses per shard
visit versus about 0.2 misses per record when streaming one contiguous buffer.
Three things bound the damage, and one does not:

1. Prefetching converts serialized misses to parallel ones (above).
2. It self-amortizes: when the consumer falls behind, shard backlogs deepen and
   the fixed per-shard cost spreads over larger batches. The design degrades
   *into* its efficient regime.
3. `K` is decoupled from `P`, so the scan never grows past the configured bound.
4. **Not bounded:** the band where every shard holds one or two records while the
   consumer is keeping up. There the fixed cost is not amortized. It is a narrow
   band, but it is real, and it is where latency is most visible.

Also note that at `P = 1000` with a single consumer, aggregate throughput is
capped by one core's drain rate regardless of producer-side design. If that
ceiling is the binding constraint, the answer is multiple consumers, which is a
different data structure than the one specified here. Benchmark early enough to
find out.

## Sleep Protocol

Consumer state machine:

```
   +------+  empty xn  +-------+  empty xm  +----------+
   | SPIN | ---------> | YIELD | ---------> | SLEEPING |
   +------+            +-------+            +----------+
      ^                                          |
      +------------- futex_wake / work ----------+

   SLEEPING is rare under load, so the consumer_state line stays clean & shared
```

Any "non-empty" hint is a **hint only, never authoritative**. The consumer's
termination condition is always a real scan of the cursors, so a lost hint costs
latency and never a record. This is affordable precisely because the
unconditional scan is cheap.

The sleeping consumer is the one case where a lost signal is a hang rather than a
delay, so it gets a Dekker interlock:

```
        PRODUCER                         CONSUMER
        --------                         --------

   (1)  store tail  (release)      (A)  store state = SLEEPING  (seq_cst)
        --- full barrier ---            --- full barrier ---
   (2)  load  state (seq_cst)      (B)  load all K tails
        if SLEEPING -> wake             if all empty -> futex_wait


   Bad outcome = (2) reads NOT_SLEEPING  AND  (B) reads empty
                        |                          |
                        |                          +- B must precede 1
                        +- 2 must precede A            in the total order
                                              in the total order

   program order:  1 -> 2      and      A -> B

   assemble:   1 -> 2 -> A -> B -> 1        <- cycle
                                            +--- contradiction

   Therefore at least one of {consumer sees the data, producer sends the wake}
   always holds.  The wakeup cannot be lost.
```

Note this is why no amount of atomicity on a flag fixes the problem: the hazard
is the *gap between two operations*, not the operations themselves.

For a consumer living in an `epoll` loop, substitute an eventfd for the futex and
signal only on the empty-to-non-empty edge. Make the notification a policy
parameter rather than a build-time fork.

## Crash Containment

```
   producer killed between reserve and commit:

   shard 7:  [rec][rec][RESERVED, no header]  [ ... ]
                            ^
                    consumer stops here

   blast radius = shard 7 only  (1/64 of throughput)
   shards 0..6, 8..63 keep draining normally

   reaper (periodic, off the hot path):
        owner dead (kill(pid,0) == ESRCH)  AND  age > T
              |
        stamp header kind=ABANDONED, len=reserved_len
              |
        consumer skips it, shard resumes
```

This is the concession sharding costs. At `K = P` a dead producer needs no
recovery at all — its ring simply idles and the consumer drains what was
committed. At `K = 64` the reaper is required: real code, with a real timeout
constant that must distinguish a descheduled producer from a dead one.

Slot metadata carries `{owner_pid, owner_tid, generation, state}`. Generation is
bumped on reclamation so a stale producer cannot resurrect into a reused slot.

## API Shape

```cpp
// producer, zero-copy
std::span<std::byte> reserve(size_t n);   // contiguous, thanks to mirroring
void commit(size_t actual_n);             // may be < n

// producer, copy
bool push(std::span<const std::byte>);

// consumer
std::span<const std::byte> peek();        // contiguous
void pop();
size_t peek_batch(std::span<span> out);   // optional
```

The gap between `reserve` and `commit` is user code of unbounded duration, and it
is what makes the abandoned-record path necessary. Under the cooperative
trust model both forms are acceptable. Were the trust model ever to change to
hostile peers, restrict the cross-process boundary to `push`, which narrows the
window to a memcpy.

## Build Order

1. **Mutex-guarded ring** — a futex-backed lock around a single mirrored ring.
   Not the shipping design; it exists as the differential-test oracle and the
   correctness reference. Cheap to write and immediately useful.
2. **Single mirrored SPSC ring** — the whole primitive, standalone: mapping,
   framing, reserve/commit/peek/pop. Most of the risk lives here.
3. **Shard registration and the multiplexing consumer** — `K` rings, rotating
   prefetched sweep, hash assignment.
4. **Sleep protocol** — Dekker interlock, adaptive spin, futex and eventfd
   policies.
5. **Reaper** — only if step 3's benchmarks confirm `K < P`.

## Verification Plan

This is a class of code where "it passes" means nothing without deliberate
adversarial testing.

- **ThreadSanitizer** over a randomized producer mix, both regimes of `P`.
- **Differential harness** against the step-1 mutex oracle: identical input
  streams, assert identical per-shard output sequences.
- **Fault injection**: `SIGSTOP` a producer mid-`reserve`, assert the other
  `K - 1` shards keep flowing, then assert the reaper recovers the stalled shard.
- **SIGBUS**: verify the seals actually prevent a peer's `ftruncate` from
  killing the consumer.
- **Litmus test for the sleep protocol** — the Dekker argument above is small
  enough to model-check (CDSChecker or a hand-written harness). Do it; the
  failure mode is a rare hang, which is the worst kind to debug in production.
- **Benchmarks that decide open questions**, not just report numbers: the `K`
  sizing rule, and whether the single consumer is the binding throughput
  constraint.

## Open Questions

1. **Is `K < P` actually needed?** If the real producer count sits at the low end,
   `K = P` removes the reaper, the abandoned-record path, and the hash
   assignment. Measure before building step 5.
2. **Load shape.** Partitioned capacity is wrong for bursty single-producer
   traffic. Confirm the shape before committing.
3. **Is one consumer sufficient** at the top of the producer range, or is the
   real requirement MPMC?
4. **Does anything need cross-producer ordering?** If not, drop the `rdtsc`
   stamp and the 8 bytes per record it costs.
5. **Non-Linux support** — currently proposed as "refuse to construct". Confirm
   nothing needs macOS or Windows.
