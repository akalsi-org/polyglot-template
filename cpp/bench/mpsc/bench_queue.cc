// Benchmarks for the surviving queues: TwoPlaneRing and SpscRing.
//
// NOT part of the doctest unit-test path -- build and run by hand:
//
//   g++ -std=c++20 -O2 -DNDEBUG -march=x86-64-v3 -mtune=generic -mprfchw
//       -Wall -Wextra -I cpp/lib
//       cpp/bench/mpsc/bench_queue.cc cpp/lib/pgt/mpsc/queue.cc
//       cpp/lib/pgt/mpsc/region.cc cpp/lib/pgt/mpsc/policy.cc
//       -lpthread -o bench_queue         (one command line)
//   /tmp/bench_queue <mode> [args]      (run with no args for usage)
//
// -DNDEBUG IS NOT OPTIONAL, and -O2 does not imply it. Without it the debug
// misuse asserts run inside the measured loops -- including a gettid() syscall
// per commit -- which inflated an entire campaign by ~3.5x and INVERTED its
// headline result (clear-forward appeared to beat fetch_add at w=1; it does
// not). The #error below makes that mistake impossible rather than merely
// documented.
//
// Build a second binary with -DPGT_MPSC_NO_PREFETCH for the prefetch A/B.
//
// Modes:
//   ab <writers> <secs>          clear-forward vs fetch_add claim, 56B payload
//   writers <secs>               throughput at 1,2,4,8,16,32 writers
//   capacity <secs>              capacity sweep, 1 writer, 56B payload
//   sizes <secs>                 payload size sweep, 1 writer
//   latency <writers> <samples>  per-push rdtscp distribution
//   hops <writers> <secs>        walk hop histogram via onClaim
//   drain <mib>                  reader drain rate over a pre-filled ring
//
// The fetch_add comparison ring is FIXED-EXTENT ONLY (the reader zeroes retired
// descriptors, which only lands on descriptor boundaries when every record has
// the same extent) and has no identity, recovery, or misuse guards.  It is a
// deliberately stripped fixed-extent comparison, not a full queue substitute:
// the omitted safeguards favor fetch_add, while the 56-byte benchmark gives
// both implementations the same fixed extent.

// A debug build measures the asserts, not the queue. See the note above.
#ifndef NDEBUG
#error "bench_queue must be built with -DNDEBUG; -O2 alone does not define it"
#endif

#include "pgt/mpsc/queue.hh"
#include "pgt/mpsc/two_plane.hh"

#include <linux/perf_event.h>
#include <pthread.h>
#include <sched.h>
#include <sys/ioctl.h>
#include <sys/syscall.h>
#include <unistd.h>
#include <x86intrin.h>

#include <algorithm>
#include <atomic>
#include <chrono>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <fstream>
#include <map>
#include <string>
#include <thread>
#include <vector>

