#!/usr/bin/env python3
"""Validate package metadata and resolve a sealed runtime closure."""

from __future__ import annotations

import argparse
import hashlib
import json
import re
import sys
import tomllib
from pathlib import Path, PurePosixPath

NAME = re.compile(r"^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$")
VERSION = re.compile(r"^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-[0-9A-Za-z.-]+)?$")
TARGET = re.compile(r"^(x86_64|aarch64)-linux-(musl|gnu)$")
DIGEST = re.compile(r"^[0-9a-f]{64}$")


class ModelError(ValueError):
    pass


def load(path: Path) -> dict:
    with path.open("rb") as stream:
        return tomllib.load(stream)


def require(condition: bool, message: str) -> None:
    if not condition:
        raise ModelError(message)


def packages(model: dict) -> dict[str, dict]:
    require(model.get("schema_version") == 1, "package schema_version must be 1")
    result: dict[str, dict] = {}
    for item in model.get("package", []):
        name = item.get("name", "")
        require(bool(NAME.fullmatch(name)), f"invalid package name: {name!r}")
        require(name not in result, f"duplicate package: {name}")
        require(bool(VERSION.fullmatch(item.get("version", ""))), f"{name}: version must be exact SemVer")
        require(item.get("kind") in {"application", "runtime"}, f"{name}: invalid kind")
        targets = item.get("supported_targets", [])
        require(bool(targets) and len(targets) == len(set(targets)), f"{name}: supported_targets must be nonempty and unique")
        require(all(TARGET.fullmatch(x) for x in targets), f"{name}: invalid supported target")
        runtime = item.get("runtime")
        if runtime is not None:
            require(item["kind"] == "application" and isinstance(runtime, dict), f"{name}: runtime must be an application requirement object")
            require(bool(NAME.fullmatch(runtime.get("name", ""))), f"{name}: invalid runtime name")
            constraint = runtime.get("version", "")
            require(constraint not in {"", "*", "latest"} and constraint.startswith("^"), f"{name}: runtime version must be a bounded caret constraint")
            require(bool(VERSION.fullmatch(constraint[1:] + (".0" if constraint.count(".") == 1 else ""))), f"{name}: invalid runtime version constraint")
            require(bool(NAME.fullmatch(runtime.get("variant", "").replace("_", "-"))), f"{name}: invalid runtime variant")
        if item["kind"] == "runtime":
            require(bool(NAME.fullmatch(item.get("runtime_name", ""))), f"{name}: runtime_name is required")
            require(bool(item.get("variant")), f"{name}: variant is required")
            require(bool(NAME.fullmatch(item.get("loader_tool", ""))), f"{name}: loader_tool is required")
            artifacts = item.get("artifact", [])
            require({a.get("target") for a in artifacts} == set(targets), f"{name}: exactly one artifact per supported target is required")
            require(all(DIGEST.fullmatch(a.get("sha256", "")) for a in artifacts), f"{name}: invalid artifact digest")
        result[name] = item
    require(bool(result), "at least one package is required")
    return result


def compatible(constraint: str, version: str) -> bool:
    base = [int(x) for x in constraint[1:].split(".")]
    while len(base) < 3:
        base.append(0)
    actual = tuple(int(x) for x in version.split("-")[0].split("."))
    return actual >= tuple(base) and actual[0] == base[0]


