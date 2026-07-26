#pragma once

// Exact thread liveness. Never a timeout.
//
// Recovery in every variant is gated on PROVEN death, not on a record having been
// in flight "too long". A stopped-but-alive writer must block the queue:
// reclaiming its reservation would let it resume and scribble over recycled
// bytes, and no amount of waiting makes "slow" distinguishable from "dead".
//
// Zombies must count as dead. tgkill(tgid, tid, 0) succeeds for a zombie group
// leader and /proc/<tid> exists for one, so a writer whose process died mid-claim
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

#include "../core/types.hh"

#include <fcntl.h>
#include <unistd.h>

#include <cstdio>
#include <cstring>

namespace pgt::mpsc {

// True if `tid` names a thread that may still execute. A zombie is not alive.
[[nodiscard]] inline bool threadAlive(u32_t tid) noexcept {
  if (tid == 0) return false;

  char path[64];
  std::snprintf(path, sizeof(path), "/proc/%u/stat", tid);
  int const fd = ::open(path, O_RDONLY | O_CLOEXEC);
  if (fd < 0) return false;  // gone

  char buf[256];
  ssz_t const got = ::read(fd, buf, sizeof(buf) - 1);
  ::close(fd);
  if (got <= 0) return false;
  buf[got] = '\0';

  // /proc/<tid>/stat is "pid (comm) state ...", and comm may contain spaces or
  // parentheses, so scan back from the LAST ')' rather than tokenising forward.
  char const* p = std::strrchr(buf, ')');
  if (p == nullptr) return false;
  ++p;
  while (*p == ' ') ++p;

  return *p != 'Z' && *p != 'X';
}

// Cheaper liveness for the process-local configuration, where a zombie cannot
// arise because every writer is a thread of this process. Callers that may have
// cross-process writers must use threadAlive().
[[nodiscard]] inline bool threadAliveLocal(u32_t tgid, u32_t tid) noexcept;

[[nodiscard]] inline u32_t currentTid() noexcept {
  return static_cast<u32_t>(::gettid());
}

}  // namespace pgt::mpsc
