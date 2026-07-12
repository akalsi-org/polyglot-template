#!/usr/bin/env bash
set -euo pipefail

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd -P)
workflow=$root/.github/workflows/ci-release.yml

[[ $(grep -c 'runner: ubuntu-24.04$' "$workflow") == 1 ]]
[[ $(grep -c 'runner: ubuntu-24.04-arm$' "$workflow") == 1 ]]
grep -Fq 'actions/cache@5a3ec84eff668545956fd18022155c47e93e2684' "$workflow"
grep -Fq 'polyglot-tools-v1-${{ runner.os }}-${{ matrix.target }}-' "$workflow"
grep -Fq "hashFiles('tools.lock.toml', 'toolchain/**')" "$workflow"
! grep -q 'restore-keys:' "$workflow"

bootstrap_line=$(grep -n './repo.sh bootstrap$' "$workflow" | cut -d: -f1)
offline_line=$(grep -n './repo.sh bootstrap --offline$' "$workflow" | cut -d: -f1)
doctor_line=$(grep -n './repo.sh doctor --deep$' "$workflow" | cut -d: -f1)
((bootstrap_line < offline_line && offline_line < doctor_line))

for command in \
  './repo.sh cpp-build dbg' './repo.sh cpp-run dbg' \
  './repo.sh cpp-build opt' './repo.sh cpp-run opt' \
  './repo.sh python -I -c' './repo.sh python-check' \
  './repo.sh ts-check' './repo.sh tsweb-check' './repo.sh go-check'; do
  grep -Fq "$command" "$workflow"
done

printf 'workflow contract: ok\n'
