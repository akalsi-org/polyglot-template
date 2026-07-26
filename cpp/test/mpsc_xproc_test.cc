// CROSS-PROCESS fault injection: real processes, real signals, the memfd
// shared by descriptor. Unlike mpsc_fault_test.cc, children here never reuse
// the parent's queue object -- each one calls attach(fd) and builds its OWN
// Region mapping and queue instance, which is the deployment the design
// exists for and a code path (magic/version verification, fresh cursors,
// separate TLS and members) that fork-with-inherited-state never executes.
//
// Children communicate through pipes, exit codes, and signals only; doctest
// assertions live in the parent. Every child arms an alarm so a protocol bug
// shows up as a failed test, not a hung suite.
//
// Covered here and nowhere else:
//   * fresh-process attach for writers and readers on both Mpsc and MultiSpsc
//   * zombies count as dead (unreaped writer, reader must still recover)
//   * reader restart: takeover by a fresh process, resume at read_pos,
//     at-least-once redelivery, exclusive attach while the holder lives
//   * MultiSpsc slot recycling gates: dead-but-undrained ring must NOT be
//     recycled; drained ring must be; generation bumps on recycle
//   * fork with an inherited reservation on the sharded SPSC path: the child
//     traps instead of committing, then wins its OWN slot and writes

#include "pgt/mpsc/queue.hh"
#include "pgt/mpsc/region.hh"

#include <doctest/doctest.h>

#include <fcntl.h>
#include <signal.h>
#include <sys/wait.h>
#include <unistd.h>

#include <atomic>
#include <chrono>
#include <cstdio>
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
using XRing = Ring<CountingPolicy>;

Config smallConfig(u32_t shards = 1) {
  Config cfg;
  cfg.capacity = 4096;
  cfg.shards = shards;
  return cfg;
}

constexpr sz_t kRecLen = 24;

template <typename Q>
void fillTag(WriteSpan s, u32_t tag) {
  std::memcpy(s.data(), &tag, 4);
  for (sz_t i = 4; i < kRecLen; ++i) s[i] = static_cast<std::byte>(tag * 31 + i);
}

// Child-side write; no doctest. Returns false on any failure.
template <typename Q>
bool childWriteTag(Q& q, u32_t tag) {
  WriteSpan const s = q.reserve(kRecLen);
  if (s.data() == nullptr) return false;
  fillTag<Q>(s, tag);
  q.commit(kRecLen);
  return true;
}

template <typename Q>
void pushTag(Q& q, u32_t tag) {
  WriteSpan const s = q.reserve(kRecLen);
  REQUIRE(s.data() != nullptr);
  fillTag<Q>(s, tag);
  q.commit(kRecLen);
}

// Parent-side: pops the next record within the deadline and returns its tag,
// validating the fill. REQUIREs on timeout.
template <typename Q>
u32_t popTag(Q& q) {
  auto const deadline = std::chrono::steady_clock::now() + std::chrono::seconds(30);
  for (;;) {
    ReadSpan const r = q.peek();
    if (r.data() == nullptr) {
      REQUIRE(std::chrono::steady_clock::now() < deadline);
      continue;
    }
    REQUIRE(r.size() == kRecLen);
    u32_t tag;
    std::memcpy(&tag, r.data(), 4);
    for (sz_t i = 4; i < r.size(); ++i) {
      REQUIRE(r[i] == static_cast<std::byte>(tag * 31 + i));
    }
    q.pop();
    return tag;
  }
}

// Child-side pop with retry; returns false on timeout/mismatch of length.
template <typename Q>
bool childPopTag(Q& q, u32_t* tag_out, bool pop = true) {
  for (int i = 0; i < 20'000'000; ++i) {
    ReadSpan const r = q.peek();
    if (r.data() == nullptr) continue;
    if (r.size() != kRecLen) return false;
    std::memcpy(tag_out, r.data(), 4);
    if (pop) q.pop();
    return true;
  }
  return false;
}

