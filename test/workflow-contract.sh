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
for command in shell exec build test cpp-test python-test ts-test go-test tsweb-test; do
  grep -Eq "^  ${command}( |$)" <<<"$help"
done
for removed in python-check ts-check go-check tsweb-check; do
  ! grep -Eq "^  ${removed}( |$)" <<<"$help"
done

[[ $($root/repo.sh _job-budget 1) == '1 1' ]]
[[ $($root/repo.sh _job-budget 4) == '2 2' ]]
[[ $($root/repo.sh _job-budget 8) == '4 2' ]]
[[ $($root/repo.sh _job-budget 16) == '5 3' ]]

grep -Fq 'jobserver_start' "$root/repo.sh"
grep -Fq 'jobserver_stop' "$root/repo.sh"
grep -Fq 'mkfifo' "$root/repo.sh"
grep -Fq 'MAKEFLAGS="--jobserver-auth=fifo:' "$root/repo.sh"
grep -Fq 'export MAKEFLAGS' "$root/repo.sh"
grep -Fq 'tokens=' "$root/repo.sh"
grep -Fq -- '--jobserver-auth=' "$root/repo.sh"

environment=$($root/repo.sh exec bash -c 'printf "%s|%s|%s|%s|%s|%s\n" "$POLYGLOT_ROOT" "$POLYGLOT_TARGET" "$CXX" "$GOROOT" "$DENO_DIR" "$PYTHONPATH"')
IFS='|' read -r env_root env_target env_cxx env_goroot env_deno_dir env_pythonpath <<<"$environment"
[[ $env_root == "$root" ]]
[[ $env_target =~ ^(x86_64|aarch64)-linux-musl$ ]]
[[ -x $env_cxx && -d $env_goroot && -d $env_deno_dir ]]
[[ $env_pythonpath == "$root/build/python/$env_target/lib:$root/python/lib:$root/python/app" ]]
[[ $($root/repo.sh exec python -I -c 'print("python launcher")') == 'python launcher' ]]
[[ $($root/repo.sh exec go version) == 'go version go1.'* ]]
[[ $($root/repo.sh exec deno --version | head -n1) == 'deno 2.9.2 '* ]]
[[ $($root/repo.sh exec bash -c 'which python') == "$root/.local/bin/python" ]]
[[ $($root/repo.sh exec bash -c 'which go') == "$root/.local/bin/go" ]]
[[ $($root/repo.sh exec bash -c 'which gcc') == "$root/.local/bin/gcc" ]]
[[ $($root/repo.sh exec bash -c 'which g++') == "$root/.local/bin/g++" ]]
for binutil in ar ranlib nm strip objcopy ld; do
  [[ $($root/repo.sh exec bash -c "which $binutil") == "$root/.local/bin/$binutil" ]]
done
[[ $(env -i PATH=/usr/bin:/bin "$root/.local/bin/python" -I -c 'print("self-contained python")') == 'self-contained python' ]]
[[ $(env -i PATH=/usr/bin:/bin "$root/.local/bin/go" version) == 'go version go1.'* ]]
[[ $(env -i PATH=/usr/bin:/bin "$root/.local/bin/gcc" -dumpmachine) == "$env_target" ]]
[[ $(env -i PATH=/usr/bin:/bin "$root/.local/bin/g++" -dumpmachine) == "$env_target" ]]
env -i PATH=/usr/bin:/bin "$root/.local/bin/ar" --version >/dev/null
env -i PATH=/usr/bin:/bin "$root/.local/bin/strip" --version >/dev/null

shell_home=$(mktemp -d)
trap 'rm -rf -- "$shell_home"' EXIT
printf 'export PATH=/usr/bin:/bin\n' >"$shell_home/.bashrc"
shell_paths=$(printf 'which python\nwhich go\nwhich gcc\nwhich g++\npython -I -c "print(42)"\ngo version\nexit\n' | HOME=$shell_home "$root/repo.sh" shell 2>/dev/null)
mapfile -t shell_path_lines <<<"$shell_paths"
[[ ${shell_path_lines[0]} == "$root/.local/bin/python" ]]
[[ ${shell_path_lines[1]} == "$root/.local/bin/go" ]]
[[ ${shell_path_lines[2]} == "$root/.local/bin/gcc" ]]
[[ ${shell_path_lines[3]} == "$root/.local/bin/g++" ]]
[[ $shell_paths == *$'\n42\n'* ]]
[[ $shell_paths == *$'\ngo version go1.'* ]]
grep -Fq 'repo-shell.bashrc' "$root/repo.sh"
! grep -Fq -- '--login' "$root/repo.sh"
! grep -Fq 'toolchain/env' "$root/repo.sh"

grep -Fq 'repo: "."' "$root/.moon/workspace.yml"
for task in cpp-build python-build ts-build go-build tsweb-build compile-commands build test; do
  grep -Eq "^  ${task}:$" "$root/moon.yml"
done

printf 'workflow contract: ok\n'
