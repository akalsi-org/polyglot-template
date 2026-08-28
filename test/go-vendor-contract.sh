#!/usr/bin/env bash
set -euo pipefail

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd -P)
tmp=$(mktemp -d); trap 'rm -rf -- "$tmp"' EXIT

# A normal external module is deliberately used by go/test/greeting_test.go.
# Start with entirely empty Go caches and disable both module services: this
# succeeds only when the checked-in vendor tree is a complete closure.
grep -Fq 'github.com/google/go-cmp v0.7.0' "$ROOT/go.mod"
grep -Fq 'github.com/google/go-cmp v0.7.0' "$ROOT/go.sum"
grep -Fq '# github.com/google/go-cmp v0.7.0' "$ROOT/vendor/modules.txt"
grep -Fq 'github.com/google/go-cmp/cmp' "$ROOT/go/test/greeting_test.go"
grep -Fq "GOENV=off GOTOOLCHAIN=local GOFLAGS='-mod=vendor -buildvcs=false'" "$ROOT/rules/go.bzl"
grep -Fq 'build -buildvcs=false -trimpath' "$ROOT/rules/go.bzl"
grep -Fq 'test -buildvcs=false -trimpath' "$ROOT/rules/go.bzl"
grep -Fq '"_gosum": attrs.source(default = "//:go.sum")' "$ROOT/rules/go.bzl"
grep -Fq '"_vendor": attrs.dep(default = "//:go_vendor"' "$ROOT/rules/go.bzl"

real_go=$("$ROOT/repo.sh" exec sh -c 'printf "%s\n" "$POLYGLOT_GO"')
"$ROOT/repo.sh" exec env \
  GOPATH="$tmp/gopath" \
  GOMODCACHE="$tmp/gomodcache" \
  GOCACHE="$tmp/gocache" \
  GOENV=off \
  GOFLAGS='-mod=vendor -buildvcs=false' \
  GOPROXY=off \
  GOSUMDB=off \
  "$real_go" test -trimpath ./go/test

# Persist hostile user defaults through the real Go binary. Repository launchers
# must ignore this file instead of inheriting `go env -w` state.
user_goenv="$tmp/user-goenv"
GOENV="$user_goenv" "$real_go" env -w \
  CGO_ENABLED=1 GOFLAGS=-mod=mod GOTOOLCHAIN=auto
repo_go_env=$(GOENV="$user_goenv" "$ROOT/repo.sh" go env GOENV GOTOOLCHAIN CGO_ENABLED GOFLAGS)
mapfile -t repo_go_values <<<"$repo_go_env"
[[ -z ${repo_go_values[0]} ]]
[[ ${repo_go_values[1]} == local ]]
[[ ${repo_go_values[2]} == 0 ]]
[[ ${repo_go_values[3]} == *'-mod=vendor'* && ${repo_go_values[3]} == *'-buildvcs=false'* ]]
wrapper_go_env=$(env -i PATH=/usr/bin:/bin GOENV="$user_goenv" GOFLAGS=-mod=mod \
  "$ROOT/.local/bin/go" env GOENV GOTOOLCHAIN CGO_ENABLED GOFLAGS)
mapfile -t wrapper_go_values <<<"$wrapper_go_env"
[[ -z ${wrapper_go_values[0]} ]]
[[ ${wrapper_go_values[1]} == local ]]
[[ ${wrapper_go_values[2]} == 0 ]]
[[ ${wrapper_go_values[3]} == *'-mod=vendor'* && ${wrapper_go_values[3]} == *'-buildvcs=false'* ]]

# Go embeds repository status by default. An unrelated dirty file must not
# change a repository-built binary or add VCS settings to it.
fixture="$tmp/vcs-fixture"
mkdir -p "$fixture/cmd"
cat >"$fixture/go.mod" <<'EOF'
module example.com/vcs-fixture

go 1.27
EOF
cat >"$fixture/cmd/main.go" <<'EOF'
package main

func main() {}
EOF
printf 'unrelated\n' >"$fixture/README.md"
git -C "$fixture" init -q
git -C "$fixture" config user.email contract@example.invalid
git -C "$fixture" config user.name 'Contract Test'
git -C "$fixture" add go.mod cmd/main.go README.md
git -C "$fixture" commit -qm initial
(
  cd "$fixture"
  GOENV="$user_goenv" "$ROOT/repo.sh" go build -trimpath -o clean ./cmd
  printf 'dirty\n' >>README.md
  GOENV="$user_goenv" "$ROOT/repo.sh" go build -trimpath -o dirty ./cmd
)
cmp "$fixture/clean" "$fixture/dirty"
if "$real_go" version -m "$fixture/dirty" | grep -Fq $'build\tvcs='; then
  printf 'Go environment contract: binary contains undeclared VCS metadata\n' >&2
  exit 1
fi

printf 'Go vendor and environment contracts: ok\n'
