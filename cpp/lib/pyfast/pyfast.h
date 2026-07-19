/*
 * pyfast.h — Zero-overhead vectorcall scaffolding for CPython C extensions
 *
 * A pure-C header. Provides clean macros over:
 *   - METH_FASTCALL / METH_FASTCALL|METH_KEYWORDS  (zero-alloc dispatch)
 *   - PEP 590 vectorcall        (zero-alloc callable-type protocol)
 *   - Py_TPFLAGS_METHOD_DESCRIPTOR  (skip bound-method allocation)
 *   - __attribute__((cleanup))  (auto-release for buffers and refs)
 *
 * Properties:
 *   - Single header, no C++ dependency, no STL, no templates
 *   - Compiles in milliseconds
 *   - Zero allocations in function dispatch paths
 *   - No dispatch layer — your C function IS the dispatch
 *   - No link-time library dependency
 *   - Contextual error messages: "argument 0: expected int, got str"
 *   - Bounds-checked argument access: malformed call sites raise
 *     TypeError instead of reading past the argument vector
 *
 * Requires: CPython 3.8+, GCC or Clang (statement expressions, cleanup attr)
 *
 * Usage:
 *   #include "pyfast/pyfast.h"
 *
 * Naming: every macro keeps the original `V*` prefix (V for "vectorcall",
 * this header's organizing idea) rather than adopting `PYFAST_`/`PF_`. The
 * file/library identity is "pyfast"; the macro namespace is a separate
 * concern and a rename would touch every call site in every consumer for
 * no behavioral gain. See REPORT.md for the fuller rationale.
 */

#ifndef PYFAST_H
#define PYFAST_H

#ifndef PY_SSIZE_T_CLEAN
#define PY_SSIZE_T_CLEAN
#endif
#include <Python.h>
#include <stddef.h>

/* ── Contextual Type Errors ────────────────────────────────────────
 *
 * Every unpacker produces a TypeError with context:
 *
 *   VINT(0, &x)    →  "argument 0: expected int, got str"
 *   VKWUINT("n",…) →  "keyword 'n': expected int, got list"
 *
 * Context strings are built at compile time via string concatenation:
 *   "argument " #i      for positional arg i (must be an integer literal)
 *   "keyword '" k "'"   for keyword k (must be a string literal)
 *
 * Zero runtime cost — no snprintf, no static buffers, no __thread.
 *
 * `obj` may be NULL here (e.g. a failed/out-of-range VA(i)) — in that case
 * the message reports "got NULL" instead of dereferencing a null pointer
 * via Py_TYPE().
 */

static inline void
_vtype_err(const char *ctx, const char *expected, PyObject *obj)
{
  const char *got = obj ? Py_TYPE(obj)->tp_name : "NULL";
  PyErr_Format(PyExc_TypeError, "%s: expected %s, got %s",
               ctx, expected, got);
}

/* ── Module Functions ──────────────────────────────────────────────
 *
 * VFUNC(name)       positional-only  (METH_FASTCALL)
 * VFUNC_KW(name)    with keywords    (METH_FASTCALL | METH_KEYWORDS)
 *
 * Inside function body:
 *   VA(i)           positional arg i (0-indexed, PyObject*), bounds-checked
 *   VN              positional arg count (Py_ssize_t)
 *   VK              keyword names tuple, may be NULL (VFUNC_KW only)
 */

/* `_s` (the bound module/NULL) is deliberately part of the METH_FASTCALL
 * ABI signature but unused by the vast majority of module-level
 * functions (methods use it via VSELF; module functions never do) —
 * marked maybe-unused so -Wextra doesn't force every VFUNC body to
 * `(void)_s;` for a parameter it structurally can't omit. */
#define VFUNC(name) \
  static PyObject *name(PyObject *_s __attribute__((unused)), \
                        PyObject *const *_a, Py_ssize_t _n)

#define VFUNC_KW(name) \
  static PyObject *name(PyObject *_s __attribute__((unused)), \
                        PyObject *const *_a, \
                        Py_ssize_t _n, PyObject *_k)

