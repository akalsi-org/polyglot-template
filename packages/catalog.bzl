"""Canonical package and runtime-release catalog.

This is the single source of truth for every package identity, supported
platform, executable contract, runtime requirement, runtime artifact, and
resolved runtime closure.  Keep it literal-only: repository tooling reads the
``PACKAGE_CATALOG`` assignment without evaluating Starlark, while Buck loads
the same value when declaring package targets.
"""

PACKAGE_CATALOG = {
  "schema_version": 1,
  # Deployment admission metadata. The target triple alone does not say which
  # CPU may execute the native code inside a package.
  "native_isa_baselines": {
    "x86_64-linux-musl": "x86-64-v3; mtune=generic; writable-prefetch enabled",
    "aarch64-linux-musl": "Armv8.2-A",
  },
  "packages": [
    {
      "name": "polyglot-demo",
      "version": "0.1.0",
      "kind": "application",
      "executables": ["go-hello", "python-hello"],
      "supported_targets": ["x86_64-linux-musl", "aarch64-linux-musl"],
      "runtime": {"name": "python", "version": "^3.14", "variant": "install_only_stripped"},
    },
    {
      "name": "gateway",
      "version": "1.4.0",
      "kind": "application",
      "executables": ["gateway", "gateway-admin"],
      "supported_targets": ["x86_64-linux-musl", "aarch64-linux-musl"],
      "runtime": {"name": "python", "version": "^3.14", "variant": "install_only_stripped"},
    },
    {
      "name": "schema-cli",
      "version": "0.8.2",
      "kind": "application",
      "executables": ["schema-cli"],
      "supported_targets": ["x86_64-linux-musl", "aarch64-linux-musl"],
    },
    {
      "name": "polyglot-server",
      "version": "0.1.0",
      "kind": "application",
      "executables": ["server"],
      "supported_targets": ["x86_64-linux-musl", "aarch64-linux-musl"],
      "smoke_args": {"server": ["--smoke"]},
    },
    {
      "name": "runtime-python",
      "version": "3.14.6",
      "kind": "runtime",
      "runtime_name": "python",
      "variant": "install_only_stripped",
      "loader_tool": "gcc-musl",
      "supported_targets": ["x86_64-linux-musl", "aarch64-linux-musl"],
      "artifacts": [
        {"target": "x86_64-linux-musl", "filename": "cpython-3.14.6+20260610-x86_64-unknown-linux-musl-install_only_stripped.tar.gz", "sha256": "54eca143d09ed3c596ecad5e3bfcb7724387818f8f984f44f3d8c0e9f36681d3"},
        {"target": "aarch64-linux-musl", "filename": "cpython-3.14.6+20260610-aarch64-unknown-linux-musl-install_only_stripped.tar.gz", "sha256": "5c75bea22f425ebc94912c618518a5aa4753eedeea6b5da5939f027d3a9c0fec"},
      ],
    },
  ],
  "runtime_resolutions": [
    {"package": "polyglot-demo", "target": "x86_64-linux-musl", "runtime_package": "runtime-python", "runtime_version": "3.14.6", "variant": "install_only_stripped", "release_tag": "packages/runtime-python/v3.14.6", "artifact": "cpython-3.14.6+20260610-x86_64-unknown-linux-musl-install_only_stripped.tar.gz", "sha256": "54eca143d09ed3c596ecad5e3bfcb7724387818f8f984f44f3d8c0e9f36681d3", "loader_version": "16.1.0-userdocs-2628", "loader_sha256": "a6a1ec6a830777eac1982909ba50c9b6b53853166051edcf9bb6b6bf9581bff6", "loader_path": "x86_64-linux-musl/x86_64-linux-musl/lib/ld-musl-x86_64.so.1"},
    {"package": "polyglot-demo", "target": "aarch64-linux-musl", "runtime_package": "runtime-python", "runtime_version": "3.14.6", "variant": "install_only_stripped", "release_tag": "packages/runtime-python/v3.14.6", "artifact": "cpython-3.14.6+20260610-aarch64-unknown-linux-musl-install_only_stripped.tar.gz", "sha256": "5c75bea22f425ebc94912c618518a5aa4753eedeea6b5da5939f027d3a9c0fec", "loader_version": "16.1.0-userdocs-2628", "loader_sha256": "770f2fcd3e74a965fa958b82406fabb02114dfc2c0ea40deadb70d96088a3abf", "loader_path": "aarch64-linux-musl/aarch64-linux-musl/lib/ld-musl-aarch64.so.1"},
    {"package": "gateway", "target": "x86_64-linux-musl", "runtime_package": "runtime-python", "runtime_version": "3.14.6", "variant": "install_only_stripped", "release_tag": "packages/runtime-python/v3.14.6", "artifact": "cpython-3.14.6+20260610-x86_64-unknown-linux-musl-install_only_stripped.tar.gz", "sha256": "54eca143d09ed3c596ecad5e3bfcb7724387818f8f984f44f3d8c0e9f36681d3", "loader_version": "16.1.0-userdocs-2628", "loader_sha256": "a6a1ec6a830777eac1982909ba50c9b6b53853166051edcf9bb6b6bf9581bff6", "loader_path": "x86_64-linux-musl/x86_64-linux-musl/lib/ld-musl-x86_64.so.1"},
    {"package": "gateway", "target": "aarch64-linux-musl", "runtime_package": "runtime-python", "runtime_version": "3.14.6", "variant": "install_only_stripped", "release_tag": "packages/runtime-python/v3.14.6", "artifact": "cpython-3.14.6+20260610-aarch64-unknown-linux-musl-install_only_stripped.tar.gz", "sha256": "5c75bea22f425ebc94912c618518a5aa4753eedeea6b5da5939f027d3a9c0fec", "loader_version": "16.1.0-userdocs-2628", "loader_sha256": "770f2fcd3e74a965fa958b82406fabb02114dfc2c0ea40deadb70d96088a3abf", "loader_path": "aarch64-linux-musl/aarch64-linux-musl/lib/ld-musl-aarch64.so.1"},
  ],
}

def package_by_name(name):
  for package in PACKAGE_CATALOG["packages"]:
    if package["name"] == name:
      return package
  fail("unknown package: {}".format(name))
