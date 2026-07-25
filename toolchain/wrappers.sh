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

write_clang_format_tool_wrapper() {
  local local_dir=$1 output=$2 clang_format=$3 loader=$4 clang_rel loader_rel loader_dir_rel clang_lib_rel
  clang_rel=$(wrapper_relative_path "$local_dir" "$clang_format")
  loader_rel=$(wrapper_relative_path "$local_dir" "$loader")
  loader_dir_rel=$(wrapper_relative_path "$local_dir" "$(dirname -- "$loader")")
  clang_lib_rel=$(wrapper_relative_path "$local_dir" "$(dirname -- "$(dirname -- "$(dirname -- "$clang_format")")")/../clang_format.libs")
  install_tool_wrapper "$output" "#!/bin/sh
set -eu
ROOT=\$(CDPATH= cd -- \"\$(dirname -- \"\$0\")/..\" && pwd -P)
exec \"\$ROOT/$loader_rel\" --library-path \"\$ROOT/$loader_dir_rel:\$ROOT/$clang_lib_rel\" \"\$ROOT/$clang_rel\" \"\$@\"
"
}

write_repo_tool_wrappers() {
  local local_dir=$1 cc=$2 cxx=$3 python=$4 loader=$5 deno=$6 go=$7 buck2=$8 clang_format=$9
  local gcc_install=${10} target=${11} shellcheck=${12} env_bin binutil binutil_rel
  env_bin="$local_dir/bin"
  mkdir -p "$env_bin"
  write_python_tool_wrapper "$local_dir" "$env_bin/python" "$python" "$loader"
  write_python_tool_wrapper "$local_dir" "$env_bin/python3" "$python" "$loader"
  write_direct_tool_wrapper "$local_dir" "$env_bin/gcc" "$cc"
  write_direct_tool_wrapper "$local_dir" "$env_bin/g++" "$cxx"
  write_direct_tool_wrapper "$local_dir" "$env_bin/go" "$go"
  write_direct_tool_wrapper "$local_dir" "$env_bin/deno" "$deno"
  write_direct_tool_wrapper "$local_dir" "$env_bin/buck2" "$buck2"
  write_clang_format_tool_wrapper "$local_dir" "$env_bin/clang-format" "$clang_format" "$loader"
  # Statically linked: no loader, no --library-path, so the direct wrapper
  # applies. Written last so an older bootstrap that has every other tool but
  # not this one still produces the wrappers it can.
  write_direct_tool_wrapper "$local_dir" "$env_bin/shellcheck" "$shellcheck"
  for binutil in ar ranlib nm strip objcopy ld; do
    binutil_rel=$(lock_value gcc-musl "$target" "$binutil")
    write_direct_tool_wrapper "$local_dir" "$env_bin/$binutil" "$gcc_install/$binutil_rel"
  done
}
