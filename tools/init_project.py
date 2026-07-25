#!/usr/bin/env python3
"""Project Initializer / Renamer for Polyglot Template.

Renames repository namespaces, Go module paths, package catalog entries, and
docs when instantiating a new repository from polyglot-template, then walks
the maintainer through pruning two categories of template-only content:

1. This template's OWN self-referential test files (TEST_STRIP_CANDIDATES) -
   the ones that assert exact strings from THIS repo's docs or CI YAML, or
   that only pass because of the demo catalog/example code this template
   ships with. Those tests have no value in a fork and are guaranteed to
   start failing the moment a fork does what forking is for (rewrite the
   README, customize CI, replace the demo catalog).
2. The leaf demo code itself (DEMO_STRIP_GROUPS) - the greeting/hello
   example library and app in the C++ and Python lanes, whose only purpose
   is to give a fresh clone something that builds. Removing a group also
   applies the exact edits its removal requires elsewhere in the graph
   (BUCK dep lists, packages/BUCK, packages/catalog.bzl), so the graph
   stays buildable rather than merely smaller.

The intended workflow is one command on a fresh clone: it renames, strips
both categories of template-only content, and rewrites git history down to a
single "Initial commit" with no upstream remote. It never pushes - the exact
commands to create and push to your own remote are printed at the end.

Usage:
  python3 tools/init_project.py <new-project-name> [new-org-name]
                                [--keep-all-tests | --choose-tests]
                                [--keep-all-demos | --choose-demos]
                                [--keep-history]

Both pruning passes STRIP BY DEFAULT: a fork that wanted this template's
self-referential contracts and demo scaffolding would not be running this
script. --keep-all-tests / --keep-all-demos opt out wholesale;
--choose-tests / --choose-demos restore the per-file interactive walkthrough.

MAINTAINER NOTE (read this before adding a new test/ file): if a new
test/*.sh or test/*.py file's assertions are specific to THIS repository's
own prose, CI YAML, or demo catalog/example code - rather than testing
reusable Buck2 rule/tooling behavior - add it to TEST_STRIP_CANDIDATES below.
Nothing else discovers these files automatically; a file left off this list
silently ships into every future fork and rots there. See
.agents/md/overview.md for the fuller version of this note.

MAINTAINER NOTE (demo pruning): DEMO_STRIP_GROUPS is deliberately limited to
LEAF demo code - content nothing else in the template depends on once the
group's own declared edits are applied. Whole lanes (go/, ts/, tsweb/) and
the demo package entries are NOT candidates: removing those would require
this script to also rewrite repo.sh's per-lane verbs, root //:deno-cache's
hand-listed closure (which `./repo.sh lint` checks fail-closed), go/BUCK's
lint dep lists, go.mod/vendor/, and .github/workflows/verify.yml. That is a
fork's own editing job, not a scripted one.

KEEP-LIST - reusable infrastructure that is NEVER a prune candidate, listed
here because some of it does not look like infrastructure at a glance:
  cpp/lib/pyfast/**                 the CPython fast-call header library
  python/test/pyfast_test_ext.c     pyfast's ONLY test fixture; it lives in
  python/test/extension_init.py     python/test/ and reads like demo code,
  python/test/pyfast_test_ext/**    but //cpp/lib/pyfast:leak_check and
  python/test/test_pyfast_extension.py  test_pyfast_extension.py are what
                                    prove pyfast.h works at all
  python/lib/testlib.py             the test framework, plus its self-test
  python/test/test_testlib.py       and the runner that imports TestContext
  tools/py_test_runner.py           (testlib is inert without it)
  tools/py_cover.py, tools/coverage_*.py
  python/lib/fastbytes/**           the worked py_extension example (C++
                                    srcs + stubs + py.typed) a fork copies
                                    when adding its own extension
  cpp/test/doctest_runner.cc        shared per-configuration test runner
  cpp/test/reflection.cc            rules/toolchain.bzl's C++26 reflection
                                    capability probe - deleting cpp/test/
                                    breaks the TOOLCHAIN, not just tests
  cpp/lib/pgt/core/**               generic fixed-width/platform headers
  rules/, tools/, toolchain/, config/, platforms/, bxl/
"""

from __future__ import annotations

import argparse
import os
import re
import shutil
import subprocess
import sys
from dataclasses import dataclass

OLD_PROJECT = "polyglot-template"
OLD_ORG = "akalsi-org"
OLD_MODULE = f"github.com/{OLD_ORG}/{OLD_PROJECT}"

