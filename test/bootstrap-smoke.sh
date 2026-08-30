#!/usr/bin/env bash
set -euo pipefail

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd -P)
tmp=$(mktemp -d); trap 'rm -rf -- "$tmp"' EXIT
export POLYGLOT_LOCAL_DIR="$tmp/local"
# The resolved-lock fixture below is served over file://, which
# toolchain/fetch_binary.sh only accepts under this explicit test-only opt-in.
export POLYGLOT_ALLOW_FILE_URL=1

# fetch_binary.sh's scheme allowlist: only https:// is fetchable without the
# opt-in above, and no other scheme is fetchable at all.
expect_fetch_rejection() {
  local url=$1
  if env -u POLYGLOT_ALLOW_FILE_URL "$ROOT/toolchain/fetch_binary.sh" "$url" \
    0000000000000000000000000000000000000000000000000000000000000000 "$tmp/rejected" >/dev/null 2>&1; then
    printf 'fetch_binary.sh accepted a disallowed URL scheme: %s\n' "$url" >&2; exit 1
  fi
  [[ ! -e $tmp/rejected ]] || { printf 'fetch_binary.sh wrote output for a rejected URL: %s\n' "$url" >&2; exit 1; }
}
expect_fetch_rejection 'http://example.invalid/tool.tar.gz'
expect_fetch_rejection 'ftp://example.invalid/tool.tar.gz'
expect_fetch_rejection "file://$tmp/nothing.tar.gz"

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
expect_usage_failure "$ROOT/repo.sh" bootstrap --repair --repair
expect_usage_failure "$ROOT/repo.sh" bootstrap --offline --dry-run --repair extra
expect_usage_failure "$ROOT/repo.sh" toolchain-lock unexpected
expect_usage_failure "$ROOT/repo.sh" toolchain-qualify unexpected
expect_usage_failure "$ROOT/repo.sh" compile-commands dbg extra
expect_usage_failure "$ROOT/repo.sh" cpp-build dbg extra

help_output=$("$ROOT/repo.sh" help)
[[ $help_output == *bootstrap* ]]
[[ $help_output == *toolchain-lock* ]]
[[ $help_output == *toolchain-qualify* ]]
"$ROOT/repo.sh" toolchain-lock --check
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
url = "file://$tmp/fixture.tar.gz"
sha256 = "$sha"
archive = "fixture.tar.gz"
expected = "bin/fixture"
EOF
export POLYGLOT_LOCK_FILE="$tmp/resolved.lock.toml"
first=$("$ROOT/repo.sh" bootstrap --offline)
[[ $first == *'installed fixture 1.0'* ]]
second=$("$ROOT/repo.sh" bootstrap --offline)
[[ $second == *'already installed'* ]]
printf '#!/usr/bin/env sh\nprintf "corrupt fixture\\n"\n' >"$POLYGLOT_LOCAL_DIR/toolchain/$target/fixture-1.0/bin/fixture"
chmod +x "$POLYGLOT_LOCAL_DIR/toolchain/$target/fixture-1.0/bin/fixture"
repair=$("$ROOT/repo.sh" bootstrap --offline --repair)
[[ $repair == *'installed fixture 1.0'* ]]
[[ $("$POLYGLOT_LOCAL_DIR/toolchain/$target/fixture-1.0/bin/fixture") == 'fixture 1.0' ]]
printf 'corrupt cache\n' >"$POLYGLOT_LOCAL_DIR/downloads/$sha-fixture.tar.gz"
if "$ROOT/repo.sh" bootstrap --offline --repair >/dev/null 2>&1; then
  printf 'offline bootstrap unexpectedly accepted a corrupt cache\n' >&2; exit 1
fi
recovered=$("$ROOT/repo.sh" bootstrap --repair)
[[ $recovered == *'installed fixture 1.0'* ]]
[[ $(sha256sum "$POLYGLOT_LOCAL_DIR/downloads/$sha-fixture.tar.gz" | awk '{print $1}') == "$sha" ]]
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

# ShellCheck is a pinned artifact, and `./repo.sh lint` runs it
# unconditionally. It used to be an opportunistic `command -v` check, which
# meant its findings appeared only on CI, where the host happened to provide
# it - a comment that ShellCheck parsed as a malformed directive passed lint
# locally and broke the CI run. Assert both halves of the fix: the tool is
# locked, and lint depends on the pinned copy rather than PATH.
unset POLYGLOT_LOCK_FILE
grep -Fq 'tool = "shellcheck"' "$ROOT/tools.lock.toml"
grep -Fq '"$POLYGLOT_SHELLCHECK" --severity=error --shell=bash' "$ROOT/repo.sh"
if grep -Fq 'command -v shellcheck' "$ROOT/repo.sh"; then
  printf 'repo.sh still gates shell linting on a host-provided ShellCheck\n' >&2; exit 1
fi

# scratch_file is always assigned as `var=$(scratch_file)`, a subshell. An
# EXIT trap inside that function fires when the substitution ends and deletes
# the file the caller just received. The trap must belong to the invoking
# shell; the function body must only create the file.
scratch_body=$(awk '/^scratch_file\(\)/,/^}/' "$ROOT/repo.sh")
if grep -q 'trap' <<<"$scratch_body"; then
  printf 'scratch_file must not set an EXIT trap: $(scratch_file) is a subshell\n' >&2
  exit 1
fi
grep -Fq "trap 'rm -rf -- \"\$SCRATCH_DIR\"' EXIT" "$ROOT/repo.sh" || {
  printf 'repo.sh must register the scratch EXIT trap in the invoking shell\n' >&2
  exit 1
}

printf 'bootstrap smoke: ok\n'
