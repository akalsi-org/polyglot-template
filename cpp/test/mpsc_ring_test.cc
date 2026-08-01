// In-process tests for the two-plane MPSC queue (pgt::mpsc::MpscRing),
// the surviving multi-producer variant. Structure and conventions:
//
//  * every queue test uses a SMALL capacity and pushes many capacities of data,
//    so the ring wraps repeatedly -- a test that never wraps cannot see the
//    defect class that FREE-carries-position and the self-certifying commit tag
//    exist to prevent;
//  * the differential test drives the queue and a trivially-correct
//    mutex-guarded reference with the same stream and compares delivered bytes
//    exactly;
//  * no-progress deadlines are reset on every delivered record, so a wedged
//    queue fails within one window while a slow-but-progressing drain does not.
//
// Both the compact and the padded cell layouts are instantiated everywhere, so
// a layout-dependent indexing bug cannot hide in the variant that is not
// benchmarked.
//
// Fork-based fault injection (killed / stopped writers) lives in
// mpsc_ring_fault_test.cc so this file stays runnable under TSan. Unlike in-band queue,
// this variant has NO in-band descriptor, so the "stale walker probes plain
// payload bytes" race is structurally absent and needs no suppression.

#include "pgt/mpsc/desc.hh"
#include "pgt/mpsc/liveness.hh"
#include "pgt/mpsc/mpsc_ring.hh"

#include <doctest/doctest.h>

#include <fcntl.h>
#include <sys/resource.h>
#include <cerrno>
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

using MpscCompact = MpscRing<>;
using MpscPadded = MpscRing<DefaultPolicy, true>;

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

 private:
  std::mutex m_;
  std::deque<std::vector<std::byte>> q_;
};

struct Deadline {
  static constexpr std::chrono::seconds kWindow{60};
  std::chrono::steady_clock::time_point end = std::chrono::steady_clock::now() + kWindow;
  void reset() { end = std::chrono::steady_clock::now() + kWindow; }
  [[nodiscard]] bool expired() const { return std::chrono::steady_clock::now() > end; }
};

template <typename F>
bool eventually(F&& f) {
  auto const deadline = std::chrono::steady_clock::now() + std::chrono::seconds(5);
  while (!f()) {
    if (std::chrono::steady_clock::now() >= deadline) return false;
    std::this_thread::sleep_for(std::chrono::milliseconds(1));
  }
  return true;
}

std::byte patternByte(u32_t writer, u32_t seq, sz_t i) {
  return static_cast<std::byte>(writer * 151u + seq * 29u + static_cast<u32_t>(i) * 7u + 3u);
}

Config smallConfig() {
  Config cfg;
  cfg.capacity = 4096;  // one page: wraps every ~63 minimal records
  return cfg;
}

// ---------------------------------------------------------------------------
// Layout arithmetic. These are the numbers the design rests on, so they are
// asserted rather than left to prose.
// ---------------------------------------------------------------------------
TEST_CASE("MpscRing layout arithmetic") {
  // No in-band header: a payload that is a multiple of the grain packs exactly,
  // which is the case in-band queue's 8-byte header costs a whole extra grain.
  CHECK(tpExtentFor(0) == kGrain);  // never zero: a zero extent breaks the walk
  CHECK(tpExtentFor(1) == kGrain);
  CHECK(tpExtentFor(64) == kGrain);  // in-band queue needs 128 here
  CHECK(tpExtentFor(65) == 2 * kGrain);
  CHECK(tpExtentFor(128) == 2 * kGrain);

  // The commit tag is self-certifying in the same sense freeWord() is: it
  // carries the unwrapped position, so no lap can alias another.
  CHECK(commitTag(0) != 0);  // a zero-filled cell is never "committed"
  CHECK(commitTag(64) != commitTag(0));
  CHECK(commitTag(1u << 20) != commitTag(0));
  for (u64_t lap = 1; lap <= 4; ++lap) {
    CHECK(commitTag(64) != commitTag(64 + lap * (1u << 20)));
  }

  // Metadata: 8 bytes of Claim + 16 bytes of Result per 64-byte payload grain.
  CHECK(MpscCompact::metadataRatio() == doctest::Approx(0.375));
  CHECK(MpscPadded::metadataRatio() == doctest::Approx(2.0));
}

// ---------------------------------------------------------------------------
// Differential against the reference, single writer, exact byte-stream compare,
// with arbitrary short commits and boundary sizes, wrapping many times.
// ---------------------------------------------------------------------------
TEST_CASE_TEMPLATE("MpscRing differential vs reference with short commits", Q, MpscCompact,
                   MpscPadded) {
  Q q;
  REQUIRE(Q::create(smallConfig(), q));
  REQUIRE(q.attachReader());
  ReferenceQueue ref;

  sz_t const special[] = {0, 1, kGrain - 1, kGrain, kGrain + 1, 2 * kGrain, 300};
  std::mt19937 rng(0xC0FFEE);
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
      if (m != 0) std::memcpy(s.data(), bytes.data(), m);
      // The next record must still begin at the FULL reserved extent even when
      // the commit is short; the reference only ever sees the m bytes.
      q.commit(s, m);
      ref.push(std::move(bytes));
    }
    drain_compare();
  }
}