/* ── Bounds-Checked Argument Access ──────────────────────────────────
 *
 * VA(i) used to be an unchecked `_a[i]`. Every unpacker in this header
 * (VINT, VSTR, VTYPE_CAST via raw VA() use, ...) trusted the caller to
 * have validated `_n` first (VEXPECT/VEXPECT_MIN); several demo call
 * sites didn't, which turned a bad Python-level call into an
 * out-of-bounds read instead of a TypeError.
 *
 * _va_checked() now range-checks `i` against `_n` and raises a TypeError
 * (rather than reading OOB) when the argument doesn't exist. Every
 * unpacker below (_v_long, _v_str, ...) treats a NULL `obj` as "error
 * already set by VA(), propagate" rather than re-reporting.
 *
 * Overhead: when VA(i) is preceded by VEXPECT(n) with the same literal
 * n, and i is a literal < n, GCC's value-range propagation folds the
 * `i >= _n` branch away entirely at -O2 once both are inlined (verified
 * via -S; see REPORT.md). Without a preceding VEXPECT, the check is a
 * single predicted-not-taken branch per access.
 */
static inline PyObject *
_va_checked(PyObject *const *a, Py_ssize_t n, Py_ssize_t i, const char *func)
{
  if (i < 0 || i >= n) {
    PyErr_Format(PyExc_TypeError,
                 "%s: missing required positional argument (index %zd, only %zd given)",
                 func, i, n);
    return NULL;
  }
  return a[i];
}

#define VA(i)  _va_checked(_a, _n, (i), __func__)
#define VN     _n
#define VK     _k

/* ── Return Helpers ──────────────────────────────────────────────── */

#define VRETURN_NONE        Py_RETURN_NONE
#define VRETURN_BOOL(v)     return PyBool_FromLong(v)
#define VRETURN_LONG(v)     return PyLong_FromLong(v)
#define VRETURN_UNSIGNED(v) return PyLong_FromUnsignedLong(v)
#define VRETURN_DOUBLE(v)   return PyFloat_FromDouble(v)
#define VRETURN_STR(s)      return PyUnicode_FromString(s)
#define VRETURN(obj)        return (obj)

/* ── Keyword Lookup (zero-alloc, O(n) linear scan) ──────────────── */

static inline PyObject *
_vkw(const char *key, PyObject *kwnames,
     PyObject *const *args, Py_ssize_t npos, int required)
{
  if (kwnames) {
    Py_ssize_t nkw = PyTuple_GET_SIZE(kwnames);
    for (Py_ssize_t i = 0; i < nkw; i++)
      if (!PyUnicode_CompareWithASCIIString(
              PyTuple_GET_ITEM(kwnames, i), key))
        return args[npos + i];
  }
  if (required)
    PyErr_Format(PyExc_TypeError,
                 "missing required keyword argument '%s'", key);
  return NULL;
}

#define VKW(k)     _vkw(k, _k, _a, _n, 1)
#define VKW_OPT(k) _vkw(k, _k, _a, _n, 0)

/* ── Argument Validation ─────────────────────────────────────────── */

#define VEXPECT(n) \
  do { \
    if (_n != (n)) { \
      PyErr_Format(PyExc_TypeError, \
          "%s: expected %d argument%s, got %zd", \
          __func__, (int)(n), (n) == 1 ? "" : "s", _n); \
      return NULL; \
    } \
  } while (0)

#define VEXPECT_MIN(n) \
  do { \
    if (_n < (n)) { \
      PyErr_Format(PyExc_TypeError, \
          "%s: expected at least %d argument%s, got %zd", \
          __func__, (int)(n), (n) == 1 ? "" : "s", _n); \
      return NULL; \
    } \
  } while (0)

