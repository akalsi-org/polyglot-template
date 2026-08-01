#pragma once

// Exact thread liveness. Never a timeout.
//
// Recovery in every variant is gated on PROVEN death, not on a record having been
// in flight "too long". A stopped-but-alive writer must block the queue:
// reclaiming its reservation would let it resume and scribble over recycled
// bytes, and no amount of waiting makes "slow" distinguishable from "dead".
//
// Zombies must count as dead. Existence probes -- tgkill(tgid, tid, 0) and
// sched_getscheduler(tid) alike -- succeed for a zombie, and /proc/<tid> exists
// for one, so a writer whose process died mid-claim
// but was never reaped would read as alive forever -- the reader backs off
// forever and the queue hangs with no thread stopped. We parse the state field
// and treat 'Z' and 'X' as dead. Exited *threads* inside a live process are
// auto-reaped, so the pure-thread case is safe either way.
//
// Thread ids are unique within a PID namespace and /proc/<tid> is accessible for
// any thread, not only group leaders -- which is why a bare 22-bit tid suffices
// in the descriptor and no tgid needs to be stored per record. All peers must
// therefore share a PID namespace.
//
// Assumes tid reuse does not occur. The /proc check is tid-only, so a recycled
// tid reads as alive and blocks recovery: a false-ALIVE, which hangs rather than
// corrupting -- the safe direction.

#include "pgt/core/types.hh"

#include <fcntl.h>
#include <sys/syscall.h>
#include <unistd.h>

#include <cerrno>

#include <cstdio>
#include <cstring>

namespace pgt::mpsc {

// True if `tid` names a thread that may still execute. A zombie is not alive.
// True only when the kernel says no such thread exists.
//
// sched_getscheduler() takes a TID, needs no file descriptor and no tgid, and
// reports ESRCH for a thread that is gone. That makes it a definitive DEATH proof
// obtainable under fd exhaustion, which /proc is not.
//
// tgkill() would also serve but needs the owner's tgid, and records carry only a
// 22-bit tid -- checking a writer in another process against the creator's tgid
// returns ESRCH for a perfectly live thread, i.e. a false DEAD, the one direction
// that corrupts. This probe has no such requirement.
//
// It cannot replace the /proc parse: a ZOMBIE still exists, so a negative result
// means "exists", not "alive".
[[nodiscard]] inline bool existenceProbeSaysGone(u32_t tid) noexcept {
  // RAW SYSCALL, deliberately -- musl stubs the sched_getscheduler() wrapper to
  // return ENOSYS unconditionally, for live and dead threads alike. Going
  // through libc here makes the probe silently useless on the production
  // toolchain: it would never report ESRCH, the fd-free fast path would never
  // fire, and under fd exhaustion every thread would read as alive, wedging
  // recovery instead of performing it. Verified on this toolchain: the wrapper
  // gives ENOSYS for both, the raw syscall gives 0 and ESRCH respectively.
  errno = 0;
  return ::syscall(SYS_sched_getscheduler, static_cast<long>(tid)) == -1 && errno == ESRCH;
}

[[nodiscard]] inline bool threadAlive(u32_t tid) noexcept {
  if (tid == 0) return false;

  if (existenceProbeSaysGone(tid)) return false;  // proven gone, no fd required

  char path[64];
  std::snprintf(path, sizeof(path), "/proc/%u/stat", tid);
  // EVERY failure below returns ALIVE. Only ENOENT-class absence and an observed
  // Z/X state prove death; EMFILE, ENFILE, EACCES, hidepid, a namespace mismatch,
  // a short read or an unparseable line all mean WE CANNOT TELL, and "cannot
  // tell" must never authorise recovery. The previous code returned false for
  // all of them, so fd exhaustion made every live writer read as dead -- and
  // cascaded, since once one check failed that way they all did.
  int fd;
  do {
    fd = ::open(path, O_RDONLY | O_CLOEXEC);
  } while (fd < 0 && errno == EINTR);
  // ENOENT is the one open() failure that PROVES death -- the thread's /proc
  // entry is gone. Everything else (EMFILE, ENFILE, EACCES, hidepid, namespace
  // mismatch) means we cannot tell, and cannot-tell must read as alive.
  // Do not collapse this into an unconditional `return true`: the fast probe
  // above is an optimisation, not the sole death oracle, and removing this
  // backstop makes every reaped thread read as alive -- which wedges reader
  // takeover and blocks recovery entirely.
  if (fd < 0) return errno != ENOENT;

  char buf[256];
  ssz_t got;
  do {
    got = ::read(fd, buf, sizeof(buf) - 1);
  } while (got < 0 && errno == EINTR);
  ::close(fd);
  if (got <= 0) {
    // A zero-length read is not ambiguity, it is the entry vanishing under us:
    // open() succeeded, then the task was reaped before read(). Re-probe, because
    // existence is authoritative here and EOF is not. Treating EOF as "unknown ->
    // alive" made reader takeover after a joined thread flake 25% of the time.
    return !existenceProbeSaysGone(tid);
  }
  buf[got] = '\0';

  // /proc/<tid>/stat is "pid (comm) state ...", and comm may contain spaces or
  // parentheses, so scan back from the LAST ')' rather than tokenising forward.
  char const* p = std::strrchr(buf, ')');
  if (p == nullptr) return !existenceProbeSaysGone(tid);  // truncated: re-probe
  ++p;
  while (*p == ' ') ++p;

  return *p != 'Z' && *p != 'X';
}

// (A tgkill-based threadAliveLocal() was declared here and never implemented.
// Removed: sched_getscheduler above gives the same fd-free death proof without
// needing a tgid, so the process-local special case buys nothing and its
// misconfiguration -- a foreign writer checked against the creator's tgid --
// would produce a false DEAD.)

[[nodiscard]] inline u32_t currentTid() noexcept { return static_cast<u32_t>(::gettid()); }

}  // namespace pgt::mpsc
