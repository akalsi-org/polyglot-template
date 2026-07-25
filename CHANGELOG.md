# Changelog

## packages/polyglot-demo/v0.1.0

- Packages real C++, Go, Python/C++, and React 19 outputs with one bundled CPython runtime per target.
- Runs every executable and validates the static web bundle from an isolated extraction.

## packages/polyglot-server/v0.1.0

- Packages the Deno server application with its pinned Deno runtime and a generated launcher.
- Supports a zero-npm-dependency application closure and verifies `server --smoke` from an isolated extraction.

## packages/gateway/v1.4.0

- RESERVED, NOT RELEASED. `gateway` is a catalog-only declaration in `packages/catalog.bzl` with no `//packages:gateway` Buck target, so `./repo.sh package gateway` and the tagged release path fail closed by design.
- Reserves the intended gateway application package contract with exact CPython runtime closure metadata. This section exists because a changelog section is a `release-check` precondition; it becomes a real release only once the in-graph `package()` target exists.

## packages/schema-cli/v0.8.2

- RESERVED, NOT RELEASED. `schema-cli` is a catalog-only declaration with no `//packages:schema-cli` Buck target and fails closed the same way.
- Reserves the intended schema CLI native package contract, on the same terms as `gateway` above.