char procState(pid_t pid) {
  char path[64];
  std::snprintf(path, sizeof(path), "/proc/%d/stat", pid);
  int const fd = ::open(path, O_RDONLY | O_CLOEXEC);
  if (fd < 0) return '?';
  char buf[256];
  ssz_t const got = ::read(fd, buf, sizeof(buf) - 1);
  ::close(fd);
  if (got <= 0) return '?';
  buf[got] = '\0';
  char const* p = std::strrchr(buf, ')');
  if (p == nullptr) return '?';
  ++p;
  while (*p == ' ') ++p;
  return *p;
}

struct Pipe {
  int fds[2] = {-1, -1};
  Pipe() { REQUIRE(::pipe(fds) == 0); }
  ~Pipe() {
    if (fds[0] >= 0) ::close(fds[0]);
    if (fds[1] >= 0) ::close(fds[1]);
  }
  void send(char c) const { REQUIRE(::write(fds[1], &c, 1) == 1); }
  char recv() const {
    char c = 0;
    REQUIRE(::read(fds[0], &c, 1) == 1);
    return c;
  }
  // Child-side, no doctest.
  void csend(char c) const { (void)!::write(fds[1], &c, 1); }
  char crecv() const {
    char c = 0;
    return ::read(fds[0], &c, 1) == 1 ? c : 0;
  }
};

int waitFor(pid_t pid) {
  int st = 0;
  REQUIRE(waitpid(pid, &st, 0) == pid);
  return st;
}

// ---------------------------------------------------------------------------
// 1. A fresh process attaches by fd, claims, half-fills, dies. Its live
// neighbours -- another fresh-attach process and the creator -- keep writing,
// and the reader delivers every committed record in order around exactly one
// reclaim. This is mpsc_fault_test's first scenario moved onto the REAL
// cross-process path: the dead writer had its own mapping and its own object.
// ---------------------------------------------------------------------------
TEST_CASE("xproc: writer killed mid-reservation among live fresh-attach neighbours") {
  static std::atomic<u64_t> reclaims{0};
  static CountingPolicy pol{{}, &reclaims};
  static XRing q{pol};  // static: Ring's writer TLS identifies queues by address
  REQUIRE(XRing::create(smallConfig(), q));
  REQUIRE(q.attachReader());
  REQUIRE(q.attachWriter());
  int const fd = q.region().fd();
  REQUIRE(fd >= 0);

  u32_t tag = 0;
  for (; tag < 5; ++tag) pushTag(q, tag);

  pid_t const killer = fork();
  REQUIRE(killer >= 0);
  if (killer == 0) {
    alarm(20);
    static XRing cq;  // fresh object, fresh mapping, same file
    if (!XRing::attach(fd, cq)) _exit(1);
    if (!cq.attachWriter()) _exit(2);
    WriteSpan const s = cq.reserve(100);
    if (s.data() == nullptr) _exit(3);
    std::memset(s.data(), 0x5a, 50);
    raise(SIGKILL);
    _exit(4);  // unreachable
  }
  int st = waitFor(killer);
  REQUIRE(WIFSIGNALED(st));
  REQUIRE(WTERMSIG(st) == SIGKILL);

  // A live neighbour, also fresh-attach, writes past the dead record.
  pid_t const neighbour = fork();
  REQUIRE(neighbour >= 0);
  if (neighbour == 0) {
    alarm(20);
    static XRing cq;
    if (!XRing::attach(fd, cq)) _exit(1);
    if (!cq.attachWriter()) _exit(2);
    for (u32_t t = 100; t < 105; ++t) {
      if (!childWriteTag(cq, t)) _exit(3);
    }
    _exit(0);
  }
  st = waitFor(neighbour);
  REQUIRE(WIFEXITED(st));
  REQUIRE(WEXITSTATUS(st) == 0);

  for (; tag < 10; ++tag) pushTag(q, tag);

  // Total order: creator 0..4, dead record (reclaimed, not delivered),
  // neighbour 100..104, creator 5..9.
  for (u32_t t = 0; t < 5; ++t) CHECK(popTag(q) == t);
  for (u32_t t = 100; t < 105; ++t) CHECK(popTag(q) == t);
  for (u32_t t = 5; t < 10; ++t) CHECK(popTag(q) == t);
  CHECK(reclaims.load() == 1);
  CHECK(q.peek().data() == nullptr);
}

