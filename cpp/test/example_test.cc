#include "example/example.hh"

#include <doctest/doctest.h>

#include <string_view>

TEST_CASE("example message identifies the template") {
  CHECK(std::string_view(example_message()) == "polyglot-template");
}