// ---------------------------------------------------------------------------
// Multi-writer integrity and PER-WRITER FIFO. Nothing here asserts cross-writer
// order (that is a variant property, checked separately below).
// ---------------------------------------------------------------------------
TEST_CASE_TEMPLATE("MpscRing multi-writer integrity and per-writer FIFO", Q, MpscCompact,
                   MpscPadded) {
  constexpr u32_t kWriters = 4;
  constexpr u32_t kPerWriter = 4000;

  Q q;
  REQUIRE(Q::create(smallConfig(), q));
  REQUIRE(q.attachReader());

  std::vector<std::thread> ts;
  for (u32_t w = 0; w < kWriters; ++w) {
    ts.emplace_back([&q, w] {
      REQUIRE(q.attachWriter());
      std::byte buf[192];
      for (u32_t seq = 0; seq < kPerWriter; ++seq) {
        sz_t const n = 8 + (seq % 180);
        std::memcpy(buf, &w, 4);
        std::memcpy(buf + 4, &seq, 4);
        for (sz_t i = 8; i < n; ++i) buf[i] = patternByte(w, seq, i);
        while (!q.write(buf, n)) {
          if (q.status() == Status::kReaderDead) FAIL("reader reported dead");
          std::this_thread::yield();
        }
      }
    });
  }

  std::vector<u32_t> next(kWriters, 0);
  u64_t delivered = 0;
  Deadline dl;
  while (delivered < u64_t{kWriters} * kPerWriter) {
    ReadSpan const r = q.peek();
    if (r.data() == nullptr) {
      REQUIRE_MESSAGE(!dl.expired(), "queue made no progress for 60s");
      continue;
    }
    REQUIRE(r.size() >= 8);
    u32_t w = 0, seq = 0;
    std::memcpy(&w, r.data(), 4);
    std::memcpy(&seq, r.data() + 4, 4);
    REQUIRE(w < kWriters);
    CHECK(seq == next[w]);  // per-writer FIFO, no loss, no duplication
    next[w] = seq + 1;
    sz_t const n = 8 + (seq % 180);
    REQUIRE(r.size() == n);
    for (sz_t i = 8; i < n; ++i) REQUIRE(r[i] == patternByte(w, seq, i));
    q.pop();
    ++delivered;
    dl.reset();
  }
  for (auto& t : ts) t.join();
  CHECK(q.peek().data() == nullptr);
  for (u32_t w = 0; w < kWriters; ++w) CHECK(next[w] == kPerWriter);
}

// ---------------------------------------------------------------------------
// Zero-length records survive a wrap and stay distinguishable from "empty".
// ---------------------------------------------------------------------------
TEST_CASE_TEMPLATE("MpscRing zero-length records", Q, MpscCompact, MpscPadded) {
  Q q;
  REQUIRE(Q::create(smallConfig(), q));
  REQUIRE(q.attachReader());
  for (int round = 0; round < 500; ++round) {
    for (int k = 0; k < 20; ++k) REQUIRE(q.write(nullptr, 0));
    for (int k = 0; k < 20; ++k) {
      ReadSpan const r = q.peek();
      REQUIRE(r.data() != nullptr);  // a delivered empty record, not "no record"
      CHECK(r.size() == 0);
      q.pop();
    }
    CHECK(q.peek().data() == nullptr);  // now genuinely empty
  }
}

// ---------------------------------------------------------------------------
// Fail-fast admission. reserve() must decline atomically, before any
// irreversible state change, and a declined reserve must leave the queue able
// to deliver everything already accepted.
// ---------------------------------------------------------------------------
TEST_CASE_TEMPLATE("MpscRing full boundary declines without side effects", Q, MpscCompact,
                   MpscPadded) {
  Q q;
  REQUIRE(Q::create(smallConfig(), q));
  REQUIRE(q.attachReader());

  // Larger than the ring can ever hold: a permanent error, not backpressure.
  CHECK(q.reserve(4096).data() == nullptr);
  CHECK(q.status() == Status::kTooLarge);
  CHECK(q.reserve(4033).data() == nullptr);
  CHECK(q.status() == Status::kTooLarge);

  // Largest record this configuration can hold: capacity minus the one-grain
  // admission slack.
  WriteSpan const big = q.reserve(4032);
  REQUIRE(big.data() != nullptr);
  std::memset(big.data(), 0x7e, big.size());
  q.commit(big, big.size());

  unsigned accepted = 0;
  for (;;) {
    WriteSpan const s = q.reserve(56);
    if (s.data() == nullptr) break;
    std::memset(s.data(), 0x11, 56);
    q.commit(s, 56);
    ++accepted;
  }
  CHECK(q.status() == Status::kFull);  // backpressure, not an error
  CHECK(accepted == 0);                // the ring is full behind the big record

  ReadSpan const r = q.peek();
  REQUIRE(r.data() != nullptr);
  CHECK(r.size() == 4032);
  for (sz_t i = 0; i < r.size(); ++i) REQUIRE(r[i] == std::byte{0x7e});
  q.pop();

  // Every declined reserve above must have left the frontier untouched, so
  // admission resumes immediately once the reader drains.
  for (unsigned i = 0; i < 60; ++i) {
    WriteSpan const s = q.reserve(56);
    REQUIRE(s.data() != nullptr);
    std::memset(s.data(), static_cast<int>(i), 56);
    q.commit(s, 56);
  }
  for (unsigned i = 0; i < 60; ++i) {
    ReadSpan const rr = q.peek();
    REQUIRE(rr.data() != nullptr);
    REQUIRE(rr.size() == 56);
    CHECK(rr[0] == static_cast<std::byte>(i));
    q.pop();
  }
  CHECK(q.peek().data() == nullptr);
}