// ---------------------------------------------------------------------------
// 2. SIGSTOP is not death, on the fresh-attach path. The reader must block at
// the stopped writer's record -- reclaiming it is exactly what would let the
// writer resume and scribble over recycled bytes -- and must deliver it
// intact after SIGCONT. The design's core asymmetry.
// ---------------------------------------------------------------------------
TEST_CASE("xproc: stopped fresh-attach writer blocks the reader and resumes cleanly") {
  static std::atomic<u64_t> reclaims{0};
  static CountingPolicy pol{{}, &reclaims};
  static XRing q{pol};
  REQUIRE(XRing::create(smallConfig(), q));
  REQUIRE(q.attachReader());
  REQUIRE(q.attachWriter());
  int const fd = q.region().fd();

  pushTag(q, 0);

  pid_t const pid = fork();
  REQUIRE(pid >= 0);
  if (pid == 0) {
    alarm(30);
    static XRing cq;
    if (!XRing::attach(fd, cq)) _exit(1);
    if (!cq.attachWriter()) _exit(2);
    WriteSpan const s = cq.reserve(kRecLen);
    if (s.data() == nullptr) _exit(3);
    raise(SIGSTOP);  // returns after SIGCONT
    fillTag<XRing>(s, 1);
    cq.commit(kRecLen);
    _exit(0);
  }
  int st = 0;
  REQUIRE(waitpid(pid, &st, WUNTRACED) == pid);  // reserve done, child stopped
  REQUIRE(WIFSTOPPED(st));

  CHECK(popTag(q) == 0);  // committed-before is deliverable
  // Poll well past any transient: no delivery, no reclaim, ever.
  for (int i = 0; i < 200; ++i) {
    CHECK(q.peek().data() == nullptr);
    std::this_thread::sleep_for(std::chrono::milliseconds(1));
  }
  CHECK(reclaims.load() == 0);

  REQUIRE(kill(pid, SIGCONT) == 0);
  st = waitFor(pid);
  REQUIRE(WIFEXITED(st));
  REQUIRE(WEXITSTATUS(st) == 0);
  CHECK(popTag(q) == 1);
  CHECK(reclaims.load() == 0);
}

// ---------------------------------------------------------------------------
// 3. Zombies count as dead. The writer's process dies mid-reservation and is
// deliberately NOT reaped: /proc/<tid> still exists and tgkill would still
// succeed, so a liveness check that stops at existence reads it as alive
// forever and the queue hangs with no thread stopped. The state-field parse
// ('Z') is what makes recovery happen; assert it fires while the corpse is
// still a zombie.
// ---------------------------------------------------------------------------
TEST_CASE("xproc: unreaped zombie writer is recovered, not waited on") {
  static std::atomic<u64_t> reclaims{0};
  static CountingPolicy pol{{}, &reclaims};
  static XRing q{pol};
  REQUIRE(XRing::create(smallConfig(), q));
  REQUIRE(q.attachReader());
  REQUIRE(q.attachWriter());
  int const fd = q.region().fd();

  pid_t const pid = fork();
  REQUIRE(pid >= 0);
  if (pid == 0) {
    alarm(20);
    static XRing cq;
    if (!XRing::attach(fd, cq)) _exit(1);
    if (!cq.attachWriter()) _exit(2);
    if (cq.reserve(kRecLen).data() == nullptr) _exit(3);
    raise(SIGKILL);
    _exit(4);
  }

  // Wait for the zombie state WITHOUT reaping.
  auto const deadline = std::chrono::steady_clock::now() + std::chrono::seconds(10);
  while (procState(pid) != 'Z') {
    REQUIRE(std::chrono::steady_clock::now() < deadline);
    std::this_thread::sleep_for(std::chrono::milliseconds(1));
  }

  // The dead record blocks the stream head; records behind it only arrive
  // once the reader recovers it. Assert delivery WHILE the corpse is a
  // zombie -- this is the case that silently hangs forever if liveness stops
  // at /proc existence.
  pushTag(q, 7);
  CHECK(popTag(q) == 7);
  CHECK(reclaims.load() == 1);
  CHECK(procState(pid) == 'Z');  // still unreaped at the moment of assertion

  waitFor(pid);  // now clean up
}

