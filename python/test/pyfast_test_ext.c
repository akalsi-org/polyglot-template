#include "pyfast/pyfast.h"

#include <math.h>
#include <stdio.h>
#include <string.h>

VFUNC(add) {
  long left;
  long right;
  VEXPECT(2);
  VALL(VINT(0, &left) && VINT(1, &right));
  VRETURN_LONG(left + right);
}

VFUNC_KW(xor_bytes) {
  const char *src;
  Py_ssize_t len;
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

VFUNC_KW(xor_kwb) {
  const char *src;
  VEXPECT(0);
  Py_ssize_t len;
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

VFUNC_KW(xor_kwb_buf) {
  unsigned long key;
  VEXPECT(0);
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
    dst[i] = (unsigned char)(src[i] ^ (unsigned char)key);
  return out;
}

VFUNC_KW(greet) {
  const char *name;
  VEXPECT(0);
  Py_ssize_t name_len;
  int excited;
  if (!(name = VKWSTR("name", &name_len))) return NULL;
  if (!VKWOPT_BOOL("excited", &excited, 0)) return NULL;
  char buf[256];
  int written = snprintf(buf, sizeof buf, "Hello, %.*s%s",
                         (int)name_len, name, excited ? "!!" : ".");
  if (written < 0 || (size_t)written >= sizeof buf) {
    PyErr_SetString(PyExc_OverflowError, "name is too long");
    return NULL;
  }
  return PyUnicode_FromString(buf);
}

VFUNC_KW(prefix_join) {
  const char *prefix;
  Py_ssize_t prefix_len;
  VEXPECT(1);
  PyObject *items = VA(0);
  if (!PyList_Check(items)) {
    _vtype_err("argument 0", "list", items);
    return NULL;
  }
  if (!VKWOPT_STR("prefix", &prefix, &prefix_len)) return NULL;
  (void)prefix_len;
  VREF_AUTO(result, VNEW_STR(prefix ? prefix : ""));
  if (!result) return NULL;
  Py_ssize_t count = PyList_GET_SIZE(items);
  for (Py_ssize_t i = 0; i < count; i++) {
    VREF_AUTO(item_str, PyObject_Str(PyList_GET_ITEM(items, i)));
    if (!item_str) return NULL;
    VREF_AUTO(joined, PyUnicode_Concat(result, item_str));
    if (!joined) return NULL;
    VMOVE(result, VSTEAL(joined));
  }
  return VSTEAL(result);
}

VFUNC_KW(xor_buffer) {
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
    dst[i] = (unsigned char)(src[i] ^ (unsigned char)key);
  return out;
}

VFUNC_KW(repeat) {
  const char *src;
  Py_ssize_t len;
  long times;
  VEXPECT(1);
  if (!(src = VBYTES(0, &len))) return NULL;
  if (!VKWOPT_INT("times", &times, 1)) return NULL;
  if (times < 0) {
    PyErr_SetString(PyExc_ValueError, "times must be non-negative");
    return NULL;
  }
  if (len > 0 && (unsigned long)times >
                    (unsigned long)(PY_SSIZE_T_MAX / len)) {
    PyErr_SetString(PyExc_OverflowError, "repeat: result too large");
    return NULL;
  }
  PyObject *out = VNEW_BYTES(NULL, len * (Py_ssize_t)times);
  if (!out) return NULL;
  char *dst = PyBytes_AS_STRING(out);
  for (long i = 0; i < times; i++)
    memcpy(dst + (Py_ssize_t)i * len, src, (size_t)len);
  return out;
}

VFUNC(range_values) {
  long stop;
  VEXPECT(1);
  if (!VINT(0, &stop)) return NULL;
  VREF_AUTO(list, PyList_New(0));
  if (!list) return NULL;
  for (long i = 0; i < stop; i++) {
    VREF_AUTO(item, VNEW_LONG(i));
    if (!item || PyList_Append(list, item) < 0) return NULL;
  }
  return VSTEAL(list);
}

VTYPE_HEAD(Point)
  double x;
  double y;
VTYPE_END(Point);

static PyTypeObject Point_type;

static PyObject *Point_call(VCALL_SIG) {
  VCALL_BEGIN;
  VSELF(Point);
  VEXPECT(2);
  double dx;
  double dy;
  VALL(VDOUBLE(0, &dx) && VDOUBLE(1, &dy));
  Point *out = VNEW_CALLABLE(Point, Point_call);
  if (!out) return NULL;
  out->x = self->x + dx;
  out->y = self->y + dy;
  return (PyObject *)out;
}

static PyObject *Point_new(PyTypeObject *type, PyObject *args, PyObject *kwds) {
  (void)type;
  (void)kwds;
  double x;
  double y;
  if (!PyArg_ParseTuple(args, "dd", &x, &y)) return NULL;
  Point *self = VNEW_CALLABLE(Point, Point_call);
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
  VEXPECT(1);
  double factor;
  if (!VDOUBLE(0, &factor)) return NULL;
  self->x *= factor;
  self->y *= factor;
  VRETURN_NONE;
}

static PyObject *Point_move(VMETHOD_KW_SIG) {
  VSELF(Point);
  double dx;
  double dy;
  VALL(VKWOPT_DOUBLE("dx", &dx, 0.0) &&
       VKWOPT_DOUBLE("dy", &dy, 0.0));
  self->x += dx;
  self->y += dy;
  VRETURN_NONE;
}

static PyObject *Point_tp_repr(PyObject *self) {
  Point *point = (Point *)self;
  char buf[80];
  snprintf(buf, sizeof buf, "Point(%.16g, %.16g)", point->x, point->y);
  return PyUnicode_FromString(buf);
}

static PyObject *Point_tp_str(PyObject *self) {
  Point *point = (Point *)self;
  char buf[64];
  snprintf(buf, sizeof buf, "(%.2f, %.2f)", point->x, point->y);
  return PyUnicode_FromString(buf);
}

static PyMethodDef Point_methods[] = {
  VMETH_ENTRY("dist", Point, dist, "dist() -> float"),
  VMETH_ENTRY("scale", Point, scale, "scale(factor) -> None"),
  VMETH_KW_ENTRY("move", Point, move, "move(*, dx=0, dy=0) -> None"),
  {NULL, NULL, 0, NULL},
};

VTOBJ_DEF(Point,
  .tp_name = "pyfast_test_ext.Point",
  .tp_flags = VTYPE_FLAGS,
  .tp_methods = Point_methods,
  .tp_new = Point_new,
  .tp_dealloc = Point_dealloc,
  .tp_call = PyVectorcall_Call,
  .tp_repr = Point_tp_repr,
  .tp_str = Point_tp_str,
  .tp_doc = "Point(x, y), with callable translated copies.",
);

static PyObject *module_init(PyObject *module) {
  VTYPE_READY(Point, module);
  return module;
}

VMOD_BEGIN(_native)
  VMOD_FUNC("add", add, "add(left, right) -> int"),
  VMOD_FUNC_KW("xor_bytes", xor_bytes, "xor_bytes(data, *, key) -> bytes"),
  VMOD_FUNC_KW("xor_kwb", xor_kwb, "xor_kwb(*, data, key) -> bytes"),
  VMOD_FUNC_KW("xor_kwb_buf", xor_kwb_buf, "xor_kwb_buf(*, data, key) -> bytes"),
  VMOD_FUNC_KW("greet", greet, "greet(*, name, excited=False) -> str"),
  VMOD_FUNC_KW("prefix_join", prefix_join, "prefix_join(items, *, prefix=None) -> str"),
  VMOD_FUNC_KW("xor_buffer", xor_buffer, "xor_buffer(data, *, key) -> bytes"),
  VMOD_FUNC_KW("repeat", repeat, "repeat(data, *, times=1) -> bytes"),
  VMOD_FUNC("range", range_values, "range(stop) -> list"),
VMOD_END(_native, "Small pyfast test extension.", module_init)
