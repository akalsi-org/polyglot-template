"""Minimal first-party constraint_setting() / constraint_value() rules.

There is no prelude in this project, so buck2 has no built-in notion of these
either (they are normally supplied by @prelude//rules.bzl). This is the
smallest replacement, enough for config/defs.bzl to define a `profile`
configuration dimension that rules/cxx.bzl's select()s read.
"""

def _constraint_setting_impl(ctx: AnalysisContext) -> list[Provider]:
  return [DefaultInfo(), ConstraintSettingInfo(label = ctx.label.raw_target())]

constraint_setting = rule(
  impl = _constraint_setting_impl,
  attrs = {},
)

def _constraint_value_impl(ctx: AnalysisContext) -> list[Provider]:
  setting = ctx.attrs.setting[ConstraintSettingInfo]
  value = ConstraintValueInfo(setting = setting, label = ctx.label.raw_target())
  return [
    DefaultInfo(),
    value,
    ConfigurationInfo(constraints = {setting.label: value}, values = {}),
  ]

constraint_value = rule(
  impl = _constraint_value_impl,
  attrs = {
    "setting": attrs.dep(providers = [ConstraintSettingInfo]),
  },
)
