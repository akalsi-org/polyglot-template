// Fork-based fault injection for the mpsc queue: writers killed or stopped
// mid-protocol, and the misuse traps that stand between a caller bug and
// silent cross-process corruption.
//
// Kept separate from mpsc_test.cc because fork does not mix with TSan. The
// parent stays single-threaded in every test here, so each fork is safe.
//
// Child processes never touch doctest: they communicate through exit codes and
// signals only, and leave via _exit / a fatal signal.
//
// The design asymmetry under test throughout: a KILLED writer's record is
// recoverable (a dead thread issues no further stores), a STOPPED writer's is
// not (it may resume and scribble over recycled bytes) -- so the reader must
// reclaim the former and must NOT reclaim the latter, with no timeout anywhere.

#include "pgt/mpsc/desc.hh"
#include "pgt/mpsc/queue.hh"

#include <doctest/doctest.h>

#include <signal.h>
#include <sys/wait.h>
#include <unistd.h>

#include <atomic>
#include <chrono>
#include <cstring>
#include <thread>

namespace {

using namespace pgt;
using namespace pgt::mpsc;

struct CountingPolicy : DefaultPolicy {
  std::atomic<u64_t>* reclaims = nullptr;
  void onReclaim(u64_t, u32_t, u64_t) noexcept {
    if (reclaims != nullptr) reclaims->fetch_add(1, std::memory_order_relaxed);
  }
};
using FaultRing = Ring<CountingPolicy>;

Config smallConfig() {
  Config cfg;
  cfg.capacity = 4096;
  return cfg;
}

void pushTagged(FaultRing& q, u32_t tag) {
  std::byte buf[24];
  std::memcpy(buf, &tag, 4);
  for (sz_t i = 4; i < sizeof(buf); ++i) buf[i] = static_cast<std::byte>(tag * 31 + i);
  REQUIRE(q.write(buf, sizeof(buf)));
}

// Drains everything currently deliverable, validating tag order starting at
// `next`. Returns the next expected tag.
u32_t drainTagged(FaultRing& q, u32_t next, u32_t upto) {
  auto const deadline = std::chrono::steady_clock::now() + std::chrono::seconds(30);
  while (next < upto) {
    ReadSpan const r = q.peek();
    if (r.data() == nullptr) {
      REQUIRE(std::chrono::steady_clock::now() < deadline);
      continue;
    }
    u32_t tag;
    REQUIRE(r.size() == 24);
    std::memcpy(&tag, r.data(), 4);
    CHECK(tag == next);
    for (sz_t i = 4; i < r.size(); ++i) {
      REQUIRE(r[i] == static_cast<std::byte>(tag * 31 + i));
    }
    q.pop();
    ++next;
  }
  return next;
}

// ---------------------------------------------------------------------------
// A writer SIGKILLed between reserve() and commit(): the reader reclaims its
// record, and everything committed before and after is delivered exactly once.
// ---------------------------------------------------------------------------
TEST_CASE("killed mid-reservation writer is reclaimed; no loss, no duplication") {
  // Static queue storage in every test here: Ring's writer TLS identifies its
  // queue by address, and stack-frame reuse across tests leaks a stale
  // read_cache into a fresh queue (see the REGRESSION test at the bottom).
  static std::atomic<u64_t> reclaims{0};
  static CountingPolicy pol{{}, &reclaims};
  static FaultRing q{pol};
  REQUIRE(FaultRing::create(smallConfig(), q));
  REQUIRE(q.attachReader());
  REQUIRE(q.attachWriter());  // registers the atfork handler before any fork

  u32_t tag = 0;
  for (; tag < 5; ++tag) pushTagged(q, tag);

  pid_t const pid = fork();
  REQUIRE(pid >= 0);
  if (pid == 0) {
    // Child: claim, half-fill, die without committing. The mapping is shared;
    // the atfork handler cleared the inherited reservation state, so this is a
    // fresh writer of a separate process.
    FaultRing& cq = q;
    if (!cq.attachWriter()) _exit(1);
    WriteSpan const s = cq.reserve(100);
    if (s.data() == nullptr) _exit(2);
    std::memset(s.data(), 0x5a, 50);
    raise(SIGKILL);
    _exit(3);  // unreachable
  }
  int st = 0;
  REQUIRE(waitpid(pid, &st, 0) == pid);
  REQUIRE(WIFSIGNALED(st));
  REQUIRE(WTERMSIG(st) == SIGKILL);

  // Writers can still make progress past the dead record only after the reader
  // reclaims it; push after the kill to prove admission recovers.
  for (; tag < 10; ++tag) pushTagged(q, tag);
  drainTagged(q, 0, 10);
  CHECK(reclaims.load() == 1);
  CHECK(q.peek().data() == nullptr);
}

// ---------------------------------------------------------------------------
// Several writers killed at different points, interleaved with successful
// commits, wrapping the ring: the reader delivers every committed record in
// order and reclaims exactly the dead reservations.
// ---------------------------------------------------------------------------
TEST_CASE("repeated writer kills across wraps") {
  static std::atomic<u64_t> reclaims{0};
  static CountingPolicy pol{{}, &reclaims};
  static FaultRing q{pol};  // static: see the first test
  REQUIRE(FaultRing::create(smallConfig(), q));
  REQUIRE(q.attachReader());
  REQUIRE(q.attachWriter());

  u32_t tag = 0;
  u64_t kills = 0;
  for (int round = 0; round < 40; ++round) {  // 40 * ~6 records of extent 64+: many laps
    for (int k = 0; k < 3; ++k) pushTagged(q, tag++);
    pid_t const pid = fork();
    REQUIRE(pid >= 0);
    if (pid == 0) {
      FaultRing& cq = q;
      if (!cq.attachWriter()) _exit(1);
      WriteSpan const s = cq.reserve(static_cast<sz_t>(round % 3) * 64 + 16);
      if (s.data() == nullptr) _exit(2);
      raise(SIGKILL);
      _exit(3);
    }
    int st = 0;
    REQUIRE(waitpid(pid, &st, 0) == pid);
    REQUIRE(WIFSIGNALED(st));
    ++kills;
    for (int k = 0; k < 3; ++k) pushTagged(q, tag++);
    tag = drainTagged(q, tag - 6, tag);
  }
  CHECK(reclaims.load() == kills);
  CHECK(q.peek().data() == nullptr);
}

// ---------------------------------------------------------------------------
// A SIGSTOPped writer is ALIVE: the reader must not reclaim it, ever. On
// SIGCONT the writer finishes and its record arrives intact.
// ---------------------------------------------------------------------------
TEST_CASE("stopped writer is never reclaimed and resumes cleanly on SIGCONT") {
  static std::atomic<u64_t> reclaims{0};
  static CountingPolicy pol{{}, &reclaims};
  static FaultRing q{pol};  // static: see the first test
  REQUIRE(FaultRing::create(smallConfig(), q));
  REQUIRE(q.attachReader());
  REQUIRE(q.attachWriter());

  pushTagged(q, 0);

  pid_t const pid = fork();
  REQUIRE(pid >= 0);
  if (pid == 0) {
    FaultRing& cq = q;
    if (!cq.attachWriter()) _exit(1);
    WriteSpan const s = cq.reserve(24);
    if (s.data() == nullptr) _exit(2);
    raise(SIGSTOP);  // returns after SIGCONT
    u32_t const tag = 1;
    std::memcpy(s.data(), &tag, 4);
    for (sz_t i = 4; i < 24; ++i) s[i] = static_cast<std::byte>(tag * 31 + i);
    cq.commit(24);
    _exit(0);
  }
  int st = 0;
  REQUIRE(waitpid(pid, &st, WUNTRACED) == pid);  // reserve done, child stopped
  REQUIRE(WIFSTOPPED(st));

  // Record 0 is deliverable; the child's in-flight record pins the reader
  // there. Poll well past any plausible transient: the reader must report
  // "busy", never reclaim, and never deliver the unfinished record.
  drainTagged(q, 0, 1);
  for (int i = 0; i < 200; ++i) {
    CHECK(q.peek().data() == nullptr);
    std::this_thread::sleep_for(std::chrono::milliseconds(1));
  }
  CHECK(reclaims.load() == 0);

  REQUIRE(kill(pid, SIGCONT) == 0);
  REQUIRE(waitpid(pid, &st, 0) == pid);
  REQUIRE(WIFEXITED(st));
  REQUIRE(WEXITSTATUS(st) == 0);
  drainTagged(q, 1, 2);
  CHECK(reclaims.load() == 0);
}

// ---------------------------------------------------------------------------
// Misuse traps. Each is a silent-corruption path if the trap is missing, so
// each test proves the process dies (SIGABRT) rather than corrupting.
// ---------------------------------------------------------------------------
namespace {
template <typename F>
int runExpectingAbort(F&& f) {
  pid_t const pid = fork();
  if (pid == 0) {
    // Quiet the trap's stderr line so test output stays readable.
    int const null = open("/dev/null", O_WRONLY);
    if (null >= 0) dup2(null, 2);
    f();
    _exit(0);  // reaching here means the trap did NOT fire
  }
  int st = 0;
  waitpid(pid, &st, 0);
  return st;
}
}  // namespace

TEST_CASE("misuse traps abort the process instead of corrupting") {
  static FaultRing q;  // static: see the first test; created once across SUBCASEs
  static bool const created = FaultRing::create(smallConfig(), q);
  static bool const attached = created && q.attachWriter();
  REQUIRE(created);
  REQUIRE(attached);  // atfork handler in place before the forks below

  SUBCASE("double commit") {
    int const st = runExpectingAbort([&] {
      WriteSpan const s = q.reserve(16);
      if (s.data() == nullptr) _exit(9);
      q.commit(16);
      q.commit(16);  // must trap: the delayed-stamper defect via the API
    });
    CHECK(WIFSIGNALED(st));
    CHECK(WTERMSIG(st) == SIGABRT);
  }

  SUBCASE("grown commit") {
    int const st = runExpectingAbort([&] {
      WriteSpan const s = q.reserve(16);
      if (s.data() == nullptr) _exit(9);
      q.commit(500);  // extent grows past the reservation: fictitious boundary
    });
    CHECK(WIFSIGNALED(st));
    CHECK(WTERMSIG(st) == SIGABRT);
  }

  SUBCASE("commit from a forked child") {
    int const st = runExpectingAbort([&] {
      WriteSpan const s = q.reserve(16);
      if (s.data() == nullptr) _exit(9);
      pid_t const gc = fork();
      if (gc == 0) {
        q.commit(16);  // child's reservation state was cleared by atfork: trap
        _exit(0);
      }
      int gst = 0;
      waitpid(gc, &gst, 0);
      q.abort();  // leave the ring clean
      // Re-encode the grandchild's fate as this child's own.
      if (WIFSIGNALED(gst) && WTERMSIG(gst) == SIGABRT) raise(SIGABRT);
      _exit(0);
    });
    CHECK(WIFSIGNALED(st));
    CHECK(WTERMSIG(st) == SIGABRT);
  }

  SUBCASE("abort without a reservation") {
    int const st = runExpectingAbort([&] { q.abort(); });
    CHECK(WIFSIGNALED(st));
    CHECK(WTERMSIG(st) == SIGABRT);
  }
}

// ---------------------------------------------------------------------------
// A dead reader turns permanent backpressure into a definite error.
// ---------------------------------------------------------------------------
TEST_CASE("writer distinguishes a dead reader from a slow one") {
  static FaultRing q;  // static: see the first test
  REQUIRE(FaultRing::create(smallConfig(), q));

  // A reader attaches from a thread that then exits: its tid goes dead.
  std::thread reader([&] {
    Ring<CountingPolicy> view(q.region(), 0);
    REQUIRE(view.attachReader());
  });
  reader.join();

  std::byte buf[64] = {};
  while (q.write(buf, sizeof(buf))) {
  }
  // Full AND the recorded reader is provably dead: kReaderDead, not kFull.
  CHECK(q.status() == Status::kReaderDead);
}

}  // namespace

