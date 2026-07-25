#!/usr/bin/env bash
# Minimal reader for the deliberately flat tools.lock.toml schema.
#
# Both readers treat ANY line starting a TOML table as the end of the current
# [[artifact]] record. Leaving `active` latched across tables attributed a
# later non-artifact table's keys to the last artifact seen. Each record is
# also buffered to its end before being emitted, so a wanted key no longer has
# to appear after `tool`/`target` inside the record.

lock_value() {
  local tool=$1 target=$2 key=$3 lock=${POLYGLOT_LOCK_FILE:?}
  awk -v wanted_tool="$tool" -v wanted_target="$target" -v wanted_key="$key" '
    # awk `exit` runs END, so END re-entering emit() would print twice:
    # latch instead.
    function emit() {
      if (!found && active && t == wanted_tool && p == wanted_target && have) {
        found = 1; print val; exit
      }
    }
    /^[[:space:]]*\[/ {
      emit()
      active = ($0 ~ /^\[\[artifact\]\][[:space:]]*$/)
      t = ""; p = ""; val = ""; have = 0
      next
    }
    active && /^[[:space:]]*[A-Za-z0-9_]+[[:space:]]*=/ {
      line = $0; sub(/^[[:space:]]*/, "", line)
      k = line; sub(/[[:space:]]*=.*/, "", k)
      v = line; sub(/^[^=]*=[[:space:]]*/, "", v)
      # A quoted value may legitimately contain "#" (release URLs do), so only
      # an UNQUOTED value has a trailing comment stripped from it.
      if (v ~ /^"/) { sub(/^"/, "", v); sub(/"[[:space:]]*(#.*)?$/, "", v) }
      else { sub(/[[:space:]]*#.*/, "", v); sub(/[[:space:]]+$/, "", v) }
      if (k == "tool") t = v
      if (k == "target") p = v
      if (k == wanted_key) { val = v; have = 1 }
    }
    END { emit() }
  ' "$lock"
}

locked_tools() {
  awk '
    /^[[:space:]]*\[/ { active = ($0 ~ /^\[\[artifact\]\][[:space:]]*$/); next }
    active && /^[[:space:]]*tool[[:space:]]*=/ {
      line = $0; sub(/^[^=]*=[[:space:]]*/, "", line)
      if (line ~ /^"/) { sub(/^"/, "", line); sub(/"[[:space:]]*(#.*)?$/, "", line) }
      else { sub(/[[:space:]]*#.*/, "", line); sub(/[[:space:]]+$/, "", line) }
      if (!seen[line]++) print line
    }
  ' "${POLYGLOT_LOCK_FILE:?}"
}
