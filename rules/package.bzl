"""Generic `package()` / `package_smoke` / `package_manifest_test` rules for
the packages/ lane.

`package()` replaces the old hand-shaped `polyglot_package` rule (which knew
the exact set of polyglot-demo's five deps by name). It instead folds
PackageInfo (see rules/pkg.bzl) off an arbitrary `deps` list - each dep is any
packaging-aware target (cxx_binary, go_binary, py_library/py_extension/
py_binary, vite_build, or a cxx_library/go_library/py_library sitting
transitively underneath one) - and:

  1. Flattens the folded transitive_set of PackageEntry records (see
     rules/pkg.bzl's module docstring for what calls package_info() and what
     each lane emits) and fails loudly on any two entries claiming the same
     `dest`, naming both entries' owning targets.
  2. Flattens + dedupes the folded symbolic runtime `needs` (e.g.
     "musl-loader", "python-runtime") and resolves each ONCE against this
     project's pinned toolchain targets (see _NEED_RESOLVERS below) -
     version-aware in the sense that there is exactly one resolution per need
     per native target, so two deps requesting the same need always get the
     identical resolved artifact; this is asserted, not just assumed (see
     _resolve_needs).
  3. Runs one "kind handler" per entry kind ("loader-bin", "static-bin",
     "loader-lib", "tree", "py-app-launcher", "deno-app-launcher" - see
     _stage_lines below) to
     stage the layout, the same shape tools/package_release.py's
     assemble_polyglot_demo() + write_launcher() + write_python_runtime_launcher()
     built by hand for the one polyglot-demo package (same relative launcher
     paths, same "#!/bin/sh\\nset -eu\\n" preamble, same loader
     --library-path wiring).
  4. Reuses the EXISTING deterministic archive machinery this file already
     had (sorted entries, uid/gid 0, mtime 0, mode normalization, gzip
     mtime 0 - see _ARCHIVE_PY below, byte-for-byte the same as before this
     rewrite) and a metadata step (package.json/closure.json/runtime-ref.json
     via the embedded package_model metadata step - see _METADATA_PY below)
     that additionally asserts the `version` attr matches package.toml's own
     recorded version for this package.

package_smoke and package_manifest_test are unchanged from before this
rewrite (package_smoke only ever consumed `package()`'s DefaultInfo output
artifact, never its internal shape).

CRITICAL ARTIFACT-TRACKING LAW (see rules/pkg.bzl's module docstring, which
repeats rules/go.bzl's / rules/python.bzl's own copy of this law): every
PackageEntry.artifact embedded as literal path TEXT inside the staging script
below is also passed into that action's `hidden` list directly, and so is
every toolchain directory a resolved-need artifact was `.project()`ed from.
"""

load("//config:defs.bzl", "fail_if_cross_arch", "target_arch_attr")
load("//rules:pkg.bzl", "PackageEntry", "PackageInfo", "flatten_merged_package_entries", "flatten_merged_package_needs")
load("//toolchains:lock.bzl", "TOOLCHAINS")

def _native_target() -> str:
  # Mirrors rules/{cxx,go,python}.bzl's _native_target(): every already-
  # ported lane only ever builds the host's own musl output triplet, so
  # package() inherits that same native-only constraint.
  arch = host_info().arch
  if arch.is_x86_64:
    return "x86_64-linux-musl"
  if arch.is_aarch64:
    return "aarch64-linux-musl"
  fail("unsupported native CPU architecture")

_NATIVE_TARGET = _native_target()
_GCC = TOOLCHAINS["gcc-musl"][_NATIVE_TARGET]
_LOADER_DIR = _GCC["loader"].rsplit("/", 1)[0]
_LOADER_NAME = "ld-musl-x86_64.so.1" if _NATIVE_TARGET.startswith("x86_64") else "ld-musl-aarch64.so.1"

_PYTHON = TOOLCHAINS["python"][_NATIVE_TARGET]
_PY_ROOT_REL = _PYTHON["expected"].rsplit("/", 2)[0]  # "python", matches rules/python.bzl's _PY_ROOT_REL

_DENO = TOOLCHAINS["deno"][_NATIVE_TARGET]

_DEFAULT_PLATFORM = "//config:{}-dbg".format(_NATIVE_TARGET)

_DIGITS = "0123456789"

def _is_digits(s):
  if s == "":
    return False
  for c in s.elems():
    if c not in _DIGITS:
      return False
  return True

def _check_version(ctx, version):
  # Hand-rolled ^[0-9]+\.[0-9]+\.[0-9]+$ check: Starlark (buck2's dialect)
  # has no `re` module, so this walks the "major.minor.patch" shape
  # explicitly rather than pattern-matching.
  parts = version.split(".")
  ok = len(parts) == 3 and _is_digits(parts[0]) and _is_digits(parts[1]) and _is_digits(parts[2])
  if not ok:
    fail("package({}): version {!r} must match ^[0-9]+\\.[0-9]+\\.[0-9]+$ (major.minor.patch)".format(ctx.attrs.name, version))

