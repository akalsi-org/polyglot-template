load("//rules:coverage.bzl", "coverage_report")
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
# Per-lane lint-only invocation: DONE. Every lane's lint-as-test rules
# (rules/go.bzl's go_lint, rules/deno.bzl's deno_lint and
# tsconfig_drift_test, rules/python.bzl's py_compileall_check and
# py_lock_consistency_test) now set `labels = ["lint"]` on their
# ExternalRunnerTestInfo. This buck2 version's ExternalRunnerTestInfo does
# accept a `labels` attribute (verified: `buck2 docs starlark` doesn't cover
# builtin providers directly, so this was verified empirically instead - a
# labels=[...] kwarg analyzed and built cleanly), and `buck2 test --labels
# <label>` DOES filter to only matching-labeled targets on this pinned
# buck2 (769ca62...) - verified against `buck2 test //go:gofmt_check
# //go:go_vet //go:greeting_test --labels lint`, which ran only the two
# lint targets and skipped greeting_test. So per-lane lint-only invocation
# now works as a single invocation across the whole graph:
#   buck2 test //... --labels lint
# (verified: 9 lint-as-test targets ran - go:gofmt_check, go:go_vet,
# ts:fmt_check, ts:lint, tsweb:fmt_check, tsweb:lint, tsweb:tsconfig_drift,
# python/test:compileall, python/test:lock_consistency - and nothing else).
# //:coverage: DEFAULT-ON-FOR-dbg coverage merge target (see
# rules/coverage.bzl's module docstring for the full design). deps is just
# "every test target" across the four instrumented lanes (cpp/test's three
# doctest targets, go's greeting_test, python/test's unittest, ts/tsweb's
# deno_test targets) - coverage_report silently skips any dep without a
# CoverageInfo provider, so lint-as-test targets don't need to be excluded
# by hand.
#
# `-m //config:opt` caveat (found while verifying this target, applies
# repo-wide, not just to //:coverage): every first-party rule() here that
# has its own `default_target_platform` macro (cxx_test/cxx_binary/
# go_test/py_test/deno_test/group/coverage_report) keeps that default even
# when `-m //config:opt` is passed on the command line for that exact
# top-level target - verified empirically (`buck2 cquery -m //config:opt
# //cpp/test:example_test` still resolves to the "...-dbg" configuration,
# identical to a bare `buck2 cquery //cpp/test:example_test`). Only
# `--target-platforms //config:x86_64-linux-musl-opt` reliably selects opt
# for these targets; `-m` only takes effect for targets that do NOT declare
# their own default_target_platform. So to actually exercise opt (e.g. to
# confirm //:coverage merges zero entries there, which it does - verified),
# use:
#   buck2 build --target-platforms //config:x86_64-linux-musl-opt //:coverage
#   buck2 test --target-platforms //config:x86_64-linux-musl-opt //...
# `config/defs.bzl`'s own docstring claims `-m` works for `//cpp/...`
# - that claim was not re-verified here for non-test/non-binary targets, but
# does not hold for this repo's test/binary rules specifically.
coverage_report(
  name = "coverage",
  deps = [
    "//cpp/test:example_test",
    "//cpp/test:example_edge_test",
    "//cpp/test:pgt_core_test",
    "//go:greeting_test",
    "//python/test:unittest",
    "//ts:test",
    "//tsweb:test",
  ],
  visibility = ["PUBLIC"],
)

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

# Coverage lane plumbing (see rules/coverage.bzl's module docstring for the
# overall design). tools/ has no BUCK file of its own - every file under it
# is a member of this root package - so these two new scripts are exported
# the same way tsweb_smoke.py/package_model.py above already are, purely so
# rules/python.bzl's py_test (py_cover.py) and coverage_report below
# (coverage_merge.py) can depend on them across the package boundary.
export_file(
  name = "coverage_merge.py",
  src = "tools/coverage_merge.py",
  visibility = ["PUBLIC"],
)

export_file(
  name = "py_cover.py",
  src = "tools/py_cover.py",
  visibility = ["PUBLIC"],
)
