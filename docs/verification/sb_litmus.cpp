// SB (store-buffering) litmus harness for the tmp-mpsc sleep interlock.
//
// Shape under test (producer/consumer of the doc, reduced to its Dekker core):
//   T0 (producer):  publish tail        ; <barrier?> ; r0 = load state
//   T1 (consumer):  state = SLEEPING    ; <barrier?> ; r1 = load tail
// Forbidden outcome: r0 == AWAKE && r1 == empty  (producer skips wake AND
// consumer decides to sleep => lost wakeup / hang).
//
// Modes:
//   naive  : release store        + relaxed load   (no barrier at all)
//   broken : seq_cst RMW publish  + relaxed load   (the doc's "lock xchg trick")
//   fixed  : release store + atomic_thread_fence(seq_cst) + relaxed load
//   seqcst : seq_cst store        + seq_cst load
//
// Expectations:
//   x86-64 : naive FAILS (TSO permits SB for plain stores/loads);
//            broken PASSES — but only because the locked RMW is an x86 full
//            fence, i.e. by ISA accident, not by the C++ model;
//            fixed/seqcst PASS.
//   AArch64: naive and broken are both architecturally permitted to fail
//            (the compiled broken form has no dmb ish between the RMW and the
//            load — see ord_a64.s); fixed/seqcst PASS. Requires real ARM
//            hardware to demonstrate: qemu-user executes with the host's
//            (stronger) memory ordering, so weak-memory outcomes do not
//            reproduce under emulation on x86.
//
// TSan note: every access here is a std::atomic access, so NO mode contains a
// data race. TSan verifies happens-before on synchronization it can see; it
// does not model the seq_cst total order, so it reports nothing for the broken
// mode. A TSan-only verification plan misses this bug class entirely.
//
// herd7 encoding of the same shape (for the record; herd7 not available in
// this environment):
//   AArch64 SB+swpal+ldr
//   { 0:X1=x; 0:X3=y; 1:X1=y; 1:X3=x; }
//    P0                 | P1                 ;
//    MOV W0,#1          | MOV W0,#1          ;
//    SWPAL W0,W2,[X1]   | SWPAL W0,W2,[X1]   ;
//    LDR W4,[X3]        | LDR W4,[X3]        ;
//   exists (0:X4=0 /\ 1:X4=0)

#include <atomic>
#include <cstdint>
#include <cstdio>
#include <cstring>
#include <thread>

enum Mode { NAIVE, BROKEN, FIXED, SEQCST };

static std::atomic<uint64_t> tail;   // 0 = empty, 1 = record published
static std::atomic<uint32_t> state;  // 0 = AWAKE,  1 = SLEEPING

alignas(64) static std::atomic<uint64_t> round_no{0};
alignas(64) static std::atomic<int> ready0{0}, ready1{0};
alignas(64) static uint32_t r_prod;  // producer's view of state
alignas(64) static uint64_t r_cons;  // consumer's view of tail

template <int SIDE>  // 0 = producer(tail then state), 1 = consumer(state then tail)
static inline void run_side(Mode m) {
  if constexpr (SIDE == 0) {
    switch (m) {
      case NAIVE:
        tail.store(1, std::memory_order_release);
        r_prod = state.load(std::memory_order_relaxed);
        break;
      case BROKEN:  // the doc's trick: seq_cst RMW publish, relaxed load
        tail.exchange(1, std::memory_order_seq_cst);
        r_prod = state.load(std::memory_order_relaxed);
        break;
      case FIXED:
        tail.store(1, std::memory_order_release);
        std::atomic_thread_fence(std::memory_order_seq_cst);
        r_prod = state.load(std::memory_order_relaxed);
        break;
      case SEQCST:
        tail.store(1, std::memory_order_seq_cst);
        r_prod = state.load(std::memory_order_seq_cst);
        break;
    }
  } else {
    switch (m) {
      case NAIVE:
        state.store(1, std::memory_order_release);
        r_cons = tail.load(std::memory_order_relaxed);
        break;
      case BROKEN:  // symmetric: RMW the flag, relaxed scan
        state.exchange(1, std::memory_order_seq_cst);
        r_cons = tail.load(std::memory_order_relaxed);
        break;
      case FIXED:
        state.store(1, std::memory_order_relaxed);
        std::atomic_thread_fence(std::memory_order_seq_cst);
        r_cons = tail.load(std::memory_order_relaxed);
        break;
      case SEQCST:
        state.store(1, std::memory_order_seq_cst);
        r_cons = tail.load(std::memory_order_seq_cst);
        break;
    }
  }
}

template <int SIDE>
static void worker(Mode m, uint64_t iters) {
  auto& mine = SIDE == 0 ? ready0 : ready1;
  for (uint64_t i = 1; i <= iters; ++i) {
    mine.store((int)i, std::memory_order_release);  // arrived
    while (round_no.load(std::memory_order_acquire) < i) {
    }  // wait for go
    run_side<SIDE>(m);
    mine.store(-(int)i, std::memory_order_release);  // done
    while (round_no.load(std::memory_order_acquire) == i) {
    }  // wait reset
  }
}

int main(int argc, char** argv) {
  Mode m = FIXED;
  uint64_t iters = 20'000'000;
  if (argc > 1) {
    if (!strcmp(argv[1], "naive"))
      m = NAIVE;
    else if (!strcmp(argv[1], "broken"))
      m = BROKEN;
    else if (!strcmp(argv[1], "fixed"))
      m = FIXED;
    else if (!strcmp(argv[1], "seqcst"))
      m = SEQCST;
    else {
      fprintf(stderr, "usage: %s naive|broken|fixed|seqcst [iters]\n", argv[0]);
      return 2;
    }
  }
  if (argc > 2) iters = strtoull(argv[2], nullptr, 10);

  std::thread t0(worker<0>, m, iters);
  std::thread t1(worker<1>, m, iters);

  uint64_t violations = 0, first = 0;
  for (uint64_t i = 1; i <= iters; ++i) {
    while (ready0.load(std::memory_order_acquire) != (int)i ||
           ready1.load(std::memory_order_acquire) != (int)i) {
    }
    tail.store(0, std::memory_order_relaxed);  // reset shared state
    state.store(0, std::memory_order_relaxed);
    round_no.store(i, std::memory_order_seq_cst);  // go
    while (ready0.load(std::memory_order_acquire) != -(int)i ||
           ready1.load(std::memory_order_acquire) != -(int)i) {
    }
    if (r_prod == 0 /*AWAKE*/ && r_cons == 0 /*empty*/) {
      ++violations;
      if (!first) first = i;
    }
    round_no.store(0, std::memory_order_seq_cst);  // reset for next round
  }
  t0.join();
  t1.join();

  const char* names[] = {"naive", "broken", "fixed", "seqcst"};
  printf("mode=%-6s iters=%llu violations=%llu%s\n", names[m], (unsigned long long)iters,
         (unsigned long long)violations, violations ? " (first at iter shown below)" : "");
  if (violations)
    printf("  first violation at iteration %llu -> LOST WAKEUP possible\n",
           (unsigned long long)first);
  return violations ? 1 : 0;
}