# --- need resolution: exactly one resolver per symbolic need, keyed off this
# project's pinned toolchain targets. Because each need has exactly one
# static resolver (there is no per-instance choice of "which musl-loader"),
# two deps requesting the same need can never actually disagree - but the
# fold below still asserts it explicitly rather than silently assuming it,
# so a future second resolver path (e.g. a versioned musl-loader choice)
# can't silently produce two different artifacts under one `dest`.

def _resolve_musl_loader(ctx):
  gcc_dir = ctx.attrs._gcc[DefaultInfo].default_outputs[0]
  loader = gcc_dir.project(_GCC["loader"])
  libc = gcc_dir.project(_LOADER_DIR + "/libc.so")
  owner = "//toolchains:gcc-musl-" + _NATIVE_TARGET
  return [
    # "loader-lib", not "tree": every "loader-bin"/"py-app-launcher"
    # launcher script `exec`s this file directly, so it must land 0755 (see
    # _EXECUTABLE_KINDS above) - libc.so is only ever dlopen'd/linked
    # against, never exec'd, so it stays a plain "tree" entry at 0644.
    PackageEntry(dest = "lib/" + _LOADER_NAME, artifact = loader, kind = "loader-lib", owner = owner),
    PackageEntry(dest = "lib/libc.so", artifact = libc, kind = "tree", owner = owner),
  ]

def _resolve_python_runtime(ctx):
  python_runtime_dir = ctx.attrs._python_runtime[DefaultInfo].default_outputs[0].project(_PY_ROOT_REL)
  owner = "//toolchains:python-" + _NATIVE_TARGET
  return [PackageEntry(dest = "runtime/python", artifact = python_runtime_dir, kind = "tree", owner = owner)]

def _resolve_deno_runtime(ctx):
  # The pinned deno is a fully static single binary (zero dynamic deps, no
  # musl-loader wiring needed - see toolchains/BUCK) - "static-bin" (not a
  # new kind) already gets package()'s generic single-file staging at 0755
  # (see _EXECUTABLE_KINDS below), the same as go-hello's own binary.
  deno_dir = ctx.attrs._deno[DefaultInfo].default_outputs[0]
  deno_bin = deno_dir.project(_DENO["expected"])
  owner = "//toolchains:deno-" + _NATIVE_TARGET
  return [PackageEntry(dest = "runtime/deno/deno", artifact = deno_bin, kind = "static-bin", owner = owner)]

_NEED_RESOLVERS = {
  "deno-runtime": _resolve_deno_runtime,
  "musl-loader": _resolve_musl_loader,
  "python-runtime": _resolve_python_runtime,
}

def _resolve_needs(ctx, needs):
  resolved = {}  # need name -> list[PackageEntry], resolved exactly once
  extra_entries = []
  for need in needs:
    resolver = _NEED_RESOLVERS.get(need)
    if resolver == None:
      fail("package({}): unknown runtime need {!r} (no resolver registered in rules/package.bzl's _NEED_RESOLVERS)".format(ctx.attrs.name, need))
    if need in resolved:
      # Unreachable today (each need has exactly one static resolver), but
      # kept as a real assertion rather than dead code: a future need with
      # more than one candidate resolver must still resolve identically
      # everywhere it's requested.
      fail("package({}): need {!r} resolved more than once".format(ctx.attrs.name, need))
    entries = resolver(ctx)
    resolved[need] = entries
    extra_entries += entries
  return extra_entries

# Kinds that always stage as a single leaf artifact (a real file, or a
# marker a kind handler later writes as one file/script) - used by both the
# chmod decision in _stage_lines and the nesting check in _check_collisions
# below. "tree" is deliberately excluded from both: several lanes
# intentionally merge multiple entries under one shared tree dest (e.g.
# py_extension's "python/lib" tree entry plus py_library's individual
# "python/lib/<path>" file entries - both copy INTO that directory
# rather than clobbering a leaf - see rules/python.bzl's _py_library_impl /
# _py_extension_impl docstrings), and a plain "tree" entry's own artifact
# may itself be either a file or a directory (only known at stage-script RUN
# time, per _stage_lines' `-d` branch below).
_LEAF_KINDS = ("loader-bin", "static-bin", "loader-lib", "py-app-launcher", "deno-app-launcher", "synthetic-marker")

# Kinds whose staged artifact is a real executable that must land 0755 - a
# strict subset of _LEAF_KINDS (py-app-launcher/synthetic-marker entries
# carry no artifact of their own, so they never reach the chmod decision in
# _stage_lines below; their launcher scripts are chmod 0755 explicitly at
# the point they're written instead).
_EXECUTABLE_KINDS = ("loader-bin", "static-bin", "loader-lib")

