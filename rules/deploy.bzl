"""Read-only deployment-plan rules.

Deployment intent is declared in BUCK attributes, not an ad-hoc TOML or YAML
file.  This rule deliberately emits only a deterministic plan artifact and a
local inspection command; it has no remote transport or activation surface.
"""

def _valid_dns_label(value):
  if value == "" or len(value) > 63 or value.startswith("-") or value.endswith("-"):
    return False
  for char in value.elems():
    if not ((char >= "a" and char <= "z") or (char >= "0" and char <= "9") or char == "-"):
      return False
  return True

def _all_digits(values):
  for value in values:
    if not value.isdigit():
      return False
  return True

def _package_name(spec):
  parts = spec.split("@")
  if len(parts) != 2 or parts[0] == "" or parts[1] == "":
    fail("deployment_plan: package {!r} must be name@major.minor.patch".format(spec))
  version = parts[1].split(".")
  if len(version) != 3 or not _all_digits(version):
    fail("deployment_plan: package {!r} must use major.minor.patch".format(spec))
  return parts[0]

def _deployment_plan_impl(ctx):
  packages = sorted(ctx.attrs.packages)
  package_names = {}
  for spec in packages:
    name = _package_name(spec)
    if name in package_names:
      fail("deployment_plan: duplicate package name {!r}".format(name))
    package_names[name] = True

  if not _valid_dns_label(ctx.attrs.group):
    fail("deployment_plan: group must be a single DNS label")
  if not _valid_dns_label(ctx.attrs.environment):
    fail("deployment_plan: environment must be a single DNS label")
  if ctx.attrs.domain == "" or "." not in ctx.attrs.domain:
    fail("deployment_plan: domain must be a non-empty DNS suffix")

  routes = []
  for label, package in sorted(ctx.attrs.routes.items()):
    if not _valid_dns_label(label):
      fail("deployment_plan: route label {!r} must be a single DNS label".format(label))
    if package not in package_names:
      fail("deployment_plan: route {!r} references undeclared package {!r}".format(label, package))
    routes.append({"hostname": label + "." + ctx.attrs.domain, "package": package})

  if len(ctx.attrs.hosts) == 0:
    fail("deployment_plan: at least one eligible host is required")
  seen_hosts = {}
  for host in ctx.attrs.hosts:
    if host in seen_hosts:
      fail("deployment_plan: eligible hosts must be unique")
    seen_hosts[host] = True

  plan = {
    "schema": "polyglot.deployment-plan/v1",
    "group": ctx.attrs.group,
    "environment": ctx.attrs.environment,
    "target": ctx.attrs.target,
    "packages": packages,
    "eligible_hosts": sorted(ctx.attrs.hosts),
    "routes": routes,
  }
  output = ctx.actions.write_json(ctx.attrs.name + ".json", plan, pretty = True)
  script, written = ctx.actions.write(
    ctx.attrs.name + ".sh",
    [
      "#!/bin/sh",
      "set -eu",
      cmd_args("PLAN=\"$(pwd)/", output, "\"", delimiter = ""),
      cmd_args("TOOL=\"$(pwd)/", ctx.attrs.tool, "\"", delimiter = ""),
      "exec python3 -B \"$TOOL\" plan --plan \"$PLAN\"",
    ],
    is_executable = True,
    allow_args = True,
  )
  command = cmd_args(script, hidden = [output, ctx.attrs.tool] + written)
  return [
    DefaultInfo(default_output = output, other_outputs = written),
    RunInfo(args = command),
  ]

deployment_plan = rule(
  impl = _deployment_plan_impl,
  attrs = {
    "domain": attrs.string(),
    "environment": attrs.string(),
    "group": attrs.string(),
    "hosts": attrs.list(attrs.string()),
    "packages": attrs.list(attrs.string()),
    "routes": attrs.dict(attrs.string(), attrs.string()),
    "target": attrs.string(),
    "tool": attrs.source(),
  },
)

def _readonly_contract_test_impl(ctx):
  plan = ctx.attrs.plan[DefaultInfo].default_outputs[0]
  command = cmd_args(["python3", ctx.attrs.tool, "contract", "--plan", plan])
  return [
    DefaultInfo(default_output = plan),
    ExternalRunnerTestInfo(
      type = "deployment_readonly_contract",
      command = [command],
      run_from_project_root = True,
    ),
  ]

deployment_readonly_contract_test = rule(
  impl = _readonly_contract_test_impl,
  attrs = {
    "plan": attrs.dep(providers = [DefaultInfo]),
    "tool": attrs.source(),
  },
)
