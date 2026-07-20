#!/usr/bin/env bash
set -euo pipefail

ROOT=${POLYGLOT_ROOT:-$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd -P)}
LOCAL=${POLYGLOT_LOCAL_DIR:-$ROOT/.local}
POLYGLOT_LOCK_FILE=${POLYGLOT_LOCK_FILE:-$ROOT/tools.lock.toml}
export POLYGLOT_LOCK_FILE
. "$ROOT/toolchain/lock.sh"

(($# <= 1)) || { printf 'usage: ./repo.sh doctor [--deep]\n' >&2; exit 2; }
deep=0
case ${1:-} in --deep) deep=1;; '') :;; *) printf 'error: unknown doctor option: %s\n' "$1" >&2; exit 2;; esac
target=$($ROOT/toolchain/target.sh)
printf 'ok: target %s\n' "$target"

artifact_is_installed() {
  local kind=$1 install=$2 expected=$3 sha=$4
  [[ -f $install/.installed-$sha ]] || return 1
  case "$kind" in
    executable) [[ -x $install/$expected ]] ;;
    header) [[ -f $install/$expected ]] ;;
    *) return 1 ;;
  esac
}

deep_validate_python() {
  local install=$1 expected=$2 gcc_version loader gcc_install loader_dir
  gcc_version=$(lock_value gcc-musl "$target" version)
  loader=$(lock_value gcc-musl "$target" loader)
  gcc_install="$LOCAL/toolchain/$target/gcc-musl-$gcc_version"
  [[ $loader != UNRESOLVED* && -x $gcc_install/$loader ]] || {
    printf 'invalid: unresolved musl loader\n' >&2; return 1;
  }
  loader_dir=$(dirname -- "$gcc_install/$loader")
  "$gcc_install/$loader" --library-path "$loader_dir:$install/python/lib" "$install/$expected" -I -c \
    'import ctypes, sqlite3, ssl, zlib' >/dev/null
}

deep_validate_gcc() {
  local install=$1 expected=$2 mold binutil binutil_rel
  mold=$(lock_value gcc-musl "$target" mold)
  [[ -x $install/$mold ]] || { printf 'invalid: missing mold\n' >&2; return 1; }
  for binutil in ar ranlib nm strip objcopy ld; do
    binutil_rel=$(lock_value gcc-musl "$target" "$binutil")
    [[ -x $install/$binutil_rel ]] || { printf 'invalid: missing %s\n' "$binutil" >&2; return 1; }
  done
  [[ $("$install/$expected" -dumpmachine) == "$target" ]] || {
    printf 'invalid: compiler target mismatch\n' >&2; return 1;
  }
  "$install/$mold" --version | grep -q '^mold 2\.41\.0' || return 1
  "$install/$expected" -std=gnu++26 -freflection -fsyntax-only "$ROOT/cpp/test/reflection.cc"
}

deep_validate_deno() {
  local install=$1 expected=$2 version=$3
  [[ $("$install/$expected" --version | awk 'NR == 1 { print $2 }') == "$version" ]] || {
    printf 'invalid: Deno version mismatch\n' >&2; return 1;
  }
}

deep_validate_go() {
  local install=$1 expected=$2 version=$3
  [[ $("$install/$expected" version | awk '{ print $3 }') == "go$version" ]] || {
    printf 'invalid: Go version mismatch\n' >&2; return 1;
  }
}

deep_validate_clang_format() {
  local install=$1 expected=$2 gcc_version loader gcc_install loader_dir
  gcc_version=$(lock_value gcc-musl "$target" version)
  loader=$(lock_value gcc-musl "$target" loader)
  gcc_install="$LOCAL/toolchain/$target/gcc-musl-$gcc_version"
  loader_dir=$(dirname -- "$gcc_install/$loader")
  "$gcc_install/$loader" --library-path "$loader_dir:$install/clang_format.libs" "$install/$expected" --version >/dev/null
}

deep_validate_doctest() {
  local install=$1 expected=$2
  grep -q '^#define DOCTEST_VERSION_MAJOR 2$' "$install/$expected"
}

deep_validate_buck2() {
  local install=$1 expected=$2 declared_hash reported
  "$install/$expected" --version >/dev/null
  declared_hash=$(lock_value buck2 "$target" content_hash)
  [[ -z $declared_hash ]] && return 0
  reported=$("$install/$expected" --version | awk '{ print $2 }')
  [[ $reported == "$declared_hash" ]] || {
    printf 'invalid: buck2 reported content hash %s does not match pinned %s\n' "$reported" "$declared_hash" >&2
    return 1
  }
}

deep_validate() {
  local tool=$1 kind=$2 install=$3 expected=$4 version=$5
  case "$tool:$kind" in
    python:*) deep_validate_python "$install" "$expected" ;;
    gcc-musl:*) deep_validate_gcc "$install" "$expected" ;;
    deno:*) deep_validate_deno "$install" "$expected" "$version" ;;
    go:*) deep_validate_go "$install" "$expected" "$version" ;;
    clang-format:*) deep_validate_clang_format "$install" "$expected" ;;
    doctest:*) deep_validate_doctest "$install" "$expected" ;;
    buck2:*) deep_validate_buck2 "$install" "$expected" ;;
    *:executable) "$install/$expected" --version >/dev/null ;;
  esac
}

failed=0
while IFS= read -r tool; do
  version=$(lock_value "$tool" "$target" version)
  sha=$(lock_value "$tool" "$target" sha256)
  expected=$(lock_value "$tool" "$target" expected)
  kind=$(lock_value "$tool" "$target" kind)
  kind=${kind:-executable}
  install="$LOCAL/toolchain/$target/$tool-$version"
  if [[ ! $sha =~ ^[0-9a-f]{64}$ ]]; then
    printf 'unresolved: %s %s\n' "$tool" "$version"; failed=1; continue
  fi
  if ! artifact_is_installed "$kind" "$install" "$expected" "$sha"; then
    printf 'missing: %s %s\n' "$tool" "$version"; failed=1; continue
  fi
  if ((deep)) && ! deep_validate "$tool" "$kind" "$install" "$expected" "$version"; then
    failed=1; continue
  fi
  printf 'ok: %s %s\n' "$tool" "$version"
done < <(locked_tools)
((failed == 0))