def _check_dest(ctx, dest, owner):
  # Reject rather than silently normalize: absolute paths, "."/".." path
  # segments, and empty segments (which also catches a leading/trailing/
  # duplicated "/") are always a bug in a kind handler or in a lane's own
  # PackageEntry construction, never something package() should paper over.
  if dest.startswith("/"):
    fail("package({}): dest {!r} (from {}) must be a package-relative path, not absolute".format(ctx.attrs.name, dest, owner))
  for segment in dest.split("/"):
    if segment == "":
      fail("package({}): dest {!r} (from {}) has an empty path segment (leading/trailing/duplicate '/')".format(ctx.attrs.name, dest, owner))
    if segment == "." or segment == "..":
      fail("package({}): dest {!r} (from {}) has a {!r} path segment - malformed path".format(ctx.attrs.name, dest, owner, segment))

def _check_collisions(ctx, entries):
  by_dest = {}
  for entry in entries:
    _check_dest(ctx, entry.dest, entry.owner)
    other = by_dest.get(entry.dest)
    if other != None:
      fail("package({}): dest collision at {!r} between {} and {}".format(ctx.attrs.name, entry.dest, other.owner, entry.owner))
    by_dest[entry.dest] = entry

  # Prefix/nesting conflicts: dest B nested under dest A ("A/B...") is only
  # legitimate when BOTH sides are "tree" entries merging into a shared
  # directory (see _LEAF_KINDS' docstring above) - anything else nesting
  # under/over a single-leaf-artifact dest is a real collision, just not a
  # same-dest one.
  dests = sorted(by_dest.keys())
  for i in range(len(dests)):
    dest_a = dests[i]
    a = by_dest[dest_a]
    prefix = dest_a + "/"
    for dest_b in dests[i + 1:]:
      if not dest_b.startswith(prefix):
        continue
      b = by_dest[dest_b]
      if a.kind == "tree" and b.kind == "tree":
        continue
      fail(
        "package({}): dest {!r} (from {}, kind {!r}) nests {!r} (from {}, kind {!r}) - only \"tree\"-kind entries may share a directory this way".format(
          ctx.attrs.name,
          dest_a,
          a.owner,
          a.kind,
          dest_b,
          b.owner,
          b.kind,
        ),
      )

def _synthesize_launcher_markers(entries):
  # _stage_lines writes bin/<name> for every "loader-bin" entry, plus
  # bin/python + bin/python3 once whenever ANY "py-app-launcher" entry is
  # present - none of which have their own PackageEntry, so without this
  # they're invisible to _check_collisions and another target quietly
  # staging the same bin/ dest would collide at RUN time instead of at
  # analysis time. ("py-app-launcher"'s own bin/<name> dest already has a
  # real marker entry - see rules/python.bzl's _py_binary_impl - so it
  # doesn't need one synthesized here.)
  markers = []
  has_py_app = False
  for entry in entries:
    if entry.kind == "loader-bin":
      name = entry.dest.rsplit("/", 1)[-1]
      markers.append(PackageEntry(dest = "bin/" + name, artifact = None, kind = "synthetic-marker", owner = entry.owner))
    elif entry.kind == "py-app-launcher":
      has_py_app = True
  if has_py_app:
    shared_owner = "<package(): shared bin/python + bin/python3 launcher>"
    markers.append(PackageEntry(dest = "bin/python", artifact = None, kind = "synthetic-marker", owner = shared_owner))
    markers.append(PackageEntry(dest = "bin/python3", artifact = None, kind = "synthetic-marker", owner = shared_owner))
  return markers

# --- kind handlers: build the `stage.sh` script body that lays out every
# entry under $OUT, plus the two synthesized launcher families
# ("loader-bin" -> bin/<name>; "py-app-launcher" -> bin/<name> + bin/python +
# bin/python3).

def _dirname(path):
  return path.rsplit("/", 1)[0] if "/" in path else ""

