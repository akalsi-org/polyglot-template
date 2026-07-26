#include "../mpsc/policy.hh"

#include <sched.h>
#include <time.h>

namespace pgt::mpsc {

// Spin -> yield -> sleep. The knees are deliberately early: past a few dozen
// iterations the wait is caller-controlled (a reservation held open across
// arbitrary user code), so burning a core stops buying latency. The sleep is
// kept well under a scheduler tick so a wait that ends soon after the knee
// still resumes promptly.
void BackoffPolicy::ladder(u32_t iter) noexcept {
  if (iter < 32) {
    cpuRelax();
    return;
  }
  if (iter < 64) {
    sched_yield();
    return;
  }
  timespec ts{};
  ts.tv_nsec = 50'000;  // 50us
  nanosleep(&ts, nullptr);
}

}  // namespace pgt::mpsc
