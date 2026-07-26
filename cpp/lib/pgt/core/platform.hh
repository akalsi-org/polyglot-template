#pragma once

#include "types.hh"

#include <bit>

#if defined(_WIN32)
#include <windows.h>
#elif defined(__unix__) || defined(__APPLE__)
#include <unistd.h>
#endif

namespace pgt {

enum class OperatingSystem {
  kLinux,
  kMacOs,
  kWindows,
  kOther,
};

#if defined(__linux__)
inline constexpr OperatingSystem kHostOs = OperatingSystem::kLinux;
#elif defined(__APPLE__)
inline constexpr OperatingSystem kHostOs = OperatingSystem::kMacOs;
#elif defined(_WIN32)
inline constexpr OperatingSystem kHostOs = OperatingSystem::kWindows;
#else
inline constexpr OperatingSystem kHostOs = OperatingSystem::kOther;
#endif

inline constexpr bool kIs64Bit = sizeof(void*) == 8;
inline constexpr std::endian kNativeEndianness = std::endian::native;
inline constexpr sz_t kCacheLineSize = 64;
inline constexpr sz_t kPreferredPageSize = 64 * 1024;
inline constexpr sz_t kMaxAlignment = alignof(max_align_t);

[[nodiscard]] inline sz_t pageSize() noexcept {
#if defined(_WIN32)
  SYSTEM_INFO system_info{};
  GetSystemInfo(&system_info);
  return system_info.dwPageSize;
#elif defined(__unix__) || defined(__APPLE__)
  auto const result = sysconf(_SC_PAGESIZE);
  return result > 0 ? static_cast<sz_t>(result) : 0;
#else
  return 0;
#endif
}

}  // namespace pgt
