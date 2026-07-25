#!/usr/bin/env bash
set -euo pipefail

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd -P)
export POLYGLOT_ROOT=${POLYGLOT_ROOT:-$ROOT}
export POLYGLOT_LOCAL_DIR=${POLYGLOT_LOCAL_DIR:-$ROOT/.local}
. "$ROOT/toolchain/wrappers.sh"

# Every buck2 --show-output path below is relative to the repository root, so
# the whole script runs from $ROOT. The passthrough commands (shell, exec,
# buck2, python, deno, go) restore the caller's directory before handing over,
# because those are the caller's own working directory, not this script's.
INVOCATION_CWD=$PWD
cd -- "$ROOT"

restore_invocation_cwd() { cd -- "$INVOCATION_CWD"; }

# A path the CALLER typed is relative to the caller's directory, not to $ROOT.
# Used for the handful of commands that accept a filesystem path argument.
caller_path() {
  case $1 in
    /*) printf '%s\n' "$1" ;;
    *) printf '%s/%s\n' "$INVOCATION_CWD" "$1" ;;
  esac
}

# Repo scratch policy (mirrors tools/coverage_merge.py): never /tmp (small
# tmpfs), always under buck-out. One directory per invocation, removed by an
# EXIT trap so a `set -e` abort mid-function cannot leak it.
SCRATCH_DIR=
scratch_file() {
  if [[ -z $SCRATCH_DIR ]]; then
    mkdir -p "$ROOT/buck-out/v2/tmp"
    SCRATCH_DIR=$(mktemp -d "$ROOT/buck-out/v2/tmp/repo-sh.XXXXXX")
    trap 'rm -rf -- "$SCRATCH_DIR"' EXIT
  fi
  mktemp "$SCRATCH_DIR/scratch.XXXXXX"
}

tool_path() {
  local tool=$1 target version expected
  target=$("$ROOT/toolchain/target.sh")
  export POLYGLOT_LOCK_FILE=${POLYGLOT_LOCK_FILE:-$ROOT/tools.lock.toml}
  . "$ROOT/toolchain/lock.sh"
  version=$(lock_value "$tool" "$target" version)
  expected=$(lock_value "$tool" "$target" expected)
  printf '%s/toolchain/%s/%s-%s/%s\n' "$POLYGLOT_LOCAL_DIR" "$target" "$tool" "$version" "$expected"
}

setup_environment() {
  local target gcc python deno go buck2 clang_format shellcheck gcc_bin go_root path_prefix loader loader_path env_bin gcc_install jobs
  target=$("$ROOT/toolchain/target.sh")
  export POLYGLOT_TARGET=$target
  export POLYGLOT_LOCK_FILE=${POLYGLOT_LOCK_FILE:-$ROOT/tools.lock.toml}

  gcc=$(tool_path gcc-musl)
  python=$(tool_path python)
  deno=$(tool_path deno)
  go=$(tool_path go)
  buck2=$(tool_path buck2)
  clang_format=$(tool_path clang-format)
  shellcheck=$(tool_path shellcheck)
  for tool in "$gcc" "$python" "$deno" "$go" "$buck2" "$clang_format" "$shellcheck"; do
    [[ -x $tool ]] || { printf 'error: pinned toolchain is not installed; run ./repo.sh bootstrap\n' >&2; return 1; }
  done

  gcc_bin=$(dirname -- "$gcc")
  go_root=$(dirname -- "$(dirname -- "$go")")
  . "$ROOT/toolchain/lock.sh"
  loader=$(lock_value gcc-musl "$target" loader)
  gcc_install="$POLYGLOT_LOCAL_DIR/toolchain/$target/gcc-musl-$(lock_value gcc-musl "$target" version)"
  loader_path="$gcc_install/$loader"
  [[ -x $loader_path ]] || { printf 'error: pinned toolchain is not installed; run ./repo.sh bootstrap\n' >&2; return 1; }
  env_bin="$POLYGLOT_LOCAL_DIR/bin"
  export CC="${gcc%g++}gcc"
  export CXX=$gcc
  write_repo_tool_wrappers "$POLYGLOT_LOCAL_DIR" "$CC" "$CXX" "$python" "$loader_path" "$deno" "$go" "$buck2" "$clang_format" "$gcc_install" "$target" "$shellcheck"
  export POLYGLOT_CXX=$gcc
  export POLYGLOT_PYTHON=$python
  export POLYGLOT_DENO=$deno
  export POLYGLOT_GO=$go
  export POLYGLOT_BUCK2=$buck2
  export POLYGLOT_CLANG_FORMAT=$env_bin/clang-format
  export POLYGLOT_SHELLCHECK=$env_bin/shellcheck
  export GOROOT=$go_root
  export GOPATH="$POLYGLOT_LOCAL_DIR/cache/go/path"
  export GOMODCACHE="$POLYGLOT_LOCAL_DIR/cache/go/mod"
  export GOCACHE="$POLYGLOT_LOCAL_DIR/cache/go/build"
  export GOBIN="$POLYGLOT_LOCAL_DIR/bin"
  export GOTOOLCHAIN=local
  export CGO_ENABLED=0
  export GOEXPERIMENT=jsonv2
  # -p is host-core-count parallelism for direct `./repo.sh go ...`/`go-build`/
  # `go-test` invocations outside buck2 (which schedules its own graph). There
  # is no cross-lane job budget to split any more (that was the retired
  # task-runner's jobserver-fed concurrency knob) - this is simply
  # host_jobs(). Assign before exporting: `export X=$(f)` is a builtin
  # invocation whose own status masks f's, so a rejected POLYGLOT_JOBS would
  # otherwise leave a malformed `-p=` behind and continue.
  jobs=$(host_jobs)
  export GOFLAGS="-p=$jobs"
  export DENO_DIR="$POLYGLOT_LOCAL_DIR/cache/deno"
  export XDG_CACHE_HOME="$POLYGLOT_LOCAL_DIR/cache/xdg"
  export PYTHONPATH="$ROOT/build/python/$target/lib:$ROOT/python/lib:$ROOT/python/app"
  path_prefix="$env_bin:$gcc_bin:$go_root/bin:$(dirname -- "$deno"):$(dirname -- "$buck2")"
  export POLYGLOT_PATH_PREFIX=$path_prefix
  export PATH="$path_prefix:$PATH"
}

host_jobs() {
  local jobs=${POLYGLOT_JOBS:-}
  if [[ -z $jobs ]]; then jobs=$(getconf _NPROCESSORS_ONLN 2>/dev/null || printf '1\n'); fi
  [[ $jobs =~ ^[1-9][0-9]*$ ]] || { printf 'error: POLYGLOT_JOBS must be a positive integer\n' >&2; return 2; }
  printf '%s\n' "$jobs"
}

# Prints `--target-platforms //config:<target>-opt` (as separate args, one per
# line) when profile is opt, nothing for dbg (every buck2 rule here already
# defaults to dbg via its own default_target_platform - see config/defs.bzl).
target_platform_args() {
  local profile=$1 target
  [[ $profile == opt ]] || return 0
  target=$("$ROOT/toolchain/target.sh")
  printf -- '--target-platforms\n//config:%s-opt\n' "$target"
}

stage_python_extensions() {
  local target stage parent manifest manifest_tmp tmp query_out query_err output_out output_err entry output name rel hash
  local -a plat=("$@") extensions outputs
  target=$("$ROOT/toolchain/target.sh")
  stage="$ROOT/build/python/$target/lib"
  parent=$(dirname -- "$stage")
  manifest="$parent/.lib.manifest"
  query_out=$(scratch_file)
  query_err=$(scratch_file)
  if ! "$POLYGLOT_BUCK2" uquery "kind('^_py_extension_rule$', '//...')" >"$query_out" 2>"$query_err"; then
    cat "$query_err" >&2
    rm -f "$query_out" "$query_err"
    return 1
  fi
  mapfile -t extensions < <(grep '^root//' "$query_out" | sort)
  rm -f "$query_out" "$query_err"
  if ((${#extensions[@]} == 0)); then
    rm -rf "$stage" "$manifest"
    return 0
  fi
  output_out=$(scratch_file)
  output_err=$(scratch_file)
  if ! "$POLYGLOT_BUCK2" build "${plat[@]}" --show-output "${extensions[@]}" >"$output_out" 2>"$output_err"; then
    cat "$output_err" >&2
    rm -f "$output_out" "$output_err"
    return 1
  fi
  mapfile -t outputs < <(awk '{ print $2 }' "$output_out" | sort -u)
  rm -f "$output_out" "$output_err"
  ((${#outputs[@]} == ${#extensions[@]})) || { printf 'error: extension output query returned an incomplete result\n' >&2; return 1; }
  mkdir -p "$parent"
  manifest_tmp=$(mktemp "$parent/.lib.manifest.tmp.XXXXXX")
  for output in "${outputs[@]}"; do
    [[ -d $ROOT/$output ]] || { printf 'error: Python extension output is not a directory: %s\n' "$output" >&2; rm -f "$manifest_tmp"; return 1; }
    while IFS= read -r -d '' entry; do
      rel=${entry#"$ROOT/$output/"}
      if [[ -L $entry ]]; then
        printf 'link %s %s\n' "$rel" "$(readlink -- "$entry")"
      else
        hash=$(sha256sum "$entry" | awk '{ print $1 }')
        printf 'file %s %s\n' "$rel" "$hash"
      fi
    done < <(find "$ROOT/$output" \( -type f -o -type l \) -print0 | sort -z)
  done >"$manifest_tmp"
  if [[ -d $stage ]] && cmp -s "$manifest_tmp" "$manifest"; then
    rm -f "$manifest_tmp"
    return 0
  fi
  tmp=$(mktemp -d "$parent/.lib.tmp.XXXXXX")
  for output in "${outputs[@]}"; do
    for entry in "$ROOT/$output"/*; do
      name=$(basename -- "$entry")
      [[ -e "$tmp/$name" ]] && { printf 'error: Python extension package collision at %s\n' "$name" >&2; rm -rf "$tmp"; rm -f "$manifest_tmp"; return 1; }
    done
    cp -R "$ROOT/$output/." "$tmp"
  done
  rm -rf "$stage"
  mv "$tmp" "$stage"
  mv "$manifest_tmp" "$manifest"
}

# Package publication is intentionally Buck2-owned. A catalog entry is
# release metadata, not evidence that this checkout knows how to build its
# executable payload: only a //packages:<name> package() target supplies that
# evidence.  Keep the target probe in one place so `package` and CI's early
# tag gate fail with the same actionable error instead of falling through to
# the legacy raw-build-dir assembler (which repo.sh never populated).
require_buck_package_target() {
  local name=$1 probe_err probe_status
  probe_err=$(scratch_file)
  if "$POLYGLOT_BUCK2" targets "//packages:$name" >/dev/null 2>"$probe_err"; then
    rm -f "$probe_err"
    return 0
  else
    probe_status=$?
  fi
  if grep -q '^Unknown target `' "$probe_err"; then
    rm -f "$probe_err"
    printf 'error: package %q has no //packages:%s build target; catalog-only packages cannot be assembled or released by this repository\n' "$name" "$name" >&2
    return 2
  fi
  printf 'error: buck2 targets probe for //packages:%s failed:\n' "$name" >&2
  cat "$probe_err" >&2
  rm -f "$probe_err"
  return "$probe_status"
}

usage() {
  cat <<'EOF'
Usage: ./repo.sh <command> [options]

Commands:
  shell                        Start an interactive shell (the default).
  exec <command> [args...]     Run a command in the pinned repository environment.
  help                         Show this help.
  target                       Print the CPU-native musl output triplet.
  bootstrap [--offline] [--dry-run] [--repair]
                               Install the locked repo-local toolchain; --repair
                               reinstalls every locked artifact after probing it.
  doctor [--deep]              Validate target and installed tools.
  toolchain-lock [--check]     Regenerate, or verify, toolchains/lock.bzl.
  toolchain-qualify [--offline]
                               Regenerate the derived lock, reinstall and probe
                               every locked artifact, then run deep doctor.
  buck2 [args...]              Run the pinned Buck2 binary directly.
  infra-lint                   Check shell/Python infra scripts outside the buck2 graph.
  format [--check]             Apply, or check, repository formatting for every lane.
  lint                         Run buck2's lint-as-test targets plus infra-lint.
  build [dbg|opt]              Build every lane's primary outputs (discovered by rule kind).
  coverage                     Build the merged dbg coverage report
                               (bxl/coverage.bxl) and render a browsable
                               single-file HTML report next to it.
  test [dbg|opt]               Test every language target in the selected profile (default: dbg).
  compile-commands [dbg|opt]   Materialize compile_commands.json via the BXL compdb.
  cpp-build [dbg|opt]          Build the C++ hello binary via buck2.
  cpp-run [dbg|opt]            Run the C++ app through buck2.
  cpp-test                     Build, run, and test the C++ lane via buck2.
  python [args...]             Run the pinned Python through the pinned musl loader.
  python-build                 Build the pinned-ABI C++ extension via buck2.
  python-test                  Build and test the Python lane via buck2.
  deno [args...]               Run raw pinned Deno arguments; they may access the network.
  ts-build                     Type-check TypeScript against the frozen graph via buck2.
  ts-test                      Build and test local TypeScript via buck2.
  tsweb-build                  Build the React 19 static application via buck2.
  tsweb-test                   Build and test browser TypeScript via buck2.
  go [args...]                 Run raw pinned Go arguments; they may access the network.
  go-build                     Build the Go application via buck2.
  go-test                      Build, run, and test the Go lane via buck2.
  init-project <name> [org]    Initialize a new project from this template with custom names.
  infra-test                   Run repository infrastructure tests (bootstrap/workflow/package).
  package-list                 List declared package identity, targets, and executables.
  package-explain <name>       Show catalog identity, runtime closure, and Buck target status.
  package-validate             Validate package and runtime closure metadata.
  package-resolve <name> [out] Resolve an exact native-target package closure.
  package-target-check <name>  Require a Buck2 package target suitable for release.
  package <name> [dbg|opt]     Assemble a deterministic package from build outputs.
  package-smoke <archive> <name>
                               Verify a packaged artifact and exact runtime closure.
  release-check <name> <tag>   Check package tag, changelog, archive, and smoke test.
  release-notes <tag>          Print release notes for an exact changelog tag.
  ci                           Run every gate CI runs: deep doctor, lint,
                               package validation, infra tests, build, test,
                               and coverage.

Bootstrap is the only command allowed to fetch toolchain artifacts. Builds must
use tools beneath .local/toolchain and never fall back to host compilers.
EOF
}

command=${1:-shell}
if (($#)); then shift; fi

require_no_args() {
  local command=$1
  (($# == 1)) || { printf 'usage: ./repo.sh %s\n' "$command" >&2; exit 2; }
}

pinned_python() {
  "$POLYGLOT_LOCAL_DIR/bin/python3" "$@"
}

# toolchain-lock regenerates the derived Starlark lock, and `toolchain-qualify`
# runs it BEFORE bootstrap - so it is the one repository script that can be
# asked to run before a pinned interpreter exists. Prefer the pinned
# interpreter whenever it is installed (the hermeticity rule) and fall back to
# the host only to break that ordering cycle; gen_toolchain_lock.py is pure
# stdlib and version-insensitive.
lock_python() {
  if [[ -x $POLYGLOT_LOCAL_DIR/bin/python3 ]]; then
    "$POLYGLOT_LOCAL_DIR/bin/python3" "$@"
  else
    python3 "$@"
  fi
}

case "$command" in
  shell)
    (($# == 0)) || { printf 'usage: ./repo.sh\n' >&2; exit 2; }
    setup_environment
    restore_invocation_cwd
    exec bash --noprofile --rcfile "$ROOT/toolchain/repo-shell.bashrc" -i
    ;;
  exec)
    (($# >= 1)) || { printf 'usage: ./repo.sh exec <command> [args...]\n' >&2; exit 2; }
    setup_environment
    restore_invocation_cwd
    exec "$@"
    ;;
  help|-h|--help)
    require_no_args "$command" "$@"
    usage
    ;;
  target)
    require_no_args target "$@"
    "$ROOT/toolchain/target.sh"
    ;;
  bootstrap)
    (($# <= 3)) || { printf 'usage: ./repo.sh bootstrap [--offline] [--dry-run] [--repair]\n' >&2; exit 2; }
    "$ROOT/toolchain/bootstrap.sh" "$@"
    ;;
  doctor) "$ROOT/toolchain/doctor.sh" "$@" ;;
  toolchain-lock)
    (($# <= 1)) || { printf 'usage: ./repo.sh toolchain-lock [--check]\n' >&2; exit 2; }
    case ${1:-} in
      '') lock_python "$ROOT/tools/gen_toolchain_lock.py" --lock "$ROOT/tools.lock.toml" --output "$ROOT/toolchains/lock.bzl" ;;
      --check) lock_python "$ROOT/tools/gen_toolchain_lock.py" --lock "$ROOT/tools.lock.toml" --output "$ROOT/toolchains/lock.bzl" --check ;;
      *) printf 'usage: ./repo.sh toolchain-lock [--check]\n' >&2; exit 2 ;;
    esac
    ;;
  toolchain-qualify)
    (($# <= 1)) || { printf 'usage: ./repo.sh toolchain-qualify [--offline]\n' >&2; exit 2; }
    case ${1:-} in
      '') offline=() ;;
      --offline) offline=(--offline) ;;
      *) printf 'usage: ./repo.sh toolchain-qualify [--offline]\n' >&2; exit 2 ;;
    esac
    "$ROOT/repo.sh" toolchain-lock
    "$ROOT/repo.sh" bootstrap "${offline[@]}" --repair
    "$ROOT/repo.sh" doctor --deep
    ;;
  buck2)
    setup_environment
    restore_invocation_cwd
    exec "$POLYGLOT_BUCK2" "$@"
    ;;
  infra-lint)
    require_no_args infra-lint "$@"
    setup_environment
    # `bash -n f1 f2 ...` parses ONLY f1 and treats the rest as positional
    # parameters, so every file after the first went unchecked. One process
    # per file is the only way to actually gate them all.
    mapfile -t infra_scripts < <(printf '%s\n' "$ROOT/repo.sh" "$ROOT"/.vscode/go "$ROOT"/toolchain/*.sh "$ROOT"/test/*.sh)
    for script in "${infra_scripts[@]}"; do
      bash -n "$script" || { printf 'error: shell syntax check failed: %s\n' "$script" >&2; exit 1; }
    done
    # Pinned in tools.lock.toml and installed by bootstrap, so this is a
    # MANDATORY gate, not the opportunistic `command -v` check it used to be.
    # That version ran only where the host happened to provide the binary -
    # CI, essentially never a developer machine - so its findings appeared
    # for the first time after a push. A statically linked 2.3MB download is
    # a small price for a gate that fails where the code is written.
    #
    # NOTE for anyone editing comments in this repository's shell scripts: a
    # comment whose first word is "shellcheck" is parsed as a DIRECTIVE, and
    # an unrecognized directive key is a hard error (SC1072/SC1073), not a
    # warning. That is exactly how prose here once broke CI.
    "$POLYGLOT_SHELLCHECK" --severity=error --shell=bash -- "${infra_scripts[@]}"
    pinned_python "$ROOT/tools/lint.py"
    ;;
  format)
    (($# <= 1)) || { printf 'usage: ./repo.sh format [--check]\n' >&2; exit 2; }
    format_check=0
    case ${1:-} in
      '') ;;
      --check) format_check=1 ;;
      *) printf 'usage: ./repo.sh format [--check]\n' >&2; exit 2 ;;
    esac
    setup_environment
    if ((format_check)); then
      pinned_python "$ROOT/tools/format.py" --check
    else
      pinned_python "$ROOT/tools/format.py"
    fi
    if ((format_check)); then
      "$POLYGLOT_DENO" fmt --check ts tsweb deno.json
      # -r: with an empty Go lane xargs would otherwise run gofmt with no
      # arguments, which reads stdin (hangs on a tty, passes vacuously in CI).
      mapfile -t format_go < <(find "$ROOT/go" -name '*.go' -type f -print0 | xargs -0 -r "$GOROOT/bin/gofmt" -l)
      ((${#format_go[@]} == 0)) || { printf 'gofmt: files not canonically formatted:\n%s\nrun: ./repo.sh format\n' "${format_go[*]}" >&2; exit 1; }
    else
      "$POLYGLOT_DENO" fmt ts tsweb deno.json
      find "$ROOT/go" -name '*.go' -type f -print0 | xargs -0 -r "$GOROOT/bin/gofmt" -w
    fi
    ;;
  lint)
    (($# == 0)) || { printf 'usage: ./repo.sh lint\n' >&2; exit 2; }
    "$ROOT/repo.sh" infra-lint
    "$ROOT/repo.sh" format --check
    setup_environment
    "$POLYGLOT_BUCK2" test //... --labels lint
    ;;
  build)
    profile=${1:-dbg}
    (($# <= 1)) || { printf 'usage: ./repo.sh build [dbg|opt]\n' >&2; exit 2; }
    [[ $profile == dbg || $profile == opt ]] || { printf 'error: profile must be dbg or opt\n' >&2; exit 2; }
    setup_environment
    mapfile -t plat < <(target_platform_args "$profile")
    # No hand-listed //:build group: the graph is the list. Discover every
    # lane's primary build output by rule kind, so a new app/site target
    # participates by existing (see the root BUCK file's header comment).
    # buck2's stderr goes to a file, surfaced only on failure: uquery's
    # progress noise would otherwise pollute every build, but swallowing
    # it outright turns a real Starlark/daemon error into an unexplained
    # "no buildable targets discovered".
    uquery_err=$(scratch_file)
    mapfile -t buildables < <("$POLYGLOT_BUCK2" uquery \
      "kind('^_(cxx_binary|go_binary|py_binary|deno_check|vite_build)_rule$', '//...')" 2>"$uquery_err" | grep '^root//')
    ((${#buildables[@]} > 0)) || { cat "$uquery_err" >&2; rm -f "$uquery_err"; printf 'error: no buildable targets discovered\n' >&2; exit 1; }
    rm -f "$uquery_err"
    "$POLYGLOT_BUCK2" build "${plat[@]}" "${buildables[@]}"
    stage_python_extensions "${plat[@]}"
    [[ ${POLYGLOT_DEFER_COMPDB:-0} == 1 ]] || "$ROOT/repo.sh" compile-commands "$profile"
    ;;
  coverage)
    (($# == 0)) || { printf 'usage: ./repo.sh coverage\n' >&2; exit 2; }
    setup_environment
    # bxl/coverage.bxl prints the ensured merged.lcov path then the
    # summary.txt path. Nothing used to read either, so the merged total could
    # fall to 0% with every gate still green - gate it here, on the same
    # summary CI uploads. rules/coverage.bzl owns the merge action's argv, so
    # the floor is enforced from this side of the graph rather than by passing
    # --min-total into the action.
    cov_out=$("$POLYGLOT_BUCK2" bxl -M all //bxl:coverage.bxl:coverage)
    printf '%s\n' "$cov_out"
    cov_summary=$(printf '%s\n' "$cov_out" | grep -E 'summary\.txt$' | tail -n1)
    [[ -n $cov_summary && -f $cov_summary ]] || {
      printf 'error: coverage bxl did not report a summary file: %s\n' "$cov_out" >&2; exit 1;
    }
    cov_lcov=$(printf '%s\n' "$cov_out" | grep -E 'merged\.lcov$' | tail -n1)
    [[ -n $cov_lcov && -f $cov_lcov ]] || {
      printf 'error: coverage bxl did not report a merged lcov file: %s\n' "$cov_out" >&2; exit 1;
    }
    # The browsable report is rendered OUTSIDE the graph, on purpose. It is a
    # view of merged.lcov plus the working-tree sources, not a build input:
    # nothing depends on it, and making it a buck action would mean declaring
    # every repo source as an input to the merge just to annotate them.
    # Rendered before the floor check so a FAILING run still leaves a report
    # explaining which lines are missing - that is exactly when it is wanted.
    cov_html=${POLYGLOT_COVERAGE_HTML:-$ROOT/buck-out/coverage-report/coverage.html}
    pinned_python "$ROOT/tools/coverage_html.py" "$cov_lcov" "$cov_html" \
      --root "$ROOT" --title 'Coverage report' \
      ${POLYGLOT_COVERAGE_MARKDOWN:+--markdown "$POLYGLOT_COVERAGE_MARKDOWN"} \
      ${POLYGLOT_COVERAGE_HTML_LINK:+--html-link "$POLYGLOT_COVERAGE_HTML_LINK"}
    pinned_python "$ROOT/tools/coverage_merge.py" --check-total "$cov_summary" \
      --min-total "${POLYGLOT_COVERAGE_MIN:-90.0}"
    ;;
  test)
    (($# <= 1)) || { printf 'usage: ./repo.sh test [dbg|opt]\n' >&2; exit 2; }
    profile=${1:-dbg}
    [[ $profile == dbg || $profile == opt ]] || { printf 'error: profile must be dbg or opt\n' >&2; exit 2; }
    setup_environment
    mapfile -t plat < <(target_platform_args "$profile")
    "$POLYGLOT_BUCK2" test "${plat[@]}" //...
    [[ ${POLYGLOT_DEFER_COMPDB:-0} == 1 ]] || "$ROOT/repo.sh" compile-commands "$profile"
    ;;
  compile-commands)
    (($# <= 1)) || { printf 'usage: ./repo.sh compile-commands [dbg|opt]\n' >&2; exit 2; }
    profile=${1:-${POLYGLOT_CPP_PROFILE:-dbg}}
    [[ $profile == dbg || $profile == opt ]] || { printf 'error: profile must be dbg or opt\n' >&2; exit 2; }
    setup_environment
    mapfile -t plat < <(target_platform_args "$profile")
    bxl_err=$(scratch_file)
    if ! out=$("$POLYGLOT_BUCK2" bxl -M all "${plat[@]}" //bxl:compdb.bxl:compdb 2>"$bxl_err"); then
      cat "$bxl_err" >&2; rm -f "$bxl_err"
      printf 'error: compdb bxl failed\n' >&2; exit 1
    fi
    rm -f "$bxl_err"
    out_file=$(printf '%s\n' "$out" | grep -E '^buck-out/.*\.json$' | tail -n1)
    if [[ -z $out_file || ! -f $out_file ]]; then
      out=$("$POLYGLOT_BUCK2" bxl -M all --no-remote-cache "${plat[@]}" //bxl:compdb.bxl:compdb 2>/dev/null || true)
      out_file=$(printf '%s\n' "$out" | grep -E '^buck-out/.*\.json$' | tail -n1)
    fi
    [[ -n $out_file && -f $out_file ]] || { printf 'error: compdb bxl output file not found in stdout: %s\n' "$out" >&2; exit 1; }
    tmp="$ROOT/.compile_commands.json.tmp.$$"
    cp -- "$out_file" "$tmp"
    mv -- "$tmp" "$ROOT/compile_commands.json"
    ;;
  cpp-build)
    (($# <= 1)) || { printf 'usage: ./repo.sh cpp-build [dbg|opt]\n' >&2; exit 2; }
    profile=${1:-${POLYGLOT_CPP_PROFILE:-dbg}}
    [[ $profile == dbg || $profile == opt ]] || { printf 'error: profile must be dbg or opt\n' >&2; exit 2; }
    setup_environment
    mapfile -t plat < <(target_platform_args "$profile")
    "$POLYGLOT_BUCK2" build "${plat[@]}" //cpp/app/hello:hello
    [[ ${POLYGLOT_DEFER_COMPDB:-0} == 1 ]] || "$ROOT/repo.sh" compile-commands "$profile"
    ;;
  cpp-run)
    (($# <= 1)) || { printf 'usage: ./repo.sh cpp-run [dbg|opt]\n' >&2; exit 2; }
    profile=${1:-dbg}
    [[ $profile == dbg || $profile == opt ]] || { printf 'error: profile must be dbg or opt\n' >&2; exit 2; }
    setup_environment
    mapfile -t plat < <(target_platform_args "$profile")
    "$POLYGLOT_BUCK2" run "${plat[@]}" //cpp/app/hello:hello
    ;;
  cpp-test)
    require_no_args cpp-test "$@"
    setup_environment
    "$POLYGLOT_BUCK2" test //cpp/...
    "$ROOT/repo.sh" cpp-run dbg
    ;;
  python)
    target=$("$ROOT/toolchain/target.sh")
    export POLYGLOT_LOCK_FILE=${POLYGLOT_LOCK_FILE:-$ROOT/tools.lock.toml}
    . "$ROOT/toolchain/lock.sh"
    gcc_version=$(lock_value gcc-musl "$target" version)
    loader=$(lock_value gcc-musl "$target" loader)
    python_version=$(lock_value python "$target" version)
    python_expected=$(lock_value python "$target" expected)
    gcc_install="$POLYGLOT_LOCAL_DIR/toolchain/$target/gcc-musl-$gcc_version"
    python_install="$POLYGLOT_LOCAL_DIR/toolchain/$target/python-$python_version"
    [[ -x $gcc_install/$loader && -x $python_install/$python_expected ]] || { printf 'error: pinned Python toolchain is not installed\n' >&2; exit 1; }
    loader_dir=$(dirname -- "$gcc_install/$loader")
    build_python="$ROOT/build/python/$target/lib"
    export PYTHONPATH="$build_python:$ROOT/python/lib:$ROOT/python/app"
    restore_invocation_cwd
    "$gcc_install/$loader" --library-path "$loader_dir:$python_install/python/lib" "$python_install/$python_expected" "$@"
    ;;
  python-build)
    require_no_args python-build "$@"
    setup_environment
    "$POLYGLOT_BUCK2" build //python/app/hello:hello
    # The application target brings in its Python dependencies, while this
    # generic helper materializes every declared extension for direct runtime
    # imports under the target-specific PYTHONPATH root.
    stage_python_extensions
    [[ ${POLYGLOT_DEFER_COMPDB:-0} == 1 ]] || "$ROOT/repo.sh" compile-commands "${POLYGLOT_CPP_PROFILE:-dbg}"
    ;;
  python-test)
    require_no_args python-test "$@"
    setup_environment
    "$POLYGLOT_BUCK2" test //python/...
    ;;
  deno)
    deno=$(tool_path deno)
    [[ -x $deno ]] || { printf 'error: pinned Deno is not installed; run ./repo.sh bootstrap\n' >&2; exit 1; }
    export DENO_DIR="$POLYGLOT_LOCAL_DIR/cache/deno"
    restore_invocation_cwd
    exec "$deno" "$@"
    ;;
  ts-build)
    require_no_args ts-build "$@"
    setup_environment
    "$POLYGLOT_BUCK2" build //ts/app/hello:check //ts/app/server:check
    ;;
  ts-test)
    require_no_args ts-test "$@"
    setup_environment
    "$POLYGLOT_BUCK2" test //ts/...
    ;;
  tsweb-build)
    require_no_args tsweb-build "$@"
    setup_environment
    "$POLYGLOT_BUCK2" build //tsweb:site
    ;;
  tsweb-test)
    require_no_args tsweb-test "$@"
    setup_environment
    "$POLYGLOT_BUCK2" test //tsweb/...
    ;;
  go)
    target=$("$ROOT/toolchain/target.sh")
    export POLYGLOT_LOCK_FILE=${POLYGLOT_LOCK_FILE:-$ROOT/tools.lock.toml}
    . "$ROOT/toolchain/lock.sh"
    go_version=$(lock_value go "$target" version)
    go_install="$POLYGLOT_LOCAL_DIR/toolchain/$target/go-$go_version/go"
    [[ -x $go_install/bin/go ]] || { printf 'error: pinned Go is not installed; run ./repo.sh bootstrap\n' >&2; exit 1; }
    export GOROOT="$go_install"
    export GOPATH="$POLYGLOT_LOCAL_DIR/cache/go/path"
    export GOMODCACHE="$POLYGLOT_LOCAL_DIR/cache/go/mod"
    export GOCACHE="$POLYGLOT_LOCAL_DIR/cache/go/build"
    export GOBIN="$POLYGLOT_LOCAL_DIR/bin"
    export GOTOOLCHAIN=local
    export CGO_ENABLED=0
    export GOEXPERIMENT=jsonv2
    # See setup_environment: assign first so host_jobs' rejection status is
    # not masked by the `export` builtin's own success.
    go_jobs=$(host_jobs)
    export GOFLAGS="-p=$go_jobs"
    export PATH="$GOROOT/bin:$PATH"
    restore_invocation_cwd
    exec "$GOROOT/bin/go" "$@"
    ;;
  go-build)
    require_no_args go-build "$@"
    setup_environment
    "$POLYGLOT_BUCK2" build //go/app/hello:hello
    ;;
  go-test)
    require_no_args go-test "$@"
    setup_environment
    "$POLYGLOT_BUCK2" test //go/...
    ;;
  package-validate)
    require_no_args package-validate "$@"
    setup_environment
    "$POLYGLOT_BUCK2" test //packages:manifest-validate
    ;;
  init-project)
    (($# >= 1 && $# <= 2)) || { printf 'usage: ./repo.sh init-project <new-project-name> [new-org-name]\n' >&2; exit 2; }
    setup_environment
    pinned_python "$ROOT/tools/init_project.py" "$@"
    ;;
  package-list)
    (($# == 0)) || { printf 'usage: ./repo.sh package-list\n' >&2; exit 2; }
    setup_environment
    pinned_python "$ROOT/tools/package_model.py" --catalog "$ROOT/packages/catalog.bzl" \
      --tools-lock "$ROOT/tools.lock.toml" list
    ;;
  package-resolve)
    (($# >= 1 && $# <= 2)) || { printf 'usage: ./repo.sh package-resolve <name> [out-dir]\n' >&2; exit 2; }
    target=$("$ROOT/toolchain/target.sh")
    setup_environment
    args=(--catalog "$ROOT/packages/catalog.bzl" --tools-lock "$ROOT/tools.lock.toml" resolve --package "$1" --target "$target")
    [[ ${2:-} ]] && args+=(--out-dir "$(caller_path "$2")")
    pinned_python "$ROOT/tools/package_model.py" "${args[@]}"
    ;;
  package)
    (($# >= 1 && $# <= 2)) || { printf 'usage: ./repo.sh package <name> [dbg|opt]\n' >&2; exit 2; }
    profile=${2:-opt}
    [[ $profile == dbg || $profile == opt ]] || { printf 'error: profile must be dbg or opt\n' >&2; exit 2; }
    target=$("$ROOT/toolchain/target.sh")
    setup_environment
    require_buck_package_target "$1"
    version=$(pinned_python "$ROOT/tools/package_model.py" --catalog "$ROOT/packages/catalog.bzl" \
      --tools-lock "$ROOT/tools.lock.toml" resolve \
      --package "$1" --target "$target" | pinned_python -c 'import json, sys; print(json.load(sys.stdin)["version"])')
    mapfile -t plat < <(target_platform_args "$profile")
    # Mirror every other buck2 call here: stderr to a scratch file, surfaced
    # only on failure. Swallowing it turned a real build error into an
    # unexplained `cp` failure on an empty path.
    package_err=$(scratch_file)
    if ! out=$("$POLYGLOT_BUCK2" build "${plat[@]}" --show-output "//packages:$1" 2>"$package_err" | awk '{ print $2 }'); then
      cat "$package_err" >&2
      printf 'error: buck2 build of //packages:%s failed\n' "$1" >&2; exit 1
    fi
    # --show-output prints one line per built output; anything but exactly one
    # makes the `cp` below fail obscurely on a multi-line path.
    [[ $(printf '%s\n' "$out" | grep -c .) == 1 ]] || {
      cat "$package_err" >&2
      printf 'error: buck2 did not report exactly one output for //packages:%s: %s\n' "$1" "$out" >&2; exit 1
    }
    mkdir -p "$ROOT/dist"
    archive="$ROOT/dist/$1-$version-$target.tar.gz"
    tmp="$archive.tmp.$$"
    cp -- "$ROOT/$out" "$tmp"
    mv -- "$tmp" "$archive"
    pinned_python "$ROOT/tools/release_evidence.py" package \
      --archive "$archive" --catalog "$ROOT/packages/catalog.bzl" \
      --tools-lock "$ROOT/tools.lock.toml" --package "$1" --target "$target" --profile "$profile"
    printf '%s\n' "$archive"
    ;;
  package-target-check)
    (($# == 1)) || { printf 'usage: ./repo.sh package-target-check <name>\n' >&2; exit 2; }
    setup_environment
    require_buck_package_target "$1"
    ;;
  package-explain)
    (($# == 1)) || { printf 'usage: ./repo.sh package-explain <name>\n' >&2; exit 2; }
    target=$("$ROOT/toolchain/target.sh")
    setup_environment
    pinned_python "$ROOT/tools/package_model.py" --catalog "$ROOT/packages/catalog.bzl" \
      --tools-lock "$ROOT/tools.lock.toml" resolve --package "$1" --target "$target"
    if "$POLYGLOT_BUCK2" targets "//packages:$1" >/dev/null 2>&1; then
      printf 'buck_target=//packages:%s\n' "$1"
    else
      printf 'buck_target=missing\n'
    fi
    ;;
  package-smoke)
    (($# == 2)) || { printf 'usage: ./repo.sh package-smoke <archive> <name>\n' >&2; exit 2; }
    target=$("$ROOT/toolchain/target.sh")
    setup_environment
    pinned_python "$ROOT/tools/package_release.py" --root "$ROOT" --catalog "$ROOT/packages/catalog.bzl" \
      --tools-lock "$ROOT/tools.lock.toml" \
      --dist-dir "$ROOT/dist" --changelog "$ROOT/CHANGELOG.md" smoke \
      --archive "$(caller_path "$1")" --package "$2" --target "$target" --execute
    ;;
  release-check)
    (($# == 2)) || { printf 'usage: ./repo.sh release-check <name> <tag>\n' >&2; exit 2; }
    target=$("$ROOT/toolchain/target.sh")
    setup_environment
    pinned_python "$ROOT/tools/package_release.py" --root "$ROOT" --catalog "$ROOT/packages/catalog.bzl" \
      --tools-lock "$ROOT/tools.lock.toml" \
      --dist-dir "$ROOT/dist" --changelog "$ROOT/CHANGELOG.md" release-check \
      --package "$1" --target "$target" --tag "$2"
    ;;
  release-notes)
    (($# == 1)) || { printf 'usage: ./repo.sh release-notes <tag>\n' >&2; exit 2; }
    setup_environment
    pinned_python "$ROOT/tools/package_release.py" --root "$ROOT" --catalog "$ROOT/packages/catalog.bzl" \
      --tools-lock "$ROOT/tools.lock.toml" \
      --dist-dir "$ROOT/dist" --changelog "$ROOT/CHANGELOG.md" release-notes --tag "$1"
    ;;
  infra-test)
    require_no_args infra-test "$@"
    setup_environment
    bash "$ROOT/test/bootstrap-smoke.sh"
    bash "$ROOT/test/go-vendor-contract.sh"
    bash "$ROOT/test/workflow-contract.sh"
    bash "$ROOT/test/docs-contract.sh"
    pinned_python "$ROOT/test/editor-contract.py"
    pinned_python -m unittest discover -s "$ROOT/test" -p 'test_*.py'
    bash "$ROOT/test/test-package-model.sh"
    bash "$ROOT/test/test-package-release.sh"
    bash "$ROOT/test/graph-compdb-contract.sh"
    bash "$ROOT/test/deno-manifest-contract.sh"
    ;;
  ci)
    (($# == 0)) || { printf 'usage: ./repo.sh ci\n' >&2; exit 2; }
    # A strict superset of what .github/workflows/verify.yml runs, so a local
    # green result means the same thing CI's green result does. Keep this list
    # and verify.yml's job steps in lockstep.
    "$ROOT/repo.sh" doctor --deep
    "$ROOT/repo.sh" lint
    "$ROOT/repo.sh" package-validate
    "$ROOT/repo.sh" infra-test
    "$ROOT/repo.sh" build
    "$ROOT/repo.sh" test
    "$ROOT/repo.sh" coverage
    ;;
  *) printf 'error: unknown command: %s\n' "$command" >&2; usage >&2; exit 2 ;;
esac
