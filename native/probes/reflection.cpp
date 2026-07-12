#include <meta>

struct reflected_type {
  int value;
};

constexpr auto reflected = ^^reflected_type;
static_assert(std::meta::is_type(reflected));

int main() {}