def _stage_lines(ctx, entries, needs):
  lines = []
  hidden = []

  # One mkdir -p covering every entry's own parent dir, plus bin/ (every
  # loader-bin/py-app-launcher entry writes there) and lib/ (whenever the
  # musl loader is staged) up front, so later cp/cat lines never race a
  # missing parent directory.
  dirs = {"bin": True}
  if "musl-loader" in needs:
    dirs["lib"] = True
  for entry in entries:
    d = _dirname(entry.dest)
    if d:
      dirs[d] = True
  lines.append("mkdir -p " + " ".join(["\"$OUT/{}\"".format(d) for d in sorted(dirs.keys())]))

  # Stage every entry that carries a real artifact (kinds "tree",
  # "static-bin", "loader-bin" - a "py-app-launcher" entry's artifact is
  # always None, handled separately below). Whether the artifact is a
  # directory or a single file is only known at RUN time (Starlark doesn't
  # see through an Artifact to know which), so the staging line branches on
  # `-d` rather than needing separate kind-specific Starlark-side handling
  # for "tree" vs "static-bin" - this is the "plain staging" the contract's
  # kind-handler table means by grouping those two kinds together.
  idx = 0
  for entry in entries:
    if entry.artifact == None:
      continue
    idx += 1
    var = "ART{}".format(idx)
    lines.append(cmd_args("{}=\"$(pwd)/".format(var), entry.artifact, "\"", delimiter = ""))
    hidden.append(entry.artifact)
    dest = "\"$OUT/{}\"".format(entry.dest)
    # A directory artifact's own file permissions are whatever its own
    # producing action gave them (cp -R preserves them) - only the
    # single-file branch needs an explicit mode, since that's the only case
    # package() itself picks the mode. Plain sources/assets stay 0644;
    # "loader-bin"/"static-bin" raw binaries and the musl loader's own
    # "loader-lib" entry (exec'd directly by launcher scripts, see below)
    # need 0755 - see _LEAF_KINDS' docstring above for why nothing else does.
    mode = "0755" if entry.kind in _EXECUTABLE_KINDS else "0644"
    lines.append(
      "if [ -d \"${var}\" ]; then mkdir -p {dest}; cp -R \"${var}\"/. {dest}/; else cp \"${var}\" {dest}; chmod {mode} {dest}; fi".format(
        var = var,
        dest = dest,
        mode = mode,
      ),
    )

  # "loader-bin": bin/<name> launcher exec'ing the staged libexec/<name>
  # binary through the staged lib/<loader>. Byte-for-byte
  # write_launcher()'s shape.
  for entry in entries:
    if entry.kind != "loader-bin":
      continue
    name = entry.dest.rsplit("/", 1)[-1]
    lines += [
      "cat > \"$OUT/bin/{name}\" <<'PKGEOF'".format(name = name),
      "#!/bin/sh",
      "set -eu",
      "ROOT=$(CDPATH= cd -- \"$(dirname -- \"$0\")/..\" && pwd -P)",
      "exec \"$ROOT/lib/{loader}\" --library-path \"$ROOT/lib\" \"$ROOT/{dest}\" \"$@\"".format(loader = _LOADER_NAME, dest = entry.dest),
      "PKGEOF",
      "chmod 0755 \"$OUT/bin/{name}\"".format(name = name),
    ]

  # "py-app-launcher": bin/<name> (execing this same target's own `main`
  # source entry, whose staged dest is carried directly on the marker's own
  # `meta` - see rules/python.bzl's py_binary emission and rules/pkg.bzl's
  # PackageEntry.meta doc comment; this is the same mechanism
  # "deno-app-launcher" below uses for its own entrypoint, and for the same
  # reason: pairing a marker back to its source by dest-prefix/owner
  # convention alone isn't reliable once a py_binary isn't required to live
  # under any particular directory) plus the shared bin/python + bin/python3
  # loader-wrapped interpreter launchers (written once total, byte-for-byte
  # write_python_runtime_launcher()'s shape).
  wrote_python_launchers = False
  for entry in entries:
    if entry.kind != "py-app-launcher":
      continue
    name = entry.dest.rsplit("/", 1)[-1]
    main_dest = entry.meta
    if main_dest == None:
      fail("package({}): py-app-launcher entry for dest {!r} (target {}) has no `meta` entrypoint dest set".format(ctx.attrs.name, entry.dest, entry.owner))
    lines += [
      "cat > \"$OUT/bin/{name}\" <<'PKGEOF'".format(name = name),
      "#!/bin/sh",
      "set -eu",
      "ROOT=$(CDPATH= cd -- \"$(dirname -- \"$0\")/..\" && pwd -P)",
      "export PYTHONPATH=\"$ROOT/python/lib:$ROOT/python/app\"",
      "exec \"$ROOT/bin/python\" \"$ROOT/{main_dest}\" \"$@\"".format(main_dest = main_dest),
      "PKGEOF",
      "chmod 0755 \"$OUT/bin/{name}\"".format(name = name),
    ]
    if not wrote_python_launchers:
      wrote_python_launchers = True
      for py in ("python", "python3"):
        lines += [
          "cat > \"$OUT/bin/{py}\" <<'PKGEOF'".format(py = py),
          "#!/bin/sh",
          "set -eu",
          "ROOT=$(CDPATH= cd -- \"$(dirname -- \"$0\")/..\" && pwd -P)",
          "exec \"$ROOT/lib/{loader}\" --library-path \"$ROOT/lib:$ROOT/runtime/python/lib\" \"$ROOT/runtime/python/bin/python3\" \"$@\"".format(loader = _LOADER_NAME),
          "PKGEOF",
          "chmod 0755 \"$OUT/bin/{py}\"".format(py = py),
        ]

  # "deno-app-launcher": a small generated per-package deno.json (staged at
  # ts/deno.json - written ONCE, before any launcher below references it)
  # that maps this project's "@/" import scope onto the staged "lib/" root,
  # mirroring the repo's own root deno.json scope (`"./ts/": {"@/":
  # "./ts/lib/"}`) but rooted at ts/ itself since that's where deno_app's
  # own staged closure lands (a root-level sibling of bin/lib/libexec/
  # runtime/share). Deliberately NOT the repo's real deno.json: that file
  # declares npm: imports (react/vite/...) this packaging path refuses to
  # ship - see the npm-closure guard below - and has no reason to carry
  # deno.lock/fmt/lint/test config into a packaged app at all.
  #
  # npm-closure guard: package() only supports zero-npm-dep deno apps today
  # - rather than silently shipping the whole npm store (deno vendors real
  # package copies, not symlinks - see rules/deno.bzl's module docstring),
  # this fails the BUILD loudly the moment any deno-app-launcher entry's
  # own staged ts/ closure contains an "npm:" specifier anywhere, before
  # any launcher is ever run.
  has_deno_app = False
  for entry in entries:
    if entry.kind == "deno-app-launcher":
      has_deno_app = True
      break
  if has_deno_app:
    lines += [
      "if grep -rl 'npm:' \"$OUT/ts\" >/dev/null 2>&1; then",
      "  echo \"package({}): npm-dependent deno apps not yet supported by packaging (found npm: specifier under ts)\" >&2".format(ctx.attrs.name),
      "  exit 1",
      "fi",
      "cat > \"$OUT/ts/deno.json\" <<'PKGEOF'",
      "{",
      "  \"scopes\": {",
      "    \"./\": { \"@/\": \"./lib/\" }",
      "  }",
      "}",
      "PKGEOF",
    ]

  for entry in entries:
    if entry.kind != "deno-app-launcher":
      continue
    name = entry.dest.rsplit("/", 1)[-1]
    main_dest = entry.meta
    if main_dest == None or not main_dest.startswith("ts/"):
      fail("package({}): deno-app-launcher entry for dest {!r} (target {}) has an invalid entry path {!r} - packaging only supports ts/ deno apps today".format(ctx.attrs.name, entry.dest, entry.owner, main_dest))
    lines += [
      "cat > \"$OUT/bin/{name}\" <<'PKGEOF'".format(name = name),
      "#!/bin/sh",
      "set -eu",
      "ROOT=$(CDPATH= cd -- \"$(dirname -- \"$0\")/..\" && pwd -P)",
      # --no-remote: fail-closed on any non-local import (the key offline
      # guarantee - deno never even attempts a network fetch, cached or
      # not). --allow-net: this app calls Deno.serve(). --config: the
      # generated ts/deno.json above, so "@/" resolves inside the staged
      # layout exactly like it does in the repo's own deno.json.
      "exec \"$ROOT/runtime/deno/deno\" run --no-remote --allow-net --config \"$ROOT/ts/deno.json\" \"$ROOT/{main_dest}\" \"$@\"".format(main_dest = main_dest),
      "PKGEOF",
      "chmod 0755 \"$OUT/bin/{name}\"".format(name = name),
    ]

  # __pycache__/*.pyc exclusion (mirrors tools/package_release.py's
  # copy_tree(..., ignore=(...))): run ONCE at the end over the two roots
  # that can ever carry probe/build-time bytecode cache files
  # (runtime/python and python), rather than once per staged entry -
  # entries under python/ number in the dozens for a real py_library
  # fan-out, and re-walking the same shared subtree that many times bought
  # nothing. `2>/dev/null || true` also makes each find tolerant of either
  # root not existing at all (a pure cxx/go/deno package stages neither).
  lines += [
    "find \"$OUT/runtime/python\" \"$OUT/python\" -name __pycache__ -type d -prune -exec rm -rf {} + 2>/dev/null || true",
    "find \"$OUT/runtime/python\" \"$OUT/python\" -name '*.pyc' -delete 2>/dev/null || true",
  ]

  return lines, hidden

