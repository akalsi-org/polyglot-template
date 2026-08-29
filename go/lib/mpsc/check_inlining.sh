#!/usr/bin/env bash
set -euo pipefail

package_dir=$(cd "$(dirname "$0")" && pwd)
repo_root=$(cd "$package_dir/../../.." && pwd)
output=$(cd "$repo_root" && CGO_ENABLED=0 "$repo_root/repo.sh" go test -run '^$' -gcflags='all=-m=2' ./go/lib/mpsc 2>&1)

require_inline() {
  local symbol=$1
  if ! grep -F "can inline $symbol" <<<"$output" >/dev/null; then
    printf 'missing inline result for %s\n' "$symbol" >&2
    exit 1
  fi
}

require_inline 'SpinWait'
require_inline 'loadAcquire32'
require_inline 'compareAndSwap32'
require_inline 'loadAcquire64'
require_inline 'loadRelaxed64'
require_inline 'storeRelaxed64'
require_inline 'storeRelease64'
require_inline 'compareAndSwap64'
require_inline 'compareAndSwapAcquire64'
require_inline 'fetchOrAcqRel64'
require_inline 'fetchAndRelease64'
require_inline 'fetchAddAcqRel32'
require_inline '(*MPSCProducer).Write'
require_inline '(*MPSCConsumer).Peek'
require_inline '(*MPSCConsumer).Pop'
require_inline '(*SPSCProducer).Write'
require_inline '(*SPSCConsumer).Peek'
require_inline '(*SPSCConsumer).Pop'
require_inline '(*mpscWriter).Reserve'
require_inline '(*mpscWriter).Commit'
require_inline '(*mpscWriter).Write'
require_inline '(*mpscReader).Peek'
require_inline '(*mpscReader).Pop'
require_inline '(*spscWriter).Write'
require_inline '(*spscReader).Peek'
