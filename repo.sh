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
  cpp-configure [dbg|opt]      Generate Ninja and compile_commands.json.
  cpp-build [dbg|opt]          Build with the pinned repo-local GCC and Ninja.
  cpp-run [dbg|opt]            Run the C++ app through the pinned musl loader.
  cpp-test                     Test the C++ build graph and policy.
  python [args...]             Run the pinned Python through the pinned musl loader.
  python-build                 Build the pinned-ABI C++ extension.
  python-check                 Compile and test the Python lane.
  deno [args...]               Run the pinned Deno binary.
  ts-check                     Check, lint, and test local TypeScript.
  tsweb-check                  Check, lint, and test browser TypeScript.
  tsweb-build                  Build the React 19 static application.
  go [args...]                 Run pinned Go with isolated repository caches.
  go-check                     Format, vet, test, build, and run the Go lane.
  cpp-reflection-probe         Print the pinned GCC reflection probe command.
  package-validate             Validate package and runtime closure metadata.
  package-resolve <name> [out] Resolve an exact native-target package closure.
  package <name> [dbg|opt]     Assemble a deterministic package from build outputs.
  package-smoke <archive> <name>
                               Verify a packaged artifact and exact runtime closure.
  release-check <name> <tag>   Check package tag, changelog, archive, and smoke test.
  release-notes <tag>          Print release notes for an exact changelog tag.
  test                         Run repository infrastructure and C++ graph tests.
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
    bash -n "$ROOT/repo.sh" "$ROOT"/.vscode/go "$ROOT"/toolchain/*.sh "$ROOT"/test/*.sh
    python3 "$ROOT/tools/lint.py"
    ;;
  cpp-configure)
    profile=${1:-dbg}
    [[ $profile == dbg || $profile == opt ]] || { printf 'error: profile must be dbg or opt\n' >&2; exit 2; }
    target=$("$ROOT/toolchain/target.sh")
    cxx=$(tool_path gcc-musl)
    [[ -x $cxx ]] || { printf 'error: pinned compiler is not installed; run ./repo.sh bootstrap\n' >&2; exit 1; }
    python3 "$ROOT/tools/cpp_graph.py" configure --root "$ROOT" --profile "$profile" \
      --output "$ROOT/build/cpp/$target/$profile" --cxx "$cxx"
    ln -sfn "build/cpp/$target/$profile/compile_commands.json" "$ROOT/compile_commands.json"
    ;;
  cpp-build)
    profile=${1:-dbg}
    "$ROOT/repo.sh" cpp-configure "$profile"
    target=$("$ROOT/toolchain/target.sh")
    ninja=$(tool_path ninja)
    [[ -x $ninja ]] || { printf 'error: pinned Ninja is not installed; run ./repo.sh bootstrap\n' >&2; exit 1; }
    cxx=$(tool_path gcc-musl)
    (cd "$ROOT" && "$cxx" -std=gnu++26 -freflection -fsyntax-only cpp/test/reflection.cc)
    (cd "$ROOT" && "$ninja" -f "build/cpp/$target/$profile/build.ninja")
    ;;
  cpp-run)
    profile=${1:-dbg}
    [[ $profile == dbg || $profile == opt ]] || { printf 'error: profile must be dbg or opt\n' >&2; exit 2; }
    target=$("$ROOT/toolchain/target.sh")
    export POLYGLOT_LOCK_FILE=${POLYGLOT_LOCK_FILE:-$ROOT/tools.lock.toml}
    . "$ROOT/toolchain/lock.sh"
    version=$(lock_value gcc-musl "$target" version)
    loader=$(lock_value gcc-musl "$target" loader)
    install="$POLYGLOT_LOCAL_DIR/toolchain/$target/gcc-musl-$version"
    binary="$ROOT/build/cpp/$target/$profile/bin/hello"
    [[ -x $install/$loader && -x $binary ]] || { printf 'error: build and bootstrap the %s profile first\n' "$profile" >&2; exit 1; }
    loader_dir=$(dirname -- "$install/$loader")
    "$install/$loader" --library-path "$loader_dir" "$binary"
    ;;
  cpp-test)
    python3 -m unittest discover -s "$ROOT/cpp/test" -p 'test_*.py'
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
    export PYTHONPATH="$build_python:$ROOT/python/lib:$ROOT/python/app${PYTHONPATH:+:$PYTHONPATH}"
    "$gcc_install/$loader" --library-path "$loader_dir:$python_install/python/lib" "$python_install/$python_expected" "$@"
    ;;
  python-build)
    target=$("$ROOT/toolchain/target.sh")
    cxx=$(tool_path gcc-musl)
    [[ -x $cxx ]] || { printf 'error: pinned compiler is not installed; run ./repo.sh bootstrap\n' >&2; exit 1; }
    python3 "$ROOT/tools/python_build.py" --root "$ROOT" --target "$target" --cxx "$cxx"
    ;;
  python-check)
    "$ROOT/repo.sh" python-build
    "$ROOT/repo.sh" python -m compileall -q python/lib python/app python/test
    "$ROOT/repo.sh" python -m unittest discover -s python/test -p 'test_*.py'
    "$ROOT/repo.sh" python python/app/hello/main.py
    ;;
  deno)
    deno=$(tool_path deno)
    [[ -x $deno ]] || { printf 'error: pinned Deno is not installed; run ./repo.sh bootstrap\n' >&2; exit 1; }
    export DENO_DIR="$POLYGLOT_LOCAL_DIR/cache/deno"
    exec "$deno" "$@"
    ;;
  ts-check)
    "$ROOT/repo.sh" deno --version
    "$ROOT/repo.sh" deno check --frozen ts/app/hello/main.ts ts/test/greeting_test.ts
    "$ROOT/repo.sh" deno fmt --check ts/
    "$ROOT/repo.sh" deno lint ts/
    "$ROOT/repo.sh" deno test --frozen ts/test/
    "$ROOT/repo.sh" deno run --frozen ts/app/hello/main.ts
    ;;
  tsweb-check)
    "$ROOT/repo.sh" deno check --frozen tsweb/app/site/main.tsx tsweb/test/app_test.tsx tsweb/test/title_test.ts tsweb/vite.config.ts
    "$ROOT/repo.sh" deno fmt --check tsweb/
    "$ROOT/repo.sh" deno lint tsweb/
    "$ROOT/repo.sh" deno test --frozen --allow-env=NODE_ENV tsweb/test/
    "$ROOT/repo.sh" tsweb-build
    python3 "$ROOT/tools/tsweb_smoke.py" --root "$ROOT/build/tsweb/site"
    ;;
  tsweb-build)
    "$ROOT/repo.sh" deno run --cached-only --frozen -A npm:vite@8.1.4 build --config tsweb/vite.config.ts
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
    export PATH="$GOROOT/bin:$PATH"
    exec "$GOROOT/bin/go" "$@"
    ;;
  go-check)
    target=$("$ROOT/toolchain/target.sh")
    go_bin=$(tool_path go)
    go_root=$(dirname -- "$(dirname -- "$go_bin")")
    unformatted=$("$go_root/bin/gofmt" -l "$ROOT/go")
    [[ -z $unformatted ]] || { printf 'error: unformatted Go files:\n%s\n' "$unformatted" >&2; exit 1; }
    "$ROOT/repo.sh" go version
    [[ $("$ROOT/repo.sh" go env GOEXPERIMENT) == jsonv2 ]]
    "$ROOT/repo.sh" go mod verify
    "$ROOT/repo.sh" go vet ./go/...
    "$ROOT/repo.sh" go test ./go/...
    mkdir -p "$ROOT/build/go/$target"
    "$ROOT/repo.sh" go build -trimpath -o "$ROOT/build/go/$target/hello" ./go/app/hello
    "$ROOT/build/go/$target/hello"
    ;;
  cpp-reflection-probe)
    cxx=$(tool_path gcc-musl)
    [[ -x $cxx ]] || { printf 'error: pinned compiler is not installed; run ./repo.sh bootstrap\n' >&2; exit 1; }
    python3 "$ROOT/tools/cpp_graph.py" reflection-probe --root "$ROOT" --cxx "$cxx"
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
    python3 "$ROOT/test/editor-contract.py"
    bash "$ROOT/test/test-package-model.sh"
    bash "$ROOT/test/test-package-release.sh"
    "$ROOT/repo.sh" cpp-test
    ;;
  ci)
    "$ROOT/repo.sh" lint
    "$ROOT/repo.sh" package-validate
    "$ROOT/repo.sh" test
    ;;
  *) printf 'error: unknown command: %s\n' "$command" >&2; usage >&2; exit 2 ;;
esac