RENAME_TARGETS = [
  "README.md",
  "CHANGELOG.md",
  "go.mod",
  "deno.json",
  "packages/catalog.bzl",
  "packages/BUCK",
  "bxl/compdb.bxl",
  "rules/cxx.bzl",
  "rules/go.bzl",
  "rules/python.bzl",
  "rules/deno.bzl",
  "rules/package.bzl",
  "cpp/lib/example/example.cc",
  "cpp/test/example_test.cc",
  "go/app/hello/main.go",
  "go/app/hello/main_test.go",
  "go/test/greeting_test.go",
  "tsweb/app/site/index.html",
  "tsweb/lib/app/app.tsx",
  "tsweb/lib/title/title.ts",
  "tsweb/test/title_test.ts",
  "docs/LANGUAGE-GUIDE.md",
  # `repo=akalsi-org/polyglot-template` in the coverage-artifact fetch recipe.
  "docs/CI-RELEASE.md",
]

# LICENSE is deliberately NOT renamed: "Copyright (c) <year> akalsi-org" is a
# statement about who holds copyright in this template's code, not a namespace.
# A fork changing it is asserting authorship, which is the fork's call to make
# by hand.


# The demo packages are named after this template, and their names contain no
# "polyglot-template" for RENAME_TARGETS to catch: a fork shipped real
# archives called polyglot-demo/polyglot-server, and .github/workflows/
# verify.yml released `package=polyglot-demo` at `packages/polyglot-demo/
# v0.1.0`. The "polyglot-" prefix is rewritten to "<project>-" in these files
# AFTER the polyglot-template rename above has already run, so the only
# remaining "polyglot-" occurrences are package names and CI cache-key
# prefixes - both of which SHOULD carry the new project's name.
PACKAGE_RENAME_TARGETS = [
  "packages/catalog.bzl",
  "packages/BUCK",
  ".github/workflows/verify.yml",
  ".github/workflows/package-release.yml",
  "infra/deploy/BUCK",
  "docs/ARCHITECTURE.md",
  "docs/CI-RELEASE.md",
  "docs/PRINCIPLES.md",
  "README.md",
  ".agents/md/overview.md",
  # Rule and tool comments that name //packages:polyglot-demo as the worked
  # example; they go stale the moment the package is renamed. NOT a blanket
  # sweep of rules/ - rules/group.bzl names the retired `polyglot_package`
  # rule, which is history rather than package identity and has no new name
  # to take.
  "rules/go.bzl",
  "rules/package.bzl",
  "rules/python.bzl",
  "tools/package_release.py",
  "tsweb/BUCK",
  # A fixture string ("polyglot-test") written and read back by one testlib
  # self-test; arbitrary, but it is the last thing in a fork still saying
  # "polyglot", and renaming both halves keeps the assertion true.
  "python/test/test_testlib.py",
]
OLD_PACKAGE_PREFIX = "polyglot-"

# The deployment plan/observation schema identifiers are also named after this
# template ("polyglot.deployment-plan/v1"). They are a wire contract between
# rules/deploy.bzl and infra/deploy/readonly.py and appear nowhere else, so
# they rename together safely.
SCHEMA_RENAME_TARGETS = [
  "infra/deploy/readonly.py",
  "rules/deploy.bzl",
]
OLD_SCHEMA_PREFIX = "polyglot.deployment-"

# The C++ namespace, its include root, and its Buck target are all named after
# this template ("namespace pgt", `#include "pgt/core/types.hh"`,
# //cpp/lib/pgt/core:pgt_core). Nothing here contains "polyglot-template"
# either, so every fork kept a pgt:: namespace. cpp/lib/pgt is renamed to the
# project's own slug and these files rewritten to match.
OLD_NAMESPACE = "pgt"
NAMESPACE_RENAME_TARGETS = [
  "cpp/lib/pgt/core/BUCK",
  "cpp/lib/pgt/core/platform.hh",
  "cpp/lib/pgt/core/types.hh",
  "cpp/test/BUCK",
  "cpp/test/core_test.cc",
  "rules/cxx.bzl",
  "docs/LANGUAGE-GUIDE.md",
]

def namespace_slug(new_project: str) -> str:
  """A C++-identifier-safe namespace derived from the project name."""
  slug = re.sub(r"[^a-z0-9]+", "_", new_project.lower()).strip("_")
  if not slug:
    return OLD_NAMESPACE
  if slug[0].isdigit():
    slug = "ns_" + slug
  return slug


@dataclass
class TestStripCandidate:
  path: str
  # A snippet unique enough to identify the exact repo.sh infra-test line
  # that invokes this file - NOT the whole line, so minor formatting drift
  # in repo.sh doesn't break the match.
  invocation_snippet: str
  rationale: str


