#!/usr/bin/env bash
set -euo pipefail

ROOT=${POLYGLOT_ROOT:-$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd -P)}
LOCAL=${POLYGLOT_LOCAL_DIR:-$ROOT/.local}
POLYGLOT_LOCK_FILE=${POLYGLOT_LOCK_FILE:-$ROOT/tools.lock.toml}
export POLYGLOT_LOCK_FILE
. "$ROOT/toolchain/lock.sh"

deep=0
case ${1:-} in --deep) deep=1;; '') :;; *) printf 'error: unknown doctor option: %s\n' "$1" >&2; exit 2;; esac
target=$($ROOT/toolchain/target.sh)
printf 'ok: target %s\n' "$target"
failed=0
while IFS= read -r tool; do
  version=$(lock_value "$tool" "$target" version)
  sha=$(lock_value "$tool" "$target" sha256)
  expected=$(lock_value "$tool" "$target" expected)
  install="$LOCAL/toolchain/$target/$tool-$version"
  if [[ ! $sha =~ ^[0-9a-f]{64}$ ]]; then
    printf 'unresolved: %s %s\n' "$tool" "$version"; failed=1; continue
  fi
  if [[ ! -f $install/.installed-$sha || ! -x $install/$expected ]]; then
    printf 'missing: %s %s\n' "$tool" "$version"; failed=1; continue
  fi
  if ((deep)); then
    if [[ $tool == python ]]; then
      gcc_version=$(lock_value gcc-musl "$target" version)
      loader=$(lock_value gcc-musl "$target" loader)
      gcc_install="$LOCAL/toolchain/$target/gcc-musl-$gcc_version"
      [[ $loader != UNRESOLVED* && -x $gcc_install/$loader ]] || { printf 'invalid: unresolved musl loader\n'; failed=1; continue; }
      loader_dir=$(dirname -- "$gcc_install/$loader")
      "$gcc_install/$loader" --library-path "$loader_dir:$install/python/lib" "$install/$expected" -I -c \
        'import ctypes, sqlite3, ssl, zlib' >/dev/null
    elif [[ $tool == gcc-musl ]]; then
      mold=$(lock_value "$tool" "$target" mold)
      [[ -x $install/$mold ]] || { printf 'invalid: missing mold\n'; failed=1; continue; }
      [[ $("$install/$expected" -dumpmachine) == "$target" ]] || { printf 'invalid: compiler target mismatch\n'; failed=1; continue; }
      "$install/$mold" --version | grep -q '^mold 2\.41\.0'
      "$install/$expected" -std=gnu++26 -freflection -fsyntax-only "$ROOT/cpp/test/reflection.cc"
    elif [[ $tool == deno ]]; then
      [[ $("$install/$expected" --version | awk 'NR == 1 { print $2 }') == "$version" ]] || {
        printf 'invalid: Deno version mismatch\n'; failed=1; continue;
      }
    elif [[ $tool == ninja ]]; then
      [[ $("$install/$expected" --version) == "$version" ]] || {
        printf 'invalid: Ninja version mismatch\n'; failed=1; continue;
      }
    else
      "$install/$expected" --version >/dev/null
    fi
  fi
  printf 'ok: %s %s\n' "$tool" "$version"
done < <(locked_tools)
((failed == 0))