// ---------------------------------------------------------------------------
// abort() publishes a void record: it is skipped, the next record still begins
// at the full reserved extent, and nothing is lost around it.
// ---------------------------------------------------------------------------
TEST_CASE_TEMPLATE("MpscRing abort skips exactly the reservation", Q, MpscCompact, MpscPadded) {
  Q q;
  REQUIRE(Q::create(smallConfig(), q));
  REQUIRE(q.attachReader());
  for (int round = 0; round < 300; ++round) {
    u32_t const tag = static_cast<u32_t>(round);
    REQUIRE(q.write(&tag, 4));
    WriteSpan const s = q.reserve(200);
    REQUIRE(s.data() != nullptr);
    std::memset(s.data(), 0xcd, s.size());
    q.abort();
    u32_t const tag2 = tag + 1000;
    REQUIRE(q.write(&tag2, 4));

    ReadSpan a = q.peek();
    REQUIRE(a.size() == 4);
    u32_t got = 0;
    std::memcpy(&got, a.data(), 4);
    CHECK(got == tag);
    q.pop();
    ReadSpan b = q.peek();
    REQUIRE(b.size() == 4);
    std::memcpy(&got, b.data(), 4);
    CHECK(got == tag2);
    q.pop();
    CHECK(q.peek().data() == nullptr);
  }
}

// Cross-writer CLAIM order is deliberately not asserted anywhere here. It is
// not externally observable: any ticket a writer takes is sequenced either
// before or after its own claim CAS, never atomically with it, so a ticket
// stream can legitimately disagree with the claim stream. What is observable --
// per-writer FIFO, no loss, no duplication, no tearing -- is checked above, and
// that is the same line the existing in-band queue suite draws.

// ---------------------------------------------------------------------------
// A writer holding a reservation across arbitrary user code must NOT block
// other writers' claims -- only the reader. This is the property the two-plane
// walk buys by allowing a walker to advance past an in-flight record, and it is
// the one behaviour that differs observably from in-band queue.
// ---------------------------------------------------------------------------
TEST_CASE_TEMPLATE("MpscRing in-flight record does not block later claims", Q, MpscCompact,
                   MpscPadded) {
  Q q;
  REQUIRE(Q::create(smallConfig(), q));
  REQUIRE(q.attachReader());

  std::atomic<bool> held{false};
  std::atomic<bool> release{false};
  std::atomic<unsigned> behind{0};

  std::thread holder([&] {
    REQUIRE(q.attachWriter());
    WriteSpan const s = q.reserve(64);
    REQUIRE(s.data() != nullptr);
    std::memset(s.data(), 0xa5, s.size());
    held.store(true, std::memory_order_release);
    while (!release.load(std::memory_order_acquire)) std::this_thread::yield();
    q.commit(s, 64);
  });

  while (!held.load(std::memory_order_acquire)) std::this_thread::yield();

  // The holder's record pins the READER, so nothing is deliverable; but claims
  // behind it must still succeed.
  for (unsigned i = 0; i < 20; ++i) {
    std::byte buf[32];
    std::memset(buf, static_cast<int>(i), sizeof(buf));
    if (q.write(buf, sizeof(buf))) ++behind;
  }
  CHECK(behind.load() == 20);
  CHECK(q.peek().data() == nullptr);  // reader still pinned at the in-flight record

  release.store(true, std::memory_order_release);
  holder.join();

  Deadline dl;
  ReadSpan first{};
  while ((first = q.peek()).data() == nullptr) REQUIRE(!dl.expired());
  CHECK(first.size() == 64);
  q.pop();
  for (unsigned i = 0; i < 20; ++i) {
    ReadSpan r{};
    dl.reset();
    while ((r = q.peek()).data() == nullptr) REQUIRE(!dl.expired());
    REQUIRE(r.size() == 32);
    CHECK(r[0] == static_cast<std::byte>(i));
    q.pop();
  }
  CHECK(q.peek().data() == nullptr);
}

