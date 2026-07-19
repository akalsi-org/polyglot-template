#!/usr/bin/env bash
set -euo pipefail

ROOT=${POLYGLOT_ROOT:-$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd -P)}
LOCAL=${POLYGLOT_LOCAL_DIR:-$ROOT/.local}
POLYGLOT_LOCK_FILE=${POLYGLOT_LOCK_FILE:-$ROOT/tools.lock.toml}
export POLYGLOT_LOCK_FILE
. "$ROOT/toolchain/lock.sh"
. "$ROOT/toolchain/wrappers.sh"

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

extract_archive() {
  local archive=$1 cache=$2 destination=$3 expected=$4 listing
  case "$archive" in
    *.tar.gz|*.tgz) listing=$(tar -tzf "$cache"); validate_members "$listing"; tar -xzf "$cache" -C "$destination" --no-same-owner --no-same-permissions ;;
    *.tar.xz) listing=$(tar -tJf "$cache"); validate_members "$listing"; tar -xJf "$cache" -C "$destination" --no-same-owner --no-same-permissions ;;
    *.tar.zst) listing=$(tar --zstd -tf "$cache"); validate_members "$listing"; tar --zstd -xf "$cache" -C "$destination" --no-same-owner --no-same-permissions ;;
    *.zip) command -v unzip >/dev/null; listing=$(unzip -Z1 "$cache"); validate_members "$listing"; unzip -q "$cache" -d "$destination" ;;
    *.zst)
      command -v zstd >/dev/null || { printf 'error: bootstrap requires zstd for %s\n' "$archive" >&2; return 1; }
      zstd -d -q -f -o "$destination/$expected" "$cache"
      chmod +x "$destination/$expected"
      ;;
    *) printf 'error: unsupported archive: %s\n' "$archive" >&2; return 1 ;;
  esac
}

validate_expected_artifact() {
  local tool=$1 kind=$2 root=$3 expected=$4
  case "$kind" in
    header) [[ -f $root/$expected ]] || { printf 'error: %s missing expected header %s\n' "$tool" "$expected" >&2; return 1; } ;;
    executable) [[ -x $root/$expected ]] || { printf 'error: %s missing expected executable %s\n' "$tool" "$expected" >&2; return 1; } ;;
    *) printf 'error: unsupported artifact kind for %s: %s\n' "$tool" "$kind" >&2; return 1 ;;
  esac
}

normalize_gcc_loader() {
  local root=$1 declared_loader loader_target
  declared_loader=$(lock_value gcc-musl "$target" loader)
  [[ -L $root/$declared_loader ]] || { printf 'error: gcc-musl declared loader is missing\n' >&2; return 1; }
  loader_target=$(readlink -- "$root/$declared_loader")
  [[ $loader_target != /lib/libc.so ]] || {
    rm -- "$root/$declared_loader"
    ln -s libc.so "$root/$declared_loader"
  }
}

probe_python() {
  local root=$1 expected=$2 gcc_version loader gcc_install loader_dir
  gcc_version=$(lock_value gcc-musl "$target" version)
  loader=$(lock_value gcc-musl "$target" loader)
  gcc_install="$LOCAL/toolchain/$target/gcc-musl-$gcc_version"
  [[ $loader != UNRESOLVED* && -x $gcc_install/$loader ]] || {
    printf 'error: Python probe requires resolved musl loader from gcc-musl\n' >&2; return 1;
  }
  loader_dir=$(dirname -- "$gcc_install/$loader")
  "$gcc_install/$loader" --library-path "$loader_dir:$root/python/lib" "$root/$expected" -I -c \
    'import ctypes, hashlib, sqlite3, ssl, sys, zlib; print(sys.version)' >/dev/null || {
    printf 'error: Python musl-loader capability probe failed\n' >&2; return 1;
  }
}

probe_gcc() {
  local root=$1 expected=$2 mold dumpmachine binutil binutil_rel
  mold=$(lock_value gcc-musl "$target" mold)
  [[ -x $root/$mold ]] || { printf 'error: gcc-musl missing declared mold executable\n' >&2; return 1; }
  for binutil in ar ranlib nm strip objcopy ld; do
    binutil_rel=$(lock_value gcc-musl "$target" "$binutil")
    [[ -x $root/$binutil_rel ]] || { printf 'error: gcc-musl missing declared %s executable\n' "$binutil" >&2; return 1; }
  done
  dumpmachine=$("$root/$expected" -dumpmachine)
  [[ $dumpmachine == "$target" ]] || { printf 'error: compiler target mismatch: %s\n' "$dumpmachine" >&2; return 1; }
  "$root/$mold" --version | grep -q '^mold 2\.41\.0' || { printf 'error: mold capability probe failed\n' >&2; return 1; }
  "$root/$expected" -std=gnu++26 -freflection -fsyntax-only "$ROOT/cpp/test/reflection.cc" || {
    printf 'error: GCC C++26 reflection capability probe failed\n' >&2; return 1;
  }
}

probe_go() {
  local root=$1 expected=$2 version=$3
  "$root/$expected" version | grep -q "go$version" || { printf 'error: Go capability probe failed\n' >&2; return 1; }
}

probe_executable() {
  local tool=$1 root=$2 expected=$3
  "$root/$expected" --version >/dev/null 2>&1 || { printf 'error: %s capability probe failed\n' "$tool" >&2; return 1; }
}

