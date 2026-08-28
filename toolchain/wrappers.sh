#!/usr/bin/env bash
set -euo pipefail

wrapper_relative_path() {
  local root=$1 path=$2
  realpath -s --relative-to="$root" "$path"
}

install_tool_wrapper() {
  local output=$1 content=$2 staged
  staged="$output.staged.$$"
  printf '%s' "$content" >"$staged"
  chmod +x "$staged"
  mv -- "$staged" "$output"
}

write_direct_tool_wrapper() {
  local local_dir=$1 output=$2 executable=$3 executable_rel
  executable_rel=$(wrapper_relative_path "$local_dir" "$executable")
  install_tool_wrapper "$output" "#!/bin/sh
set -eu
ROOT=\$(CDPATH= cd -- \"\$(dirname -- \"\$0\")/..\" && pwd -P)
exec \"\$ROOT/$executable_rel\" \"\$@\"
"
}

write_python_tool_wrapper() {
  local local_dir=$1 output=$2 python=$3 loader=$4 python_rel loader_rel loader_dir_rel python_lib_rel
  python_rel=$(wrapper_relative_path "$local_dir" "$python")
  loader_rel=$(wrapper_relative_path "$local_dir" "$loader")
  loader_dir_rel=$(wrapper_relative_path "$local_dir" "$(dirname -- "$loader")")
  python_lib_rel=$(wrapper_relative_path "$local_dir" "$(dirname -- "$(dirname -- "$python")")/lib")
  install_tool_wrapper "$output" "#!/bin/sh
set -eu
ROOT=\$(CDPATH= cd -- \"\$(dirname -- \"\$0\")/..\" && pwd -P)
exec \"\$ROOT/$loader_rel\" --library-path \"\$ROOT/$loader_dir_rel:\$ROOT/$python_lib_rel\" \"\$ROOT/$python_rel\" \"\$@\"
"
}

write_go_tool_wrapper() {
  local local_dir=$1 output=$2 go=$3 go_rel goroot_rel
  go_rel=$(wrapper_relative_path "$local_dir" "$go")
  goroot_rel=$(wrapper_relative_path "$local_dir" "$(dirname -- "$(dirname -- "$go")")")
  install_tool_wrapper "$output" "#!/bin/sh
set -eu
ROOT=\$(CDPATH= cd -- \"\$(dirname -- \"\$0\")/..\" && pwd -P)
jobs=\${POLYGLOT_JOBS:-}
if [ -z \"\$jobs\" ]; then jobs=\$(getconf _NPROCESSORS_ONLN 2>/dev/null || printf '1\\n'); fi
case \$jobs in ''|*[!0-9]*|0) printf 'error: POLYGLOT_JOBS must be a positive integer\\n' >&2; exit 2 ;; esac
export GOROOT=\"\$ROOT/$goroot_rel\"
export GOPATH=\"\$ROOT/cache/go/path\"
export GOMODCACHE=\"\$ROOT/cache/go/mod\"
export GOCACHE=\"\$ROOT/cache/go/build\"
export GOBIN=\"\$ROOT/bin\"
export GOENV=off GOTOOLCHAIN=local CGO_ENABLED=0
export GOFLAGS=\"-p=\$jobs -mod=vendor -buildvcs=false\"
export PATH=\"\$GOROOT/bin:\$PATH\"
exec \"\$ROOT/$go_rel\" \"\$@\"
"
}

write_repo_tool_wrappers() {
  local local_dir=$1 python=$2 loader=$3 deno=$4 go=$5 buck2=$6 shellcheck=$7 env_bin
  env_bin="$local_dir/bin"
  mkdir -p "$env_bin"
  write_python_tool_wrapper "$local_dir" "$env_bin/python" "$python" "$loader"
  write_python_tool_wrapper "$local_dir" "$env_bin/python3" "$python" "$loader"
  write_go_tool_wrapper "$local_dir" "$env_bin/go" "$go"
  write_direct_tool_wrapper "$local_dir" "$env_bin/deno" "$deno"
  write_direct_tool_wrapper "$local_dir" "$env_bin/buck2" "$buck2"
  write_direct_tool_wrapper "$local_dir" "$env_bin/shellcheck" "$shellcheck"
}
