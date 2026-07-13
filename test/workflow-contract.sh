#!/usr/bin/env bash
set -euo pipefail

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd -P)
workflow=$root/.github/workflows/ci-release.yml

[[ $(grep -c 'runner: ubuntu-24.04$' "$workflow") == 1 ]]
[[ $(grep -c 'runner: ubuntu-24.04-arm$' "$workflow") == 1 ]]
grep -Fq 'actions/cache@5a3ec84eff668545956fd18022155c47e93e2684' "$workflow"
grep -Fq 'actions/upload-artifact@ea165f8d65b6e75b540449e92b4886f43607fa02' "$workflow"
grep -Fq 'actions/download-artifact@d3f86a106a0bac45b974a628896c90dbdf5c8093' "$workflow"
grep -Fq 'polyglot-tools-v1-${{ runner.os }}-${{ matrix.target }}-' "$workflow"
grep -Fq "hashFiles('tools.lock.toml', 'toolchain/**')" "$workflow"
grep -Fq 'polyglot-deno-v1-${{ runner.os }}-${{ matrix.target }}-' "$workflow"
grep -Fq "hashFiles('tools.lock.toml', 'deno.lock')" "$workflow"
grep -Fq 'polyglot-moon-v1-${{ runner.os }}-${{ matrix.target }}-' "$workflow"
grep -Fq "hashFiles('tools.lock.toml', 'moon.yml', '.moon/**')" "$workflow"
grep -Fq '            node_modules' "$workflow"
! grep -q 'restore-keys:' "$workflow"
grep -Fq 'tags: ["packages/*/v*"]' "$workflow"
grep -Fq 'gh release create "$GITHUB_REF_NAME" artifacts/*' "$workflow"

bootstrap_line=$(grep -n './repo.sh bootstrap$' "$workflow" | cut -d: -f1)
offline_line=$(grep -n './repo.sh bootstrap --offline$' "$workflow" | cut -d: -f1)
doctor_line=$(grep -n './repo.sh doctor --deep$' "$workflow" | cut -d: -f1)
((bootstrap_line < offline_line && offline_line < doctor_line))

for command in \
  './repo.sh lint' './repo.sh package-validate' './repo.sh build' './repo.sh test' \
  './repo.sh cpp-build dbg' './repo.sh cpp-run dbg' \
  './repo.sh cpp-build opt' './repo.sh cpp-run opt' \
  './repo.sh python -I -c'; do
  grep -Fq "$command" "$workflow"
done

help=$($root/repo.sh help)
for command in build test cpp-test python-test ts-test go-test tsweb-test; do
  grep -Eq "^  ${command}( |$)" <<<"$help"
done
for removed in python-check ts-check go-check tsweb-check; do
  ! grep -Eq "^  ${removed}( |$)" <<<"$help"
done

grep -Fq 'repo: "."' "$root/.moon/workspace.yml"
for task in cpp-build python-build ts-build go-build tsweb-build compile-commands build test; do
  grep -Eq "^  ${task}:$" "$root/moon.yml"
done

printf 'workflow contract: ok\n'
