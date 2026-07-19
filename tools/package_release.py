#!/usr/bin/env python3
"""Assemble deterministic packages and verify release metadata."""

from __future__ import annotations

import argparse
import gzip
import json
import os
import shutil
import subprocess
import sys
import tarfile
import tempfile
import tomllib
from pathlib import Path, PurePosixPath
from typing import Any

import package_model

# Hard ceiling on any single smoke execution (defense-in-depth): a smoke
# that can hang forever is a broken smoke regardless of whether the
# executable was given the right args (e.g. a manifest typo, or a future
# long-running executable nobody remembered to add smoke_args for). 60s is
# generous for every executable in this repo's packages today (all exit in
# well under a second).
SMOKE_EXECUTE_TIMEOUT_SECONDS = 60


class ReleaseError(ValueError):
  pass


def require(condition: bool, message: str) -> None:
  if not condition:
    raise ReleaseError(message)


def load(path: Path) -> dict[str, Any]:
  with path.open("rb") as stream:
    return tomllib.load(stream)


def catalog(manifest: Path) -> dict[str, dict[str, Any]]:
  return package_model.packages(load(manifest))


def package_entry(manifest: Path, name: str) -> dict[str, Any]:
  packages = catalog(manifest)
  require(name in packages, f"unknown package: {name}")
  entry = packages[name]
  require(entry["kind"] == "application", f"{name}: only applications can be assembled")
  executables = entry.get("executables", [])
  require(isinstance(executables, list) and executables and all(isinstance(item, str) for item in executables), f"{name}: executables must be a non-empty string array")
  for executable in executables:
    path = PurePosixPath(executable)
    require(not path.is_absolute() and ".." not in path.parts and executable not in {"", "."}, f"{name}: invalid executable path: {executable!r}")
  return entry


def stable_json(value: dict[str, Any]) -> str:
  return json.dumps(value, sort_keys=True, indent=2) + "\n"


def write_text(path: Path, value: str) -> None:
  path.parent.mkdir(parents=True, exist_ok=True)
  path.write_text(value, encoding="utf-8")


def copy_executable(source: Path, destination: Path) -> None:
  require(source.is_file(), f"missing built executable: {source}")
  require(os.access(source, os.X_OK), f"built executable is not executable: {source}")
  destination.parent.mkdir(parents=True, exist_ok=True)
  shutil.copy2(source, destination)
  os.chmod(destination, 0o755)


def reset_tarinfo(info: tarfile.TarInfo) -> tarfile.TarInfo:
  info.uid = 0
  info.gid = 0
  info.uname = "root"
  info.gname = "root"
  info.mtime = 0
  if info.isdir():
    info.mode = 0o755
  elif info.mode & 0o111:
    info.mode = 0o755
  else:
    info.mode = 0o644
  return info


def make_archive(stage: Path, archive: Path) -> None:
  temporary = archive.with_suffix(archive.suffix + ".tmp")
  if temporary.exists():
    temporary.unlink()
  with temporary.open("wb") as raw:
    with gzip.GzipFile(filename="", mode="wb", fileobj=raw, mtime=0) as zipped:
      with tarfile.open(fileobj=zipped, mode="w") as tar:
        for path in sorted(stage.rglob("*")):
          tar.add(path, arcname=path.relative_to(stage).as_posix(), recursive=False, filter=reset_tarinfo)
  temporary.replace(archive)


def resolve_closure(args: argparse.Namespace, package_name: str, target: str) -> tuple[dict[str, Any], dict[str, Any]]:
  return package_model.resolve(load(args.manifest), load(args.lock), load(args.tools_lock), package_name, target)


