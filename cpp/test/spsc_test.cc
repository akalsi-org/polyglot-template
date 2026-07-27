// SpscRing tests: the surviving single-producer variant.
//
// One writer per ring, no claim arbitration, wait-free. Sole ownership is
// arbitrated through the Region's shared writer registry, so the claim is valid
// cross-process -- which is why the ownership cases here fork rather than just
// spawning a thread.
//
// Fork conventions, as elsewhere in this suite: the parent stays
// single-threaded, children never touch doctest and leave via _exit or a fatal
// signal, and a child expected to abort silences BOTH stdout and stderr because
// doctest's inherited SIGABRT handler narrates a partial summary otherwise.

#include "pgt/core/platform.hh"
#include "pgt/mpsc/desc.hh"
#include "pgt/mpsc/queue.hh"
#include "pgt/mpsc/region.hh"

#include <doctest/doctest.h>

#include <fcntl.h>
#include <signal.h>
#include <sys/wait.h>
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

std::byte patternByte(u32_t writer, u32_t seq, sz_t i) {
  return static_cast<std::byte>(writer * 151u + seq * 29u + static_cast<u32_t>(i) * 7u + 3u);
}

struct Deadline {
  std::chrono::steady_clock::time_point end =
    std::chrono::steady_clock::now() + std::chrono::seconds(60);
  [[nodiscard]] bool expired() const { return std::chrono::steady_clock::now() > end; }
};

Config tinyConfig(u32_t shards) {
  Config cfg;
  cfg.capacity = 4096;  // wraps every ~63 minimal records, per shard
  cfg.shards = shards;
  return cfg;
}

// Runs `f` in a child that is expected to abort, and returns its wait status.
//
// The child silences BOTH streams: stderr carries the trap's diagnostic, and
// stdout carries doctest's inherited SIGABRT handler narrating "test case
// CRASHED" plus a partial "Status: FAILURE!" summary from a run that was never
// going to finish. _exit discipline cannot prevent that -- a signal handler is
// not an atexit handler -- so the handler is also reset to SIG_DFL.
template <typename F>
int runExpectingAbort(F&& f) {
  pid_t const pid = fork();
  if (pid == 0) {
    int const null = open("/dev/null", O_WRONLY);
    if (null >= 0) {
      dup2(null, 1);
      dup2(null, 2);
    }
    signal(SIGABRT, SIG_DFL);
    f();
    _exit(0);  // reaching here means the trap did NOT fire
  }
  int st = 0;
  waitpid(pid, &st, 0);
  return st;
}

Config smallConfig() {
  Config cfg;
  cfg.capacity = 4096;  // one page: wraps every ~63 minimal records
  return cfg;
}

TEST_CASE("Spsc: direct short commit and zero-length write") {
  Spsc q;
  REQUIRE(Spsc::create(smallConfig(), q));
  REQUIRE(q.attachWriter());
  REQUIRE(q.attachReader());

  WriteSpan const s = q.reserve(31);
  REQUIRE(s.data() != nullptr);
  std::memset(s.data(), 0x5a, 17);
  q.commit(s, 17);
  ReadSpan r = q.peek();
  REQUIRE(r.data() != nullptr);
  CHECK(r.size() == 17);
  CHECK(r[0] == std::byte{0x5a});
  q.pop();

  REQUIRE(q.write(nullptr, 0));
  r = q.peek();
  REQUIRE(r.data() != nullptr);
  CHECK(r.size() == 0);
  q.pop();

  // A second view cannot take over live writer-private cursors. Explicitly
  // detach the first owner before transferring the shared slot to a fresh view.
  SpscRing<> fresh(q.region(), 0);
  CHECK(!fresh.attachWriter());
  q.detachWriter();
  REQUIRE(fresh.attachWriter());
  // The detached view is stale even though both views have the same thread id.
  // It must not regain ownership while `fresh` owns the shared slot.
  CHECK(!q.attachWriter());
  REQUIRE(fresh.write(nullptr, 0));
  r = q.peek();
  REQUIRE(r.data() != nullptr);
  CHECK(r.size() == 0);
  q.pop();
  fresh.detachWriter();
  REQUIRE(q.attachWriter());
  q.detachWriter();
}

// ---------------------------------------------------------------------------
// Region.
// ---------------------------------------------------------------------------

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

