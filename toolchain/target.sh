#!/usr/bin/env bash
set -euo pipefail

os=${POLYGLOT_TEST_OS:-$(uname -s)}
[[ $os == Linux ]] || { printf 'error: unsupported host operating system: %s\n' "$os" >&2; exit 1; }

machine=${POLYGLOT_TEST_MACHINE:-$(uname -m)}
case "$machine" in
  x86_64|amd64) printf '%s\n' x86_64-linux-musl ;;
  aarch64|arm64) printf '%s\n' aarch64-linux-musl ;;
  *) printf 'error: unsupported native CPU architecture: %s\n' "$machine" >&2; exit 1 ;;
esac
