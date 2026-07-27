#!/usr/bin/env python3
"""Create and verify deterministic package-release evidence."""

from __future__ import annotations

import argparse
import hashlib
import json
import sys
import tarfile
from pathlib import Path, PurePosixPath
from typing import Any


def fail(message: str) -> None:
  raise ValueError(message)


def digest(path: Path) -> str:
  hasher = hashlib.sha256()
  with path.open("rb") as stream:
    for chunk in iter(lambda: stream.read(1024 * 1024), b""):
      hasher.update(chunk)
  return hasher.hexdigest()


def canonical(value: Any) -> str:
  return json.dumps(value, indent=2, sort_keys=True) + "\n"


def write_json(path: Path, value: Any) -> None:
  temporary = path.with_name(path.name + ".tmp")
  temporary.write_text(canonical(value), encoding="utf-8")
  temporary.replace(path)


def archive_json(archive: Path, name: str) -> dict[str, Any]:
  try:
    with tarfile.open(archive, mode="r:gz") as tar:
      member = tar.getmember(name)
      path = PurePosixPath(member.name)
      if path.is_absolute() or ".." in path.parts:
        fail(f"{archive}: unsafe {name} member")
      stream = tar.extractfile(member)
      if stream is None:
        fail(f"{archive}: missing {name}")
      return json.loads(stream.read())
  except (KeyError, tarfile.TarError, json.JSONDecodeError) as error:
    fail(f"{archive}: invalid {name}: {error}")


def package_evidence(args: argparse.Namespace) -> int:
  archive = args.archive.resolve()
  if not archive.is_file():
    fail(f"archive does not exist: {archive}")
  metadata = archive_json(archive, "package.json")
  closure = archive_json(archive, "closure.json")
  for field, expected in (("package", args.package), ("target", args.target), ("profile", args.profile)):
    if metadata.get(field) != expected:
      fail(f"archive package metadata {field!r} does not match {expected!r}")
  version = metadata.get("version")
  if not isinstance(version, str) or not version:
    fail("archive package metadata has no version")
  closure_digest = closure.get("closure_sha256")
  if metadata.get("closure_sha256") != closure_digest or not isinstance(closure_digest, str):
    fail("archive package metadata closure digest does not match closure.json")
  baseline = closure.get("native_isa_baseline")
  if not isinstance(baseline, str) or not baseline or metadata.get("native_isa_baseline") != baseline:
    fail("archive package metadata native ISA baseline does not match closure.json")

  archive_digest = digest(archive)
  catalog_digest = digest(args.catalog)
  lock_digest = digest(args.tools_lock)
  subject = {"name": archive.name, "sha256": archive_digest}
  sbom = {
    "schema": "polyglot.sbom/v1",
    "package": {
      "name": args.package,
      "version": version,
      "target": args.target,
      "native_isa_baseline": baseline,
    },
    "components": [
      {"name": "package-closure", "sha256": closure_digest, "type": "runtime-closure"},
      {"name": "packages/catalog.bzl", "sha256": catalog_digest, "type": "source"},
      {"name": "tools.lock.toml", "sha256": lock_digest, "type": "source"},
    ],
    "subject": subject,
  }
  provenance = {
    "schema": "polyglot.provenance/v1",
    "build_definition": {
      "external_parameters": {
        "package": args.package,
        "profile": args.profile,
        "target": args.target,
        "native_isa_baseline": baseline,
      },
      "resolved_dependencies": [
        {"name": "packages/catalog.bzl", "sha256": catalog_digest},
        {"name": "tools.lock.toml", "sha256": lock_digest},
        {"name": "closure.json", "sha256": closure_digest},
      ],
    },
    "subject": subject,
  }
  sbom_path = archive.with_name(archive.name + ".sbom.json")
  provenance_path = archive.with_name(archive.name + ".provenance.json")
  write_json(sbom_path, sbom)
  write_json(provenance_path, provenance)
  print(sbom_path)
  print(provenance_path)
  return 0