# --- package(): fold deps' PackageInfo, resolve needs, stage, then archive.

def _package_impl(ctx: AnalysisContext) -> list[Provider]:
  fail_if_cross_arch(ctx, _NATIVE_TARGET)
  _check_version(ctx, ctx.attrs.version)

  # Merge every dep's PackageInfo.entries (resp. .needs) tset as children of
  # ONE new tset node and traverse THAT once, rather than flattening each
  # dep's tset separately and concatenating the python lists - see
  # flatten_merged_package_entries()'s docstring in rules/pkg.bzl: a target
  # shared transitively by two deps (a diamond dep) is a single DAG node
  # reachable from both, and only a single shared traversal dedupes it the
  # way buck2's tsets are meant to.
  infos = [d[PackageInfo] for d in ctx.attrs.deps if PackageInfo in d]
  entries = flatten_merged_package_entries(ctx, infos)
  needs = flatten_merged_package_needs(ctx, infos)

  extra_entries = _resolve_needs(ctx, needs)
  all_entries = entries + extra_entries
  markers = _synthesize_launcher_markers(all_entries)
  _check_collisions(ctx, all_entries + markers)

  # Sort by dest so the staging script's content (and its ART<n> variable
  # numbering) is stable regardless of the tset traversal order above.
  all_entries = [pair[1] for pair in sorted([(e.dest, e) for e in all_entries])]

  stage_lines, stage_hidden = _stage_lines(ctx, all_entries, needs)

  stage = ctx.actions.declare_output(ctx.attrs.name + "-stage", dir = True)
  metadata_script = ctx.actions.write(ctx.attrs.name + "-metadata.py", _METADATA_PY)
  lines = [
    "#!/bin/sh",
    "set -eu",
    cmd_args("OUT=\"$(pwd)/", stage.as_output(), "\"", delimiter = ""),
  ] + stage_lines + [
    # package.json/closure.json/runtime-ref.json: same metadata.py step as
    # before this rewrite, inside this one staging action (a second action
    # can't keep writing into an already-declared output directory),
    # invoking the real root-exported //:package_model.py module - see
    # _METADATA_PY below.
    cmd_args("METADATA_SCRIPT=\"$(pwd)/", metadata_script, "\"", delimiter = ""),
    cmd_args("PACKAGE_MODEL=\"$(pwd)/", ctx.attrs._package_model, "\"", delimiter = ""),
    cmd_args("MANIFEST=\"$(pwd)/", ctx.attrs.manifest, "\"", delimiter = ""),
    cmd_args("LOCK=\"$(pwd)/", ctx.attrs.lock, "\"", delimiter = ""),
    cmd_args("TOOLS_LOCK=\"$(pwd)/", ctx.attrs.tools_lock, "\"", delimiter = ""),
    "python3 -B \"$METADATA_SCRIPT\" \"$PACKAGE_MODEL\" \"$MANIFEST\" \"$LOCK\" \"$TOOLS_LOCK\" %s %s %s %s \"$OUT\"" % (ctx.attrs.name, _NATIVE_TARGET, ctx.attrs.profile, ctx.attrs.version),
  ]
  metadata_inputs = [ctx.attrs._package_model, ctx.attrs.manifest, ctx.attrs.lock, ctx.attrs.tools_lock]
  stage_inputs = stage_hidden + [metadata_script] + metadata_inputs
  stage_script, stage_written = ctx.actions.write(ctx.attrs.name + "-stage.sh", lines, is_executable = True, allow_args = True)
  ctx.actions.run(
    cmd_args(["/bin/sh", stage_script, stage.as_output()], hidden = stage_inputs + stage_written),
    category = "package_stage",
    identifier = ctx.attrs.name,
  )

  archive = ctx.actions.declare_output(ctx.attrs.name + ".tar.gz")
  archive_script = ctx.actions.write(ctx.attrs.name + "-archive.py", _ARCHIVE_PY)
  ctx.actions.run(
    cmd_args(["python3", archive_script, stage, archive.as_output()]),
    category = "package_archive",
    identifier = ctx.attrs.name,
  )

  return [DefaultInfo(
    default_outputs = [archive],
    sub_targets = {"stage": [DefaultInfo(default_output = stage)]},
  )]

