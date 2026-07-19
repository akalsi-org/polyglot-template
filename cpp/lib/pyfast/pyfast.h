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
 *
 * Requires: CPython 3.8+, GCC or Clang (statement expressions, cleanup attr)
 *
 * Usage:
 *   #include "pyfast/pyfast.h"
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
 */

static inline void
_vtype_err(const char *ctx, const char *expected, PyObject *obj)
{
  PyErr_Format(PyExc_TypeError, "%s: expected %s, got %s",
               ctx, expected, Py_TYPE(obj)->tp_name);
}

/* ── Module Functions ──────────────────────────────────────────────
 *
 * VFUNC(name)       positional-only  (METH_FASTCALL)
 * VFUNC_KW(name)    with keywords    (METH_FASTCALL | METH_KEYWORDS)
 *
 * Inside function body:
 *   VA(i)           positional arg i (0-indexed, PyObject*)
 *   VN              positional arg count (Py_ssize_t)
 *   VK              keyword names tuple, may be NULL (VFUNC_KW only)
 */

#define VFUNC(name) \
  static PyObject *name(PyObject *_s, PyObject *const *_a, Py_ssize_t _n)

#define VFUNC_KW(name) \
  static PyObject *name(PyObject *_s, PyObject *const *_a, \
                        Py_ssize_t _n, PyObject *_k)

#define VA(i)  _a[i]
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
 */

static inline int
_v_long(PyObject *obj, long *out, const char *ctx)
{
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
  if (!PyUnicode_Check(obj)) {
    _vtype_err(ctx, "str", obj);
    return NULL;
  }
  return PyUnicode_AsUTF8AndSize(obj, out_len);
}

static inline const char *
_v_bytes(PyObject *obj, Py_ssize_t *out_len, const char *ctx)
{
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

#define VTYPE_CAST(obj, T) \
  (VTYPE_IS(obj, T) \
   ? (T *)(obj) \
   : (_vtype_err("argument 0", #T, (obj)), (T *)NULL))

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

#define VKWOPT_INT(k, out, def) \
  ({ PyObject *_v = VKW_OPT(k); \
     if (!_v) { *(out) = (def); 1; } \
     else _v_long(_v, out, _VKW_CTX(k)); })

#define VKWOPT_UINT(k, out, def) \
  ({ PyObject *_v = VKW_OPT(k); \
     if (!_v) { *(out) = (def); 1; } \
     else _v_ulong(_v, out, _VKW_CTX(k)); })

#define VKWOPT_DOUBLE(k, out, def) \
  ({ PyObject *_v = VKW_OPT(k); \
     if (!_v) { *(out) = (def); 1; } \
     else _v_double(_v, out, _VKW_CTX(k)); })

#define VKWOPT_BOOL(k, out, def) \
  ({ PyObject *_v = VKW_OPT(k); \
     if (!_v) { *(out) = (def); 1; } \
     else _v_bool(_v, out, _VKW_CTX(k)); })

#define VKWOPT_STR(k, out_ptr, out_len) \
  ({ PyObject *_v = VKW_OPT(k); \
     if (!_v) { *(out_ptr) = NULL; 1; } \
     else { const char *_r = _v_str(_v, out_len, _VKW_CTX(k)); \
            *(out_ptr) = _r; _r != NULL; } })

#define VKWOPT_BYTES(k, out_ptr, out_len) \
  ({ PyObject *_v = VKW_OPT(k); \
     if (!_v) { *(out_ptr) = NULL; 1; } \
     else { const char *_r = _v_bytes(_v, out_len, _VKW_CTX(k)); \
            *(out_ptr) = _r; _r != NULL; } })

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
 *     .tp_dealloc = Point_dealloc,
 *     .tp_call = PyVectorcall_Call,
 *   );
 *
 *   Point *p = VNEW(Point);
 *   VTYPE_READY(Point, m);
 *
 * You MUST set .tp_flags = VTYPE_FLAGS in VTOBJ_DEF.  C designated
 * initializers take the last value for a field, so the macro cannot
 * set it for you — your __VA_ARGS__ would silently override it.
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

#define VMETHOD_SIG \
  (PyObject *_s, PyObject *const *_a, Py_ssize_t _n)

#define VMETHOD_KW_SIG \
  (PyObject *_s, PyObject *const *_a, Py_ssize_t _n, PyObject *_k)

#define VMETH_ENTRY(name, T, fn, doc) \
  {name, (PyCFunction)(void(*)(void))T##_##fn, METH_FASTCALL, doc}

#define VMETH_KW_ENTRY(name, T, fn, doc) \
  {name, (PyCFunction)(void(*)(void))T##_##fn, METH_FASTCALL | METH_KEYWORDS, doc}

/* ── Type Checks ─────────────────────────────────────────────────── */

#define VTYPE_IS(obj, T) (Py_TYPE(obj) == &T##_type)

#define VTYPE_GUARD(obj, T, arg) \
  do { \
    if (!VTYPE_IS(obj, T)) { \
      _vtype_err(arg, #T, (obj)); \
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
 *
 * VBUF_SCOPED + VBUF_DONE is safe: VBUF_DONE NULLs buf->obj,
 * so the cleanup handler skips the double-release.
 *
 * VSTEAL sets the variable to NULL (so the cleanup handler skips it)
 * and returns the saved pointer.  Only use with return:
 *   return VSTEAL(result);
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

#endif /* VCALL_H */