namespace {

using namespace pgt;
using namespace pgt::mpsc;

using Clock = std::chrono::steady_clock;

std::vector<unsigned> g_cpus;

[[noreturn]] void benchmarkError(char const* message) {
  std::fprintf(stderr, "benchmark setup failed: %s\n", message);
  std::exit(2);
}

void pinTo(unsigned cpu) {
  cpu_set_t set;
  CPU_ZERO(&set);
  CPU_SET(cpu, &set);
  int const rc = pthread_setaffinity_np(pthread_self(), sizeof(set), &set);
  if (rc != 0) {
    std::fprintf(stderr, "benchmark setup failed: could not pin to cpu %u: %s\n", cpu,
                 std::strerror(rc));
    std::exit(2);
  }
}

// Use only CPUs available to this process (which may be a cpuset subset), and
// put one logical CPU from each physical core before SMT siblings.  The old
// numeric mapping described one particular host, not this benchmark's host.
void discoverCpus() {
  cpu_set_t allowed;
  CPU_ZERO(&allowed);
  if (sched_getaffinity(0, sizeof(allowed), &allowed) != 0) {
    benchmarkError("sched_getaffinity failed");
  }

  std::map<std::string, std::vector<unsigned>> by_core;
  for (unsigned cpu = 0; cpu < CPU_SETSIZE; ++cpu) {
    if (!CPU_ISSET(cpu, &allowed)) continue;
    std::ifstream package("/sys/devices/system/cpu/cpu" + std::to_string(cpu) +
                          "/topology/physical_package_id");
    std::ifstream core("/sys/devices/system/cpu/cpu" + std::to_string(cpu) + "/topology/core_id");
    std::string package_id;
    std::string core_id;
    if (!(package >> package_id) || !(core >> core_id)) {
      benchmarkError("could not read CPU package/core topology");
    }
    by_core[package_id + ":" + core_id].push_back(cpu);
  }
  if (by_core.empty()) benchmarkError("the process has no allowed CPUs");

  for (auto const& [_, siblings] : by_core) g_cpus.push_back(siblings.front());
  for (auto const& [_, siblings] : by_core) {
    g_cpus.insert(g_cpus.end(), siblings.begin() + 1, siblings.end());
  }
}

void pinToSlot(unsigned slot) {
  if (slot >= g_cpus.size()) {
    std::fprintf(stderr, "benchmark setup failed: need %u CPUs, but only %zu are allowed\n",
                 slot + 1, g_cpus.size());
    std::exit(2);
  }
  pinTo(g_cpus[slot]);
}

double tscGhz() {
  auto const t0 = Clock::now();
  u64_t const c0 = __rdtsc();
  std::this_thread::sleep_for(std::chrono::milliseconds(200));
  u64_t const c1 = __rdtsc();
  auto const t1 = Clock::now();
  return static_cast<double>(c1 - c0) / std::chrono::duration<double, std::nano>(t1 - t0).count();
}

// ---------------------------------------------------------------------------
// harness
// ---------------------------------------------------------------------------

struct RunResult {
  u64_t records = 0;
  double secs = 0;
  std::vector<u64_t> lat;         // WRITE COST: the successful attempt only (1 in 64)
  std::vector<u64_t> adm;         // ADMISSION DELAY: retry wait before it (retried samples)
  std::vector<u64_t> per_writer;  // records per writer: starvation detector
  u64_t misses = 0;               // coherence line transfers, timed window only
  u64_t cycles = 0;
  u64_t insns = 0;
  u64_t sampled = 0;
  u64_t retried = 0;
};

double g_ghz = 0;  // TSC GHz, calibrated once in main

// The standard percentile set (design-owner request): p50/p95/p99/p99.99/max,
// always with the sample count -- a p99.99 from 10k samples is one observation
// and must be visibly weaker than one from 8M. Maxima on WSL2 are usually
// scheduler preemption, not queue behavior; samples beyond 8x p99 are counted
// and the max annotated so nobody sizes a latency budget from OS jitter.
// Caveat printed with every line: rdtscp sampling costs ~25 cycles, which
// inflates the LOW percentiles.
void printLat(char const* tag, std::vector<u64_t>& s, double ghz) {
  if (s.empty() || ghz <= 0) return;
  std::sort(s.begin(), s.end());
  auto pct = [&](double p) { return s[static_cast<sz_t>(p * (s.size() - 1))] / ghz; };
  double const p99 = pct(0.99);
  sz_t jitter = 0;
  for (auto it = s.rbegin(); it != s.rend() && *it / ghz > 8 * p99; ++it) ++jitter;
  std::printf(
    "  %-22s lat(ns) n=%-9zu p50=%-6.0f p95=%-6.0f p99=%-7.0f p99.99=%-8.0f "
    "max=%.0f%s%s [rdtscp ~25cyc overhead inflates low pcts]\n",
    tag, s.size(), pct(0.50), pct(0.95), p99, pct(0.9999), s.back() / ghz,
    jitter ? " (max: OS-jitter-suspect, >8x p99)" : "",
    s.size() < 100000 ? " (p99.99 weak: n<100k)" : "");
  std::fflush(stdout);
}

// Generic producer/consumer run over any queue with write()/reader ops.
// setup(w) runs once in each writer thread before the start barrier (writer
// registration for the variants that need it). tryOne(w) makes ONE admission
// attempt and returns success -- the retry loop lives HERE so sampled pushes
// can split WRITE COST (the successful attempt alone: reserve+memcpy+commit)
// from ADMISSION DELAY (the retry wait before it). Merged, a full-ring stall
// masquerades as queue latency: "p99 = 29us" reads as a slow queue when the
// truth may be "the write costs 200ns and the reader was 28us behind" -- a
// different conclusion pointing at a different fix.
// ---------------------------------------------------------------------------
// per-thread coherence counters (perf_event_open, no external perf tool)
// ---------------------------------------------------------------------------
//
// perf c2c is unavailable here: it needs AMD IBS precise memory sampling and no
// ibs_op PMU is exposed under WSL2. But the BASIC hardware counters are, and a
// two-thread probe shows PERF_COUNT_HW_CACHE_MISSES separates a shared line from
// private lines by ~35000x (8.27M vs 235 misses over 40M atomic RMWs). So these
// counters do observe coherence line transfers -- what they cannot do is
// attribute them to an ADDRESS. Per-thread totals plus variant ablation is the
// attribution method that remains.
//
// Counted per writer thread over the timed window only, enabled after the arm
// barrier and disabled at the stop edge. Reading costs two syscalls per thread
// per run, both outside the measured interval, so this perturbs nothing.
struct ThreadCounters {
  int misses = -1, cycles = -1, insns = -1;