# Byte-for-byte port of tools/package_release.py's reset_tarinfo() +
# make_archive() (unchanged from before this rewrite): sorted(stage.rglob("*"))
# entry order, uid=gid=0, uname=gname="root", mtime=0 on every tar member,
# dirs 0755, any executable bit -> 0755, everything else -> 0644, and the
# gzip wrapper itself written with mtime=0 (gzip.GzipFile(..., mtime=0)) so
# the compressed bytes are reproducible too, not just the uncompressed tar
# bytes.
_ARCHIVE_PY = [
  "import gzip, sys, tarfile",
  "from pathlib import Path",
  "",
  "stage = Path(sys.argv[1])",
  "archive = Path(sys.argv[2])",
  "",
  "",
  "def reset(info):",
  "    info.uid = 0",
  "    info.gid = 0",
  "    info.uname = 'root'",
  "    info.gname = 'root'",
  "    info.mtime = 0",
  "    if info.isdir():",
  "        info.mode = 0o755",
  "    elif info.mode & 0o111:",
  "        info.mode = 0o755",
  "    else:",
  "        info.mode = 0o644",
  "    return info",
  "",
  "",
  "archive.parent.mkdir(parents=True, exist_ok=True)",
  "with archive.open('wb') as raw:",
  "    with gzip.GzipFile(filename='', mode='wb', fileobj=raw, mtime=0) as zipped:",
  "        with tarfile.open(fileobj=zipped, mode='w') as tar:",
  "            for path in sorted(stage.rglob('*')):",
  "                tar.add(str(path), arcname=path.relative_to(stage).as_posix(), recursive=False, filter=reset)",
]

