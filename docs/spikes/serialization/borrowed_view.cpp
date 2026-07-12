#include <array>
#include <bit>
#include <chrono>
#include <cstddef>
#include <cstdint>
#include <cstdlib>
#include <cstring>
#include <iostream>
#include <new>
#include <stdexcept>
#include <span>
#include <string_view>
#include <vector>

namespace {

std::uint64_t allocation_count = 0;

constexpr std::uint32_t magic = 0x314B4250; // "PBK1" on little endian.
constexpr std::size_t header_size = 64;
constexpr std::size_t metadata_size = 16;
constexpr std::size_t level_size = 16;

void write_u32(std::span<std::byte> out, std::size_t at, std::uint32_t value) {
  if constexpr (std::endian::native == std::endian::big) {
    value = std::byteswap(value);
  }
  std::memcpy(out.data() + at, &value, sizeof(value));
}

void write_u64(std::span<std::byte> out, std::size_t at, std::uint64_t value) {
  if constexpr (std::endian::native == std::endian::big) {
    value = std::byteswap(value);
  }
  std::memcpy(out.data() + at, &value, sizeof(value));
}

std::uint32_t read_u32(std::span<const std::byte> in, std::size_t at) {
  std::uint32_t value{};
  std::memcpy(&value, in.data() + at, sizeof(value));
  if constexpr (std::endian::native == std::endian::big) {
    value = std::byteswap(value);
  }
  return value;
}

std::uint64_t read_u64(std::span<const std::byte> in, std::size_t at) {
  std::uint64_t value{};
  std::memcpy(&value, in.data() + at, sizeof(value));
  if constexpr (std::endian::native == std::endian::big) {
    value = std::byteswap(value);
  }
  return value;
}

class level_view {
public:
  explicit level_view(std::span<const std::byte, level_size> bytes) : bytes_(bytes) {}

  [[nodiscard]] std::int64_t price() const {
    return std::bit_cast<std::int64_t>(read_u64(bytes_, 0));
  }

  [[nodiscard]] std::uint64_t quantity() const { return read_u64(bytes_, 8); }

private:
  std::span<const std::byte, level_size> bytes_;
};

class levels_view {
public:
  explicit levels_view(std::span<const std::byte> bytes) : bytes_(bytes) {}

  [[nodiscard]] std::size_t size() const { return bytes_.size() / level_size; }

  [[nodiscard]] level_view operator[](std::size_t index) const {
    const auto offset = index * level_size;
    return level_view{std::span<const std::byte, level_size>{bytes_.subspan(offset, level_size)}};
  }

private:
  std::span<const std::byte> bytes_;
};

class metadata_view {
public:
  metadata_view(std::span<const std::byte> buffer, std::size_t offset)
      : buffer_(buffer), offset_(offset) {
    if (offset > buffer.size() || metadata_size > buffer.size() - offset) {
      throw std::runtime_error("invalid metadata range");
    }
  }

  [[nodiscard]] std::string_view venue() const {
    const auto offset = read_u32(buffer_, offset_);
    const auto size = read_u32(buffer_, offset_ + 4);
    if (offset > buffer_.size() || size > buffer_.size() - offset) {
      throw std::runtime_error("invalid venue range");
    }
    return {reinterpret_cast<const char*>(buffer_.data() + offset), size};
  }

private:
  std::span<const std::byte> buffer_;
  std::size_t offset_;
};

class book_view {
public:
  explicit book_view(std::span<const std::byte> bytes) : bytes_(bytes) {
    if (bytes.size() < header_size || read_u32(bytes, 0) != magic) {
      throw std::runtime_error("invalid book buffer");
    }
  }

  [[nodiscard]] std::uint64_t instrument_id() const { return read_u64(bytes_, 8); }
  [[nodiscard]] std::uint64_t sequence() const { return read_u64(bytes_, 16); }

  [[nodiscard]] metadata_view metadata() const { return metadata_view{bytes_, read_u32(bytes_, 24)}; }