// ---------------------------------------------------------------------------
// Reader restart. The reader cursor is view-private; the shared licence is
// read_pos, which advances only on pop(). So a reader that PEEKS a record and
// dies before popping it must have that record redelivered to its replacement:
// the contract is at-least-once, and the boundary between "delivered" and
// "retired" is pop(), never peek().
//
// This is the one recovery path the fault suite cannot cover, because it is
// about the READER dying rather than a writer, and it needs a second view over
// the same planes rather than a fork.
// ---------------------------------------------------------------------------
TEST_CASE_TEMPLATE("MpscRing reader restart resumes at read_pos", Q, MpscCompact, MpscPadded) {
  Q q;
  REQUIRE(Q::create(smallConfig(), q));
  REQUIRE(q.attachWriter());

  auto push_tagged = [&](u32_t tag) {
    std::byte buf[12];
    std::memcpy(buf, &tag, 4);
    for (sz_t i = 4; i < sizeof(buf); ++i) buf[i] = static_cast<std::byte>(tag * 31 + i);
    while (!q.write(buf, sizeof(buf))) std::this_thread::yield();
  };
  auto tag_of = [](ReadSpan r) {
    u32_t t;
    REQUIRE(r.size() >= 4);
    std::memcpy(&t, r.data(), 4);
    return t;
  };

  constexpr u32_t kRecords = 20;
  for (u32_t t = 0; t < kRecords; ++t) push_tagged(t);

  // First reader: a separate view. Pops 0..9, then PEEKS 10 without popping and
  // exits. Running it on its own thread makes the death real -- the tid is
  // reaped on join, so the replacement's attach sees a genuinely dead reader
  // rather than a synthetically cleared slot.
  std::thread first([&] {
    Q r1;
    REQUIRE(Q::createView(q, r1));
    REQUIRE(r1.attachReader());
    for (u32_t t = 0; t < 10; ++t) {
      ReadSpan const r = r1.peek();
      REQUIRE(r.data() != nullptr);
      CHECK(tag_of(r) == t);
      r1.pop();
    }
    ReadSpan const peeked = r1.peek();  // observed, deliberately NOT popped
    REQUIRE(peeked.data() != nullptr);
    CHECK(tag_of(peeked) == 10);
  });
  first.join();

  // join proves C++ completion, but kernel TID liveness can converge later. A
  // failed attach only observes shared ownership, so one successor may retry.
  // Replacement reader must resume at 10, not 11: the peek did not retire it.
  Q r2;
  REQUIRE(Q::createView(q, r2));
  REQUIRE(eventually([&] { return r2.attachReader(); }));
  for (u32_t t = 10; t < kRecords; ++t) {
    ReadSpan r = r2.peek();
    auto const deadline = std::chrono::steady_clock::now() + std::chrono::seconds(30);
    while (r.data() == nullptr) {
      REQUIRE(std::chrono::steady_clock::now() < deadline);
      r = r2.peek();
    }
    CHECK(tag_of(r) == t);
    for (sz_t i = 4; i < r.size(); ++i) {
      REQUIRE(r[i] == static_cast<std::byte>(t * 31 + i));
    }
    r2.pop();
  }
  CHECK(r2.peek().data() == nullptr);
}

// A live reader must NOT be displaced. attachReader() is the guard that keeps
// two readers from splitting one stream, and it may only yield to PROVEN death.
//
// The incumbent must be a genuinely different live thread: attachReader() is
// deliberately idempotent for the SAME thread (`cur != self`), so attaching two
// views from one thread proves nothing and would pass vacuously.
TEST_CASE_TEMPLATE("MpscRing live reader cannot be displaced", Q, MpscCompact, MpscPadded) {
  Q q;
  REQUIRE(Q::create(smallConfig(), q));

  std::atomic<bool> attached{false};
  std::atomic<bool> release{false};
  std::thread incumbent([&] {
    Q r1;
    REQUIRE(Q::createView(q, r1));
    REQUIRE(r1.attachReader());
    attached.store(true, std::memory_order_release);
    // Stay ALIVE while the challenger attempts to take over.
    while (!release.load(std::memory_order_acquire)) std::this_thread::yield();
  });
  while (!attached.load(std::memory_order_acquire)) std::this_thread::yield();

  Q challenger;
  REQUIRE(Q::createView(q, challenger));
  CHECK_FALSE(challenger.attachReader());  // refused: the incumbent is alive

  release.store(true, std::memory_order_release);
  incumbent.join();

  // join proves C++ completion, but kernel TID liveness can converge later. A
  // failed attach only observes shared ownership, so one successor may retry.
  Q successor;
  REQUIRE(Q::createView(q, successor));
  REQUIRE(eventually([&] { return successor.attachReader(); }));
}

TEST_CASE_TEMPLATE("MpscRing same-thread second reader view is refused", Q, MpscCompact,
                   MpscPadded) {
  Q q;
  REQUIRE(Q::create(smallConfig(), q));
  REQUIRE(q.attachReader());
  REQUIRE(q.attachReader());  // idempotent for the same view

  Q challenger;
  REQUIRE(Q::createView(q, challenger));
  CHECK_FALSE(challenger.attachReader());
  REQUIRE(q.attachReader());  // incumbent remains attached
  q.detachReader();
  REQUIRE(challenger.attachReader());
  CHECK_FALSE(q.attachReader());
  challenger.detachReader();
}

