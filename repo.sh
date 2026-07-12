#!/usr/bin/env bash
set -euo pipefail

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd -P)
export POLYGLOT_ROOT=${POLYGLOT_ROOT:-$ROOT}
export POLYGLOT_LOCAL_DIR=${POLYGLOT_LOCAL_DIR:-$ROOT/.local}

tool_path() {
  local tool=$1 target version expected
  target=$("$ROOT/toolchain/target.sh")
  export POLYGLOT_LOCK_FILE=${POLYGLOT_LOCK_FILE:-$ROOT/tools.lock.toml}
  . "$ROOT/toolchain/lock.sh"
  version=$(lock_value "$tool" "$target" version)
  expected=$(lock_value "$tool" "$target" expected)
  printf '%s/toolchain/%s/%s-%s/%s\n' "$POLYGLOT_LOCAL_DIR" "$target" "$tool" "$version" "$expected"
}

usage() {
  cat <<'EOF'
Usage: ./repo.sh <command> [options]

Commands:
  help                         Show this help.
  target                       Print the CPU-native musl output triplet.
  bootstrap [--offline] [--dry-run]
                               Install the locked repo-local toolchain.
  doctor [--deep]              Validate target and installed tools.
  lint                         Check shell, Python, TOML, and JSON sources.
  native-configure [dbg|opt]   Generate Ninja and compile_commands.json.
  native-build [dbg|opt]       Build with the pinned repo-local GCC and Ninja.
  native-run [dbg|opt]         Run the native example through the pinned musl loader.
  python [args...]             Run the pinned Python through the pinned musl loader.
  deno [args...]               Run the pinned Deno binary.
  deno-check                   Check, lint, and test the repository Deno lane.
  reflection-probe             Print the pinned GCC reflection probe command.
  package-validate             Validate package and runtime closure metadata.
  package-resolve <name> [out] Resolve an exact native-target package closure.
  package <name> [dbg|opt]     Assemble a deterministic package from build outputs.
  package-smoke <archive> <name>
                               Verify a packaged artifact and exact runtime closure.
  release-check <name> <tag>   Check package tag, changelog, archive, and smoke test.
  release-notes <tag>          Print release notes for an exact changelog tag.
  test                         Run bootstrap, package-model, and native tests.
  ci                           Run the complete pre-bootstrap validation slice.

Bootstrap is the only command allowed to fetch toolchain artifacts. Builds must
use tools beneath .local/toolchain and never fall back to host compilers.
EOF
}

