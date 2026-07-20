# Changelog

## packages/polyglot-demo/v0.1.0

- Packages real C++, Go, Python/C++, and React 19 outputs with one bundled CPython runtime per target.
- Runs every executable and validates the static web bundle from an isolated extraction.

## packages/polyglot-server/v0.1.0

- Packages the Deno server application with its pinned Deno runtime and a generated launcher.
- Supports a zero-npm-dependency application closure and verifies `server --smoke` from an isolated extraction.

## packages/gateway/v1.4.0

- Establishes the gateway application package contract with exact CPython runtime closure metadata.

## packages/schema-cli/v0.8.2

- Establishes the schema CLI native package contract.
