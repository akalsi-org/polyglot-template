#!/usr/bin/env bash
set -euo pipefail

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd -P)
manifest=$(cd "$root" && ./repo.sh buck2 build --show-output //:deno-cache[manifest] | awk 'NF { path = $NF } END { print path }')
[[ -f $manifest ]] || { printf 'Deno manifest was not materialized: %s\n' "$manifest" >&2; exit 1; }

python3 - "$root" "$manifest" <<'PY'
import json
import sys
from pathlib import Path

root = Path(sys.argv[1])
entries = json.loads(Path(sys.argv[2]).read_text())
assert isinstance(entries, list) and entries, "Deno manifest is empty"
assert entries == sorted(set(entries)), "Deno manifest is not stable and deduplicated"
expected = sorted(
    path.relative_to(root).as_posix()
    for lane in (root / "ts", root / "tsweb")
    for path in lane.rglob("*")
    if path.suffix in {".ts", ".tsx"}
)
assert entries == expected, ("manifest/source mismatch", sorted(set(expected) ^ set(entries)))
print(f"Deno graph manifest: ok ({len(entries)} entries)")
PY