/* ── Scalar Unpackers ──────────────────────────────────────────────
 *
 * VINT(i, &out)      long from arg i         → 1 on success, 0 on error
 * VUINT(i, &out)     unsigned long from arg i
 * VDOUBLE(i, &out)   double from arg i       (accepts int or float)
 * VBOOL(i, &out)     truth value from arg i   (any object)
 *
 * Error: "argument N: expected <type>, got <actual>"
 * The index i must be an integer literal for the compile-time message.
 *
 * Each of these accepts a NULL obj (VA(i) having already raised
 * TypeError for an out-of-range index) and fails without touching the
 * exception state a second time.
 */

static inline int
_v_long(PyObject *obj, long *out, const char *ctx)
{
  if (!obj) return 0;
  if (!PyLong_Check(obj)) {
    _vtype_err(ctx, "int", obj);
    return 0;
  }
  *out = PyLong_AsLong(obj);
  return !PyErr_Occurred();
}

static inline int
_v_ulong(PyObject *obj, unsigned long *out, const char *ctx)
{
  if (!obj) return 0;
  if (!PyLong_Check(obj)) {
    _vtype_err(ctx, "int", obj);
    return 0;
  }
  *out = PyLong_AsUnsignedLong(obj);
  return !PyErr_Occurred();
}

static inline int
_v_double(PyObject *obj, double *out, const char *ctx)
{
  if (!obj) return 0;
  if (!PyLong_Check(obj) && !PyFloat_Check(obj)) {
    _vtype_err(ctx, "float", obj);
    return 0;
  }
  *out = PyFloat_AsDouble(obj);
  return !PyErr_Occurred();
}

static inline int
_v_bool(PyObject *obj, int *out, const char *ctx)
{
  (void)ctx;
  if (!obj) return 0;
  *out = PyObject_IsTrue(obj);
  return *out >= 0;
}

#define VINT(i, out)    _v_long(VA(i), out, "argument " #i)
#define VUINT(i, out)   _v_ulong(VA(i), out, "argument " #i)
#define VDOUBLE(i, out) _v_double(VA(i), out, "argument " #i)
#define VBOOL(i, out)   _v_bool(VA(i), out, "argument " #i)

/* ── String / Bytes Accessors ──────────────────────────────────────
 *
 * VSTR(i, &len)   → const char*  (UTF-8, borrowed from Python str)
 * VBYTES(i, &len) → const char*  (raw bytes, exact type check)
 *
 * len may be NULL.  Returned pointer is valid while the Python
 * object is alive.  NULL on error (exception set).
 */

static inline const char *
_v_str(PyObject *obj, Py_ssize_t *out_len, const char *ctx)
{
  if (!obj) return NULL;
  if (!PyUnicode_Check(obj)) {
    _vtype_err(ctx, "str", obj);
    return NULL;
  }
  return PyUnicode_AsUTF8AndSize(obj, out_len);
}

static inline const char *
_v_bytes(PyObject *obj, Py_ssize_t *out_len, const char *ctx)
{
  if (!obj) return NULL;
  if (!PyBytes_Check(obj)) {
    _vtype_err(ctx, "bytes", obj);
    return NULL;
  }
  if (out_len) *out_len = PyBytes_GET_SIZE(obj);
  return PyBytes_AS_STRING(obj);
}

#define VSTR(i, plen)   _v_str(VA(i), plen, "argument " #i)
#define VBYTES(i, plen) _v_bytes(VA(i), plen, "argument " #i)

/* ── Buffer Access ─────────────────────────────────────────────────
 *
 * VBUFFER(i, &buf)    acquire readonly buffer from arg i
 * VBUF_DONE(&buf)     release and NULL-out (safe to call twice)
 *
 * VBUFFER clears CPython's generic buffer error and replaces it
 * with a contextual TypeError ("argument N: expected bytes-like, got …").
 * This is intentional: the "which argument" context is more actionable.
 */

static inline int
_v_buffer(PyObject *obj, Py_buffer *buf, const char *ctx)
{
  if (!obj) return 0;
  if (PyObject_GetBuffer(obj, buf, PyBUF_SIMPLE) == -1) {
    PyErr_Clear();
    _vtype_err(ctx, "bytes-like", obj);
    return 0;
  }
  return 1;
}

