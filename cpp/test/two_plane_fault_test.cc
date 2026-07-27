// Fork-based fault injection for the EXPERIMENTAL two-plane MPSC variant.
//
// Kept separate from two_plane_test.cc because fork does not mix with TSan. The
// parent stays single-threaded in every test here, so each fork is safe.
//
// Child processes never touch doctest: they communicate through exit codes and
// signals only, and leave via _exit / a fatal signal.
//
// The asymmetry under test, unchanged from Ring: a KILLED writer's record is
// recoverable (a dead thread issues no further stores), a STOPPED writer's is
// not (it may resume and scribble over recycled bytes) -- so the reader must
// reclaim the former and must NOT reclaim the latter, with no timeout anywhere.
//
// Two paths are specific to this variant and are exercised deliberately:
//   * death BETWEEN the claim CAS and the successor promotion (kClaimed), where
//     recovery must promote the successor cell itself;
//   * death AFTER the payload is complete but BEFORE the Result tag is
//     published -- and its converse, death immediately AFTER the tag, where the
//     reader must DELIVER rather than recover. That is the r5 stale-snapshot
//     defect class, re-checked against the split planes.

#include "pgt/mpsc/desc.hh"
#include "pgt/mpsc/two_plane.hh"

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
using FaultRing = TwoPlaneRing<CountingPolicy, false>;

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
TEST_CASE("two-plane: killed mid-reservation writer is reclaimed") {
  std::atomic<u64_t> reclaims{0};
  CountingPolicy pol;
  pol.reclaims = &reclaims;
  FaultRing q{pol};
  REQUIRE(FaultRing::create(smallConfig(), q));
  REQUIRE(q.attachReader());
  REQUIRE(q.attachWriter());  // registers the atfork handler before any fork

  u32_t tag = 0;
  for (; tag < 5; ++tag) pushTagged(q, tag);

  pid_t const pid = fork();
  REQUIRE(pid >= 0);
  if (pid == 0) {
    FaultRing& cq = q;
    if (!cq.attachWriter()) _exit(1);
    WriteSpan const s = cq.reserve(100);
    if (s.data() == nullptr) _exit(2);
    std::memset(s.data(), 0x5a, 50);  // payload half-written, Result never tagged
    raise(SIGKILL);
    _exit(3);
  }
  int st = 0;
  REQUIRE(waitpid(pid, &st, 0) == pid);
  REQUIRE(WIFSIGNALED(st));
  REQUIRE(WTERMSIG(st) == SIGKILL);

  for (; tag < 10; ++tag) pushTagged(q, tag);
  drainTagged(q, 0, 10);
  CHECK(reclaims.load() == 1);
  CHECK(q.peek().data() == nullptr);
}

// ---------------------------------------------------------------------------
// Death between the claim CAS and the successor promotion. The child cannot be
// stopped between two adjacent stores from outside, so it is stopped by
// construction: it forks a grandchild that is killed while its parent holds
// nothing, then the child itself is killed immediately after reserve() returns.
// Recovery must promote the successor cell -- if it does not, no later claim
// can ever succeed and the queue wedges rather than corrupting.
// ---------------------------------------------------------------------------
TEST_CASE("two-plane: recovery promotes the successor cell of a killed claimant") {
  std::atomic<u64_t> reclaims{0};
  CountingPolicy pol;
  pol.reclaims = &reclaims;
  FaultRing q{pol};
  REQUIRE(FaultRing::create(smallConfig(), q));
  REQUIRE(q.attachReader());
  REQUIRE(q.attachWriter());

  std::byte before[8]{std::byte{0x11}};
  REQUIRE(q.write(before, sizeof(before)));

  pid_t const pid = fork();
  REQUIRE(pid >= 0);
  if (pid == 0) {
    if (!q.attachWriter()) _exit(1);
    WriteSpan const s = q.reserve(80);
    if (s.data() == nullptr) _exit(2);
    std::memset(s.data(), 0x5a, s.size());
    raise(SIGKILL);
    _exit(3);
  }
  int st = 0;
  REQUIRE(waitpid(pid, &st, 0) == pid);
  REQUIRE(WIFSIGNALED(st));

  // NOTE ON WHAT THIS DOES AND DOES NOT COVER. The child is killed AFTER
  // reserve() returns, so finishClaim() already ran to completion: it promoted
  // the successor AND vouched, leaving Claim[p] == kCleared. recover() therefore
  // takes its non-promoting branch here. The successor is claimable because the
  // dead WRITER promoted it, not because recovery did.
  //
  // The kClaimed branch of recover() -- the case where the owner died before
  // promoting -- cannot be reached by killing a child at an API boundary, since
  // the window is two adjacent stores inside finishClaim(). It is covered by the
  // forged-state test in two_plane_test.cc instead.
  std::byte after[8]{std::byte{0x22}};
  REQUIRE(q.write(after, sizeof(after)));
  auto const deadline = std::chrono::steady_clock::now() + std::chrono::seconds(30);
  for (unsigned delivered = 0; delivered < 2;) {
    ReadSpan const r = q.peek();
    if (r.data() == nullptr) {
      REQUIRE(std::chrono::steady_clock::now() < deadline);
      continue;
    }
    REQUIRE(r.size() == sizeof(before));
    CHECK(r[0] == (delivered == 0 ? std::byte{0x11} : std::byte{0x22}));
    q.pop();
    ++delivered;
  }
  CHECK(reclaims.load() == 1);
  CHECK(q.peek().data() == nullptr);
}