def assemble(args: argparse.Namespace) -> int:
  entry = package_entry(args.manifest, args.package)
  target = args.target
  require(target in entry["supported_targets"], f"{args.package}: target is not supported: {target}")
  closure, runtime_ref = resolve_closure(args, args.package, target)
  # assemble() is for manifest-declared packages with no buck2 package()
  # target (e.g. gateway, schema-cli) - a package with a buck2 target (e.g.
  # polyglot-demo, polyglot-server) is built and archived entirely in-graph
  # by //packages:<name> (see rules/package.bzl); repo.sh's `package`
  # command routes to that path instead of here for those names. There is
  # no default build directory: this tool never invokes a build itself, so
  # --build-dir is required and a missing/empty directory fails with an
  # honest message instead of silently resolving to a path nothing
  # populates.
  build_dir = args.build_dir
  require(build_dir is not None, f"{args.package}: --build-dir was not given; this package has no build target in this repository yet")
  require(build_dir.is_dir() and any(build_dir.iterdir()), f"{args.package}: no built executables at {build_dir}; this package has no build target in this repository yet")
  package_id = f"{args.package}-{entry['version']}-{target}"
  dist_dir = args.dist_dir
  stage_parent = dist_dir / "stage"
  stage = stage_parent / package_id
  if stage.exists():
    shutil.rmtree(stage)
  stage.mkdir(parents=True)
  for executable in entry["executables"]:
    copy_executable(build_dir / "bin" / executable, stage / "bin" / executable)
  metadata = {
    "schema_version": 1,
    "package": args.package,
    "version": entry["version"],
    "target": target,
    "profile": args.profile,
    "executables": list(entry["executables"]),
    "closure_sha256": closure["closure_sha256"],
  }
  write_text(stage / "package.json", stable_json(metadata))
  write_text(stage / "closure.json", stable_json(closure))
  if runtime_ref:
    write_text(stage / "runtime-ref.json", stable_json(runtime_ref))
  archive = dist_dir / f"{package_id}.tar.gz"
  dist_dir.mkdir(parents=True, exist_ok=True)
  make_archive(stage, archive)
  print(archive)
  return 0


def extract_archive(archive: Path, destination: Path) -> Path:
  require(archive.is_file(), f"archive does not exist: {archive}")
  with tarfile.open(archive, mode="r:gz") as tar:
    members = tar.getmembers()
    require(bool(members), "archive is empty")
    for member in members:
      path = PurePosixPath(member.name)
      require(not path.is_absolute() and ".." not in path.parts, f"archive contains unsafe path: {member.name}")
    tar.extractall(destination, filter="data")
  return destination


def read_json(path: Path) -> dict[str, Any]:
  return json.loads(path.read_text(encoding="utf-8"))


def smoke(args: argparse.Namespace) -> int:
  entry = package_entry(args.manifest, args.package)
  with tempfile.TemporaryDirectory(prefix="package-smoke-") as scratch_name:
    scratch = Path(scratch_name)
    extract_archive(args.archive, scratch)
    metadata = read_json(scratch / "package.json")
    require(metadata.get("package") == args.package, "package metadata identity mismatch")
    require(metadata.get("version") == entry["version"], "package metadata version mismatch")
    target = args.target or metadata.get("target")
    require(isinstance(target, str) and target in entry["supported_targets"], "package metadata target mismatch")
    require(metadata.get("executables") == entry["executables"], "package metadata executable list mismatch")
    closure, runtime_ref = resolve_closure(args, args.package, target)
    packaged_closure = read_json(scratch / "closure.json")
    require(packaged_closure == closure, "packaged closure does not match exact runtime resolution")
    require(metadata.get("closure_sha256") == closure["closure_sha256"], "package metadata closure digest mismatch")
    if runtime_ref:
      require(read_json(scratch / "runtime-ref.json") == runtime_ref, "runtime reference mismatch")
    smoke_args = entry.get("smoke_args") or {}
    for executable in entry["executables"]:
      path = scratch / "bin" / executable
      require(path.is_file(), f"packaged executable is missing: {executable}")
      require(os.access(path, os.X_OK), f"packaged executable is not executable: {executable}")
      if args.execute:
        command = [str(path)] + list(smoke_args.get(executable, []))
        try:
          subprocess.run(
            command,
            cwd=scratch,
            check=True,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
            timeout=SMOKE_EXECUTE_TIMEOUT_SECONDS,
          )
        except subprocess.TimeoutExpired:
          raise ReleaseError(
            f"{args.package}: smoke execution of {executable!r} did not exit within "
            f"{SMOKE_EXECUTE_TIMEOUT_SECONDS}s (command: {command!r}) - if this executable "
            f"legitimately needs different arguments to exit on its own, declare them via "
            f"package.toml's smoke_args"
          ) from None
  print("package smoke: ok")
  return 0


