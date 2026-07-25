"""Profile (dbg/opt) configuration for the first-party cpp lane.

Defines a `profile` constraint_setting with dbg/opt constraint_values, plus
one platform() per (native-or-cross arch x profile) pair for completeness /
`--target-platforms` use. rules/cxx.bzl's flag selection (config/flags.bzl)
keys off the bare constraint_value targets directly via select(), which is
what makes the invocation below work without requiring a full platform
switch.

Invocation:
 buck2 build //cpp/...                    # profile defaults to dbg (DEFAULT branch of every select())
 buck2 build --target-platforms //config:x86_64-linux-musl-opt //cpp/...  # opt: the reliable form
 buck2 build -m //config:opt <target>     # -m/--modifier parses, BUT verified on the pinned buck2
                      # (769ca62...): it does NOT override a target's own
                      # default_target_platform, and every test/binary macro in
                      # rules/ injects one — cquery under -m still resolves the
                      # -dbg configuration for those targets. Prefer
                      # --target-platforms for any opt invocation.

Cross-arch is a declared but NOT built dimension: every platform() below also
carries an `arch` constraint_value (x86_64-linux-musl / aarch64-linux-musl),
so `--target-platforms //config:aarch64-linux-musl-opt` resolves and selects
a real platform label. But every rule in rules/ (cxx.bzl, go.bzl, python.bzl,
package.bzl) picks its toolchain deps (`//toolchains:gcc-musl-<X>`, etc.) off
a load-time `_NATIVE_TARGET` computed from `host_info().arch` - i.e. the
buck2-daemon host's own CPU, never the resolved target platform's arch. On an
x86_64 host, selecting the aarch64 platform therefore does NOT cross-compile;
it silently reuses the host's x86_64 toolchain under an aarch64-labeled
configuration. Full cross-compilation is out of scope for this project (no
aarch64-hosted toolchain artifacts are pinned in tools.lock.toml). Instead,
`target_arch_attr()` below threads the resolved platform's arch through as a
select()-backed attr, and each rules/*.bzl file's toolchain-tools helper
calls `fail_if_cross_arch()` first thing so a mismatched platform fails loud
at analysis time instead of silently building host-arch output under a
foreign-arch label.
"""

load("//platforms:defs.bzl", "platform")
load("//rules:constraint.bzl", "constraint_setting", "constraint_value")
load("//rules:host.bzl", "native_target")

_ARCHES = ["x86_64-linux-musl", "aarch64-linux-musl"]
_PROFILES = ["dbg", "opt"]

_NATIVE_TARGET = native_target()

def define_config():
  constraint_setting(name = "profile")
  for name in _PROFILES:
    constraint_value(name = name, setting = ":profile")
  constraint_setting(name = "arch")
  for arch in _ARCHES:
    constraint_value(name = arch, setting = ":arch")
  for arch in _ARCHES:
    for name in _PROFILES:
      platform(
        name = "{}-{}".format(arch, name),
        constraint_values = [":" + name, ":" + arch],
      )

def target_arch_attr():
  # A hidden attr every rules/*.bzl toolchain-consuming rule adds to its
  # attrs (alongside _gcc/_go/_python/etc.): resolves via select() to the
  # STRING name of the resolved target platform's own arch constraint_value
  # (set on every platform() by define_config() above), independent of the
  # buck2-daemon host's own CPU. DEFAULT (no explicit --target-platforms /
  # a platform predating this constraint) falls back to the host-native
  # target, matching every rule file's own load-time _NATIVE_TARGET so
  # ordinary native builds are unaffected.
  return attrs.string(default = select({
    "//config:x86_64-linux-musl": "x86_64-linux-musl",
    "//config:aarch64-linux-musl": "aarch64-linux-musl",
    "DEFAULT": _NATIVE_TARGET,
  }))

def fail_if_cross_arch(ctx, native_target):
  # Called first thing by every rules/*.bzl toolchain-tools helper (see
  # this module's docstring). ctx.attrs._target_arch is native_target's
  # counterpart resolved off the ACTUAL target platform buck2 picked for
  # this configured target, so a mismatch here means the target was reached
  # under a foreign-arch platform whose toolchain deps this rule cannot
  # actually cross-compile for.
  if ctx.attrs._target_arch != native_target:
    fail((
      "{}: target platform arch {!r} does not match this build's " +
      "host-native arch {!r}. This project pins toolchain artifacts for " +
      "the host's own architecture only (see tools.lock.toml) and does " +
      "not support cross-compilation - build/test this target from a " +
      "{} host instead, or drop --target-platforms to use the default " +
      "native platform."
    ).format(ctx.label.raw_target(), ctx.attrs._target_arch, native_target, native_target))
