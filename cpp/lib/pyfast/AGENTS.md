# pyfast - agent guide

Single-header pure-C library (`pyfast.h`) of macro scaffolding for
zero-overhead CPython C extensions: METH_FASTCALL / METH_FASTCALL|KEYWORDS,
PEP 590 vectorcall, Py_TPFLAGS_METHOD_DESCRIPTOR, and
`__attribute__((cleanup))` auto-release. `python/test:pyfast_test_extension`
is the small in-graph test fixture. Formerly `python/lib/vcall` (renamed and
relocated 2026-07-19).

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

## Defect history (all FIXED 2026-07-19)

Two independent gpt-5.6-sol passes converged on the same list;
best-of-both merged.

Five triaged by external review, four more found by compiling an exploratory
extension against the pinned toolchain:

1. `VA(i)` was unchecked `_a[i]` -> now `_va_checked()`, raises
   TypeError on OOB; folds away at -O2 after a matching `VEXPECT(n)`
   (proven via -S codegen inspection).
2. Reassigning a cleanup-managed variable leaked -> `VMOVE(name, expr)`
   (proven by sys.getallocatedblocks A/B: 90002 -> 2 over 300 calls).
3. Dead `Point_vc` / unarmed vc_call slot -> `VNEW_CALLABLE(T, vcfn)`
   plus `VCALL_SIG`/`VCALL_BEGIN` (rejects keywords rather than
   silently dropping them).
4. `VTYPE_IS(NULL, T)` NULL-deref -> NULL-safe, as is `_vtype_err`.
5. `VTYPE_CAST`/`VTYPE_GUARD` multi-eval -> single-bind statement
   exprs; `VTYPE_CAST` gained a `ctx` arg (was hardcoded "argument 0").
6. `VKWOPT_*` ended statement-exprs with an if/else STATEMENT -> whole
   macro was void-typed, every call site failed to compile. Ternaries.
7. `VMETHOD_SIG`/`VMETHOD_KW_SIG` self-parenthesized -> `fn((...))` is
   invalid C. Macros now carry no parens; call site supplies them.
8. Unpackers require `long*` for VINT output, not `int*`.
9. `PyUnicode_FromFormat` has NO %f/%g -> runtime SystemError; also
   naming tp_methods entries "__repr__"/"__str__" does NOT wire
   tp_repr/tp_str on a STATIC PyTypeObject (heap-type-only slot sync)
   - set `.tp_repr`/`.tp_str` directly.

`python/test:pyfast_test_extension` is a C-mode `py_extension` that consumes
this header through `cxx_deps`. Its top-level function tests run via
`py_tests()` and prove normal and error calls through the pinned interpreter.
Add new pyfast behavior there, keeping tests plain `test_*` functions.