TEST_CASE("standalone Spsc rejects inherited writer ownership") {
  Spsc q;
  REQUIRE(Spsc::create(smallConfig(), q));
  REQUIRE(q.attachWriter());

  WriteSpan const s = q.reserve(16);
  REQUIRE(s.data() != nullptr);
  int const st = runExpectingAbort([&] { q.commit(s, 16); });
  CHECK(WIFSIGNALED(st));
  CHECK(WTERMSIG(st) == SIGABRT);

  // The child must not affect the parent's writer-private reservation.
  q.abort();
  WriteSpan const next = q.reserve(0);
  REQUIRE(next.data() != nullptr);
  q.commit(next, 0);
  q.detachWriter();
}

// ---------------------------------------------------------------------------
// A dead reader turns permanent backpressure into a definite error.
// ---------------------------------------------------------------------------

// REGRESSION (found by audit, 2026-07-26): the same unchecked extent overflow
// as two-plane, but with a worse landing. extentFor adds an 8-byte header
// BEFORE rounding, so extentFor(SIZE_MAX - 8) == 0 exactly -- a ZERO extent.
// The admission test is `need > max_need_`, which zero passes trivially because
// zero is small. A zero extent also violates the encoding contract that an
// extent is never zero, and a reader advancing by zero does not terminate.
TEST_CASE("Spsc declines unrepresentable lengths and never yields a zero extent") {
  Spsc q;
  REQUIRE(Spsc::create(smallConfig(), q));
  REQUIRE(q.attachWriter());
  REQUIRE(q.attachReader());

  for (sz_t n : {SIZE_MAX, SIZE_MAX - 7, SIZE_MAX - 8, SIZE_MAX - 9, SIZE_MAX - 63, SIZE_MAX - 70,
                 kMaxPayload + 1}) {
    CAPTURE(n);
    WriteSpan const s = q.reserve(n);
    CHECK(s.data() == nullptr);
    CHECK(s.size() == 0);
    CHECK(q.status() == Status::kTooLarge);
  }

  std::byte probe[64]{};
  CHECK_FALSE(q.write(probe, SIZE_MAX - 8));

  // Still usable, and still delivering exact lengths.
  REQUIRE(q.write(probe, 24));
  ReadSpan const got = q.peek();
  REQUIRE(got.data() != nullptr);
  CHECK(got.size() == 24);
  q.pop();
}

// REGRESSION (Gemini review, 2026-07-26): the slot generation was write-only.
//
// attachWriter() bumped it on takeover with the comment "fence any stale actor
// still holding the old {slot, generation} pair" -- and nothing anywhere read
// it, so that fence did not exist. A documented protection that is not
// implemented is worse than none: the next change reasonably relies on it.
//
// The bump is forged here because a real takeover requires the incumbent to be
// PROVEN dead, which cannot be arranged for the calling thread. What is under
// test is that ownsWriter() consults the generation at all -- if it does, an
// incarnation change invalidates this view and the misuse trap fires.
TEST_CASE("Spsc ownership is fenced by the slot generation, not just the tid") {
  Spsc q;
  REQUIRE(Spsc::create(smallConfig(), q));
  REQUIRE(q.attachWriter());
  REQUIRE(q.attachReader());

  std::byte buf[24];
  std::memset(buf, 0x33, sizeof buf);
  REQUIRE(q.write(buf, sizeof buf));  // ownership is good before any bump

  // Everything happens INSIDE the child, on its own queue. The obvious version
  // of this test -- bump the generation here, then write in a forked child --
  // passes for the wrong reason: the atfork handler clears writer TLS, so the
  // child traps on the tid check whether or not the generation is consulted at
  // all. It reported a mutant with the generation check deleted as caught.
  int const st = runExpectingAbort([&] {
    Spsc c;
    if (!Spsc::create(smallConfig(), c)) _exit(9);
    if (!c.attachWriter()) _exit(9);
    std::byte local[24];
    std::memset(local, 0x44, sizeof local);
    if (!c.write(local, sizeof local)) _exit(9);  // ownership good here
    // Advance the slot to a new incarnation behind this view's back.
    std::atomic_ref<u32_t>(c.region().writerSlots()->generation)
      .fetch_add(1, std::memory_order_acq_rel);
    (void)c.write(local, sizeof local);  // must trap iff generation is consulted
  });
  CHECK(WIFSIGNALED(st));
  CHECK(WTERMSIG(st) == SIGABRT);
}
}  // namespace