// ---------------------------------------------------------------------------
// REGRESSION (found by audit, 2026-07-26): unchecked overflow in the extent
// rounding admitted an unrepresentable reservation.
//
// tpExtentFor rounds up to a grain. For payload in [SIZE_MAX-62, SIZE_MAX] the
// `+63` wraps to 0, and the `need == 0 -> kGrain` branch then rewrote it as the
// SMALLEST possible record. Admission only ever sees the extent, so it accepted
// it. reserve(SIZE_MAX) returned kOk with a WriteSpan advertising SIZE_MAX
// writable bytes over a one-grain reservation, and write() memcpy's
// span.size() bytes -- an unbounded out-of-bounds write reachable from any
// caller whose length came from an upstream subtraction that underflowed.
//
// The fix checks representability BEFORE rounding, because rounding destroys
// the evidence that the request was too large.
// ---------------------------------------------------------------------------
TEST_CASE_TEMPLATE("MpscRing declines unrepresentable lengths without lying", Q, MpscCompact,
                   MpscPadded) {
  Q q;
  REQUIRE(Q::create(smallConfig(), q));
  REQUIRE(q.attachWriter());
  REQUIRE(q.attachReader());

  // Every one of these rounds to a small-looking extent, or overflows outright.
  for (sz_t n : {SIZE_MAX, SIZE_MAX - 1, SIZE_MAX - 62, SIZE_MAX - 63, SIZE_MAX - 64, SIZE_MAX - 70,
                 SIZE_MAX / 2, kTpMaxPayload + 1}) {
    CAPTURE(n);
    WriteSpan const s = q.reserve(n);
    CHECK(s.data() == nullptr);
    CHECK(s.size() == 0);
    CHECK(q.status() == Status::kTooLarge);
  }

  // write() must decline rather than memcpy an absurd length.
  std::byte probe[64]{};
  CHECK_FALSE(q.write(probe, SIZE_MAX));
  CHECK_FALSE(q.write(probe, SIZE_MAX - 62));

  // And the queue is still usable afterwards: a declined request must leave no
  // reservation behind and consume no space.
  REQUIRE(q.write(probe, 24));
  ReadSpan const got = q.peek();
  REQUIRE(got.data() != nullptr);
  CHECK(got.size() == 24);
  q.pop();
  CHECK(q.peek().data() == nullptr);
}

// The bound must be TIGHT, not merely safe: rejecting everything would pass the
// test above while breaking the queue.
TEST_CASE_TEMPLATE("MpscRing still admits every representable size", Q, MpscCompact, MpscPadded) {
  Q q;
  REQUIRE(Q::create(smallConfig(), q));
  REQUIRE(q.attachWriter());
  REQUIRE(q.attachReader());
  std::vector<std::byte> buf(4096);
  for (sz_t i = 0; i < buf.size(); ++i) buf[i] = static_cast<std::byte>(i);
  for (sz_t n : {sz_t{0}, sz_t{1}, sz_t{63}, sz_t{64}, sz_t{65}, sz_t{56}, sz_t{120}}) {
    CAPTURE(n);
    REQUIRE(q.write(buf.data(), n));
    ReadSpan const got = q.peek();
    REQUIRE(got.data() != nullptr);
    CHECK(got.size() == n);
    for (sz_t i = 0; i < n; ++i) CHECK(got[i] == buf[i]);
    q.pop();
  }
}

