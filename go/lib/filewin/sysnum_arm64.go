//go:build linux && arm64

package filewin

const (
	sysFutex         = 98
	sysFallocate     = 47
	sysFadvise64     = 223
	sysSyncFileRange = 84
)

// io_uring entry points. See uring_linux.go.
const (
	sysIoUringSetup    = 425
	sysIoUringEnter    = 426
	sysIoUringRegister = 427
)
