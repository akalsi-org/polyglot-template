#pragma once

// The contract every queue variant implements.
//
// Two ring algorithms x two topologies. Sharding is orthogonal to the ring, so
// the multiplexer is written once and composes with either:
//
//                 standalone                 sharded (K rings, one file)
//   Ring          Mpsc                       ShardedMpsc
//   SpscRing      Spsc                       MultiSpsc
//
//   Ring       many writers on one arena. Total order across all writers.
//              Claim-by-CAS-on-descriptor, writer-stamps-successor, reader-only
//              recovery. Strongest ordering, lowest claim throughput.
//
//   SpscRing   ONE writer on one arena. No claim arbitration: reserve is a plain
//              store, so it is WAIT-FREE rather than lock-free. The tail is a
//              publication cursor, so the reader never observes an incomplete
//              record and there is NO recovery protocol at all -- a dead writer's
//              ring simply idles. Fastest, weakest ordering.
//
//   Sharded<R> K rings of either type in one file and one mapping, with writer
//              routing and a rotating reader sweep.
//
// Ordering is the axis they differ on, and it is a property of the variant rather
// than of the API. Nothing here promises a total order; consult the variant.
//
// SHARD ASSIGNMENT IS STICKY. A writer is bound to one shard at registration and
// stays there for its lifetime. This is a guarantee, not an implementation
// detail: it is what makes PER-WRITER FIFO hold in the sharded variants, since
// all of a writer's records land in one ring and are therefore ordered with
// respect to each other.
//
// Do not add shard migration for load balancing. Moving a live writer splits its
// stream across two rings, and the reader's sweep can then deliver its later
// records before its earlier ones -- a silent reordering that no caller can
// detect. (Migration is only safe if cross-shard order is reconstructed
// downstream, e.g. from per-record timestamps, which this API does not do.)
// Placement quality is a registration-time concern; see Config::shards.
//
// REGISTRATION ASYMMETRY, which is a real difference callers must handle:
//
//   Mpsc         no registration. Any thread may write. attachWriter() cannot fail.
//   Spsc         registration is EXCLUSIVE and ENFORCES the single-writer
//                contract: a second thread's attachWriter() FAILS rather than
//                silently creating a second writer. maxWriters() == 1.
//   ShardedMpsc  registration ASSIGNS a shard, many writers to one. Cannot fail.
//   MultiSpsc    registration takes EXCLUSIVE ownership of a ring, one writer to
//                one shard -- so it FAILS once every shard is claimed.
//
// Spsc enforcing rather than merely documenting its contract is deliberate. Two
// writers on an SPSC ring is the one misuse that silently corrupts -- the whole
// algorithm (no claim CAS, publication-cursor semantics, no recovery path) rests
// on sole ownership. A cold-path slot claim turns that into a detected failure
// for free, and keeps the same code correct standalone and under Sharded.
//
// maxWriters() bounds LIVE writers, not attach() successes over a lifetime. A
// writer that exits releases its ring: once its owner is provably dead AND the
// ring has drained, the slot is recycled and a new writer may claim it. So a
// MultiSpsc with K=2 will happily admit a third, fourth, ... writer over time as
// earlier ones exit -- it will just never have more than two live at once. Both
// gates are required before reuse; recycling a slot whose ring still holds
// undelivered records would reintroduce cross-writer byte reuse, which is the
// one thing SpscRing's correctness argument forbids.
//
// Only MultiSpsc has a writer capacity, and it equals the shard count. Query it
// with maxWriters(): kUnboundedWriters for the others. Generic code that must
// handle both should branch on that rather than discovering kNoSlot at runtime.
//
// CHOOSING ONE. Measured, Zen 5, 56 B records, push latency in ns:
//
//                          Mrec/s    p50     p95     p99     p99.99
//   Spsc        w=1          64.2     21       -       -          -
//   Mpsc        w=1          53.7     32      74      96        298
//   Mpsc        w=8           2.3   1722   16316   28912      66945
//   ShardedMpsc w=8, K=8     29.6    191     468    6144      44633
//   MultiSpsc   w=8          70.6     53     276    1159      18347
//
//   Total order across ALL writers      -> Mpsc. Excellent to 2 writers,
//                                          collapses beyond; shard past that.
//   Many writers, per-shard order OK    -> ShardedMpsc, K >= writers.
//   Per-writer FIFO is enough           -> MultiSpsc. Fastest at every
//                                          percentile; writer count capped at K.
//   Single producer                     -> Spsc.
//
// A total order across all writers costs ~30x under contention (Mpsc vs
// MultiSpsc at w=8); per-SHARD order costs ~2.4x over per-writer order at the
// median. Ordering is expensive globally and cheap locally -- the argument for
// sharding rather than abandoning order.
//
// BUT SHARDING FIXES THE MEDIAN, NOT THE TAIL. ShardedMpsc improves Mpsc's p50
// by 9x while its p99.99 (45 us) converges back toward Mpsc's (67 us), and stays
// 2.4x worse than MultiSpsc's (18 us). Head-of-line blocking is a PER-RING
// property: more shards means fewer writers stuck behind any one stall, but a
// writer stalled in its own reserve-to-commit window still pins its ring's
// reader progress no matter how many other rings exist. If you are choosing
// ShardedMpsc for tail latency rather than for ordering, that is the wrong
// reason -- take MultiSpsc.
//
// (Maxima are omitted deliberately: on the measurement box every run tripped
// the >8x-p99 OS-jitter check, so observed maxima were scheduler preemption,
// not queue behaviour. They need bare metal to mean anything.)
//
// The API is deliberately two-phase. A single-shot write() cannot express a short
// commit -- the caller must be able to reserve an upper bound, discover the actual
// length while filling, and commit less.
//
// LIFETIME. The span from reserve() is valid only until commit() or abort(); the
// span from peek() only until pop(). The reader may overwrite a record's bytes
// once it has passed them, and read-position publication is what licenses that.
//
// HEAD-OF-LINE. In the ordered variants the reserve-to-commit window pins the
// reader: it cannot pass an uncommitted record, so consumption halts and
// admission continues only until the ring fills behind it. User-code duration
// between reserve and commit therefore bounds queue availability for every
// participant. This is a contract on callers, not something the queue can defend
// against -- the no-timeout rule means a slow writer and a hung writer are
// deliberately indistinguishable.
//
// A corollary for SINGLE-THREADED writer+reader use: recovery and busy-record
// progress happen only inside peek(). A thread that is both the writer and the
// reader can therefore wedge itself -- if its write path blocks on the ring
// (e.g. behind a dead writer's record awaiting recovery, or behind capacity
// that only draining frees), the peek() that would unblock it never runs.
// Such a caller must interleave peek()/pop() with its writes and must not spin
// in reserve()/write() retry loops.

