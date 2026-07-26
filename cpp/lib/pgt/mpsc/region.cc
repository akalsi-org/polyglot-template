#include "../mpsc/region.hh"

#include "../core/platform.hh"
#include "../mpsc/desc.hh"

#include <fcntl.h>
#include <sys/mman.h>
#include <sys/stat.h>
#include <unistd.h>

#include <bit>
#include <cerrno>
#include <cstdint>
#include <cstring>

namespace pgt::mpsc {
namespace {

inline constexpr u64_t kMagic = 0x7067'745f'6d70'7363ull;  // "pgt_mpsc"
inline constexpr u32_t kVersion = 1;

// The control area occupies file offsets [0, ctrl): one Control block, one
// ShardControl per shard, then the shared writer-slot registry (occupancy
// bitmap + WriterSlot table -- shared because slot arbitration is
// cross-process; see region.hh), padded to a whole number of pages so the
// arenas that follow are page-aligned (a requirement for their mmap offsets).
[[nodiscard]] sz_t controlBytes(u32_t shards, sz_t page) noexcept {
  sz_t const bitmap_words = (static_cast<sz_t>(shards) + 63) / 64;
  sz_t const raw = sizeof(Control) + static_cast<sz_t>(shards) * sizeof(ShardControl) +
                   bitmap_words * sizeof(u64_t) + static_cast<sz_t>(shards) * sizeof(WriterSlot);
  return (raw + page - 1) & ~(page - 1);
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

[[nodiscard]] bool geometryOk(u64_t capacity, u32_t shards, sz_t page) noexcept {
  return shards != 0 && capacity != 0 && std::has_single_bit(capacity) &&
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
  capacity_ = 0;
  shard_count_ = 0;
}

Region::~Region() { reset(); }

Region::Region(Region&& other) noexcept
    : fd_(other.fd_),
      base_(other.base_),
      reservation_(other.reservation_),
      control_(other.control_),
      shards_(other.shards_),
      arena_(other.arena_),
      capacity_(other.capacity_),
      shard_count_(other.shard_count_) {
  other.fd_ = -1;
  other.base_ = nullptr;
  other.reservation_ = 0;
  other.control_ = nullptr;
  other.shards_ = nullptr;
  other.arena_ = nullptr;
  other.capacity_ = 0;
  other.shard_count_ = 0;
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
    capacity_ = other.capacity_;
    shard_count_ = other.shard_count_;
    other.fd_ = -1;
    other.base_ = nullptr;
    other.reservation_ = 0;
    other.control_ = nullptr;
    other.shards_ = nullptr;
    other.arena_ = nullptr;
    other.capacity_ = 0;
    other.shard_count_ = 0;
  }
  return *this;
}

bool Region::create(Config const& cfg, Region& out) noexcept {
  sz_t const page = pageSize();
  if (page == 0 || cfg.shards == 0) {
    errno = EINVAL;
    return false;
  }

  // Round capacity up to a power of two that is at least a page; page sizes are
  // powers of two, so bit_ceil covers the multiple-of-page requirement too.
  u64_t const cap = std::bit_ceil(cfg.capacity < page ? static_cast<u64_t>(page) : cfg.capacity);
  // A record extent may be as large as the whole arena, so the descriptor's
  // size field must be able to encode `cap` itself.
  if (cap > kMaxExtent) {
    errno = EINVAL;
    return false;
  }

  sz_t const ctrl = controlBytes(cfg.shards, page);
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
  out = std::move(r);
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
  sz_t const ctrl = controlBytes(hdr.shards, page);
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
  out = std::move(r);
  return true;
}

}  // namespace pgt::mpsc
