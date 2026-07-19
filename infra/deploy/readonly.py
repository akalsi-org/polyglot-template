#!/usr/bin/env python3
"""Validate and inspect deployment plans without network or host mutation.

This is intentionally the only deployment operator surface in the repository:
`plan`, `status`, and `verify`.  `apply`, `rollback`, and remote transport are
not implemented and are not accepted command names.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import tempfile
from pathlib import Path
from typing import Any


PLAN_SCHEMA = "polyglot.deployment-plan/v1"
OBSERVATION_SCHEMA = "polyglot.deployment-observation/v1"


def canonical_json(value: object) -> bytes:
  return json.dumps(value, sort_keys = True, separators = (",", ":")).encode("utf-8")


def deployment_digest(plan: dict[str, Any]) -> str:
  return "sha256:" + hashlib.sha256(canonical_json(plan)).hexdigest()


def load_object(path: Path, description: str) -> dict[str, Any]:
  try:
    value = json.loads(path.read_text(encoding = "utf-8"))
  except (OSError, json.JSONDecodeError) as error:
    raise ValueError(f"cannot read {description} {path}: {error}") from error
  if not isinstance(value, dict):
    raise ValueError(f"{description} must be a JSON object")
  return value


def validate_plan(plan: dict[str, Any]) -> None:
  required = {"schema", "group", "environment", "target", "packages", "eligible_hosts", "routes"}
  missing = required - plan.keys()
  unknown = plan.keys() - required
  if missing or unknown:
    raise ValueError(f"plan keys mismatch: missing={sorted(missing)} unknown={sorted(unknown)}")
  if plan["schema"] != PLAN_SCHEMA:
    raise ValueError(f"unsupported plan schema: {plan['schema']!r}")
  for key in ("group", "environment", "target"):
    if not isinstance(plan[key], str) or not plan[key]:
      raise ValueError(f"plan {key} must be a non-empty string")
  if not isinstance(plan["packages"], list) or not plan["packages"] or plan["packages"] != sorted(plan["packages"]):
    raise ValueError("plan packages must be a non-empty sorted list")
  package_names: set[str] = set()
  for package in plan["packages"]:
    if not isinstance(package, str) or package.count("@") != 1:
      raise ValueError(f"invalid package specification: {package!r}")
    name, version = package.split("@", 1)
    if not name or len(version.split(".")) != 3 or not all(part.isdigit() for part in version.split(".")):
      raise ValueError(f"invalid package specification: {package!r}")
    if name in package_names:
      raise ValueError(f"duplicate package: {name}")
    package_names.add(name)
  if not isinstance(plan["eligible_hosts"], list) or not plan["eligible_hosts"] or plan["eligible_hosts"] != sorted(set(plan["eligible_hosts"])):
    raise ValueError("plan eligible_hosts must be a non-empty sorted unique list")
  if not isinstance(plan["routes"], list) or not plan["routes"]:
    raise ValueError("plan routes must be a non-empty list")
  hostnames: set[str] = set()
  for route in plan["routes"]:
    if not isinstance(route, dict) or set(route) != {"hostname", "package"}:
      raise ValueError("each route must contain only hostname and package")
    if not isinstance(route["hostname"], str) or not route["hostname"] or route["hostname"] in hostnames:
      raise ValueError(f"invalid or duplicate route hostname: {route.get('hostname')!r}")
    if route["package"] not in package_names:
      raise ValueError(f"route references undeclared package: {route['package']!r}")
    hostnames.add(route["hostname"])


def status(plan: dict[str, Any], observation: dict[str, Any]) -> tuple[dict[str, Any], list[str]]:
  validate_plan(plan)
  expected = deployment_digest(plan)
  errors: list[str] = []
  if observation.get("schema") != OBSERVATION_SCHEMA:
    errors.append("observation schema mismatch")
  for key in ("group", "environment", "target"):
    if observation.get(key) != plan[key]:
      errors.append(f"observation {key} mismatch")
  if observation.get("deployment_digest") != expected:
    errors.append("deployment digest mismatch")
  expected_units = {f"{plan['group']}-{item.split('@', 1)[0]}.service" for item in plan["packages"]}
  units = observation.get("units")
  if not isinstance(units, dict):
    errors.append("observation units must be an object")
  else:
    for unit in sorted(expected_units):
      if units.get(unit) != "active":
        errors.append(f"unit not active: {unit}")
  routes = observation.get("routes")
  if not isinstance(routes, dict):
    errors.append("observation routes must be an object")
  else:
    for route in plan["routes"]:
      if routes.get(route["hostname"]) != expected:
        errors.append(f"route digest mismatch: {route['hostname']}")
  return {
    "schema": "polyglot.deployment-status/v1",
    "group": plan["group"],
    "environment": plan["environment"],
    "deployment_digest": expected,
    "state": "ready" if not errors else "drifted",
    "errors": errors,
  }, errors


def observation_for(plan: dict[str, Any]) -> dict[str, Any]:
  digest = deployment_digest(plan)
  return {
    "schema": OBSERVATION_SCHEMA,
    "group": plan["group"],
    "environment": plan["environment"],
    "target": plan["target"],
    "deployment_digest": digest,
    "units": {f"{plan['group']}-{item.split('@', 1)[0]}.service": "active" for item in plan["packages"]},
    "routes": {route["hostname"]: digest for route in plan["routes"]},
  }


def run_contract(plan: dict[str, Any]) -> int:
  validate_plan(plan)
  healthy = observation_for(plan)
  report, errors = status(plan, healthy)
  if errors or report["state"] != "ready":
    raise AssertionError("healthy observation was not ready")
  healthy["routes"][plan["routes"][0]["hostname"]] = "sha256:wrong"
  report, errors = status(plan, healthy)
  if not errors or report["state"] != "drifted":
    raise AssertionError("route drift was not detected")
  print("deployment readonly contract: ok")
  return 0


def main(argv: list[str] | None = None) -> int:
  parser = argparse.ArgumentParser(description = __doc__)
  commands = parser.add_subparsers(dest = "command", required = True)
  for name in ("plan", "status", "verify", "contract"):
    command = commands.add_parser(name)
    command.add_argument("--plan", type = Path, required = True)
    if name in {"status", "verify"}:
      command.add_argument("--observation", type = Path, required = True)
  args = parser.parse_args(argv)
  try:
    plan = load_object(args.plan, "plan")
    validate_plan(plan)
    if args.command == "plan":
      print(json.dumps({**plan, "deployment_digest": deployment_digest(plan)}, indent = 2, sort_keys = True))
      return 0
    if args.command == "contract":
      return run_contract(plan)
    observation = load_object(args.observation, "observation")
    report, errors = status(plan, observation)
    print(json.dumps(report, indent = 2, sort_keys = True))
    if args.command == "verify" and errors:
      return 1
    return 0
  except (AssertionError, ValueError) as error:
    print(f"deployment readonly error: {error}", file = __import__("sys").stderr)
    return 2


if __name__ == "__main__":
  raise SystemExit(main())
