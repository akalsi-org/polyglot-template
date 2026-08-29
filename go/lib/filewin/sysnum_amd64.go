//go:build linux && amd64

package filewin

const (
	sysFallocate     = 285
	sysFadvise64     = 221
	sysSyncFileRange = 277
)

// io_uring entry points. See uring_linux.go.
const (
	sysIoUringSetup    = 425
	sysIoUringEnter    = 426
	sysIoUringRegister = 427
)
