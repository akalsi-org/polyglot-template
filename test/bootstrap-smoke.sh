#!/usr/bin/env bash
set -euo pipefail

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd -P)
tmp=$(mktemp -d); trap 'rm -rf -- "$tmp"' EXIT
export POLYGLOT_LOCAL_DIR="$tmp/local"

[[ $($ROOT/repo.sh target) =~ ^(x86_64|aarch64)-linux-musl$ ]]
[[ $(POLYGLOT_TEST_MACHINE=amd64 "$ROOT/repo.sh" target) == x86_64-linux-musl ]]
[[ $(POLYGLOT_TEST_MACHINE=arm64 "$ROOT/repo.sh" target) == aarch64-linux-musl ]]
if POLYGLOT_TEST_MACHINE=riscv64 "$ROOT/repo.sh" target >/dev/null 2>&1; then
  printf 'unsupported target unexpectedly succeeded\n' >&2; exit 1
fi
if POLYGLOT_TEST_OS=Darwin "$ROOT/repo.sh" target >/dev/null 2>&1; then
  printf 'unsupported host operating system unexpectedly succeeded\n' >&2; exit 1
fi

expect_usage_failure() {
  local status
  set +e
  "$@" >/dev/null 2>&1
  status=$?
  set -e
  ((status == 2)) || { printf 'command did not reject invalid arity: %s\n' "$*" >&2; exit 1; }
}
expect_usage_failure "$ROOT/repo.sh" help extra
expect_usage_failure "$ROOT/repo.sh" target extra
expect_usage_failure "$ROOT/repo.sh" doctor --deep extra
expect_usage_failure "$ROOT/repo.sh" bootstrap --offline --offline
expect_usage_failure "$ROOT/repo.sh" compile-commands dbg extra
expect_usage_failure "$ROOT/repo.sh" cpp-build dbg extra

help_output=$("$ROOT/repo.sh" help)
[[ $help_output == *bootstrap* ]]
grep -Fq 'chmod -R u+w -- "$install"' "$ROOT/toolchain/bootstrap.sh"
grep -Fq 'chmod -R u+w -- "$tmp"' "$ROOT/toolchain/bootstrap.sh"
target=$("$ROOT/repo.sh" target)
awk -v target="$target" '
  /^\[\[artifact\]\]$/ { selected = 0 }
  $0 == "target = \"" target "\"" { selected = 1 }
  selected && /^url = / && !replaced_url {
    print "url = \"UNRESOLVED: fixture\""
    replaced_url = 1
    next
  }
  selected && /^sha256 = / && !replaced_sha {
    print "sha256 = \"UNRESOLVED\""
    replaced_sha = 1
    next
  }
  { print }
  END {
    if (!replaced_url || !replaced_sha) exit 1
  }
' "$ROOT/tools.lock.toml" >"$tmp/unresolved.lock.toml"
export POLYGLOT_LOCK_FILE="$tmp/unresolved.lock.toml"
dry_output=$("$ROOT/repo.sh" bootstrap --dry-run --offline)
[[ $dry_output == *'LOCK UNRESOLVED'* ]]
if "$ROOT/repo.sh" bootstrap --offline >/dev/null 2>&1; then
  printf 'unresolved live bootstrap unexpectedly succeeded\n' >&2; exit 1
fi
if "$ROOT/repo.sh" doctor >/dev/null 2>&1; then
  printf 'doctor unexpectedly accepted unresolved lock\n' >&2; exit 1
fi

# Exercise the complete offline, checksum-verified, idempotent install path with
# a synthetic executable. This deliberately uses no host compiler.
mkdir -p "$tmp/payload/bin" "$POLYGLOT_LOCAL_DIR/downloads"
printf '#!/usr/bin/env sh\nprintf "fixture 1.0\\n"\n' >"$tmp/payload/bin/fixture"
chmod +x "$tmp/payload/bin/fixture"
tar -czf "$tmp/fixture.tar.gz" -C "$tmp/payload" .
sha=$(sha256sum "$tmp/fixture.tar.gz" | awk '{print $1}')
cp "$tmp/fixture.tar.gz" "$POLYGLOT_LOCAL_DIR/downloads/$sha-fixture.tar.gz"
cat >"$tmp/resolved.lock.toml" <<EOF
schema = 1
[[artifact]]
tool = "fixture"
target = "$target"
version = "1.0"
url = "https://invalid.example/fixture.tar.gz"
sha256 = "$sha"
archive = "fixture.tar.gz"
expected = "bin/fixture"
EOF
export POLYGLOT_LOCK_FILE="$tmp/resolved.lock.toml"
first=$("$ROOT/repo.sh" bootstrap --offline)
[[ $first == *'installed fixture 1.0'* ]]
second=$("$ROOT/repo.sh" bootstrap --offline)
[[ $second == *'already installed'* ]]
"$ROOT/repo.sh" doctor --deep | grep -q 'ok: fixture 1.0'

# Header-only artifacts are pinned and installed without pretending to be
# executable tools.
mkdir -p "$tmp/header-payload/include"
printf '#define FIXTURE_HEADER 1\n' >"$tmp/header-payload/include/fixture.h"
tar -czf "$tmp/header.tar.gz" -C "$tmp/header-payload" .
header_sha=$(sha256sum "$tmp/header.tar.gz" | awk '{print $1}')
cp "$tmp/header.tar.gz" "$POLYGLOT_LOCAL_DIR/downloads/$header_sha-header.tar.gz"
cat >"$tmp/header.lock.toml" <<EOF
schema = 1
[[artifact]]
tool = "header-fixture"
target = "$target"
version = "1.0"
url = "https://invalid.example/header.tar.gz"
sha256 = "$header_sha"
archive = "header.tar.gz"
expected = "include/fixture.h"
kind = "header"
EOF
export POLYGLOT_LOCK_FILE="$tmp/header.lock.toml"
"$ROOT/repo.sh" bootstrap --offline | grep -q 'installed header-fixture 1.0'
"$ROOT/repo.sh" doctor --deep | grep -q 'ok: header-fixture 1.0'

printf 'bootstrap smoke: ok\n'