#include "pgt/core/types.hh"
#include "pgt/mpsc/desc.hh"

#include <cstddef>
#include <span>

namespace pgt::mpsc {

enum class Backend {
  // Anonymous, sealable, passed by fd. The default. Only backend that supports
  // F_SEAL_SHRINK, which is what stops a peer's ftruncate from SIGBUS-ing the
  // reader -- the most common way a shared-memory queue dies in production.
  kMemfd,
  // Named under /dev/shm. Loses seals, gains a name a successor can open
  // unaided: the one thing that makes reader restart work without depending on
  // a surviving writer or a supervisor holding the fd.
  kShm,
  // Explicit path. Almost always wrong for a queue on a real filesystem --
  // MAP_SHARED means continuous writeback for data nobody intends to persist.
  // Supported so a crashed ring can be inspected post-mortem.
  kFile,
};

struct Config {
  // Bytes of ring per shard. Rounded up to a power of two and a multiple of the
  // page size.
  //
  // MEASURED: capacity does NOT affect push latency. Push cost is flat (~70ns on
  // Zen 5) from 256 KiB to 256 MiB, because while the reader keeps pace the hot
  // working set is the moving frontier window -- a few KiB -- which is cache
  // resident at any ring size. The ring as a whole is never the working set.
  //
  // What capacity buys is BACKLOG TOLERANCE: how far the reader may fall behind
  // before writers see backpressure. A cold backlog then streams out at DRAM
  // bandwidth (~28 GB/s measured), so a large ring costs nothing until it is
  // actually used as one.
  //
  // Padding interacts here rather than with throughput: at 64-byte grain a 64 B
  // payload occupies a 128 B extent, so padding waste is a tax on effective
  // capacity -- and therefore on the full-rate under backlog -- not a
  // throughput sawtooth. Records sized 64k-8 (56, 120, 184, ...) pack exactly.
  sz_t capacity = 1u << 20;

