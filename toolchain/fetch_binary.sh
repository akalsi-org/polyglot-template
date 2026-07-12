#!/usr/bin/env bash
set -euo pipefail

usage() { printf 'Usage: fetch_binary.sh URL SHA256 OUTPUT\n' >&2; exit 2; }
[[ $# == 3 ]] || usage
url=$1 expected=$2 output=$3
[[ $expected =~ ^[0-9a-f]{64}$ ]] || { printf 'error: unresolved/invalid SHA-256 for %s\n' "$url" >&2; exit 1; }

mkdir -p "$(dirname -- "$output")"
partial="$output.partial.$$"
trap 'rm -f -- "$partial"' EXIT
if command -v curl >/dev/null 2>&1; then
  curl --fail --location --retry 3 --connect-timeout 20 --output "$partial" "$url"
elif command -v wget >/dev/null 2>&1; then
  wget --tries=3 --timeout=20 --output-document="$partial" "$url"
else
  printf 'error: bootstrap requires curl or wget\n' >&2
  exit 1
fi
actual=$(sha256sum "$partial" | awk '{print $1}')
[[ $actual == "$expected" ]] || { printf 'error: checksum mismatch for %s\n' "$url" >&2; exit 1; }
mv -f -- "$partial" "$output"
trap - EXIT
