#!/usr/bin/env bash
set -euo pipefail

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd -P)
python=${POLYGLOT_LOCAL_DIR:-$root/.local}/bin/python3
[[ -x $python ]] || { printf 'pinned Python is not installed; run ./repo.sh bootstrap\n' >&2; exit 1; }
# Quote the subtarget: bash reads an unquoted [manifest] as a glob character
# class, so it survives only while no single-character file named d/e/f/i/m/n/
# s/t happens to sit in the working directory.
manifest=$(cd "$root" && ./repo.sh buck2 build --show-output '//:deno-cache[manifest]' | awk 'NF { path = $NF } END { print path }')
[[ -f $manifest ]] || { printf 'Deno manifest was not materialized: %s\n' "$manifest" >&2; exit 1; }

"$python" - "$root" "$manifest" <<'PY'
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
# The packaged ts/deno.json is DERIVED from the root deno.json, never restated.
# This used to assert the opposite - that package.bzl contained a hand-written
# copy of every import - which is precisely the drift this repo's P2 ("one
# truth per artifact class") forbids: bumping a version in deno.json left the
# packaged map stale, and the launcher's `--frozen` only caught it at runtime,
# inside a shipped archive. So the contract is now inverted: prove that no
# specifier is hardcoded, and that the generator reads the root config.
for name, specifier in deno_config["imports"].items():
    restated = f'\\\"{name}\\\": \\\"{specifier}\\\"'
    assert restated not in package_rules, (
        f"packaged Deno config restates a locked import instead of deriving it: {name}"
    )
    assert specifier not in package_rules, (
        f"packaged Deno config hardcodes the specifier for {name}; derive it from //:deno.json"
    )
assert '"_deno_json": attrs.source(default = "//:deno.json")' in package_rules, (
    "package() no longer sources the root deno.json; the packaged ts/deno.json would drift"
)
assert r'root.get(\"imports\", {})' in package_rules, (
    "the packaged ts/deno.json generator does not copy the root config's imports verbatim"
)
assert r'{\"nodeModulesDir\": \"manual\"' in package_rules

rules = (root / "rules" / "deno.bzl").read_text()

# The no-network namespace policy lives in ONE shared helper. It used to be
# copy-pasted into deno_cache and deno_graph_check only, which left the other
# four deno rules - including vite_build, which runs vite with `-A` (all
# permissions, --allow-net included) - relying on `--cached-only` alone.
# --cached-only constrains module RESOLUTION, not what the program does, so
# the module docstring's "none of them touch the network" held for 2 of 6.
helper = rules[rules.index("def _run_offline_lines"):rules.index("def _resolve_entries")]
assert "if unshare -rn true 2>/dev/null; then" in helper, "run_offline lost its userns guard"
assert r"unshare -rn sh -c 'ip link set lo up 2>/dev/null || true; exec \"$@\"'" in helper
assert '"  exit 1",' in helper, "run_offline must FAIL CLOSED when userns is unavailable"

# Every deno invocation site goes through it - this is the guarantee the
# refactor bought, so assert the count rather than a single call site.
offline_sites = rules.count("_run_offline_lines(")
assert offline_sites >= 4, f"expected every deno lane wrapped in run_offline, found {offline_sites}"

graph_rule = rules[rules.index("def _deno_graph_check_impl"):rules.index("_deno_graph_check_rule = rule")]
assert r'run_offline \"$DENO\" info --json --frozen --no-remote \"$entry\"' in graph_rule
assert '_run_offline_lines("deno graph resolution")' in graph_rule
assert r'\"$DENO\" info --json \"$entry\"' not in graph_rule

# The generic staged runner (deno_check/deno_test/deno_lint/deno_fmt/
# vite_build all route through it) must be wrapped too.
staged = rules[rules.index("def _stage_and_run"):rules.index("def _deno_graph_check_impl")]
assert 'run_offline \\"$DENO\\" \\"$@\\"' in staged, "_stage_and_run does not run deno offline"
print(f"Deno graph manifest and offline graph policy: ok ({len(entries)} entries)")
PY
