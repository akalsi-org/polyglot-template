"""Generic packaging contract shared by every lane's rules
(rules/{cxx,go,python,deno}.bzl) and consumed by rules/package.bzl's
`package()` rule.

Each packaging-aware rule impl calls `package_info(ctx, entries, needs, deps)`
once, at the end of its own impl, and adds the returned PackageInfo to its
provider list alongside its lane-specific provider (CxxInfo/GoInfo/PyInfo/...).
`entries` is this target's OWN list[PackageEntry] (empty for targets that
don't stage anything themselves - e.g. cxx_library); `needs` is this target's
own list[str] of symbolic runtime requirements (e.g. "musl-loader"); `deps` is
the list of dependency targets (typically ctx.attrs.deps) whose own
PackageInfo providers (if any - deps without one are silently skipped) get
folded in as transitive_set children, so a `package()` rule anywhere
downstream sees the whole folded graph without every intermediate rule having
to re-derive it by hand.

Every packaging-aware rule must also add `labels` (default []) to its attrs
(see PACKAGE_LABELS_ATTR below) - `package_info()` no-ops (returns an empty
PackageInfo) when "no-package" is one of that target's own labels, without
requiring the rule impl itself to special-case anything.

Test rules and cxx_adapter are deliberately NOT packaging-aware: they don't
call package_info() at all (not even with empty entries/needs), and don't
carry the `labels` attr - a target that will never be staged into a package
has no reason to participate in this contract.

CRITICAL ARTIFACT-TRACKING LAW (mirrors every other rules/*.bzl file in this
project - see rules/go.bzl's / rules/python.bzl's module docstrings for the
general rule this repeats): rules/package.bzl's kind handlers write launcher
scripts that embed a PackageEntry.artifact's staged destination path as
literal TEXT. Any script that does this MUST also pass the underlying
Artifact (PackageEntry.artifact itself, and any toolchain dir it was
`.project()`ed from) into the consuming action's `hidden` list - a text-only
reference is not enough for buck2 to track/materialize it correctly.
"""

PackageEntry = record(
  # Package-relative destination path, e.g. "bin/cpp-hello",
  # "app/python/lib/example/example.py", "app/web".
  dest = field(str),
  # The staged artifact (a single file OR a whole directory - kind handlers
  # decide how to copy it). None for entries that carry no artifact of their
  # own and exist purely as a marker for a kind handler to synthesize output
  # from (e.g. "py-app-launcher").
  artifact = field([Artifact, None], None),
  # One of "loader-bin", "static-bin", "tree", "loader-lib",
  # "py-app-launcher" - see rules/package.bzl's kind-handler dispatch.
  # "loader-lib" is the musl loader's own executable (ld-musl-*.so.1,
  # exec'd directly by every "loader-bin"/"py-app-launcher" launcher
  # script) - distinguished from the plain "tree" kind (e.g. libc.so,
  # which sits alongside it but is never exec'd) purely so
  # rules/package.bzl's staging script knows which artifacts must land
  # 0755 instead of the default 0644.
  kind = field(str),
  # str(ctx.label.raw_target()) of the target that emitted this entry - used
  # for dest-collision error messages and to pair a py_binary's
  # "py-app-launcher" marker entry back to its own app/python/app/* source
  # entry (see rules/package.bzl's kind-handler dispatch).
  owner = field(str),
)

# Two independent transitive_sets (rather than one combined one) since
# entries and needs have unrelated shapes and are consumed independently by
# rules/package.bzl (entries drive staging; needs drive toolchain resolution).
PackageEntrySet = transitive_set()
PackageNeedSet = transitive_set()

PackageInfo = provider(fields = ["entries", "needs"])

_EMPTY_PACKAGE_INFO = PackageInfo(entries = None, needs = None)

# Every packaging-aware rule's attrs should include this (merged in via `|`,
# same pattern as rules/cxx.bzl's _TOOLCHAIN_ATTRS/_PROFILE_ATTRS).
PACKAGE_LABELS_ATTR = {
  "labels": attrs.list(attrs.string(), default = []),
}

