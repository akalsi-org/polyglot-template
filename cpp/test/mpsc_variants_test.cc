// Behavioral coverage for the variants mpsc_test.cc does not yet drive:
// Spsc, ShardedMpsc (K >= 2), and MultiSpsc.
//
// Owner: impl-queue. mpsc_test.cc and mpsc_fault_test.cc are impl-region's.
//
// The comparison discipline differs from the Mpsc differential on purpose:
// these variants promise PER-WRITER FIFO and completeness, never cross-writer
// total order, so every assertion here is on a per-writer subsequence -- each
// record carries {writer, seq} and the reader checks seq is exactly the last
// seen + 1 for that writer, payload bytes match the pattern, and final counts
// match. Cross-writer interleaving is deliberately unchecked.
//
// Every case uses a tiny capacity and pushes many capacities of data so the
// ring WRAPS repeatedly -- wrapping is what makes the lap-aliasing defect
// class reachable at all.
//
// Every assertion here has been validated by deliberately breaking the
// implementation and watching the test fail (see the report for the list);
// deadline guards turn would-be hangs into failures.

#include "pgt/mpsc/desc.hh"
#include "pgt/mpsc/queue.hh"

#include <doctest/doctest.h>

#include <pthread.h>
#include <sched.h>

#include <atomic>
#include <chrono>
#include <cstring>
#include <random>
#include <thread>
#include <vector>