probe_buck2() {
  local root=$1 expected=$2 declared_hash reported
  "$root/$expected" --version >/dev/null 2>&1 || { printf 'error: buck2 capability probe failed\n' >&2; return 1; }
  declared_hash=$(lock_value buck2 "$target" content_hash)
  [[ -z $declared_hash ]] && return 0
  reported=$("$root/$expected" --version | awk '{ print $2 }')
  [[ $reported == "$declared_hash" ]] || {
    printf 'error: buck2 reported content hash %s does not match pinned %s\n' "$reported" "$declared_hash" >&2
    return 1
  }
}

probe_doctest() {
  local root=$1 expected=$2
  grep -q '^#define DOCTEST_VERSION_MAJOR 2$' "$root/$expected" || {
    printf 'error: doctest header capability probe failed\n' >&2; return 1;
  }
}

probe_artifact() {
  local tool=$1 kind=$2 root=$3 expected=$4 version=$5
  case "$tool:$kind" in
    python:*) probe_python "$root" "$expected" ;;
    gcc-musl:*) probe_gcc "$root" "$expected" ;;
    go:*) probe_go "$root" "$expected" "$version" ;;
    doctest:*) probe_doctest "$root" "$expected" ;;
    buck2:*) probe_buck2 "$root" "$expected" ;;
    *:executable) probe_executable "$tool" "$root" "$expected" ;;
  esac
}

tool_install_path() {
  local tool=$1 version expected
  version=$(lock_value "$tool" "$target" version)
  expected=$(lock_value "$tool" "$target" expected)
  [[ -n $version && -n $expected ]] || return 1
  printf '%s/toolchain/%s/%s-%s/%s\n' "$LOCAL" "$target" "$tool" "$version" "$expected"
}

write_bootstrap_wrappers() {
  local cxx cc python deno go buck2 loader gcc_version loader_path gcc_install
  cxx=$(tool_install_path gcc-musl) || return 0
  cc="${cxx%g++}gcc"
  python=$(tool_install_path python) || return 0
  deno=$(tool_install_path deno) || return 0
  go=$(tool_install_path go) || return 0
  buck2=$(tool_install_path buck2) || return 0
  gcc_version=$(lock_value gcc-musl "$target" version)
  loader=$(lock_value gcc-musl "$target" loader)
  gcc_install="$LOCAL/toolchain/$target/gcc-musl-$gcc_version"
  loader_path="$gcc_install/$loader"
  for tool in "$cc" "$cxx" "$python" "$deno" "$go" "$buck2" "$loader_path"; do
    [[ -x $tool ]] || return 0
  done
  write_repo_tool_wrappers "$LOCAL" "$cc" "$cxx" "$python" "$loader_path" "$deno" "$go" "$buck2" "$gcc_install" "$target"
  printf 'bootstrap: wrote self-contained tool wrappers\n'
}

install_one() {
  local tool=$1 version url sha archive expected kind install stamp cache tmp old link resolved
  version=$(lock_value "$tool" "$target" version)
  url=$(lock_value "$tool" "$target" url)
  sha=$(lock_value "$tool" "$target" sha256)
  archive=$(lock_value "$tool" "$target" archive)
  expected=$(lock_value "$tool" "$target" expected)
  kind=$(lock_value "$tool" "$target" kind)
  kind=${kind:-executable}
  [[ -n $version && -n $archive && -n $expected ]] || {
    printf 'error: no complete %s artifact for %s\n' "$tool" "$target" >&2; return 1;
  }
  install="$LOCAL/toolchain/$target/$tool-$version"
  stamp="$install/.installed-$sha"
  cache="$LOCAL/downloads/$sha-$archive"
  if [[ -f $stamp && (($kind == header && -f $install/$expected) || ($kind == executable && -x $install/$expected)) ]]; then
    [[ $tool != go ]] || chmod -R u+w -- "$install"
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
  extract_archive "$archive" "$cache" "$tmp" "$expected"
  [[ $tool != go ]] || chmod -R u+w -- "$tmp"
  [[ $tool != gcc-musl ]] || normalize_gcc_loader "$tmp"
  while IFS= read -r -d '' link; do
    resolved=$(realpath -m -- "$link")
    case "$resolved" in "$tmp"/*) :;; *) printf 'error: archive contains escaping symlink: %s\n' "$link" >&2; return 1;; esac
  done < <(find "$tmp" -type l -print0)
  validate_expected_artifact "$tool" "$kind" "$tmp" "$expected"
  probe_artifact "$tool" "$kind" "$tmp" "$expected" "$version"
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

if ((dry_run == 0)); then
  write_bootstrap_wrappers

  deno_version=$(lock_value deno "$target" version)
  deno_expected=$(lock_value deno "$target" expected)
  deno_install="$LOCAL/toolchain/$target/deno-$deno_version"
  if [[ -n $deno_version && -x $deno_install/$deno_expected ]]; then
    if ((offline == 0)); then
      # Entry list mirrors //:deno-cache's (root BUCK) - this seed is what
      # the in-graph deno_cache action later resolves from with
      # --cached-only, so an entry missing here surfaces as that action's
      # fail-closed "run ./repo.sh bootstrap" error, never a network fetch.
      DENO_DIR="$LOCAL/cache/deno" "$deno_install/$deno_expected" cache --frozen \
        "$ROOT/ts/app/hello/main.ts" \
        "$ROOT/ts/app/server/main.ts" \
        "$ROOT/ts/test/greeting_test.ts" \
        "$ROOT/tsweb/app/site/main.tsx" \
        "$ROOT/tsweb/test/app_test.tsx" \
        "$ROOT/tsweb/test/title_test.ts" \
        "$ROOT/tsweb/vite.config.ts"
      printf 'bootstrap: cached locked Deno dependency graph\n'
    else
      printf 'bootstrap: preserved cached locked Deno dependency graph\n'
    fi
  fi
fi
