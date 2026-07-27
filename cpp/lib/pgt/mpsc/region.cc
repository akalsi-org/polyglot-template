#include "../mpsc/region.hh"

#include "../core/platform.hh"
#include "../mpsc/desc.hh"

#include <fcntl.h>
#include <sys/mman.h>
#include <sys/stat.h>
#include <unistd.h>

#include <bit>
#include <cassert>
#include <cerrno>
#include <cstdint>
#include <cstring>

namespace pgt::mpsc {
namespace {

inline constexpr u64_t kMagic = 0x7067'745f'6d70'7363ull;  // "pgt_mpsc"
// Bumped to 2 when the claim word gained a lap-parity bit (desc.hh kLapShift).
// A v1 region's claim words have that bit clear regardless of lap, so a v2
// reader would mistake stale previous-lap words for current-lap claims and
// decline to promote a successor -- wedging the ring rather than corrupting it,
// but wedging it silently. Refusing the attach is the honest outcome.
inline constexpr u32_t kVersion = 2;

// The control area occupies file offsets [0, ctrl): one Control block, one
// ShardControl per shard, then the shared writer-slot registry (occupancy
// bitmap + WriterSlot table -- shared because slot arbitration is
// cross-process; see region.hh), padded to a whole number of pages so the
// arenas that follow are page-aligned (a requirement for their mmap offsets).
// Unpadded sum, split out so the layout-agreement asserts below can compare
// the accessor walk against it EXACTLY: comparing against the page-rounded
// value would let a divergence smaller than the alignment slack pass -- the
// same masking that makes such a bug unobservable at most shard counts.
[[nodiscard]] constexpr sz_t align64(sz_t n) noexcept { return (n + 63) & ~sz_t{63}; }

// Offset of the metadata planes within the control area, or the whole unpadded
// control size when there are none. Split out so both the sizing path and the
// accessor-agreement assert derive the plane base from ONE expression.
[[nodiscard]] sz_t planeBase(u32_t shards) noexcept {
  sz_t const bitmap_words = (static_cast<sz_t>(shards) + 63) / 64;
  sz_t const raw = sizeof(Control) + static_cast<sz_t>(shards) * sizeof(ShardControl) +
                   bitmap_words * sizeof(u64_t) + static_cast<sz_t>(shards) * sizeof(WriterSlot);
  return raw;
}

// Bytes of Claim + Result plane for ONE shard. Each block is rounded to the
// 64-byte line: the padded stride exists to put one cell per line, and a block
// that merely starts 8-aligned pads without separating.
[[nodiscard]] sz_t shardPlaneBytes(u64_t capacity, u64_t claim_stride,
                                   u64_t result_stride) noexcept {
  if (claim_stride == 0) return 0;
  u64_t const cells = capacity / kGrain;
  return align64(static_cast<sz_t>(cells * claim_stride)) +
         align64(static_cast<sz_t>(cells * result_stride));
}

[[nodiscard]] sz_t controlBytesRaw(u32_t shards, u64_t capacity, u64_t claim_stride,
                                   u64_t result_stride) noexcept {
  sz_t const base = planeBase(shards);
  if (claim_stride == 0) return base;
  return align64(base) +
         static_cast<sz_t>(shards) * shardPlaneBytes(capacity, claim_stride, result_stride);
}

[[nodiscard]] sz_t controlBytes(u32_t shards, u64_t capacity, u64_t claim_stride,
                                u64_t result_stride, sz_t page) noexcept {
  return (controlBytesRaw(shards, capacity, claim_stride, result_stride) + page - 1) & ~(page - 1);
}

// One mapping routine for every backend: reserve the whole span as PROT_NONE,
// then MAP_FIXED each piece into it. The reservation is what makes the double
// mapping race-free -- without it another thread's unrelated mmap can land
// between the two MAP_FIXED calls and the mirror invariant silently breaks.
// On failure nothing stays mapped and errno reports the cause.
[[nodiscard]] bool mapMirrored(int fd, sz_t ctrl, u64_t capacity, u32_t shards, std::byte*& base,
                               sz_t& reservation) noexcept {
  sz_t const span = ctrl + static_cast<sz_t>(shards) * 2 * capacity;
  void* const hole =
    mmap(nullptr, span, PROT_NONE, MAP_PRIVATE | MAP_ANONYMOUS | MAP_NORESERVE, -1, 0);
  if (hole == MAP_FAILED) return false;
  auto* const b = static_cast<std::byte*>(hole);

  auto fix = [&](sz_t at, sz_t len, off_t off) noexcept {
    return mmap(b + at, len, PROT_READ | PROT_WRITE, MAP_SHARED | MAP_FIXED, fd, off) != MAP_FAILED;
  };

  // Control page once, then each arena TWICE back to back from the same file
  // offset: that is the whole mirror.
  bool ok = fix(0, ctrl, 0);
  for (u32_t i = 0; ok && i < shards; ++i) {
    off_t const off = static_cast<off_t>(ctrl + static_cast<sz_t>(i) * capacity);
    sz_t const at = ctrl + static_cast<sz_t>(i) * 2 * capacity;
    ok = fix(at, capacity, off) && fix(at + capacity, capacity, off);
  }
  if (!ok) {
    int const saved = errno;
    munmap(b, span);  // takes the MAP_FIXED pieces down with the reservation
    errno = saved;
    return false;
  }
  base = b;
  reservation = span;
  return true;
}

// Creates the backing object for cfg's backend, sized and (where the backend
// allows) sealed. On failure the fd is closed and errno reports the cause.
//
// Seals are memfd-only: F_ADD_SEALS requires memfd_create with
// MFD_ALLOW_SEALING, so no attempt is made on the named backends and their
// absence there is not an error.
[[nodiscard]] bool openBacking(Config const& cfg, sz_t file_size, int& out_fd) noexcept {
  int fd = -1;
  switch (cfg.backend) {
    case Backend::kMemfd:
      fd = static_cast<int>(memfd_create("pgt-mpsc", MFD_CLOEXEC | MFD_ALLOW_SEALING));
      break;
    case Backend::kShm:
      // The name is deliberately left in place: a successor reader must be able
      // to shm_open it unaided after this process dies, and that is the one
      // thing kShm buys over kMemfd. The caller owns the name's lifetime and is
      // responsible for the eventual shm_unlink; the Region owns only the fd.
      if (cfg.name == nullptr) {
        errno = EINVAL;
        return false;
      }
      fd = shm_open(cfg.name, O_RDWR | O_CREAT | O_EXCL, 0600);
      break;
    case Backend::kFile:
      // MAP_SHARED on a real filesystem means continuous writeback of every
      // dirty page for data nobody intends to persist. Forensics only.
      if (cfg.name == nullptr) {
        errno = EINVAL;
        return false;
      }
      fd = open(cfg.name, O_RDWR | O_CREAT | O_CLOEXEC, 0600);
      break;
  }
  if (fd < 0) return false;

  if (ftruncate(fd, static_cast<off_t>(file_size)) != 0) goto fail;

  // Load-bearing twice: a first-touch fault cannot SIGBUS under memory
  // pressure, and no lazy fault means no kernel-side allocation on the hot
  // path -- this is what makes the queue allocation-free after construction.
  if (cfg.preallocate && fallocate(fd, 0, 0, static_cast<off_t>(file_size)) != 0) goto fail;

  // Applied before the descriptor could ever be shared. F_SEAL_SHRINK is the
  // one that matters even inside a cooperative trust boundary: a crashing
  // peer's ftruncate is the most common way a shared-memory queue dies.
  if (cfg.backend == Backend::kMemfd &&
      fcntl(fd, F_ADD_SEALS, F_SEAL_SHRINK | F_SEAL_GROW | F_SEAL_SEAL) != 0) {
    goto fail;
  }

  out_fd = fd;
  return true;

fail:
  int const saved = errno;
  close(fd);
  errno = saved;
  return false;
}

// An upper bound on the shard count is a CORRECTNESS requirement, not tidiness.
// attach() derives the file size from shards * capacity and mapMirrored() spans
// shards * 2 * capacity; with shards unbounded across a u32 and capacity up to
// kMaxExtent (~4 GiB), both products can wrap. A header that wraps them to a
// small value passes the file-size check by coincidence and yields a mapping far
// smaller than the arena accessors assume.
//
// 65536 shards is already absurd -- 256 B of ShardControl plus a WriterSlot each
// is ~17 MiB of control area before a single byte of ring -- and it makes every
// product here provably non-wrapping: 2^16 shards * 2^32 capacity * 2 = 2^49.
inline constexpr u32_t kMaxShards = 1u << 16;

[[nodiscard]] bool geometryOk(u64_t capacity, u32_t shards, sz_t page) noexcept {
  return shards != 0 && shards <= kMaxShards && capacity != 0 && std::has_single_bit(capacity) &&
         (capacity & (page - 1)) == 0 && capacity <= kMaxExtent;
}

}  // namespace

void Region::reset() noexcept {
  int const saved = errno;  // close/munmap must not clobber a caller's errno
  if (base_ != nullptr) munmap(base_, reservation_);
  if (fd_ >= 0) close(fd_);
  errno = saved;
  fd_ = -1;
  base_ = nullptr;
  reservation_ = 0;
  control_ = nullptr;
  shards_ = nullptr;
  arena_ = nullptr;
  planes_ = nullptr;
  capacity_ = 0;
  shard_count_ = 0;
  claim_stride_ = 0;
  result_stride_ = 0;
}

Region::~Region() { reset(); }

Region::Region(Region&& other) noexcept
    : fd_(other.fd_),
      base_(other.base_),
      reservation_(other.reservation_),
      control_(other.control_),
      shards_(other.shards_),
      arena_(other.arena_),
      planes_(other.planes_),
      capacity_(other.capacity_),
      shard_count_(other.shard_count_),
      claim_stride_(other.claim_stride_),
      result_stride_(other.result_stride_) {
  other.fd_ = -1;
  other.base_ = nullptr;
  other.reservation_ = 0;
  other.control_ = nullptr;
  other.shards_ = nullptr;
  other.arena_ = nullptr;
  other.planes_ = nullptr;
  other.capacity_ = 0;
  other.shard_count_ = 0;
  other.claim_stride_ = 0;
  other.result_stride_ = 0;
}

Region& Region::operator=(Region&& other) noexcept {
  if (this != &other) {
    reset();
    fd_ = other.fd_;
    base_ = other.base_;
    reservation_ = other.reservation_;
    control_ = other.control_;
    shards_ = other.shards_;
    arena_ = other.arena_;
    planes_ = other.planes_;
    capacity_ = other.capacity_;
    shard_count_ = other.shard_count_;
    claim_stride_ = other.claim_stride_;
    result_stride_ = other.result_stride_;
    other.fd_ = -1;
    other.base_ = nullptr;
    other.reservation_ = 0;
    other.control_ = nullptr;
    other.shards_ = nullptr;
    other.arena_ = nullptr;
    other.planes_ = nullptr;
    other.capacity_ = 0;
    other.shard_count_ = 0;
    other.claim_stride_ = 0;
    other.result_stride_ = 0;
  }
  return *this;
}

bool Region::create(Config const& cfg, Region& out) noexcept {
  sz_t const page = pageSize();
  if (page == 0 || cfg.shards == 0) {
    errno = EINVAL;
    return false;
  }

  // Reject before bit_ceil: rounding a value above the largest encodable
  // power of two is undefined/zero rather than a recoverable invalid geometry.
  u64_t const requested = cfg.capacity < page ? static_cast<u64_t>(page) : cfg.capacity;
  if (requested > kMaxExtent) {
    errno = EINVAL;
    return false;
  }
  // Round capacity up to a power of two that is at least a page; page sizes are
  // powers of two, so bit_ceil covers the multiple-of-page requirement too.
  u64_t const cap = std::bit_ceil(requested);
  // A record extent may be as large as the whole arena, so the descriptor's
  // size field must be able to encode `cap` itself.
  if (cap > kMaxExtent) {
    errno = EINVAL;
    return false;
  }

  // Plane strides are a geometry input, validated like any other. A result
  // stride without a claim stride (or vice versa) is a caller bug, not a
  // degenerate-but-usable region.
  if ((cfg.plane_claim_stride == 0) != (cfg.plane_result_stride == 0) ||
      cfg.plane_claim_stride > 64 || cfg.plane_result_stride > 64) {
    errno = EINVAL;
    return false;
  }
  sz_t const ctrl =
    controlBytes(cfg.shards, cap, cfg.plane_claim_stride, cfg.plane_result_stride, page);
  if (cap > (SIZE_MAX - ctrl) / (2 * static_cast<sz_t>(cfg.shards))) {
    errno = EINVAL;  // reservation would overflow the address arithmetic
    return false;
  }
  sz_t const file_size = ctrl + static_cast<sz_t>(cfg.shards) * cap;

  int fd = -1;
  if (!openBacking(cfg, file_size, fd)) return false;

  std::byte* base = nullptr;
  sz_t reservation = 0;
  if (!mapMirrored(fd, ctrl, cap, cfg.shards, base, reservation)) {
    int const saved = errno;
    close(fd);
    errno = saved;
    return false;
  }

  // Initialise the control area from a clean slate: kFile may hand back a
  // reused file, so none of this may rely on fresh-mapping zero fill.
  std::memset(base, 0, ctrl);
  auto* const control = reinterpret_cast<Control*>(base);
  control->magic = kMagic;
  control->version = kVersion;
  control->shards = cfg.shards;
  control->capacity = cap;
  control->tgid = static_cast<u32_t>(getpid());
  control->reader_tid = 0;
  control->claim_stride = static_cast<u32_t>(cfg.plane_claim_stride);
  control->result_stride = static_cast<u32_t>(cfg.plane_result_stride);
  // The memset above already zeroed the rest of the control area: every
  // ShardControl cursor (write_hint = read_pos = 0), the writer occupancy
  // bitmap, and every WriterSlot {owner_tid = 0, generation = 0}.

  // Position 0 of every shard must be claimable: stamp freeWord(0) explicitly.
  // A fresh memfd is zero-filled and freeWord(0) happens to BE 0, but the
  // invariant is "desc[0] reads kFree(0)", not "the backend zeroes memory" --
  // a reused kFile backing breaks the coincidence.
  u64_t const fw = freeWord(0);
  for (u32_t i = 0; i < cfg.shards; ++i) {
    std::memcpy(base + ctrl + static_cast<sz_t>(i) * 2 * cap, &fw, sizeof(fw));
  }

  Region r;
  r.fd_ = fd;
  r.base_ = base;
  r.reservation_ = reservation;
  r.control_ = control;
  r.shards_ = reinterpret_cast<ShardControl*>(base + sizeof(Control));
  r.arena_ = base + ctrl;
  r.capacity_ = cap;
  r.shard_count_ = cfg.shards;
  r.claim_stride_ = static_cast<u32_t>(cfg.plane_claim_stride);
  r.result_stride_ = static_cast<u32_t>(cfg.plane_result_stride);
  r.planes_ = cfg.plane_claim_stride == 0 ? nullptr : base + align64(planeBase(cfg.shards));
  // A zero-filled Claim cell decodes as freeWord(0), which matches ONLY grain
  // 0, so the frontier starts at position 0 and every other cell already reads
  // "not free for me". The memset above guarantees that regardless of backend,
  // which a reused kFile backing would otherwise break.

  // Plane accessors are a THIRD independent walk of the same layout, so pin
  // them to the sizing expression too -- same reasoning as the control walk
  // below, and the same failure mode: planes overlapping arena 0 would corrupt
  // the first records written rather than fault.
  assert(
    (cfg.plane_claim_stride == 0 ||
     r.resultPlane(cfg.shards - 1) +
         align64(static_cast<sz_t>((cap / kGrain) * cfg.plane_result_stride)) ==
       base + controlBytesRaw(cfg.shards, cap, cfg.plane_claim_stride, cfg.plane_result_stride)) &&
    "plane accessor walk disagrees with controlBytes()");
  // The control layout is computed TWICE, independently: controlBytes() sums
  // it, the accessors walk it pointer by pointer. They agree today, but
  // nothing structural keeps them agreeing -- a field added to one and not the
  // other diverges silently, and at an exact-fit shard count that puts the
  // WriterSlot table inside arena 0, where registration corrupts the first
  // records written. Demand EXACT agreement with the unpadded sum: comparing
  // against arena(0) would let page-align slack hide any divergence smaller
  // than the slack, at every count where slack exists -- which is most.
  assert(reinterpret_cast<std::byte const*>(r.writerSlots() + r.shardCount()) ==
           base + planeBase(r.shardCount()) &&
         "control-area accessor walk disagrees with controlBytes()");
  out = std::move(r);
  // Re-check AFTER the move. Region has hand-written move operations because it
  // owns an fd and a mapping, so a member added to the class and not to those
  // operations is silently dropped -- which is exactly how the plane strides
  // were lost the first time, arriving as a zeroed geometry that mapped the
  // planes onto a null base. The asserts above run on the local `r` and cannot
  // see it; this one can.
  assert(out.capacity() == cap && out.shardCount() == cfg.shards &&
         out.claimStride() == cfg.plane_claim_stride &&
         out.resultStride() == cfg.plane_result_stride &&
         (cfg.plane_claim_stride == 0) == (out.claimPlane(0) == nullptr) &&
         "Region move operations dropped a member");
  return true;
}

// On success the Region takes ownership of `fd` and closes it on destruction;
// on failure the fd is left untouched and still belongs to the caller.
bool Region::attach(int fd, Region& out) noexcept {
  sz_t const page = pageSize();
  if (page == 0 || fd < 0) {
    errno = EINVAL;
    return false;
  }

  struct stat st{};
  if (fstat(fd, &st) != 0) return false;
  if (static_cast<u64_t>(st.st_size) < page) {
    errno = EPROTO;
    return false;
  }

  // Probe the control header before committing to a full layout: geometry
  // comes from the file itself, and everything is re-derived from it.
  void* const probe = mmap(nullptr, page, PROT_READ, MAP_SHARED, fd, 0);
  if (probe == MAP_FAILED) return false;
  Control hdr;
  std::memcpy(&hdr, probe, sizeof(hdr));
  munmap(probe, page);

  if (hdr.magic != kMagic || hdr.version != kVersion) {
    errno = EPROTO;
    return false;
  }
  if (!geometryOk(hdr.capacity, hdr.shards, page)) {
    errno = EPROTO;
    return false;
  }
  if ((hdr.claim_stride == 0) != (hdr.result_stride == 0) || hdr.claim_stride > 64 ||
      hdr.result_stride > 64) {
    errno = EPROTO;  // header claims a plane geometry that cannot be laid out
    return false;
  }
  sz_t const ctrl =
    controlBytes(hdr.shards, hdr.capacity, hdr.claim_stride, hdr.result_stride, page);
  // The same overflow guard create() applies. Without it the size comparison
  // below is made against a WRAPPED product, so a header can satisfy it by
  // coincidence. geometryOk's shard bound already makes this unreachable; it is
  // kept because the two paths must not disagree about what is representable,
  // and because a future change to either bound should fail here rather than
  // silently produce a short mapping.
  if (hdr.capacity > (SIZE_MAX - ctrl) / (2 * static_cast<sz_t>(hdr.shards))) {
    errno = EPROTO;
    return false;
  }
  if (static_cast<u64_t>(st.st_size) != ctrl + static_cast<u64_t>(hdr.shards) * hdr.capacity) {
    errno = EPROTO;  // header claims a geometry the file does not have
    return false;
  }

  std::byte* base = nullptr;
  sz_t reservation = 0;
  if (!mapMirrored(fd, ctrl, hdr.capacity, hdr.shards, base, reservation)) return false;

  Region r;
  r.fd_ = fd;
  r.base_ = base;
  r.reservation_ = reservation;
  r.control_ = reinterpret_cast<Control*>(base);
  r.shards_ = reinterpret_cast<ShardControl*>(base + sizeof(Control));
  r.arena_ = base + ctrl;
  r.capacity_ = hdr.capacity;
  r.shard_count_ = hdr.shards;
  r.claim_stride_ = hdr.claim_stride;
  r.result_stride_ = hdr.result_stride;
  r.planes_ = hdr.claim_stride == 0 ? nullptr : base + align64(planeBase(hdr.shards));
  // Same exact-agreement check as create(); see the comment there. attach()
  // needs it independently -- a version skew between the creating and
  // attaching binaries is exactly a divergence of the two computations.
  assert(reinterpret_cast<std::byte const*>(r.writerSlots() + r.shardCount()) ==
           base + planeBase(r.shardCount()) &&
         "control-area accessor walk disagrees with controlBytes()");
  out = std::move(r);
  return true;
}

}  // namespace pgt::mpsc