command=${1:-help}
if (($#)); then shift; fi
case "$command" in
  help|-h|--help) usage ;;
  target) "$ROOT/toolchain/target.sh" "$@" ;;
  bootstrap) "$ROOT/toolchain/bootstrap.sh" "$@" ;;
  doctor) "$ROOT/toolchain/doctor.sh" "$@" ;;
  lint)
    bash -n "$ROOT/repo.sh" "$ROOT"/toolchain/*.sh "$ROOT"/test/*.sh
    python3 "$ROOT/tools/lint.py"
    ;;
  native-configure)
    profile=${1:-dbg}
    [[ $profile == dbg || $profile == opt ]] || { printf 'error: profile must be dbg or opt\n' >&2; exit 2; }
    target=$("$ROOT/toolchain/target.sh")
    cxx=$(tool_path gcc-musl)
    [[ -x $cxx ]] || { printf 'error: pinned compiler is not installed; run ./repo.sh bootstrap\n' >&2; exit 1; }
    python3 "$ROOT/tools/native_graph.py" configure --root "$ROOT" --profile "$profile" \
      --output "$ROOT/build/native/$target/$profile" --cxx "$cxx"
    ln -sfn "build/native/$target/$profile/compile_commands.json" "$ROOT/compile_commands.json"
    ;;
  native-build)
    profile=${1:-dbg}
    "$ROOT/repo.sh" native-configure "$profile"
    target=$("$ROOT/toolchain/target.sh")
    ninja=$(tool_path ninja)
    [[ -x $ninja ]] || { printf 'error: pinned Ninja is not installed; run ./repo.sh bootstrap\n' >&2; exit 1; }
    cxx=$(tool_path gcc-musl)
    (cd "$ROOT" && "$cxx" -std=gnu++26 -freflection -fsyntax-only native/probes/reflection.cpp)
    (cd "$ROOT" && "$ninja" -f "build/native/$target/$profile/build.ninja")
    ;;
  native-run)
    profile=${1:-dbg}
    [[ $profile == dbg || $profile == opt ]] || { printf 'error: profile must be dbg or opt\n' >&2; exit 2; }
    target=$("$ROOT/toolchain/target.sh")
    export POLYGLOT_LOCK_FILE=${POLYGLOT_LOCK_FILE:-$ROOT/tools.lock.toml}
    . "$ROOT/toolchain/lock.sh"
    version=$(lock_value gcc-musl "$target" version)
    loader=$(lock_value gcc-musl "$target" loader)
    install="$POLYGLOT_LOCAL_DIR/toolchain/$target/gcc-musl-$version"
    binary="$ROOT/build/native/$target/$profile/bin/hello"
    [[ -x $install/$loader && -x $binary ]] || { printf 'error: build and bootstrap the %s profile first\n' "$profile" >&2; exit 1; }
    loader_dir=$(dirname -- "$install/$loader")
    "$install/$loader" --library-path "$loader_dir" "$binary"
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
    "$gcc_install/$loader" --library-path "$loader_dir:$python_install/python/lib" "$python_install/$python_expected" "$@"
    ;;
  deno)
    deno=$(tool_path deno)
    [[ -x $deno ]] || { printf 'error: pinned Deno is not installed; run ./repo.sh bootstrap\n' >&2; exit 1; }
    export DENO_DIR="$POLYGLOT_LOCAL_DIR/cache/deno"
    exec "$deno" "$@"
    ;;
  deno-check)
    "$ROOT/repo.sh" deno --version
    "$ROOT/repo.sh" deno check --frozen deno/main.ts deno/main_test.ts
    "$ROOT/repo.sh" deno lint deno/
    "$ROOT/repo.sh" deno test --frozen deno/
    ;;
  reflection-probe)
    cxx=$(tool_path gcc-musl)
    [[ -x $cxx ]] || { printf 'error: pinned compiler is not installed; run ./repo.sh bootstrap\n' >&2; exit 1; }
    python3 "$ROOT/tools/native_graph.py" reflection-probe --root "$ROOT" --cxx "$cxx"
    ;;
  package-validate)
    python3 "$ROOT/tools/package_model.py" --manifest "$ROOT/package.toml" \
      --lock "$ROOT/runtime-resolution.lock.toml" --tools-lock "$ROOT/tools.lock.toml" validate
    ;;
  package-resolve)
    (($# >= 1 && $# <= 2)) || { printf 'usage: ./repo.sh package-resolve <name> [out-dir]\n' >&2; exit 2; }
    target=$("$ROOT/toolchain/target.sh")
    args=(--manifest "$ROOT/package.toml" --lock "$ROOT/runtime-resolution.lock.toml" --tools-lock "$ROOT/tools.lock.toml" resolve --package "$1" --target "$target")
    [[ ${2:-} ]] && args+=(--out-dir "$2")
    python3 "$ROOT/tools/package_model.py" "${args[@]}"
    ;;
  package)
    (($# >= 1 && $# <= 2)) || { printf 'usage: ./repo.sh package <name> [dbg|opt]\n' >&2; exit 2; }
    profile=${2:-opt}
    [[ $profile == dbg || $profile == opt ]] || { printf 'error: profile must be dbg or opt\n' >&2; exit 2; }
    target=$("$ROOT/toolchain/target.sh")
    python3 "$ROOT/tools/package_release.py" --root "$ROOT" --manifest "$ROOT/package.toml" \
      --lock "$ROOT/runtime-resolution.lock.toml" --tools-lock "$ROOT/tools.lock.toml" \
      --dist-dir "$ROOT/dist" --changelog "$ROOT/CHANGELOG.md" package \
      --package "$1" --target "$target" --profile "$profile"
    ;;
  package-smoke)
    (($# == 2)) || { printf 'usage: ./repo.sh package-smoke <archive> <name>\n' >&2; exit 2; }
    target=$("$ROOT/toolchain/target.sh")
    python3 "$ROOT/tools/package_release.py" --root "$ROOT" --manifest "$ROOT/package.toml" \
      --lock "$ROOT/runtime-resolution.lock.toml" --tools-lock "$ROOT/tools.lock.toml" \
      --dist-dir "$ROOT/dist" --changelog "$ROOT/CHANGELOG.md" smoke \
      --archive "$1" --package "$2" --target "$target"
    ;;
  release-check)
    (($# == 2)) || { printf 'usage: ./repo.sh release-check <name> <tag>\n' >&2; exit 2; }
    target=$("$ROOT/toolchain/target.sh")
    python3 "$ROOT/tools/package_release.py" --root "$ROOT" --manifest "$ROOT/package.toml" \
      --lock "$ROOT/runtime-resolution.lock.toml" --tools-lock "$ROOT/tools.lock.toml" \
      --dist-dir "$ROOT/dist" --changelog "$ROOT/CHANGELOG.md" release-check \
      --package "$1" --target "$target" --tag "$2"
    ;;
  release-notes)
    (($# == 1)) || { printf 'usage: ./repo.sh release-notes <tag>\n' >&2; exit 2; }
    python3 "$ROOT/tools/package_release.py" --root "$ROOT" --manifest "$ROOT/package.toml" \
      --lock "$ROOT/runtime-resolution.lock.toml" --tools-lock "$ROOT/tools.lock.toml" \
      --dist-dir "$ROOT/dist" --changelog "$ROOT/CHANGELOG.md" release-notes --tag "$1"
    ;;
  test)
    bash "$ROOT/test/bootstrap-smoke.sh"
    bash "$ROOT/test/workflow-contract.sh"
    bash "$ROOT/test/test-package-model.sh"
    bash "$ROOT/test/test-package-release.sh"
    python3 -m unittest discover -s "$ROOT/native" -p 'test_*.py'
    ;;
  ci)
    "$ROOT/repo.sh" lint
    "$ROOT/repo.sh" package-validate
    "$ROOT/repo.sh" test
    ;;
  *) printf 'error: unknown command: %s\n' "$command" >&2; usage >&2; exit 2 ;;
esac
