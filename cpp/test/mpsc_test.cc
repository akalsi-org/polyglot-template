// In-process tests for the mpsc queue variants and the mirrored Region.
//
// Structure notes:
//  * Queue-behaviour tests are TEST_CASE_TEMPLATE over the QueueLike types so
//    they re-point at SpscRing / Sharded by appending to the type list. They
//    assert only what every variant promises: per-writer FIFO, integrity, and
//    completeness -- never cross-writer total order.
//  * Every queue test uses a SMALL capacity and pushes several capacities of
//    data, so the ring wraps repeatedly. A test that never wraps cannot see the
//    defect class FREE-carries-position exists to prevent.
//  * The differential test drives the queue and a trivially-correct
//    mutex-guarded reference with the same stream and compares delivered bytes
//    exactly. It is the highest-value test here: framing, boundary, and
//    ordering errors all land in one assertion.
//
// Fork-based fault injection (killed/stopped writers, misuse death tests) is in
// mpsc_fault_test.cc, kept separate so this file stays runnable under TSan.
// TSan: run with TSAN_OPTIONS="suppressions=cpp/test/tsan.supp" -- the file
// documents the one accepted race class (stale walker probes vs plain payload
// bytes, inherent to in-band descriptors) and the recorded cheap fix.

#include "pgt/core/platform.hh"
#include "pgt/mpsc/desc.hh"
#include "pgt/mpsc/queue.hh"
#include "pgt/mpsc/region.hh"

#include <doctest/doctest.h>

#include <pthread.h>
#include <sched.h>
#include <sys/mman.h>
#include <unistd.h>

#include <atomic>
#include <chrono>
#include <cstring>
#include <deque>
#include <mutex>
#include <random>
#include <thread>
#include <vector>

namespace {

using namespace pgt;
using namespace pgt::mpsc;

// Trivially-correct mutex-guarded variable-length queue. No cleverness on
// purpose: its only job is to be obviously right.
class ReferenceQueue {
 public:
  void push(std::vector<std::byte> v) {
    std::lock_guard<std::mutex> g(m_);
    q_.push_back(std::move(v));
  }
  bool pop(std::vector<std::byte>& out) {
    std::lock_guard<std::mutex> g(m_);
    if (q_.empty()) return false;
    out = std::move(q_.front());
    q_.pop_front();
    return true;
  }
  [[nodiscard]] bool empty() {
    std::lock_guard<std::mutex> g(m_);
    return q_.empty();
  }

