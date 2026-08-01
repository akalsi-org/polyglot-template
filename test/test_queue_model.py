#!/usr/bin/env python3
"""Repository-level tests for the executable SPSC/MPSC protocol model."""

from __future__ import annotations

import importlib.util
import pathlib
import sys
import unittest


ROOT = pathlib.Path(__file__).resolve().parents[1]
MODEL_PATH = ROOT / "docs" / "verification" / "queue_model.py"


def load_model():
  spec = importlib.util.spec_from_file_location("queue_model", MODEL_PATH)
  assert spec is not None
  module = importlib.util.module_from_spec(spec)
  assert spec.loader is not None
  sys.modules[spec.name] = module
  spec.loader.exec_module(module)
  return module


class QueueProtocolModelTest(unittest.TestCase):
  @classmethod
  def setUpClass(cls):
    cls.model = load_model()

  def test_base_models_pass_with_declared_bounds(self):
    results = self.model.run_suite("all", include_mutants=False)
    self.assertGreater(results["spsc:base"]["states"], 0)
    self.assertGreater(results["mpsc:base"]["states"], 0)
    self.assertEqual(results["spsc:base"]["depth"], self.model.MAX_SPSC_DEPTH)
    self.assertEqual(results["mpsc:base"]["depth"], self.model.MAX_MPSC_DEPTH)

  def test_negative_control_mutants_are_rejected(self):
    results = self.model.run_suite("all", include_mutants=True)
    for mutant in self.model.SPSC_MUTANTS:
      self.assertIn(f"spsc:{mutant}", results)
      self.assertIn("trace", results[f"spsc:{mutant}"])
    for mutant in self.model.MPSC_MUTANTS:
      self.assertIn(f"mpsc:{mutant}", results)
      self.assertIn("trace", results[f"mpsc:{mutant}"])


if __name__ == "__main__":
  unittest.main()
