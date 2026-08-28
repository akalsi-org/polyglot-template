load("//rules:deno.bzl", "deno_cache")
load("//rules:file.bzl", "export_file", "export_files")

# Buck2 owns build, test, lint, coverage, and package graph execution.
# repo.sh discovers Go, Python, Deno, and web outputs by rule kind.
# The pinned Python runtime uses the packaged musl loader.
# deno-cache is shared by the TypeScript and web lanes.
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
  name = "go.sum",
  src = "go.sum",
  visibility = ["PUBLIC"],
)

# Keep every vendored file as a real Buck input: Go resolves normal external
# modules from this committed tree with -mod=vendor, so an edit to any package
# invalidates the actions that compile or test it.
export_files(
  name = "go_vendor",
  srcs = glob(["vendor/**"]),
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