# package.json/closure.json/runtime-ref.json: reuses package_model.py's own
# packages()/resolve() (imported as a real module, not re-implemented),
# unchanged from before this rewrite - the package_name argument is now
# ctx.attrs.name (the package() target's own name, e.g. "polyglot-demo")
# rather than a separate `package_name` attr, since the generic rule has no
# reason to let those two diverge.
_METADATA_PY = [
  "import json, sys",
  "from pathlib import Path",
  "",
  # Do not write bytecode cache and do not resolve() the exported
  # //:package_model.py artifact: buck2's export_file() output is typically
  # a symlink back to the real tools/package_model.py, and resolve()-ing it
  # before sys.path insertion would make CPython treat that real source
  # directory as the import root, writing __pycache__ there from inside a
  # build action - a hermeticity/side-effect footgun this avoids entirely.
  "sys.dont_write_bytecode = True",
  "package_model_path = Path(sys.argv[1])",
  "sys.path.insert(0, str(package_model_path.parent))",
  "import package_model",
  "",
  "manifest = Path(sys.argv[2])",
  "lock = Path(sys.argv[3])",
  "tools_lock = Path(sys.argv[4])",
  "package_name = sys.argv[5]",
  "target = sys.argv[6]",
  "profile = sys.argv[7]",
  "attr_version = sys.argv[8]",
  "stage = Path(sys.argv[9])",
  "",
  "model = package_model.load(manifest)",
  "catalog = package_model.packages(model)",
  "entry = catalog[package_name]",
  "closure, runtime_ref = package_model.resolve(model, package_model.load(lock), package_model.load(tools_lock), package_name, target)",
  "",
  # package()'s `version` attr is validated independently (rules/package.bzl's
  # _check_version) and passed through here so it can never silently drift
  # from what package.toml's manifest itself says for this package - a
  # mismatch means someone edited one without the other.
  "if attr_version != entry['version']:",
  "    sys.exit(",
  "        'package(): version attr %r does not match package.toml version %r for package %r'",
  "        % (attr_version, entry['version'], package_name)",
  "    )",
  "",
  "",
  "def write_json(path, value):",
  "    path.write_text(json.dumps(value, sort_keys=True, indent=2) + '\\n', encoding='utf-8')",
  "",
  "",
  "metadata = {",
  "    'schema_version': 1,",
  "    'package': package_name,",
  "    'version': entry['version'],",
  "    'target': target,",
  "    'profile': profile,",
  "    'executables': list(entry['executables']),",
  "    'closure_sha256': closure['closure_sha256'],",
  "}",
  "write_json(stage / 'package.json', metadata)",
  "write_json(stage / 'closure.json', closure)",
  "if runtime_ref:",
  "    write_json(stage / 'runtime-ref.json', runtime_ref)",
]

_package_rule = rule(
  impl = _package_impl,
  attrs = {
    "deps": attrs.list(attrs.dep(), default = []),
    "lock": attrs.source(default = "//:runtime-resolution.lock.toml"),
    "manifest": attrs.source(default = "//:package.toml"),
    "profile": attrs.string(default = select({"//config:opt": "opt", "DEFAULT": "dbg"})),
    "tools_lock": attrs.source(default = "//:tools.lock.toml"),
    "version": attrs.string(),
    "_deno": attrs.dep(default = "//toolchains:deno-" + _NATIVE_TARGET, providers = [DefaultInfo]),
    "_gcc": attrs.dep(default = "//toolchains:gcc-musl-" + _NATIVE_TARGET, providers = [DefaultInfo]),
    "_package_model": attrs.source(default = "//:package_model.py"),
    "_python_runtime": attrs.dep(default = "//toolchains:python-" + _NATIVE_TARGET, providers = [DefaultInfo]),
    "_target_arch": target_arch_attr(),
  },
)

def package(**kwargs):
  kwargs.setdefault("default_target_platform", _DEFAULT_PLATFORM)
  _package_rule(**kwargs)

