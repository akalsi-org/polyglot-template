from __future__ import annotations

import importlib.util
import os
import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
INIT_PROJECT_PATH = ROOT / "tools" / "init_project.py"
SPEC = importlib.util.spec_from_file_location("init_project", INIT_PROJECT_PATH)
assert SPEC is not None and SPEC.loader is not None
init_project = importlib.util.module_from_spec(SPEC)
sys.modules[SPEC.name] = init_project
SPEC.loader.exec_module(init_project)


class InitProjectTest(unittest.TestCase):
  def test_renamed_go_tree_contains_no_old_module_and_builds(self) -> None:
    with tempfile.TemporaryDirectory() as temporary_directory:
      fork = Path(temporary_directory) / "fork"
      fork.mkdir()
      shutil.copytree(ROOT / "go", fork / "go")
      shutil.copytree(ROOT / "vendor", fork / "vendor")
      shutil.copy2(ROOT / "go.mod", fork / "go.mod")
      shutil.copy2(ROOT / "go.sum", fork / "go.sum")

      go_sources = sorted((fork / "go").rglob("*.go"))
      self.assertTrue(go_sources)
      for source in go_sources:
        with source.open("a", encoding="utf-8") as file:
          file.write(f"\n// rename sentinel: {init_project.OLD_MODULE}\n")

      init_project.rename_project(str(fork), "renamed-project", "renamed-org")

      old_module_files = [
        str(path.relative_to(fork))
        for path in [fork / "go.mod", *go_sources]
        if init_project.OLD_MODULE in path.read_text(encoding="utf-8")
      ]
      self.assertEqual([], old_module_files)

      go = shutil.which(
        "go",
        path=os.pathsep.join((str(ROOT / ".local" / "bin"), os.environ.get("PATH", ""))),
      )
      if go is None:
        self.fail("bootstrap the pinned Go toolchain before running this test")
      environment = os.environ.copy()
      environment.update({
        "CGO_ENABLED": "0",
        "GOCACHE": str(fork / ".gocache"),
        "GOENV": "off",
        "GOFLAGS": "-mod=vendor -buildvcs=false",
        "GOTOOLCHAIN": "local",
      })
      build = subprocess.run(
        [go, "build", "./go/..."],
        cwd=fork,
        env=environment,
        check=False,
        capture_output=True,
        text=True,
      )
      self.assertEqual(0, build.returncode, build.stdout + build.stderr)


if __name__ == "__main__":
  unittest.main()
