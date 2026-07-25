#!/usr/bin/env bash
set -euo pipefail

usage() { printf 'Usage: fetch_binary.sh URL SHA256 OUTPUT\n' >&2; exit 2; }
[[ $# == 3 ]] || usage
url=$1 expected=$2 output=$3
[[ $expected =~ ^[0-9a-f]{64}$ ]] || { printf 'error: unresolved/invalid SHA-256 for %s\n' "$url" >&2; exit 1; }

# Scheme allowlist. --location alone would happily follow an HTTPS -> HTTP
# redirect, downgrading a locked artifact fetch to plaintext; --proto/
# --proto-redir pin the transport for the initial request AND every redirect.
# file:// stays reachable only for test/bootstrap-smoke.sh's synthetic
# fixture, behind an explicit opt-in that production bootstraps never set.
allow_file=${POLYGLOT_ALLOW_FILE_URL:-0}
curl_proto=(--proto '=https' --proto-redir '=https')
case "$url" in
  https://*) ;;
  file://*)
    [[ $allow_file == 1 ]] || { printf 'error: file:// artifact URLs require POLYGLOT_ALLOW_FILE_URL=1 (test fixtures only): %s\n' "$url" >&2; exit 1; }
    curl_proto=(--proto '=file')
    ;;
  *) printf 'error: unsupported artifact URL scheme (https only): %s\n' "$url" >&2; exit 1 ;;
esac

mkdir -p "$(dirname -- "$output")"
partial="$output.partial.$$"
trap 'rm -f -- "$partial"' EXIT
if command -v curl >/dev/null 2>&1; then
  # --max-time bounds the whole transfer; --connect-timeout only bounds the
  # handshake, so a stalled body could hang bootstrap indefinitely.
  curl --fail --location "${curl_proto[@]}" --retry 3 --connect-timeout 20 \
    --max-time "${POLYGLOT_FETCH_MAX_TIME:-900}" --output "$partial" -- "$url"
elif command -v wget >/dev/null 2>&1; then
  wget --tries=3 --timeout=20 --output-document="$partial" -- "$url"
else
  printf 'error: bootstrap requires curl or wget\n' >&2
  exit 1
fi
actual=$(sha256sum "$partial" | awk '{print $1}')
[[ $actual == "$expected" ]] || { printf 'error: checksum mismatch for %s\n' "$url" >&2; exit 1; }
mv -f -- "$partial" "$output"
trap - EXIT