// ---------------------------------------------------------------------------
// 4. Reader restart. R1 (fresh process) consumes part of the stream and dies
// between peek and pop; a second reader cannot attach while R1 lives; R2
// (another fresh process) takes over after R1's death, resumes exactly at
// read_pos, and redelivers the peeked-but-unpopped record: at-least-once, by
// contract.
// ---------------------------------------------------------------------------
TEST_CASE("xproc: reader killed mid-stream; successor resumes at read_pos, at-least-once") {
  static Mpsc q;
  REQUIRE(Mpsc::create(smallConfig(), q));
  REQUIRE(q.attachWriter());
  int const fd = q.region().fd();

  for (u32_t t = 0; t < 10; ++t) pushTag(q, t);

  Pipe ready, go;
  pid_t const r1 = fork();
  REQUIRE(r1 >= 0);
  if (r1 == 0) {
    alarm(30);
    static Mpsc cq;
    if (!Mpsc::attach(fd, cq)) _exit(1);
    if (!cq.attachReader()) _exit(2);
    ready.csend('A');
    if (go.crecv() != 'g') _exit(3);
    u32_t tag = 0;
    for (u32_t t = 0; t < 3; ++t) {  // pop 0,1,2
      if (!childPopTag(cq, &tag) || tag != t) _exit(4);
    }
    if (!childPopTag(cq, &tag, /*pop=*/false) || tag != 3) _exit(5);  // peek 3, no pop
    ready.csend('B');
    raise(SIGKILL);  // dies between peek and pop
    _exit(6);
  }
  REQUIRE(ready.recv() == 'A');

  // Exactly one reader: while R1 lives, a takeover attempt must fail.
  pid_t const intruder = fork();
  REQUIRE(intruder >= 0);
  if (intruder == 0) {
    alarm(20);
    static Mpsc cq;
    if (!Mpsc::attach(fd, cq)) _exit(1);
    _exit(cq.attachReader() ? 51 : 50);  // 50 = correctly refused
  }
  int st = waitFor(intruder);
  REQUIRE(WIFEXITED(st));
  CHECK(WEXITSTATUS(st) == 50);

  go.send('g');
  REQUIRE(ready.recv() == 'B');
  st = waitFor(r1);  // reap: R1 is now provably dead
  REQUIRE(WIFSIGNALED(st));

  pid_t const r2 = fork();
  REQUIRE(r2 >= 0);
  if (r2 == 0) {
    alarm(30);
    static Mpsc cq;
    if (!Mpsc::attach(fd, cq)) _exit(1);
    if (!cq.attachReader()) _exit(2);  // takeover from the proven-dead R1
    u32_t tag = 0;
    for (u32_t t = 3; t < 10; ++t) {  // tag 3 REDELIVERED: at-least-once
      if (!childPopTag(cq, &tag) || tag != t) _exit(10 + t);
    }
    if (cq.peek().data() != nullptr) _exit(30);
    _exit(0);
  }
  st = waitFor(r2);
  REQUIRE(WIFEXITED(st));
  CHECK(WEXITSTATUS(st) == 0);
}

