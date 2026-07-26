// Compare traversal strategies over a varlen record buffer.
// Walks:
//   dep        : read header -> advance by len (serial addr computation), touch 1 byte payload
//   dep_pay    : same but touch payload every 64B (realistic consume)
//   dep_pf     : length-first pipelining: after header, prefetch next record, then touch payload
//   fixed      : fixed stride = avg record size, touch same bytes (addresses computable)
//   precomp    : offsets precomputed in array (full MLP ceiling), touch payload
//   twopass    : pass1 headers->offset array (serial), pass2 payload via offsets (MLP)
//   randchase  : true pointer chase, shuffled order (contrast baseline)
#define _GNU_SOURCE
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>

static inline uint64_t now_ns(void) {
  struct timespec ts; clock_gettime(CLOCK_MONOTONIC, &ts);
  return (uint64_t)ts.tv_sec * 1000000000ull + ts.tv_nsec;
}

static uint64_t rng_s = 0x9e3779b97f4a7c15ull;
static inline uint64_t rng(void) {
  rng_s ^= rng_s << 13; rng_s ^= rng_s >> 7; rng_s ^= rng_s << 17; return rng_s;
}

typedef struct { uint8_t *buf; size_t nbytes; size_t nrec; size_t *off; size_t avg; } Layout;

// header u64 at off: total record step (16-aligned), >=16 (8 hdr + >=8 payload)
static Layout make(size_t nbytes, size_t minlen, size_t maxlen) {
  Layout L; L.buf = aligned_alloc(4096, nbytes + 4096); memset(L.buf, 1, nbytes + 4096);
  L.off = malloc((nbytes / 16 + 2) * sizeof(size_t));
  size_t pos = 0, n = 0, tot = 0;
  while (pos + 8 + maxlen + 16 <= nbytes) {
    size_t len = minlen + rng() % (maxlen - minlen + 1);
    size_t step = (8 + len + 15) & ~(size_t)15;
    *(uint64_t *)(L.buf + pos) = step;
    L.off[n++] = pos; tot += step; pos += step;
  }
  L.nbytes = pos; L.nrec = n; L.avg = tot / n;
  return L;
}

static volatile uint64_t sink;

#define REP_BEST(expr, iters, out_ns_per) do {                          \
    uint64_t best = ~0ull;                                              \
    for (int r = 0; r < (iters); r++) {                                 \
      uint64_t t0 = now_ns(); expr; uint64_t dt = now_ns() - t0;        \
      if (dt < best) best = dt;                                         \
    }                                                                   \
    out_ns_per = (double)best / (double)L.nrec;                         \
  } while (0)

static uint64_t w_dep(Layout *L) {
  uint64_t s = 0; size_t pos = 0, end = L->nbytes; uint8_t *b = L->buf;
  while (pos < end) { uint64_t step = *(uint64_t *)(b + pos); s += b[pos + 8]; pos += step; }
  return s;
}
static uint64_t w_dep_pay(Layout *L) {
  uint64_t s = 0; size_t pos = 0, end = L->nbytes; uint8_t *b = L->buf;
  while (pos < end) {
    uint64_t step = *(uint64_t *)(b + pos);
    for (size_t o = 8; o < step; o += 64) s += b[pos + o];
    pos += step;
  }
  return s;
}
static uint64_t w_dep_pf(Layout *L) {
  uint64_t s = 0; size_t pos = 0, end = L->nbytes; uint8_t *b = L->buf;
  while (pos < end) {
    uint64_t step = *(uint64_t *)(b + pos);
    size_t nxt = pos + step;
    __builtin_prefetch(b + nxt, 0, 3);
    __builtin_prefetch(b + nxt + 64, 0, 3);
    for (size_t o = 8; o < step; o += 64) s += b[pos + o];
    pos = nxt;
  }
  return s;
}
static uint64_t w_fixed(Layout *L) {
  uint64_t s = 0; size_t end = L->nbytes, step = L->avg & ~(size_t)15; uint8_t *b = L->buf;
  for (size_t pos = 0; pos + step <= end; pos += step)
    for (size_t o = 8; o < step; o += 64) s += b[pos + o];
  return s;
}
static uint64_t w_precomp(Layout *L) {
  uint64_t s = 0; uint8_t *b = L->buf;
  for (size_t i = 0; i < L->nrec; i++) {
    size_t pos = L->off[i]; uint64_t step = *(uint64_t *)(b + pos);
    for (size_t o = 8; o < step; o += 64) s += b[pos + o];
  }
  return s;
}
static uint64_t w_twopass(Layout *L, size_t *tmp) {
  uint64_t s = 0; size_t pos = 0, end = L->nbytes, n = 0; uint8_t *b = L->buf;
  while (pos < end) { tmp[n++] = pos; pos += *(uint64_t *)(b + pos); }
  for (size_t i = 0; i < n; i++) {
    size_t p = tmp[i]; uint64_t step = *(uint64_t *)(b + p);
    for (size_t o = 8; o < step; o += 64) s += b[p + o];
  }
  return s;
}
// random chase: build a shuffled ring of record indices; store next-offset in bytes 8..16
static void link_random(Layout *L) {
  size_t n = L->nrec; size_t *perm = malloc(n * sizeof(size_t));
  for (size_t i = 0; i < n; i++) perm[i] = i;
  for (size_t i = n - 1; i > 0; i--) { size_t j = rng() % (i + 1); size_t t = perm[i]; perm[i] = perm[j]; perm[j] = t; }
  for (size_t i = 0; i < n; i++)
    *(uint64_t *)(L->buf + L->off[perm[i]] + 8) = L->off[perm[(i + 1) % n]];
}
static uint64_t w_rand(Layout *L) {
  uint64_t s = 0; size_t pos = L->off[0]; uint8_t *b = L->buf;
  for (size_t i = 0; i < L->nrec; i++) { size_t nxt = *(uint64_t *)(b + pos + 8); s += b[pos]; pos = nxt; }
  return s;
}

int main(int argc, char **argv) {
  size_t nbytes = argc > 1 ? strtoull(argv[1], 0, 0) : (1u << 20);
  size_t minlen = argc > 2 ? strtoull(argv[2], 0, 0) : 16;
  size_t maxlen = argc > 3 ? strtoull(argv[3], 0, 0) : 104;
  int iters = argc > 4 ? atoi(argv[4]) : 20;
  Layout L = make(nbytes, minlen, maxlen);
  size_t *tmp = malloc(L.nrec * sizeof(size_t));
  double ns;
  printf("bytes=%zu recs=%zu avg=%zu\n", L.nbytes, L.nrec, L.avg);
  sink = w_dep(&L); // warm
  REP_BEST(sink = w_dep(&L), iters, ns);      printf("  dep       %7.2f ns/rec\n", ns);
  REP_BEST(sink = w_dep_pay(&L), iters, ns);  printf("  dep_pay   %7.2f ns/rec\n", ns);
  REP_BEST(sink = w_dep_pf(&L), iters, ns);   printf("  dep_pf    %7.2f ns/rec\n", ns);
  REP_BEST(sink = w_fixed(&L), iters, ns);    printf("  fixed     %7.2f ns/rec\n", ns);
  REP_BEST(sink = w_precomp(&L), iters, ns);  printf("  precomp   %7.2f ns/rec\n", ns);
  REP_BEST(sink = w_twopass(&L, tmp), iters, ns); printf("  twopass   %7.2f ns/rec\n", ns);
  link_random(&L);
  REP_BEST(sink = w_rand(&L), iters, ns);     printf("  randchase %7.2f ns/rec\n", ns);
  return 0;
}
