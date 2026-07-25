"""`command -v` guards for the host utilities this project's generated action
scripts shell out to.

These are the only build inputs NOT pinned by tools.lock.toml: every
compiler, interpreter and CLI is a real in-graph Artifact, but the scripts
that stage and probe them still call the host's own tar, unzip, sha256sum,
find, unshare and ip. Previously only tar was acknowledged anywhere, and a
host missing one of the others failed with whatever diagnostic that specific
utility's absence produced - `unzip: not found` from inside a buck2 action
log, with no hint about which toolchain target needed it or what to install.

`require_host_tools([...])` returns shell lines to splice in at the TOP of a
generated script, before any use, so the failure is a single actionable
message naming the tool and what it is for.

sha256sum additionally has a FORMAT dependency, not just a presence one:
rules/toolchain.bzl parses its output with `cut -d ' ' -f1`, which assumes
GNU coreutils' "<hash>  <path>" two-space form. That is checked by
`sha256sum_guard()` below rather than assumed.
"""

# Tool name -> why this project needs it, quoted into the error message so a
# host missing it learns which lane to look at.
_PURPOSE = {
  # NOTE: no apostrophes in these strings. They are spliced into a
  # single-quoted shell `echo`, where an apostrophe terminates the literal
  # and turns the rest of the message into syntax errors.
  "tar": "extract .tar.gz/.tar.xz toolchain archives and package archives - see rules/toolchain.bzl and rules/package.bzl",
  "unzip": "extract .zip toolchain archives - see rules/toolchain.bzl",
  "sha256sum": "verify a staged toolchain archive against its pinned sha256 - see rules/toolchain.bzl",
  "find": "prune __pycache__ and *.pyc from a staged package - see rules/package.bzl",
  "unshare": "run deno inside a no-network user namespace - see the offline enforcement in rules/deno.bzl",
  "ip": "bring loopback up inside that network namespace - see the offline enforcement in rules/deno.bzl",
}

def require_host_tools(tools: list[str]) -> list[str]:
  lines = []
  for tool in tools:
    purpose = _PURPOSE.get(tool)
    if purpose == None:
      fail("require_host_tools: no documented purpose for {!r} - add one to rules/hosttools.bzl".format(tool))
    lines += [
      "command -v %s >/dev/null 2>&1 || {" % tool,
      "  echo 'error: required host tool %s not found on PATH. This project pins every compiler and interpreter, but still needs the host to provide %s (used to: %s). Install it and retry.' >&2" % (tool, tool, purpose),
      "  exit 1",
      "}",
    ]
  return lines

def sha256sum_guard() -> list[str]:
  """Presence guard for sha256sum PLUS a one-shot check that its output is
  GNU coreutils' two-space "<hash>  <path>" format, since rules/toolchain.bzl
  slices field 1 out of it with `cut -d ' ' -f1`. BSD/macOS sha256sum
  substitutes print a different shape, which `cut` would happily turn into a
  wrong-but-plausible hash and therefore a bogus mismatch error."""
  return require_host_tools(["sha256sum"]) + [
    "if [ \"$(printf '' | sha256sum | cut -d ' ' -f1)\" != \"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855\" ]; then",
    "  echo 'error: sha256sum on this host does not print GNU coreutils'\"'\"' \"<hash>  <path>\" format, which this script parses with `cut -d \" \" -f1`. Install GNU coreutils sha256sum and retry.' >&2",
    "  exit 1",
    "fi",
  ]