def _dep_package_infos(deps):
  return [d[PackageInfo] for d in deps if PackageInfo in d]

def package_info(ctx: AnalysisContext, entries: list = [], needs: list = [], deps: list = []) -> PackageInfo:
  """Builds this target's own PackageInfo, folding in `deps`' PackageInfo (if
  any) as transitive_set children. No-ops when "no-package" is one of this
  target's own `labels`."""
  if "no-package" in ctx.attrs.labels:
    return _EMPTY_PACKAGE_INFO

  infos = _dep_package_infos(deps)
  entry_children = [i.entries for i in infos if i.entries != None]
  need_children = [i.needs for i in infos if i.needs != None]

  entries_tset = None
  if entries or entry_children:
    entries_tset = ctx.actions.tset(PackageEntrySet, value = list(entries), children = entry_children)

  needs_tset = None
  if needs or need_children:
    needs_tset = ctx.actions.tset(PackageNeedSet, value = list(needs), children = need_children)

  return PackageInfo(entries = entries_tset, needs = needs_tset)

def flatten_package_entries(info: [PackageInfo, None]) -> list:
  # Traversal order is not guaranteed stable/sorted; rules/package.bzl's
  # package() rule sorts by `dest` itself before staging, so callers here
  # don't need to care about order.
  if info == None or info.entries == None:
    return []
  out = []
  for value in info.entries.traverse():
    out += value
  return out

def flatten_package_needs(info: [PackageInfo, None]) -> list:
  if info == None or info.needs == None:
    return []
  seen = {}
  out = []
  for value in info.needs.traverse():
    for need in value:
      if need not in seen:
        seen[need] = True
        out.append(need)
  return out

def flatten_merged_package_entries(ctx: AnalysisContext, infos: list) -> list:
  """Same result shape as calling flatten_package_entries() once per `infos`
  element and concatenating - EXCEPT correct under diamond deps. Two of
  `infos`' tsets may share a descendant node (e.g. two deps of a `package()`
  target both transitively depend on the same py_library), and traversing
  each tset separately then concatenating the flattened python lists visits
  that shared node once per parent, duplicating its entries even though
  buck2's tset DAG never actually stores it twice. Building ONE new tset
  node with every info's `entries` tset as a child and traversing THAT once
  fixes it structurally: a single traverse() call visits each DAG node
  exactly once no matter how many parents reach it, so the shared
  descendant's entries surface exactly once - no post-hoc (dest, owner)
  dedup needed."""
  children = [i.entries for i in infos if i.entries != None]
  if not children:
    return []
  merged = ctx.actions.tset(PackageEntrySet, children = children)
  out = []
  for value in merged.traverse():
    out += value
  return out

def flatten_merged_package_needs(ctx: AnalysisContext, infos: list) -> list:
  """needs analogue of flatten_merged_package_entries() - see its docstring
  for why merging as children of one tset node before a single traverse()
  matters under diamond deps."""
  children = [i.needs for i in infos if i.needs != None]
  if not children:
    return []
  merged = ctx.actions.tset(PackageNeedSet, children = children)
  seen = {}
  out = []
  for value in merged.traverse():
    for need in value:
      if need not in seen:
        seen[need] = True
        out.append(need)
  return out

def check_pkg_name(ctx, pkg_name: str) -> str:
  """Validates a packaged-executable name at the emitting rule, before it is
  embedded in a PackageEntry dest. Names are single path components: a
  separator or dot-segment here would silently nest or escape under bin/
  and libexec/, sidestepping package()'s dest checks until much later."""
  if pkg_name == "" or pkg_name in (".", "..") or "/" in pkg_name:
    fail("{}: invalid pkg_name \"{}\": must be a non-empty single path component".format(
      ctx.label.raw_target(),
      pkg_name,
    ))
  return pkg_name
