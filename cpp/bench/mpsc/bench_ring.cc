// Benchmarks for the shared-ring MPSC queue (pgt::mpsc::Ring).
//
// NOT part of the doctest unit-test path -- build and run by hand:
//
//   g++ -std=c++20 -O2 -march=native -Wall -Wextra -I cpp/lib
//       cpp/bench/mpsc/bench_ring.cc cpp/lib/pgt/mpsc/queue.cc
//       cpp/lib/pgt/mpsc/region.cc cpp/lib/pgt/mpsc/policy.cc
//       -lpthread -o bench_ring          (one command line)
//   /tmp/bench_ring <mode> [args]      (run with no args for usage)
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
// the same extent) and has no identity, no recovery, and no misuse guards.
// That is the point: it prices the claim mechanism alone, and the restriction
// FAVORS fetch_add.

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
#include <string>
#include <thread>
#include <vector>

namespace {

using namespace pgt;
using namespace pgt::mpsc;

using Clock = std::chrono::steady_clock;

void pinTo(unsigned cpu) {
  cpu_set_t set;
  CPU_ZERO(&set);
  CPU_SET(cpu, &set);
  if (pthread_setaffinity_np(pthread_self(), sizeof(set), &set) != 0) {
    std::fprintf(stderr, "warning: could not pin to cpu %u\n", cpu);
  }
}

// SMT siblings enumerate as (0,1),(2,3),... here (verified via topology);
// spread threads across physical cores first.
unsigned cpuForSlot(unsigned slot) {
  unsigned const cores = 12;
  return (slot < cores) ? slot * 2 : (slot - cores) * 2 + 1;
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
  std::atomic<u64_t> hist[64];
};

struct HopsPolicy : DefaultPolicy {
  HopsState* state = nullptr;
  void onClaim(u64_t, u64_t, u32_t hops) noexcept {
    state->hist[hops < 64 ? hops : 63].fetch_add(1, std::memory_order_relaxed);
  }
  void onBusy(u32_t) noexcept { cpuRelax(); }
  void onContended(u32_t) noexcept { cpuRelax(); }
};

// ---------------------------------------------------------------------------
// harness
// ---------------------------------------------------------------------------

struct RunResult {
  u64_t records = 0;
  double secs = 0;
  std::vector<u64_t> lat;  // pooled per-push rdtscp samples (1 in 64 pushes)
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
// registration for the variants that need it).
template <typename SetupFn, typename WriteFn, typename DrainFn>
RunResult runFor(unsigned writers, double secs, SetupFn&& setup, WriteFn&& writeOne,
                 DrainFn&& drainSome) {
  std::atomic<bool> stop{false};
  std::atomic<unsigned> ready{0};
  std::atomic<bool> go{false};
  std::atomic<unsigned> finished{0};
  std::vector<u64_t> counts(writers, 0);
  std::vector<std::vector<u64_t>> lats(writers);
  std::vector<std::thread> ts;
  ts.reserve(writers);
  for (unsigned w = 0; w < writers; ++w) {
    ts.emplace_back([&, w] {
      pinTo(cpuForSlot(w + 1));  // slot 0 is the reader
      setup(w);
      ready.fetch_add(1);
      while (!go.load(std::memory_order_acquire)) cpuRelax();
      u64_t n = 0;
      auto& lat = lats[w];
      while (!stop.load(std::memory_order_relaxed)) {
        if ((n & 63) == 0) {  // sample 1 in 64: percentiles at ~1.5% perturbation
          unsigned aux;
          u64_t const c0 = __rdtscp(&aux);
          writeOne(w);
          lat.push_back(__rdtscp(&aux) - c0);
        } else {
          writeOne(w);
        }
        ++n;
      }
      counts[w] = n;
      finished.fetch_add(1, std::memory_order_release);
    });
  }
  pinTo(cpuForSlot(0));
  while (ready.load() != writers) cpuRelax();
  auto const t0 = Clock::now();
  go.store(true, std::memory_order_release);
  while (std::chrono::duration<double>(Clock::now() - t0).count() < secs) {
    drainSome();
  }
  stop.store(true, std::memory_order_relaxed);
  // Keep draining until every writer has exited: one may be blocked on a full
  // ring and needs the reader to make room before it can see the stop flag.
  while (finished.load(std::memory_order_acquire) != writers) drainSome();
  for (auto& t : ts) t.join();
  auto const t1 = Clock::now();
  RunResult r;
  for (auto c : counts) r.records += c;
  r.secs = std::chrono::duration<double>(t1 - t0).count();
  for (auto& v : lats) r.lat.insert(r.lat.end(), v.begin(), v.end());
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
      writers, secs, [](unsigned) {}, [&](unsigned) { spinWrite(q, payload_buf, payload); },
      [&] { return drainBatch(q); });
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
      [&](unsigned) { spinWrite(q, payload_buf, payload); }, [&] { return drainBatch(q); });
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
  return runFor(
      writers, secs, [](unsigned) {}, [&](unsigned) { q.write(payload_buf, payload); },
      [&] {
        u64_t n = 0;
        for (unsigned i = 0; i < 256; ++i) {
          if (q.tryPop() == ~u64_t{0}) break;
          ++n;
        }
        return n;
      });
}

void report(char const* name, unsigned writers, sz_t payload, RunResult r) {
  double const mrps = r.records / r.secs / 1e6;
  double const gbs = r.records / r.secs * extentFor(payload) / 1e9;
  std::printf("%-24s w=%-2u payload=%-6zu %10.2f Mrec/s  %8.2f GB/s  %8.1f ns/rec\n", name,
              writers, payload, mrps, gbs, 1e3 / mrps);
  std::fflush(stdout);
  printLat("push", r.lat, g_ghz);
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
      pinTo(cpuForSlot(w + 1));
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
  pinTo(cpuForSlot(0));
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
      pinTo(cpuForSlot(w + 1));
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
  pinTo(cpuForSlot(0));
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
        pinTo(cpuForSlot(w + 1));
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
    pinTo(cpuForSlot(0));
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
    static HopsState hs{};
    HopsPolicy pol;
    pol.state = &hs;
    auto r = benchRing<HopsPolicy>(1u << 20, writers, secs, 56, pol);
    report("ring+hops", writers, 56, r);
    u64_t total = 0;
    for (auto& h : hs.hist) total += h.load();
    std::printf("hops histogram (claims=%llu):\n", (unsigned long long)total);
    for (unsigned i = 0; i < 64; ++i) {
      u64_t const c = hs.hist[i].load();
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
    pinTo(cpuForSlot(1));
    u64_t filled = 0;
    while (q.write(payload_buf, 56)) ++filled;  // fill to backpressure
    pinTo(cpuForSlot(0));
    auto const t0 = Clock::now();
    u64_t drained = 0;
    while (true) {
      auto s = q.peek();
      if (s.data() == nullptr) break;
      q.pop();
      ++drained;
    }
    auto const dt = std::chrono::duration<double>(Clock::now() - t0).count();
    std::printf("drain: %llu records (%zu MiB ring) in %.3fs = %.2f Mrec/s, %.2f ns/rec\n",
                (unsigned long long)drained, mib, dt, drained / dt / 1e6,
                dt * 1e9 / drained);
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