// ---------------------------------------------------------------------------
// 5. MultiSpsc slot recycling gates. A dead writer's slot is reusable only
// after BOTH (a) proven death and (b) its ring drained. Reusing it earlier
// reintroduces cross-writer byte reuse -- the thing SpscRing's correctness
// argument forbids -- so the middle assertion here (attach FAILS with kNoSlot
// while the dead ring still holds records) is the load-bearing one.
// ---------------------------------------------------------------------------
TEST_CASE("xproc: dead writer's slot recycles only after its ring drains") {
  static MultiSpsc q;
  REQUIRE(MultiSpsc::create(smallConfig(/*shards=*/2), q));
  REQUIRE(q.attachReader());
  int const fd = q.region().fd();

  // W1: claims a slot, commits three records, dies. Reaped: proven dead,
  // ring undrained.
  pid_t const w1 = fork();
  REQUIRE(w1 >= 0);
  if (w1 == 0) {
    alarm(20);
    static MultiSpsc cq;
    if (!MultiSpsc::attach(fd, cq)) _exit(1);
    if (!cq.attachWriter()) _exit(2);
    for (u32_t t = 1000; t < 1003; ++t) {
      if (!childWriteTag(cq, t)) _exit(3);
    }
    raise(SIGKILL);
    _exit(4);
  }
  int st = waitFor(w1);
  REQUIRE(WIFSIGNALED(st));

  // W2: claims the other slot and STAYS ALIVE holding it.
  Pipe w2_ready, w2_done;
  pid_t const w2 = fork();
  REQUIRE(w2 >= 0);
  if (w2 == 0) {
    alarm(60);
    static MultiSpsc cq;
    if (!MultiSpsc::attach(fd, cq)) _exit(1);
    if (!cq.attachWriter()) _exit(2);
    if (!childWriteTag(cq, 2000)) _exit(3);
    w2_ready.csend('R');
    (void)w2_done.crecv();  // hold the slot until the parent is done
    _exit(0);
  }
  REQUIRE(w2_ready.recv() == 'R');

  // W3: both slots taken -- one by a live writer (gate a), one by a dead
  // writer whose ring is UNDRAINED (gate b). Attach must fail, kNoSlot.
  pid_t const w3 = fork();
  REQUIRE(w3 >= 0);
  if (w3 == 0) {
    alarm(20);
    static MultiSpsc cq;
    if (!MultiSpsc::attach(fd, cq)) _exit(1);
    if (cq.attachWriter()) _exit(41);  // recycled early: THE bug this test exists for
    _exit(cq.status() == Status::kNoSlot ? 40 : 42);
  }
  st = waitFor(w3);
  REQUIRE(WIFEXITED(st));
  CHECK(WEXITSTATUS(st) == 40);

  // Drain everything: three from the dead ring, one from W2. Per-writer FIFO
  // only -- count per-writer sequences, order across writers is unspecified.
  u32_t next_dead = 1000, seen_w2 = 0;
  for (int i = 0; i < 4; ++i) {
    u32_t const tag = popTag(q);
    if (tag >= 2000) {
      CHECK(tag == 2000);
      ++seen_w2;
    } else {
      CHECK(tag == next_dead);
      ++next_dead;
    }
  }
  CHECK(next_dead == 1003);
  CHECK(seen_w2 == 1);

  // W4: the dead ring is now drained, so its slot must recycle -- and the
  // slot generation must have been bumped to fence stale {slot, generation}
  // holders.
  u32_t const gen_before = q.region().writerSlots()[0].generation +
                           q.region().writerSlots()[1].generation;
  pid_t const w4 = fork();
  REQUIRE(w4 >= 0);
  if (w4 == 0) {
    alarm(20);
    static MultiSpsc cq;
    if (!MultiSpsc::attach(fd, cq)) _exit(1);
    if (!cq.attachWriter()) _exit(2);
    if (!childWriteTag(cq, 3000)) _exit(3);
    _exit(0);
  }
  st = waitFor(w4);
  REQUIRE(WIFEXITED(st));
  REQUIRE(WEXITSTATUS(st) == 0);
  CHECK(popTag(q) == 3000);
  u32_t const gen_after = q.region().writerSlots()[0].generation +
                          q.region().writerSlots()[1].generation;
  CHECK(gen_after == gen_before + 1);

  w2_done.send('D');
  st = waitFor(w2);
  REQUIRE(WIFEXITED(st));
  REQUIRE(WEXITSTATUS(st) == 0);
}

