#include "pyfast/pyfast.h"

#include <math.h>

/* ═══════════════════════════════════════════════════════════════════════════
   Module-level functions
   ═══════════════════════════════════════════════════════════════════════════ */

VFUNC(vcall_add) {
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
    result = VSTEAL(joined);
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

VFUNC_KW(vcall_repeat) {
  const char *src; Py_ssize_t len;
  int times;
  VEXPECT(1);
  if (!(src = VBYTES(0, &len))) return NULL;
  if (!VKWOPT_INT("times", &times, 1)) return NULL;
  PyObject *out = VNEW_BYTES(NULL, len * times);
  if (!out) return NULL;
  char *dst = PyBytes_AS_STRING(out);
  for (int t = 0; t < times; t++)
    memcpy(dst + (Py_ssize_t)t * len, src, len);
  return out;
}

VFUNC(vcall_range) {
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
   Point — vectorcall type with zero-alloc methods
   ═══════════════════════════════════════════════════════════════════════════ */

VTYPE_HEAD(Point)
  double x, y;
VTYPE_END(Point);

static PyObject *
Point_vc(PyObject *callable,
         PyObject *const *args, size_t nargsf, PyObject *kwnames)
{
  Py_ssize_t n = PyVectorcall_NARGS(nargsf);
  if (n != 2) {
    PyErr_Format(PyExc_TypeError,
                 "Point() takes 2 arguments (%zd given)", n);
    return NULL;
  }
  double x, y;
  VALL(_v_double(args[0], &x, "argument 0")
       && _v_double(args[1], &y, "argument 1"));
  Point *self = VNEW(Point);
  if (!self) return NULL;
  self->x = x;
  self->y = y;
  return (PyObject *)self;
}

static void Point_dealloc(PyObject *self) {
  Py_TYPE(self)->tp_free(self);
}

static PyObject *Point_dist(VMETHOD_SIG) {
  VSELF(Point);
  VRETURN_DOUBLE(sqrt(self->x * self->x + self->y * self->y));
}

static PyObject *Point_scale(VMETHOD_SIG) {
  VSELF(Point);
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

static PyObject *Point_repr(VMETHOD_SIG) {
  VSELF(Point);
  return PyUnicode_FromFormat("Point(%.16g, %.16g)", self->x, self->y);
}

static PyObject *Point_str(VMETHOD_SIG) {
  VSELF(Point);
  return PyUnicode_FromFormat("(%.2f, %.2f)", self->x, self->y);
}

static PyMethodDef Point_methods[] = {
  VMETH_ENTRY("dist",  Point, dist,  "dist() -> float\nDistance from origin."),
  VMETH_ENTRY("scale", Point, scale, "scale(factor)\nMultiply coordinates by factor."),
  VMETH_KW_ENTRY("move", Point, move, "move(*, dx=0, dy=0)\nTranslate by (dx, dy)."),
  VMETH_ENTRY("__repr__", Point, repr, NULL),
  VMETH_ENTRY("__str__",  Point, str,  NULL),
  {NULL, NULL, 0, NULL}
};

VTOBJ_DEF(Point,
  .tp_name = "vcall_demo.Point",
  .tp_flags = VTYPE_FLAGS,
  .tp_methods = Point_methods,
  .tp_dealloc = Point_dealloc,
  .tp_call = PyVectorcall_Call,
  .tp_doc = "Point(x, y)",
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
