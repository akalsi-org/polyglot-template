#define PY_SSIZE_T_CLEAN
#include <Python.h>

namespace {

PyObject* xor_bytes(PyObject*, PyObject* args) {
  Py_buffer input{};
  PyObject* key_object = nullptr;
  if (!PyArg_ParseTuple(args, "y*O:xor_bytes", &input, &key_object)) {
    return nullptr;
  }
  // The "I" converter is PyLong_AsUnsignedLongMask: it wraps silently
  // instead of raising, so 2**32 arrived here as 0 and the range check
  // below saw an in-range key. Convert explicitly — PyLong_AsUnsignedLong
  // raises OverflowError for negative and oversized values, and anything
  // that survives that is a real value the byte-range check can judge.
  if (!PyLong_Check(key_object)) {
    PyBuffer_Release(&input);
    PyErr_SetString(PyExc_TypeError, "key must be an int");
    return nullptr;
  }
  const unsigned long key = PyLong_AsUnsignedLong(key_object);
  if (key == static_cast<unsigned long>(-1) && PyErr_Occurred() != nullptr) {
    PyBuffer_Release(&input);
    return nullptr;
  }
  if (key > 0xffU) {
    PyBuffer_Release(&input);
    PyErr_SetString(PyExc_ValueError, "key must fit in one byte");
    return nullptr;
  }

  PyObject* output = PyBytes_FromStringAndSize(nullptr, input.len);
  if (output == nullptr) {
    PyBuffer_Release(&input);
    return nullptr;
  }
  auto* destination = reinterpret_cast<unsigned char*>(PyBytes_AS_STRING(output));
  const auto* source = static_cast<const unsigned char*>(input.buf);
  for (Py_ssize_t index = 0; index < input.len; ++index) {
    destination[index] = static_cast<unsigned char>(source[index] ^ key);
  }
  PyBuffer_Release(&input);
  return output;
}

PyMethodDef methods[] = {
  {"xor_bytes", xor_bytes, METH_VARARGS, "XOR every byte with an 8-bit key."},
  {nullptr, nullptr, 0, nullptr},
};

PyModuleDef module = {
  PyModuleDef_HEAD_INIT,
  "_native",
  "Pinned-musl byte helpers.",
  -1,
  methods,
  nullptr,
  nullptr,
  nullptr,
  nullptr,
};

}  // namespace

PyMODINIT_FUNC PyInit__native() { return PyModule_Create(&module); }
