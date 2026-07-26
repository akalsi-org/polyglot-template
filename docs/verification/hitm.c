// Cross-core: producer core writes varlen records; consumer core walks them
// while lines are modified in producer's cache. Compare dep chase vs precomputed
// offsets (MLP ceiling) vs random chase over the same freshly-written data.
#define _GNU_SOURCE
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>
#include <pthread.h>
#include <sched.h>
#include <stdatomic.h>

static inline uint64_t now_ns(void) {
  struct timespec ts; clock_gettime(CLOCK_MONOTONIC, &ts);
  return (uint64_t)ts.tv_sec * 1000000000ull + ts.tv_nsec;
}
static uint64_t rng_s = 0x9e3779b97f4a7c15ull;
static inline uint64_t rng(void) { rng_s ^= rng_s << 13; rng_s ^= rng_s >> 7; rng_s ^= rng_s << 17; return rng_s; }

static uint8_t *buf; static size_t nbytes, nrec; static size_t *off; static size_t *randoff;
static atomic_int phase; // 0 idle, 1 producer writes, 2 consumer walks
static volatile uint64_t sink;

static void pin(int cpu) {
  cpu_set_t s; CPU_ZERO(&s); CPU_SET(cpu, &s);
  pthread_setaffinity_np(pthread_self(), sizeof(s), &s);
}

static void layout(size_t minlen, size_t maxlen) {
  size_t pos = 0; nrec = 0;
  while (pos + 8 + maxlen + 16 <= nbytes) {
    size_t len = minlen + rng() % (maxlen - minlen + 1);
    size_t step = (8 + len + 15) & ~(size_t)15;
    off[nrec++] = pos; pos += step;
  }
  nbytes = pos;
  // shuffled visit order for randchase
  for (size_t i = 0; i < nrec; i++) randoff[i] = off[i];
  for (size_t i = nrec - 1; i > 0; i--) { size_t j = rng() % (i + 1); size_t t = randoff[i]; randoff[i] = randoff[j]; randoff[j] = t; }
}

static void *producer(void *arg) {
  (void)arg; pin(2);
  for (;;) {
    while (atomic_load_explicit(&phase, memory_order_acquire) != 1) { if (atomic_load(&phase) == -1) return 0; }
    // write every record: header + payload (touch all lines, leave Modified)
    for (size_t i = 0; i < nrec; i++) {
      size_t pos = off[i];
      size_t step = (i + 1 < nrec ? off[i + 1] : nbytes) - pos;
      *(uint64_t *)(buf + pos) = step;
      for (size_t o = 8; o < step; o += 8) *(uint64_t *)(buf + pos + o) = i;
    }
    atomic_store_explicit(&phase, 2, memory_order_release);
  }
}

static uint64_t walk_dep(void) {
  uint64_t s = 0; size_t pos = 0;
  while (pos < nbytes) {
    uint64_t step = *(uint64_t *)(buf + pos);
    for (size_t o = 8; o < step; o += 64) s += buf[pos + o];
    pos += step;
  }
  return s;
}
static uint64_t walk_dep_pf(void) {
  uint64_t s = 0; size_t pos = 0;
  while (pos < nbytes) {
    uint64_t step = *(uint64_t *)(buf + pos);
    size_t nxt = pos + step;
    __builtin_prefetch(buf + nxt, 0, 3); __builtin_prefetch(buf + nxt + 64, 0, 3);
    for (size_t o = 8; o < step; o += 64) s += buf[pos + o];
    pos = nxt;
  }
  return s;
}
static uint64_t walk_pre(void) {
  uint64_t s = 0;
  for (size_t i = 0; i < nrec; i++) {
    size_t pos = off[i]; uint64_t step = *(uint64_t *)(buf + pos);
    for (size_t o = 8; o < step; o += 64) s += buf[pos + o];
  }
  return s;
}
static uint64_t walk_rand(void) {
  uint64_t s = 0;
  for (size_t i = 0; i < nrec; i++) {
    size_t pos = randoff[i]; uint64_t step = *(uint64_t *)(buf + pos);
    for (size_t o = 8; o < step; o += 64) s += buf[pos + o];
  }
  return s;
}

int main(int argc, char **argv) {
  nbytes = argc > 1 ? strtoull(argv[1], 0, 0) : (32u << 20);
  int trials = argc > 2 ? atoi(argv[2]) : 7;
  buf = aligned_alloc(4096, nbytes + 4096); memset(buf, 0, nbytes + 4096);
  off = malloc((nbytes / 16 + 2) * sizeof(size_t));
  randoff = malloc((nbytes / 16 + 2) * sizeof(size_t));
  layout(16, 104);
  printf("bytes=%zu recs=%zu\n", nbytes, nrec);
  pthread_t pt; atomic_store(&phase, 0); pthread_create(&pt, 0, producer, 0);
  pin(10);
  const char *names[] = {"dep", "dep_pf", "precomp", "randchase"};
  uint64_t (*fns[])(void) = {walk_dep, walk_dep_pf, walk_pre, walk_rand};
  for (int v = 0; v < 4; v++) {
    uint64_t best = ~0ull;
    for (int t = 0; t < trials; t++) {
      atomic_store_explicit(&phase, 1, memory_order_release);
      while (atomic_load_explicit(&phase, memory_order_acquire) != 2) {}
      uint64_t t0 = now_ns(); sink = fns[v](); uint64_t dt = now_ns() - t0;
      if (dt < best) best = dt;
    }
    printf("  %-9s %7.2f ns/rec  (%5.1f M rec/s)\n", names[v],
           (double)best / nrec, nrec * 1000.0 / best);
  }
  atomic_store(&phase, -1); pthread_join(pt, 0);
  return 0;
}
