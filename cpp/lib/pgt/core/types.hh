#pragma once

#include <cstddef>
#include <cstdint>

namespace pgt {

using u8_t = std::uint8_t;
using u16_t = std::uint16_t;
using u32_t = std::uint32_t;
using u64_t = std::uint64_t;
using u128_t = __uint128_t;

using i8_t = std::int8_t;
using i16_t = std::int16_t;
using i32_t = std::int32_t;
using i64_t = std::int64_t;
using i128_t = __int128_t;

using f32_t = float;
using f64_t = double;

using sz_t = std::size_t;
using ssz_t = std::ptrdiff_t;
using max_align_t = std::max_align_t;

}  // namespace pgt