// ---------------------------------------------------------------------------
// REGRESSION (audit, 2026-07-26): recover() destroyed a live record.
//
// finishClaim() promotes the successor and only THEN vouches, so Claim[p] reads
// kClaimed ACROSS the promotion. An owner killed in that window leaves
// p == kClaimed with q already promoted -- and because this variant lets walkers
// advance past kClaimed, a second writer may legally claim q, fill it, and
// commit. recover()'s unconditional promotion then overwrote that live claim
// word with freeWord(q): the committed record vanished, and a third writer could
// reclaim q over bytes still in use.
//
// The state is FORGED rather than raced. The window is two adjacent stores with
// no externally interceptable boundary; a natural-load attempt at ~273,000
// SIGKILLs over ~16M records never hit it, which is expected for a window of
// tens of nanoseconds against microsecond signal granularity. Forging is the
// only way to test it deterministically, and the forged state is exactly what a
// kill in that window leaves behind.
// ---------------------------------------------------------------------------
TEST_CASE_TEMPLATE("MpscRing: the successor of a kClaimed record is not claimable", Q, MpscCompact,
                   MpscPadded) {
  // This is the invariant that lets recovery promote unconditionally, and it is
  // what the in-band ring got for free by blocking the walk entirely.
  //
  // A record still kClaimed has been kClaimed since its CAS, so no walker can
  // ever have observed it otherwise, so nobody can have claimed its successor.
  // Recovery therefore has nothing at q to destroy and nothing to classify --
  // which is why neither a lap counter nor a retirement stamp is needed.
  //
  // Previously this test asserted the opposite shape: writer B DID claim q, and
  // recovery had to be taught not to destroy it. That protection turned out to
  // be unimplementable at any counter width, so the invariant was restored
  // instead of the guard being strengthened.
  Q q;
  REQUIRE(Q::create(smallConfig(), q));
  REQUIRE(q.attachReader());
  REQUIRE(q.attachWriter());

  Region const& r = q.region();
  u64_t const cells = r.capacity() / kGrain;
  auto claim_at = [&](u64_t pos) {
    u64_t const cell = (pos / kGrain) & (cells - 1);
    return std::atomic_ref<u64_t>(
      *reinterpret_cast<u64_t*>(r.claimPlane(0) + cell * Q::kClaimStride));
  };

  std::byte abuf[24];
  std::memset(abuf, 0xAA, sizeof abuf);
  std::atomic<u32_t> dead_tid{0};
  std::atomic<bool> reserved{false};
  std::thread owner([&] {
    dead_tid.store(currentTid());
    if (!q.attachWriter()) return;
    WriteSpan const s = q.reserve(sizeof abuf);
    if (s.data() == nullptr) return;
    std::memcpy(s.data(), abuf, sizeof abuf);
    reserved.store(true);  // no commit: the owner "dies" holding it
  });
  owner.join();
  REQUIRE(reserved.load());
  REQUIRE(eventually([&] { return !threadAlive(dead_tid.load()); }));

  u64_t const extent = tpExtentFor(sizeof abuf);
  u64_t const p = 0, qpos = p + extent;
  REQUIRE(claim_at(qpos).load() == freeWord(qpos));  // the owner promoted q

  // Forge the kill window faithfully: p back to kClaimed under the dead owner,
  // AND the hint rolled back to p.
  //
  // Rolling the hint back is not cosmetic. finishClaim stores the hint AFTER the
  // vouch, so an owner that dies before vouching never advances it -- leaving the
  // hint advanced past a kClaimed record is a state that cannot occur. Without
  // this, a writer reaches q through the FAST path, which claims straight off the
  // hint and never walks, so the predecessor is never examined and the guard
  // cannot fire. That is also precisely why the fast path needs no check of its
  // own: reaching q from the hint already implies q's predecessor was vouched.
  claim_at(p).store(packRecord(extent, 0, State::kClaimed, dead_tid.load()),
                    std::memory_order_release);
  std::atomic_ref<u64_t>(r.shard(0)->writeHint()).store(p, std::memory_order_release);

  // A writer must NOT be able to take q while p reads kClaimed, even though q
  // is genuinely freeWord(q) and would otherwise be claimable.
  //
  // It BLOCKS rather than failing: reserveSlow restarts, re-walks, and hits the
  // same guard, which is the documented behaviour for a writer held up by an
  // in-flight record. So the block is observed from outside -- q must stay
  // untouched while the reader is not running -- and then released by letting
  // recovery fire. Joining the writer before draining would simply deadlock.
  std::byte bbuf[24];
  std::memset(bbuf, 0xBB, sizeof bbuf);
  std::atomic<bool> took{false};
  std::thread blocked([&] {
    if (!q.attachWriter()) return;
    if (q.write(bbuf, sizeof bbuf)) took.store(true);
  });

  // Observe the block: q stays the frontier while p reads kClaimed.
  auto const observe_until = std::chrono::steady_clock::now() + std::chrono::milliseconds(50);
  while (std::chrono::steady_clock::now() < observe_until) {
    CHECK(claim_at(qpos).load() == freeWord(qpos));
    CHECK_FALSE(took.load());
    std::this_thread::yield();
  }

  // Now let the reader run. Recovery aborts p, which lifts the block -- so the
  // stall is bounded by recovery rather than permanent.
  bool saw_b = false;
  auto const dl = std::chrono::steady_clock::now() + std::chrono::seconds(30);
  while (!saw_b) {
    REQUIRE(std::chrono::steady_clock::now() < dl);
    ReadSpan const rs = q.peek();
    if (rs.data() == nullptr) continue;
    if (rs.size() == sizeof bbuf && rs[0] == static_cast<std::byte>(0xBB)) saw_b = true;
    q.pop();
  }
  blocked.join();
  CHECK(took.load());  // the writer got through once recovery unblocked it
  CHECK(saw_b);
}

