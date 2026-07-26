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
//   * kShm named rendezvous: a successor reader shm_opens the name UNAIDED --
//     no inherited fd -- and resumes; the one thing kShm buys over kMemfd
//   * dead reader turns full into kReaderDead on Spsc / ShardedMpsc /
//     MultiSpsc (the Mpsc flavor lives in mpsc_fault_test)

#include "pgt/mpsc/queue.hh"
#include "pgt/mpsc/region.hh"

#include <doctest/doctest.h>

#include <fcntl.h>
#include <signal.h>
#include <sys/mman.h>
#include <sys/wait.h>
#include <unistd.h>

#include <atomic>
#include <chrono>
#include <cstdint>
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
  q.commit(s, kRecLen);
  return true;
}

template <typename Q>
void pushTag(Q& q, u32_t tag) {
  WriteSpan const s = q.reserve(kRecLen);
  REQUIRE(s.data() != nullptr);
  fillTag<Q>(s, tag);
  q.commit(s, kRecLen);
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
    cq.commit(s, kRecLen);
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
      cq.commit(s, kRecLen);  // MUST trap: routing TLS was cleared
      _exit(5);            // reaching here means it committed
    }
    int gst = 0;
    if (waitpid(g, &gst, 0) != g) _exit(6);
    if (!WIFSIGNALED(gst) || WTERMSIG(gst) != SIGABRT) _exit(7);

    // Parent's reservation is intact; finish it so the ring stays clean.
    fillTag<MultiSpsc>(s, 4000);
    cq.commit(s, kRecLen);

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

// ---------------------------------------------------------------------------
// 7. kShm named rendezvous. Same restart shape as test 4, but both readers
// reach the region by shm_open on the NAME, with no inherited fd in play --
// which is the one thing the kShm backend buys over kMemfd, and the deployment
// where reader restart earns its keep: the crashed reader's successor needs no
// surviving donor for the descriptor.
// ---------------------------------------------------------------------------
TEST_CASE("xproc: shm-named reader restart; successor rendezvous by name, at-least-once") {
  char name[64];
  std::snprintf(name, sizeof(name), "/pgt_xproc_%d", getpid());
  ::shm_unlink(name);  // clear a stale name from any crashed prior run
  Config cfg = smallConfig();
  cfg.backend = Backend::kShm;
  cfg.name = name;
  static Mpsc q;
  REQUIRE(Mpsc::create(cfg, q));
  REQUIRE(q.attachWriter());
  for (u32_t t = 0; t < 10; ++t) pushTag(q, t);

  Pipe ready, go;
  pid_t const r1 = fork();
  REQUIRE(r1 >= 0);
  if (r1 == 0) {
    alarm(30);
    int const sfd = ::shm_open(name, O_RDWR, 0);  // rendezvous BY NAME, unaided
    if (sfd < 0) _exit(1);
    static Mpsc cq;
    if (!Mpsc::attach(sfd, cq)) _exit(2);
    if (!cq.attachReader()) _exit(3);
    ready.csend('A');
    if (go.crecv() != 'g') _exit(4);
    u32_t tag = 0;
    for (u32_t t = 0; t < 3; ++t) {
      if (!childPopTag(cq, &tag) || tag != t) _exit(5);
    }
    if (!childPopTag(cq, &tag, /*pop=*/false) || tag != 3) _exit(6);  // peek, no pop
    ready.csend('B');
    raise(SIGKILL);
    _exit(7);
  }
  REQUIRE(ready.recv() == 'A');
  go.send('g');
  REQUIRE(ready.recv() == 'B');
  int st = waitFor(r1);
  REQUIRE(WIFSIGNALED(st));

  pid_t const r2 = fork();
  REQUIRE(r2 >= 0);
  if (r2 == 0) {
    alarm(30);
    int const sfd = ::shm_open(name, O_RDWR, 0);
    if (sfd < 0) _exit(1);
    static Mpsc cq;
    if (!Mpsc::attach(sfd, cq)) _exit(2);
    if (!cq.attachReader()) _exit(3);  // takeover from the proven-dead R1
    u32_t tag = 0;
    for (u32_t t = 3; t < 10; ++t) {  // tag 3 redelivered: at-least-once
      if (!childPopTag(cq, &tag) || tag != t) _exit(10 + t);
    }
    _exit(0);
  }
  st = waitFor(r2);
  REQUIRE(WIFEXITED(st));
  CHECK(WEXITSTATUS(st) == 0);
  // The caller owns the name's lifetime (region.cc): unlink it here.
  REQUIRE(::shm_unlink(name) == 0);
}