# --- package_smoke: extract the already-built archive and exercise every
# bin/ executable plus (optionally) the packaged web assets. No toolchain
# deps at all - every binary in the archive is either static (go-hello,
# deno) or already loader-wrapped (cpp-hello, python, python3,
# python-hello), exactly like tools/package_release.py's smoke()'s
# --execute path, but without needing host toolchain state to run it.
#
# `checks` is a list of raw shell command lines run against the extracted
# archive at $WORK (e.g. "\"$WORK/bin/go-hello\"", "\"$WORK/bin/server\"
# --smoke") - the caller (packages/BUCK) owns the exact set of binaries/
# flags for its own package, since that shape differs per package (compare
# polyglot-demo's five-binary smoke below to polyglot-server's single
# `bin/server --smoke`). `smoke_script` stays a separate optional attr
# (rather than folded into `checks`, which are raw shell) since it is a real
# tracked source input (see the ARTIFACT-TRACKING LAW in this file's module
# docstring) invoked against the packaged web assets specifically.
def _package_smoke_impl(ctx: AnalysisContext) -> list[Provider]:
  archive = ctx.attrs.package[DefaultInfo].default_outputs[0]
  smoke_script = ctx.attrs.smoke_script

  lines = [
    "#!/bin/sh",
    "set -eu",
    cmd_args("ARCHIVE=\"$(pwd)/", archive, "\"", delimiter = ""),
  ]
  hidden = [archive]
  if smoke_script != None:
    lines.append(cmd_args("SMOKE_SCRIPT=\"$(pwd)/", smoke_script, "\"", delimiter = ""))
    hidden.append(smoke_script)
  lines += [
    # Anchored under buck-out, not bare mktemp -d (which defaults to /tmp) -
    # host /tmp is a small tmpfs shared by every concurrent build/test on
    # this machine (see rules/go.bzl's _write_go_script for the identical
    # rationale).
    "SCRATCH_BASE=\"$(pwd)/buck-out/v2/tmp/package-smoke\"",
    "mkdir -p \"$SCRATCH_BASE\"",
    "WORK=$(mktemp -d \"$SCRATCH_BASE/tmp.XXXXXX\")",
    "trap 'rm -rf \"$WORK\"' EXIT",
    "tar -xzf \"$ARCHIVE\" -C \"$WORK\"",
  ] + list(ctx.attrs.checks)
  if smoke_script != None:
    lines.append("python3 \"$SMOKE_SCRIPT\" --root \"$WORK/web\"")

  script, written = ctx.actions.write(ctx.attrs.name + ".sh", lines, is_executable = True, allow_args = True)
  command = cmd_args(script, hidden = hidden + written)
  return [
    DefaultInfo(default_output = script, other_outputs = written),
    RunInfo(args = command),
    ExternalRunnerTestInfo(
      type = "package_smoke",
      command = [command],
      run_from_project_root = True,
    ),
  ]

_package_smoke_rule = rule(
  impl = _package_smoke_impl,
  attrs = {
    "checks": attrs.list(attrs.string(), default = []),
    "package": attrs.dep(providers = [DefaultInfo]),
    "smoke_script": attrs.option(attrs.source(), default = None),
  },
)

def package_smoke(**kwargs):
  kwargs.setdefault("default_target_platform", _DEFAULT_PLATFORM)
  _package_smoke_rule(**kwargs)

# --- package_manifest_test: `python3 package_model.py validate`, repo.sh's
# package-validate lane, taking the root-exported //:package_model.py itself
# as a real tracked attrs.source() input alongside the root-exported
# manifest/lock/tools-lock files, so an edit to any of the four invalidates
# this test's buck2 cache entry. Unchanged from before this rewrite.

def _package_manifest_test_impl(ctx: AnalysisContext) -> list[Provider]:
  lines = [
    "#!/bin/sh",
    "set -eu",
    cmd_args("PACKAGE_MODEL=\"$(pwd)/", ctx.attrs.package_model, "\"", delimiter = ""),
    cmd_args("MANIFEST=\"$(pwd)/", ctx.attrs.manifest, "\"", delimiter = ""),
    cmd_args("LOCK=\"$(pwd)/", ctx.attrs.lock, "\"", delimiter = ""),
    cmd_args("TOOLS_LOCK=\"$(pwd)/", ctx.attrs.tools_lock, "\"", delimiter = ""),
    "exec python3 -B \"$PACKAGE_MODEL\" --manifest \"$MANIFEST\" --lock \"$LOCK\" --tools-lock \"$TOOLS_LOCK\" validate",
  ]
  script, written = ctx.actions.write(ctx.attrs.name + ".sh", lines, is_executable = True, allow_args = True)
  command = cmd_args(script, hidden = [ctx.attrs.package_model, ctx.attrs.manifest, ctx.attrs.lock, ctx.attrs.tools_lock] + written)
  return [
    DefaultInfo(default_output = script, other_outputs = written),
    RunInfo(args = command),
    ExternalRunnerTestInfo(
      type = "package_manifest_validation",
      command = [command],
      run_from_project_root = True,
    ),
  ]

_package_manifest_test_rule = rule(
  impl = _package_manifest_test_impl,
  attrs = {
    "lock": attrs.source(),
    "manifest": attrs.source(),
    "package_model": attrs.source(default = "//:package_model.py"),
    "tools_lock": attrs.source(),
  },
)

def package_manifest_test(**kwargs):
  kwargs.setdefault("default_target_platform", _DEFAULT_PLATFORM)
  _package_manifest_test_rule(**kwargs)