# Ordered clearest-self-referential-first, so the interactive prompt below
# asks about the least ambiguous removals before the judgment calls. See the
# MAINTAINER NOTE in this module's docstring for how to keep this in sync.
TEST_STRIP_CANDIDATES = [
  TestStripCandidate(
    path="test/docs-contract.sh",
    invocation_snippet="test/docs-contract.sh",
    rationale=(
      "Asserts exact prose strings from THIS repository's own README,\n"
      "  ARCHITECTURE.md, CI-RELEASE.md, and CHANGELOG.md. It tests nothing\n"
      "  about your project - it will start failing the moment you rewrite\n"
      "  any of those files, which forking implies you will."
    ),
  ),
  TestStripCandidate(
    path="test/workflow-contract.sh",
    invocation_snippet="test/workflow-contract.sh",
    rationale=(
      "Asserts exact step names, order, and structure of THIS repository's\n"
      "  own .github/workflows/*.yml. Breaks the moment you customize CI -\n"
      "  different runners, a different matrix, different package names -\n"
      "  which most forks do immediately."
    ),
  ),
  TestStripCandidate(
    path="test/test-package-model.sh",
    invocation_snippet="test/test-package-model.sh",
    rationale=(
      "Tests real, reusable package-resolution logic (determinism, tamper\n"
      "  detection, floating-version rejection) but only through the demo\n"
      "  catalog's 'polyglot-demo'/'runtime-python' entries. Keep this if you\n"
      "  keep the demo catalog as a working example; it needs rewriting\n"
      "  against your real packages otherwise."
    ),
  ),
  TestStripCandidate(
    path="test/test-package-release.sh",
    invocation_snippet="test/test-package-release.sh",
    rationale=(
      "Tests real release/checksum/evidence logic but only through the\n"
      "  demo catalog's 'gateway' entry. Same caveat as test-package-model.sh:\n"
      "  keep it with the demo catalog, or rewrite it against your real\n"
      "  packages. Removing this test does NOT remove the 'gateway' catalog\n"
      "  entry itself - that is a separate decision in packages/catalog.bzl."
    ),
  ),
  TestStripCandidate(
    path="test/go-vendor-contract.sh",
    invocation_snippet="test/go-vendor-contract.sh",
    rationale=(
      "Tests the real vendored-Go build mechanism (GOFLAGS=-mod=vendor,\n"
      "  offline module resolution) but only through the demo's go-cmp\n"
      "  import in go/test/greeting_test.go. Breaks if you remove that\n"
      "  demo test without keeping an equivalent vendored dependency."
    ),
  ),
  TestStripCandidate(
    path="test/editor-contract.py",
    invocation_snippet="test/editor-contract.py",
    rationale=(
      "Asserts this template's own chosen editor settings (2-space indent,\n"
      "  clang-format width, Deno fmt config) match .vscode/.editorconfig/\n"
      "  .clang-format. Keep it if you keep those conventions; low value\n"
      "  otherwise."
    ),
  ),
]


@dataclass
class Edit:
  """One declarative edit a demo group must make to a file it does not own.

  kind is one of:
    "drop_lines"   - remove every line whose stripped text equals one of
                     `values`. Stripped equality, not substring: prose in a
                     BUCK header comment that happens to name the same
                     target must survive.
    "drop_target"  - remove the whole top-level rule invocation whose body
                     declares `name = "<value>",`, plus the contiguous
                     comment block immediately above it.
    "drop_range"   - remove the lines from the first one containing
                     values[0] up to (not including) the next one containing
                     values[1]. Used for whole prose sections, which are
                     bounded by their neighbours rather than by exact text.
    "replace_text" - exact substring values[0] -> values[1], anywhere in the
                     file, including across line breaks. For prose surgery
                     where a sentence, not a line, is the unit.
    "reset_file"   - overwrite the file with values[0].
    "drop_item"    - starting at the line containing `anchor`, find the
                     first following line that mentions `"<value>"` and
                     remove that inline list item, and its separating comma,
                     from that one line. The anchor scopes the search so a
                     name that appears in several one-line lists is only
                     removed from the intended entry's.
  """

  path: str
  kind: str
  values: tuple[str, ...]
  anchor: str = ""


# Applied unconditionally, because these are wrong in EVERY fork rather than
# being a matter of taste: a spawned project whose README still explains how
# to instantiate the template, and whose CHANGELOG is the template's own
# release history.
def template_prose_edits(new_project: str) -> tuple[Edit, ...]:
  return (
    Edit(
      "README.md", "replace_text",
      ("# Polyglot Template Bootstrap Slice", f"# {new_project}"),
    ),
    Edit(
      "README.md", "drop_range",
      (
        "### Instantiating A New Repository From This Template",
        "Running `./repo.sh` starts an interactive shell",
      ),
    ),
    Edit("CHANGELOG.md", "reset_file", ("# Changelog\n",)),
  )


@dataclass
class DemoStripGroup:
  key: str
  title: str
  paths: tuple[str, ...]
  edits: tuple[Edit, ...]
  rationale: str
  # Things this removal leaves stale that the script deliberately does NOT
  # rewrite - repo.sh's own per-lane convenience verbs. Printed at the end so
  # a fork is told exactly what it now owns rather than discovering it at the
  # next `./repo.sh cpp-run`.
  warnings: tuple[str, ...] = ()


