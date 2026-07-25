#!/usr/bin/env python3
"""Project Initializer / Renamer for Polyglot Template.

Renames repository namespaces, Go module paths, package catalog entries, and
docs when instantiating a new repository from polyglot-template, then walks
the maintainer through pruning this template's OWN self-referential test
files - the ones that assert exact strings from THIS repo's docs or CI YAML,
or that only pass because of the demo catalog/example code this template
ships with. Those tests have no value in a fork and are guaranteed to start
failing the moment a fork does what forking is for (rewrite the README,
customize CI, replace the demo catalog).

Usage:
  python3 tools/init_project.py <new-project-name> [new-org-name]
                                [--keep-all-tests | --strip-all-tests]

MAINTAINER NOTE (read this before adding a new test/ file): if a new
test/*.sh or test/*.py file's assertions are specific to THIS repository's
own prose, CI YAML, or demo catalog/example code - rather than testing
reusable Buck2 rule/tooling behavior - add it to TEST_STRIP_CANDIDATES below.
Nothing else discovers these files automatically; a file left off this list
silently ships into every future fork and rots there. See
.agents/md/overview.md for the fuller version of this note.
"""

from __future__ import annotations

import argparse
import os
import re
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
]


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
    help="Skip the interactive prompt; keep every template self-test as-is.",
  )
  strip_mode.add_argument(
    "--strip-all-tests", action="store_true",
    help="Skip the interactive prompt; remove every candidate self-test without asking.",
  )
  args = parser.parse_args()

  new_project = args.new_project.strip()
  new_org = args.new_org.strip()
  if not re.match(r"^[A-Za-z0-9._-]+$", new_project) or not re.match(r"^[A-Za-z0-9._-]+$", new_org):
    sys.exit("init_project.py: project and org names must be non-empty and contain only "
             "letters, digits, '.', '_', or '-'")

  root = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
  rename_project(root, new_project, new_org)

  if args.keep_all_tests:
    print("\n--keep-all-tests: leaving every template self-test in place.")
    chosen: list[TestStripCandidate] = []
  elif args.strip_all_tests:
    print("\n--strip-all-tests: removing every template self-test candidate.")
    chosen = [c for c in TEST_STRIP_CANDIDATES if os.path.isfile(os.path.join(root, c.path))]
  elif not sys.stdin.isatty():
    sys.exit(
      "init_project.py: stdin is not a terminal, so the interactive test-pruning\n"
      "step cannot ask questions. Re-run with --keep-all-tests or --strip-all-tests\n"
      "to choose non-interactively."
    )
  else:
    chosen = choose_candidates_interactively(root)

  if chosen:
    print("\nRemoving the template self-tests you chose:")
    strip_test_files(root, chosen)
  else:
    print("\nNo template self-tests removed.")

  print(
    "\nRun './repo.sh format' and './repo.sh test' to verify your new project graph,\n"
    "and './repo.sh infra-test' to confirm the (possibly trimmed) infra test suite\n"
    "still passes."
  )


if __name__ == "__main__":
  main()
