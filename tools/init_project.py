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