// ---------------------------------------------------------------------------
// A writer that COMMITS and then dies owns a delivered record, not a
// recoverable one. This is the r5 stale-snapshot defect class against the split
// planes: peek() snapshots the Claim word (still kCleared, since commit never
// rewrites it) and only Result distinguishes the two cases, so the Result tag
// MUST be re-read after the liveness verdict.
// ---------------------------------------------------------------------------
TEST_CASE("two-plane: a committed-then-dead writer's record is delivered, not reclaimed") {
  std::atomic<u64_t> reclaims{0};
  CountingPolicy pol;
  pol.reclaims = &reclaims;
  FaultRing q{pol};
  REQUIRE(FaultRing::create(smallConfig(), q));
  REQUIRE(q.attachReader());
  REQUIRE(q.attachWriter());

  for (int round = 0; round < 50; ++round) {
    pid_t const pid = fork();
    REQUIRE(pid >= 0);
    if (pid == 0) {
      if (!q.attachWriter()) _exit(1);
      WriteSpan const s = q.reserve(24);
      if (s.data() == nullptr) _exit(2);
      u32_t const tag = 0xfeed;
      std::memcpy(s.data(), &tag, 4);
      for (sz_t i = 4; i < 24; ++i) s[i] = static_cast<std::byte>(i);
      q.commit(s, 24);
      raise(SIGKILL);  // dead the instant after publication
      _exit(3);
    }
    int st = 0;
    REQUIRE(waitpid(pid, &st, 0) == pid);
    REQUIRE(WIFSIGNALED(st));

    auto const deadline = std::chrono::steady_clock::now() + std::chrono::seconds(30);
    ReadSpan r{};
    while ((r = q.peek()).data() == nullptr) {
      REQUIRE(std::chrono::steady_clock::now() < deadline);
    }
    REQUIRE(r.size() == 24);
    u32_t got = 0;
    std::memcpy(&got, r.data(), 4);
    CHECK(got == 0xfeed);
    for (sz_t i = 4; i < r.size(); ++i) REQUIRE(r[i] == static_cast<std::byte>(i));
    q.pop();
  }
  CHECK(reclaims.load() == 0);  // nothing was recovered; everything was delivered
  CHECK(q.peek().data() == nullptr);
}

