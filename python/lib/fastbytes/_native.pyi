# Typed surface of the fastbytes._native C extension (fastbytes.cc).
# Kept honest by python/test/test_stubs.py, which compares this stub's
# public names against the built module in-graph.

from collections.abc import Buffer

def xor_bytes(data: Buffer, key: int, /) -> bytes: ...
