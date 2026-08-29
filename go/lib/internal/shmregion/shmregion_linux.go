//go:build linux && (amd64 || arm64)

// Package shmregion owns file-backed mirrored shared-memory mappings.
package shmregion

import (
	"errors"
	"io"
	"math"
	"os"
	"strings"
	"syscall"
	"unsafe"
)

// Backend selects the storage that backs a mapping.
type Backend uint8

// ErrProbeTooSmall reports that the backing file cannot contain the requested probe.
var ErrProbeTooSmall = errors.New("shmregion: attachment probe is too small")

const (
	// BackendMemfd uses an anonymous memory file.
	BackendMemfd Backend = iota
	// BackendSHM creates an exclusive file under /dev/shm from a simple name.
	// A simple name is nonempty and contains no "..", slash, backslash, or NUL.
	BackendSHM
	// BackendFile creates an exclusive file at a nonempty CreateOptions.Name by default.
	BackendFile
)

// Layout gives the control and single-mapped arena lengths in bytes.
type Layout struct {
	ControlBytes uint64
	ArenaBytes   uint64
}

// CreateOptions controls backing-file creation.
type CreateOptions struct {
	Backend            Backend
	Name               string
	Layout             Layout
	DisablePreallocate bool
	AllowOverwrite     bool
	MemfdName          string
	DisableMemfdSeals  bool
}

// Mapping owns one descriptor and one mirrored virtual-memory reservation.
type Mapping struct {
	fd      int
	memory  []byte
	control []byte
	arena   []byte
	closed  bool
}

// Probe reads the start of fd into dst and returns the backing-file size.
// Probe does not take ownership of fd or change its file offset.
func Probe(fd int, dst []byte) (uint64, error) {
	var stat syscall.Stat_t
	if err := syscall.Fstat(fd, &stat); err != nil {
		return 0, err
	}
	if stat.Size < 0 || uint64(stat.Size) < uint64(len(dst)) {
		return 0, ErrProbeTooSmall
	}
	if len(dst) == 0 {
		return uint64(stat.Size), nil
	}
	n, err := syscall.Pread(fd, dst, 0)
	if err != nil {
		return 0, err
	}
	if n != len(dst) {
		return 0, io.ErrUnexpectedEOF
	}
	return uint64(stat.Size), nil
}

// Create creates and maps a new mirrored shared-memory region.
func Create(options CreateOptions) (*Mapping, error) {
	if err := validateCreateOptions(options); err != nil {
		return nil, err
	}
	if err := validateLayout(options.Layout); err != nil {
		return nil, err
	}
	backing, err := openBacking(options, options.Layout.ControlBytes+options.Layout.ArenaBytes)
	if err != nil {
		return nil, err
	}
	mapping, err := mapBacking(backing.fd, options.Layout)
	if err != nil {
		return nil, cleanupBacking(backing, err)
	}
	return mapping, nil
}

// Attach duplicates fd with close-on-exec and maps the validated layout.
func Attach(fd int, layout Layout) (*Mapping, error) {
	if err := validateLayout(layout); err != nil {
		return nil, err
	}
	dup, err := DupCloexec(fd)
	if err != nil {
		return nil, err
	}
	var stat syscall.Stat_t
	if err := syscall.Fstat(dup, &stat); err != nil {
		syscall.Close(dup)
		return nil, err
	}
	if stat.Size < 0 || uint64(stat.Size) != layout.ControlBytes+layout.ArenaBytes {
		syscall.Close(dup)
		return nil, syscall.EINVAL
	}
	mapping, err := mapMirrored(dup, layout)
	if err != nil {
		syscall.Close(dup)
		return nil, err
	}
	return mapping, nil
}

// Control returns the single-mapped control bytes.
// Close invalidates the returned slice.
func (m *Mapping) Control() []byte {
	if m == nil {
		return nil
	}
	return m.control
}

// Arena returns two contiguous virtual mappings of the arena bytes.
// Close invalidates the returned slice.
func (m *Mapping) Arena() []byte {
	if m == nil {
		return nil
	}
	return m.arena
}

// DupFD duplicates the backing descriptor with close-on-exec enabled.
// The caller owns the returned descriptor.
func (m *Mapping) DupFD() (int, error) {
	if m == nil || m.closed {
		return -1, syscall.EBADF
	}
	return DupCloexec(m.fd)
}

// Closed reports whether Close released the mapping.
func (m *Mapping) Closed() bool { return m == nil || m.closed }

// Close releases the mapping and its owned descriptor.
func (m *Mapping) Close() error {
	if m == nil || m.closed {
		return nil
	}
	m.closed = true
	unmapErr := syscall.Munmap(m.memory)
	closeErr := syscall.Close(m.fd)
	m.memory = nil
	m.control = nil
	m.arena = nil
	if unmapErr != nil {
		return unmapErr
	}
	return closeErr
}

// DupCloexec duplicates fd with close-on-exec enabled.
func DupCloexec(fd int) (int, error) {
	const fDupfdCloexec = 1030
	r0, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), fDupfdCloexec, 0)
	if errno != 0 {
		return -1, errno
	}
	return int(r0), nil
}

