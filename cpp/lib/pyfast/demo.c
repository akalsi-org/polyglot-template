#include "pyfast/pyfast.h"

#include <math.h>
#include <string.h>

/* ═══════════════════════════════════════════════════════════════════════════
   Module-level functions
   ═══════════════════════════════════════════════════════════════════════════ */

/* Previously: no VEXPECT, so VINT(0,..)/VINT(1,..) read _a[0]/_a[1]
 * unconditionally — calling add() or add(1) from Python read past the
 * (possibly zero-length) argument vector. VA(i) itself is now
 * bounds-checked as defense-in-depth, but the correct fix at the call
 * site is still to state the arity up front. */
VFUNC(vcall_add) {
  VEXPECT(2);
  long x, y;
  VALL(VINT(0, &x) && VINT(1, &y));
  VRETURN_LONG(x + y);
}

VFUNC_KW(vcall_xor) {
  const char *src; Py_ssize_t len;
  unsigned long key;
  VEXPECT(1);
  if (!(src = VBYTES(0, &len))) return NULL;
  if (!VKWUINT("key", &key)) return NULL;
  if (key > 0xffU) {
    PyErr_SetString(PyExc_ValueError, "key must fit in one byte");
    return NULL;
  }
  PyObject *out = VNEW_BYTES(NULL, len);
  if (!out) return NULL;
  unsigned char *dst = (unsigned char *)PyBytes_AS_STRING(out);
  for (Py_ssize_t i = 0; i < len; i++)
    dst[i] = (unsigned char)(src[i] ^ (unsigned char)key);
  return out;
}

/* Keyword-only bytes: xor_kwb(*, data, key) */
VFUNC_KW(vcall_xor_kwb) {
  const char *src; Py_ssize_t len;
  unsigned long key;
  if (!(src = VKWBYTES("data", &len))) return NULL;
  if (!VKWUINT("key", &key)) return NULL;
  if (key > 0xffU) {
    PyErr_SetString(PyExc_ValueError, "key must fit in one byte");
    return NULL;
  }
  PyObject *out = VNEW_BYTES(NULL, len);
  if (!out) return NULL;
  unsigned char *dst = (unsigned char *)PyBytes_AS_STRING(out);
  for (Py_ssize_t i = 0; i < len; i++)
    dst[i] = (unsigned char)(src[i] ^ (unsigned char)key);
  return out;
}

/* VKWBUFFER + VBUF_SCOPED — keyword-only buffer, auto-cleanup */
VFUNC_KW(vcall_xor_kwb_buf) {
  unsigned long key;
  VBUF_SCOPED(buf);
  if (!VKWBUFFER("data", &buf)) return NULL;
  if (!VKWUINT("key", &key)) return NULL;
  if (key > 0xffU) {
    PyErr_SetString(PyExc_ValueError, "key must fit in one byte");
    return NULL;
  }
  PyObject *out = VNEW_BYTES(NULL, buf.len);
  if (!out) return NULL;
  unsigned char *dst = (unsigned char *)PyBytes_AS_STRING(out);
  const unsigned char *src = (const unsigned char *)buf.buf;
  for (Py_ssize_t i = 0; i < buf.len; i++)
    dst[i] = (unsigned char)(src[i] ^ key);
  return out;
}

VFUNC_KW(vcall_greet) {
  const char *name; Py_ssize_t name_len;
  int excited;
  if (!(name = VKWSTR("name", &name_len))) return NULL;
  if (!VKWOPT_BOOL("excited", &excited, 0)) return NULL;
  if (excited)
    return PyUnicode_FromFormat("Hello, %.*s!!", (int)name_len, name);
  return PyUnicode_FromFormat("Hello, %.*s.", (int)name_len, name);
}

/* Exercises VMOVE: each loop iteration used to do `result =
 * VSTEAL(joined);`, which clobbers the cleanup-managed `result` pointer
 * without releasing the reference it held from the previous iteration —
 * a leak on every iteration but the last. VMOVE(result, VSTEAL(joined))
 * DECREFs the old `result` first. */
VFUNC_KW(vcall_prefix_join) {
  PyObject *items;
  const char *prefix; Py_ssize_t prefix_len;
  VEXPECT(1);
  items = VA(0);
  if (!PyList_Check(items)) {
    _vtype_err("argument 0", "list", items);
    return NULL;
  }
  if (!VKWOPT_STR("prefix", &prefix, &prefix_len)) return NULL;
  Py_ssize_t n = PyList_GET_SIZE(items);
  VREF_AUTO(result, VNEW_STR(prefix ? prefix : ""));
  if (!result) return NULL;
  for (Py_ssize_t i = 0; i < n; i++) {
    VREF_AUTO(item_str, PyObject_Str(PyList_GET_ITEM(items, i)));
    if (!item_str) return NULL;
    VREF_AUTO(joined, PyUnicode_Concat(result, item_str));
    if (!joined) return NULL;
    VMOVE(result, VSTEAL(joined));
  }
  return VSTEAL(result);
}