# Leaf demo code only - see this module's docstring for why whole lanes and
# the demo package entries are deliberately absent. Each group's `edits` must
# leave the Buck2 graph buildable on its own, so groups can be chosen in any
# combination.
DEMO_STRIP_GROUPS = [
  DemoStripGroup(
    key="cpp-demo",
    title="C++ demo library and hello app",
    paths=(
      "cpp/lib/example",
      "cpp/app/hello",
      "cpp/test/example_test.cc",
      "cpp/test/example_edge_test.cc",
    ),
    edits=(
      Edit("cpp/test/BUCK", "drop_target", ("example_test", "example_edge_test")),
      Edit("packages/BUCK", "drop_lines", ('"//cpp/app/hello:hello",', '"bin/cpp-hello": [],')),
      Edit("packages/catalog.bzl", "drop_item", ("cpp-hello",), anchor='"name": "polyglot-demo"'),
      # graph-compdb-contract.sh asserts the BXL compdb covers real graph
      # compile actions by naming four of them. The two C++ demo sources go
      # with the demo; fastbytes.cc and pyfast_test_ext.c remain, so the
      # contract still asserts something real.
      Edit(
        "test/graph-compdb-contract.sh", "drop_lines",
        ('"cpp/app/hello/main.cc",', '"cpp/lib/example/example.cc",'),
      ),
    ),
    rationale=(
      "`example_message()` and the binary that prints it exist only so a\n"
      "  fresh clone has something in the C++ lane that builds. Removing this\n"
      "  also drops the two cxx_test targets that test it, //cpp/app/hello\n"
      "  from the polyglot-demo package, and 'cpp-hello' from the catalog.\n"
      "  KEPT: cpp/lib/pgt/core (generic headers), cpp/test/doctest_runner.cc,\n"
      "  and cpp/test/reflection.cc - the last is rules/toolchain.bzl's\n"
      "  reflection capability probe, so cpp/test/ itself must survive."
    ),
    warnings=(
      "repo.sh's cpp-build, cpp-run, and cpp-test verbs still name "
      "//cpp/app/hello:hello; point them at your own C++ binary or drop them.",
    ),
  ),
  DemoStripGroup(
    key="python-demo",
    title="Python demo library and hello app",
    paths=(
      "python/lib/example",
      "python/app/hello",
      "python/test/test_example.py",
    ),
    edits=(
      Edit(
        "python/test/BUCK", "drop_lines",
        (
          '"test_example.py",',
          '"//python/lib/example:example",',
          '"//python/app/hello:hello",',
          '"python/app",',
        ),
      ),
      Edit(
        "packages/BUCK", "drop_lines",
        (
          '"//python/app/hello:hello",',
          '"bin/python-hello": [],',
          '"bin/python": ["--version"],',
          '"bin/python3": ["--version"],',
        ),
      ),
      Edit("packages/catalog.bzl", "drop_item", ("python-hello",), anchor='"name": "polyglot-demo"'),
    ),
    rationale=(
      "python/lib/example's greeting() and the app that prints it are demo\n"
      "  scaffolding. Removing the app also removes polyglot-demo's only\n"
      "  py_binary, and rules/package.bzl stages the shared bin/python +\n"
      "  bin/python3 launchers only when a py-app-launcher entry is present -\n"
      "  so those two smoke commands are dropped with it.\n"
      "  KEPT: python/lib/fastbytes (the worked py_extension example),\n"
      "  python/lib/testlib.py and its self-test, and the whole pyfast test\n"
      "  fixture under python/test - that fixture is pyfast.h's only test."
    ),
    warnings=(
      "repo.sh's python-build verb still names //python/app/hello:hello; "
      "point it at your own py_binary or drop it.",
    ),
  ),
  DemoStripGroup(
    key="history-docs",
    title="This template's own design history",
    paths=(
      "docs/IMPROVEMENT-ROADMAP.md",
      "docs/DESIGN-README.md",
      "docs/proposals",
      "docs/spikes",
    ),
    edits=(
      # Every surviving reference to the removed pages, so a fork is not left
      # with dead links. Each fails closed if the prose has drifted.
      Edit(
        "README.md", "replace_text",
        (
          " See [the historical parallel-build spike](docs/spikes/parallel-build.md),"
          " which predates the Buck2 migration.",
          "",
        ),
      ),
      Edit(
        "docs/CURRENT-CAPABILITIES.md", "replace_text",
        (
          "verification. Design material in [DEPLOYMENT.md](proposals/DEPLOYMENT.md),\n"
          "[FLEET.md](proposals/FLEET.md), and [DESIGN-README.md](DESIGN-README.md) remains a\n"
          "proposal unless it is listed here.\n",
          "verification.\n",
        ),
      ),
      Edit(
        "docs/PRINCIPLES.md", "replace_text",
        (
          "enforcement: proposed - P15 through P18 describe the deployment and "
          "fleet design in docs/proposals/. Only the read-only",
          "enforcement: proposed - only the read-only",
        ),
      ),
      Edit(
        "test/docs-contract.sh", "drop_lines",
        (
          "grep -Fq 'Status: the canonical catalog' \"$root/docs/IMPROVEMENT-ROADMAP.md\"",
          "grep -Fq 'Status: the bootstrap-first README Quick Start' \"$root/docs/IMPROVEMENT-ROADMAP.md\"",
          "grep -Fq 'Status: proposed implementation baseline' \"$root/docs/proposals/FLEET.md\"",
          "grep -Fq 'Status: proposed design baseline' \"$root/docs/DESIGN-README.md\"",
        ),
      ),
    ),
    rationale=(
      "The audit roadmap, design index, deployment/fleet proposals, and the\n"
      "  parallel-build and serialization spikes are THIS template's own\n"
      "  design history - roughly 60K of decisions your project did not make\n"
      "  and cannot act on. The reference docs a fork actually uses\n"
      "  (ARCHITECTURE, PRINCIPLES, LANGUAGE-GUIDE, TOOLCHAIN-LIFECYCLE,\n"
      "  CI-RELEASE, CURRENT-CAPABILITIES, EDITOR, TROUBLESHOOTING) are kept,\n"
      "  with their references to the removed pages rewritten."
    ),
  ),
]


