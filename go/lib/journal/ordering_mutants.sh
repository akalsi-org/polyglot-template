#!/usr/bin/env bash
set -eu

package_dir=$(cd "$(dirname "$0")" && pwd)
repo_root=$(cd "$package_dir/../../.." && pwd)
repetitions=${JOURNAL_ORDERING_MUTANTS_REPETITIONS:-20}
strict=${JOURNAL_ORDERING_MUTANTS_REQUIRE_CAUGHT:-0}
architecture=$(uname -m)
workspace=$(mktemp -d)
trap 'rm -rf "$workspace"' EXIT

mkdir -p \
  "$workspace/baseline/go/lib/journal" \
  "$workspace/baseline/go/lib/hostcpu" \
  "$workspace/baseline/go/lib/internal/orderedatomic" \
  "$workspace/baseline/go/lib/internal/shmregion"
cp "$repo_root/go.mod" "$workspace/baseline/go.mod"
cp -a "$repo_root/vendor" "$workspace/baseline/vendor"
cp "$package_dir"/*.go "$workspace/baseline/go/lib/journal/"
cp "$repo_root/go/lib/hostcpu"/*.go "$repo_root/go/lib/hostcpu"/*.s "$workspace/baseline/go/lib/hostcpu/"
cp "$repo_root/go/lib/internal/orderedatomic"/*.go "$repo_root/go/lib/internal/orderedatomic"/*.s "$workspace/baseline/go/lib/internal/orderedatomic/"
cp "$repo_root/go/lib/internal/shmregion"/*.go "$workspace/baseline/go/lib/internal/shmregion/"

run_once() {
  local root=$1
  (
    cd "$root" || exit 1
    timeout 90s "$repo_root/repo.sh" go test -count=1 \
      -run '^(Test(Cursor|Durable)SeqlockPublishesCompletePairs|TestJournal(RecordPublication|ReservationInvalidation|RecordRevalidation)Ordering)$' \
      ./go/lib/journal
  ) >/dev/null 2>&1
}

if ! run_once "$workspace/baseline"; then
  printf 'INVALID baseline failed\n'
  exit 1
fi
printf 'PASS baseline\n'

mutant="$workspace/cursor-store-barrier"
cp -a "$workspace/baseline" "$mutant"
python3 - "$mutant/go/lib/journal/ring_linux.go" <<'PY'
from pathlib import Path
import sys
path = Path(sys.argv[1])
text = path.read_text()
old = "\torderedatomic.StoreRelease64(lock, version+1)\n\torderedatomic.StoreBarrier()\n\torderedatomic.StoreRelaxed64(region.ptr64(sequenceOffset), uint64(cursor.sequence))"
new = "\torderedatomic.StoreRelease64(lock, version+1)\n\torderedatomic.StoreRelaxed64(region.ptr64(sequenceOffset), uint64(cursor.sequence))"
if text.count(old) != 1:
    raise SystemExit("expected one cursor barrier mutation point")
path.write_text(text.replace(old, new, 1))
PY

if ! (cd "$mutant" && "$repo_root/repo.sh" go test -run '^$' ./go/lib/journal >/dev/null 2>&1); then
  printf 'INVALID cursor-store-barrier did not compile\n'
  exit 1
fi

store_caught=0
for ((iteration=1; iteration<=repetitions; iteration++)); do
  if ! run_once "$mutant"; then
    store_caught=1
    printf 'CAUGHT cursor-store-barrier repetition=%d\n' "$iteration"
    break
  fi
done
if ((store_caught == 0)); then
  printf 'NOT-CAUGHT cursor-store-barrier repetitions=%d\n' "$repetitions"
fi

mutant="$workspace/cursor-load-barrier"
cp -a "$workspace/baseline" "$mutant"
python3 - "$mutant/go/lib/journal/ring_linux.go" <<'PY'
from pathlib import Path
import sys
path = Path(sys.argv[1])
text = path.read_text()
old = "\tnextPosition := orderedatomic.LoadRelaxed64((*uint64)(unsafe.Add(base, uintptr(positionOffset))))\n\torderedatomic.LoadBarrier()\n\tafter := orderedatomic.LoadAcquire64(lock)"
new = "\tnextPosition := orderedatomic.LoadRelaxed64((*uint64)(unsafe.Add(base, uintptr(positionOffset))))\n\tafter := orderedatomic.LoadAcquire64(lock)"
if text.count(old) != 1:
    raise SystemExit("expected one cursor load barrier mutation point")
path.write_text(text.replace(old, new, 1))
PY

if ! (cd "$mutant" && "$repo_root/repo.sh" go test -run '^$' ./go/lib/journal >/dev/null 2>&1); then
  printf 'INVALID cursor-load-barrier did not compile\n'
  exit 1
fi

load_caught=0
for ((iteration=1; iteration<=repetitions; iteration++)); do
  if ! run_once "$mutant"; then
    load_caught=1
    printf 'CAUGHT cursor-load-barrier repetition=%d\n' "$iteration"
    break
  fi
done
if ((load_caught == 0)); then
  printf 'NOT-CAUGHT cursor-load-barrier repetitions=%d\n' "$repetitions"
fi

mutant="$workspace/durable-store-barrier"
cp -a "$workspace/baseline" "$mutant"
python3 - "$mutant/go/lib/journal/ring_record_linux.go" <<'PY'
from pathlib import Path
import sys
path = Path(sys.argv[1])
text = path.read_text()
old = "\t}\n\torderedatomic.StoreBarrier()\n\torderedatomic.StoreRelaxed64(region.ptr64(JournalHeaderDurableSequenceOffset), uint64(proof.to.sequence))"
new = "\t}\n\torderedatomic.StoreRelaxed64(region.ptr64(JournalHeaderDurableSequenceOffset), uint64(proof.to.sequence))"
if text.count(old) != 1:
    raise SystemExit("expected one durable barrier mutation point")
path.write_text(text.replace(old, new, 1))
PY

if ! (cd "$mutant" && "$repo_root/repo.sh" go test -run '^$' ./go/lib/journal >/dev/null 2>&1); then
  printf 'INVALID durable-store-barrier did not compile\n'
  exit 1
fi

durable_caught=0
for ((iteration=1; iteration<=repetitions; iteration++)); do
  if ! run_once "$mutant"; then
    durable_caught=1
    printf 'CAUGHT durable-store-barrier repetition=%d\n' "$iteration"
    break
  fi
done
if ((durable_caught == 0)); then
  printf 'NOT-CAUGHT durable-store-barrier repetitions=%d\n' "$repetitions"
fi

mutant="$workspace/reservation-invalidation-barrier"
cp -a "$workspace/baseline" "$mutant"
python3 - "$mutant/go/lib/journal/producer_linux.go" <<'PY'
from pathlib import Path
import sys
path = Path(sys.argv[1])
text = path.read_text()
old = "\tproducer.region.fillTagsRelaxed(producer.position, grains, writingTag)\n\torderedatomic.StoreBarrier()\n\tproducer.region.fillDescriptorsRelaxed("
new = "\tproducer.region.fillTagsRelaxed(producer.position, grains, writingTag)\n\tproducer.region.fillDescriptorsRelaxed("
if text.count(old) != 1:
    raise SystemExit("expected one reservation invalidation barrier mutation point")
path.write_text(text.replace(old, new, 1))
PY

if ! (cd "$mutant" && "$repo_root/repo.sh" go test -run '^$' ./go/lib/journal >/dev/null 2>&1); then
  printf 'INVALID reservation-invalidation-barrier did not compile\n'
  exit 1
fi

invalidation_caught=0
for ((iteration=1; iteration<=repetitions; iteration++)); do
  if ! run_once "$mutant"; then
    invalidation_caught=1
    printf 'CAUGHT reservation-invalidation-barrier repetition=%d\n' "$iteration"
    break
  fi
done
if ((invalidation_caught == 0)); then
  printf 'NOT-CAUGHT reservation-invalidation-barrier repetitions=%d\n' "$repetitions"
fi

mutant="$workspace/start-tag-release"
cp -a "$workspace/baseline" "$mutant"
python3 - "$mutant/go/lib/journal/producer_linux.go" <<'PY'
from pathlib import Path
import sys
path = Path(sys.argv[1])
text = path.read_text()
old = "\torderedatomic.StoreRelease64(producer.region.tag(span.position), stableTag)"
new = "\torderedatomic.StoreRelaxed64(producer.region.tag(span.position), stableTag)"
if text.count(old) != 1:
    raise SystemExit("expected one start tag release mutation point")
path.write_text(text.replace(old, new, 1))
PY

if ! (cd "$mutant" && "$repo_root/repo.sh" go test -run '^$' ./go/lib/journal >/dev/null 2>&1); then
  printf 'INVALID start-tag-release did not compile\n'
  exit 1
fi

start_release_caught=0
for ((iteration=1; iteration<=repetitions; iteration++)); do
  if ! run_once "$mutant"; then
    start_release_caught=1
    printf 'CAUGHT start-tag-release repetition=%d\n' "$iteration"
    break
  fi
done
if ((start_release_caught == 0)); then
  printf 'NOT-CAUGHT start-tag-release repetitions=%d\n' "$repetitions"
fi

mutant="$workspace/record-revalidation-barrier"
cp -a "$workspace/baseline" "$mutant"
python3 - "$mutant/go/lib/journal/ring_record_linux.go" <<'PY'
from pathlib import Path
import sys
path = Path(sys.argv[1])
text = path.read_text()
old = "\t}\n\torderedatomic.LoadBarrier()\n\tif orderedatomic.LoadAcquire64(region.tag(cursor.position)) != expectedTag {"
new = "\t}\n\tif orderedatomic.LoadAcquire64(region.tag(cursor.position)) != expectedTag {"
if text.count(old) != 1:
    raise SystemExit("expected one record revalidation barrier mutation point")
path.write_text(text.replace(old, new, 1))
PY

if ! (cd "$mutant" && "$repo_root/repo.sh" go test -run '^$' ./go/lib/journal >/dev/null 2>&1); then
  printf 'INVALID record-revalidation-barrier did not compile\n'
  exit 1
fi

revalidation_caught=0
for ((iteration=1; iteration<=repetitions; iteration++)); do
  if ! run_once "$mutant"; then
    revalidation_caught=1
    printf 'CAUGHT record-revalidation-barrier repetition=%d\n' "$iteration"
    break
  fi
done
if ((revalidation_caught == 0)); then
  printf 'NOT-CAUGHT record-revalidation-barrier repetitions=%d\n' "$repetitions"
fi

if [[ "$strict" == 1 ]]; then
  case "$architecture" in
    aarch64|arm64) ;;
    *)
      printf 'INVALID strict mode requires native arm64; found %s\n' "$architecture"
      exit 1
      ;;
  esac
  if ((store_caught == 0 || load_caught == 0 || durable_caught == 0 ||
    invalidation_caught == 0 || start_release_caught == 0 || revalidation_caught == 0)); then
    exit 1
  fi
elif [[ "$architecture" != aarch64 && "$architecture" != arm64 ]]; then
  printf 'CONTROL %s does not adjudicate the weak-order mutant\n' "$architecture"
fi
