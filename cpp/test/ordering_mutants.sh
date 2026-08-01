#!/usr/bin/env bash
# Ordering-mutation campaign for the mpsc queue's memory-order table.
#
# WHY THIS EXISTS. Two mutations are provably uncatchable on x86-64: weakening
# the commit store or the vouch store from release to relaxed produces
# BYTE-IDENTICAL machine code there (x86-TSO makes every store a release, and
# GCC does not exploit relaxed's reordering license in this TU -- verified by
# objdump diff). No x86 test, no stress duration, and no TSan run can ever
# distinguish those mutants. On aarch64, release emits stlr and relaxed emits
# plain str -- genuinely different, genuinely reorderable by the CPU -- so a
# weakly-ordered machine is the only dynamic instrument that can tell whether
# the test suite guards the ordering table.
#
# RUN ON REAL AARCH64 HARDWARE (the repo's ubuntu-24.04-arm runner). qemu-user
# is NOT a substitute: TCG executes guest threads on host threads under the
# host's memory model, so an emulated aarch64 pass is silently as blind as
# x86. The script does not stop you running it on x86 -- there the expected
# result is NOT-CAUGHT for both mutants, which is itself the control.
#
# Weak-memory failures are probabilistic: each mutant's tests are repeated
# (default 20, override with ORDERING_MUTANT_REPS) and ANY failing repetition
# counts as CAUGHT. Baseline is built from the same snapshot and must pass
# first, so a broken tree or concurrent edit cannot produce a false CAUGHT --
# that exact contamination happened once on x86 and cost the campaign a false
# positive.
#
# Usage: cpp/test/ordering_mutants.sh   (from the repo root)
# Exit: 0 = ran to completion (see the report lines for CAUGHT/NOT-CAUGHT),
#       2 = campaign invalid (baseline failed or sed did not apply).
set -u

REPS="${ORDERING_MUTANT_REPS:-20}"
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
# Locate doctest by the header itself, never by a triple: this script's whole
# purpose is to run on aarch64, so hardcoding an x86 toolchain path would break
# it on the one machine it exists for. The include root is the directory holding
# doctest/doctest.h.
DT_HDR="$(find "$ROOT/.local/toolchain" -path '*/doctest/doctest.h' -print -quit 2>/dev/null)"
if [ -z "$DT_HDR" ]; then
  echo "INVALID: doctest headers not found under .local/toolchain -- run ./repo.sh bootstrap" >&2
  exit 2
fi
DT="$(dirname "$(dirname "$DT_HDR")")"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# Concurrency-heavy cases only: these are the ones whose wrap + contention can
# surface a reordered publish. Region/desc/restart tests cannot and just cost time.
FILTER="*differential*,*multi-writer*,*in-flight*"

build() {  # build <tree> <out>
  g++ -std=c++20 -I "$1" -I "$DT" -O2 -w -pthread \
    "$ROOT/cpp/test/mpsc_ring_test.cc" "$1/pgt/mpsc/region.cc" "$1/pgt/mpsc/policy.cc" \
    "$ROOT/cpp/test/doctest_runner.cc" -o "$2" 2> "$WORK/build.log"
}

run_mutant() {  # run_mutant <name> <sed-expr>
  local name=$1 expr=$2
  local D="$WORK/$name"
  rm -rf "$D" && mkdir -p "$D" && cp -r "$ROOT/cpp/lib/pgt" "$D/pgt"
  build "$D" "$D/base" || { echo "$name: INVALID (baseline build failed)"; return 2; }
  timeout 300 "$D/base" -tc="$FILTER" > "$D/base.log" 2>&1 \
    || { echo "$name: INVALID (baseline failed -- tree or box is bad)"; return 2; }
  sed -i "$expr" "$D/pgt/mpsc/mpsc_ring.hh"
  cmp -s "$D/pgt/mpsc/mpsc_ring.hh" "$ROOT/cpp/lib/pgt/mpsc/mpsc_ring.hh" \
    && { echo "$name: INVALID (mutation did not apply -- mpsc_ring.hh drifted?)"; return 2; }
  build "$D" "$D/mut" || { echo "$name: CAUGHT (mutant fails to build)"; return 0; }
  local i
  for i in $(seq 1 "$REPS"); do
    if ! timeout 300 "$D/mut" -tc="$FILTER" > "$D/mut.log" 2>&1; then
      echo "$name: CAUGHT on repetition $i of $REPS"
      return 0
    fi
  done
  echo "$name: NOT CAUGHT in $REPS repetitions"
}

echo "arch=$(uname -m) reps=$REPS"
rc=0
run_mutant commit-release-to-relaxed \
  's/std::memory_order_release);  \/\/ publishes the payload/std::memory_order_relaxed);/' || rc=2
run_mutant vouch-release-to-relaxed \
  's/std::memory_order_release);  \/\/ vouch (I2)/std::memory_order_relaxed);/' || rc=2
exit $rc
