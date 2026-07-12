#!/usr/bin/env python3

import importlib.util
import json
import sys
import tempfile
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
SPEC = importlib.util.spec_from_file_location("native_graph", ROOT / "tools" / "native_graph.py")
native_graph = importlib.util.module_from_spec(SPEC)
assert SPEC.loader
sys.modules[SPEC.name] = native_graph
SPEC.loader.exec_module(native_graph)


class NativeGraphTest(unittest.TestCase):
    def configure(self, profile: str) -> Path:
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        output = Path(temporary.name)
        native_graph.configure(ROOT, ROOT / "native" / "native.toml", profile, output)
        return output

    def test_dbg_and_opt_policy(self):
        dbg = json.loads((self.configure("dbg") / "compile_commands.json").read_text())
        opt = json.loads((self.configure("opt") / "compile_commands.json").read_text())
        self.assertTrue(all("-std=gnu++26" in row["arguments"] for row in dbg + opt))
        self.assertTrue(all(not any(flag.startswith("-fsanitize") for flag in row["arguments"]) for row in dbg + opt))
        self.assertTrue(all("-fno-omit-frame-pointer" in row["arguments"] for row in dbg))
        self.assertTrue(all("-fno-omit-frame-pointer" not in row["arguments"] for row in opt))
        self.assertTrue(all("-fhardened" not in row["arguments"] for row in dbg + opt))
        self.assertTrue(all("-ftrivial-auto-var-init=zero" not in row["arguments"] for row in dbg + opt))

    def test_third_party_is_in_canonical_compdb_by_default(self):
        output = self.configure("dbg")
        compdb = json.loads((output / "compile_commands.json").read_text())
        sources = {Path(row["file"]).relative_to(ROOT).as_posix() for row in compdb}
        self.assertEqual(sources, {"native/src/hello.cpp", "native/third_party/example/example.cpp"})

    def test_adapter_projection_reconciles_with_owned_compdb_subset(self):
        output = self.configure("dbg")
        compdb = json.loads((output / "compile_commands.json").read_text())
        actions = json.loads((output / "native-actions.json").read_text())["actions"]
        adapter = actions["adapter:example-vendor"]
        owned_sources = {row["file"] for row in adapter}
        canonical_subset = [row for row in compdb if row["file"] in owned_sources]
        self.assertEqual(adapter, canonical_subset)

    def test_generation_is_deterministic(self):
        output = self.configure("opt")
        before = {path.name: path.read_bytes() for path in output.iterdir()}
        native_graph.configure(ROOT, ROOT / "native" / "native.toml", "opt", output)
        after = {path.name: path.read_bytes() for path in output.iterdir()}
        self.assertEqual(before, after)

    def test_ninja_creates_output_directories(self):
        ninja = (self.configure("dbg") / "build.ninja").read_text()
        self.assertIn("mkdir -p build/native/dbg/obj/", ninja)
        self.assertIn("mkdir -p build/native/dbg/bin", ninja)

    def test_reflection_probe_contract(self):
        data = native_graph.load_manifest(ROOT / "native" / "native.toml")
        toolchain = data["toolchain"]
        self.assertEqual(toolchain["standard"], "gnu++26")
        self.assertEqual(toolchain["reflection_flag"], "-freflection")
        self.assertTrue((ROOT / "native" / "probes" / "reflection.cpp").is_file())


if __name__ == "__main__":
    unittest.main()
