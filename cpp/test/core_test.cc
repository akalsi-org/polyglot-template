#include "pgt/core/types.hh"
#include "pgt/core/platform.hh"

#include <doctest/doctest.h>

#include <bit>
#include <cstddef>
#include <cstdint>
#include <type_traits>

static_assert(std::is_same_v<pgt::u8_t, std::uint8_t>);
static_assert(std::is_same_v<pgt::u16_t, std::uint16_t>);
static_assert(std::is_same_v<pgt::u32_t, std::uint32_t>);
static_assert(std::is_same_v<pgt::u64_t, std::uint64_t>);
static_assert(std::is_same_v<pgt::u128_t, __uint128_t>);

static_assert(std::is_same_v<pgt::i8_t, std::int8_t>);
static_assert(std::is_same_v<pgt::i16_t, std::int16_t>);
static_assert(std::is_same_v<pgt::i32_t, std::int32_t>);
static_assert(std::is_same_v<pgt::i64_t, std::int64_t>);
static_assert(std::is_same_v<pgt::i128_t, __int128_t>);

static_assert(std::is_same_v<pgt::f32_t, float>);
static_assert(std::is_same_v<pgt::f64_t, double>);
static_assert(std::is_same_v<pgt::sz_t, std::size_t>);
static_assert(std::is_same_v<pgt::ssz_t, std::ptrdiff_t>);
static_assert(std::is_same_v<pgt::max_align_t, std::max_align_t>);
static_assert(pgt::kIs64Bit);
static_assert(pgt::kNativeEndianness == std::endian::little);
static_assert(pgt::kCacheLineSize == 64);
static_assert(pgt::kPreferredPageSize == 64 * 1024);
static_assert(pgt::kMaxAlignment == alignof(std::max_align_t));

TEST_CASE("pgt core aliases and platform properties are available") {
  CHECK(sizeof(pgt::u128_t) == 16);
  auto const pageSize = pgt::pageSize();
  REQUIRE(pageSize > 0);
  CHECK(pgt::kPreferredPageSize % pageSize == 0);
}
