"""Minimal first-party platform() / execution_platforms() rules.

There is no prelude in this project, so buck2 has no built-in notion of a
platform target: these two rules are the smallest possible replacement,
enough to give bxl_actions() (see toolchains/compdb.bxl) a concrete execution
platform to bind to.
"""

def _platform_impl(ctx: AnalysisContext) -> list[Provider]:
  constraints = {}
  for dep in ctx.attrs.constraint_values:
    value = dep[ConstraintValueInfo]
    constraints[value.setting.label] = value
  return [
    DefaultInfo(),
    PlatformInfo(
      label = str(ctx.label.raw_target()),
      configuration = ConfigurationInfo(constraints = constraints, values = {}),
    ),
  ]

platform = rule(
  impl = _platform_impl,
  attrs = {
    # Empty by default: execution platforms (see :host below) carry no
    # constraints. config/defs.bzl reuses this same rule for target
    # platforms and does pass constraint_values (e.g. the profile
    # dbg/opt constraint_value targets).
    "constraint_values": attrs.list(attrs.dep(providers = [ConstraintValueInfo]), default = []),
  },
)

def _execution_platforms_impl(ctx: AnalysisContext) -> list[Provider]:
  return [
    DefaultInfo(),
    ExecutionPlatformRegistrationInfo(
      platforms = [
        ExecutionPlatformInfo(
          label = dep.label.raw_target(),
          configuration = dep[PlatformInfo].configuration,
          executor_config = CommandExecutorConfig(
            local_enabled = True,
            remote_enabled = False,
          ),
        )
        for dep in ctx.attrs.platforms
      ],
    ),
  ]

execution_platforms = rule(
  impl = _execution_platforms_impl,
  attrs = {
    "platforms": attrs.list(attrs.dep(providers = [PlatformInfo])),
  },
)
