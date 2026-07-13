#include "example/example.hh"

#include <doctest/doctest.h>

#include <string_view>

TEST_CASE("example message is stable across calls") {
  CHECK(example_message() != nullptr);
  CHECK_FALSE(std::string_view(example_message()).empty());
}
