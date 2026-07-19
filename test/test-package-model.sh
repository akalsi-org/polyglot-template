#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
tool="$root/tools/package_model.py"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

catalog="$root/packages/catalog.bzl"
python3 "$tool" --catalog "$catalog" --tools-lock "$root/tools.lock.toml" validate
package_list=$("$root/repo.sh" package-list)
grep -Fq $'NAME\tVERSION\tKIND\tTARGETS\tEXECUTABLES' <<<"$package_list"
grep -Fq $'polyglot-demo\t0.1.0\tapplication' <<<"$package_list"
grep -Fq $'runtime-python\t3.14.6\truntime' <<<"$package_list"
sed -e '0,/sha256 = "UNRESOLVED"/s//sha256 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"/' \
    -e '0,/loader = "UNRESOLVED:[^"]*"/s//loader = "lib\/ld-musl.so.1"/' \
    "$root/tools.lock.toml" >"$tmp/resolved-tools.lock.toml"
resolved_tools="$tmp/resolved-tools.lock.toml"
python3 "$tool" --catalog "$catalog" --tools-lock "$resolved_tools" resolve \
  --package polyglot-demo --target x86_64-linux-musl --out-dir "$tmp/out" >"$tmp/first"
python3 "$tool" --catalog "$catalog" --tools-lock "$resolved_tools" resolve \
  --package polyglot-demo --target x86_64-linux-musl --out-dir "$tmp/out2" >"$tmp/second"
cmp "$tmp/first" "$tmp/second"
cmp "$tmp/out/closure.json" "$tmp/out2/closure.json"
grep -q '"runtime_package": "runtime-python"' "$tmp/out/runtime-ref.json"

sed '0,/loader_sha256.*a6a1ec/s//loader_sha256": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"/' \
  "$catalog" >"$tmp/mismatched-loader.bzl"
if python3 "$tool" --catalog "$tmp/mismatched-loader.bzl" \
  --tools-lock "$resolved_tools" resolve --package polyglot-demo --target x86_64-linux-musl >/dev/null 2>&1; then
  echo "mismatched loader identity unexpectedly resolved" >&2
  exit 1
fi

if python3 "$tool" --catalog "$catalog" --tools-lock "$resolved_tools" resolve \
  --package polyglot-demo --target riscv64-linux-musl >/dev/null 2>&1; then
  echo "off-target resolution unexpectedly succeeded" >&2
  exit 1
fi

sed 's/"version": "\^3.14"/"version": "latest"/' "$catalog" >"$tmp/floating.bzl"
if python3 "$tool" --catalog "$tmp/floating.bzl" --tools-lock "$resolved_tools" validate >/dev/null 2>&1; then
  echo "floating runtime unexpectedly validated" >&2
  exit 1
fi

grep -v '"package": "polyglot-demo", "target": "x86_64-linux-musl"' "$catalog" >"$tmp/unresolved.bzl"
if python3 "$tool" --catalog "$tmp/unresolved.bzl" --tools-lock "$resolved_tools" resolve \
  --package polyglot-demo --target x86_64-linux-musl >/dev/null 2>&1; then
  echo "unresolved runtime unexpectedly succeeded" >&2
  exit 1
fi

grep -q '"smoke_args": {"server": \["--smoke"\]}' "$catalog"
sed 's/"smoke_args": {"server": \["--smoke"\]}/"smoke_args": {"nonexistent": ["--smoke"]}/' "$catalog" >"$tmp/bad-smoke-args-key.bzl"
if python3 "$tool" --catalog "$tmp/bad-smoke-args-key.bzl" --tools-lock "$resolved_tools" validate >/dev/null 2>&1; then
  echo "smoke_args referencing a nonexistent executable unexpectedly validated" >&2
  exit 1
fi

sed 's/"smoke_args": {"server": \["--smoke"\]}/"smoke_args": {"server": "--smoke"}/' "$catalog" >"$tmp/bad-smoke-args-shape.bzl"
if python3 "$tool" --catalog "$tmp/bad-smoke-args-shape.bzl" --tools-lock "$resolved_tools" validate >/dev/null 2>&1; then
  echo "smoke_args with a non-array value unexpectedly validated" >&2
  exit 1
fi

echo "package model tests passed"
