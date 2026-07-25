#!/usr/bin/env bash
set -euo pipefail

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd -P)
python=${POLYGLOT_LOCAL_DIR:-$root/.local}/bin/python3
[[ -x $python ]] || { printf 'pinned Python is not installed; run ./repo.sh bootstrap\n' >&2; exit 1; }
compdb=$root/compile_commands.json
had_compdb=0
# The developer's compile_commands.json is repository state, so its backup
# stays beside it under buck-out rather than in /tmp: an EXIT trap does not
# fire on SIGKILL, and a recoverable copy next to the file is far easier to
# find than an anonymous /tmp name on a tmpfs that a reboot clears.
backup_dir=$root/buck-out/v2/tmp
if [[ -e $compdb ]]; then
  had_compdb=1
  mkdir -p "$backup_dir"
  backup=$backup_dir/compile_commands.json.contract-backup
  cp -- "$compdb" "$backup"
else
  backup=''
fi
cleanup() {
  if ((had_compdb)); then
    cp -- "$backup" "$compdb"
    rm -f -- "$backup"
  else
    rm -f -- "$compdb"
  fi
}
trap cleanup EXIT

# Exercise the BXL query and inspect its generated behavior, rather than
# trusting comments or the presence of a hand-written source list.
"$root/repo.sh" compile-commands
"$python" - "$root" "$compdb" <<'PY'
import json
import sys
from pathlib import Path

root = Path(sys.argv[1]).resolve()
entries = json.loads(Path(sys.argv[2]).read_text())
assert isinstance(entries, list) and entries, "compdb is empty"

files = set()
for entry in entries:
  assert entry["directory"] == str(root)
  assert isinstance(entry["arguments"], list) and entry["arguments"]
  assert "-c" in entry["arguments"] and "-o" in entry["arguments"]
  source = Path(entry["file"]).resolve()
  files.add(source.relative_to(root).as_posix())

expected = {
  "cpp/app/hello/main.cc",
  "cpp/lib/example/example.cc",
  "python/lib/fastbytes/fastbytes.cc",
  "python/test/pyfast_test_ext.c",
}
missing = expected - files
assert not missing, f"compdb is missing graph-backed compile actions: {sorted(missing)}"
print(f"graph/compdb behavior: ok ({len(entries)} compile actions)")
PY