// ---------------------------------------------------------------------------
// 8. A dead reader turns permanent backpressure into a definite error, on the
// variants mpsc_fault_test does not cover (its flavor is Mpsc, thread-exit,
// same process). Here the reader is a separate PROCESS that attached and
// exited, and the writer discovers its death through the full path's liveness
// check: kReaderDead, never kFull.
// ---------------------------------------------------------------------------
TEST_CASE_TEMPLATE("xproc: dead reader process yields kReaderDead on the full path", Q,  //
                   Spsc, ShardedMpsc, MultiSpsc) {
  static Q* qs = new Q;  // fresh heap instance per instantiation; never reused
  Q& q = *qs;
  REQUIRE(Q::create(smallConfig(/*shards=*/2), q));
  int const fd = q.region().fd();
  REQUIRE(fd >= 0);

  // The reader attaches from a process that then exits: its recorded tid is
  // provably dead once reaped.
  pid_t const r = fork();
  REQUIRE(r >= 0);
  if (r == 0) {
    alarm(20);
    static Q cq;
    if (!Q::attach(fd, cq)) _exit(1);
    if (!cq.attachReader()) _exit(2);
    _exit(0);
  }
  int const st = waitFor(r);
  REQUIRE(WIFEXITED(st));
  REQUIRE(WEXITSTATUS(st) == 0);

  REQUIRE(q.attachWriter());
  std::byte buf[64] = {};
  int writes = 0;
  while (q.write(buf, sizeof(buf))) {
    REQUIRE(++writes < 1000);  // one ring's worth at most; runaway means no full
  }
  CHECK(q.status() == Status::kReaderDead);
  q.detachWriter();
}

// ---------------------------------------------------------------------------
// 9. Adversarial registry geometry, cross-process (extends mpsc_test's
// 100-shard geometry test per impl-region). With enough shards to spill the
// bitmap into a second word, a fresh process is steered onto the LAST slot --
// its claim fetch_or lands in the final bitmap word -- and every registry
// write must stay inside the control area: the record committed in arena 0
// before the claims must drain intact afterwards, and no bit beyond the valid
// range may appear. An under-sized control area fails the bound check first.
// ---------------------------------------------------------------------------
TEST_CASE("xproc: last-slot claim in a multi-word bitmap never touches arena 0") {
  // 127 shards: bitmapWords() == 2 AND, on a 4 KiB-page host, the WriterSlot
  // table is what pushes the control area across its final page boundary --
  // chosen so that a controlBytes() that stops reserving the slot table is
  // NOT absorbed by page-align slack but fails the bound check below.
  // (Verified: with the slot-table term deleted from controlBytes, this
  // REQUIRE fires; at 100 shards the alignment slack swallowed the bug.)
  constexpr u32_t kShards = 127;  // valid bits in word 1: 0..62
  static MultiSpsc q;
  REQUIRE(MultiSpsc::create(smallConfig(kShards), q));
  REQUIRE(q.attachReader());
  Region const& r = q.region();
  int const fd = r.fd();

  // Layout bound: the registry ends at or before arena 0. This is the check
  // that fails FAST if controlBytes() ever stops reserving the slot table.
  REQUIRE(reinterpret_cast<std::byte const*>(r.writerSlots() + kShards) <= r.arena(0));
  REQUIRE(r.bitmapWords() == 2);

  // A real record in arena 0 first, so registry writes have something to hit.
  REQUIRE(q.attachWriter());  // this thread wins slot 0
  pushTag(q, 7000);

  // Steer the next attacher onto slot 99: mark slots 1..98 occupied-mid-handoff
  // (bit set, owner 0 -- the state the recycler must skip). White-box, but only
  // through the shared words the registry contract already publishes.
  for (u32_t s = 1; s < kShards - 1; ++s) {
    std::atomic_ref<u64_t>(r.writerBitmap()[s / 64]).fetch_or(1ull << (s % 64),
                                                              std::memory_order_acq_rel);
  }

  pid_t const w = fork();
  REQUIRE(w >= 0);
  if (w == 0) {
    alarm(20);
    static MultiSpsc cq;
    if (!MultiSpsc::attach(fd, cq)) _exit(1);
    if (!cq.attachWriter()) _exit(2);  // only slot 99 is claimable
    if (!childWriteTag(cq, 7099)) _exit(3);
    _exit(0);
  }
  int const st = waitFor(w);
  REQUIRE(WIFEXITED(st));
  REQUIRE(WEXITSTATUS(st) == 0);

  // The child's claim is visible here (shared registry, second mapping)...
  u64_t const word1 = std::atomic_ref<u64_t>(r.writerBitmap()[1]).load(std::memory_order_acquire);
  CHECK((word1 & (1ull << 62)) != 0);   // slot 126's bit, the last valid one
  CHECK((word1 >> 63) == 0);            // nothing landed beyond the table
  CHECK(std::atomic_ref<u32_t>(r.writerSlots()[kShards - 1].owner_tid)
            .load(std::memory_order_acquire) != 0);

  // ...and neither the claims nor the slot writes touched ring bytes: both
  // records drain intact, ring 0's first and ring 99's only.
  u32_t const a = popTag(q);
  u32_t const b = popTag(q);
  CHECK(a + b == 14099);  // {7000, 7099} in either order
  CHECK(q.peek().data() == nullptr);
  q.detachWriter();  // release this thread's Sharded binding for later tests
}

