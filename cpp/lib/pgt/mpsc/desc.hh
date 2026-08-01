#pragma once

// Record descriptor encoding, shared by every queue variant.
//
// Eight bytes per record, 64-byte-grain indexed. SpscRing stores the descriptor
// at the head of the in-band record; MpscRing stores it in the Claim plane.
// `state` occupies the low bits in EVERY encoding so it can be decoded before
// anything else.
//
//   bits 0..2     state   kFree=0 | kClaimed=1 | kCleared=2 | kCommitted=3 | kAborted=4
//
//   state == kFree:
//     bits  3..60   pos >> 6     the unwrapped position this slot IS
//     bits 61..63   reserved
//
//   otherwise:
//     bits  3..28   size         reservation extent, 64-byte units
//     bits 29..34   remainder    committed length mod 64
//     bits 35..56   tid          owning thread (22 bits; PID_MAX_LIMIT is 2^22)
//     bits 57..63   reserved
//
// Two properties this encoding exists to provide:
//
//   1. FREE carries its own position. A slot is claimable iff it reads exactly
//      kFree(p) for the position p the claimant believes it is at -- NOT "iff it
//      is zero". Two unwrapped positions differing by a multiple of the ring size
//      are the same physical word, so a zero-expecting CAS would let a writer
//      stopped across a lap boundary claim a slot from the wrong lap. pos>>6 needs
//      exactly 58 bits for a 64-bit position space and bits 3..60 provide exactly
//      58, so kFree(p) != kFree(p + k*N) unconditionally.
//
//   2. State records whether successor promotion is vouched. kCleared means the
//      owner completed the promotion; kClaimed does not. MpscRing may walk past a
//      kClaimed cell because its Claim plane never contains payload, but it must
//      not claim that record's immediate successor.
//
// The numeric state values are ABI: this word is shared between separately
// compiled processes. Do not reorder them, and do not express this as a bitfield
// (bit order and allocation within bitfields are implementation-defined).
//
// kFree is 0 deliberately. An untouched arena word is all zeros, which decodes as
// kFree(0) -- harmless, since any claimant at p != 0 mismatches and restarts. Had
// kAborted been 0, an untouched word would decode as a zero-extent aborted record
// and a walker reaching one would advance by zero.

#include "pgt/core/types.hh"

