#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
tool="$root/tools/package_release.py"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

sed -e 's/loader_sha256 = "UNRESOLVED"/loader_sha256 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"/g' \
    -e 's|loader_path = "UNRESOLVED:[^"]*"|loader_path = "lib/ld-musl.so.1"|g' \
    "$root/runtime-resolution.lock.toml" >"$tmp/resolved-runtime.lock.toml"
sed -e '0,/sha256 = "UNRESOLVED"/s//sha256 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"/' \
    -e '0,/loader = "UNRESOLVED:[^"]*"/s//loader = "lib\/ld-musl.so.1"/' \
    "$root/tools.lock.toml" >"$tmp/resolved-tools.lock.toml"

build="$tmp/build"
mkdir -p "$build/bin"
printf '#!/usr/bin/env bash\nexit 0\n' >"$build/bin/gateway"
printf '#!/usr/bin/env bash\nexit 0\n' >"$build/bin/gateway-admin"
chmod +x "$build/bin/gateway" "$build/bin/gateway-admin"

common=(--root "$root" --manifest "$root/package.toml" --lock "$tmp/resolved-runtime.lock.toml" --tools-lock "$tmp/resolved-tools.lock.toml" --dist-dir "$tmp/dist" --changelog "$root/CHANGELOG.md")

archive=$(python3 "$tool" "${common[@]}" package --package gateway --target x86_64-linux-musl --profile opt --build-dir "$build")
cp "$archive" "$tmp/first-package.tar.gz"
python3 "$tool" "${common[@]}" smoke --archive "$archive" --package gateway --target x86_64-linux-musl --execute
python3 "$tool" "${common[@]}" release-check --package gateway --target x86_64-linux-musl --tag packages/gateway/v1.4.0
python3 "$tool" "${common[@]}" release-notes --tag packages/gateway/v1.4.0 >"$tmp/notes"
grep -q "exact CPython runtime closure" "$tmp/notes"

archive2=$(python3 "$tool" "${common[@]}" package --package gateway --target x86_64-linux-musl --profile opt --build-dir "$build")
cmp "$tmp/first-package.tar.gz" "$archive2"

if python3 "$tool" "${common[@]}" release-check --package gateway --target x86_64-linux-musl --tag packages/gateway/v9.9.9 >/dev/null 2>&1; then
  echo "release-check unexpectedly accepted a mismatched tag" >&2
  exit 1
fi

sed '0,/loader_sha256 = "[a-f0-9]*"/s//loader_sha256 = "UNRESOLVED"/' \
  "$root/runtime-resolution.lock.toml" >"$tmp/unresolved-runtime.lock.toml"
if python3 "$tool" --root "$root" --manifest "$root/package.toml" --lock "$tmp/unresolved-runtime.lock.toml" --tools-lock "$root/tools.lock.toml" --dist-dir "$tmp/unresolved" package --package gateway --target x86_64-linux-musl --profile opt --build-dir "$build" >/dev/null 2>&1; then
  echo "package unexpectedly accepted unresolved runtime closure" >&2
  exit 1
fi

echo "package release tests passed"