// ---------------------------------------------------------------------------
// Repeated kills across many wraps, with real claim/commit traffic in between.
// ---------------------------------------------------------------------------
TEST_CASE("two-plane: repeated writer kills across wraps") {
  std::atomic<u64_t> reclaims{0};
  CountingPolicy pol;
  pol.reclaims = &reclaims;
  FaultRing q{pol};
  REQUIRE(FaultRing::create(smallConfig(), q));
  REQUIRE(q.attachReader());
  REQUIRE(q.attachWriter());

  u32_t tag = 0;
  u64_t kills = 0;
  for (int round = 0; round < 40; ++round) {
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
// A SIGSTOPped writer is ALIVE: the reader must not reclaim it, ever. Time is
// never evidence of death. On SIGCONT the writer finishes and its record
// arrives intact.
// ---------------------------------------------------------------------------
TEST_CASE("two-plane: stopped writer is never reclaimed and resumes cleanly") {
  std::atomic<u64_t> reclaims{0};
  CountingPolicy pol;
  pol.reclaims = &reclaims;
  FaultRing q{pol};
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
    cq.commit(s, 24);
    _exit(0);
  }
  int st = 0;
  REQUIRE(waitpid(pid, &st, WUNTRACED) == pid);
  REQUIRE(WIFSTOPPED(st));

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
// A stopped writer must not block LATER claims -- only delivery. This is the
// two-plane walk property under a real stopped process rather than a thread.
// ---------------------------------------------------------------------------
TEST_CASE("two-plane: a stopped writer blocks the reader but not other writers") {
  std::atomic<u64_t> reclaims{0};
  CountingPolicy pol;
  pol.reclaims = &reclaims;
  FaultRing q{pol};
  REQUIRE(FaultRing::create(smallConfig(), q));
  REQUIRE(q.attachReader());
  REQUIRE(q.attachWriter());

  pid_t const pid = fork();
  REQUIRE(pid >= 0);
  if (pid == 0) {
    FaultRing& cq = q;
    if (!cq.attachWriter()) _exit(1);
    WriteSpan const s = cq.reserve(24);
    if (s.data() == nullptr) _exit(2);
    raise(SIGSTOP);
    cq.abort();
    _exit(0);
  }
  int st = 0;
  REQUIRE(waitpid(pid, &st, WUNTRACED) == pid);
  REQUIRE(WIFSTOPPED(st));

  // Claims behind the stalled reservation still succeed.
  for (u32_t tag = 100; tag < 120; ++tag) pushTagged(q, tag);
  CHECK(q.peek().data() == nullptr);  // but nothing is deliverable
  CHECK(reclaims.load() == 0);

  REQUIRE(kill(pid, SIGCONT) == 0);
  REQUIRE(waitpid(pid, &st, 0) == pid);
  REQUIRE(WIFEXITED(st));
  drainTagged(q, 100, 120);  // the aborted record is skipped, the rest arrive
  CHECK(reclaims.load() == 0);
  CHECK(q.peek().data() == nullptr);
}

// ---------------------------------------------------------------------------
// Cross-process attach by descriptor. This is what moving the planes into the
// region bought: previously they lived in a private MAP_SHARED|MAP_ANONYMOUS
// mapping that only fork() could inherit, so an unrelated process could not
// participate at all.
//
// The child does NOT use the inherited object. It calls attach() on a fresh
// ring, which mmaps the region independently from the descriptor -- so a plane
// pointer that only happened to be valid via inheritance would fail here.
// ---------------------------------------------------------------------------
TEST_CASE("two-plane: a process attaching by fd alone can write") {
  FaultRing q;
  REQUIRE(FaultRing::create(smallConfig(), q));
  REQUIRE(q.attachReader());
  REQUIRE(q.attachWriter());

  u32_t tag = 0;
  for (; tag < 5; ++tag) pushTagged(q, tag);

  int const fd = q.region().fd();
  REQUIRE(fd >= 0);

  pid_t const pid = fork();
  REQUIRE(pid >= 0);
  if (pid == 0) {
    // Child: a wholly separate ring object over the same descriptor.
    FaultRing cq;
    if (!FaultRing::attach(fd, cq)) _exit(1);
    if (!cq.attachWriter()) _exit(2);
    for (u32_t t = 5; t < 10; ++t) {
      std::byte buf[24];
      std::memcpy(buf, &t, 4);
      for (sz_t i = 4; i < sizeof(buf); ++i) buf[i] = static_cast<std::byte>(t * 31 + i);
      if (!cq.write(buf, sizeof(buf))) _exit(3);
    }
    _exit(0);
  }
  int st = 0;
  REQUIRE(waitpid(pid, &st, 0) == pid);
  REQUIRE(WIFEXITED(st));
  REQUIRE(WEXITSTATUS(st) == 0);

  // Every record, from both processes, in reservation order.
  drainTagged(q, 0, 10);
  CHECK(q.peek().data() == nullptr);
}

// A binary built for one cell layout must not attach to a region created with
// the other: it would index the Claim plane with the wrong stride and read a
// neighbouring record's ownership word as its own. The strides are recorded in
// the region header precisely so this is detectable rather than silent.
TEST_CASE("two-plane: attaching with a mismatched cell layout is refused") {
  using Compact = TwoPlaneRing<DefaultPolicy, false>;
  using Padded = TwoPlaneRing<DefaultPolicy, true>;

  Compact compact;
  REQUIRE(Compact::create(smallConfig(), compact));
  Padded wrong;
  CHECK_FALSE(Padded::attach(compact.region().fd(), wrong));

  Padded padded;
  REQUIRE(Padded::create(smallConfig(), padded));
  Compact wrong2;
  CHECK_FALSE(Compact::attach(padded.region().fd(), wrong2));

  // The matching layout still attaches, so the check rejects mismatch rather
  // than rejecting everything.
  Compact ok;
  CHECK(Compact::attach(compact.region().fd(), ok));
}

}  // namespace
