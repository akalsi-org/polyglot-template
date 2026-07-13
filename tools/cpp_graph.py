#!/usr/bin/env python3
"""Generate deterministic Ninja and compilation database files from cpp.toml."""

from __future__ import annotations

import argparse
import json
import shlex
import tomllib
from dataclasses import dataclass
from pathlib import Path, PurePosixPath
from typing import Any


SCHEMA_VERSION = 1


def fail(message: str) -> "NoReturn":
  raise SystemExit(message)


def relpath(value: str, *, field: str) -> str:
  path = PurePosixPath(value)
  if path.is_absolute() or ".." in path.parts or value in ("", "."):
    fail(f"{field} must be a non-empty repository-relative path: {value!r}")
  return path.as_posix()


def string_list(value: Any, *, field: str) -> list[str]:
  if not isinstance(value, list) or not all(isinstance(item, str) for item in value):
    fail(f"{field} must be an array of strings")
  return list(value)


fixed_args: list[str] = [
  "-D__BEGIN_DECLS=",
  "-D__END_DECLS=",
]

@dataclass(frozen=True)
class Action:
  owner: str
  source: str
  output: str
  arguments: tuple[str, ...]

  def compdb(self, root: Path) -> dict[str, Any]:
    return {
      "directory": str(root),
      "file": str(root / self.source),
      "output": str(root / self.output),
      "arguments": list(self.arguments) + fixed_args,
    }


def load_manifest(path: Path) -> dict[str, Any]:
  with path.open("rb") as stream:
    data = tomllib.load(stream)
  if not isinstance(data.get("toolchain"), dict):
    fail("cpp.toml requires [toolchain]")
  return data


def profile_flags(data: dict[str, Any], profile: str) -> tuple[list[str], list[str]]:
  profiles = data.get("profiles", {})
  if profile not in profiles:
    fail(f"unknown C++ profile: {profile}")
  selected = profiles[profile]
  compile_flags = string_list(selected.get("compile_flags", []), field=f"profiles.{profile}.compile_flags")
  link_flags = string_list(selected.get("link_flags", []), field=f"profiles.{profile}.link_flags")
  forbidden = {"-fhardened", "-ftrivial-auto-var-init=zero"}
  present = forbidden.intersection(compile_flags + link_flags)
  if present:
    fail(f"forbidden C++ flags in {profile}: {', '.join(sorted(present))}")
  sanitizers = [flag for flag in compile_flags + link_flags if flag.startswith("-fsanitize")]
  if sanitizers:
    fail(f"sanitizers are outside the pinned musl toolchain contract: {', '.join(sanitizers)}")
  return compile_flags, link_flags


def load_adapter(root: Path, declaration: dict[str, Any], cxx: str, common: list[str], build: str) -> list[Action]:
  name = declaration.get("name")
  if not isinstance(name, str) or not name:
    fail("adapter name must be a non-empty string")
  fragment_path = relpath(declaration.get("fragment", ""), field=f"adapter {name} fragment")
  fragment = json.loads((root / "cpp" / fragment_path).read_text())
  if set(fragment) != {"schema_version", "adapter", "actions"}:
    fail(f"adapter {name}: fragment has unknown or missing fields")
  if fragment["schema_version"] != SCHEMA_VERSION or fragment["adapter"] != name:
    fail(f"adapter {name}: incompatible schema or identity")
  actions: list[Action] = []
  for index, raw in enumerate(fragment["actions"]):
    if set(raw) != {"source", "output", "arguments"}:
      fail(f"adapter {name} action {index}: unknown or missing fields")
    source = f"cpp/{relpath(raw['source'], field=f'adapter {name} source')}"
    leaf_output = relpath(raw["output"], field=f"adapter {name} output")
    extras = [
      f"-Icpp/{item[2:]}" if item.startswith("-I") else item
      for item in string_list(raw["arguments"], field=f"adapter {name} arguments")
    ]
    output = f"{build}/obj/adapters/{name}/{leaf_output}"
    arguments = (cxx, *common, *extras, "-c", source, "-o", output)
    actions.append(Action(f"adapter:{name}", source, output, tuple(arguments)))
  return actions


def build_graph(root: Path, data: dict[str, Any], profile: str, build: str | None = None) -> tuple[list[Action], dict[str, list[str]], list[str]]:
  toolchain = data["toolchain"]
  cxx = toolchain.get("cxx")
  standard = toolchain.get("standard")
  reflection = toolchain.get("reflection_flag")
  if not all(isinstance(item, str) and item for item in (cxx, standard, reflection)):
    fail("toolchain cxx, standard, and reflection_flag must be strings")
  if standard != "gnu++26" or reflection != "-freflection":
    fail("the baseline compiler contract is -std=gnu++26 -freflection")
  compile_flags, link_flags = profile_flags(data, profile)
  tool_bin = str(Path(cxx).parent) if Path(cxx).is_absolute() else None
  tool_search = [f"-B{tool_bin}"] if tool_bin else []
  common = [f"-std={standard}", *tool_search, *compile_flags]
  build = build or f"build/cpp/{profile}"
  actions: list[Action] = []
  links: dict[str, list[str]] = {}
  for target in data.get("targets", []):
    name = target.get("name")
    if not isinstance(name, str) or not name or target.get("type") != "executable":
      fail("only named executable targets are supported in the bootstrap slice")
    includes = [f"-Icpp/{relpath(item, field=f'target {name} include')}" for item in string_list(target.get("include_dirs", []), field=f"target {name} include_dirs")]
    outputs: list[str] = []
    for source in string_list(target.get("sources", []), field=f"target {name} sources"):
      source = f"cpp/{relpath(source, field=f'target {name} source')}"
      output = f"{build}/obj/targets/{name}/{source}.o"
      arguments = (cxx, *common, *includes, "-c", source, "-o", output)
      actions.append(Action(f"target:{name}", source, output, tuple(arguments)))
      outputs.append(output)
    adapter_dependencies = string_list(target.get("adapter_dependencies", []), field=f"target {name} adapter_dependencies")
    links[name] = outputs + [f"@adapter:{item}" for item in adapter_dependencies]
  for adapter in data.get("adapters", []):
    if adapter.get("include_in_compdb", True):
      actions.extend(load_adapter(root, adapter, cxx, common, build))
  actions.sort(key=lambda action: (action.source, action.output, action.owner))
  if len({action.source for action in actions}) != len(actions):
    fail("each translation unit must have exactly one canonical compile action")
  return actions, links, [cxx, *tool_search, *link_flags]


