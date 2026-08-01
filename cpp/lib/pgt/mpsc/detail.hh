#pragma once

#include "pgt/core/types.hh"

#include <cstdio>
#include <cstdlib>

namespace pgt::mpsc::detail {

inline constexpr u64_t kInvalidPos = ~u64_t{0};

// Misuse trap for silent-corruption paths. These checks remain active in release
// builds because the failure modes corrupt shared memory across processes.
[[noreturn, gnu::noinline, gnu::cold]] inline void misuseTrap(char const* what) noexcept {
  std::fprintf(stderr, "pgt::mpsc: fatal API misuse: %s\n", what);
  std::abort();
}

}  // namespace pgt::mpsc::detail

// Keep hot-path frames visible to ThreadSanitizer, so suppressions can identify
// exact functions rather than applying broad file-wide rules.
#if defined(__SANITIZE_THREAD__)
#define PGT_MPSC_TSAN 1
#elif defined(__has_feature)
#if __has_feature(thread_sanitizer)
#define PGT_MPSC_TSAN 1
#endif
#endif
#if defined(PGT_MPSC_TSAN)
#define PGT_MPSC_HOT
#else
#define PGT_MPSC_HOT [[gnu::always_inline]]
#endif