func validateCreateOptions(options CreateOptions) error {
	switch options.Backend {
	case BackendMemfd:
		return nil
	case BackendSHM:
		if options.Name == "" || strings.Contains(options.Name, "..") || strings.ContainsAny(options.Name, "/\\\x00") {
			return syscall.EINVAL
		}
		return nil
	case BackendFile:
		if options.Name == "" {
			return syscall.EINVAL
		}
		return nil
	default:
		return syscall.EINVAL
	}
}

func validateLayout(layout Layout) error {
	page := uint64(os.Getpagesize())
	if layout.ControlBytes == 0 || layout.ArenaBytes == 0 {
		return syscall.EINVAL
	}
	if layout.ControlBytes%page != 0 || layout.ArenaBytes%page != 0 {
		return syscall.EINVAL
	}
	if layout.ControlBytes > math.MaxUint64-layout.ArenaBytes {
		return syscall.EOVERFLOW
	}
	fileBytes := layout.ControlBytes + layout.ArenaBytes
	if layout.ArenaBytes > math.MaxUint64-fileBytes || fileBytes+layout.ArenaBytes > uint64(math.MaxInt) {
		return syscall.EOVERFLOW
	}
	return nil
}

type backingFile struct {
	fd          int
	createdPath string
}

var (
	ftruncateBacking = syscall.Ftruncate
	fallocateBacking = syscall.Fallocate
	mapBacking       = mapMirrored
	sealMemfd        = func(fd int) error {
		const fAddSeals = 1033
		const seals = 0x0002 | 0x0004 | 0x0001
		_, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), fAddSeals, seals)
		if errno != 0 {
			return errno
		}
		return nil
	}
)

func openBacking(options CreateOptions, size uint64) (backingFile, error) {
	backing := backingFile{fd: -1}
	var err error
	switch options.Backend {
	case BackendMemfd:
		name := options.MemfdName
		if name == "" {
			name = "pgt-shmregion"
		}
		nameBytes, err := syscall.BytePtrFromString(name)
		if err != nil {
			return backing, err
		}
		r0, _, errno := syscall.Syscall(sysMemfdCreate, uintptr(unsafe.Pointer(nameBytes)), 0x0001|0x0002, 0)
		if errno != 0 {
			return backing, errno
		}
		backing.fd = int(r0)
	case BackendSHM:
		backing.createdPath = "/dev/shm/" + options.Name
		backing.fd, err = syscall.Open(backing.createdPath, syscall.O_RDWR|syscall.O_CREAT|syscall.O_EXCL|syscall.O_CLOEXEC, 0600)
		if err != nil {
			return backingFile{fd: -1}, err
		}
	case BackendFile:
		flags := syscall.O_RDWR | syscall.O_CREAT | syscall.O_CLOEXEC
		if options.AllowOverwrite {
			flags |= syscall.O_TRUNC
		} else {
			flags |= syscall.O_EXCL
			backing.createdPath = options.Name
		}
		backing.fd, err = syscall.Open(options.Name, flags, 0600)
		if err != nil {
			return backingFile{fd: -1}, err
		}
	default:
		return backing, syscall.EINVAL
	}
	if err = ftruncateBacking(backing.fd, int64(size)); err != nil {
		return backingFile{fd: -1}, cleanupBacking(backing, err)
	}
	if !options.DisablePreallocate {
		if err = fallocateBacking(backing.fd, 0, 0, int64(size)); err != nil {
			return backingFile{fd: -1}, cleanupBacking(backing, err)
		}
	}
	if options.Backend == BackendMemfd && !options.DisableMemfdSeals {
		if err = sealMemfd(backing.fd); err != nil {
			return backingFile{fd: -1}, cleanupBacking(backing, err)
		}
	}
	return backing, nil
}

func cleanupBacking(backing backingFile, cause error) error {
	closeErr := syscall.Close(backing.fd)
	var unlinkErr error
	if backing.createdPath != "" {
		unlinkErr = syscall.Unlink(backing.createdPath)
	}
	if closeErr == nil && unlinkErr == nil {
		return cause
	}
	return errors.Join(cause, closeErr, unlinkErr)
}

func mapMirrored(fd int, layout Layout) (*Mapping, error) {
	controlBytes := uintptr(layout.ControlBytes)
	arenaBytes := uintptr(layout.ArenaBytes)
	span := controlBytes + 2*arenaBytes
	hole, err := syscall.Mmap(-1, 0, int(span), syscall.PROT_NONE, syscall.MAP_PRIVATE|syscall.MAP_ANON|0x4000)
	if err != nil {
		return nil, err
	}
	base := uintptr(unsafe.Pointer(&hole[0]))
	unmap := func() { _ = syscall.Munmap(hole) }
	fixed := func(at, length, offset uintptr) error {
		mapped, _, errno := syscall.Syscall6(syscall.SYS_MMAP, base+at, length, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED|syscall.MAP_FIXED, uintptr(fd), offset)
		if errno != 0 {
			return errno
		}
		if mapped != base+at {
			return syscall.EFAULT
		}
		return nil
	}
	if err := fixed(0, controlBytes, 0); err != nil {
		unmap()
		return nil, err
	}
	if err := fixed(controlBytes, arenaBytes, controlBytes); err != nil {
		unmap()
		return nil, err
	}
	if err := fixed(controlBytes+arenaBytes, arenaBytes, controlBytes); err != nil {
		unmap()
		return nil, err
	}
	return &Mapping{
		fd:      fd,
		memory:  hole,
		control: hole[:controlBytes],
		arena:   hole[controlBytes:],
	}, nil
}