  // Shard count. Ignored by the standalone variants (always 1). For MultiSpsc
  // this is the maximum number of registered writers.
  //
  // For ShardedMpsc, tune to WRITERS PER SHARD (wps). A shared Ring does not
  // plateau under contention, it COLLAPSES -- a claim-CAS loser restarts its
  // walk over hot lines, and the causal chain (successor stamp, vouch, hint)
  // adds three serialized exclusive line acquisitions:
  //
  //   one Ring, writers   1      2      4      8      16
  //   Mrec/s              53.7   ~19    5.0    2.3    1.6
  //
  // Sharding recovers it, and the curve never flattens and never regresses --
  // 16 writers spread over K shards, measured:
  //
  //   K (wps)   1 (16)   2 (8)   4 (4)   8 (2)   16 (1)
  //   Mrec/s    1.5      4.0     7.3     17.8    35.7
  //
  // Roughly 2x per halving of wps, all the way down. So: **wps = 1 is optimal**;
  // wps = 2 is the reasonable compromise at half the memory; every doubling
  // beyond that costs about half the aggregate. (Two earlier estimates here --
  // 8-16 and then 2-4 writers per shard -- were both too conservative.)
  //
  // Shard count itself is nearly free for the reader: sparse-traffic delivery
  // p50 measured 84 / 86 / 117 ns at K = 8 / 32 / 128, so the sweep costs ~33ns
  // going from 8 to 128 shards. The reader drains at ~450 Mrec/s
  // (DRAM-bandwidth-bound) and is never the constraint. Spend shards freely;
  // memory for the arenas is the only real limit.
  u32_t shards = 1;

  Backend backend = Backend::kMemfd;

  // For kShm (a name) or kFile (a path).
  char const* name = nullptr;

  // Preallocate at construction so no first-touch fault can SIGBUS under memory
  // pressure or allocate on the hot path. Turning this off makes the queue no
  // longer allocation-free.
  bool preallocate = true;
};

// Returned by reserve(): empty when the queue cannot accept the record.
using WriteSpan = std::span<std::byte>;
using ReadSpan = std::span<std::byte const>;

enum class Status : u8_t {
  kOk = 0,
  kFull,          // backpressure; retry later
  kReaderDead,    // no reader will ever drain: a definite error, not backpressure
  kTooLarge,      // exceeds what this configuration can ever hold
  kNoSlot,        // writer registration failed (MultiSpsc / Sharded slot table)
};

// Writer capacity of a variant with no registration limit.
inline constexpr u32_t kUnboundedWriters = 0xffffffffu;

// clang-format off
template <typename Q>
concept QueueLike = requires(Q q, void const* p, sz_t n, u32_t slot) {
  // Writer capacity. kUnboundedWriters unless registration is exclusive.
  { q.maxWriters() } -> std::same_as<u32_t>;

  // Writer. reserve() returns an empty span on failure; status() reports why.
  { q.reserve(n) }        -> std::same_as<WriteSpan>;
  { q.commit(n) }         -> std::same_as<void>;
  { q.abort() }           -> std::same_as<void>;
  { q.write(p, n) }       -> std::same_as<bool>;
  { q.status() }          -> std::same_as<Status>;

  // Reader. peek() returns an empty span when nothing is deliverable yet.
  { q.peek() }            -> std::same_as<ReadSpan>;
  { q.pop() }             -> std::same_as<void>;

  // Attachment. Both are cold paths. attachReader() enforces exactly one reader
  // and, where the variant supports restart, takes over from a proven-dead
  // predecessor -- resuming exactly at its published read position, since the
  // reader keeps no state that is not in the control page.
  { q.attachWriter() }    -> std::same_as<bool>;
  { q.attachReader() }    -> std::same_as<bool>;
  { q.detachWriter() }    -> std::same_as<void>;
};
// clang-format on

}  // namespace pgt::mpsc
