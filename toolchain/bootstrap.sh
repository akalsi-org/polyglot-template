#!/usr/bin/env bash
set -euo pipefail

ROOT=${POLYGLOT_ROOT:-$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd -P)}
LOCAL=${POLYGLOT_LOCAL_DIR:-$ROOT/.local}
POLYGLOT_LOCK_FILE=${POLYGLOT_LOCK_FILE:-$ROOT/tools.lock.toml}
export POLYGLOT_LOCK_FILE
. "$ROOT/toolchain/lock.sh"

offline=0 dry_run=0
for arg in "$@"; do
  case "$arg" in
    --offline) offline=1 ;;
    --dry-run) dry_run=1 ;;
    *) printf 'error: unknown bootstrap option: %s\n' "$arg" >&2; exit 2 ;;
  esac
done
[[ -f $POLYGLOT_LOCK_FILE ]] || { printf 'error: lock file not found: %s\n' "$POLYGLOT_LOCK_FILE" >&2; exit 1; }
target=$($ROOT/toolchain/target.sh)

install_one() {
  local tool=$1 version url sha archive expected install stamp cache tmp old listing link resolved
  version=$(lock_value "$tool" "$target" version)
  url=$(lock_value "$tool" "$target" url)
  sha=$(lock_value "$tool" "$target" sha256)
  archive=$(lock_value "$tool" "$target" archive)
  expected=$(lock_value "$tool" "$target" expected)
  [[ -n $version && -n $archive && -n $expected ]] || {
    printf 'error: no complete %s artifact for %s\n' "$tool" "$target" >&2; return 1;
  }
  install="$LOCAL/toolchain/$target/$tool-$version"
  stamp="$install/.installed-$sha"
  cache="$LOCAL/downloads/$sha-$archive"
  if [[ -f $stamp && -x $install/$expected ]]; then
    printf 'bootstrap: %s %s already installed\n' "$tool" "$version"; return
  fi
  if [[ $url == UNRESOLVED* || ! $sha =~ ^[0-9a-f]{64}$ ]]; then
    if ((dry_run)); then
      printf 'bootstrap: would install %s %s for %s (LOCK UNRESOLVED)\n' "$tool" "$version" "$target"; return
    fi
    printf 'error: %s %s for %s is unresolved in tools.lock.toml\n' "$tool" "$version" "$target" >&2
    return 1
  fi
  if ((dry_run)); then printf 'bootstrap: would install %s %s for %s\n' "$tool" "$version" "$target"; return; fi
  if [[ ! -f $cache ]]; then
    ((offline == 0)) || { printf 'error: offline cache miss for %s %s\n' "$tool" "$version" >&2; return 1; }
    "$ROOT/toolchain/fetch_binary.sh" "$url" "$sha" "$cache"
  else
    local actual; actual=$(sha256sum "$cache" | awk '{print $1}')
    [[ $actual == "$sha" ]] || { printf 'error: cached checksum mismatch for %s\n' "$tool" >&2; return 1; }
  fi
  tmp="$LOCAL/toolchain/$target/.${tool}-${version}.tmp.$$"
  rm -rf -- "$tmp"; mkdir -p "$tmp"
  trap 'rm -rf -- "$tmp"' RETURN
  case "$archive" in
    *.tar.gz|*.tgz) listing=$(tar -tzf "$cache"); validate_members "$listing"; tar -xzf "$cache" -C "$tmp" --no-same-owner --no-same-permissions ;;
    *.tar.xz) listing=$(tar -tJf "$cache"); validate_members "$listing"; tar -xJf "$cache" -C "$tmp" --no-same-owner --no-same-permissions ;;
    *.tar.zst) listing=$(tar --zstd -tf "$cache"); validate_members "$listing"; tar --zstd -xf "$cache" -C "$tmp" --no-same-owner --no-same-permissions ;;
    *.zip) command -v unzip >/dev/null; listing=$(unzip -Z1 "$cache"); validate_members "$listing"; unzip -q "$cache" -d "$tmp" ;;
    *) printf 'error: unsupported archive: %s\n' "$archive" >&2; return 1 ;;
  esac
  if [[ $tool == gcc-musl ]]; then
    local declared_loader loader_target
    declared_loader=$(lock_value "$tool" "$target" loader)
    [[ -L $tmp/$declared_loader ]] || { printf 'error: gcc-musl declared loader is missing\n' >&2; return 1; }
    loader_target=$(readlink -- "$tmp/$declared_loader")
    if [[ $loader_target == /lib/libc.so ]]; then
      rm -- "$tmp/$declared_loader"
      ln -s libc.so "$tmp/$declared_loader"
    fi
  fi
  while IFS= read -r -d '' link; do
    resolved=$(realpath -m -- "$link")
    case "$resolved" in "$tmp"/*) :;; *) printf 'error: archive contains escaping symlink: %s\n' "$link" >&2; return 1;; esac
  done < <(find "$tmp" -type l -print0)
  [[ -x $tmp/$expected ]] || { printf 'error: %s missing expected executable %s\n' "$tool" "$expected" >&2; return 1; }
  if [[ $tool == python ]]; then
    local gcc_version loader gcc_install loader_dir
    gcc_version=$(lock_value gcc-musl "$target" version)
    loader=$(lock_value gcc-musl "$target" loader)
    gcc_install="$LOCAL/toolchain/$target/gcc-musl-$gcc_version"
    [[ $loader != UNRESOLVED* && -x $gcc_install/$loader ]] || {
      printf 'error: Python probe requires resolved musl loader from gcc-musl\n' >&2; return 1;
    }
    loader_dir=$(dirname -- "$gcc_install/$loader")
    "$gcc_install/$loader" --library-path "$loader_dir:$tmp/python/lib" "$tmp/$expected" -I -c \
      'import ctypes, hashlib, sqlite3, ssl, sys, zlib; print(sys.version)' >/dev/null || {
      printf 'error: Python musl-loader capability probe failed\n' >&2; return 1;
    }
  elif [[ $tool == gcc-musl ]]; then
    local mold dumpmachine probe_source
    mold=$(lock_value "$tool" "$target" mold)
    [[ -x $tmp/$mold ]] || { printf 'error: gcc-musl missing declared mold executable\n' >&2; return 1; }
    dumpmachine=$("$tmp/$expected" -dumpmachine)
    [[ $dumpmachine == "$target" ]] || { printf 'error: compiler target mismatch: %s\n' "$dumpmachine" >&2; return 1; }
    "$tmp/$mold" --version | grep -q '^mold 2\.41\.0' || { printf 'error: mold capability probe failed\n' >&2; return 1; }
    probe_source="$ROOT/cpp/test/reflection.cc"
    "$tmp/$expected" -std=gnu++26 -freflection -fsyntax-only "$probe_source" || {
      printf 'error: GCC C++26 reflection capability probe failed\n' >&2; return 1;
    }
  elif [[ $tool == go ]]; then
    "$tmp/$expected" version | grep -q "go$version" || { printf 'error: Go capability probe failed\n' >&2; return 1; }
  else
    "$tmp/$expected" --version >/dev/null 2>&1 || { printf 'error: %s capability probe failed\n' "$tool" >&2; return 1; }
  fi
  old="$install.replaced.$$"
  [[ ! -e $install ]] || mv -- "$install" "$old"
  if ! mv -- "$tmp" "$install"; then [[ ! -e $old ]] || mv -- "$old" "$install"; return 1; fi
  rm -rf -- "$old"
  : >"$stamp"
  trap - RETURN
  printf 'bootstrap: installed %s %s for %s\n' "$tool" "$version" "$target"
}

validate_members() {
  local listing=$1
  if printf '%s\n' "$listing" | awk '
    /^\// { bad=1 }
    { n=split($0, part, "/"); depth=0; for (i=1; i<=n; i++) { if (part[i]=="..") depth--; else if (part[i]!="" && part[i]!=".") depth++; if (depth<0) bad=1 } }
    END { exit !bad }
  '; then
    printf 'error: archive contains an escaping path\n' >&2; return 1
  fi
}

while IFS= read -r tool; do install_one "$tool"; done < <(locked_tools)
