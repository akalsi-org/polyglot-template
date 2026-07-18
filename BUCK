load("//rules:file.bzl", "export_file")
load("//rules:group.bzl", "group")

# Uniform build/test/lint entry points across all five lanes.
#
# //:build is a real, buildable group(): `buck2 build //:build` builds every
# lane's primary build output (mirroring moon.yml's own top-level `build`
# task, which depends on cpp-build/python-build/ts-build/go-build/
# tsweb-build) plus the packaged polyglot-demo application.
#
# There is deliberately no //:test or //:lint group target. Buck2 has no
# prelude here to supply test_suite()/alias(), and a first-party rule can
# forward at most one ExternalRunnerTestInfo - see rules/group.bzl's doc
# comment for why that makes a *runnable* many-tests-in-one-target group
# unbuildable in general (empirically: `buck2 test` on a target that has no
# ExternalRunnerTestInfo of its own, such as a group() of test deps, reports
# "NO TESTS RAN" rather than running its deps' tests or erroring).
#
# Use `buck2 test //...` to run all 20 tests, including every lane's
# lint-as-test targets (go:gofmt_check, go:go_vet, ts:fmt_check, ts:lint,
# tsweb:fmt_check, tsweb:lint, python/test:compileall, plus the doctest and
# drift/manifest tests) - there is no separate "lint" verb in this buck2
# graph, matching repo.sh's own lint lane being per-language rather than a
# single command. To run one lane's tests (including its lint-as-test
# targets) only, use that lane's own package pattern, e.g.:
#   buck2 test //cpp/...
#   buck2 test //go/...
#   buck2 test //python/...
#   buck2 test //ts/...
#   buck2 test //tsweb/...
#   buck2 test //packages/...
# Per-lane lint-only invocation would need a `labels = ["lint"]` (or
# equivalent) attribute threaded through each lane's own lint rule
# (rules/go.bzl's go_lint, rules/deno.bzl's deno_lint, rules/python.bzl's
# py_compileall_check) so `buck2 test //... --labels lint` could select
# them; none of those rules currently accept a labels attr, and adding one
# is outside this change's file ownership (root BUCK, toolchains/,
# tools/lint.py, .github/workflows/ci-release.yml, rules/group.bzl only) -
# flagged for whoever owns those lane rule files next.
group(
  name = "build",
  deps = [
    "//cpp/app/hello:hello",
    "//go:hello",
    "//python/app:hello",
    "//ts:check",
    "//tsweb:site",
    "//packages:polyglot-demo",
  ],
  visibility = ["PUBLIC"],
)

export_file(
  name = "deno.json",
  src = "deno.json",
  visibility = ["PUBLIC"],
)

export_file(
  name = "deno.lock",
  src = "deno.lock",
  visibility = ["PUBLIC"],
)

export_file(
  name = "go.mod",
  src = "go.mod",
  visibility = ["PUBLIC"],
)

export_file(
  name = "tsweb_smoke.py",
  src = "tools/tsweb_smoke.py",
  visibility = ["PUBLIC"],
)

export_file(
  name = "package.toml",
  src = "package.toml",
  visibility = ["PUBLIC"],
)

export_file(
  name = "runtime-resolution.lock.toml",
  src = "runtime-resolution.lock.toml",
  visibility = ["PUBLIC"],
)

export_file(
  name = "tools.lock.toml",
  src = "tools.lock.toml",
  visibility = ["PUBLIC"],
)

export_file(
  name = "CHANGELOG.md",
  src = "CHANGELOG.md",
  visibility = ["PUBLIC"],
)

export_file(
  name = "package_model.py",
  src = "tools/package_model.py",
  visibility = ["PUBLIC"],
)