// ---------------------------------------------------------------------------
// 10. Registry at the EXACT-FIT shard count (per team-lead's analysis). The
// control layout is computed twice -- controlBytes() sums it, the region.hh
// accessors walk it -- and a divergence between them is only OBSERVABLE from
// outside when the control area has no page-align slack to hide it. At most
// counts the slack is hundreds of bytes (2128 at the 100 shards the geometry
// test uses); at the exact-fit count it is ZERO, so any accessor walking one
// byte past the reservation lands in arena 0, where registry writes corrupt
// the first records written. The count is DERIVED from the live page size and
// struct sizes -- hardcoding 15 would silently stop being a boundary test on
// a 16 KiB-page machine. Complements test 9 (which catches the converse:
// controlBytes() under-reserving) and the create()/attach() assert in
// region.cc (which catches both at every count, debug builds only).
// ---------------------------------------------------------------------------
namespace {

// Shard count whose control area fills its final page exactly (zero slack).
// Where the host page size admits none below the cap, the tightest-slack
// count is returned and the test degrades to a near-boundary test.
u32_t exactFitShards(sz_t page, sz_t* slack_out) {
  u32_t best = 1;
  sz_t best_slack = page;
  for (u32_t s = 1; s <= 256; ++s) {
    sz_t const bytes = sizeof(Control) + s * sizeof(ShardControl) +
                       ((s + 63) / 64) * sizeof(u64_t) + s * sizeof(WriterSlot);
    sz_t const slack = (page - bytes % page) % page;
    if (slack < best_slack) {
      best_slack = slack;
      best = s;
      if (slack == 0) break;
    }
  }
  *slack_out = best_slack;
  return best;
}

void runRegistryBoundary(u32_t shards, sz_t page) {
  auto* const qp = new MultiSpsc;  // fresh heap instance; deliberately leaked
  MultiSpsc& q = *qp;
  REQUIRE(MultiSpsc::create(smallConfig(shards), q));
  REQUIRE(q.attachReader());
  Region const& r = q.region();

  REQUIRE(reinterpret_cast<std::byte const*>(r.writerSlots() + shards) <= r.arena(0));
  REQUIRE(reinterpret_cast<std::uintptr_t>(r.arena(0)) % page == 0);

  // A committed record in arena 0 as the canary...
  REQUIRE(q.attachWriter());  // slot 0
  pushTag(q, 8000);

  // ...then exercise EVERY byte the registry protocol can ever write -- all
  // occupancy bits, every owner_tid, every generation, through the LAST slot,
  // whose final byte is exactly what an overflow pushes into arena 0.
  for (u32_t s = 1; s < shards; ++s) {
    std::atomic_ref<u64_t>(r.writerBitmap()[s / 64])
        .fetch_or(1ull << (s % 64), std::memory_order_acq_rel);
    std::atomic_ref<u32_t>(r.writerSlots()[s].owner_tid)
        .store(100000u + s, std::memory_order_release);
    std::atomic_ref<u32_t>(r.writerSlots()[s].generation).fetch_add(1, std::memory_order_acq_rel);
  }
  std::atomic_ref<u32_t>(r.writerSlots()[0].generation).fetch_add(1, std::memory_order_acq_rel);

  // The canary drains intact: no registry write reached ring bytes.
  CHECK(popTag(q) == 8000);
  CHECK(q.peek().data() == nullptr);
  q.detachWriter();
}

}  // namespace

TEST_CASE("xproc: registry writes at the exact-fit shard count stay out of arena 0") {
  sz_t const page = static_cast<sz_t>(::sysconf(_SC_PAGESIZE));
  REQUIRE(page > 0);
  sz_t slack = 0;
  u32_t const s = exactFitShards(page, &slack);
  CAPTURE(page);
  CAPTURE(s);
  CAPTURE(slack);  // 0 on 4 KiB pages (s == 15); tightest available otherwise
  runRegistryBoundary(s, page);      // zero slack: the only observable-overflow count
  runRegistryBoundary(s + 1, page);  // first count past the boundary: spills to a new page
}

}  // namespace