def expected_tag(entry: dict[str, Any]) -> str:
  return f"packages/{entry['name']}/v{entry['version']}"


def changelog_section(changelog: Path, tag: str) -> str:
  require(changelog.is_file(), f"missing changelog: {changelog}")
  lines = changelog.read_text(encoding="utf-8").splitlines()
  heading = f"## {tag}"
  start = next((index for index, line in enumerate(lines) if line.strip() == heading), None)
  require(start is not None, f"changelog is missing section: {heading}")
  end = next((index for index in range(start + 1, len(lines)) if lines[index].startswith("## ")), len(lines))
  body = "\n".join(lines[start + 1 : end]).strip()
  require(bool(body), f"changelog section is empty: {heading}")
  return body


def release_check(args: argparse.Namespace) -> int:
  entry = package_entry(args.manifest, args.package)
  tag = args.tag
  require(tag == expected_tag(entry), f"tag must be {expected_tag(entry)}")
  changelog_section(args.changelog, tag)
  target = args.target
  archive = args.dist_dir / f"{args.package}-{entry['version']}-{target}.tar.gz"
  smoke_args = argparse.Namespace(**vars(args), archive=archive, execute=True)
  smoke(smoke_args)
  print("release check: ok")
  return 0


def release_notes(args: argparse.Namespace) -> int:
  print(changelog_section(args.changelog, args.tag))
  return 0


def main() -> int:
  parser = argparse.ArgumentParser()
  parser.add_argument("--root", type=Path, default=Path.cwd())
  parser.add_argument("--manifest", type=Path, default=Path("package.toml"))
  parser.add_argument("--lock", type=Path, default=Path("runtime-resolution.lock.toml"))
  parser.add_argument("--tools-lock", type=Path, default=Path("tools.lock.toml"))
  parser.add_argument("--dist-dir", type=Path, default=Path("dist"))
  parser.add_argument("--changelog", type=Path, default=Path("CHANGELOG.md"))
  sub = parser.add_subparsers(dest="command", required=True)
  package_cmd = sub.add_parser("package")
  package_cmd.add_argument("--package", required=True)
  package_cmd.add_argument("--target", required=True)
  package_cmd.add_argument("--profile", choices=("dbg", "opt"), default="opt")
  package_cmd.add_argument("--build-dir", type=Path)
  smoke_cmd = sub.add_parser("smoke")
  smoke_cmd.add_argument("--archive", type=Path, required=True)
  smoke_cmd.add_argument("--package", required=True)
  smoke_cmd.add_argument("--target")
  smoke_cmd.add_argument("--execute", action="store_true")
  release_cmd = sub.add_parser("release-check")
  release_cmd.add_argument("--package", required=True)
  release_cmd.add_argument("--target", required=True)
  release_cmd.add_argument("--tag", required=True)
  notes_cmd = sub.add_parser("release-notes")
  notes_cmd.add_argument("--tag", required=True)
  args = parser.parse_args()
  try:
    args.root = args.root.resolve()
    for attr in ("manifest", "lock", "tools_lock", "dist_dir", "changelog"):
      setattr(args, attr, getattr(args, attr).resolve())
    if args.command == "package":
      if args.build_dir is not None:
        args.build_dir = args.build_dir.resolve()
      return assemble(args)
    if args.command == "smoke":
      args.archive = args.archive.resolve()
      return smoke(args)
    if args.command == "release-check":
      return release_check(args)
    if args.command == "release-notes":
      return release_notes(args)
  except (OSError, json.JSONDecodeError, tarfile.TarError, subprocess.CalledProcessError, tomllib.TOMLDecodeError, package_model.ModelError, ReleaseError) as error:
    print(f"package release error: {error}", file=sys.stderr)
    return 2
  raise AssertionError(args.command)


if __name__ == "__main__":
  raise SystemExit(main())
