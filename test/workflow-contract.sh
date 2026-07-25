#!/usr/bin/env bash
set -euo pipefail

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd -P)
workflow=$root/.github/workflows/verify.yml
release_workflow=$root/.github/workflows/package-release.yml
caller_workflow=$root/.github/workflows/ci-release.yml

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

assert_fixed_count() {
  local description=$1 expected=$2 text=$3 file=$4 actual
  actual=$(grep -F -c -- "$text" "$file" || true)
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

assert_count 'x86_64 runners across parallel jobs' 3 'runner: ubuntu-24.04$' "$workflow"
assert_count 'ARM runners across parallel jobs' 3 'runner: ubuntu-24.04-arm$' "$workflow"
assert_present 'workflow pins actions/cache to the approved SHA' grep -Fq 'actions/cache@5a3ec84eff668545956fd18022155c47e93e2684' "$workflow"
assert_present 'workflow pins actions/upload-artifact to the approved SHA' grep -Fq 'actions/upload-artifact@ea165f8d65b6e75b540449e92b4886f43607fa02' "$workflow"
assert_present 'release workflow pins actions/download-artifact to the approved SHA' grep -Fq 'actions/download-artifact@d3f86a106a0bac45b974a628896c90dbdf5c8093' "$release_workflow"
assert_present 'toolchain cache key is runner and target scoped' grep -Fq 'polyglot-v1-${{ runner.os }}-${{ matrix.target }}' "$workflow"
assert_absent 'Buck cache key must not invalidate for every rule change' grep -Fq "'rules/**'" "$workflow"
assert_present 'Go build cache path is target scoped' grep -Fq 'buck-out/go-build-cache/${{ matrix.target }}' "$workflow"
assert_present 'Deno dependency cache includes node_modules' grep -Fq '            node_modules' "$workflow"
assert_present 'release workflow only runs for package tags' grep -Fq 'tags: ["packages/*/v*"]' "$release_workflow"
assert_present 'release workflow preserves in-progress releases' grep -Fq 'cancel-in-progress: false' "$release_workflow"
assert_present 'release workflow checks for an existing release' grep -Fq 'gh release view "$GITHUB_REF_NAME" --repo "$GITHUB_REPOSITORY"' "$release_workflow"
assert_present 'release workflow pins actions/attest to the approved SHA' \
  grep -Fq 'actions/attest@36051bcae73b7c2a8a6945a48cbf80953c6baa35' "$release_workflow"
assert_present 'release workflow grants attestation write permission' grep -Fq 'attestations: write' "$release_workflow"
assert_present 'attestation is minted by the publisher, over published archives' \
  grep -Fq 'subject-path: artifacts/*.tar.gz' "$release_workflow"
assert_order 'archives are attested only after their checksums are verified' "$release_workflow" \
  'sha256sum --check' 'Attest published package archives'
assert_present 'release workflow validates published checksums' grep -Fq 'sha256sum --check' "$release_workflow"
assert_present 'verify workflow records checksums against the bare archive name' \
  grep -Fq '(cd dist && sha256sum "$(basename "$archive")" > "$(basename "$archive").sha256")' "$workflow"
assert_absent 'verify workflow must not bake the dist/ prefix into a checksum body' \
  grep -Eq '^ +sha256sum "\$archive" > "\$archive\.sha256"$' "$workflow"
assert_present 'verify workflow defaults every job to read-only token scope' \
  grep -Eq '^permissions:$' "$workflow"
# THE rule that a previous iteration got wrong, and that GitHub only reports
# as "Invalid workflow file" at dispatch time, on every branch push: a called
# workflow's job may request no more than its CALLER grants. Both callers of
# verify.yml reach it through a `verify:` job with no permissions block, so
# verify.yml's effective ceiling is ci-release.yml's `contents: read`. Any
# write scope anywhere in the called workflow invalidates it - assert on the
# scopes, not on where they appear, because the failure is total.
for scope in 'id-token: write' 'attestations: write' 'artifact-metadata: write' 'contents: write'; do
  assert_absent "verify workflow must not request '$scope' (it exceeds its caller's grant)" \
    grep -Fq "$scope" "$workflow"
done
assert_absent 'verify workflow must not mint attestations' grep -Fq 'actions/attest@' "$workflow"
assert_absent 'the verify caller must not widen token scope for branch runs' \
  grep -Eq 'id-token: write|attestations: write' "$caller_workflow"
assert_absent 'verify workflow must not expand a tag-derived value into a run body' \
  grep -Fq 'package=${{ steps.package.outputs.name }}' "$workflow"
assert_present 'verify workflow passes the tag-derived package name as environment data' \
  grep -Fq '          package: ${{ steps.package.outputs.name }}' "$workflow"
assert_present 'release job bootstraps the pinned toolchain before publishing' \
  grep -Fq './repo.sh bootstrap --offline || ./repo.sh bootstrap' "$release_workflow"
assert_present 'release job restores the same cache structure verify uses' \
  grep -Fq 'key: polyglot-v1-${{ runner.os }}-x86_64-linux-musl-${{ hashFiles(' "$release_workflow"

# The published-checksum path is the one that broke: asserting only that the
# literal string "sha256sum --check" appears proves nothing about whether the
# recorded name survives upload-artifact's prefix stripping. Reproduce the
# whole round trip - write the checksum exactly the way verify.yml does, flatten
# dist/ into artifacts/ exactly the way upload-artifact does, then run the
# publisher's verification command for real.
checksum_round_trip() {
  local scratch
  scratch=$(mktemp -d "$root/buck-out/v2/tmp/workflow-contract.XXXXXX")
  mkdir -p "$scratch/dist" "$scratch/artifacts"
  local archive="$scratch/dist/polyglot-demo-0.1.0-x86_64-linux-musl.tar.gz"
  printf 'payload\n' >"$archive"
  # Verbatim from verify.yml's "Assemble and verify tagged package" step.
  (cd "$scratch/dist" && sha256sum "$(basename "$archive")" > "$(basename "$archive").sha256")
  # upload-artifact uploads dist/*.tar.gz* with the dist/ prefix stripped, and
  # download-artifact re-materializes them flat under artifacts/.
  cp -- "$scratch/dist"/* "$scratch/artifacts/"
  # Verbatim from package-release.yml's publish step.
  local checksum
  for checksum in "$scratch/artifacts"/*.tar.gz.sha256; do
    (cd "$scratch/artifacts" && sha256sum --check "$(basename "$checksum")") >/dev/null || {
      rm -rf -- "$scratch"
      fail 'published checksum does not verify from a flattened artifacts/ layout'
    }
  done
  # And prove the assertion has teeth: reproduce the OLD dist/-prefixed body
  # and require that it still fails from the flattened layout.
  printf '%s  dist/%s\n' "$(sha256sum "$archive" | awk '{ print $1 }')" "$(basename "$archive")" \
    >"$scratch/artifacts/prefixed.sha256"
  if (cd "$scratch/artifacts" && sha256sum --check prefixed.sha256) >/dev/null 2>&1; then
    rm -rf -- "$scratch"
    fail 'a dist/-prefixed checksum body unexpectedly verified; this test cannot detect the regression it exists for'
  fi
  rm -rf -- "$scratch"
}
mkdir -p "$root/buck-out/v2/tmp"
checksum_round_trip
assert_present 'tag verification rejects lightweight release tags' grep -Fq 'git cat-file -t "$GITHUB_REF_NAME"' "$workflow"
assert_present 'tag verification fetches annotated tag objects' grep -Fq 'git fetch --force --tags origin' "$workflow"
assert_present 'tag packaging requires an SBOM sidecar' grep -Fq 'test -s "$archive.sbom.json"' "$workflow"
assert_present 'tag packaging requires a provenance sidecar' grep -Fq 'test -s "$archive.provenance.json"' "$workflow"
assert_present 'tag workflow uploads SBOM evidence' grep -Fq 'dist/*.tar.gz.sbom.json' "$workflow"
assert_present 'tag workflow uploads provenance evidence' grep -Fq 'dist/*.tar.gz.provenance.json' "$workflow"
assert_present 'release workflow verifies evidence and writes a manifest' grep -Fq 'tools/release_evidence.py release-manifest' "$release_workflow"
assert_present 'release workflow publishes the release manifest' grep -Fq 'artifacts/release-manifest.json' "$release_workflow"
assert_present 'release workflow compares published assets byte-for-byte' grep -Fq 'cmp -- "$asset" "published/$name"' "$release_workflow"
assert_present 'release workflow uploads assets to the selected repository' grep -Fq 'gh release upload "$GITHUB_REF_NAME" "$asset" --repo "$GITHUB_REPOSITORY"' "$release_workflow"
assert_present 'release workflow creates releases with archives, checksums, and evidence' grep -Fq 'gh release create "$GITHUB_REF_NAME" "${release_assets[@]}' "$release_workflow"
assert_absent 'workflow must not reference Moon' grep -qi 'moon' "$workflow"

for command in \
  './repo.sh lint' './repo.sh package-validate' './repo.sh infra-test' \
  './repo.sh test opt' './repo.sh coverage' \
  './repo.sh package-target-check "$package"' \
  './repo.sh cpp-build dbg' './repo.sh cpp-run dbg' \
  './repo.sh cpp-build opt' './repo.sh cpp-run opt' \
  './repo.sh exec buck2 test --target-platforms //config:${{ matrix.target }}-opt //python/test:test' \
  './repo.sh python -I -c'; do
  assert_present "workflow retains distinct check: $command" grep -Fq "$command" "$workflow"
done
assert_present 'workflow generates coverage from a cold Buck output tree' \
  grep -Fq './repo.sh exec buck2 clean && ./repo.sh coverage' "$workflow"
# The lint job's gates must precede the offline replay. The replay begins with
# `buck2 clean`, so gates placed after it rebuild the whole graph from scratch
# on a cold daemon - measured at roughly double the lint step's cost - while
# the restored buck-out cache is thrown away having served one toolchain
# build. Ordering is the entire fix, so it is the thing asserted.
assert_order 'lint job gates run before the cache-destroying offline replay' "$workflow" \
  'name: Run repository quality gates & infra tests' \
  'name: Offline full-graph replay (cold buck-out, no network)'
assert_present 'workflow uploads a target-keyed merged coverage artifact' \
  grep -Fq 'name: coverage-${{ matrix.target }}' "$workflow"
assert_present 'workflow uploads the merged lcov report' \
  grep -Fq 'buck-out/**/merged.lcov' "$workflow"
assert_present 'workflow uploads the coverage summary' \
  grep -Fq 'buck-out/**/summary.txt' "$workflow"
assert_present 'workflow uploads the browsable HTML coverage report' \
  grep -Fq 'buck-out/coverage-report/coverage.html' "$workflow"
# The job summary is the only coverage view a reviewer sees without
# downloading anything, so assert the whole chain that produces it: the
# generator is asked for Markdown, the Markdown reaches $GITHUB_STEP_SUMMARY,
# and both steps survive a failing coverage floor.
assert_present 'workflow asks the coverage run for a job-summary rendering' \
  grep -Fq 'POLYGLOT_COVERAGE_MARKDOWN: coverage-summary.md' "$workflow"
assert_present 'workflow writes coverage into the GitHub job summary' \
  grep -Fq 'cat coverage-summary.md' "$workflow"
assert_present 'workflow links the job summary to the HTML report artifact' \
  grep -Fq 'steps.coverage-artifact.outputs.artifact-url' "$workflow"
assert_present 'coverage publication survives a failing coverage floor' \
  grep -Fq 'name: Publish coverage to the job summary' "$workflow"
# ...but only once coverage actually ran: a bare always() would turn any
# earlier failure into a second, misleading red step for an artifact that was
# never produced. Both publication steps must carry the same guard.
assert_equal 'both coverage publication steps are guarded on the coverage step' \
  2 "$(grep -Fc "if: always() && steps.coverage.conclusion != 'skipped'" "$workflow")"
assert_present 'branch and PR runs rehearse release without publishing' \
  grep -Fq "if: \${{ !startsWith(github.ref, 'refs/tags/packages/') }}" "$workflow"
assert_present 'release rehearsal packages the selected package under opt' \
  grep -Fq './repo.sh package "$package" opt' "$workflow"
assert_present 'release rehearsal verifies the selected package tag' \
  grep -Fq './repo.sh release-check "$package" "$tag"' "$workflow"
assert_absent 'verification workflow must not publish releases' grep -Fq 'gh release ' "$workflow"
assert_present 'tag package artifacts remain package-tag guarded' \
  grep -Fq "if: startsWith(github.ref, 'refs/tags/packages/')" "$workflow"
assert_present 'package release workflow remains the tag-only publisher' \
  grep -Fq 'gh release create' "$release_workflow"
assert_present 'package release workflow only invokes verify on package tags' \
  grep -Fq 'tags: ["packages/*/v*"]' "$release_workflow"
assert_present 'workflow retains a network-isolated cold full-graph build/test replay' \
  grep -Fq "unshare -rn sh -c 'ip link set lo up 2>/dev/null || true; ./repo.sh exec buck2 clean && ./repo.sh build && ./repo.sh test'" "$workflow"
assert_count 'offline replay performs one full-graph build' 1 '^          unshare -rn sh -c .*./repo.sh exec buck2 clean && ./repo.sh build && ./repo.sh test' "$workflow"
assert_absent 'workflow must not repeat the full graph through a direct Buck2 test' grep -Fq './repo.sh exec buck2 test //...' "$workflow"
assert_count 'workflow must not run a second aggregate repo build after the offline replay' 0 '^          ./repo.sh build$' "$workflow"
assert_count 'workflow must not run a second aggregate repo test after the offline replay' 0 '^          ./repo.sh test$' "$workflow"

assert_count 'each package assembly is preceded by an explicit target validation' 2 \
  './repo.sh package-target-check "\$package"' "$workflow"
assert_count 'workflow assembles package artifacts only for rehearsal and tag release paths' 2 \
  './repo.sh package "\$package" opt' "$workflow"
if sed -n '/^  package)/,/^  package-smoke)/p' "$root/repo.sh" | grep -Fq 'package_release.py'; then
  fail 'repo package command still falls through to the raw-build assembler'
fi

# Every check CI runs must be reachable from `./repo.sh ci`, so a local green
# result means what the CI green result means. The two graph contracts used to
# be workflow-only steps; they now belong to infra-test, and `ci` is a strict
# superset of the lint job's gates.
infra_test_block=$(sed -n '/^  infra-test)/,/^    ;;$/p' "$root/repo.sh")
for script in test/graph-compdb-contract.sh test/deno-manifest-contract.sh; do
  assert_present "repo infra-test runs $script" grep -Fq "$script" <<<"$infra_test_block"
done
ci_block=$(sed -n '/^  ci)/,/^    ;;$/p' "$root/repo.sh")
for gate in 'doctor --deep' 'lint' 'package-validate' 'infra-test' 'build' 'test' 'coverage'; do
  assert_present "repo ci runs $gate" grep -Fq "\"\$ROOT/repo.sh\" $gate" <<<"$ci_block"
done

help=$("$root/repo.sh" help)
for command in shell exec buck2 format lint build test cpp-build cpp-run cpp-test python-build python-test \
  ts-build ts-test tsweb-build tsweb-test go-build go-test init-project package-list package package-smoke; do
  assert_present "repo help documents '$command'" grep -Eq "^  ${command}( |$)" <<<"$help"
done
for removed in _job-budget cpp-configure cpp-reflection-probe; do
  assert_absent "repo help must not document removed command '$removed'" grep -Eq "^  ${removed}( |$)" <<<"$help"
done

assert_absent 'repo.sh must not reference Moon' grep -qi 'moon' "$root/repo.sh"

assert_present 'repo.sh build delegates to the pinned Buck2 binary' grep -Fq '"$POLYGLOT_BUCK2" build' "$root/repo.sh"
assert_present 'repo.sh test selects the requested target platform' grep -Fq 'mapfile -t plat < <(target_platform_args "$profile")' "$root/repo.sh"
assert_present 'repo.sh test delegates to pinned Buck2 over the full graph' grep -Fq '"$POLYGLOT_BUCK2" test "${plat[@]}" //...' "$root/repo.sh"
assert_fixed_count 'aggregate build, test, and cpp-build refresh profile compdb' 3 \
  '[[ ${POLYGLOT_DEFER_COMPDB:-0} == 1 ]] || "$ROOT/repo.sh" compile-commands "$profile"' "$root/repo.sh"
assert_present 'repo.sh lint runs Buck2 lint-labelled tests' grep -Fq '"$POLYGLOT_BUCK2" test //... --labels lint' "$root/repo.sh"

environment=$("$root/repo.sh" exec bash -c 'printf "%s|%s|%s|%s|%s|%s\n" "$POLYGLOT_ROOT" "$POLYGLOT_TARGET" "$CXX" "$GOROOT" "$DENO_DIR" "$PYTHONPATH"')
IFS='|' read -r env_root env_target env_cxx env_goroot env_deno_dir env_pythonpath <<<"$environment"
assert_equal 'repo environment exposes the repository root' "$root" "$env_root"
assert_matches 'repo environment selects a supported musl target' '^(x86_64|aarch64)-linux-musl$' "$env_target"
[[ -x $env_cxx && -d $env_goroot && -d $env_deno_dir ]] || fail 'repo environment must expose executable C++ compiler and Go/Deno directories'
assert_equal 'repo environment sets the isolated Python path' "$root/build/python/$env_target/lib:$root/python/lib:$root/python/app" "$env_pythonpath"
assert_equal 'repo python launcher runs in isolated mode' 'python launcher' "$("$root/repo.sh" exec python -I -c 'print("python launcher")')"
assert_matches 'repo Go launcher uses Go 1.x' '^go version go1\.' "$("$root/repo.sh" exec go version)"
assert_matches 'repo Deno launcher uses the pinned release' '^deno 2\.9\.2 ' "$("$root/repo.sh" exec deno --version | head -n1)"
assert_matches 'repo Buck2 launcher runs Buck2' '^buck2 ' "$("$root/repo.sh" exec buck2 --version)"
assert_equal 'repo shell resolves Python from .local/bin' "$root/.local/bin/python" "$("$root/repo.sh" exec bash -c 'which python')"
assert_equal 'repo shell resolves Go from .local/bin' "$root/.local/bin/go" "$("$root/repo.sh" exec bash -c 'which go')"
assert_equal 'repo shell resolves GCC from .local/bin' "$root/.local/bin/gcc" "$("$root/repo.sh" exec bash -c 'which gcc')"
assert_equal 'repo shell resolves G++ from .local/bin' "$root/.local/bin/g++" "$("$root/repo.sh" exec bash -c 'which g++')"
assert_equal 'repo shell resolves Buck2 from .local/bin' "$root/.local/bin/buck2" "$("$root/repo.sh" exec bash -c 'which buck2')"
for binutil in ar ranlib nm strip objcopy ld; do
  assert_equal "repo shell resolves $binutil from .local/bin" "$root/.local/bin/$binutil" "$("$root/repo.sh" exec bash -c "which $binutil")"
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