VFUNC_KW(vcall_xor_buffer) {
  unsigned long key;
  VEXPECT(1);
  VBUF_SCOPED(buf);
  if (!VBUFFER(0, &buf)) return NULL;
  if (!VKWUINT("key", &key)) return NULL;
  if (key > 0xffU) {
    PyErr_SetString(PyExc_ValueError, "key must fit in one byte");
    return NULL;
  }
  PyObject *out = VNEW_BYTES(NULL, buf.len);
  if (!out) return NULL;
  unsigned char *dst = (unsigned char *)PyBytes_AS_STRING(out);
  const unsigned char *src = (const unsigned char *)buf.buf;
  for (Py_ssize_t i = 0; i < buf.len; i++)
    dst[i] = (unsigned char)(src[i] ^ key);
  return out;
}

/* times is `long`, not `int`: VKWOPT_INT (like VINT) always writes
 * through a `long *` — passing an `int *` here used to be a silent
 * pointer-type mismatch that only -Wincompatible-pointer-types caught. */
VFUNC_KW(vcall_repeat) {
  const char *src; Py_ssize_t len;
  long times;
  VEXPECT(1);
  if (!(src = VBYTES(0, &len))) return NULL;
  if (!VKWOPT_INT("times", &times, 1)) return NULL;
  PyObject *out = VNEW_BYTES(NULL, len * times);
  if (!out) return NULL;
  char *dst = PyBytes_AS_STRING(out);
  for (long t = 0; t < times; t++)
    memcpy(dst + (Py_ssize_t)t * len, src, len);
  return out;
}

/* Previously: no VEXPECT, so VINT(0,..) read _a[0] unconditionally —
 * range() with zero arguments read past the argument vector. */
VFUNC(vcall_range) {
  VEXPECT(1);
  long stop;
  if (!VINT(0, &stop)) return NULL;
  VREF_AUTO(list, PyList_New(0));
  if (!list) return NULL;
  for (long i = 0; i < stop; i++) {
    VREF_AUTO(item, VNEW_LONG(i));
    if (!item) return NULL;
    if (PyList_Append(list, item) < 0) return NULL;
  }
  return VSTEAL(list);
}

/* ═══════════════════════════════════════════════════════════════════════════
   Point — vectorcall type: fastcall methods AND a callable instance
   ═══════════════════════════════════════════════════════════════════════════ */

VTYPE_HEAD(Point)
  double x, y;
VTYPE_END(Point);

/* Forward declaration: Point_call (below) constructs new Points via
 * VNEW_CALLABLE(Point, ...), which needs Point_type; Point_new (below)
 * also needs it to arm freshly allocated instances. VTOBJ_DEF's
 * definition further down completes this tentative declaration — legal
 * C, and the same pattern any self-referential/constructing type needs. */
static PyTypeObject Point_type;

/* Point's own vc_call: what runs when an existing Point instance is
 * *called*, e.g. `p(1.0, -2.0)`. This used to be Point_vc, dead code
 * wired up incorrectly as if calling the *type* `Point(x, y)` went
 * through vectorcall (it doesn't — see pyfast.h's VTYPE_FLAGS doc
 * comment). Now it is what it is actually declared as: the instance
 * call, which returns a new Point translated by (dx, dy). */
static PyObject *
Point_call(VCALL_SIG)
{
  VCALL_BEGIN;
  VSELF(Point);
  VEXPECT(2);
  double dx, dy;
  VALL(VDOUBLE(0, &dx) && VDOUBLE(1, &dy));
  Point *out = VNEW_CALLABLE(Point, Point_call);
  if (!out) return NULL;
  out->x = self->x + dx;
  out->y = self->y + dy;
  return (PyObject *)out;
}

/* Real construction path: Point(x, y). tp_new (not vc_call — see
 * pyfast.h) parses via the standard (args, kwds) tuple/dict convention
 * (there is no vectorcall hook for type construction available to
 * extension authors) and arms vc_call via VNEW_CALLABLE so every live
 * instance has a valid function pointer there before tp_call =
 * PyVectorcall_Call can ever read it. */
static PyObject *
Point_new(PyTypeObject *type, PyObject *args, PyObject *kwds)
{
  (void)kwds;
  double x, y;
  if (!PyArg_ParseTuple(args, "dd", &x, &y)) return NULL;
  Point *self = VNEW_CALLABLE(Point, Point_call);
  if (!self) return NULL;
  self->x = x;
  self->y = y;
  (void)type;
  return (PyObject *)self;
}

static void Point_dealloc(PyObject *self) {
  Py_TYPE(self)->tp_free(self);
}

static PyObject *Point_dist(VMETHOD_SIG) {
  VSELF(Point);
  VRETURN_DOUBLE(sqrt(self->x * self->x + self->y * self->y));
}

/* Previously: no VEXPECT, so VDOUBLE(0,..) read _a[0] unconditionally —
 * scale() with zero arguments read past the argument vector. */