def verify_asset(path: Path) -> tuple[str, dict[str, Any], dict[str, Any]]:
  if not path.is_file() or not path.name.endswith(".tar.gz"):
    fail(f"invalid release archive: {path}")
  sbom_path = path.with_name(path.name + ".sbom.json")
  provenance_path = path.with_name(path.name + ".provenance.json")
  if not sbom_path.is_file() or not provenance_path.is_file():
    fail(f"missing evidence sidecar for {path.name}")
  subject_digest = digest(path)
  sbom = json.loads(sbom_path.read_text(encoding="utf-8"))
  provenance = json.loads(provenance_path.read_text(encoding="utf-8"))
  for label, evidence, schema in (("SBOM", sbom, "polyglot.sbom/v1"), ("provenance", provenance, "polyglot.provenance/v1")):
    if evidence.get("schema") != schema:
      fail(f"{path.name}: invalid {label} schema")
    subject = evidence.get("subject")
    if subject != {"name": path.name, "sha256": subject_digest}:
      fail(f"{path.name}: {label} subject does not bind this archive")
  package = sbom.get("package")
  if not isinstance(package, dict) or not all(
    isinstance(package.get(key), str) and package[key]
    for key in ("name", "version", "target", "native_isa_baseline")
  ):
    fail(f"{path.name}: invalid SBOM package identity")
  parameters = provenance.get("build_definition", {}).get("external_parameters", {})
  if parameters.get("native_isa_baseline") != package["native_isa_baseline"]:
    fail(f"{path.name}: provenance native ISA baseline does not match SBOM")
  return subject_digest, sbom, provenance


def release_manifest(args: argparse.Namespace) -> int:
  archives = sorted({path.resolve() for path in args.archives})
  if not archives:
    fail("at least one archive is required")
  verified = [(path, *verify_asset(path)) for path in archives]
  identities = {(sbom["package"]["name"], sbom["package"]["version"]) for _, _, sbom, _ in verified}
  if len(identities) != 1:
    fail("release archives must contain exactly one package/version")
  package, version = identities.pop()
  if args.tag != f"packages/{package}/v{version}":
    fail(f"tag must be packages/{package}/v{version}")
  targets = [sbom["package"]["target"] for _, _, sbom, _ in verified]
  if len(targets) != len(set(targets)):
    fail("release archives contain duplicate targets")
  manifest = {
    "schema": "polyglot.release-manifest/v1",
    "package": package,
    "tag": args.tag,
    "version": version,
    "artifacts": [
      {
        "archive": path.name,
        "provenance": path.name + ".provenance.json",
        "sbom": path.name + ".sbom.json",
        "sha256": archive_digest,
        "target": sbom["package"]["target"],
      }
      for path, archive_digest, sbom, _ in verified
    ],
  }
  output = args.output.resolve()
  write_json(output, manifest)
  print(output)
  return 0


def main() -> int:
  parser = argparse.ArgumentParser()
  subparsers = parser.add_subparsers(dest="command", required=True)
  package = subparsers.add_parser("package")
  package.add_argument("--archive", type=Path, required=True)
  package.add_argument("--catalog", type=Path, required=True)
  package.add_argument("--tools-lock", type=Path, required=True)
  package.add_argument("--package", required=True)
  package.add_argument("--target", required=True)
  package.add_argument("--profile", choices=("dbg", "opt"), required=True)
  manifest = subparsers.add_parser("release-manifest")
  manifest.add_argument("--tag", required=True)
  manifest.add_argument("--output", type=Path, required=True)
  manifest.add_argument("archives", nargs="+", type=Path)
  args = parser.parse_args()
  try:
    if args.command == "package":
      return package_evidence(args)
    if args.command == "release-manifest":
      return release_manifest(args)
  except (OSError, ValueError, json.JSONDecodeError) as error:
    print(f"release evidence error: {error}", file=sys.stderr)
    return 2
  raise AssertionError(args.command)


if __name__ == "__main__":
  raise SystemExit(main())
