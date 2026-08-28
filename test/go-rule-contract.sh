#!/usr/bin/env bash
set -euo pipefail

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd -P)
fixture_name="closure_contract_${BASHPID}"
fixture="$root/go/$fixture_name"
output="$root/buck-out/v2/tmp/$fixture_name.log"

cleanup() {
  rm -rf -- "$fixture" "$output"
}
trap cleanup EXIT

mkdir -p "$fixture/dep" "$fixture/declared" "$fixture/missing" "$fixture/orphan" "$(dirname -- "$output")"
cat >"$fixture/dep/dep.go" <<'EOF'
package dep

func Value() string { return "declared closure" }
EOF
cat >"$fixture/declared/main.go" <<EOF
package main

import (
  "fmt"
  "github.com/akalsi-org/polyglot-template/go/$fixture_name/dep"
)

func main() { fmt.Println(dep.Value()) }
EOF
cp -- "$fixture/declared/main.go" "$fixture/missing/main.go"
cat >"$fixture/orphan/orphan.go" <<'EOF'
package orphan

const Undeclared = true
EOF
cat >"$fixture/BUCK" <<EOF
load("//rules:go.bzl", "go_binary", "go_graph_check", "go_library")

go_library(
  name = "dep",
  srcs = ["dep/dep.go"],
)

go_binary(
  name = "declared",
  package = "./go/$fixture_name/declared",
  srcs = ["declared/main.go"],
  deps = [":dep"],
)

go_binary(
  name = "missing",
  package = "./go/$fixture_name/missing",
  srcs = ["missing/main.go"],
)

go_graph_check(
  name = "lane_graph",
  packages = [
    "./go/$fixture_name/declared",
    "./go/$fixture_name/missing",
  ],
  deps = [":dep", ":declared", ":missing"],
)

go_graph_check(
  name = "undeclared_graph",
  packages = ["./go/$fixture_name/orphan"],
)
EOF

cd "$root"
binary=$(./repo.sh buck2 build --show-output "//go/$fixture_name:declared" | awk 'NF == 2 { print $2 }')
[[ -n $binary && -x $root/$binary ]] || {
  printf 'Go rule contract: build did not report an executable output\n' >&2
  exit 1
}
if ./repo.sh go version -m "$root/$binary" | grep -Fq $'build\tvcs='; then
  printf 'Go rule contract: packaged binary contains undeclared VCS metadata\n' >&2
  exit 1
fi
if ./repo.sh buck2 build "//go/$fixture_name:lane_graph" >"$output" 2>&1; then
  printf 'Go rule contract: the lane graph hid an undeclared per-target dependency\n' >&2
  exit 1
fi
grep -Fq "//go/$fixture_name:missing" "$output" || {
  printf 'Go rule contract: failure did not identify the target\n' >&2
  exit 1
}
grep -Fq "go/$fixture_name/dep/dep.go" "$output" || {
  printf 'Go rule contract: failure did not identify the missing dependency source\n' >&2
  exit 1
}

if ./repo.sh buck2 build "//go/$fixture_name:undeclared_graph" >"$output" 2>&1; then
  printf 'Go rule contract: the lane-wide undeclared-file check passed\n' >&2
  exit 1
fi
grep -Fq 'missing from the declared srcs/deps graph' "$output" || {
  printf 'Go rule contract: lane-wide failure lost its contract diagnostic\n' >&2
  exit 1
}
grep -Fq "go/$fixture_name/orphan/orphan.go" "$output" || {
  printf 'Go rule contract: lane-wide failure did not identify the undeclared file\n' >&2
  exit 1
}

printf 'Go per-target and lane dependency closure contracts: ok\n'
