"""Profile (dbg/opt) configuration for the first-party cpp lane.

Defines a `profile` constraint_setting with dbg/opt constraint_values, plus
one platform() per (native-or-cross arch x profile) pair for completeness /
`--target-platforms` use. rules/cxx.bzl's flag selection (config/flags.bzl)
keys off the bare constraint_value targets directly via select(), which is
what makes the invocation below work without requiring a full platform
switch.

Invocation:
 buck2 build //cpp/...                    # profile defaults to dbg (DEFAULT branch of every select())
 buck2 build -m //config:opt //cpp/...    # -m/--modifier accepts a bare constraint_value target
                      # per `buck2 build --help`; verified against the pinned
                      # buck2 binary (buck2 769ca62...) for this repo.
 buck2 build --target-platforms //config:x86_64-linux-musl-opt //cpp/...  # explicit platform, if -m
                      # is ever unavailable on a future pinned buck2 version.
"""

load("//platforms:defs.bzl", "platform")
load("//rules:constraint.bzl", "constraint_setting", "constraint_value")

_ARCHES = ["x86_64-linux-musl", "aarch64-linux-musl"]
_PROFILES = ["dbg", "opt"]

def define_config():
  constraint_setting(name = "profile")
  for name in _PROFILES:
    constraint_value(name = name, setting = ":profile")
  for arch in _ARCHES:
    for name in _PROFILES:
      platform(
        name = "{}-{}".format(arch, name),
        constraint_values = [":" + name],
      )