def rename_project(root: str, new_project: str, new_org: str) -> None:
  new_module = f"github.com/{new_org}/{new_project}"
  print(f"Initializing new project '{new_project}' (org: '{new_org}')...")
  for relative_path in RENAME_TARGETS:
    full_path = os.path.join(root, relative_path)
    if not os.path.isfile(full_path):
      continue
    with open(full_path, "r", encoding="utf-8") as f:
      content = f.read()
    updated = content.replace(OLD_MODULE, new_module)
    updated = updated.replace(OLD_PROJECT, new_project)
    if new_org != OLD_ORG:
      updated = updated.replace(OLD_ORG, new_org)
    if updated != content:
      with open(full_path, "w", encoding="utf-8") as f:
        f.write(updated)
      print(f"  updated: {relative_path}")
  print(f"\nProject '{new_project}' successfully initialized.")


def rename_packages(root: str, new_project: str) -> None:
  """Rewrite the demo packages' identity to the new project's own name.

  Runs AFTER rename_project, so every "polyglot-template" is already gone and
  the only "polyglot-" left is package identity (polyglot-demo,
  polyglot-server) and CI cache-key prefixes - all of which should carry the
  project's name.
  """
  substitutions = [
    (PACKAGE_RENAME_TARGETS, OLD_PACKAGE_PREFIX, f"{new_project}-"),
    (SCHEMA_RENAME_TARGETS, OLD_SCHEMA_PREFIX, f"{new_project}.deployment-"),
  ]
  for targets, old, new in substitutions:
    for relative_path in targets:
      full_path = os.path.join(root, relative_path)
      if not os.path.isfile(full_path):
        continue
      with open(full_path, "r", encoding = "utf-8") as f:
        content = f.read()
      updated = content.replace(old, new)
      if updated != content:
        with open(full_path, "w", encoding = "utf-8") as f:
          f.write(updated)
        print(f"  packages: {relative_path}")


def rename_namespace(root: str, new_project: str) -> None:
  """Rename the pgt C++ namespace, include root, and Buck target to the project."""
  slug = namespace_slug(new_project)
  if slug == OLD_NAMESPACE:
    return
  for relative_path in NAMESPACE_RENAME_TARGETS:
    full_path = os.path.join(root, relative_path)
    if not os.path.isfile(full_path):
      continue
    with open(full_path, "r", encoding = "utf-8") as f:
      content = f.read()
    # "pgt" appears only as the namespace, the include root, and the target
    # name in these files - all of which move together.
    updated = content.replace(OLD_NAMESPACE, slug)
    if updated != content:
      with open(full_path, "w", encoding = "utf-8") as f:
        f.write(updated)
      print(f"  namespace: {relative_path}")
  # Last, because every path above is relative to the pre-move location.
  old_dir = os.path.join(root, "cpp", "lib", OLD_NAMESPACE)
  new_dir = os.path.join(root, "cpp", "lib", slug)
  if os.path.isdir(old_dir) and not os.path.exists(new_dir):
    shutil.move(old_dir, new_dir)
    print(f"  namespace: cpp/lib/{OLD_NAMESPACE}/ -> cpp/lib/{slug}/")


def prompt_yes_no(question: str) -> bool:
  answer = input(question).strip().lower()
  return answer in ("y", "yes")


def choose_candidates_interactively(root: str) -> list[TestStripCandidate]:
  print(
    "\nThis template ships its OWN self-referential test files - contracts\n"
    "that verify THIS repository's docs, CI, and demo catalog, not your\n"
    "project. Going through them one at a time:\n"
  )
  chosen: list[TestStripCandidate] = []
  for candidate in TEST_STRIP_CANDIDATES:
    if not os.path.isfile(os.path.join(root, candidate.path)):
      continue
    print(f"\n{candidate.path}")
    print(f"  {candidate.rationale}")
    if prompt_yes_no("  Remove this file? [y/N]: "):
      chosen.append(candidate)
  return chosen


