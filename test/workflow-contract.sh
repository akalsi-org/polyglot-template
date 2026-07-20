#!/usr/bin/env bash
set -euo pipefail

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd -P)
workflow=$root/.github/workflows/verify.yml
release_workflow=$root/.github/workflows/package-release.yml

fail() {
  printf 'workflow contract: failed: %s\n' "$1" >&2
  exit 1
}

assert_present() {
  local description=$1
  shift
  "$@" >/dev/null 2>&1 || fail "$description"
}

assert_absent() {
  local description=$1
  shift
  if "$@" >/dev/null 2>&1; then
    fail "$description"
  fi
}

assert_count() {
  local description=$1 expected=$2 pattern=$3 file=$4 actual
  actual=$(grep -c -- "$pattern" "$file" || true)
  [[ $actual == "$expected" ]] || fail "$description (expected $expected, found $actual)"
}

assert_order() {
  local description=$1 file=$2 before=$3 after=$4
  local -a before_lines after_lines
  mapfile -t before_lines < <(grep -nF -- "$before" "$file" | cut -d: -f1 || true)
  mapfile -t after_lines < <(grep -nF -- "$after" "$file" | cut -d: -f1 || true)
  (( ${#before_lines[@]} == 1 && ${#after_lines[@]} == 1 && before_lines[0] < after_lines[0] )) || \
    fail "$description (expected '$before' before '$after')"
}

assert_order_pattern() {
  local description=$1 file=$2 before=$3 after=$4
  local -a before_lines after_lines
  mapfile -t before_lines < <(grep -nE -- "$before" "$file" | cut -d: -f1 || true)
  mapfile -t after_lines < <(grep -nE -- "$after" "$file" | cut -d: -f1 || true)
  (( ${#before_lines[@]} == 1 && ${#after_lines[@]} == 1 && before_lines[0] < after_lines[0] )) || \
    fail "$description (expected '$before' before '$after')"
}

assert_equal() {
  local description=$1 expected=$2 actual=$3
  [[ $actual == "$expected" ]] || fail "$description (expected '$expected', got '$actual')"
}

assert_matches() {
  local description=$1 pattern=$2 value=$3
  [[ $value =~ $pattern ]] || fail "$description (got '$value')"
}

assert_contains() {
  local description=$1 needle=$2 value=$3
  [[ $value == *"$needle"* ]] || fail "$description (missing '$needle')"
}

assert_count 'exactly one x86_64 runner' 1 'runner: ubuntu-24.04$' "$workflow"
assert_count 'exactly one ARM runner' 1 'runner: ubuntu-24.04-arm$' "$workflow"
assert_present 'workflow pins actions/cache to the approved SHA' grep -Fq 'actions/cache@5a3ec84eff668545956fd18022155c47e93e2684' "$workflow"
assert_present 'workflow pins actions/upload-artifact to the approved SHA' grep -Fq 'actions/upload-artifact@ea165f8d65b6e75b540449e92b4886f43607fa02' "$workflow"
assert_present 'release workflow pins actions/download-artifact to the approved SHA' grep -Fq 'actions/download-artifact@d3f86a106a0bac45b974a628896c90dbdf5c8093' "$release_workflow"
assert_present 'toolchain cache key is runner and target scoped' grep -Fq 'polyglot-tools-v1-${{ runner.os }}-${{ matrix.target }}-' "$workflow"
assert_present 'toolchain cache key includes lockfile and toolchain sources' grep -Fq "hashFiles('tools.lock.toml', 'toolchain/**')" "$workflow"
assert_present 'Deno cache key is runner and target scoped' grep -Fq 'polyglot-deno-v1-${{ runner.os }}-${{ matrix.target }}-' "$workflow"
assert_present 'Deno cache key includes the frozen manifest graph' grep -Fq "hashFiles('tools.lock.toml', 'deno.json', 'deno.lock')" "$workflow"
assert_present 'Buck cache key is runner and target scoped' grep -Fq 'polyglot-buck2-v1-${{ runner.os }}-${{ matrix.target }}-' "$workflow"
assert_present 'Buck cache key covers toolchain inputs only' grep -Fq "hashFiles('tools.lock.toml', 'toolchain/**', 'toolchains/**', 'rules/toolchain.bzl', '.buckconfig')" "$workflow"
assert_absent 'Buck cache key must not invalidate for every rule change' grep -Fq "'rules/**'" "$workflow"
assert_present 'Go build cache path is target scoped' grep -Fq 'buck-out/go-build-cache/${{ matrix.target }}' "$workflow"
assert_present 'Go cache key includes relevant source and toolchain inputs' grep -Fq "hashFiles('go.mod', 'go.sum', 'tools.lock.toml', 'rules/go.bzl', 'toolchains/**')" "$workflow"
assert_present 'Deno dependency cache includes node_modules' grep -Fq '            node_modules' "$workflow"
assert_absent 'workflow must not use broad cache restore keys' grep -q 'restore-keys:' "$workflow"
assert_present 'release workflow only runs for package tags' grep -Fq 'tags: ["packages/*/v*"]' "$release_workflow"
assert_present 'release workflow preserves in-progress releases' grep -Fq 'cancel-in-progress: false' "$release_workflow"
assert_present 'release workflow checks for an existing release' grep -Fq 'gh release view "$GITHUB_REF_NAME" --repo "$GITHUB_REPOSITORY"' "$release_workflow"
assert_present 'workflow pins actions/attest to the approved SHA' grep -Fq 'actions/attest@36051bcae73b7c2a8a6945a48cbf80953c6baa35' "$workflow"
assert_present 'package release caller grants attestation write permission' grep -Fq 'attestations: write' "$release_workflow"
assert_present 'release workflow validates published checksums' grep -Fq 'sha256sum --check' "$release_workflow"
assert_present 'release workflow compares published assets byte-for-byte' grep -Fq 'cmp -- "$asset" "published/$name"' "$release_workflow"
assert_present 'release workflow uploads assets to the selected repository' grep -Fq 'gh release upload "$GITHUB_REF_NAME" "$asset" --repo "$GITHUB_REPOSITORY"' "$release_workflow"
assert_present 'release workflow creates releases with assets and checksums' grep -Fq 'gh release create "$GITHUB_REF_NAME" "${assets[@]}" "${checksums[@]}' "$release_workflow"
assert_absent 'workflow must not reference Moon' grep -qi 'moon' "$workflow"

assert_order_pattern 'online bootstrap precedes offline bootstrap' "$workflow" \
  '^[[:space:]]*\./repo\.sh bootstrap$' '^[[:space:]]*\./repo\.sh bootstrap --offline$'
assert_order_pattern 'offline bootstrap precedes deep doctor' "$workflow" \
  '^[[:space:]]*\./repo\.sh bootstrap --offline$' '^[[:space:]]*\./repo\.sh doctor --deep$'

for command in \
  './repo.sh exec buck2 build //toolchains:native' \
  './repo.sh lint' './repo.sh package-validate' \
  './repo.sh package-target-check "$package"' \
  './test/graph-compdb-contract.sh' \
  './test/deno-manifest-contract.sh' \
  './repo.sh cpp-build dbg' './repo.sh cpp-run dbg' \
  './repo.sh cpp-build opt' './repo.sh cpp-run opt' \
  './repo.sh exec buck2 test --target-platforms //config:${{ matrix.target }}-opt //python/test:test' \
  './repo.sh python -I -c'; do
  assert_present "workflow retains distinct check: $command" grep -Fq "$command" "$workflow"
done
assert_present 'workflow retains a network-isolated cold full-graph build/test replay' \
  grep -Fq "unshare -rn sh -c 'ip link set lo up 2>/dev/null || true; ./repo.sh exec buck2 clean && ./repo.sh build && ./repo.sh test'" "$workflow"
assert_count 'offline replay performs one full-graph build' 1 '^          unshare -rn sh -c .*./repo.sh exec buck2 clean && ./repo.sh build && ./repo.sh test' "$workflow"
assert_absent 'workflow must not repeat the full graph through a direct Buck2 test' grep -Fq './repo.sh exec buck2 test //...' "$workflow"
assert_count 'workflow must not run a second aggregate repo build after the offline replay' 0 '^          ./repo.sh build$' "$workflow"
assert_count 'workflow must not run a second aggregate repo test after the offline replay' 0 '^          ./repo.sh test$' "$workflow"

assert_order 'tag package target validation precedes package assembly' "$workflow" \
  './repo.sh package-target-check "$package"' './repo.sh package "$package" opt'
if sed -n '/^  package)/,/^  package-smoke)/p' "$root/repo.sh" | grep -Fq 'package_release.py'; then
  fail 'repo package command still falls through to the raw-build assembler'
fi

help=$($root/repo.sh help)
for command in shell exec buck2 format lint build test cpp-build cpp-run cpp-test python-build python-test \
  ts-build ts-test tsweb-build tsweb-test go-build go-test package-list package package-smoke; do
  assert_present "repo help documents '$command'" grep -Eq "^  ${command}( |$)" <<<"$help"
done
for removed in _job-budget cpp-configure cpp-reflection-probe; do
  assert_absent "repo help must not document removed command '$removed'" grep -Eq "^  ${removed}( |$)" <<<"$help"
done

assert_absent 'repo.sh must not reference Moon' grep -qi 'moon' "$root/repo.sh"

assert_present 'repo.sh build delegates to the pinned Buck2 binary' grep -Fq '"$POLYGLOT_BUCK2" build' "$root/repo.sh"
assert_present 'repo.sh test selects the requested target platform' grep -Fq 'mapfile -t plat < <(target_platform_args "$profile")' "$root/repo.sh"
assert_present 'repo.sh test delegates to pinned Buck2 over the full graph' grep -Fq '"$POLYGLOT_BUCK2" test "${plat[@]}" //...' "$root/repo.sh"
assert_present 'repo.sh lint runs Buck2 lint-labelled tests' grep -Fq '"$POLYGLOT_BUCK2" test //... --labels lint' "$root/repo.sh"

environment=$($root/repo.sh exec bash -c 'printf "%s|%s|%s|%s|%s|%s\n" "$POLYGLOT_ROOT" "$POLYGLOT_TARGET" "$CXX" "$GOROOT" "$DENO_DIR" "$PYTHONPATH"')
IFS='|' read -r env_root env_target env_cxx env_goroot env_deno_dir env_pythonpath <<<"$environment"
assert_equal 'repo environment exposes the repository root' "$root" "$env_root"
assert_matches 'repo environment selects a supported musl target' '^(x86_64|aarch64)-linux-musl$' "$env_target"
[[ -x $env_cxx && -d $env_goroot && -d $env_deno_dir ]] || fail 'repo environment must expose executable C++ compiler and Go/Deno directories'
assert_equal 'repo environment sets the isolated Python path' "$root/build/python/$env_target/lib:$root/python/lib:$root/python/app" "$env_pythonpath"
assert_equal 'repo python launcher runs in isolated mode' 'python launcher' "$($root/repo.sh exec python -I -c 'print("python launcher")')"
assert_matches 'repo Go launcher uses Go 1.x' '^go version go1\.' "$($root/repo.sh exec go version)"
assert_matches 'repo Deno launcher uses the pinned release' '^deno 2\.9\.2 ' "$($root/repo.sh exec deno --version | head -n1)"
assert_matches 'repo Buck2 launcher runs Buck2' '^buck2 ' "$($root/repo.sh exec buck2 --version)"
assert_equal 'repo shell resolves Python from .local/bin' "$root/.local/bin/python" "$($root/repo.sh exec bash -c 'which python')"
assert_equal 'repo shell resolves Go from .local/bin' "$root/.local/bin/go" "$($root/repo.sh exec bash -c 'which go')"
assert_equal 'repo shell resolves GCC from .local/bin' "$root/.local/bin/gcc" "$($root/repo.sh exec bash -c 'which gcc')"
assert_equal 'repo shell resolves G++ from .local/bin' "$root/.local/bin/g++" "$($root/repo.sh exec bash -c 'which g++')"
assert_equal 'repo shell resolves Buck2 from .local/bin' "$root/.local/bin/buck2" "$($root/repo.sh exec bash -c 'which buck2')"
for binutil in ar ranlib nm strip objcopy ld; do
  assert_equal "repo shell resolves $binutil from .local/bin" "$root/.local/bin/$binutil" "$($root/repo.sh exec bash -c "which $binutil")"
done
assert_equal 'self-contained Python launcher works without environment setup' 'self-contained python' "$(env -i PATH=/usr/bin:/bin "$root/.local/bin/python" -I -c 'print("self-contained python")')"
assert_matches 'self-contained Go launcher works without environment setup' '^go version go1\.' "$(env -i PATH=/usr/bin:/bin "$root/.local/bin/go" version)"
assert_equal 'self-contained GCC launcher reports the selected target' "$env_target" "$(env -i PATH=/usr/bin:/bin "$root/.local/bin/gcc" -dumpmachine)"
assert_equal 'self-contained G++ launcher reports the selected target' "$env_target" "$(env -i PATH=/usr/bin:/bin "$root/.local/bin/g++" -dumpmachine)"
assert_present 'self-contained ar launcher runs without environment setup' env -i PATH=/usr/bin:/bin "$root/.local/bin/ar" --version
assert_present 'self-contained strip launcher runs without environment setup' env -i PATH=/usr/bin:/bin "$root/.local/bin/strip" --version
assert_present 'self-contained Buck2 launcher runs without environment setup' env -i PATH=/usr/bin:/bin "$root/.local/bin/buck2" --version

shell_home=$(mktemp -d)
trap 'rm -rf -- "$shell_home"' EXIT
printf 'export PATH=/usr/bin:/bin\n' >"$shell_home/.bashrc"
shell_paths=$(printf 'which python\nwhich go\nwhich gcc\nwhich g++\nwhich buck2\npython -I -c "print(42)"\ngo version\nexit\n' | HOME=$shell_home "$root/repo.sh" shell 2>/dev/null)
mapfile -t shell_path_lines <<<"$shell_paths"
assert_equal 'interactive repo shell resolves Python from .local/bin' "$root/.local/bin/python" "${shell_path_lines[0]:-}"
assert_equal 'interactive repo shell resolves Go from .local/bin' "$root/.local/bin/go" "${shell_path_lines[1]:-}"
assert_equal 'interactive repo shell resolves GCC from .local/bin' "$root/.local/bin/gcc" "${shell_path_lines[2]:-}"
assert_equal 'interactive repo shell resolves G++ from .local/bin' "$root/.local/bin/g++" "${shell_path_lines[3]:-}"
assert_equal 'interactive repo shell resolves Buck2 from .local/bin' "$root/.local/bin/buck2" "${shell_path_lines[4]:-}"
assert_contains 'interactive repo shell runs Python' $'\n42\n' "$shell_paths"
assert_contains 'interactive repo shell runs Go' $'\ngo version go1.' "$shell_paths"
assert_present 'repo.sh sources its dedicated shell rc file' grep -Fq 'repo-shell.bashrc' "$root/repo.sh"
assert_absent 'repo.sh must not launch shell as a login shell' grep -Fq -- '--login' "$root/repo.sh"
assert_absent 'repo.sh must not source deprecated toolchain/env' grep -Fq 'toolchain/env' "$root/repo.sh"

printf 'workflow contract: ok\n'