TEST_CASE("liveness under fd exhaustion: live reads alive, dead still reads dead") {
  std::atomic<u32_t> live_tid{0};
  std::atomic<bool> release{false};
  std::thread live([&] {
    live_tid.store(currentTid());
    while (!release.load(std::memory_order_acquire)) std::this_thread::yield();
  });
  while (live_tid.load() == 0) std::this_thread::yield();

  std::atomic<u32_t> dead_tid{0};
  std::thread reaped([&] { dead_tid.store(currentTid()); });
  reaped.join();
  // join() proves C++ completion before the kernel must finish removing the
  // task. Establish that the fd-free oracle has converged before making /proc
  // unavailable, or the intentional unknown-is-alive fallback can win this
  // teardown race.
  REQUIRE(eventually([&] { return existenceProbeSaysGone(dead_tid.load()); }));

  // Exhaust the descriptor table. Lower RLIMIT_NOFILE first: on a host with a
  // large limit, opening until natural exhaustion takes long enough that the
  // test would either hang or (worse) give up early and assert nothing, which
  // is how the first version of this test passed vacuously.
  rlimit old_limit{};
  REQUIRE(::getrlimit(RLIMIT_NOFILE, &old_limit) == 0);
  rlimit tight = old_limit;
  tight.rlim_cur = 128;
  bool const lowered = ::setrlimit(RLIMIT_NOFILE, &tight) == 0;

  std::vector<int> hogged;
  errno = 0;
  for (;;) {
    int const fd = ::open("/dev/null", O_RDONLY | O_CLOEXEC);
    if (fd < 0) break;
    hogged.push_back(fd);
    if (hogged.size() > 100000) break;  // guard against an absurd limit
  }
  bool const exhausted = (errno == EMFILE || errno == ENFILE);
  CAPTURE(hogged.size());
  CAPTURE(lowered);
  // The whole point is to run the checks with NO descriptors available. If we
  // could not get there, say so loudly rather than reporting a green vacuum.
  REQUIRE(exhausted);

  bool const live_verdict = threadAlive(live_tid.load());
  bool const dead_verdict = threadAlive(dead_tid.load());

  for (int fd : hogged) ::close(fd);
  ::setrlimit(RLIMIT_NOFILE, &old_limit);
  release.store(true, std::memory_order_release);
  live.join();

  CHECK(live_verdict);        // a live thread must NEVER read as dead
  CHECK_FALSE(dead_verdict);  // and death must still be provable without fds
}

// The COMPLEMENT of the clobber test: recovery must still promote when the owner
// genuinely died before promoting. Getting the guard wrong in this direction is
// silent too -- the successor never becomes the frontier, no writer can claim it,
// and the ring wedges rather than corrupting.
//
// Also forged, and for the same reason: the window is two adjacent stores inside
// finishClaim(). Note the fault suite does NOT cover this branch -- killing a
// child after reserve() returns leaves kCleared, not kClaimed.
TEST_CASE_TEMPLATE("MpscRing recovery promotes a successor the dead owner never reached", Q,
                   MpscCompact, MpscPadded) {
  Q q;
  REQUIRE(Q::create(smallConfig(), q));
  REQUIRE(q.attachReader());
  REQUIRE(q.attachWriter());

  std::atomic<u32_t> dead_tid{0};
  std::thread reaped([&] { dead_tid.store(currentTid()); });
  reaped.join();

  Region const& r = q.region();
  u64_t const cells = r.capacity() / kGrain;
  auto claim_at = [&](u64_t pos) {
    u64_t const cell = (pos / kGrain) & (cells - 1);
    return std::atomic_ref<u64_t>(
      *reinterpret_cast<u64_t*>(r.claimPlane(0) + cell * Q::kClaimStride));
  };

  // Advance one lap so the stale word we plant carries the OPPOSITE parity.
  {
    std::byte fill[24];
    std::memset(fill, 0x11, sizeof fill);
    for (u64_t w = 0; w < r.capacity(); w += tpExtentFor(sizeof fill)) {
      REQUIRE(q.write(fill, sizeof fill));
      REQUIRE(q.peek().data() != nullptr);
      q.pop();
    }
  }

  std::byte abuf[24];
  std::memset(abuf, 0xAA, sizeof abuf);
  // The reservation is taken on a thread that then EXITS without committing.
  // WriterTls is `inline static thread_local` and therefore SHARED across queue
  // instances on a thread, so a reservation abandoned on the main thread leaks
  // into later test cases and trips the "reserve while holding a reservation on
  // another ring" misuse trap there -- passing in isolation, aborting in
  // sequence. Letting the owner thread die also matches the scenario: a killed
  // owner's thread-local state goes with it.
  std::atomic<bool> reserved{false};
  std::thread owner([&] {
    if (!q.attachWriter()) return;
    WriteSpan const s = q.reserve(sizeof abuf);
    if (s.data() == nullptr) return;
    std::memcpy(s.data(), abuf, sizeof abuf);
    reserved.store(true);  // deliberately no commit and no abort
  });
  owner.join();
  REQUIRE(reserved.load());
  u64_t const extent = tpExtentFor(sizeof abuf);
  u64_t const p = r.capacity(), qpos = p + extent;

  // Forge "died BEFORE promoting": p claimed by a dead owner, and q holding a
  // stale previous-lap word rather than freeWord(q).
  claim_at(p).store(packRecord(extent, 0, State::kClaimed, dead_tid.load()),
                    std::memory_order_release);
  claim_at(qpos).store(packRecord(extent, 0, State::kCleared, dead_tid.load()),
                       std::memory_order_release);
  REQUIRE(claim_at(qpos).load() != freeWord(qpos));

  // Recovery must turn q back into the frontier, or this write can never land.
  std::byte bbuf[24];
  std::memset(bbuf, 0xBB, sizeof bbuf);
  bool saw_b = false;
  auto const deadline = std::chrono::steady_clock::now() + std::chrono::seconds(30);
  std::atomic<bool> wrote{false};
  std::thread b([&] {
    if (!q.attachWriter()) return;
    while (std::chrono::steady_clock::now() < deadline && !wrote.load()) {
      if (q.write(bbuf, sizeof bbuf)) wrote.store(true);
    }
  });
  while (!saw_b) {
    REQUIRE(std::chrono::steady_clock::now() < deadline);
    ReadSpan const rs = q.peek();
    if (rs.data() == nullptr) continue;
    if (rs.size() == sizeof bbuf && rs[0] == static_cast<std::byte>(0xBB)) saw_b = true;
    q.pop();
  }
  b.join();
  CHECK(wrote.load());
  CHECK(saw_b);
}