def strip_repo_sh_invocations(root: str, candidates: list[TestStripCandidate]) -> None:
  if not candidates:
    return
  repo_sh = os.path.join(root, "repo.sh")
  with open(repo_sh, "r", encoding="utf-8") as f:
    lines = f.readlines()
  snippets = [candidate.invocation_snippet for candidate in candidates]
  kept = [line for line in lines if not any(snippet in line for snippet in snippets)]
  if kept != lines:
    with open(repo_sh, "w", encoding="utf-8") as f:
      f.writelines(kept)


def strip_test_files(root: str, candidates: list[TestStripCandidate]) -> None:
  for candidate in candidates:
    full_path = os.path.join(root, candidate.path)
    if os.path.isfile(full_path):
      os.remove(full_path)
      print(f"  removed: {candidate.path}")
  strip_repo_sh_invocations(root, candidates)


INITIAL_COMMIT_MESSAGE = "Initial commit"
DEFAULT_BRANCH = "main"


def _git(root: str, *args: str, check: bool = True) -> subprocess.CompletedProcess[str]:
  return subprocess.run(
    ("git", "-C", root) + args,
    check=check, capture_output=True, text=True,
  )


def rewrite_history(root: str, new_project: str, new_org: str) -> bool:
  """Collapse this clone's history into one "Initial commit" on a fresh branch.

  Deliberately an orphan commit rather than `rm -rf .git`: the visible result
  is identical - one commit, no tags, no branches, no upstream - but the
  template's objects survive in the reflog until a gc, so running this in the
  wrong directory is recoverable for a while instead of instantly final.

  Never pushes, and removes `origin` precisely so a later `git push` cannot
  reach the template's own repository. main() prints the commands to attach
  your own remote.
  """
  if _git(root, "rev-parse", "--git-dir", check=False).returncode != 0:
    print("\nNot a git repository - skipping the history rewrite.")
    return False

  previous = _git(root, "rev-parse", "--short", "HEAD", check=False).stdout.strip()
  temporary_branch = "init-project-orphan"
  _git(root, "checkout", "--orphan", temporary_branch)
  _git(root, "add", "-A")
  commit = _git(root, "commit", "-m", INITIAL_COMMIT_MESSAGE, check=False)
  if commit.returncode != 0:
    sys.exit(
      "init_project.py: the initial commit failed. git said:\n"
      + (commit.stderr or commit.stdout).rstrip()
      + "\n\nThe rename and pruning are already applied to your working tree; "
        "fix the above (commonly an unset user.name/user.email) and commit "
        "by hand."
    )

  # Every other branch and tag belongs to the template's history, not yours.
  for line in _git(root, "for-each-ref", "--format=%(refname:short)", "refs/heads/").stdout.split():
    if line != temporary_branch:
      _git(root, "branch", "-D", line)
  for tag in _git(root, "tag").stdout.split():
    _git(root, "tag", "-d", tag)
  _git(root, "branch", "-m", DEFAULT_BRANCH)
  if _git(root, "remote", "get-url", "origin", check=False).returncode == 0:
    _git(root, "remote", "remove", "origin")

  print(f"\nHistory rewritten: one commit ({INITIAL_COMMIT_MESSAGE!r}) on '{DEFAULT_BRANCH}'.")
  if previous:
    print(f"  The template's history ended at {previous}; it is still in this clone's")
    print("  reflog until a `git gc`, so this is recoverable until then.")
  print("  'origin' removed - nothing here can push to the template.")
  return True


def print_push_instructions(new_project: str, new_org: str) -> None:
  # Explicit ssh:// rather than the scp-like git@github.com:org/repo form.
  # Both are SSH and both work; this one matches the URL style this template's
  # own origin uses, and unlike scp-like syntax it can carry a port if a fork
  # ever needs one.
  print(
    "\nThis script never pushes. To publish, create the empty repository and:\n"
    f"\n  git remote add origin ssh://git@github.com/{new_org}/{new_project}"
    f"\n  git push -u origin {DEFAULT_BRANCH}\n"
    "\nOr, with the GitHub CLI doing both:\n"
    f"\n  gh repo create {new_org}/{new_project} --private --source=. --remote=origin --push\n"
  )


def _read_lines(path: str) -> list[str]:
  with open(path, "r", encoding="utf-8") as f:
    return f.readlines()


def _write_lines(path: str, lines: list[str]) -> None:
  with open(path, "w", encoding="utf-8") as f:
    f.writelines(lines)


def _apply_drop_lines(lines: list[str], values: tuple[str, ...]) -> list[str]:
  wanted = set(values)
  return [line for line in lines if line.strip() not in wanted]