namespace pgt::mpsc {

// Record alignment. Records start on this boundary, so adjacent records never
// share a cache line. 64 matches the x86-64 coherence granule; raise to 128 if
// Apple M-series (whose line size IS 128) becomes a target.
inline constexpr sz_t kGrain = 64;
inline constexpr sz_t kHeaderSize = 8;

// Smallest possible record extent. Guarantees any gap is either zero or large
// enough to hold a descriptor, so a short commit's trailer always fits.
inline constexpr sz_t kMinExtent = kGrain;

enum class State : u8_t {
  kFree = 0,
  kClaimed = 1,
  kCleared = 2,
  kCommitted = 3,
  kAborted = 4,
};

inline constexpr u64_t kStateBits = 3;
inline constexpr u64_t kStateMask = (1ull << kStateBits) - 1;

// Non-free layout
inline constexpr u64_t kSizeShift = 3;
inline constexpr u64_t kSizeBits = 26;
inline constexpr u64_t kSizeMask = (1ull << kSizeBits) - 1;
inline constexpr u64_t kRemShift = 29;
inline constexpr u64_t kRemBits = 6;
inline constexpr u64_t kRemMask = (1ull << kRemBits) - 1;
inline constexpr u64_t kTidShift = 35;
inline constexpr u64_t kTidBits = 22;
inline constexpr u64_t kTidMask = (1ull << kTidBits) - 1;

// Free layout
inline constexpr u64_t kFreePosShift = 3;
inline constexpr u64_t kFreePosBits = 58;
inline constexpr u64_t kFreePosMask = (1ull << kFreePosBits) - 1;

// Largest representable reservation extent, in bytes.
inline constexpr u64_t kMaxExtent = kSizeMask * kGrain;

[[nodiscard]] inline constexpr State stateOf(u64_t w) noexcept {
  return static_cast<State>(w & kStateMask);
}

// The word that marks position `pos` as claimable. Only this exact value permits
// a claim at `pos`; see property (1) above.
[[nodiscard]] inline constexpr u64_t freeWord(u64_t pos) noexcept {
  return (((pos >> 6) & kFreePosMask) << kFreePosShift) | static_cast<u64_t>(State::kFree);
}

// Position encoded in a free word. Only meaningful when stateOf(w) == kFree.
// Reconstructs the low 58*64 bits of the position; callers compare against a
// position they already hold, so truncation above that is not observable.
[[nodiscard]] inline constexpr u64_t freePos(u64_t w) noexcept {
  return ((w >> kFreePosShift) & kFreePosMask) << 6;
}

[[nodiscard]] inline constexpr bool isFreeFor(u64_t w, u64_t pos) noexcept {
  return w == freeWord(pos);
}

[[nodiscard]] inline constexpr u64_t packRecord(u64_t extent_bytes, u32_t remainder, State state,
                                                u32_t tid) noexcept {
  return (static_cast<u64_t>(state) & kStateMask) |
         (((extent_bytes / kGrain) & kSizeMask) << kSizeShift) |
         ((static_cast<u64_t>(remainder) & kRemMask) << kRemShift) |
         ((static_cast<u64_t>(tid) & kTidMask) << kTidShift);
}

[[nodiscard]] inline constexpr u64_t withState(u64_t w, State s) noexcept {
  return (w & ~kStateMask) | (static_cast<u64_t>(s) & kStateMask);
}

// Reservation extent in bytes. Never zero for a legitimate record, which callers
// may assert: kMinExtent is 64 and a trailer's remainder is either 0 or >= 64.
[[nodiscard]] inline constexpr u64_t extentOf(u64_t w) noexcept {
  return ((w >> kSizeShift) & kSizeMask) * kGrain;
}

[[nodiscard]] inline constexpr u32_t tidOf(u64_t w) noexcept {
  return static_cast<u32_t>((w >> kTidShift) & kTidMask);
}

// Committed payload length. Only meaningful when stateOf(w) == kCommitted.
//
// For a given extent, extentFor() admits payloads in a window of exactly 64
// consecutive integers -- (extent - 72, extent - 8] -- so `remainder` (the
// payload length mod 64) identifies which one uniquely.
[[nodiscard]] inline constexpr u64_t committedLen(u64_t w) noexcept {
  u64_t const rem = (w >> kRemShift) & kRemMask;
  u64_t const hi = extentOf(w) - kHeaderSize;  // largest payload for this extent
  u64_t const delta = (hi - rem) & (kGrain - 1);
  return hi - delta;
}

// Largest payload whose extent is representable. THE CALLER MUST CHECK THIS
// BEFORE CALLING extentFor -- the rounding below overflows silently, and the
// overflow destroys the very information needed to reject the request.
//
// Measured, before this bound existed: extentFor(SIZE_MAX - 8) == 0. A ZERO
// extent is not merely a wrong number; desc.hh's own contract says an extent is
// never zero, and a walker advancing by zero does not terminate. The in-band
// admission check `need > max_need_` passes a zero extent happily, because zero
// is small.
inline constexpr sz_t kMaxPayload = static_cast<sz_t>(kMaxExtent) - kHeaderSize;

// Precondition: payload <= kMaxPayload. Use extentForChecked() at any boundary
// where the length is caller-supplied.
[[nodiscard]] inline constexpr sz_t extentFor(sz_t payload) noexcept {
  sz_t const need = kHeaderSize + payload;
  return (need + (kGrain - 1)) & ~(kGrain - 1);
}

// The admission-safe form: false means the request cannot be represented at all
// and must be rejected before any state changes. Written as a comparison
// against a precomputed bound rather than as an addition, so it cannot itself
// overflow.
[[nodiscard]] inline constexpr bool extentForChecked(sz_t payload, u64_t& out) noexcept {
  if (payload > kMaxPayload) return false;
  out = extentFor(payload);
  return true;
}

static_assert(kFreePosBits == 58, "free position must cover a 64-bit space at 64B grain");
// The bound must be tight AND overflow-free: one below it must round to a real
// extent, and the bound itself must not wrap.
static_assert(extentFor(kMaxPayload) == kMaxExtent, "kMaxPayload must round to kMaxExtent");
static_assert(extentFor(kMaxPayload) != 0, "the bound must not be the overflow value");
static_assert(kTidShift + kTidBits <= 64);
static_assert(static_cast<u64_t>(State::kFree) == 0, "untouched memory must decode as free");

}  // namespace pgt::mpsc
