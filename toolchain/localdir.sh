#!/usr/bin/env bash
# Resolves - and, for a linked worktree, materializes - the pinned-toolchain
# directory (.local) for a repository root. Sourced by repo.sh,
# toolchain/bootstrap.sh, and toolchain/doctor.sh so the rule lives in exactly
# one place.
#
# WHY A SYMLINK AND NOT JUST AN ENV VAR: rules/toolchain.bzl's staging action
# reads `.local/downloads/<sha256>-<archive>` as a path relative to the buck2
# project root, because a buck2 action's cwd is that root and the archive is
# deliberately untracked. So POLYGLOT_LOCAL_DIR alone can move repo.sh's view
# of the toolchain but NOT the graph's: point it somewhere else and every
# `stage_toolchain_archive` action fails with "run ./repo.sh bootstrap" even
# though bootstrap has run. The directory must therefore be reachable AT
# <root>/.local. Sharing is done by making that path a symlink, which works
# because .local is untracked by buck2 and gitignored - verified empirically:
# a linked worktree whose .local symlinks to the main worktree's builds the
# whole graph with no bootstrap of its own.
#
# .local holds the pinned toolchain plus every language cache (go, deno, xdg),
# so a linked worktree resolving it against its OWN root would bootstrap a
# second full copy - per worktree. Worktrees of one repository share it.
#
# The discriminator is that a linked worktree's .git is a FILE ("gitdir: ..."),
# where a normal clone's is a directory. That distinction matters: a copy of
# this tree sitting inside some unrelated repository has no .git of its own at
# all, and must keep its own .local rather than being redirected up into
# whatever repo happens to contain it.
#
# CAVEAT: <local>/bin's wrappers are rewritten from tools.lock.toml on every
# setup_environment, so worktrees sharing this directory must agree on that
# lock - a worktree on a toolchain-bump branch rewrites the wrappers for the
# others, and concurrent ./repo.sh runs across worktrees race on it. Set
# POLYGLOT_LOCAL_DIR explicitly for a worktree that needs its own toolchain.

# Where the toolchain physically lives for this root. POLYGLOT_LOCAL_DIR wins
# when set; otherwise a linked worktree points at the main worktree's copy.
resolve_local_dir() {
  local root=$1 common parent
  if [[ -n ${POLYGLOT_LOCAL_DIR:-} ]]; then
    printf '%s\n' "$POLYGLOT_LOCAL_DIR"
    return
  fi
  if [[ -f $root/.git ]] && common=$(cd -- "$root" && git rev-parse --git-common-dir 2>/dev/null) && [[ -n $common ]]; then
    [[ $common == /* ]] || common=$root/$common
    if [[ -d $common ]]; then
      common=$(CDPATH= cd -- "$common" && pwd -P)
      parent=$(CDPATH= cd -- "$common/.." && pwd -P)
      # repo.sh beside the common dir means that parent IS the main worktree.
      # Without it (a bare main repository) there is no working tree to sit
      # next to, so the shared directory lives in the common dir.
      if [[ -f $parent/repo.sh ]]; then
        printf '%s/.local\n' "$parent"
      else
        printf '%s/.local\n' "$common"
      fi
      return
    fi
  fi
  printf '%s/.local\n' "$root"
}

# Guarantees <root>/.local reaches the resolved toolchain, then prints that
# path. Callers must use what this prints, not the resolved target: the buck2
# graph can only reach the toolchain through <root>/.local.
ensure_local_dir() {
  local root=$1 shared link current
  # An explicit POLYGLOT_LOCAL_DIR is a deliberate override - honour it
  # verbatim and touch nothing. Callers that set it (toolchain/bootstrap.sh's
  # own smoke test, a throwaway toolchain in CI) want exactly the directory
  # they named, not a symlink planted in the repository.
  if [[ -n ${POLYGLOT_LOCAL_DIR:-} ]]; then
    printf '%s\n' "$POLYGLOT_LOCAL_DIR"
    return
  fi
  shared=$(resolve_local_dir "$root")
  link=$root/.local
  if [[ $shared == "$link" ]]; then
    printf '%s\n' "$link"
    return
  fi
  if [[ -L $link ]]; then
    current=$(readlink -f -- "$link" || true)
    if [[ $current != $(readlink -f -- "$shared" || printf '%s' "$shared") ]]; then
      ln -sfn -- "$shared" "$link"
    fi
  elif [[ -e $link ]]; then
    # A real directory here is somebody's actual bootstrapped toolchain.
    # Replacing it silently would orphan gigabytes and lose any local state,
    # so this fails closed and lets a human decide.
    printf 'error: %s is a real directory, but the toolchain for this tree resolves to\n' "$link" >&2
    printf '       %s.\n' "$shared" >&2
    printf '       Remove or move %s, or set POLYGLOT_LOCAL_DIR to keep using it.\n' "$link" >&2
    return 1
  else
    mkdir -p -- "$shared"
    ln -sfn -- "$shared" "$link"
  fi
  printf '%s\n' "$link"
}
