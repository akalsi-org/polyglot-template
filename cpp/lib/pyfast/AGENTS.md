# pyfast - agent guide

Single-header pure-C library (`pyfast.h`) of macro scaffolding for
zero-overhead CPython C extensions: METH_FASTCALL / METH_FASTCALL|KEYWORDS,
PEP 590 vectorcall, Py_TPFLAGS_METHOD_DESCRIPTOR, and
`__attribute__((cleanup))` auto-release. `demo.c` is a demonstration
extension module and the intended test fixture. Formerly `python/lib/vcall`
(renamed and relocated 2026-07-19).

## Ground rules

- Pure C, no C++/STL. Requires the repo's pinned GCC (statement
  expressions, cleanup attribute) and pinned CPython 3.14 headers at
  `.local/toolchain/<triple>/python-*/python/include/python3.14/`.
- Verify CPython C-API claims against those pinned headers, not memory or
  web docs (e.g. `Py_TPFLAGS_HAVE_VECTORCALL` EXISTS in 3.14 at
  object.h:553 - a review claim that it was removed has been refuted).
- Buck2 target: `//cpp/lib/pyfast:pyfast` (header-only cxx_library;
  include as `"pyfast/pyfast.h"` via the cpp/lib logical include root).
- Two-space indent. Never run `buck2 clean`/`kill`/`killall`.

## Known defects (triaged external review, 2026-07-19)

1. CRITICAL - confirmed: `VA(i)` is unchecked `_a[i]`; `VINT(0, ...)`
   without a prior `VEXPECT(n)` reads out of bounds (demo's vcall_add,
   vcall_range, Point_scale all do this). Prefer making accessors
   range-check against `_n` over relying on caller discipline.
2. CRITICAL - credible: reassigning a cleanup-managed variable
   (`result = VSTEAL(joined)` in demo) leaks the old reference.
3. CRITICAL - credible: dead `Point_vc` constructor; `tp_call =
   PyVectorcall_Call` without proper `Py_TPFLAGS_HAVE_VECTORCALL` +
   `tp_vectorcall` setup on the type.
4. MAJOR: `VTYPE_IS(NULL, T)` dereferences NULL via `Py_TYPE`.
5. MAJOR: `VTYPE_CAST`/`VTYPE_GUARD` multi-evaluate their argument.

## Wiring gaps (build machinery, separate from header work)

- rules/python.bzl `py_extension` compiles C++ only; needs a C mode
  (gcc, C std flags) before demo.c can build as an extension.
- `py_extension` does not yet consume cxx_library include-tree deps
  (rules/cxx.bzl IncludeTreeSet) - needed to depend on `:pyfast`.

## Test plan (when wired)

1. Build demo as py_extension in dbg AND opt (macro bugs surface at -O2).
2. Python unittest importing the built demo: every macro family,
   positional/keyword paths, the error-message contract ("argument 0:
   expected int, got str"), exception state after failures, refcount
   neutrality via sys.getrefcount deltas.
3. Lint-labeled strict-compile probe (-Wall -Werror) of demo.c.
