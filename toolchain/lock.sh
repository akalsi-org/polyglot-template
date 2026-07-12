#!/usr/bin/env bash
# Minimal reader for the deliberately flat tools.lock.toml schema.

lock_value() {
  local tool=$1 target=$2 key=$3 lock=${POLYGLOT_LOCK_FILE:?}
  awk -v wanted_tool="$tool" -v wanted_target="$target" -v wanted_key="$key" '
    /^\[\[artifact\]\]$/ { active=1; t=""; p=""; next }
    active && /^[[:space:]]*[A-Za-z0-9_]+[[:space:]]*=/ {
      line=$0; sub(/^[[:space:]]*/, "", line)
      k=line; sub(/[[:space:]]*=.*/, "", k)
      v=line; sub(/^[^=]*=[[:space:]]*/, "", v)
      sub(/[[:space:]]*#.*/, "", v)
      gsub(/^"|"$/, "", v)
      if (k == "tool") t=v
      if (k == "target") p=v
      if (t == wanted_tool && p == wanted_target && k == wanted_key) { print v; exit }
    }
  ' "$lock"
}

locked_tools() {
  awk '
    /^\[\[artifact\]\]$/ { active=1; t=""; next }
    active && /^[[:space:]]*tool[[:space:]]*=/ {
      line=$0; sub(/^[^=]*=[[:space:]]*/, "", line); sub(/[[:space:]]*#.*/, "", line)
      gsub(/^"|"$/, "", line); if (!seen[line]++) print line
    }
  ' "${POLYGLOT_LOCK_FILE:?}"
}
