#!/usr/bin/env bash
set -u

package_dir=$(cd "$(dirname "$0")" && pwd)
repo_root=$(cd "$package_dir/../../.." && pwd)
repetitions=${ORDERING_MUTANTS_REPETITIONS:-20}
records=${PGT_MPSC_ORDERING_RECORDS:-20000}
strict=${ORDERING_MUTANTS_REQUIRE_CAUGHT:-0}
architecture=$(uname -m)
test_pattern='TestReleaseAcquirePublishesPayload$|TestMPSCReleaseAcquirePublication$'
workspace=$(mktemp -d)
trap 'rm -rf "$workspace"' EXIT

mkdir -p \
  "$workspace/baseline/go/lib/mpsc" \
  "$workspace/baseline/go/lib/hostcpu" \
  "$workspace/baseline/go/lib/internal/orderedatomic" \
  "$workspace/baseline/go/lib/internal/shmregion"
printf 'module github.com/akalsi-org/polyglot-template\n\ngo 1.27\n' >"$workspace/baseline/go.mod"
cp "$package_dir"/*.go "$workspace/baseline/go/lib/mpsc/"
cp "$repo_root/go/lib/hostcpu"/*.go "$repo_root/go/lib/hostcpu"/*.s "$workspace/baseline/go/lib/hostcpu/"
cp "$repo_root/go/lib/internal/orderedatomic"/*.go "$repo_root/go/lib/internal/orderedatomic"/*.s "$workspace/baseline/go/lib/internal/orderedatomic/"
cp "$repo_root/go/lib/internal/shmregion"/*.go "$workspace/baseline/go/lib/internal/shmregion/"

run_once() {
  local root=$1
  (
    cd "$root" || exit 1
    PGT_MPSC_ORDERING_RECORDS="$records" timeout 90s "$repo_root/repo.sh" go test -count=1 -run "$test_pattern" ./go/lib/mpsc
  ) >/dev/null 2>&1
}

if ! run_once "$workspace/baseline"; then
  printf 'INVALID baseline failed\n'
  exit 1
fi
printf 'PASS baseline\n'

mutate_once() {
  local file=$1
  local old=$2
  local new=$3
  python3 - "$file" "$old" "$new" <<'PY'
from pathlib import Path
import sys
path = Path(sys.argv[1])
old = sys.argv[2]
new = sys.argv[3]
text = path.read_text()
count = text.count(old)
if count != 1:
    raise SystemExit(f"expected one mutation point, found {count}")
path.write_text(text.replace(old, new, 1))
PY
}

invalid=0
survived=0
run_mutant() {
  local name=$1
  local file=$2
  local old=$3
  local new=$4
  local root="$workspace/$name"
  cp -a "$workspace/baseline" "$root"
  if ! mutate_once "$root/$file" "$old" "$new"; then
    printf 'INVALID %s mutation did not apply\n' "$name"
    invalid=1
    return
  fi
  if ! (cd "$root" && "$repo_root/repo.sh" go test -run '^$' ./go/lib/mpsc >/dev/null 2>&1); then
    printf 'INVALID %s did not compile\n' "$name"
    invalid=1
    return
  fi
  local caught=0
  local iteration
  for ((iteration=1; iteration<=repetitions; iteration++)); do
    if ! run_once "$root"; then
      caught=1
      break
    fi
  done
  if ((caught)); then
    printf 'CAUGHT %s repetition=%d\n' "$name" "$iteration"
  else
    printf 'NOT-CAUGHT %s repetitions=%d\n' "$name" "$repetitions"
    survived=1
  fi
}

run_mutant \
  arm64-release-store \
  go/lib/internal/orderedatomic/atomic_arm64.s \
  $'TEXT ·StoreRelease64(SB), NOSPLIT, $0-16\n\tMOVD ptr+0(FP), R0\n\tMOVD value+8(FP), R1\n\tSTLR R1, (R0)' \
  $'TEXT ·StoreRelease64(SB), NOSPLIT, $0-16\n\tMOVD ptr+0(FP), R0\n\tMOVD value+8(FP), R1\n\tMOVD R1, (R0)'

run_mutant \
  arm64-acquire-load \
  go/lib/internal/orderedatomic/atomic_arm64.s \
  $'TEXT ·LoadAcquire64(SB), NOSPLIT, $0-16\n\tMOVD ptr+0(FP), R0\n\tLDAR (R0), R1' \
  $'TEXT ·LoadAcquire64(SB), NOSPLIT, $0-16\n\tMOVD ptr+0(FP), R0\n\tMOVD (R0), R1'

run_mutant \
  commit-tag-release \
  go/lib/mpsc/queue_linux.go \
  $'\tstoreRelaxed64(length, uint64(actual))\n\tstoreRelease64(tag, commitTag(w.pos))' \
  $'\tstoreRelaxed64(length, uint64(actual))\n\tstoreRelaxed64(tag, commitTag(w.pos))'

run_mutant \
  peek-tag-acquire \
  go/lib/mpsc/queue_linux.go \
  $'\t\tif loadAcquire64(tag) == commitTag(r.rd) {\n\t\t\tn := loadRelaxed64(length)' \
  $'\t\tif loadRelaxed64(tag) == commitTag(r.rd) {\n\t\t\tn := loadRelaxed64(length)'

run_mutant \
  recovery-tag-acquire \
  go/lib/mpsc/queue_linux.go \
  $'\t\tif loadAcquire64(tag) == commitTag(r.rd) {\n\t\t\tcontinue' \
  $'\t\tif loadRelaxed64(tag) == commitTag(r.rd) {\n\t\t\tcontinue'

if ((invalid)); then
  exit 1
fi
if [[ "$strict" == 1 ]]; then
  case "$architecture" in
    aarch64|arm64) ;;
    *)
      printf 'INVALID strict mode requires native arm64; found %s\n' "$architecture"
      exit 1
      ;;
  esac
  if ((survived)); then
    exit 1
  fi
elif [[ "$architecture" != aarch64 && "$architecture" != arm64 ]]; then
  printf 'CONTROL %s does not adjudicate weak-order mutants\n' "$architecture"
fi