// ---------------------------------------------------------------------------
// KNOWN BUG (reported): Ring's WriterTls identifies its owning queue by
// pointer. A queue constructed at the address of a destroyed one -- guaranteed
// here by running the same noinline frame twice -- inherits the previous
// queue's read_cache, which then LEADS the new ring's reader. That violates
// read_cache's own contract ("may lag, never leads"): reserveSlow treats every
// walk as stale and spins forever, and the same stale value reaching the
// admission check can over-admit into unconsumed records. may_fail: this
// documents the defect without blocking the suite; it flips to passing when
// queue identity gains a generation/epoch.
// ---------------------------------------------------------------------------
namespace {
[[gnu::noinline]] void tlsEpisode(int rounds) {
  Mpsc q;
  if (!Mpsc::create(smallConfig(), q)) _exit(10);
  if (!q.attachReader()) _exit(11);
  std::byte buf[64] = {};
  for (int i = 0; i < rounds; ++i) {  // push far past one lap so the full
    while (!q.write(buf, sizeof(buf))) {  // path refreshes read_cache
    }
    if (q.peek().data() != nullptr) q.pop();
  }
}
}  // namespace

TEST_CASE("REGRESSION: queue recreated at a reused address inherits stale writer TLS"
          * doctest::may_fail()) {
  pid_t const pid = fork();
  REQUIRE(pid >= 0);
  if (pid == 0) {
    tlsEpisode(400);  // leaves tls_: owner = frame-local address, read_cache ~25K
    alarm(5);         // with the bug, the single write below never returns
    tlsEpisode(1);
    _exit(0);
  }
  int st = 0;
  REQUIRE(waitpid(pid, &st, 0) == pid);
  bool const clean_exit = WIFEXITED(st) && WEXITSTATUS(st) == 0;
  CHECK_MESSAGE(clean_exit, "writer hung on a fresh empty ring: stale WriterTls read_cache");
}
