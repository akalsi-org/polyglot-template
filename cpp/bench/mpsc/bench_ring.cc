// Benchmarks for the shared-ring MPSC queue (pgt::mpsc::Ring).
//
// NOT part of the doctest unit-test path -- build and run by hand:
//
//   g++ -std=c++20 -O2 -DNDEBUG -march=native -Wall -Wextra -I cpp/lib
//       cpp/bench/mpsc/bench_ring.cc cpp/lib/pgt/mpsc/queue.cc
//       cpp/lib/pgt/mpsc/region.cc cpp/lib/pgt/mpsc/policy.cc
//       -lpthread -o bench_ring          (one command line)
//   /tmp/bench_ring <mode> [args]      (run with no args for usage)
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
#error "bench_ring must be built with -DNDEBUG; -O2 alone does not define it"
#endif

#include "pgt/mpsc/queue.hh"

#include <pthread.h>
#include <sched.h>
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
    std::ifstream core("/sys/devices/system/cpu/cpu" + std::to_string(cpu) +
                       "/topology/core_id");
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
    std::fprintf(stderr, "benchmark setup failed: need %u CPUs, but only %zu are allowed\n", slot + 1,
                 g_cpus.size());
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
  return static_cast<double>(c1 - c0) /
         std::chrono::duration<double, std::nano>(t1 - t0).count();
}

// ---------------------------------------------------------------------------
// fetch_add comparison ring (bench-local; see file comment for what it omits)
// ---------------------------------------------------------------------------

class FetchAddRing {
 public:
  bool create(sz_t capacity) {
    Config cfg;
    cfg.capacity = capacity;
    return Region::create(cfg, region_);
  }

  bool write(void const* data, sz_t n) {
    sz_t const need = extentFor(n);
    auto cursor = std::atomic_ref<u64_t>(region_.shard(0)->publishPos());
    auto read_pos = std::atomic_ref<u64_t>(region_.shard(0)->read_pos);
    u64_t const p = cursor.fetch_add(need, std::memory_order_relaxed);  // claim: one RMW
    // fetch_add cannot decline: once claimed the region is part of the byte
    // sequence, so on backpressure the writer can only wait for space.
    while (p + need - read_pos.load(std::memory_order_acquire) > region_.capacity()) {
      cpuRelax();
    }
    std::byte* const base = region_.arena(0);
    u64_t const mask = region_.mask();
    std::memcpy(base + ((p + kHeaderSize) & mask), data, n);
    std::atomic_ref<u64_t>(*reinterpret_cast<u64_t*>(base + (p & mask)))
        .store(packRecord(need, static_cast<u32_t>(n & (kGrain - 1)), State::kCommitted, 0),
               std::memory_order_release);
    return true;
  }

  // Returns record payload length, or ~0ull when nothing is ready.
  u64_t tryPop() {
    std::byte* const base = region_.arena(0);
    u64_t const mask = region_.mask();
    auto desc = std::atomic_ref<u64_t>(*reinterpret_cast<u64_t*>(base + (rd_ & mask)));
    u64_t const d = desc.load(std::memory_order_acquire);
    if (d == 0) return ~u64_t{0};
    u64_t const len = committedLen(d);
    desc.store(0, std::memory_order_relaxed);  // re-arm for the next lap
    rd_ += extentOf(d);
    std::atomic_ref<u64_t>(region_.shard(0)->read_pos)
        .store(rd_, std::memory_order_release);
    return len;
  }

 private:
  Region region_;
  u64_t rd_ = 0;
};

// ---------------------------------------------------------------------------
// hop-histogram policy (a handle: shared state behind a pointer)
// ---------------------------------------------------------------------------

struct HopsState {
  u64_t hist[64]{};
};

struct HopsPolicy : DefaultPolicy {
  std::vector<HopsState>* states = nullptr;
  inline static thread_local HopsState* local = nullptr;

  void bindWriter(unsigned writer) noexcept { local = &(*states)[writer]; }
  void reset() noexcept {
    for (auto& state : *states) {
      for (u64_t& count : state.hist) count = 0;
    }
  }
  void onClaim(u64_t, u64_t, u32_t hops) noexcept { ++local->hist[hops < 64 ? hops : 63]; }
  void onBusy(u32_t) noexcept { cpuRelax(); }
  void onContended(u32_t) noexcept { cpuRelax(); }
};