 private:
  std::mutex m_;
  std::deque<std::vector<std::byte>> q_;
};

// Policy handle with external counters (policies are copied; state must live
// outside -- see policy.hh).
struct CountingPolicy : DefaultPolicy {
  std::atomic<u64_t>* reclaims = nullptr;
  std::atomic<u64_t>* wraps = nullptr;
  std::atomic<u64_t>* contended = nullptr;
  void onReclaim(u64_t, u32_t, u64_t) noexcept {
    if (reclaims != nullptr) reclaims->fetch_add(1, std::memory_order_relaxed);
  }
  void onWrap(u64_t) noexcept {
    if (wraps != nullptr) wraps->fetch_add(1, std::memory_order_relaxed);
  }
  void onContended(u32_t) noexcept {
    if (contended != nullptr) contended->fetch_add(1, std::memory_order_relaxed);
    cpuRelax();
  }
  // Yield on busy rather than pure spin: peek() consults /proc for EVERY
  // in-flight encounter, so a spinning reader in a preemption-heavy regime
  // goes syscall-bound (measured: ~450 records/s, 36s of sys time) -- and the
  // yield is also what hands a preempted mid-window writer its core back.
  void onBusy(u32_t) noexcept { std::this_thread::yield(); }
};

// NO-PROGRESS deadline, not an absolute budget: reset() on every delivered
// record. A wedged queue still fails within one window; a slow-but-progressing
// drain does not. The distinction matters on shared/CI boxes -- an absolute
// budget made the oversubscribed regime flake purely under external load,
// presenting as "passes standalone, fails in-suite".
struct Deadline {
  static constexpr std::chrono::seconds kWindow{60};
  std::chrono::steady_clock::time_point end = std::chrono::steady_clock::now() + kWindow;
  void reset() { end = std::chrono::steady_clock::now() + kWindow; }
  [[nodiscard]] bool expired() const { return std::chrono::steady_clock::now() > end; }
};

std::byte patternByte(u32_t writer, u32_t seq, sz_t i) {
  return static_cast<std::byte>(writer * 151u + seq * 29u + static_cast<u32_t>(i) * 7u + 3u);
}

Config smallConfig() {
  Config cfg;
  cfg.capacity = 4096;  // one page: wraps every ~63 minimal records
  return cfg;
}

// ---------------------------------------------------------------------------
// Differential against the reference, single writer, exact byte-stream compare.
// ---------------------------------------------------------------------------
TEST_CASE_TEMPLATE("differential vs reference queue with short commits", Q, Mpsc) {
  Q q;  // stack-constructed, as a caller would: the epoch-identity regression
        // (mpsc_fault_test.cc) lived exactly in the recreate-at-same-address path
  REQUIRE(Q::create(smallConfig(), q));
  REQUIRE(q.attachReader());
  ReferenceQueue ref;

  // Boundary sizes stated in the plan plus a random spread. 56 = kGrain - 8
  // packs perfectly; 57 forces a 2-grain extent with a 1-byte spill.
  sz_t const special[] = {0, 1, kGrain - 8, kGrain - 7, kGrain - 1, kGrain, 2 * kGrain - 8, 300};
  std::mt19937 rng(0xC0FFEE);
  u64_t pushed_bytes = 0;
  u64_t byte_seed = 1;

  auto drain_compare = [&] {
    for (;;) {
      ReadSpan const r = q.peek();
      if (r.data() == nullptr) break;
      std::vector<std::byte> want;
      REQUIRE_MESSAGE(ref.pop(want), "queue delivered a record the reference does not have");
      REQUIRE(r.size() == want.size());
      REQUIRE(std::memcmp(r.data(), want.data(), want.size()) == 0);
      q.pop();
    }
  };

  for (int round = 0; round < 400; ++round) {
    int const records = 1 + static_cast<int>(rng() % 8);
    for (int k = 0; k < records; ++k) {
      sz_t const nmax = (rng() % 4 == 0) ? special[rng() % std::size(special)] : rng() % 300;
      // Short commit roughly half the time nmax allows one.
      sz_t const m = (rng() % 2 == 0) ? nmax : static_cast<sz_t>(rng() % (nmax + 1));
      WriteSpan s = q.reserve(nmax);
      if (s.data() == nullptr) {
        REQUIRE(q.status() == Status::kFull);
        drain_compare();
        s = q.reserve(nmax);
        REQUIRE(s.data() != nullptr);
      }
      std::vector<std::byte> bytes(m);
      for (sz_t i = 0; i < m; ++i) {
        byte_seed = byte_seed * 6364136223846793005ull + 1442695040888963407ull;
        bytes[i] = static_cast<std::byte>(byte_seed >> 56);
      }
      std::memcpy(s.data(), bytes.data(), m);
      q.commit(m);
      pushed_bytes += extentFor(m);
      ref.push(std::move(bytes));
    }
    drain_compare();
  }
  drain_compare();
  CHECK(ref.empty());
  // The point of the small capacity: this must have wrapped many times.
  CHECK(pushed_bytes > 8 * 4096);
}

// ---------------------------------------------------------------------------
// Many writers, randomized sizes, aborts injected. Asserts what every variant
// promises: per-writer FIFO order, payload integrity, nothing lost, nothing
// duplicated -- across many wraps.
// ---------------------------------------------------------------------------
TEST_CASE_TEMPLATE("multi-writer randomized stress with aborts", Q, Mpsc) {
  constexpr u32_t kWriters = 4;
  constexpr u32_t kRecords = 8000;  // per writer; ~2000 laps of a 4 KiB ring

  Q q;
  REQUIRE(Q::create(smallConfig(), q));
  REQUIRE(q.attachReader());

  std::atomic<bool> failed{false};
  std::vector<std::thread> writers;
  writers.reserve(kWriters);
  for (u32_t w = 0; w < kWriters; ++w) {
    writers.emplace_back([&q, &failed, w] {
      REQUIRE(q.attachWriter());
      std::mt19937 rng(w * 7919u + 17u);
      std::byte buf[512];
      for (u32_t seq = 0; seq < kRecords && !failed.load(std::memory_order_relaxed); ++seq) {
        sz_t const n = 8 + rng() % 200;
        std::memcpy(buf, &w, 4);
        std::memcpy(buf + 4, &seq, 4);
        for (sz_t i = 8; i < n; ++i) buf[i] = patternByte(w, seq, i);
        // Occasionally reserve-and-abort: aborted records must be invisible.
        if (seq % 37 == 5) {
          WriteSpan const s = q.reserve(16);
          if (s.data() != nullptr) q.abort();
        }
        while (!q.write(buf, n)) {
          if (q.status() != Status::kFull) {
            failed.store(true, std::memory_order_relaxed);
            return;
          }
          std::this_thread::yield();
        }
      }
      q.detachWriter();
    });
  }

  u32_t next_seq[kWriters] = {};
  u64_t delivered = 0;
  u64_t bad = 0;
  Deadline dl;
  while (delivered < static_cast<u64_t>(kWriters) * kRecords) {
    ReadSpan const r = q.peek();
    if (r.data() == nullptr) {
      REQUIRE_MESSAGE(!dl.expired(), "reader made no progress for 60s");
      continue;
    }
    u32_t w = ~0u;
    u32_t seq = ~0u;
    if (r.size() < 8) {
      ++bad;
    } else {
      std::memcpy(&w, r.data(), 4);
      std::memcpy(&seq, r.data() + 4, 4);
      if (w >= kWriters || seq != next_seq[w]) {
        ++bad;
      } else {
        ++next_seq[w];
        for (sz_t i = 8; i < r.size(); ++i) {
          if (r[i] != patternByte(w, seq, i)) {
            ++bad;
            break;
          }
        }
      }
    }
    q.pop();
    ++delivered;
    dl.reset();  // progress-based, same rationale as contentionRun
    REQUIRE_MESSAGE(bad == 0, "record ", delivered, " writer ", w, " seq ", seq, " corrupt");
  }
  for (auto& t : writers) t.join();
  CHECK(!failed.load());
  CHECK(bad == 0);
  // Completeness: every writer's full sequence arrived in order.
  for (u32_t w = 0; w < kWriters; ++w) CHECK(next_seq[w] == kRecords);
  // And nothing extra: the frontier is clean after all writers stopped.
  CHECK(q.peek().data() == nullptr);
}

// ---------------------------------------------------------------------------
// Contention regimes. An unpinned test can go green without ever contending --
// the scheduler runs the writers sequentially and the claim CAS never loses --
// so each regime ASSERTS contention happened (nonzero onContended). A
// contention test recording zero contention did not test what it claims.
// ---------------------------------------------------------------------------
namespace {

// Shared body: W writer threads x R tagged records into a 4 KiB ring, reader
// validates per-writer FIFO + integrity + completeness. `pin` fixes writer i
// to core i+1 and the reader to core 0 for real cross-core coherence traffic;
// unpinned oversubscription instead forces preemption inside the
// reserve-to-commit window.
// NO doctest assertions inside the hot loops: each recorded assertion costs
// more than the queue op it checks, and an assertion-throttled reader under
// oversubscription fails the deadline without testing anything. Errors are
// recorded in plain state; every thread is JOINED before the first assertion
// fires (a throwing REQUIRE with joinable threads is std::terminate).
void contentionRun(u32_t writers, u32_t records, bool pin) {
  std::atomic<u64_t> contended{0};
  CountingPolicy pol;
  pol.contended = &contended;
  Ring<CountingPolicy> q{pol};
  REQUIRE(Ring<CountingPolicy>::create(smallConfig(), q));
  REQUIRE(q.attachReader());
  // Affinity is INHERITED by spawned threads and outlives the test, so save
  // the entry mask and restore it on every exit path -- a leaked pin quietly
  // collapses every later test onto one core.
  cpu_set_t entry_mask;
  CPU_ZERO(&entry_mask);
  REQUIRE(pthread_getaffinity_np(pthread_self(), sizeof(entry_mask), &entry_mask) == 0);
  struct AffinityGuard {
    cpu_set_t const* mask;
    ~AffinityGuard() { pthread_setaffinity_np(pthread_self(), sizeof(*mask), mask); }
  } guard{&entry_mask};
  if (pin) {
    cpu_set_t set;
    CPU_ZERO(&set);
    CPU_SET(0, &set);
    REQUIRE(pthread_setaffinity_np(pthread_self(), sizeof(set), &set) == 0);
  }

  std::atomic<bool> stop{false};
  std::atomic<u32_t> writer_errors{0};
  std::vector<std::thread> ts;
  ts.reserve(writers);
  for (u32_t w = 0; w < writers; ++w) {
    ts.emplace_back([&q, &stop, &writer_errors, w, records] {
      if (!q.attachWriter()) {
        writer_errors.fetch_add(1, std::memory_order_relaxed);
        return;
      }
      std::byte buf[96];
      for (u32_t seq = 0; seq < records && !stop.load(std::memory_order_relaxed); ++seq) {
        sz_t const n = 8 + (w * 31 + seq) % 80;
        std::memcpy(buf, &w, 4);
        std::memcpy(buf + 4, &seq, 4);
        for (sz_t i = 8; i < n; ++i) buf[i] = patternByte(w, seq, i);
        while (!q.write(buf, n)) {
          if (q.status() != Status::kFull || stop.load(std::memory_order_relaxed)) {
            writer_errors.fetch_add(1, std::memory_order_relaxed);
            return;
          }
          std::this_thread::yield();
        }
      }
      q.detachWriter();
    });
    if (pin) {
      cpu_set_t set;
      CPU_ZERO(&set);
      CPU_SET(static_cast<int>(w + 1), &set);
      if (pthread_setaffinity_np(ts.back().native_handle(), sizeof(set), &set) != 0) {
        writer_errors.fetch_add(1, std::memory_order_relaxed);
      }
    }
  }

  std::vector<u32_t> next_seq(writers, 0);
  u64_t delivered = 0;
  u64_t const expected = static_cast<u64_t>(writers) * records;
  char error[128] = {0};
  Deadline dl;
  while (delivered < expected) {
    ReadSpan const r = q.peek();
    if (r.data() == nullptr) {
      if (dl.expired()) {
        std::snprintf(error, sizeof(error), "timed out with %llu/%llu delivered",
                      static_cast<unsigned long long>(delivered),
                      static_cast<unsigned long long>(expected));
        break;
      }
      continue;
    }
    u32_t w = ~0u;
    u32_t seq = ~0u;
    bool ok = r.size() >= 8;
    if (ok) {
      std::memcpy(&w, r.data(), 4);
      std::memcpy(&seq, r.data() + 4, 4);
      ok = w < writers && seq == next_seq[w];
    }
    for (sz_t i = 8; ok && i < r.size(); ++i) ok = r[i] == patternByte(w, seq, i);
    if (!ok) {
      std::snprintf(error, sizeof(error), "record %llu (writer %u seq %u) corrupt",
                    static_cast<unsigned long long>(delivered), w, seq);
      break;
    }
    ++next_seq[w];
    q.pop();
    ++delivered;
    dl.reset();  // progress: only a stall with NO delivery should expire it
  }

  stop.store(true, std::memory_order_relaxed);
  // On the error path writers may sit in the full-ring retry loop; they exit
  // on observing `stop` (the retry loop checks it), so joining cannot hang.
  for (auto& t : ts) t.join();

  REQUIRE_MESSAGE(error[0] == '\0', error);
  CHECK(writer_errors.load() == 0);
  for (u32_t w = 0; w < writers; ++w) CHECK(next_seq[w] == records);
  // The loop-closer: the claim CAS demonstrably lost at least once, so the
  // interleavings this test exists for actually occurred.
  CHECK(contended.load() > 0);
}

}  // namespace

TEST_CASE("pinned one-writer-per-core contention with proof of contention") {
  unsigned const cores = std::thread::hardware_concurrency();
  if (cores < 3) return;  // need the reader's core plus two writer cores
  u32_t const writers = std::min(4u, cores - 1);
  contentionRun(writers, 20000, /*pin=*/true);
}

TEST_CASE("oversubscribed writers preempted inside the reserve-to-commit window") {
  unsigned const cores = std::max(1u, std::thread::hardware_concurrency());
  u32_t const writers = std::min(2 * cores, 48u);  // deliberately > core count
  contentionRun(writers, 24000u / writers + 100, /*pin=*/false);
}

// ---------------------------------------------------------------------------
// Capacity behaviour: kFull under backpressure, recovery after draining,
// kTooLarge for records that can never fit.
// ---------------------------------------------------------------------------
TEST_CASE_TEMPLATE("full is backpressure and too-large is permanent", Q, Mpsc) {
  Q q;
  REQUIRE(Q::create(smallConfig(), q));
  REQUIRE(q.attachReader());

  std::byte buf[64] = {};
  u32_t pushed = 0;
  while (q.write(buf, sizeof(buf))) ++pushed;
  CHECK(q.status() == Status::kFull);
  // The admission bound keeps kGrain of headroom (a full ring is
  // indistinguishable from an empty one without it), so the exact count is
  // (capacity - kGrain) / extent.
  CHECK(pushed == (4096 - kGrain) / extentFor(sizeof(buf)));

  // Draining everything makes the ring writable again -- across the wrap.
  u32_t popped = 0;
  while (q.peek().data() != nullptr) {
    q.pop();
    ++popped;
  }
  CHECK(popped == pushed);
  CHECK(q.write(buf, sizeof(buf)));

  CHECK(q.reserve(4096).data() == nullptr);
  CHECK(q.status() == Status::kTooLarge);
  // kTooLarge must not have perturbed the ring.
  CHECK(q.write(buf, 1));
}

// ---------------------------------------------------------------------------
// White-box recovery: a forged kClaimed descriptor with a dead owner tid is
// exactly what a writer killed between claim and successor-stamp leaves
// behind. The reader must stamp the successor, abort the record, and the ring
// must accept claims again. (The fork-based tests can only produce the
// kCleared flavour deterministically; this covers the kClaimed flavour.)
// ---------------------------------------------------------------------------
TEST_CASE("reader recovers a forged dead kClaimed record and the ring resumes") {
  std::atomic<u64_t> reclaims{0};
  CountingPolicy pol;
  pol.reclaims = &reclaims;
  Ring<CountingPolicy> q{pol};
  REQUIRE(Ring<CountingPolicy>::create(smallConfig(), q));
  REQUIRE(q.attachReader());

  std::byte buf[40];
  std::memset(buf, 0x11, sizeof(buf));
  REQUIRE(q.write(buf, sizeof(buf)));  // record A, extent 64 at pos 0

  // A tid at PID_MAX_LIMIT-1 that no live thread plausibly holds.
  u32_t const dead_tid = (1u << 22) - 2;
  REQUIRE(!threadAlive(dead_tid));

  // Forge the dead writer's claim at the frontier (pos 64): claimed, extent
  // 128, successor NOT stamped -- died before the stamp.
  Region const& reg = q.region();
  std::atomic_ref<u64_t> frontier(*reinterpret_cast<u64_t*>(reg.arena(0) + 64));
  REQUIRE(frontier.load() == freeWord(64));
  frontier.store(packRecord(128, 0, State::kClaimed, dead_tid), std::memory_order_release);

  // Reader: delivers A, then recovers the forged record (invisible), frontier.
  ReadSpan r = q.peek();
  REQUIRE(r.data() != nullptr);
  CHECK(r.size() == sizeof(buf));
  q.pop();
  // Liveness is SPACED off the reader's busy path (Ring::kBusyPollsPerLiveness):
  // /proc is consulted only after consecutive busy polls of the same record, so
  // recovery needs polling, not one call. The busy record is never delivered
  // meanwhile. (Edited by impl-queue when the spacing landed; a single peek()
  // here previously recovered immediately, and the write() below then wedged
  // this thread -- it is both writer and the only possible recoverer.)
  for (int i = 0; i < 100000 && reclaims.load() == 0; ++i) {
    CHECK(q.peek().data() == nullptr);
  }
  CHECK(reclaims.load() == 1);

  // Recovery stamped the successor, so the ring accepts claims again; the new
  // record lands after the aborted extent and round-trips intact.
  std::memset(buf, 0x22, sizeof(buf));
  REQUIRE(q.write(buf, sizeof(buf)));
  r = q.peek();
  REQUIRE(r.data() != nullptr);
  REQUIRE(r.size() == sizeof(buf));
  CHECK(std::memcmp(r.data(), buf, sizeof(buf)) == 0);
  q.pop();
}

// ---------------------------------------------------------------------------
// Reader restart: a dead reader's successor resumes at read_pos. Delivery is
// at-least-once -- a record peeked but not popped is REdelivered, and the test
// asserts that rather than exactly-once.
// ---------------------------------------------------------------------------
TEST_CASE("reader restart resumes at read_pos with at-least-once delivery") {
  Mpsc q;
  REQUIRE(Mpsc::create(smallConfig(), q));

  auto push_tagged = [&](u32_t tag) {
    std::byte buf[12];
    std::memcpy(buf, &tag, 4);
    for (sz_t i = 4; i < sizeof(buf); ++i) buf[i] = patternByte(0, tag, i);
    while (!q.write(buf, sizeof(buf))) std::this_thread::yield();
  };
  auto tag_of = [](ReadSpan r) {
    u32_t t;
    REQUIRE(r.size() >= 4);
    std::memcpy(&t, r.data(), 4);
    return t;
  };

  for (u32_t t = 0; t < 20; ++t) push_tagged(t);

  // First reader: separate view over the same region, pops 0..9, PEEKS 10
  // without popping, then dies (thread exit reaps the tid).
  std::thread first([&] {
    Ring<DefaultPolicy> r1(q.region(), 0);
    REQUIRE(r1.attachReader());
    for (u32_t t = 0; t < 10; ++t) {
      ReadSpan const r = r1.peek();
      REQUIRE(r.data() != nullptr);
      CHECK(tag_of(r) == t);
      r1.pop();
    }
    ReadSpan const r = r1.peek();  // handed to the app, never popped
    REQUIRE(r.data() != nullptr);
    CHECK(tag_of(r) == 10);
  });
  first.join();

  // A second reader cannot attach while... the first is dead, so it CAN: the
  // takeover CAS must succeed against the dead tid and resume at read_pos.
  Ring<DefaultPolicy> r2(q.region(), 0);
  REQUIRE(r2.attachReader());
  for (u32_t t = 10; t < 20; ++t) {  // 10 is redelivered: at-least-once
    ReadSpan const r = r2.peek();
    REQUIRE(r.data() != nullptr);
    CHECK(tag_of(r) == t);
    r2.pop();
  }
  CHECK(r2.peek().data() == nullptr);
}

TEST_CASE("attachReader refuses a rival while the holder lives, re-admits the holder") {
  Mpsc q;
  REQUIRE(Mpsc::create(smallConfig(), q));
  REQUIRE(q.attachReader());  // this thread, alive

  // A DIFFERENT live thread must be refused.
  bool rival_attached = true;
  std::thread rival([&] {
    Ring<DefaultPolicy> other(q.region(), 0);
    rival_attached = other.attachReader();
  });
  rival.join();
  CHECK(!rival_attached);

  // Re-attach by the current holder is an idempotent cursor re-sync (the
  // Sharded composition attaches ring by ring against one queue-wide tid).
  Ring<DefaultPolicy> same_thread(q.region(), 0);
  CHECK(same_thread.attachReader());
}

// ---------------------------------------------------------------------------
// Region.
// ---------------------------------------------------------------------------
TEST_CASE("Region: geometry, mirror aliasing, arena init") {
  Config cfg;
  cfg.capacity = 100000;  // rounds up to 131072
  cfg.shards = 3;
  Region r;
  REQUIRE(Region::create(cfg, r));
  CHECK(r.capacity() == 131072);
  CHECK(r.shardCount() == 3);
  CHECK(r.control()->shards == 3);
  CHECK(r.control()->capacity == 131072);
  CHECK(r.control()->reader_tid == 0);

  for (u32_t i = 0; i < 3; ++i) {
    // Position 0 stamped claimable.
    u64_t w;
    std::memcpy(&w, r.arena(i), sizeof(w));
    CHECK(w == freeWord(0));
    // Mirror: a store through the primary alias is visible at +capacity. The
    // barrier matters -- the two aliases are distinct objects to the compiler.
    r.arena(i)[100] = std::byte{0x5a};
    asm volatile("" ::: "memory");
    CHECK(r.arena(i)[100 + r.capacity()] == std::byte{0x5a});
    r.arena(i)[100] = std::byte{0};
  }

  // Writer registry: after the ShardControls, zeroed, inside the control area.
  CHECK(r.bitmapWords() == 1);
  CHECK(r.writerBitmap()[0] == 0);
  for (u32_t i = 0; i < 3; ++i) {
    CHECK(r.writerSlots()[i].owner_tid == 0);
    CHECK(r.writerSlots()[i].generation == 0);
  }
  CHECK(reinterpret_cast<std::byte*>(r.writerSlots() + 3) <= r.arena(0));
}

TEST_CASE("Region: control area sizes the writer registry past one page") {
  // 127 shards, DELIBERATELY not a rounder number: the registry must be
  // genuinely RESERVED in the control area, not fitting in page-align slack by
  // luck (SpscRing does its claim fetch_or on these words). At 100 shards,
  // deleting the WriterSlot term from controlBytes() is INVISIBLE -- 25744 and
  // 26544 bytes both round to 7 pages (impl-spsc's finding). At 127, the slot
  // table is exactly what pushes the control area across its final page
  // boundary (32656 -> 8 pages without it, 33672 -> 9 with), so that break
  // fails the layout REQUIRE below deterministically. The constant is
  // 4 KiB-page-specific.
  constexpr u32_t kShards = 127;
  Config cfg;
  cfg.capacity = 4096;
  cfg.shards = kShards;
  Region r;
  REQUIRE(Region::create(cfg, r));
  CHECK(r.bitmapWords() == 2);
  REQUIRE(reinterpret_cast<std::byte*>(r.writerSlots() + kShards) <= r.arena(0));
  CHECK((reinterpret_cast<uintptr_t>(r.arena(0)) & (pageSize() - 1)) == 0);
  CHECK(r.writerBitmap()[1] == 0);
  for (u32_t i = 0; i < kShards; ++i) {
    u64_t w;
    std::memcpy(&w, r.arena(i), sizeof(w));
    REQUIRE(w == freeWord(0));
  }
  // Attach re-derives the multi-page geometry.
  int const fd = dup(r.fd());
  Region s;
  REQUIRE(Region::attach(fd, s));
  CHECK(s.shardCount() == kShards);
  CHECK(s.bitmapWords() == 2);
  // Registry writes made through one mapping are visible through the other.
  s.writerSlots()[kShards - 1].owner_tid = 4242;
  asm volatile("" ::: "memory");
  CHECK(r.writerSlots()[kShards - 1].owner_tid == 4242);
  s.writerSlots()[kShards - 1].owner_tid = 0;
}

TEST_CASE("Region: attach verifies magic, version, and file size") {
  Config cfg;
  cfg.capacity = 4096;
  Region r;
  REQUIRE(Region::create(cfg, r));

  Region ok;
  int fd = dup(r.fd());
  REQUIRE(Region::attach(fd, ok));
  CHECK(ok.capacity() == r.capacity());

  // Corrupt the magic: attach must refuse (the mapping is shared, so restore).
  u64_t const saved = r.control()->magic;
  r.control()->magic = saved ^ 1;
  Region bad;
  fd = dup(r.fd());
  CHECK(!Region::attach(fd, bad));
  close(fd);  // attach failed, so the fd is still ours
  r.control()->magic = saved;

  // Wrong version: refuse.
  r.control()->version += 1;
  fd = dup(r.fd());
  CHECK(!Region::attach(fd, bad));
  close(fd);
  r.control()->version -= 1;

  // A random memfd with no header at all: refuse.
  int const junk = static_cast<int>(memfd_create("junk", 0));
  REQUIRE(junk >= 0);
  REQUIRE(ftruncate(junk, 65536) == 0);
  CHECK(!Region::attach(junk, bad));
  close(junk);
}

TEST_CASE("Region: kFile create over a dirty reused file still initialises") {
  // The claimability invariant is "desc[0] reads kFree(0)", NOT "the backend
  // zeroes memory". A fresh memfd hides a missing stamp because freeWord(0)
  // happens to be 0; a reused file full of garbage does not.
  char path[] = "/tmp/pgt-mpsc-dirty-XXXXXX";
  int const tmp = mkstemp(path);
  REQUIRE(tmp >= 0);
  std::vector<std::byte> junk(64 * 1024);
  for (sz_t i = 0; i < junk.size(); ++i) junk[i] = static_cast<std::byte>(i * 131 + 7);
  REQUIRE(write(tmp, junk.data(), junk.size()) == static_cast<ssz_t>(junk.size()));
  close(tmp);

  Config cfg;
  cfg.capacity = 4096;
  cfg.backend = Backend::kFile;
  cfg.name = path;
  Region r;
  REQUIRE(Region::create(cfg, r));
  u64_t w;
  std::memcpy(&w, r.arena(0), sizeof(w));
  // REQUIRE, not CHECK: with a garbage descriptor at position 0 the ring
  // exercise below spins forever, so fail here instead of hanging.
  REQUIRE(w == freeWord(0));
  CHECK(r.control()->reader_tid == 0);
  CHECK(r.writerBitmap()[0] == 0);

  // And the ring built over it must actually work end to end.
  Mpsc q;
  {
    Config qc = cfg;
    REQUIRE(Mpsc::create(qc, q));
    REQUIRE(q.attachReader());
    std::byte buf[24];
    for (sz_t i = 0; i < sizeof(buf); ++i) buf[i] = static_cast<std::byte>(i);
    REQUIRE(q.write(buf, sizeof(buf)));
    ReadSpan const got = q.peek();
    REQUIRE(got.size() == sizeof(buf));
    CHECK(std::memcmp(got.data(), buf, sizeof(buf)) == 0);
    q.pop();
  }
  unlink(path);
}

TEST_CASE("Region: moved-from and default objects are inert") {
  Config cfg;
  cfg.capacity = 4096;
  Region a;
  REQUIRE(Region::create(cfg, a));
  int const fd = a.fd();
  Region b = std::move(a);
  CHECK(!a.valid());  // NOLINT: moved-from state is defined here
  CHECK(b.valid());
  CHECK(b.fd() == fd);
  a = std::move(b);  // move back over the moved-from object
  CHECK(a.valid());
  Region c;
  c = std::move(a);
  CHECK(c.valid());
  // a, b, and a default-constructed Region all destruct here: must not crash
  // or double-close.
  Region d;
  static_cast<void>(d);
}

TEST_CASE("desc encoding round-trips the boundary payload sizes") {
  for (sz_t n : {sz_t{0}, sz_t{1}, kGrain - 8, kGrain - 7, kGrain, 2 * kGrain - 8, sz_t{300}}) {
    sz_t const extent = extentFor(n);
    u64_t const w =
        packRecord(extent, static_cast<u32_t>(n & (kGrain - 1)), State::kCommitted, 12345);
    CHECK(extentOf(w) == extent);
    CHECK(committedLen(w) == n);
    CHECK(tidOf(w) == 12345);
    CHECK(stateOf(w) == State::kCommitted);
  }
  CHECK(freeWord(0) == 0);
  CHECK(freePos(freeWord(4096)) == 4096);
  // The lap-ABA guard: FREE(p) never equals FREE(p + k*N).
  CHECK(freeWord(4096) != freeWord(4096 + 131072));
}

}  // namespace
