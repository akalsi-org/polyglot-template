#!/usr/bin/env bash
set -euo pipefail

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd -P)
workflow=$root/.github/workflows/ci-release.yml

# `! grep ...` does not trip `set -e` (bash exempts commands negated with
# `!` from errexit), so a bare `! grep -q pattern file` here would silently
# pass even when the pattern IS present. This helper actually enforces it.
assert_absent() {
  local description=$1; shift
  if "$@" >/dev/null 2>&1; then
    printf 'workflow contract: unexpectedly present: %s\n' "$description" >&2
    exit 1
  fi
}

[[ $(grep -c 'runner: ubuntu-24.04$' "$workflow") == 1 ]]
[[ $(grep -c 'runner: ubuntu-24.04-arm$' "$workflow") == 1 ]]
grep -Fq 'actions/cache@5a3ec84eff668545956fd18022155c47e93e2684' "$workflow"
grep -Fq 'actions/upload-artifact@ea165f8d65b6e75b540449e92b4886f43607fa02' "$workflow"
grep -Fq 'actions/download-artifact@d3f86a106a0bac45b974a628896c90dbdf5c8093' "$workflow"
grep -Fq 'polyglot-tools-v1-${{ runner.os }}-${{ matrix.target }}-' "$workflow"
grep -Fq "hashFiles('tools.lock.toml', 'toolchain/**')" "$workflow"
grep -Fq 'polyglot-deno-v1-${{ runner.os }}-${{ matrix.target }}-' "$workflow"
grep -Fq "hashFiles('tools.lock.toml', 'deno.lock')" "$workflow"
grep -Fq 'polyglot-buck2-v1-${{ runner.os }}-${{ matrix.target }}-' "$workflow"
grep -Fq "hashFiles('tools.lock.toml', 'toolchains/**', 'rules/**', 'platforms/**', '.buckconfig')" "$workflow"
grep -Fq '            node_modules' "$workflow"
assert_absent 'restore-keys: in workflow' grep -q 'restore-keys:' "$workflow"
grep -Fq 'tags: ["packages/*/v*"]' "$workflow"
grep -Fq 'gh release create "$GITHUB_REF_NAME" artifacts/*' "$workflow"
assert_absent 'moon in workflow' grep -qi 'moon' "$workflow"

bootstrap_line=$(grep -n './repo.sh bootstrap$' "$workflow" | cut -d: -f1)
offline_line=$(grep -n './repo.sh bootstrap --offline$' "$workflow" | cut -d: -f1)
doctor_line=$(grep -n './repo.sh doctor --deep$' "$workflow" | cut -d: -f1)
((bootstrap_line < offline_line && offline_line < doctor_line))

for command in \
  './repo.sh exec buck2 build //toolchains:native' './repo.sh exec buck2 test //...' \
  './repo.sh lint' './repo.sh package-validate' './repo.sh build' './repo.sh test' \
  './repo.sh cpp-build dbg' './repo.sh cpp-run dbg' \
  './repo.sh cpp-build opt' './repo.sh cpp-run opt' \
  './repo.sh python -I -c'; do
  grep -Fq "$command" "$workflow"
done

help=$($root/repo.sh help)
for command in shell exec buck2 format lint build test cpp-build cpp-run cpp-test python-build python-test \
  ts-build ts-test tsweb-build tsweb-test go-build go-test package package-smoke; do
  grep -Eq "^  ${command}( |$)" <<<"$help"
done
for removed in _job-budget cpp-configure cpp-reflection-probe; do
  assert_absent "$removed in help output" grep -Eq "^  ${removed}( |$)" <<<"$help"
done

assert_absent 'moon in repo.sh' grep -qi 'moon' "$root/repo.sh"

grep -Fq '"$POLYGLOT_BUCK2" build' "$root/repo.sh"
grep -Fq '"$POLYGLOT_BUCK2" test //...' "$root/repo.sh"
grep -Fq '"$POLYGLOT_BUCK2" test //... --labels lint' "$root/repo.sh"