// ---------------------------------------------------------------------------
// REGRESSION (external review, 2026-07-26): two-lap stale Claim defeated the
// lap-parity guard and permanently stranded the frontier.
//
// Only record starts and promoted successors write claim cells, so a cell
// INTERIOR to a large record is untouched for that entire lap. A cell claimed in
// lap 0, skipped in lap 1, and examined in lap 2 carries the same parity in laps
// 0 and 2 -- so recovery misclassified a two-lap-stale word as a live claim,
// declined to promote, and the successor never became the frontier. A wedge, not
// corruption, and no counter width fixes it: a stable record layout skips the
// same cells forever, and a 1 MiB ring turns over thousands of laps per second.
//
// SKIPPED, AND FAILING BY DESIGN -- this documents an OPEN defect.
//
// The structural fix (reader stamps a retirement marker into every retired cell,
// making "non-FREE" mean "claimed this lap" by construction) was implemented and
// REVERTED: it is sound but not viable. With 8-byte cells the reader's stamp
// lands on the same cache line as the writers' active claims, because a
// keeping-up reader sits a few cells behind the frontier. Measured 22.7
// misses/record and 6.0 Mrec/s at four writers, against 1.5 and 28.3 -- it
// erases the entire advantage of splitting the planes.
//
// Left here, skipped, so the counterexample is not lost. Remove the skip when a
// viable fix lands; it should then pass unmodified.
// ---------------------------------------------------------------------------
TEST_CASE("MpscRing: a cell left stale for more than one lap does not strand the ring") {
  Config cfg;
  cfg.capacity = 4096;
  MpscCompact q;
  REQUIRE(MpscCompact::create(cfg, q));
  REQUIRE(q.attachReader());
  REQUIRE(q.attachWriter());

  Region const& r = q.region();
  u64_t const cap = r.capacity(), cells = cap / kGrain;
  auto claim_at = [&](u64_t pos) {
    u64_t const c = (pos / kGrain) & (cells - 1);
    return std::atomic_ref<u64_t>(
      *reinterpret_cast<u64_t*>(r.claimPlane(0) + c * MpscCompact::kClaimStride));
  };
  std::vector<std::byte> buf(4096, std::byte{0x5a});
  auto push_drain = [&](sz_t n) {
    REQUIRE(q.write(buf.data(), n));
    auto const dl = std::chrono::steady_clock::now() + std::chrono::seconds(30);
    for (;;) {
      REQUIRE(std::chrono::steady_clock::now() < dl);
      if (q.peek().data() != nullptr) {
        q.pop();
        return;
      }
    }
  };

  // Lap 0: small records, so position 64 (cell 1) is a real record boundary and
  // ends up holding a genuine non-FREE claim word.
  push_drain(24);  // pos 0,  extent 64
  push_drain(24);  // pos 64, extent 64  <- cell 1 claimed here
  u64_t const lap0_word = claim_at(64).load();
  CHECK(stateOf(lap0_word) != State::kFree);  // a real stale word, not zeros
  push_drain(3968);                           // pos 128, extent 3968 -> 4096

  // Lap 1: one large record spans OVER position 4160 without starting there, so
  // cell 1 is skipped for the whole lap and keeps its lap-0 word.
  push_drain(4032);                         // pos 4096, extent 4032 -> 8128
  CHECK(claim_at(64).load() == lap0_word);  // genuinely skipped

  // Lap 2: position 8256 maps to cell 1. Under the old lap-parity guard the
  // lap-0 word matched lap 2's parity, recovery classified it as a live claim,
  // declined to promote, and the frontier was stranded. The chain check does not
  // depend on parity: a stale word is never kClaimed and its chain never reaches
  // an exact FREE frontier.

  // And the ring keeps working across the boundary that used to strand.
  for (int i = 0; i < 40; ++i) push_drain(56);
  CHECK(q.peek().data() == nullptr);
}
}  // namespace