// ---------------------------------------------------------------------------
// 6. fork() with an inherited reservation, on the sharded SPSC path. The
// ring's reservation lives in the ring OBJECT, which the child inherits by
// copy -- the atfork handler clears the child's shard ROUTING, so the commit
// must hit the release-mode trap rather than republish the parent's
// reservation from a second process. The child must then be able to win its
// OWN slot and write.
// ---------------------------------------------------------------------------
TEST_CASE("xproc: forked child cannot commit an inherited reservation; re-registers instead") {
  static MultiSpsc q;
  REQUIRE(MultiSpsc::create(smallConfig(/*shards=*/2), q));
  REQUIRE(q.attachReader());
  int const fd = q.region().fd();

  pid_t const p1 = fork();
  REQUIRE(p1 >= 0);
  if (p1 == 0) {
    alarm(30);
    static MultiSpsc cq;
    if (!MultiSpsc::attach(fd, cq)) _exit(1);
    if (!cq.attachWriter()) _exit(2);  // registers the atfork handler
    WriteSpan const s = cq.reserve(kRecLen);
    if (s.data() == nullptr) _exit(3);

    pid_t const g = fork();
    if (g < 0) _exit(4);
    if (g == 0) {
      alarm(20);
      // Quiet both streams: the trap prints to stderr, and the inherited
      // doctest crash reporter narrates the SIGABRT to stdout before
      // re-raising it -- the abort itself is what the parent asserts on.
      int const null = ::open("/dev/null", O_WRONLY);
      if (null >= 0) {
        ::dup2(null, 1);
        ::dup2(null, 2);
      }
      cq.commit(kRecLen);  // MUST trap: routing TLS was cleared
      _exit(5);            // reaching here means it committed
    }
    int gst = 0;
    if (waitpid(g, &gst, 0) != g) _exit(6);
    if (!WIFSIGNALED(gst) || WTERMSIG(gst) != SIGABRT) _exit(7);

    // Parent's reservation is intact; finish it so the ring stays clean.
    fillTag<MultiSpsc>(s, 4000);
    cq.commit(kRecLen);

    // A fresh child may re-register and gets its OWN slot.
    pid_t const g2 = fork();
    if (g2 < 0) _exit(8);
    if (g2 == 0) {
      alarm(20);
      static MultiSpsc gq;
      if (!MultiSpsc::attach(fd, gq)) _exit(9);
      if (!gq.attachWriter()) _exit(10);  // slot 1: its own, not the parent's
      if (!childWriteTag(gq, 5000)) _exit(11);
      _exit(0);
    }
    int g2st = 0;
    if (waitpid(g2, &g2st, 0) != g2) _exit(12);
    if (!WIFEXITED(g2st) || WEXITSTATUS(g2st) != 0) _exit(13);
    _exit(0);
  }
  int const st = waitFor(p1);
  REQUIRE(WIFEXITED(st));
  CHECK(WEXITSTATUS(st) == 0);

  // Both records arrive, each on its own ring.
  u32_t const a = popTag(q);
  u32_t const b = popTag(q);
  CHECK(a + b == 9000);  // {4000, 5000} in either order
  CHECK(q.peek().data() == nullptr);
}

}  // namespace