  [[nodiscard]] levels_view bids() const {
    return levels(read_u32(bytes_, 32), read_u32(bytes_, 36));
  }

  [[nodiscard]] levels_view asks() const {
    return levels(read_u32(bytes_, 40), read_u32(bytes_, 44));
  }

private:
  [[nodiscard]] levels_view levels(std::size_t offset, std::size_t count) const {
    if (count > (bytes_.size() / level_size)) {
      throw std::runtime_error("invalid level count");
    }
    const auto size = count * level_size;
    if (offset > bytes_.size() || size > bytes_.size() - offset) {
      throw std::runtime_error("invalid level range");
    }
    return levels_view{bytes_.subspan(offset, size)};
  }

  std::span<const std::byte> bytes_;
};

void encode_level(std::span<std::byte> out, std::size_t offset, std::int64_t price,
                  std::uint64_t quantity) {
  write_u64(out, offset, std::bit_cast<std::uint64_t>(price));
  write_u64(out, offset + 8, quantity);
}

std::vector<std::byte> make_book(std::size_t depth) {
  constexpr std::string_view venue = "XNAS";
  const auto metadata_offset = header_size;
  const auto bids_offset = metadata_offset + metadata_size;
  const auto asks_offset = bids_offset + depth * level_size;
  const auto venue_offset = asks_offset + depth * level_size;
  std::vector<std::byte> out(venue_offset + venue.size());

  write_u32(out, 0, magic);
  write_u64(out, 8, 42);
  write_u64(out, 16, 9'001);
  write_u32(out, 24, metadata_offset);
  write_u32(out, metadata_offset, venue_offset);
  write_u32(out, metadata_offset + 4, venue.size());
  write_u32(out, 32, bids_offset);
  write_u32(out, 36, depth);
  write_u32(out, 40, asks_offset);
  write_u32(out, 44, depth);

  for (std::size_t i = 0; i < depth; ++i) {
    encode_level(out, bids_offset + i * level_size, 10'000 - static_cast<std::int64_t>(i), i + 1);
    encode_level(out, asks_offset + i * level_size, 10'001 + static_cast<std::int64_t>(i), i + 1);
  }
  std::memcpy(out.data() + venue_offset, venue.data(), venue.size());
  return out;
}

} // namespace

void* operator new(std::size_t size) {
  ++allocation_count;
  if (void* pointer = std::malloc(size)) {
    return pointer;
  }
  throw std::bad_alloc{};
}

void operator delete(void* pointer) noexcept { std::free(pointer); }
void operator delete(void* pointer, std::size_t) noexcept { std::free(pointer); }

int main() {
  constexpr std::size_t depth = 1'000;
  constexpr std::size_t iterations = 100'000;
  auto encoded = make_book(depth);
  const auto allocations_before_views = allocation_count;

  std::uint64_t checksum = 0;
  const auto started = std::chrono::steady_clock::now();
  for (std::size_t iteration = 0; iteration < iterations; ++iteration) {
    const book_view book{encoded};
    checksum += book.instrument_id() + book.sequence() + book.metadata().venue().size();
    for (std::size_t i = 0; i < book.bids().size(); ++i) {
      checksum += static_cast<std::uint64_t>(book.bids()[i].price());
      checksum += book.asks()[i].quantity();
    }
  }
  const auto elapsed = std::chrono::steady_clock::now() - started;
  const auto elapsed_ns = std::chrono::duration_cast<std::chrono::nanoseconds>(elapsed).count();

  std::cout << "wire_bytes=" << encoded.size() << '\n'
            << "decode_allocations=" << (allocation_count - allocations_before_views) << '\n'
            << "iterations=" << iterations << '\n'
            << "levels_per_side=" << depth << '\n'
            << "ns_per_iteration=" << elapsed_ns / iterations << '\n'
            << "checksum=" << checksum << '\n';
}
