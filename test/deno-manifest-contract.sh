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
source_entries = [
    path.relative_to(root).as_posix()
    for lane in (root / "ts", root / "tsweb")
    for path in lane.rglob("*")
    if path.suffix in {".ts", ".tsx"}
]
deno_config = json.loads((root / "deno.json").read_text())
expected = sorted(source_entries + [deno_config["imports"]["pyright"]])
assert entries == expected, ("manifest/source-or-tool mismatch", sorted(set(expected) ^ set(entries)))

package_rules = (root / "rules" / "package.bzl").read_text()
for name, specifier in deno_config["imports"].items():
    encoded = f'\\\"{name}\\\": \\\"{specifier}\\\"'
    assert encoded in package_rules, f"packaged Deno config omits locked workspace import: {name}"
assert '"  \\"nodeModulesDir\\": \\"manual\\",' in package_rules

rules = (root / "rules" / "deno.bzl").read_text()
graph_rule = rules[rules.index("def _deno_graph_check_impl"):rules.index("_deno_graph_check_rule = rule")]
assert r'run_offline \"$DENO\" info --json --frozen --no-remote \"$entry\"' in graph_rule
assert "if unshare -rn true 2>/dev/null; then" in graph_rule
assert r"unshare -rn sh -c 'ip link set lo up 2>/dev/null || true; exec \"$@\"'" in graph_rule
assert "deno graph resolution cannot be enforced" in graph_rule
assert r'\"$DENO\" info --json \"$entry\"' not in graph_rule
print(f"Deno graph manifest and offline graph policy: ok ({len(entries)} entries)")
PY