environment=$($root/repo.sh exec bash -c 'printf "%s|%s|%s|%s|%s|%s\n" "$POLYGLOT_ROOT" "$POLYGLOT_TARGET" "$CXX" "$GOROOT" "$DENO_DIR" "$PYTHONPATH"')
IFS='|' read -r env_root env_target env_cxx env_goroot env_deno_dir env_pythonpath <<<"$environment"
[[ $env_root == "$root" ]]
[[ $env_target =~ ^(x86_64|aarch64)-linux-musl$ ]]
[[ -x $env_cxx && -d $env_goroot && -d $env_deno_dir ]]
[[ $env_pythonpath == "$root/build/python/$env_target/lib:$root/python/lib:$root/python/app" ]]
[[ $($root/repo.sh exec python -I -c 'print("python launcher")') == 'python launcher' ]]
[[ $($root/repo.sh exec go version) == 'go version go1.'* ]]
[[ $($root/repo.sh exec deno --version | head -n1) == 'deno 2.9.2 '* ]]
[[ $($root/repo.sh exec buck2 --version) == 'buck2 '* ]]
[[ $($root/repo.sh exec bash -c 'which python') == "$root/.local/bin/python" ]]
[[ $($root/repo.sh exec bash -c 'which go') == "$root/.local/bin/go" ]]
[[ $($root/repo.sh exec bash -c 'which gcc') == "$root/.local/bin/gcc" ]]
[[ $($root/repo.sh exec bash -c 'which g++') == "$root/.local/bin/g++" ]]
[[ $($root/repo.sh exec bash -c 'which buck2') == "$root/.local/bin/buck2" ]]
for binutil in ar ranlib nm strip objcopy ld; do
  [[ $($root/repo.sh exec bash -c "which $binutil") == "$root/.local/bin/$binutil" ]]
done
[[ $(env -i PATH=/usr/bin:/bin "$root/.local/bin/python" -I -c 'print("self-contained python")') == 'self-contained python' ]]
[[ $(env -i PATH=/usr/bin:/bin "$root/.local/bin/go" version) == 'go version go1.'* ]]
[[ $(env -i PATH=/usr/bin:/bin "$root/.local/bin/gcc" -dumpmachine) == "$env_target" ]]
[[ $(env -i PATH=/usr/bin:/bin "$root/.local/bin/g++" -dumpmachine) == "$env_target" ]]
env -i PATH=/usr/bin:/bin "$root/.local/bin/ar" --version >/dev/null
env -i PATH=/usr/bin:/bin "$root/.local/bin/strip" --version >/dev/null
env -i PATH=/usr/bin:/bin "$root/.local/bin/buck2" --version >/dev/null

shell_home=$(mktemp -d)
trap 'rm -rf -- "$shell_home"' EXIT
printf 'export PATH=/usr/bin:/bin\n' >"$shell_home/.bashrc"
shell_paths=$(printf 'which python\nwhich go\nwhich gcc\nwhich g++\nwhich buck2\npython -I -c "print(42)"\ngo version\nexit\n' | HOME=$shell_home "$root/repo.sh" shell 2>/dev/null)
mapfile -t shell_path_lines <<<"$shell_paths"
[[ ${shell_path_lines[0]} == "$root/.local/bin/python" ]]
[[ ${shell_path_lines[1]} == "$root/.local/bin/go" ]]
[[ ${shell_path_lines[2]} == "$root/.local/bin/gcc" ]]
[[ ${shell_path_lines[3]} == "$root/.local/bin/g++" ]]
[[ ${shell_path_lines[4]} == "$root/.local/bin/buck2" ]]
[[ $shell_paths == *$'\n42\n'* ]]
[[ $shell_paths == *$'\ngo version go1.'* ]]
grep -Fq 'repo-shell.bashrc' "$root/repo.sh"
assert_absent '--login in repo.sh' grep -Fq -- '--login' "$root/repo.sh"
assert_absent 'toolchain/env in repo.sh' grep -Fq 'toolchain/env' "$root/repo.sh"

printf 'workflow contract: ok\n'
