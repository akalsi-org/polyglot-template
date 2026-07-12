#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
tool="$root/tools/package_model.py"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

python3 "$tool" --manifest "$root/package.toml" --lock "$root/runtime-resolution.lock.toml" --tools-lock "$root/tools.lock.toml" validate
sed -e 's/loader_sha256 = "UNRESOLVED"/loader_sha256 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"/g' \
    -e 's|loader_path = "UNRESOLVED:[^"]*"|loader_path = "lib/ld-musl.so.1"|g' \
    "$root/runtime-resolution.lock.toml" >"$tmp/resolved-runtime.lock.toml"
sed -e '0,/sha256 = "UNRESOLVED"/s//sha256 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"/' \
    -e '0,/loader = "UNRESOLVED:[^"]*"/s//loader = "lib\/ld-musl.so.1"/' \
    "$root/tools.lock.toml" >"$tmp/resolved-tools.lock.toml"
resolved_lock="$tmp/resolved-runtime.lock.toml"
resolved_tools="$tmp/resolved-tools.lock.toml"
python3 "$tool" --manifest "$root/package.toml" --lock "$resolved_lock" --tools-lock "$resolved_tools" resolve \
  --package polyglot-demo --target x86_64-linux-musl --out-dir "$tmp/out" >"$tmp/first"
python3 "$tool" --manifest "$root/package.toml" --lock "$resolved_lock" --tools-lock "$resolved_tools" resolve \
  --package polyglot-demo --target x86_64-linux-musl --out-dir "$tmp/out2" >"$tmp/second"
cmp "$tmp/first" "$tmp/second"
cmp "$tmp/out/closure.json" "$tmp/out2/closure.json"
grep -q '"runtime_package": "runtime-python"' "$tmp/out/runtime-ref.json"

sed '0,/loader_sha256 = "[a-f0-9]*"/s//loader_sha256 = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"/' \
  "$resolved_lock" >"$tmp/mismatched-loader.lock.toml"
if python3 "$tool" --manifest "$root/package.toml" --lock "$tmp/mismatched-loader.lock.toml" \
  --tools-lock "$resolved_tools" resolve --package polyglot-demo --target x86_64-linux-musl >/dev/null 2>&1; then
  echo "mismatched loader identity unexpectedly resolved" >&2
  exit 1
fi

if python3 "$tool" --manifest "$root/package.toml" --lock "$resolved_lock" --tools-lock "$resolved_tools" resolve \
  --package polyglot-demo --target riscv64-linux-musl >/dev/null 2>&1; then
  echo "off-target resolution unexpectedly succeeded" >&2
  exit 1
fi

sed 's/version = "\^3.14"/version = "latest"/' "$root/package.toml" >"$tmp/floating.toml"
if python3 "$tool" --manifest "$tmp/floating.toml" --lock "$resolved_lock" --tools-lock "$resolved_tools" validate >/dev/null 2>&1; then
  echo "floating runtime unexpectedly validated" >&2
  exit 1
fi

sed '/package = "polyglot-demo"/,/sha256 = /d' "$root/runtime-resolution.lock.toml" >"$tmp/unresolved.toml"
if python3 "$tool" --manifest "$root/package.toml" --lock "$tmp/unresolved.toml" --tools-lock "$resolved_tools" resolve \
  --package polyglot-demo --target x86_64-linux-musl >/dev/null 2>&1; then
  echo "unresolved runtime unexpectedly succeeded" >&2
  exit 1
fi

echo "package model tests passed"
