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


def copy_tree(source: Path, destination: Path, *, ignore: tuple[str, ...] = ()) -> None:
    require(source.is_dir(), f"missing built directory: {source}")
    shutil.copytree(source, destination, dirs_exist_ok=True, symlinks=True, ignore=shutil.ignore_patterns(*ignore))


def write_launcher(path: Path, body: str) -> None:
    write_text(path, "#!/bin/sh\nset -eu\n" + body)
    os.chmod(path, 0o755)


def assemble_polyglot_demo(args: argparse.Namespace, entry: dict[str, Any], stage: Path, target: str) -> None:
    root = args.root
    local = root / ".local" / "toolchain" / target
    tools = load(args.tools_lock).get("artifact", [])
    gcc = next(item for item in tools if item.get("tool") == "gcc-musl" and item.get("target") == target)
    python = next(item for item in tools if item.get("tool") == "python" and item.get("target") == target)
    gcc_install = local / f"gcc-musl-{gcc['version']}"
    python_install = local / f"python-{python['version']}" / "python"

    copy_executable(root / f"build/cpp/{target}/{args.profile}/bin/hello", stage / "libexec/cpp-hello")
    copy_executable(root / f"build/go/{target}/hello", stage / "bin/go-hello")
    copy_tree(python_install, stage / "runtime/python", ignore=("__pycache__", "*.pyc"))
    copy_tree(root / "python/lib", stage / "app/python/lib", ignore=("*.cc", "__pycache__", "*.pyc"))
    copy_tree(root / "python/app", stage / "app/python/app", ignore=("__pycache__", "*.pyc"))
    copy_tree(root / f"build/python/{target}/lib", stage / "app/python/lib", ignore=("__pycache__", "*.pyc"))
    copy_tree(root / "build/tsweb/site", stage / "app/web")

    loader_source = gcc_install / gcc["loader"]
    libc_source = loader_source.parent / "libc.so"
    loader_name = f"ld-musl-{'x86_64' if target.startswith('x86_64') else 'aarch64'}.so.1"
    copy_executable(loader_source, stage / f"lib/{loader_name}")
    copy_executable(libc_source, stage / "lib/libc.so")
    write_launcher(
        stage / "bin/cpp-hello",
        f'ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd -P)\nexec "$ROOT/lib/{loader_name}" --library-path "$ROOT/lib" "$ROOT/libexec/cpp-hello" "$@"\n',
    )
    write_launcher(
        stage / "bin/python-hello",
        f'ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd -P)\nexport PYTHONPATH="$ROOT/app/python/lib:$ROOT/app/python/app"\nexec "$ROOT/lib/{loader_name}" --library-path "$ROOT/lib:$ROOT/runtime/python/lib" "$ROOT/runtime/python/bin/python3" "$ROOT/app/python/app/hello/main.py" "$@"\n',
    )


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
    build_dir = args.build_dir or args.root / "build" / "cpp" / target / args.profile
    package_id = f"{args.package}-{entry['version']}-{target}"
    dist_dir = args.dist_dir
    stage_parent = dist_dir / "stage"
    stage = stage_parent / package_id
    if stage.exists():
        shutil.rmtree(stage)
    stage.mkdir(parents=True)
    if entry.get("layout") == "polyglot-demo":
        assemble_polyglot_demo(args, entry, stage, target)
    else:
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
        for executable in entry["executables"]:
            path = scratch / "bin" / executable
            require(path.is_file(), f"packaged executable is missing: {executable}")
            require(os.access(path, os.X_OK), f"packaged executable is not executable: {executable}")
            if args.execute:
                subprocess.run([str(path)], cwd=scratch, check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        if entry.get("layout") == "polyglot-demo":
            require((scratch / "app/web/index.html").is_file(), "packaged React index is missing")
            subprocess.run(
                [sys.executable, str(args.root / "tools/tsweb_smoke.py"), "--root", str(scratch / "app/web")],
                check=True,
                stdout=subprocess.DEVNULL,
            )
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