def resolve(model: dict, lock: dict, tools_lock: dict, package_name: str, target: str, *, allow_unresolved_loader: bool = False) -> tuple[dict, dict]:
    catalog = packages(model)
    require(package_name in catalog, f"unknown package: {package_name}")
    app = catalog[package_name]
    require(app["kind"] == "application", f"{package_name}: only applications have closures")
    require(target in app["supported_targets"], f"{package_name}: target is not supported: {target}")
    runtime_req = app.get("runtime")
    runtime_ref = None
    if runtime_req:
        matches = [x for x in lock.get("resolution", []) if x.get("package") == package_name and x.get("target") == target]
        require(len(matches) == 1, f"{package_name}: expected exactly one locked runtime for {target}")
        resolved = matches[0]
        runtime = catalog.get(resolved.get("runtime_package"))
        require(runtime is not None and runtime["kind"] == "runtime", f"{package_name}: locked runtime package is missing")
        require(runtime["runtime_name"] == runtime_req["name"], f"{package_name}: locked runtime name mismatch")
        require(resolved.get("runtime_version") == runtime["version"] and compatible(runtime_req["version"], runtime["version"]), f"{package_name}: locked runtime version is incompatible")
        require(resolved.get("variant") == runtime_req["variant"] == runtime["variant"], f"{package_name}: locked runtime variant mismatch")
        require(target in runtime["supported_targets"], f"{package_name}: runtime does not support {target}")
        artifact = next((a for a in runtime["artifact"] if a["target"] == target), None)
        require(artifact is not None, f"{package_name}: runtime artifact is missing for {target}")
        for key in ("artifact", "sha256"):
            expected = artifact["filename" if key == "artifact" else key]
            require(resolved.get(key) == expected, f"{package_name}: locked runtime {key} mismatch")
        require(resolved.get("release_tag") == f"packages/{runtime['name']}/v{runtime['version']}", f"{package_name}: non-canonical runtime release tag")
        runtime_ref = {key: resolved[key] for key in ("runtime_package", "runtime_version", "variant", "target", "release_tag", "artifact", "sha256")}
        runtime_ref["loader_tool"] = runtime["loader_tool"]
        loader_fields = {key: resolved.get(key, "") for key in ("loader_version", "loader_sha256", "loader_path")}
        loader_matches = [item for item in tools_lock.get("artifact", []) if item.get("tool") == runtime["loader_tool"] and item.get("target") == target]
        require(len(loader_matches) == 1, f"{package_name}: expected one canonical loader tool for {target}")
        loader_artifact = loader_matches[0]
        require(loader_fields["loader_version"] == loader_artifact.get("version"), f"{package_name}: loader version does not match tools lock")
        require(loader_fields["loader_sha256"] == loader_artifact.get("sha256"), f"{package_name}: loader digest does not match tools lock")
        require(loader_fields["loader_path"] == loader_artifact.get("loader"), f"{package_name}: loader path does not match tools lock")
        path = PurePosixPath(loader_fields["loader_path"])
        loader_resolved = bool(DIGEST.fullmatch(loader_fields["loader_sha256"])) and not path.is_absolute() and ".." not in path.parts and loader_fields["loader_path"] not in {"", "UNRESOLVED"}
        require(loader_resolved or allow_unresolved_loader, f"{package_name}: musl loader closure is unresolved")
        runtime_ref.update(loader_fields)
    closure = {"schema_version": 1, "package": package_name, "version": app["version"], "target": target, "runtime": runtime_ref}
    encoded = json.dumps(closure, sort_keys=True, separators=(",", ":")).encode()
    closure["closure_sha256"] = hashlib.sha256(encoded).hexdigest()
    return closure, runtime_ref or {}


def write_json(path: Path, value: dict) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(value, sort_keys=True, indent=2) + "\n", encoding="utf-8")


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--manifest", type=Path, default=Path("package.toml"))
    parser.add_argument("--lock", type=Path, default=Path("runtime-resolution.lock.toml"))
    parser.add_argument("--tools-lock", type=Path, default=Path("tools.lock.toml"))
    sub = parser.add_subparsers(dest="command", required=True)
    sub.add_parser("validate")
    resolver = sub.add_parser("resolve")
    resolver.add_argument("--package", required=True)
    resolver.add_argument("--target", required=True)
    resolver.add_argument("--out-dir", type=Path)
    args = parser.parse_args()
    try:
        model = load(args.manifest)
        tools_lock = load(args.tools_lock)
        catalog = packages(model)
        if args.command == "validate":
            lock = load(args.lock)
            require(lock.get("schema_version") == 1, "resolution lock schema_version must be 1")
            for name, item in catalog.items():
                if item.get("runtime"):
                    for target in item["supported_targets"]:
                        resolve(model, lock, tools_lock, name, target, allow_unresolved_loader=True)
            print("package model valid")
        else:
            closure, runtime_ref = resolve(model, load(args.lock), tools_lock, args.package, args.target)
            if args.out_dir:
                write_json(args.out_dir / "closure.json", closure)
                if runtime_ref:
                    write_json(args.out_dir / "runtime-ref.json", runtime_ref)
            print(json.dumps(closure, sort_keys=True, separators=(",", ":")))
    except (OSError, tomllib.TOMLDecodeError, ModelError) as error:
        print(f"package model error: {error}", file=sys.stderr)
        return 2
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
