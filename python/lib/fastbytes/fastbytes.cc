#define PY_SSIZE_T_CLEAN
#include <Python.h>

namespace {

PyObject* xor_bytes(PyObject*, PyObject* args) {
  Py_buffer input{};
  unsigned int key = 0;
  if (!PyArg_ParseTuple(args, "y*I:xor_bytes", &input, &key)) {
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
