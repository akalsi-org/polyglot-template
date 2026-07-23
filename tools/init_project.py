#!/usr/bin/env python3
"""Project Initializer / Renamer for Polyglot Template.

Renames repository namespaces, Go module paths, package catalog entries, and docs
when instantiating a new repository from polyglot-template.

Usage:
  python3 tools/init_project.py <new-project-name> [new-org-name]
"""

import os
import sys

OLD_PROJECT = "polyglot-template"
OLD_ORG = "akalsi-org"
OLD_MODULE = f"github.com/{OLD_ORG}/{OLD_PROJECT}"

def main():
  if len(sys.argv) < 2:
    print("usage: ./repo.sh init-project <new-project-name> [new-org-name]", file=sys.stderr)
    sys.exit(2)

  new_project = sys.argv[1].strip()
  new_org = sys.argv[2].strip() if len(sys.argv) >= 3 else OLD_ORG
  new_module = f"github.com/{new_org}/{new_project}"

  root = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))

  targets = [
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

  print(f"Initializing new project '{new_project}' (org: '{new_org}')...")

  for relative_path in targets:
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
  print("Run './repo.sh format' and './repo.sh test' to verify your new project graph.")

if __name__ == "__main__":
  main()