def _apply_drop_target(lines: list[str], values: tuple[str, ...], path: str) -> list[str]:
  for target in values:
    declaration = 'name = "{}",'.format(target)
    start = None
    for index, line in enumerate(lines):
      if line[:1].isspace() or not line.rstrip("\n").endswith("("):
        continue
      end = index
      while end < len(lines) and lines[end].rstrip("\n") != ")":
        end += 1
      if end == len(lines):
        continue
      if any(body.strip() == declaration for body in lines[index:end]):
        start = index
        break
    if start is None:
      sys.exit(
        "init_project.py: {}: no top-level rule named {!r} to remove - this "
        "file has drifted from DEMO_STRIP_GROUPS".format(path, target)
      )
    # Absorb the contiguous comment block documenting the target, and one
    # blank separator line after it, so the file reads as if it never existed.
    while start > 0 and lines[start - 1].lstrip().startswith("#"):
      start -= 1
    end += 1
    if end < len(lines) and not lines[end].strip():
      end += 1
    lines = lines[:start] + lines[end:]
  return lines


def _apply_drop_item(lines: list[str], values: tuple[str, ...], anchor: str, path: str) -> list[str]:
  for item in values:
    quoted = '"{}"'.format(item)
    start = next((i for i, line in enumerate(lines) if anchor in line), None)
    if start is None:
      sys.exit("init_project.py: {}: anchor {!r} not found".format(path, anchor))
    index = next((i for i in range(start, len(lines)) if quoted in lines[i]), None)
    if index is None:
      sys.exit(
        "init_project.py: {}: no list item {!r} after {!r} - this file has "
        "drifted from DEMO_STRIP_GROUPS".format(path, item, anchor)
      )
    line = lines[index]
    for pattern in (quoted + ", ", ", " + quoted, quoted + ",", quoted):
      if pattern in line:
        lines[index] = line.replace(pattern, "", 1)
        break
  return lines


def _apply_drop_range(lines: list[str], values: tuple[str, ...], path: str) -> list[str]:
  start_needle, end_needle = values
  start = next((i for i, line in enumerate(lines) if start_needle in line), None)
  if start is None:
    sys.exit("init_project.py: {}: no line containing {!r}".format(path, start_needle))
  end = next((i for i in range(start + 1, len(lines)) if end_needle in lines[i]), None)
  if end is None:
    sys.exit(
      "init_project.py: {}: found {!r} but no following {!r} to bound the "
      "section".format(path, start_needle, end_needle)
    )
  return lines[:start] + lines[end:]


def _apply_replace_text(lines: list[str], values: tuple[str, ...], path: str) -> list[str]:
  old, new = values
  text = "".join(lines)
  if old not in text:
    sys.exit(
      "init_project.py: {}: text to replace is absent - this file has drifted "
      "from init_project.py:\n  {!r}".format(path, old)
    )
  return text.replace(old, new, 1).splitlines(keepends = True)


def apply_edits(root: str, edits: tuple[Edit, ...]) -> None:
  for edit in edits:
    full_path = os.path.join(root, edit.path)
    if not os.path.isfile(full_path):
      continue
    lines = _read_lines(full_path)
    if edit.kind == "drop_lines":
      updated = _apply_drop_lines(lines, edit.values)
    elif edit.kind == "drop_target":
      updated = _apply_drop_target(list(lines), edit.values, edit.path)
    elif edit.kind == "drop_item":
      updated = _apply_drop_item(list(lines), edit.values, edit.anchor, edit.path)
    elif edit.kind == "drop_range":
      updated = _apply_drop_range(list(lines), edit.values, edit.path)
    elif edit.kind == "replace_text":
      updated = _apply_replace_text(lines, edit.values, edit.path)
    elif edit.kind == "reset_file":
      updated = [edit.values[0]]
    else:
      sys.exit("init_project.py: unknown edit kind {!r}".format(edit.kind))
    if updated != lines:
      _write_lines(full_path, updated)
      print(f"  edited:  {edit.path}")


def remove_path(root: str, relative_path: str) -> None:
  full_path = os.path.join(root, relative_path)
  if os.path.isdir(full_path):
    shutil.rmtree(full_path)
    print(f"  removed: {relative_path}/")
  elif os.path.isfile(full_path):
    os.remove(full_path)
    print(f"  removed: {relative_path}")


def choose_demo_groups_interactively(root: str) -> list[DemoStripGroup]:
  print(
    "\nThis template also ships leaf demo code - the greeting/hello example\n"
    "library and app in the C++ and Python lanes - purely so a fresh clone\n"
    "builds and tests out of the box. Removing a group also applies the\n"
    "BUCK/catalog edits that removal requires, so the graph stays buildable:\n"
  )
  chosen: list[DemoStripGroup] = []
  for group in DEMO_STRIP_GROUPS:
    if not any(os.path.exists(os.path.join(root, path)) for path in group.paths):
      continue
    print(f"\n{group.title}")
    for path in group.paths:
      print(f"    {path}")
    print(f"  {group.rationale}")
    if prompt_yes_no("  Remove this demo code? [y/N]: "):
      chosen.append(group)
  return chosen


