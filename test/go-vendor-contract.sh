#!/usr/bin/env bash
set -euo pipefail

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd -P)
tmp=$(mktemp -d); trap 'rm -rf -- "$tmp"' EXIT

# A normal external module is deliberately used by go/test/greeting_test.go.
# Start with entirely empty Go caches and disable both module services: this
# succeeds only when the checked-in vendor tree is a complete closure.
grep -Fq 'github.com/google/go-cmp v0.7.0' "$ROOT/go.mod"
grep -Fq 'github.com/google/go-cmp v0.7.0' "$ROOT/go.sum"
grep -Fq '# github.com/google/go-cmp v0.7.0' "$ROOT/vendor/modules.txt"
grep -Fq 'github.com/google/go-cmp/cmp' "$ROOT/go/test/greeting_test.go"
grep -Fq 'GOFLAGS=-mod=vendor GOPROXY=off GOSUMDB=off' "$ROOT/rules/go.bzl"
grep -Fq '"_gosum": attrs.source(default = "//:go.sum")' "$ROOT/rules/go.bzl"
grep -Fq '"_vendor": attrs.dep(default = "//:go_vendor"' "$ROOT/rules/go.bzl"

"$ROOT/repo.sh" exec env \
  GOPATH="$tmp/gopath" \
  GOMODCACHE="$tmp/gomodcache" \
  GOCACHE="$tmp/gocache" \
  GOFLAGS=-mod=vendor \
  GOPROXY=off \
  GOSUMDB=off \
  go test -trimpath ./go/...

printf 'Go vendor contract: ok\n'
