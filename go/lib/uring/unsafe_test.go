//go:build linux && (amd64 || arm64)

package uring_test

import "unsafe"

func ptr(b []byte) unsafe.Pointer { return unsafe.Pointer(&b[0]) }