def strip_demo_groups(root: str, groups: list[DemoStripGroup]) -> None:
  for group in groups:
    for path in group.paths:
      remove_path(root, path)
    apply_edits(root, group.edits)
  warnings = [warning for group in groups for warning in group.warnings]
  if warnings:
    print("\nLeft for you to finish - removing demo code does not rewrite repo.sh:")
    for warning in warnings:
      print(f"  - {warning}")


def main() -> None:
  parser = argparse.ArgumentParser(
    prog="init_project.py",
    description="Rename this template into a new project and prune its self-referential tests.",
  )
  parser.add_argument("new_project", help="New project/repository name")
  parser.add_argument("new_org", nargs="?", default=OLD_ORG, help="New org/owner (default: unchanged)")
  strip_mode = parser.add_mutually_exclusive_group()
  strip_mode.add_argument(
    "--keep-all-tests", action="store_true",
    help="Keep every template self-test as-is.",
  )
  strip_mode.add_argument(
    "--strip-all-tests", action="store_true",
    help="Remove every candidate self-test (the default).",
  )
  strip_mode.add_argument(
    "--choose-tests", action="store_true",
    help="Decide each candidate self-test interactively, one file at a time.",
  )
  demo_mode = parser.add_mutually_exclusive_group()
  demo_mode.add_argument(
    "--keep-all-demos", action="store_true",
    help="Keep every demo code group as-is.",
  )
  demo_mode.add_argument(
    "--strip-all-demos", action="store_true",
    help="Remove every demo code group (the default).",
  )
  demo_mode.add_argument(
    "--choose-demos", action="store_true",
    help="Decide each demo code group interactively.",
  )
  parser.add_argument(
    "--keep-history", action="store_true",
    help="Keep this clone's git history instead of collapsing it to one 'Initial commit'.",
  )
  args = parser.parse_args()

  new_project = args.new_project.strip()
  new_org = args.new_org.strip()
  if not re.match(r"^[A-Za-z0-9._-]+$", new_project) or not re.match(r"^[A-Za-z0-9._-]+$", new_org):
    sys.exit("init_project.py: project and org names must be non-empty and contain only "
             "letters, digits, '.', '_', or '-'")

  root = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
  rename_project(root, new_project, new_org)

  if (args.choose_tests or args.choose_demos) and not sys.stdin.isatty():
    sys.exit(
      "init_project.py: stdin is not a terminal, so --choose-tests/--choose-demos\n"
      "cannot ask questions. Drop those flags to take the stripping default, or\n"
      "pass --keep-all-tests/--keep-all-demos."
    )

  if args.keep_all_tests:
    print("\n--keep-all-tests: leaving every template self-test in place.")
    chosen: list[TestStripCandidate] = []
  elif args.choose_tests:
    chosen = choose_candidates_interactively(root)
  else:
    print("\nRemoving every template self-test candidate (--keep-all-tests keeps them).")
    chosen = [c for c in TEST_STRIP_CANDIDATES if os.path.isfile(os.path.join(root, c.path))]

  if chosen:
    print("\nRemoving the template self-tests you chose:")
    strip_test_files(root, chosen)
  else:
    print("\nNo template self-tests removed.")

  if args.keep_all_demos:
    print("\n--keep-all-demos: leaving every demo code group in place.")
    demo_groups: list[DemoStripGroup] = []
  elif args.choose_demos:
    demo_groups = choose_demo_groups_interactively(root)
  else:
    print("\nRemoving every demo code group (--keep-all-demos keeps them).")
    demo_groups = [
      g for g in DEMO_STRIP_GROUPS
      if any(os.path.exists(os.path.join(root, path)) for path in g.paths)
    ]

  if demo_groups:
    print("\nRemoving demo code:")
    strip_demo_groups(root, demo_groups)
  else:
    print("\nNo demo code removed.")

  # AFTER pruning, deliberately. The demo groups' edits anchor on the demo
  # packages' original names ("name": "polyglot-demo"), so renaming package
  # identity first would leave those anchors unmatchable and the run would
  # fail closed partway through.
  print("\nRenaming package identity, C++ namespace, and template prose:")
  rename_packages(root, new_project)
  rename_namespace(root, new_project)
  apply_edits(root, template_prose_edits(new_project))

  if args.keep_history:
    print("\n--keep-history: leaving this clone's git history and 'origin' untouched.")
    rewritten = False
  else:
    rewritten = rewrite_history(root, new_project, new_org)

  print(
    "\nNow verify the new project graph:\n"
    "\n  ./repo.sh bootstrap\n"
    "  ./repo.sh format && ./repo.sh lint\n"
    "  ./repo.sh test && ./repo.sh infra-test\n"
  )
  if rewritten:
    print(
      "format may rewrite files; fold any such change into the initial commit\n"
      "with `git commit --amend --no-edit` rather than leaving it dangling.\n"
    )
  print_push_instructions(new_project, new_org)


if __name__ == "__main__":
  main()