#define VBUFFER(i, bufptr) _v_buffer(VA(i), bufptr, "argument " #i)
#define VBUF_DONE(bufptr)  do { PyBuffer_Release(bufptr); (bufptr)->obj = NULL; } while(0)

#define VNUL(i) (VA(i) == Py_None)

/* ── Type Cast ───────────────────────────────────────────────────── */

/* ctx names the value being cast in the error message ("argument 2",
 * "keyword 'point'", ...) — a hardcoded "argument 0" here was simply
 * wrong for every cast of anything else. */
#define VTYPE_CAST(obj, T, ctx) \
  (__extension__({ \
     PyObject *_vc_o = (PyObject *)(obj); \
     (_vc_o && Py_TYPE(_vc_o) == &T##_type) \
       ? (T *)_vc_o \
       : (_vtype_err((ctx), #T, _vc_o), (T *)NULL); \
   }))

/* ── Keyword Unpacker Shortcuts ────────────────────────────────────
 *
 * Every positional unpacker has a required-keyword and optional-keyword
 * counterpart.  Keyword context is built at compile time:
 *   "keyword 'name': expected <type>, got <actual>"
 *
 *   Positional          Required keyword         Optional keyword
 *   ──────────────────  ───────────────────────  ────────────────────────
 *   VINT(i,&out)        VKWINT(k,&out)            VKWOPT_INT(k,&out,def)
 *   VUINT(i,&out)       VKWUINT(k,&out)           VKWOPT_UINT(k,&out,def)
 *   VDOUBLE(i,&out)     VKWDOUBLE(k,&out)         VKWOPT_DOUBLE(k,&out,def)
 *   VBOOL(i,&out)       VKWBOOL(k,&out)           VKWOPT_BOOL(k,&out,def)
 *   VSTR(i,&len)        VKWSTR(k,&len)            VKWOPT_STR(k,&ptr,&len)
 *   VBYTES(i,&len)      VKWBYTES(k,&len)          VKWOPT_BYTES(k,&ptr,&len)
 *   VBUFFER(i,&buf)     VKWBUFFER(k,&buf)         (use VKW_OPT+VBUFFER)
 *
 * Scalar required variants return 1/0.  Optional variants return 1
 * always (using the default when keyword is absent); 0 only on
 * conversion error.
 *
 * String/bytes required variants return const char* (NULL on error).
 * Optional variants write NULL to *ptr when keyword absent, return 1/0.
 *
 * The keyword name k must be a string literal.
 */

#define _VKW_CTX(k) "keyword '" k "'"

#define VKWINT(k, out) \
  ({ PyObject *_v = VKW(k); \
     _v ? _v_long(_v, out, _VKW_CTX(k)) : 0; })

#define VKWUINT(k, out) \
  ({ PyObject *_v = VKW(k); \
     _v ? _v_ulong(_v, out, _VKW_CTX(k)) : 0; })

#define VKWDOUBLE(k, out) \
  ({ PyObject *_v = VKW(k); \
     _v ? _v_double(_v, out, _VKW_CTX(k)) : 0; })

#define VKWBOOL(k, out) \
  ({ PyObject *_v = VKW(k); \
     _v ? _v_bool(_v, out, _VKW_CTX(k)) : 0; })

#define VKWSTR(k, out_len) \
  ({ PyObject *_v = VKW(k); \
     _v ? _v_str(_v, out_len, _VKW_CTX(k)) : (const char *)NULL; })

#define VKWBYTES(k, out_len) \
  ({ PyObject *_v = VKW(k); \
     _v ? _v_bytes(_v, out_len, _VKW_CTX(k)) : (const char *)NULL; })

#define VKWBUFFER(k, bufptr) \
  ({ PyObject *_v = VKW(k); \
     _v ? _v_buffer(_v, bufptr, _VKW_CTX(k)) : 0; })

/*
 * These used to end their GNU statement-expression with an `if (...) {...}
 * else ...;` STATEMENT. Per the GNU extension's own rule ("if you use some
 * other kind of statement last within the braces, the construct has type
 * void"), that made every VKWOPT_* macro evaluate to void — a previously
 * undocumented defect that fails to compile the moment a caller writes
 * `if (!VKWOPT_INT(...))`, which is the only way these macros are ever
 * used (vcall_greet, vcall_prefix_join, vcall_repeat in demo.c all did
 * this — the header as shipped did not compile against its own demo).
 * Rewritten so the last thing in the braces is a ternary expression.
 */

#define VKWOPT_INT(k, out, def) \
  ({ PyObject *_v = VKW_OPT(k); \
     _v ? _v_long(_v, out, _VKW_CTX(k)) : (*(out) = (def), 1); })

#define VKWOPT_UINT(k, out, def) \
  ({ PyObject *_v = VKW_OPT(k); \
     _v ? _v_ulong(_v, out, _VKW_CTX(k)) : (*(out) = (def), 1); })

#define VKWOPT_DOUBLE(k, out, def) \
  ({ PyObject *_v = VKW_OPT(k); \
     _v ? _v_double(_v, out, _VKW_CTX(k)) : (*(out) = (def), 1); })

#define VKWOPT_BOOL(k, out, def) \
  ({ PyObject *_v = VKW_OPT(k); \
     _v ? _v_bool(_v, out, _VKW_CTX(k)) : (*(out) = (def), 1); })

#define VKWOPT_STR(k, out_ptr, out_len) \
  ({ PyObject *_v = VKW_OPT(k); \
     _v ? ((*(out_ptr) = _v_str(_v, out_len, _VKW_CTX(k))) != NULL) \
         : (*(out_ptr) = NULL, 1); })

#define VKWOPT_BYTES(k, out_ptr, out_len) \
  ({ PyObject *_v = VKW_OPT(k); \
     _v ? ((*(out_ptr) = _v_bytes(_v, out_len, _VKW_CTX(k))) != NULL) \
         : (*(out_ptr) = NULL, 1); })

/* ── Module Definition ─────────────────────────────────────────────
 *
 *   VMOD_BEGIN
 *     VMOD_FUNC("name", fn, "doc"),
 *     VMOD_FUNC_KW("name", fn, "doc"),
 *   VMOD_OBJ(module_name, "Module doc string.");
 *   VMOD_INIT(module_name)          // only if no types
 *
 * For modules with types, write your own PyInit_* that calls
 * PyModule_Create(&_vmd) + VTYPE_READY(T, m) for each type.
 */

#define VMOD_BEGIN \
  static PyMethodDef _vmt[] = {

#define VMOD_FUNC(name, fn, doc) \
  {name, (PyCFunction)(void(*)(void))fn, METH_FASTCALL, doc}

#define VMOD_FUNC_KW(name, fn, doc) \
  {name, (PyCFunction)(void(*)(void))fn, METH_FASTCALL | METH_KEYWORDS, doc}

#define VMOD_OBJ(mod, doc) \
  {NULL, NULL, 0, NULL} }; \
  static struct PyModuleDef _vmd = { \
    PyModuleDef_HEAD_INIT, #mod, doc, -1, _vmt, \
    NULL, NULL, NULL, NULL \
  };

#define VMOD_INIT(mod) \
  PyMODINIT_FUNC PyInit_##mod(void) { return PyModule_Create(&_vmd); }

/* ── Types with Vectorcall ─────────────────────────────────────────
 *
 *   VTYPE_HEAD(Point)
 *     double x, y;
 *   VTYPE_END(Point);
 *
 *   VTOBJ_DEF(Point,
 *     .tp_name = "mod.Point",
 *     .tp_flags = VTYPE_FLAGS,    // REQUIRED — see below
 *     .tp_methods = Point_methods,
 *     .tp_new = Point_new,
 *     .tp_dealloc = Point_dealloc,
 *     .tp_call = PyVectorcall_Call,
 *   );
 *
 *   VTYPE_READY(Point, m);
 *
 * You MUST set .tp_flags = VTYPE_FLAGS in VTOBJ_DEF.  C designated
 * initializers take the last value for a field, so the macro cannot
 * set it for you — your __VA_ARGS__ would silently override it.
 *
 * PEP 590 instance vectorcall — VTYPE_FLAGS + tp_vectorcall_offset (set
 * automatically by VTOBJ_DEF, pointing at the vc_call field VTYPE_HEAD
 * adds) makes INSTANCES of T callable objects when `.tp_call =
 * PyVectorcall_Call` is present *and* every live instance's vc_call slot
 * holds a valid vectorcallfunc pointer before it is ever called.
 * PyVectorcall_Call reads that slot unconditionally; a zero-initialized
 * (freshly tp_alloc'd) instance whose vc_call was never assigned is a
 * NULL function pointer waiting to be invoked. VNEW_CALLABLE below is
 * the safe constructor for such types — plain VNEW() is for types that
 * don't use tp_call/vc_call at all.
 *
 * This is instance-level vectorcall (T() objects becoming callable),
 * not "make the type itself construct via vectorcall" — that second,
 * different feature is wired through the metatype's own tp_vectorcall
 * slot inside CPython's typeobject.c and has no public C-API hook for
 * extension authors, regardless of flags set here.  Construction of a
 * VTYPE_FLAGS type still goes through the normal tp_new/tp_init path
 * (see VMETHOD_SIG-style tp_new below); vc_call is strictly about what
 * happens when an already-constructed instance is *called*.
 */

#define VTYPE_HEAD(T) \
  typedef struct { PyObject_HEAD vectorcallfunc vc_call;

#define VTYPE_END(T) } T;

#define VTYPE_FLAGS \
  (Py_TPFLAGS_DEFAULT | Py_TPFLAGS_HAVE_VECTORCALL | Py_TPFLAGS_METHOD_DESCRIPTOR)

#define VTOBJ_DEF(T, ...) \
  static PyTypeObject T##_type = { \
    PyVarObject_HEAD_INIT(NULL, 0) \
    .tp_basicsize = sizeof(T), \
    .tp_itemsize = 0, \
    .tp_vectorcall_offset = offsetof(T, vc_call), \
    __VA_ARGS__ };

#define VTYPE_READY(T, mod) \
  do { \
    if (PyType_Ready(&T##_type) < 0) return NULL; \
    Py_INCREF(&T##_type); \
    if (PyModule_AddObject(mod, #T, (PyObject *)&T##_type) < 0) { \
      Py_DECREF(&T##_type); \
      return NULL; \
    } \
  } while (0)

#define VNEW(T) ((T *)T##_type.tp_alloc(&T##_type, 0))

/* VNEW_CALLABLE(T, vcfn): allocate + immediately arm vc_call.  Use this
 * (never plain VNEW) for any type whose tp_call is PyVectorcall_Call —
 * it is the single point that keeps "flags say vectorcall-capable" and
 * "vc_call actually points at a function" from drifting apart. */
#define VNEW_CALLABLE(T, vcfn) \
  (__extension__({ \
     T *_vn_self = VNEW(T); \
     if (_vn_self) _vn_self->vc_call = (vcfn); \
     _vn_self; \
   }))

/* ── Vectorcall-Slot Bodies (for T's own vc_call, i.e. `t(...)`) ────
 *
 *   static PyObject *Point_call(VCALL_SIG) {
 *     VCALL_BEGIN;
 *     VSELF(Point);
 *     ...  // VA(i)/VN/VEXPECT all work here, same as VFUNC
 *   }
 *
 * This is the signature PEP 590 requires for a vectorcallfunc
 * (`nargsf` is a size_t with the possible PY_VECTORCALL_ARGUMENTS_OFFSET
 * bit set — never compare/pass it directly as a count). VCALL_BEGIN
 * unpacks it into the same `_n` name VA/VN/VEXPECT already key off of.
 */

/* No enclosing parens in the replacement text: VCALL_SIG (like
 * VMETHOD_SIG below) is used as `fn(VCALL_SIG)`, and the call site
 * already supplies the parens. An object-like macro whose own
 * replacement text ALSO includes `(...)` doubly parenthesizes the
 * parameter list — `fn((PyObject *_s, ...))` — which is not a valid
 * function definition. This was a real, previously undiscovered defect:
 * VMETHOD_SIG/VMETHOD_KW_SIG had self-enclosing parens, so every Point
 * method (dist/scale/move/repr/str) failed to compile. */
#define VCALL_SIG \
  PyObject *_s, PyObject *const *_a __attribute__((unused)), \
  size_t _nf, PyObject *_k __attribute__((unused))

/* VCALL_BEGIN rejects keyword arguments outright: a vc_call body that
 * silently ignored a non-empty kwnames tuple would drop `p(x, dy=...)`
 * keywords on the floor. The rare keyword-accepting vc_call should
 * hand-roll its prologue from PyVectorcall_NARGS + _k instead. */
#define VCALL_BEGIN \
  Py_ssize_t _n = PyVectorcall_NARGS(_nf); \
  do { \
    if (_k != NULL && PyTuple_GET_SIZE(_k) > 0) { \
      PyErr_Format(PyExc_TypeError, \
                   "%s: takes no keyword arguments", __func__); \
      return NULL; \
    } \
  } while (0)

/* ── Type Methods (METH_FASTCALL on tp_methods) ────────────────────
 *
 *   static PyObject *Point_dist(VMETHOD_SIG) {
 *     VSELF(Point);
 *     VRETURN_DOUBLE(sqrt(self->x * self->x + self->y * self->y));
 *   }
 *
 *   static PyMethodDef Point_methods[] = {
 *     VMETH_ENTRY("dist", Point, dist, "doc"),
 *     VMETH_KW_ENTRY("move", Point, move, "doc"),
 *     {NULL, NULL, 0, NULL}
 *   };
 */

/* See VCALL_SIG's comment above: no enclosing parens — the call site
 * `Point_dist(VMETHOD_SIG)` supplies them. This is the fix for the
 * previously undiscovered defect that made every VMETHOD_SIG/
 * VMETHOD_KW_SIG-declared function fail to compile (doubly
 * parenthesized parameter list). */
/* _a/_n unused-marked too: no-argument methods (dist/repr/str-style)
 * legitimately never touch them, and forcing `(void)_a; (void)_n;`
 * into every such body for an ABI-mandated parameter is friction the
 * macro should absorb instead. */
#define VMETHOD_SIG \
  PyObject *_s, PyObject *const *_a __attribute__((unused)), \
  Py_ssize_t _n __attribute__((unused))

#define VMETHOD_KW_SIG \
  PyObject *_s, PyObject *const *_a __attribute__((unused)), \
  Py_ssize_t _n __attribute__((unused)), PyObject *_k __attribute__((unused))

#define VMETH_ENTRY(name, T, fn, doc) \
  {name, (PyCFunction)(void(*)(void))T##_##fn, METH_FASTCALL, doc}

#define VMETH_KW_ENTRY(name, T, fn, doc) \
  {name, (PyCFunction)(void(*)(void))T##_##fn, METH_FASTCALL | METH_KEYWORDS, doc}

/* ── Type Checks ─────────────────────────────────────────────────── */

#define VTYPE_IS(obj, T) \
  (__extension__({ \
     PyObject *_ti_o = (PyObject *)(obj); \
     _ti_o && Py_TYPE(_ti_o) == &T##_type; \
   }))

#define VTYPE_GUARD(obj, T, arg) \
  do { \
    PyObject *_tg_o = (PyObject *)(obj); \
    if (!(_tg_o && Py_TYPE(_tg_o) == &T##_type)) { \
      _vtype_err(arg, #T, _tg_o); \
      return NULL; \
    } \
  } while (0)

/* ── Method Self-Cast ────────────────────────────────────────────── */

#define VSELF(T) T *self = (T *)_s

/* ── Batch Validation ──────────────────────────────────────────────
 *
 * VALL(expr)   return NULL if expr is falsy.
 * Short-circuits on first failure:
 *   VALL(VINT(0, &x) && VINT(1, &y) && VDOUBLE(2, &z));
 */

#define VALL(expr) do { if (!(expr)) return NULL; } while(0)

/* ── Auto-Cleanup (GCC __attribute__((cleanup))) ───────────────────
 *
 * VBUF_SCOPED(name)        Py_buffer, auto-release on scope exit
 * VREF_SCOPED(name)        PyObject*, auto-XDECREF on exit (NULL-init)
 * VREF_AUTO(name, expr)    PyObject*, auto-XDECREF on exit (expr-init)
 * VSTEAL(name)             return name and prevent auto-cleanup
 * VMOVE(name, expr)        reassign a cleanup-managed ref, releasing
 *                          whatever it held before
 *
 * VBUF_SCOPED + VBUF_DONE is safe: VBUF_DONE NULLs buf->obj,
 * so the cleanup handler skips the double-release.
 *
 * VSTEAL sets the variable to NULL (so the cleanup handler skips it)
 * and returns the saved pointer.  Only use with return:
 *   return VSTEAL(result);
 *
 * A cleanup-managed variable is scope-owned: `result = expr;` clobbers
 * the pointer without releasing whatever `result` held (a leak — this
 * is exactly what naively doing `result = VSTEAL(joined)` inside a loop
 * does on every iteration but the last). VMOVE releases the old value
 * first, then installs the new one, so it's always the correct spelling
 * of "replace what this cleanup-managed variable points to":
 *   VMOVE(result, VSTEAL(joined));
 *
 * No object pool is provided — CPython's pymalloc already optimizes
 * small-object allocation, and PyLong caches -5..256.  A custom pool
 * would add complexity without measurable benefit.
 */

static inline void _vbuf_cleanup(Py_buffer *buf) {
  if (buf->obj) PyBuffer_Release(buf);
}

static inline void _vdecref_cleanup(PyObject **obj) {
  Py_XDECREF(*obj);
}

#define VBUF_SCOPED(name) \
  Py_buffer name __attribute__((cleanup(_vbuf_cleanup))) = {0}

#define VREF_SCOPED(name) \
  PyObject *name __attribute__((cleanup(_vdecref_cleanup))) = NULL

#define VREF_AUTO(name, expr) \
  PyObject *name __attribute__((cleanup(_vdecref_cleanup))) = (expr)

#define VSTEAL(name) ({ PyObject *_t = (name); (name) = NULL; _t; })

#define VMOVE(name, expr) \
  do { PyObject *_vm_new = (expr); Py_XDECREF(name); (name) = _vm_new; } while (0)

/* ── Construction Helpers ──────────────────────────────────────────
 *
 * Thin wrappers around CPython constructors.  Kept as macros so they
 * compose naturally with VREF_AUTO:
 *
 *   VREF_AUTO(result, VNEW_LONG(42));
 *   if (!result) return NULL;
 *   ...
 *   return VSTEAL(result);
 */

#define VNEW_LONG(v)       PyLong_FromLong(v)
#define VNEW_UNSIGNED(v)   PyLong_FromUnsignedLong(v)
#define VNEW_DOUBLE(v)     PyFloat_FromDouble(v)
#define VNEW_BOOL(v)       PyBool_FromLong(v)
#define VNEW_STR(s)        PyUnicode_FromString(s)
#define VNEW_STRN(s, n)    PyUnicode_FromStringAndSize((s), (n))
#define VNEW_BYTES(s, n)   PyBytes_FromStringAndSize((s), (n))
#define VNEW_EMPTY_BYTES() PyBytes_FromStringAndSize(NULL, 0)

#endif /* PYFAST_H */