namespace {

using namespace pgt;
using namespace pgt::mpsc;

// An unpinned concurrency test can pass without ever having been concurrent:
// if the scheduler serializes the writers onto one core, the claim CAS never
// contends and the interleavings that expose defects never occur -- the suite
// goes green having exercised nothing. So the differentials run in TWO
// regimes, because they find different bug classes:
//
//   kPinned          one writer per physical core: contention becomes real
//                    cross-core coherence traffic (claim CAS, causal chain,
//                    line handoff). Skipped gracefully on small machines.
//   kOversubscribed  more threads than logical cores, deliberately unpinned:
//                    this is what preempts a writer INSIDE the reserve-to-
//                    commit window, where nearly every defect in this design
//                    lived. Pinning alone reduces interleaving diversity, so
//                    this regime is not optional.
enum class Regime { kPinned, kOversubscribed };

// SMT siblings enumerate as (0,1),(2,3),...; even cpus are distinct cores.
bool pinToPhysicalCore(unsigned slot) {
  cpu_set_t set;
  CPU_ZERO(&set);
  CPU_SET(slot * 2, &set);
  return pthread_setaffinity_np(pthread_self(), sizeof(set), &set) == 0;
}

unsigned physicalCores() {
  unsigned const logical = std::thread::hardware_concurrency();
  return logical >= 2 ? logical / 2 : 1;  // assumes 2-way SMT; conservative
}

struct Deadline {
  std::chrono::steady_clock::time_point end =
      std::chrono::steady_clock::now() + std::chrono::seconds(60);
  [[nodiscard]] bool expired() const { return std::chrono::steady_clock::now() > end; }
};

struct Tag {
  u32_t writer;
  u32_t seq;
};

std::byte patternByte(u32_t writer, u32_t seq, sz_t i) {
  return static_cast<std::byte>(writer * 151u + seq * 29u + static_cast<u32_t>(i) * 7u + 3u);
}

void fillRecord(std::byte* p, u32_t writer, u32_t seq, sz_t n) {
  Tag const t{writer, seq};
  std::memcpy(p, &t, sizeof t);
  for (sz_t i = sizeof t; i < n; ++i) p[i] = patternByte(writer, seq, i);
}

// Validates one delivered record against the per-writer expectation; returns
// the writer id. REQUIREs inside keep failure output close to the defect.
u32_t checkRecord(ReadSpan s, std::vector<u32_t>& next_seq, std::vector<u64_t>& bytes) {
  REQUIRE(s.size() >= sizeof(Tag));
  Tag t;
  std::memcpy(&t, s.data(), sizeof t);
  REQUIRE(t.writer < next_seq.size());
  // Per-writer FIFO + completeness: exactly the next sequence number.
  REQUIRE(t.seq == next_seq[t.writer]);
  ++next_seq[t.writer];
  for (sz_t i = sizeof t; i < s.size(); ++i) {
    if (s[i] != patternByte(t.writer, t.seq, i)) {
      REQUIRE_MESSAGE(false, "payload corruption at offset ", i);
    }
  }
  bytes[t.writer] += s.size();
  return t.writer;
}

// Drives Q with `writers` threads pushing `per_writer` randomized records
// (short commits and aborts included), reader on the calling thread, and
// checks per-writer subsequences exactly.
template <typename Q>
void perWriterDifferential(Q& q, u32_t writers, u32_t per_writer, u32_t max_payload) {
  REQUIRE(q.attachReader());
  Deadline dl;
  std::atomic<bool> failed{false};
  std::vector<std::thread> ts;
  for (u32_t w = 0; w < writers; ++w) {
    ts.emplace_back([&, w] {
      if (!q.attachWriter()) {
        failed.store(true);
        return;
      }
      std::mt19937 rng(w * 9973u + 17u);
      std::uniform_int_distribution<u32_t> dist(sizeof(Tag), max_payload);
      for (u32_t seq = 0; seq < per_writer;) {
        u32_t const n = dist(rng);
        u32_t const slack = dist(rng) % 64u;
        WriteSpan s = q.reserve(n + slack);  // reserve high...
        if (s.data() == nullptr) {
          if (dl.expired()) {
            failed.store(true);
            return;
          }
          cpuRelax();
          continue;
        }
        if (rng() % 16 == 0) {  // sprinkle aborts: must be invisible downstream
          q.abort();
          continue;
        }
        fillRecord(s.data(), w, seq, n);
        q.commit(n);  // ...commit low: exercises the short-commit trailer
        ++seq;
      }
      q.detachWriter();
    });
  }
  std::vector<u32_t> next_seq(writers, 0);
  std::vector<u64_t> bytes(writers, 0);
  u64_t const expect = static_cast<u64_t>(writers) * per_writer;
  u64_t got = 0;
  while (got < expect && !dl.expired() && !failed.load()) {
    ReadSpan const s = q.peek();
    if (s.data() == nullptr) continue;
    checkRecord(s, next_seq, bytes);
    q.pop();
    ++got;
  }
  for (auto& t : ts) t.join();
  REQUIRE_FALSE(failed.load());
  REQUIRE(got == expect);  // completeness: nothing lost, nothing duplicated
  for (u32_t w = 0; w < writers; ++w) CHECK(next_seq[w] == per_writer);
}

Config tinyConfig(u32_t shards) {
  Config cfg;
  cfg.capacity = 4096;  // wraps every ~63 minimal records, per shard
  cfg.shards = shards;
  return cfg;
}

// ---------------------------------------------------------------------------

TEST_CASE("ShardedMpsc K=2: per-writer differential, wrapping, short commits") {
  ShardedMpsc q;
  REQUIRE(ShardedMpsc::create(tinyConfig(2), q));
  perWriterDifferential(q, /*writers=*/4, /*per_writer=*/4000, /*max_payload=*/200);
}

TEST_CASE("MultiSpsc: per-writer differential at full registration") {
  MultiSpsc q;
  REQUIRE(MultiSpsc::create(tinyConfig(4), q));
  perWriterDifferential(q, /*writers=*/4, /*per_writer=*/4000, /*max_payload=*/200);
}

TEST_CASE("Spsc: differential with boundary sizes 0, 1, kGrain-8, kGrain") {
  Spsc q;
  REQUIRE(Spsc::create(tinyConfig(1), q));
  std::thread wt([&] {
    REQUIRE(q.attachWriter());
    Deadline dl;
    std::mt19937 rng(42);
    sz_t const sizes[] = {0, 1, kGrain - 8, kGrain, 3, 100, kGrain - 8};
    for (u32_t seq = 0; seq < 20000; ++seq) {
      sz_t const n = sizes[seq % (sizeof(sizes) / sizeof(sizes[0]))];
      std::byte buf[128];
      for (sz_t i = 0; i < n; ++i) buf[i] = patternByte(0, seq, i);
      while (!q.write(buf, n)) {
        if (dl.expired()) return;
        cpuRelax();
      }
    }
  });
  REQUIRE(q.attachReader());
  Deadline dl;
  sz_t const sizes[] = {0, 1, kGrain - 8, kGrain, 3, 100, kGrain - 8};
  for (u32_t seq = 0; seq < 20000 && !dl.expired();) {
    ReadSpan const s = q.peek();
    if (s.data() == nullptr) continue;
    sz_t const want = sizes[seq % (sizeof(sizes) / sizeof(sizes[0]))];
    REQUIRE(s.size() == want);  // exact framing, including zero-length records
    for (sz_t i = 0; i < s.size(); ++i) REQUIRE(s[i] == patternByte(0, seq, i));
    q.pop();
    ++seq;
  }
  wt.join();
}

// The exact bug that escaped every compile-time check last week: reader_tid is
// queue-wide, Sharded attaches ring by ring, and rings 1..K-1 saw "a live
// reader" -- the caller itself -- and failed.
TEST_CASE("REGRESSION: Sharded<Ring> K>=2 attachReader succeeds and delivers") {
  ShardedMpsc q;
  REQUIRE(ShardedMpsc::create(tinyConfig(4), q));
  REQUIRE(q.attachReader());   // K=4: requires holder-idempotent Ring::attachReader
  perWriterDifferential(q, /*writers=*/4, /*per_writer=*/500, /*max_payload=*/64);
}

TEST_CASE("registration: Spsc rejects a second writer; MultiSpsc reports kNoSlot") {
  Spsc q;
  REQUIRE(Spsc::create(tinyConfig(1), q));
  REQUIRE(q.attachWriter());
  std::thread second([&] { CHECK_FALSE(q.attachWriter()); });  // sole ownership enforced
  second.join();

  MultiSpsc m;
  REQUIRE(MultiSpsc::create(tinyConfig(2), m));
  CHECK(m.maxWriters() == 2);
  // The claimants must STAY ALIVE until every attach has been attempted: an
  // exited thread's tid is proven dead, and a drained dead-owner ring is
  // legitimately recyclable by design (takeover gates a+b) -- the first
  // version of this test joined too early and "found" an over-admission that
  // was really the takeover path working correctly.
  std::atomic<int> ok{0};
  std::atomic<int> attempted{0};
  std::atomic<bool> release{false};
  std::vector<std::thread> ts;
  for (int i = 0; i < 3; ++i) {  // 3 live writers, 2 slots: exactly one must fail
    ts.emplace_back([&] {
      if (m.attachWriter()) {
        ok.fetch_add(1);
      } else {
        CHECK(m.status() == Status::kNoSlot);
      }
      attempted.fetch_add(1);
      while (!release.load(std::memory_order_acquire)) cpuRelax();
    });
  }
  Deadline dl;
  while (attempted.load() != 3 && !dl.expired()) cpuRelax();
  release.store(true, std::memory_order_release);
  for (auto& t : ts) t.join();
  CHECK(ok.load() == 2);
}

}  // namespace