  static int openEvent(u64_t config) noexcept {
    perf_event_attr attr{};
    attr.type = PERF_TYPE_HARDWARE;
    attr.size = sizeof attr;
    attr.config = config;
    attr.disabled = 1;
    attr.exclude_kernel = 1;  // user cycles only: kernel time is not the queue
    attr.exclude_hv = 1;
    return static_cast<int>(syscall(__NR_perf_event_open, &attr, 0, -1, -1, 0));
  }
  void open() noexcept {
    misses = openEvent(PERF_COUNT_HW_CACHE_MISSES);
    cycles = openEvent(PERF_COUNT_HW_CPU_CYCLES);
    insns = openEvent(PERF_COUNT_HW_INSTRUCTIONS);
  }
  void control(unsigned long req) noexcept {
    for (int fd : {misses, cycles, insns}) {
      if (fd >= 0) ioctl(fd, req, 0);
    }
  }
  void start() noexcept {
    control(PERF_EVENT_IOC_RESET);
    control(PERF_EVENT_IOC_ENABLE);
  }
  void stop() noexcept { control(PERF_EVENT_IOC_DISABLE); }
  static u64_t readOne(int fd) noexcept {
    long long v = 0;
    if (fd < 0 || read(fd, &v, sizeof v) != static_cast<ssize_t>(sizeof v)) return 0;
    return static_cast<u64_t>(v);
  }
  void collect(u64_t& m, u64_t& c, u64_t& i) noexcept {
    m = readOne(misses);
    c = readOne(cycles);
    i = readOne(insns);
    for (int fd : {misses, cycles, insns}) {
      if (fd >= 0) close(fd);
    }
  }
};

template <typename SetupFn, typename TryFn, typename DrainFn, typename ResetFn>
RunResult runFor(unsigned writers, double secs, SetupFn&& setup, TryFn&& tryOne,
                 DrainFn&& drainSome, ResetFn&& resetStats) {
  enum class Phase : unsigned char { kWait, kWarmup, kArmed, kMeasure, kStop };
  constexpr auto kWarmup = std::chrono::milliseconds(250);
  std::atomic<Phase> phase{Phase::kWait};
  std::atomic<unsigned> ready{0};
  std::atomic<unsigned> armed{0};
  std::atomic<unsigned> finished{0};
  std::vector<u64_t> counts(writers, 0);
  std::vector<std::vector<u64_t>> costs(writers), adms(writers);
  std::vector<u64_t> sampled(writers, 0), retried(writers, 0);
  std::vector<u64_t> ctr_m(writers, 0), ctr_c(writers, 0), ctr_i(writers, 0);
  std::vector<std::thread> ts;
  ts.reserve(writers);
  for (unsigned w = 0; w < writers; ++w) {
    ts.emplace_back([&, w] {
      pinToSlot(w + 1);  // slot 0 is the reader
      setup(w);
      // Opened on THIS thread: perf_event_open with pid=0 binds the counter to
      // the calling thread, so it must not be hoisted to the parent.
      ThreadCounters counters;
      counters.open();
      ready.fetch_add(1);
      while (phase.load(std::memory_order_acquire) == Phase::kWait) cpuRelax();
      u64_t n = 0;
      auto& cost = costs[w];
      auto& adm = adms[w];
      while (true) {
        Phase const current = phase.load(std::memory_order_relaxed);
        if (current == Phase::kStop) break;
        if (current == Phase::kWarmup) {
          while (!tryOne(w)) cpuRelax();
          continue;
        }
        if (current == Phase::kArmed) {
          armed.fetch_add(1, std::memory_order_release);
          while (phase.load(std::memory_order_acquire) == Phase::kArmed) cpuRelax();
          // Counters span exactly the timed window: started once the phase has
          // actually left kArmed, stopped at the kStop edge below. Warmup and
          // shutdown drain are excluded, matching `timed=`.
          counters.start();
          continue;
        }
        // The count and samples begin only after every writer is armed.  The
        // reader remains active through shutdown so a writer blocked at the
        // deadline can exit.
        if ((n & 63) == 0) {  // sample 1 in 64: percentiles at ~1.5% perturbation
          unsigned aux;
          u64_t const t0 = __rdtscp(&aux);
          u64_t ta = t0;  // start of the eventually-successful attempt
          while (!tryOne(w)) {
            cpuRelax();
            ta = __rdtscp(&aux);
          }
          u64_t const t1 = __rdtscp(&aux);
          cost.push_back(t1 - ta);
          ++sampled[w];
          if (ta != t0) {
            adm.push_back(ta - t0);
            ++retried[w];
          }
        } else {
          while (!tryOne(w)) cpuRelax();
        }
        ++n;
      }
      counters.stop();
      counters.collect(ctr_m[w], ctr_c[w], ctr_i[w]);
      counts[w] = n;
      finished.fetch_add(1, std::memory_order_release);
    });
  }
  pinToSlot(0);
  while (ready.load() != writers) cpuRelax();
  phase.store(Phase::kWarmup, std::memory_order_release);
  auto const warmup_start = Clock::now();
  while (Clock::now() - warmup_start < kWarmup) drainSome();

  phase.store(Phase::kArmed, std::memory_order_release);
  while (armed.load(std::memory_order_acquire) != writers) drainSome();
  resetStats();
  auto const timed_start = Clock::now();
  phase.store(Phase::kMeasure, std::memory_order_release);
  while (Clock::now() - timed_start < std::chrono::duration<double>(secs)) drainSome();
  auto const timed_end = Clock::now();
  phase.store(Phase::kStop, std::memory_order_release);

  // This is shutdown/drain work, deliberately outside the throughput interval.
  while (finished.load(std::memory_order_acquire) != writers) drainSome();
  for (auto& t : ts) t.join();
  RunResult r;
  for (auto c : counts) r.records += c;
  r.secs = std::chrono::duration<double>(timed_end - timed_start).count();
  for (auto& v : costs) r.lat.insert(r.lat.end(), v.begin(), v.end());
  for (auto& v : adms) r.adm.insert(r.adm.end(), v.begin(), v.end());
  r.per_writer = counts;
  for (auto v : ctr_m) r.misses += v;
  for (auto v : ctr_c) r.cycles += v;
  for (auto v : ctr_i) r.insns += v;
  for (auto s : sampled) r.sampled += s;
  for (auto s : retried) r.retried += s;
  return r;
}

template <typename Q>
void spinWrite(Q& q, void const* buf, sz_t n) {
  while (!q.write(buf, n)) {
    if (q.status() == Status::kReaderDead) {
      std::fprintf(stderr, "reader dead?\n");
      std::exit(2);
    }
    cpuRelax();
  }
}

// ONE admission attempt, for runFor's cost/admission split.
template <typename Q>
bool tryWrite(Q& q, void const* buf, sz_t n) {
  if (q.write(buf, n)) return true;
  if (q.status() == Status::kReaderDead) {
    std::fprintf(stderr, "reader dead?\n");
    std::exit(2);
  }
  return false;
}

// Drains up to `batch` records; returns how many.
template <typename Q>
u64_t drainBatch(Q& q, unsigned batch = 256) {
  u64_t n = 0;
  for (unsigned i = 0; i < batch; ++i) {
    auto s = q.peek();
    if (s.data() == nullptr) break;
    q.pop();
    ++n;
  }
  return n;
}

char payload_buf[65536];
volatile u64_t g_payload_sink = 0;

u64_t touchPayload(ReadSpan const& payload) {
  u64_t sum = 0;
  for (sz_t i = 0; i < payload.size(); ++i) {
    sum += std::to_integer<unsigned char>(payload.data()[i]);
  }
  return sum;
}

template <bool Padded, typename PolicyT = DefaultPolicy>
RunResult benchTwoPlane(sz_t capacity, unsigned writers, double secs, sz_t payload,
                        PolicyT policy = {}) {
  using Q = TwoPlaneRing<PolicyT, Padded>;
  Q q(policy);
  if (!Q::create(Config{.capacity = capacity}, q)) {
    std::fprintf(stderr, "region create failed\n");
    std::exit(1);
  }
  if (!q.attachReader()) std::exit(1);
  return runFor(
    writers, secs,
    [&](unsigned writer) {
      if constexpr (requires(PolicyT& p, unsigned w) { p.bindWriter(w); }) {
        q.policy().bindWriter(writer);
      }
    },
    [&](unsigned) { return tryWrite(q, payload_buf, payload); }, [&] { return drainBatch(q); },
    [&] {
      if constexpr (requires(PolicyT& p) { p.reset(); }) q.policy().reset();
    });
}

// SpscRing has no claim arbitration, so it is the floor a multi-producer design
// is measured against rather than a competitor: one writer, wait-free, and the
// reader sweeps one ring.
RunResult benchSpsc(sz_t capacity, double secs, sz_t payload) {
  Spsc q;
  Config cfg;
  cfg.capacity = capacity;
  if (!Spsc::create(cfg, q)) {
    std::fprintf(stderr, "region create failed\n");
    std::exit(1);
  }
  if (!q.attachReader()) std::exit(1);
  return runFor(
    1, secs, [&](unsigned) { (void)q.attachWriter(); },
    [&](unsigned) { return tryWrite(q, payload_buf, payload); }, [&] { return drainBatch(q); },
    [&] {});
}

void reportCountersJson(char const* variant, unsigned writers, RunResult const& r) {
  double const per_rec = r.records ? static_cast<double>(r.misses) / r.records : 0.0;
  double const cyc_rec = r.records ? static_cast<double>(r.cycles) / r.records : 0.0;
  double const ins_rec = r.records ? static_cast<double>(r.insns) / r.records : 0.0;
  u64_t lo = ~u64_t{0}, hi = 0;
  for (u64_t c : r.per_writer) {
    lo = c < lo ? c : lo;
    hi = c > hi ? c : hi;
  }
  if (r.per_writer.empty()) lo = 0;
  std::printf(
    "{\"mode\":\"counters\",\"variant\":\"%s\",\"writers\":%u,\"seconds\":%.9f,"
    "\"records\":%llu,\"mrec_s\":%.9f,\"misses\":%llu,\"misses_per_record\":%.6f,"
    "\"cycles_per_record\":%.3f,\"insns_per_record\":%.3f,\"ipc\":%.4f,"
    "\"fairness_min_max\":%.6f,\"min_writer_records\":%llu,\"max_writer_records\":%llu}\n",
    variant, writers, r.secs, static_cast<unsigned long long>(r.records), r.records / r.secs / 1e6,
    static_cast<unsigned long long>(r.misses), per_rec, cyc_rec, ins_rec,
    r.cycles ? static_cast<double>(r.insns) / r.cycles : 0.0,
    hi ? static_cast<double>(lo) / hi : 0.0, static_cast<unsigned long long>(lo),
    static_cast<unsigned long long>(hi));
  std::fflush(stdout);
}

void reportThroughputJson(char const* variant, unsigned writers, RunResult const& r) {
  std::printf(
    "{\"mode\":\"throughput\",\"variant\":\"%s\",\"writers\":%u,"
    "\"seconds\":%.9f,\"records\":%llu,\"mrec_s\":%.9f}\n",
    variant, writers, r.secs, static_cast<unsigned long long>(r.records), r.records / r.secs / 1e6);
}

template <typename Q>
RunResult benchQueue(Q& q, unsigned writers, double secs, sz_t payload, bool attach) {
  if (!q.attachReader()) {
    std::fprintf(stderr, "attachReader failed\n");
    std::exit(1);
  }
  return runFor(
    writers, secs,
    [&](unsigned) {
      if (attach && !q.attachWriter()) {
        std::fprintf(stderr, "attachWriter failed (too few shards?)\n");
        std::exit(1);
      }
    },
    [&](unsigned) { return tryWrite(q, payload_buf, payload); }, [&] { return drainBatch(q); },
    [] {});
}

void report(char const* name, unsigned writers, sz_t payload, RunResult r) {
  double const mrps = r.records / r.secs / 1e6;
  double const gbs = r.records / r.secs * extentFor(payload) / 1e9;
  std::printf("%-24s w=%-2u payload=%-6zu timed=%.3fs %10.2f Mrec/s  %8.2f GB/s  %8.1f ns/rec\n",
              name, writers, payload, r.secs, mrps, gbs, 1e3 / mrps);
  std::fflush(stdout);
  printLat("write-cost", r.lat, g_ghz);
  if (r.sampled != 0) {
    std::printf("  admission-retried      %.2f%% of %llu sampled pushes\n",
                100.0 * r.retried / r.sampled, static_cast<unsigned long long>(r.sampled));
  }
  printLat("admission-delay", r.adm, g_ghz);
}

int usage() {
  std::fprintf(stderr,
               "usage: bench_queue <mode> [args]\n"
               "  throughput   two-plane|two-plane-padded|spsc <writers> <secs>\n"
               "  counters     two-plane|two-plane-padded|spsc <writers> <secs>\n"
               "               adds per-thread coherence counters and the fairness ratio\n"
               "  latency      two-plane|two-plane-padded|spsc <writers> <secs>\n"
               "  sizes        <secs>    payload size sweep, one writer\n");
  return 1;
}

// One dispatch table for every mode: the variant name is resolved once and the
// mode decides only how to REPORT. Keeping selection in one place is what stops
// a throughput run and a counters run from silently measuring different queues,
// which is exactly how an earlier campaign compared two different builds.
template <typename Report>
int dispatch(std::string const& variant, unsigned writers, double secs, Report&& report) {
  constexpr sz_t kCap = 1u << 20;
  constexpr sz_t kPayload = 56;
  if (variant == "two-plane") {
    report("two-plane", benchTwoPlane<false>(kCap, writers, secs, kPayload));
  } else if (variant == "two-plane-padded") {
    report("two-plane-padded", benchTwoPlane<true>(kCap, writers, secs, kPayload));
  } else if (variant == "spsc") {
    // One writer by construction; a request for more is a usage error rather
    // than something to silently clamp.
    if (writers != 1) {
      std::fprintf(stderr, "spsc takes exactly one writer\n");
      return 1;
    }
    report("spsc", benchSpsc(kCap, secs, kPayload));
  } else {
    return usage();
  }
  return 0;
}

}  // namespace