// ---------------------------------------------------------------------------
// harness
// ---------------------------------------------------------------------------

struct RunResult {
  u64_t records = 0;
  double secs = 0;
  std::vector<u64_t> lat;  // WRITE COST: the successful attempt only (1 in 64)
  std::vector<u64_t> adm;  // ADMISSION DELAY: retry wait before it (retried samples)
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
  std::printf("  %-22s lat(ns) n=%-9zu p50=%-6.0f p95=%-6.0f p99=%-7.0f p99.99=%-8.0f "
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
  std::vector<std::thread> ts;
  ts.reserve(writers);
  for (unsigned w = 0; w < writers; ++w) {
    ts.emplace_back([&, w] {
      pinToSlot(w + 1);  // slot 0 is the reader
      setup(w);
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

template <typename PolicyT = DefaultPolicy>
RunResult benchRing(sz_t capacity, unsigned writers, double secs, sz_t payload,
                    PolicyT policy = {}) {
  Ring<PolicyT> q(policy);
  if (!Ring<PolicyT>::create(Config{.capacity = capacity}, q)) {
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

// Any QueueLike variant. attach=true registers each writer thread first.
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
      [&](unsigned) { return tryWrite(q, payload_buf, payload); },
      [&] { return drainBatch(q); }, [] {});
}

template <typename Q>
RunResult makeAndBench(sz_t cap_per_shard, u32_t shards, unsigned writers, double secs,
                       sz_t payload, bool attach) {
  Q q;
  if (!Q::create(Config{.capacity = cap_per_shard, .shards = shards}, q)) {
    std::fprintf(stderr, "create failed (K=%u)\n", shards);
    std::exit(1);
  }
  return benchQueue(q, writers, secs, payload, attach);
}

RunResult benchFetchAdd(sz_t capacity, unsigned writers, double secs, sz_t payload) {
  FetchAddRing q;
  if (!q.create(capacity)) std::exit(1);
  // NOTE on the split: fetch_add CANNOT decline admission, so its backpressure
  // wait happens INSIDE write() and lands in write-cost, never in
  // admission-delay. That is not a harness artifact -- it is the design
  // difference itself (see "Why not fetch_add" in the spec).
  return runFor(
      writers, secs, [](unsigned) {}, [&](unsigned) { return q.write(payload_buf, payload); },
      [&] {
        u64_t n = 0;
        for (unsigned i = 0; i < 256; ++i) {
          if (q.tryPop() == ~u64_t{0}) break;
          ++n;
        }
        return n;
      }, [] {});
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
               "usage: bench_ring ab|writers|capacity|sizes|latency|hops|drain|"
               "variants|shardsweep|k1|sweepcost|platency [args]\n");
  return 1;
}

// Per-push latency for any variant: rdtscp around every write. Percentiles are
// computed over the pooled samples of all writers.
template <typename Q>
void pushLatency(char const* name, u32_t shards, unsigned writers, sz_t samples,
                 bool attach, double ghz) {
  Q q;
  if (!Q::create(Config{.capacity = 1u << 20, .shards = shards}, q)) std::exit(1);
  if (!q.attachReader()) std::exit(1);
  std::vector<std::vector<u64_t>> lat(writers);
  std::atomic<unsigned> done{0};
  std::vector<std::thread> ts;
  for (unsigned w = 0; w < writers; ++w) {
    ts.emplace_back([&, w] {
      pinToSlot(w + 1);
      if (attach && !q.attachWriter()) std::exit(1);
      auto& v = lat[w];
      v.reserve(samples);
      unsigned aux;
      for (sz_t i = 0; i < samples; ++i) {
        u64_t const c0 = __rdtscp(&aux);
        spinWrite(q, payload_buf, 56);
        u64_t const c1 = __rdtscp(&aux);
        v.push_back(c1 - c0);
      }
      done.fetch_add(1);
    });
  }
  pinToSlot(0);
  while (done.load() != writers) drainBatch(q);
  for (auto& t : ts) t.join();
  std::vector<u64_t> all;
  for (auto& v : lat) all.insert(all.end(), v.begin(), v.end());
  std::printf("%-12s K=%-3u w=%-2u push-latency:\n", name, shards, writers);
  printLat("push(every op)", all, ghz);
}

// Delivery latency under SPARSE traffic: each writer stamps rdtsc into the
// payload and throttles to ~1 record per `gap_ns`; the reader sweeps K shards
// and measures stamp-to-delivery. This prices the sweep, not the ring.
void sweepCost(u32_t shards, unsigned writers, double secs, double ghz) {
  MultiSpsc q;
  if (!MultiSpsc::create(Config{.capacity = 1u << 20, .shards = shards}, q)) std::exit(1);
  if (!q.attachReader()) std::exit(1);
  double const gap_ns = 5000.0;
  u64_t const gap_cyc = static_cast<u64_t>(gap_ns * ghz);
  std::atomic<bool> stop{false};
  std::vector<std::thread> ts;
  for (unsigned w = 0; w < writers; ++w) {
    ts.emplace_back([&, w] {
      pinToSlot(w + 1);
      if (!q.attachWriter()) std::exit(1);
      while (!stop.load(std::memory_order_relaxed)) {
        u64_t stamp[7];  // 56 bytes
        stamp[0] = __rdtsc();
        while (!q.write(stamp, 56)) cpuRelax();
        u64_t const next = stamp[0] + gap_cyc;
        while (__rdtsc() < next && !stop.load(std::memory_order_relaxed)) cpuRelax();
      }
    });
  }
  pinToSlot(0);
  std::vector<u64_t> deltas;
  deltas.reserve(4'000'000);
  auto const t0 = Clock::now();
  while (std::chrono::duration<double>(Clock::now() - t0).count() < secs) {
    auto s = q.peek();
    if (s.data() == nullptr) continue;
    u64_t stamp;
    std::memcpy(&stamp, s.data(), 8);
    deltas.push_back(__rdtsc() - stamp);
    q.pop();
  }
  stop.store(true);
  for (auto& t : ts) t.join();
  while (q.peek().data() != nullptr) q.pop();
  std::printf("sweepcost K=%-4u active-writers=%u delivery:\n", shards, writers);
  printLat("stamp-to-delivery", deltas, ghz);
}

}  // namespace

int main(int argc, char** argv) {
  if (argc < 2) return usage();
  discoverCpus();
  std::string const mode = argv[1];
  std::memset(payload_buf, 0x5a, sizeof(payload_buf));
  g_ghz = tscGhz();

  if (mode == "ab") {
    unsigned const writers = argc > 2 ? std::atoi(argv[2]) : 4;
    double const secs = argc > 3 ? std::atof(argv[3]) : 2.0;
    sz_t const cap = 1u << 20;
    report("clear-forward", writers, 56, benchRing(cap, writers, secs, 56));
    report("fetch_add", writers, 56, benchFetchAdd(cap, writers, secs, 56));
    return 0;
  }

  if (mode == "writers") {
    double const secs = argc > 2 ? std::atof(argv[2]) : 2.0;
    for (unsigned w : {1u, 2u, 4u, 8u, 16u, 32u}) {
      report("ring", w, 56, benchRing(1u << 20, w, secs, 56));
    }
    return 0;
  }

  if (mode == "capacity") {
    double const secs = argc > 2 ? std::atof(argv[2]) : 2.0;
    for (sz_t cap : {sz_t{256} << 10, sz_t{1} << 20, sz_t{4} << 20, sz_t{16} << 20,
                     sz_t{64} << 20, sz_t{256} << 20}) {
      char name[64];
      std::snprintf(name, sizeof(name), "cap=%zuKiB", cap >> 10);
      report(name, 1, 56, benchRing(cap, 1, secs, 56));
    }
    return 0;
  }

  if (mode == "sizes") {
    double const secs = argc > 2 ? std::atof(argv[2]) : 1.5;
    for (sz_t p : {sz_t{8}, sz_t{32}, sz_t{56}, sz_t{64}, sz_t{120}, sz_t{128}, sz_t{248},
                   sz_t{256}, sz_t{504}, sz_t{1016}, sz_t{2040}, sz_t{4088}, sz_t{8184},
                   sz_t{16376}}) {
      report("ring", 1, p, benchRing(1u << 22, 1, secs, p));
    }
    return 0;
  }

  if (mode == "latency") {
    unsigned const writers = argc > 2 ? std::atoi(argv[2]) : 1;
    sz_t const samples = argc > 3 ? std::atoll(argv[3]) : 2'000'000;
    double const ghz = tscGhz();
    Ring<> q;
    if (!Ring<>::create(Config{.capacity = 1u << 20}, q)) return 1;
    if (!q.attachReader()) return 1;
    std::vector<std::vector<u64_t>> lat(writers);
    std::atomic<bool> stop{false};
    std::atomic<unsigned> done{0};
    std::vector<std::thread> ts;
    for (unsigned w = 0; w < writers; ++w) {
      ts.emplace_back([&, w] {
        pinToSlot(w + 1);
        auto& v = lat[w];
        v.reserve(samples);
        unsigned aux;
        for (sz_t i = 0; i < samples; ++i) {
          u64_t const c0 = __rdtscp(&aux);
          spinWrite(q, payload_buf, 56);
          u64_t const c1 = __rdtscp(&aux);
          v.push_back(c1 - c0);
        }
        done.fetch_add(1);
      });
    }
    pinToSlot(0);
    while (done.load() != writers) drainBatch(q);
    stop.store(true);
    for (auto& t : ts) t.join();
    std::vector<u64_t> all;
    for (auto& v : lat) all.insert(all.end(), v.begin(), v.end());
    std::printf("latency w=%u:\n", writers);
    printLat("push(every op)", all, ghz);
    return 0;
  }

  if (mode == "hops") {
    unsigned const writers = argc > 2 ? std::atoi(argv[2]) : 8;
    double const secs = argc > 3 ? std::atof(argv[3]) : 2.0;
    std::vector<HopsState> states(writers);
    HopsPolicy pol;
    pol.states = &states;
    auto r = benchRing<HopsPolicy>(1u << 20, writers, secs, 56, pol);
    report("ring+hops", writers, 56, r);
    u64_t hist[64]{};
    for (auto const& state : states) {
      for (unsigned i = 0; i < 64; ++i) hist[i] += state.hist[i];
    }
    u64_t total = 0;
    for (u64_t c : hist) total += c;
    std::printf("hops histogram (claims=%llu):\n", (unsigned long long)total);
    for (unsigned i = 0; i < 64; ++i) {
      u64_t const c = hist[i];
      if (c) std::printf("  %2u hops: %10llu  (%.3f%%)\n", i, (unsigned long long)c,
                         100.0 * c / total);
    }
    return 0;
  }

  if (mode == "drain") {
    sz_t const mib = argc > 2 ? std::atoll(argv[2]) : 256;
    sz_t const cap = mib << 20;
    Ring<> q;
    if (!Ring<>::create(Config{.capacity = cap}, q)) return 1;
    if (!q.attachReader()) return 1;
    pinToSlot(0);
    u64_t filled = 0;
    while (q.write(payload_buf, 56)) ++filled;  // fill to backpressure
    pinToSlot(0);
    auto const t0 = Clock::now();
    u64_t drained = 0;
    u64_t payload_sum = 0;
    while (true) {
      auto s = q.peek();
      if (s.data() == nullptr) break;
      payload_sum += touchPayload(s);
      q.pop();
      ++drained;
    }
    g_payload_sink = payload_sum;  // retain the payload reads in the timed loop
    auto const dt = std::chrono::duration<double>(Clock::now() - t0).count();
    double const payload_mib = drained * 56.0 / (1u << 20);
    std::printf("payload-touch drain: %llu records (%.2f MiB payload from %zu MiB ring) in %.3fs = "
                "%.2f Mrec/s, %.2f MiB/s, %.2f ns/rec\n",
                (unsigned long long)drained, payload_mib, mib, dt, drained / dt / 1e6,
                payload_mib / dt, dt * 1e9 / drained);
    return 0;
  }

  if (mode == "variants") {
    double const secs = argc > 2 ? std::atof(argv[2]) : 2.0;
    report("spsc", 1, 56, makeAndBench<Spsc>(1u << 20, 1, 1, secs, 56, true));
    for (unsigned w : {1u, 2u, 4u, 8u, 16u}) {
      report("mpsc", w, 56, benchRing(1u << 20, w, secs, 56));
      u32_t const k_half = w > 1 ? w / 2 : 1;
      char name[64];
      std::snprintf(name, sizeof(name), "sharded-mpsc K=%u", k_half);
      report(name, w, 56, makeAndBench<ShardedMpsc>(1u << 20, k_half, w, secs, 56, true));
      std::snprintf(name, sizeof(name), "sharded-mpsc K=%u", w);
      report(name, w, 56, makeAndBench<ShardedMpsc>(1u << 20, w, w, secs, 56, true));
      std::snprintf(name, sizeof(name), "multi-spsc K=%u", w);
      report(name, w, 56, makeAndBench<MultiSpsc>(1u << 20, w, w, secs, 56, true));
    }
    return 0;
  }

  // One configuration per process invocation: the unit of the repetition
  // methodology. Statistical campaigns run N independent PROCESSES per config,
  // interleaved across configs by the driver script -- process-level variance
  // is the thing being measured around, so in-process repetition is not a
  // substitute.
  if (mode == "one") {
    std::string const which = argc > 2 ? argv[2] : "";
    u32_t const shards = argc > 3 ? std::atoi(argv[3]) : 1;
    unsigned const w = argc > 4 ? std::atoi(argv[4]) : 1;
    double const secs = argc > 5 ? std::atof(argv[5]) : 2.0;
    if (which == "cf") {
      report("clear-forward", w, 56, benchRing(1u << 20, w, secs, 56));
    } else if (which == "fa") {
      report("fetch_add", w, 56, benchFetchAdd(1u << 20, w, secs, 56));
    } else if (which == "shmpsc") {
      report("sharded-mpsc", w, 56, makeAndBench<ShardedMpsc>(1u << 20, shards, w, secs, 56, true));
    } else if (which == "multi") {
      report("multi-spsc", w, 56, makeAndBench<MultiSpsc>(1u << 20, shards, w, secs, 56, true));
    } else {
      return usage();
    }
    return 0;
  }

  if (mode == "shardsweep") {
    unsigned const total = argc > 2 ? std::atoi(argv[2]) : 16;
    double const secs = argc > 3 ? std::atof(argv[3]) : 2.0;
    for (u32_t k = 1; k <= total; k *= 2) {
      char name[64];
      std::snprintf(name, sizeof(name), "shmpsc K=%-3u wps=%u", k, total / k);
      report(name, total, 56, makeAndBench<ShardedMpsc>(1u << 20, k, total, secs, 56, true));
    }
    return 0;
  }

  if (mode == "k1") {
    double const secs = argc > 2 ? std::atof(argv[2]) : 2.0;
    for (unsigned w : {1u, 2u}) {
      report("bare ring", w, 56, benchRing(1u << 20, w, secs, 56));
      report("sharded K=1", w, 56, makeAndBench<ShardedMpsc>(1u << 20, 1, w, secs, 56, true));
    }
    return 0;
  }

  if (mode == "sweepcost") {
    double const secs = argc > 2 ? std::atof(argv[2]) : 3.0;
    double const ghz = tscGhz();
    for (u32_t k : {8u, 32u, 128u}) sweepCost(k, 2, secs, ghz);
    return 0;
  }

  if (mode == "platency") {
    std::string const variant = argc > 2 ? argv[2] : "mpsc";
    unsigned const writers = argc > 3 ? std::atoi(argv[3]) : 1;
    sz_t const samples = argc > 4 ? std::atoll(argv[4]) : 1'000'000;
    double const ghz = tscGhz();
    if (variant == "mpsc") {
      pushLatency<Mpsc>("mpsc", 1, writers, samples, false, ghz);
    } else if (variant == "shmpsc") {  // recommended config: 2 writers per shard
      pushLatency<ShardedMpsc>("shmpsc", writers > 1 ? writers / 2 : 1, writers, samples,
                               true, ghz);
    } else if (variant == "multi") {
      pushLatency<MultiSpsc>("multi", writers, writers, samples, true, ghz);
    } else if (variant == "spsc") {
      pushLatency<Spsc>("spsc", 1, 1, samples, true, ghz);
    } else {
      return usage();
    }
    return 0;
  }

  return usage();
}
