load("//rules:deno.bzl", "deno_cache")
load("//rules:file.bzl", "export_file")

# Uniform build/test/lint entry points across all five lanes.
#
# There is deliberately NO hand-listed //:build or //:coverage target:
# hand-maintained dep lists rot the moment a lane gains a target, which
# defeats "works out of the box". Instead the graph itself is the list -
# `./repo.sh build` discovers every lane's primary build output by rule
# kind (uquery over _cxx_binary/_go_binary/_py_binary/_deno_check/
# _vite_build rules) and `./repo.sh coverage` discovers every
# CoverageInfo-bearing test the same way via bxl/coverage.bxl. A new
# app/site/test participates automatically by existing. Packaging is
# deliberately NOT part of the build set - archives are release work
# (`buck2 build //packages/...` or `./repo.sh package <name>`), not the
# dev loop.
#
# There is deliberately no //:test or //:lint group target. Buck2 has no
# prelude here to supply test_suite()/alias(), and a first-party rule can
# forward at most one ExternalRunnerTestInfo - see rules/group.bzl's doc
# comment for why that makes a *runnable* many-tests-in-one-target group
# unbuildable in general (empirically: `buck2 test` on a target that has no
# ExternalRunnerTestInfo of its own, such as a group() of test deps, reports
# "NO TESTS RAN" rather than running its deps' tests or erroring).
#
# Use `buck2 test //...` to run all 22 tests, including every lane's
# lint-as-test targets (go:gofmt_check, go:go_vet, ts:fmt_check, ts:lint,
# tsweb:fmt_check, tsweb:lint, python/test:compileall, the two
# deno_graph_check drift checks - ts/test:graph_check, tsweb/test:graph_check
# - plus the doctest and drift/manifest tests) - there is no separate "lint"
# verb in this buck2 graph, matching repo.sh's own lint lane being
# per-language rather than a single command. To run one lane's tests
# (including its lint-as-test targets) only, use that lane's own package
# pattern, e.g.:
#   buck2 test //cpp/...
#   buck2 test //go/...
#   buck2 test //python/...
#   buck2 test //ts/...
#   buck2 test //tsweb/...
#   buck2 test //packages/...
# Per-lane lint-only invocation: DONE. Every lane's lint-as-test rules
# (rules/go.bzl's go_lint, rules/deno.bzl's deno_lint, deno_graph_check, and
# tsconfig_drift_test, rules/python.bzl's py_compileall_check and
# py_lock_consistency_test) now set `labels = ["lint"]` on their
# ExternalRunnerTestInfo. This buck2 version's ExternalRunnerTestInfo does
# accept a `labels` attribute (verified: `buck2 docs starlark` doesn't cover
# builtin providers directly, so this was verified empirically instead - a
# labels=[...] kwarg analyzed and built cleanly), and `buck2 test --labels
# <label>` DOES filter to only matching-labeled targets on this pinned
# buck2 (769ca62...) - verified against `buck2 test //go:gofmt_check
# //go:go_vet //go/test:greeting_test --labels lint`, which ran only the two
# lint targets and skipped greeting_test. So per-lane lint-only invocation
# now works as a single invocation across the whole graph:
#   buck2 test //... --labels lint
# (verified: 11 lint-as-test targets ran - go:gofmt_check, go:go_vet,
# ts:fmt_check, ts:lint, ts/test:graph_check, tsweb:fmt_check, tsweb:lint,
# tsweb:tsconfig_drift, tsweb/test:graph_check, python/test:compileall,
# python/test:lock_consistency - and nothing else).
# Coverage: DEFAULT-ON-FOR-dbg (see rules/coverage.bzl's module docstring
# for the full design). `./repo.sh coverage` runs bxl/coverage.bxl, which
# queries every instrumented-lane test by rule kind, skips any without a
# CoverageInfo provider (lint-as-test targets; everything under opt), and
# merges the rest into one lcov + summary.
#
# `-m //config:opt` caveat (found while verifying coverage, applies
# repo-wide): every first-party rule() here that
# has its own `default_target_platform` macro (cxx_test/cxx_binary/
# go_test/py_test/deno_test/group) keeps that default even
# when `-m //config:opt` is passed on the command line for that exact
# top-level target - verified empirically (`buck2 cquery -m //config:opt
# //cpp/test:example_test` still resolves to the "...-dbg" configuration,
# identical to a bare `buck2 cquery //cpp/test:example_test`). Only
# `--target-platforms //config:x86_64-linux-musl-opt` reliably selects opt
# for these targets; `-m` only takes effect for targets that do NOT declare
# their own default_target_platform. So to actually exercise opt (e.g. to
# confirm coverage merges zero entries there, which it does - verified),
# use:
#   buck2 test --target-platforms //config:x86_64-linux-musl-opt //...
# `config/defs.bzl`'s own docstring claims `-m` works for `//cpp/...`
# - that claim was not re-verified here for non-test/non-binary targets, but
# does not hold for this repo's test/binary rules specifically.
# //:deno-cache - the one deno population action shared by the ts/ and
# tsweb/ lanes (deno cache resolves the whole graph in one pass, so it
# belongs to neither lane; it lives here at the root as cross-lane
# infrastructure, next to the deno.json/deno.lock it consumes). It
# resolves STRICTLY OFFLINE from bootstrap's .local/cache/deno seed,
# enforced by running deno inside a no-network user namespace (fail-closed
# when namespaces are unavailable, absent an explicit env opt-out) - graph
# actions never fetch; see rules/deno.bzl's deno_cache doc comment. Buck
# cannot glob across nested BUCK packages, so its dependency closure remains
# explicit and repo.sh's graph query validates it. The rule derives every
# cache entry from that closure and emits a manifest subtarget that bootstrap
# consumes before it seeds the offline Deno store: no second entrypoint list.
deno_cache(
  name = "deno-cache",
  deps = [
    "//ts/app/hello:hello",
    "//ts/app/server:server",
    "//ts/lib/greeting:greeting",
    "//ts/test:test_srcs",
    "//tsweb/app/site:site",
    "//tsweb/lib/app:app",
    "//tsweb/lib/title:title",
    "//tsweb/test:test_srcs",
  ],
  srcs = {"tsweb/vite.config.ts": "//tsweb:vite.config.ts"},
  entries = ["npm:pyright@1.1.407"],
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
  name = "pyrightconfig.json",
  src = "pyrightconfig.json",
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
# rules/python.bzl's py_test (py_cover.py) and bxl/coverage.bxl
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

export_file(
  name = "py_test_runner.py",
  src = "tools/py_test_runner.py",
  visibility = ["PUBLIC"],
)

export_file(
  name = "deno_store_prune.py",
  src = "tools/deno_store_prune.py",
  visibility = ["PUBLIC"],
)

export_file(
  name = "deno_cache_exec.py",
  src = "tools/deno_cache_exec.py",
  visibility = ["PUBLIC"],
)