def shell_join(arguments: tuple[str, ...] | list[str]) -> str:
  return " ".join(shlex.quote(item) for item in arguments)


def ninja_text(actions: list[Action], links: dict[str, list[str]], linker: list[str], build: str) -> str:
  lines = ["# Generated by tools/cpp_graph.py; do not edit.", "ninja_required_version = 1.10", ""]
  lines += ["rule compile", "  command = $command", "  description = CXX $in", ""]
  lines += ["rule link", "  command = $command", "  description = LINK $out", ""]
  for action in actions:
    command = ["mkdir", "-p", str(PurePosixPath(action.output).parent)]
    lines += [f"build {action.output}: compile {action.source}", f"  command = {shell_join(command)} && {shell_join(action.arguments)}", ""]
  for name in sorted(links):
    values = links[name]
    objects = [item for item in values if item.endswith(".o")]
    dependencies = [item.removeprefix("@adapter:") for item in values if item.startswith("@adapter:")]
    flags = [item for item in values if not item.endswith(".o") and not item.startswith("@adapter:")]
    adapter_outputs = [action.output for action in actions if action.owner.removeprefix("adapter:") in dependencies]
    if set(dependencies) != {action.owner.removeprefix("adapter:") for action in actions if action.owner.removeprefix("adapter:") in dependencies}:
      fail(f"target {name}: unknown adapter dependency")
    inputs = objects + adapter_outputs
    output = f"{build}/bin/{name}"
    command = [linker[0], *inputs, *linker[1:], *flags, "-o", output]
    mkdir = ["mkdir", "-p", str(PurePosixPath(output).parent)]
    lines += [f"build {output}: link {' '.join(inputs)}", f"  command = {shell_join(mkdir)} && {shell_join(command)}", ""]
  return "\n".join(lines)


def write_if_changed(path: Path, content: str) -> None:
  path.parent.mkdir(parents=True, exist_ok=True)
  if not path.exists() or path.read_text() != content:
    path.write_text(content)


def configure(root: Path, manifest: Path, profile: str, output: Path, cxx: str | None = None) -> None:
  data = load_manifest(manifest)
  if cxx is not None:
    data["toolchain"]["cxx"] = cxx
  try:
    build = output.resolve().relative_to(root).as_posix()
  except ValueError:
    # Unit tests may write generated presentations to an external scratch
    # directory while retaining repository-relative build action paths.
    build = f"build/cpp/{profile}"
  actions, links, linker = build_graph(root, data, profile, build)
  write_if_changed(output / "build.ninja", ninja_text(actions, links, linker, build))
  compdb = [action.compdb(root) for action in actions]
  write_if_changed(output / "compile_commands.json", json.dumps(compdb, indent=2, sort_keys=True) + "\n")
  projections: dict[str, list[dict[str, Any]]] = {}
  for action in actions:
    projections.setdefault(action.owner, []).append(action.compdb(root))
  write_if_changed(output / "cpp-actions.json", json.dumps({"schema_version": 1, "actions": projections}, indent=2, sort_keys=True) + "\n")


def probe_command(manifest: Path, cxx: str | None = None) -> None:
  data = load_manifest(manifest)
  toolchain = data["toolchain"]
  command = [cxx or toolchain["cxx"], f"-std={toolchain['standard']}", toolchain["reflection_flag"], "-fsyntax-only", "cpp/test/reflection.cc"]
  print(shell_join(command))


def main() -> None:
  parser = argparse.ArgumentParser()
  parser.add_argument("command", choices=("configure", "reflection-probe"))
  parser.add_argument("--root", type=Path, default=Path.cwd())
  parser.add_argument("--manifest", type=Path)
  parser.add_argument("--profile", choices=("dbg", "opt"), default="dbg")
  parser.add_argument("--output", type=Path)
  parser.add_argument("--cxx")
  args = parser.parse_args()
  root = args.root.resolve()
  manifest = args.manifest or root / "cpp" / "cpp.toml"
  if args.command == "reflection-probe":
    probe_command(manifest, args.cxx)
    return
  output = args.output or root / "build" / "cpp" / args.profile
  configure(root, manifest, args.profile, output, args.cxx)


if __name__ == "__main__":
  main()