int main(int argc, char** argv) {
  discoverCpus();
  g_ghz = tscGhz();
  std::string const mode = argc > 1 ? argv[1] : "";
  std::string const variant = argc > 2 ? argv[2] : "";
  unsigned const writers = argc > 3 ? static_cast<unsigned>(std::atoi(argv[3])) : 0;
  double const secs = argc > 4 ? std::atof(argv[4]) : 0;

  if (mode == "sizes") {
    double const s = argc > 2 ? std::atof(argv[2]) : 0;
    if (s <= 0) return usage();
    for (sz_t n : {8u, 56u, 120u, 248u, 1016u, 4088u}) {
      report("two-plane", 1, n, benchTwoPlane<false>(1u << 20, 1, s, n));
    }
    return 0;
  }

  if (writers == 0 || secs <= 0) return usage();

  if (mode == "throughput") {
    return dispatch(variant, writers, secs,
                    [&](char const* name, RunResult r) { reportThroughputJson(name, writers, r); });
  }
  if (mode == "counters") {
    return dispatch(variant, writers, secs,
                    [&](char const* name, RunResult r) { reportCountersJson(name, writers, r); });
  }
  if (mode == "latency") {
    return dispatch(variant, writers, secs,
                    [&](char const* name, RunResult r) { report(name, writers, 56, r); });
  }
  return usage();
}
