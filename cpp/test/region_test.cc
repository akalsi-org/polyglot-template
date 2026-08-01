// Region and descriptor-encoding tests.
//
// These sit below every queue variant: the mirrored mapping, the control-area
// layout (including the shared writer registry), the backend behaviours, and
// the descriptor encoding. They are variant-agnostic and outlived the removal
// of the shared-ring MPSC variants.
//
// The registry-sizing cases matter more than they look. Page-align slack
// silently masks layout bugs, so the shard counts here are DERIVED from the
// arithmetic (the exact-fit count, and the count where a dropped term moves a
// page boundary) rather than being round numbers, and the layout asserts
// compare against the unpadded sum rather than the page-rounded reservation.

#include "pgt/core/platform.hh"
#include "pgt/mpsc/desc.hh"
#include "pgt/mpsc/spsc_ring.hh"
#include "pgt/mpsc/region.hh"

#include <doctest/doctest.h>

#include <cerrno>
#include <fcntl.h>
#include <sys/mman.h>
#include <unistd.h>

#include <cstring>
#include <string>
#include <vector>

namespace {

using namespace pgt;
using namespace pgt::mpsc;

TEST_CASE("Region: geometry, mirror aliasing, arena init") {
  Config cfg;
  cfg.capacity = 100000;  // rounds up to 131072
  cfg.shards = 3;
  Region r;
  REQUIRE(Region::create(cfg, r));
  CHECK(r.capacity() == 131072);
  CHECK(r.shardCount() == 3);
  CHECK(r.control()->shards == 3);
  CHECK(r.control()->capacity == 131072);
  CHECK(r.control()->reader_tid == 0);

  for (u32_t i = 0; i < 3; ++i) {
    // Position 0 stamped claimable.
    u64_t w;
    std::memcpy(&w, r.arena(i), sizeof(w));
    CHECK(w == freeWord(0));
    // Mirror: a store through the primary alias is visible at +capacity. The
    // barrier matters -- the two aliases are distinct objects to the compiler.
    r.arena(i)[100] = std::byte{0x5a};
    asm volatile("" ::: "memory");
    CHECK(r.arena(i)[100 + r.capacity()] == std::byte{0x5a});
    r.arena(i)[100] = std::byte{0};
  }

  // Writer registry: after the ShardControls, zeroed, inside the control area.
  CHECK(r.bitmapWords() == 1);
  CHECK(r.writerBitmap()[0] == 0);
  for (u32_t i = 0; i < 3; ++i) {
    CHECK(r.writerSlots()[i].owner_tid == 0);
    CHECK(r.writerSlots()[i].generation == 0);
  }
  CHECK(reinterpret_cast<std::byte*>(r.writerSlots() + 3) <= r.arena(0));
}

TEST_CASE("Region: rejects an unencodable capacity before rounding") {
  Config cfg;
  cfg.capacity = kMaxExtent + 1;
  Region r;
  errno = 0;
  CHECK(!Region::create(cfg, r));
  CHECK(errno == EINVAL);
}

TEST_CASE("Region: control area sizes the writer registry past one page") {
  // 127 shards, DELIBERATELY not a rounder number: the registry must be
  // genuinely RESERVED in the control area, not fitting in page-align slack by
  // luck (SpscRing does its claim fetch_or on these words). At 100 shards,
  // deleting the WriterSlot term from controlBytes() is INVISIBLE -- 25744 and
  // 26544 bytes both round to 7 pages (impl-spsc's finding). At 127, the slot
  // table is exactly what pushes the control area across its final page
  // boundary (32656 -> 8 pages without it, 33672 -> 9 with), so that break
  // fails the layout REQUIRE below deterministically. The constant is
  // 4 KiB-page-specific.
  constexpr u32_t kShards = 127;
  Config cfg;
  cfg.capacity = 4096;
  cfg.shards = kShards;
  Region r;
  REQUIRE(Region::create(cfg, r));
  CHECK(r.bitmapWords() == 2);
  REQUIRE(reinterpret_cast<std::byte*>(r.writerSlots() + kShards) <= r.arena(0));
  CHECK((reinterpret_cast<uintptr_t>(r.arena(0)) & (pageSize() - 1)) == 0);
  CHECK(r.writerBitmap()[1] == 0);
  for (u32_t i = 0; i < kShards; ++i) {
    u64_t w;
    std::memcpy(&w, r.arena(i), sizeof(w));
    REQUIRE(w == freeWord(0));
  }
  // Attach re-derives the multi-page geometry.
  int const fd = dup(r.fd());
  Region s;
  REQUIRE(Region::attach(fd, s));
  CHECK(s.shardCount() == kShards);
  CHECK(s.bitmapWords() == 2);
  // Registry writes made through one mapping are visible through the other.
  s.writerSlots()[kShards - 1].owner_tid = 4242;
  asm volatile("" ::: "memory");
  CHECK(r.writerSlots()[kShards - 1].owner_tid == 4242);
  s.writerSlots()[kShards - 1].owner_tid = 0;
}

TEST_CASE("Region: attach verifies magic, version, and file size") {
  Config cfg;
  cfg.capacity = 4096;
  Region r;
  REQUIRE(Region::create(cfg, r));

  Region ok;
  int fd = dup(r.fd());
  REQUIRE(Region::attach(fd, ok));
  CHECK(ok.capacity() == r.capacity());

  // Corrupt the magic: attach must refuse (the mapping is shared, so restore).
  u64_t const saved = r.control()->magic;
  r.control()->magic = saved ^ 1;
  Region bad;
  fd = dup(r.fd());
  CHECK(!Region::attach(fd, bad));
  close(fd);  // attach failed, so the fd is still ours
  r.control()->magic = saved;

  // Wrong version: refuse.
  r.control()->version += 1;
  fd = dup(r.fd());
  CHECK(!Region::attach(fd, bad));
  close(fd);
  r.control()->version -= 1;

  // A random memfd with no header at all: refuse.
  int const junk = static_cast<int>(memfd_create("junk", 0));
  REQUIRE(junk >= 0);
  REQUIRE(ftruncate(junk, 65536) == 0);
  CHECK(!Region::attach(junk, bad));
  close(junk);
}

TEST_CASE("Region: kFile create over a dirty reused file still initialises") {
  // The claimability invariant is "desc[0] reads kFree(0)", NOT "the backend
  // zeroes memory". A fresh memfd hides a missing stamp because freeWord(0)
  // happens to be 0; a reused file full of garbage does not.
  char path[] = "/tmp/pgt-mpsc-dirty-XXXXXX";
  int const tmp = mkstemp(path);
  REQUIRE(tmp >= 0);
  std::vector<std::byte> junk(64 * 1024);
  for (sz_t i = 0; i < junk.size(); ++i) junk[i] = static_cast<std::byte>(i * 131 + 7);
  REQUIRE(write(tmp, junk.data(), junk.size()) == static_cast<ssz_t>(junk.size()));
  close(tmp);

  Config cfg;
  cfg.capacity = 4096;
  cfg.backend = Backend::kFile;
  cfg.name = path;
  Region r;
  REQUIRE(Region::create(cfg, r));
  u64_t w;
  std::memcpy(&w, r.arena(0), sizeof(w));
  // REQUIRE, not CHECK: with a garbage descriptor at position 0 the ring
  // exercise below spins forever, so fail here instead of hanging.
  REQUIRE(w == freeWord(0));
  CHECK(r.control()->reader_tid == 0);
  CHECK(r.writerBitmap()[0] == 0);

  // And the ring built over it must actually work end to end. SpscRing<> is the
  // surviving in-band variant; what is under test is the region, not the queue.
  SpscRing<> q;
  {
    Config qc = cfg;
    REQUIRE(SpscRing<>::create(qc, q));
    REQUIRE(q.attachWriter());
    REQUIRE(q.attachReader());
    std::byte buf[24];
    for (sz_t i = 0; i < sizeof(buf); ++i) buf[i] = static_cast<std::byte>(i);
    REQUIRE(q.write(buf, sizeof(buf)));
    ReadSpan const got = q.peek();
    REQUIRE(got.size() == sizeof(buf));
    CHECK(std::memcmp(got.data(), buf, sizeof(buf)) == 0);
    q.pop();
  }
  unlink(path);
}

TEST_CASE("Region: moved-from and default objects are inert") {
  Config cfg;
  cfg.capacity = 4096;
  Region a;
  REQUIRE(Region::create(cfg, a));
  int const fd = a.fd();
  Region b = std::move(a);
  CHECK(!a.valid());  // NOLINT: moved-from state is defined here
  CHECK(b.valid());
  CHECK(b.fd() == fd);
  a = std::move(b);  // move back over the moved-from object
  CHECK(a.valid());
  Region c;
  c = std::move(a);
  CHECK(c.valid());
  // a, b, and a default-constructed Region all destruct here: must not crash
  // or double-close.
  Region d;
  static_cast<void>(d);
}

TEST_CASE("desc encoding round-trips the boundary payload sizes") {
  for (sz_t n : {sz_t{0}, sz_t{1}, kGrain - 8, kGrain - 7, kGrain, 2 * kGrain - 8, sz_t{300}}) {
    sz_t const extent = extentFor(n);
    u64_t const w =
      packRecord(extent, static_cast<u32_t>(n & (kGrain - 1)), State::kCommitted, 12345);
    CHECK(extentOf(w) == extent);
    CHECK(committedLen(w) == n);
    CHECK(tidOf(w) == 12345);
    CHECK(stateOf(w) == State::kCommitted);
  }
  CHECK(freeWord(0) == 0);
  CHECK(freePos(freeWord(4096)) == 4096);
  // The lap-ABA guard: FREE(p) never equals FREE(p + k*N).
  CHECK(freeWord(4096) != freeWord(4096 + 131072));
}

// REGRESSION (Gemini review, 2026-07-26): attach() derived the file size from
// shards * capacity with no overflow guard, while create() had one. With shards
// unbounded across a u32 and capacity up to kMaxExtent, that product wraps, and
// a header that wraps it to a small value passes the size check by coincidence
// -- yielding a mapping far smaller than the arena accessors assume.
TEST_CASE("Region: attach refuses a header whose geometry overflows") {
  struct RawHeader {
    u64_t magic;
    u32_t version;
    u32_t shards;
    u64_t capacity;
    u32_t tgid;
    u32_t reader_tid;
    u32_t claim_stride;
    u32_t result_stride;
  };
  auto forge = [](u32_t shards, u64_t capacity) {
    int const fd = ::memfd_create("pgt-forged", 0);
    REQUIRE(fd >= 0);
    REQUIRE(::ftruncate(fd, 1 << 16) == 0);
    RawHeader h{};
    h.magic = 0x7067'745f'6d70'7363ull;  // "pgt_mpsc"
    h.version = 2;
    h.shards = shards;
    h.capacity = capacity;
    REQUIRE(::pwrite(fd, &h, sizeof h, 0) == static_cast<ssz_t>(sizeof h));
    return fd;
  };

  // shards * capacity wraps u64.
  {
    int const fd = forge(0xFFFFFFFFu, 1ull << 32);
    Region r;
    errno = 0;
    CHECK_FALSE(Region::attach(fd, r));
    CHECK(errno == EPROTO);
    ::close(fd);
  }
  // A shard count past the documented bound, without wrapping.
  {
    int const fd = forge((1u << 16) + 1, 4096);
    Region r;
    CHECK_FALSE(Region::attach(fd, r));
    ::close(fd);
  }
  // Zero shards.
  {
    int const fd = forge(0, 4096);
    Region r;
    CHECK_FALSE(Region::attach(fd, r));
    ::close(fd);
  }
}
}  // namespace