static PyObject *Point_scale(VMETHOD_SIG) {
  VSELF(Point);
  VEXPECT(1);
  double factor;
  if (!VDOUBLE(0, &factor)) return NULL;
  self->x *= factor;
  self->y *= factor;
  VRETURN_NONE;
}

static PyObject *Point_move(VMETHOD_KW_SIG) {
  VSELF(Point);
  double dx, dy;
  VALL(VKWOPT_DOUBLE("dx", &dx, 0.0)
       && VKWOPT_DOUBLE("dy", &dy, 0.0));
  self->x += dx;
  self->y += dy;
  VRETURN_NONE;
}

/* Two independent, previously-unknown bugs used to live here:
 *
 * 1. PyUnicode_FromFormat() only understands a small fixed whitelist of
 *    conversions (%d/%ld/%zd/%s/%p/%A/%U/%S/%R/...) — unlike libc
 *    printf, it has no %f/%g float support. `PyUnicode_FromFormat(
 *    "Point(%.16g, %.16g)", ...)` raised SystemError("invalid format
 *    string: ...") the moment it actually ran (confirmed by directly
 *    invoking it against the pinned 3.14 runtime). snprintf() into a
 *    stack buffer, then PyUnicode_FromString(), is the correct way to
 *    interpolate a float with libc's format machinery.
 *
 * 2. Naming entries "__repr__"/"__str__" in tp_methods does NOT wire
 *    them into the tp_repr/tp_str slots for a static (non-heap)
 *    PyTypeObject like this one — that method-table-to-slot sync
 *    (fixup_slot_dispatchers) only runs for heap types created via
 *    the `class` statement / PyType_FromSpec. A static type's repr()/
 *    str() builtins fall through to the default `<module.Point object
 *    at 0x...>` unless tp_repr/tp_str are set directly, which is what
 *    the two functions below do (matching the real `reprfunc`
 *    signature — a single `PyObject *self`, not the fastcall triple).
 */
static PyObject *Point_tp_repr(PyObject *self) {
  Point *p = (Point *)self;
  char buf[64];
  snprintf(buf, sizeof buf, "Point(%.16g, %.16g)", p->x, p->y);
  return PyUnicode_FromString(buf);
}

static PyObject *Point_tp_str(PyObject *self) {
  Point *p = (Point *)self;
  char buf[64];
  snprintf(buf, sizeof buf, "(%.2f, %.2f)", p->x, p->y);
  return PyUnicode_FromString(buf);
}

static PyMethodDef Point_methods[] = {
  VMETH_ENTRY("dist",  Point, dist,  "dist() -> float\nDistance from origin."),
  VMETH_ENTRY("scale", Point, scale, "scale(factor)\nMultiply coordinates by factor."),
  VMETH_KW_ENTRY("move", Point, move, "move(*, dx=0, dy=0)\nTranslate by (dx, dy)."),
  {NULL, NULL, 0, NULL}
};

VTOBJ_DEF(Point,
  .tp_name = "vcall_demo.Point",
  .tp_flags = VTYPE_FLAGS,
  .tp_methods = Point_methods,
  .tp_new = Point_new,
  .tp_dealloc = Point_dealloc,
  .tp_call = PyVectorcall_Call,
  .tp_repr = Point_tp_repr,
  .tp_str = Point_tp_str,
  .tp_doc = "Point(x, y) - call an instance p(dx, dy) to get a translated copy.",
);

/* ═══════════════════════════════════════════════════════════════════════════
   Module
   ═══════════════════════════════════════════════════════════════════════════ */

VMOD_BEGIN
  VMOD_FUNC("add",             vcall_add,           "add(x, y) -> int"),
  VMOD_FUNC_KW("xor_bytes",    vcall_xor,           "xor_bytes(data, *, key) -> bytes"),
  VMOD_FUNC_KW("xor_kwb",      vcall_xor_kwb,       "xor_kwb(*, data, key) -> bytes"),
  VMOD_FUNC_KW("xor_kwb_buf",  vcall_xor_kwb_buf,   "xor_kwb_buf(*, data, key) -> bytes"),
  VMOD_FUNC_KW("greet",        vcall_greet,          "greet(*, name, excited=False) -> str"),
  VMOD_FUNC_KW("prefix_join",  vcall_prefix_join,    "prefix_join(items, *, prefix=None) -> str"),
  VMOD_FUNC_KW("xor_buffer",   vcall_xor_buffer,     "xor_buffer(buf, *, key) -> bytes"),
  VMOD_FUNC_KW("repeat",       vcall_repeat,         "repeat(data, *, times=1) -> bytes"),
  VMOD_FUNC("range",           vcall_range,          "range(stop) -> list"),
VMOD_OBJ(vcall_demo, "vcall.h demo: METH_FASTCALL + vectorcall + METHOD_DESCRIPTOR.")

PyMODINIT_FUNC PyInit_vcall_demo(void) {
  PyObject *m = PyModule_Create(&_vmd);
  if (!m) return NULL;
  VTYPE_READY(Point, m);
  return m;
}
