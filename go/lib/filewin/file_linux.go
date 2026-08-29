//go:build linux && (amd64 || arm64)

package filewin

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

const (
	magic      uint64 = 0x7067745f6677696e // pgt_fwin
	version    uint32 = 1
	headerSize uint64 = 4096

	offMagic     = 0
	offVersion   = 8
	offHeader    = 12
	offWritePos  = 16
	offReadPos   = 24
	offWriteGen  = 32
	offReadGen   = 36
	offCommitted = 40
	offEpoch     = 48

	futexWaitOp = 0
	futexWakeOp = 1
	madvWill    = 3
	madvDont    = 4
)

var (
	ErrFormat   = errors.New("filewin: invalid format")
	ErrMisuse   = errors.New("filewin: invalid operation")
	ErrClosed   = errors.New("filewin: closed")
	ErrTooLarge = errors.New("filewin: reservation exceeds window")
	ErrFull     = errors.New("filewin: live window is full")
)

// Config controls file creation.
type Config struct {
	Path       string
	Window     uint64
	Extent     uint64
	Ahead      uint64
	MaxReserve uint64
}

// File is one attached log file and a header mapping.
type File struct {
	path       string
	fd         int
	header     []byte
	window     uint64
	extent     uint64
	ahead      uint64
	maxReserve uint64
	page       uint64
	closed     bool
}

func pageSize() uint64 { return uint64(os.Getpagesize()) }

func alignDown(n, a uint64) uint64 { return n &^ (a - 1) }
func alignUp(n, a uint64) uint64   { return (n + a - 1) &^ (a - 1) }

func (c Config) normalize() (Config, error) {
	page := pageSize()
	if c.Path == "" {
		return Config{}, fmt.Errorf("%w: path", ErrFormat)
	}
	if c.Window == 0 {
		c.Window = 16 << 20
	}
	if c.Extent == 0 {
		c.Extent = 64 << 20
	}
	if c.Ahead == 0 {
		c.Ahead = 4 << 20
	}
	if c.MaxReserve == 0 {
		c.MaxReserve = 1 << 20
	}
	c.Window = alignUp(c.Window, page)
	c.Extent = alignUp(c.Extent, page)
	c.Ahead = alignUp(c.Ahead, page)
	if c.Window < page || c.Extent < page || c.MaxReserve > c.Window/2 {
		return Config{}, fmt.Errorf("%w: window", ErrFormat)
	}
	return c, nil
}

// Create creates an exclusive file and maps its header.
func Create(cfg Config) (*File, error) {
	cfg, err := cfg.normalize()
	if err != nil {
		return nil, err
	}
	fd, err := syscall.Open(cfg.Path, syscall.O_RDWR|syscall.O_CREAT|syscall.O_EXCL|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	initial := headerSize + cfg.Window
	if initial < headerSize+cfg.Extent {
		initial = headerSize + cfg.Extent
	}
	if err := syscall.Ftruncate(fd, int64(initial)); err != nil {
		syscall.Close(fd)
		_ = syscall.Unlink(cfg.Path)
		return nil, err
	}
	if err := fallocate(fd, 0, int64(initial)); err != nil && !errors.Is(err, syscall.ENOSYS) && !errors.Is(err, syscall.EOPNOTSUPP) {
		syscall.Close(fd)
		_ = syscall.Unlink(cfg.Path)
		return nil, err
	}
	header, err := mapRange(fd, 0, int(headerSize), syscall.PROT_READ|syscall.PROT_WRITE)
	if err != nil {
		syscall.Close(fd)
		_ = syscall.Unlink(cfg.Path)
		return nil, err
	}
	put64(header, offMagic, magic)
	put32(header, offVersion, version)
	put32(header, offHeader, uint32(headerSize))
	put64(header, offCommitted, initial)
	put64(header, offEpoch, 1)
	f := &File{
		path: cfg.Path, fd: fd, header: header,
		window: cfg.Window, extent: cfg.Extent, ahead: cfg.Ahead,
		maxReserve: cfg.MaxReserve, page: pageSize(),
	}
	return f, nil
}

// Open attaches to an existing file.
func Open(path string, cfg Config) (*File, error) {
	cfg.Path = path
	cfg, err := cfg.normalize()
	if err != nil {
		return nil, err
	}
	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	header, err := mapRange(fd, 0, int(headerSize), syscall.PROT_READ|syscall.PROT_WRITE)
	if err != nil {
		syscall.Close(fd)
		return nil, err
	}
	if get64(header, offMagic) != magic || get32(header, offVersion) != version || get32(header, offHeader) != uint32(headerSize) {
		_ = syscall.Munmap(header)
		syscall.Close(fd)
		return nil, ErrFormat
	}
	return &File{
		path: path, fd: fd, header: header,
		window: cfg.Window, extent: cfg.Extent, ahead: cfg.Ahead,
		maxReserve: cfg.MaxReserve, page: pageSize(),
	}, nil
}

// Close unmaps the header and closes the descriptor.
func (f *File) Close() error {
	if f == nil || f.closed {
		return nil
	}
	f.closed = true
	err := syscall.Munmap(f.header)
	if closeErr := syscall.Close(f.fd); err == nil {
		err = closeErr
	}
	f.header = nil
	return err
}

func (f *File) writePos() *uint64 { return (*uint64)(unsafe.Pointer(&f.header[offWritePos])) }
func (f *File) readPos() *uint64  { return (*uint64)(unsafe.Pointer(&f.header[offReadPos])) }
func (f *File) writeGen() *uint32 { return (*uint32)(unsafe.Pointer(&f.header[offWriteGen])) }
func (f *File) readGen() *uint32  { return (*uint32)(unsafe.Pointer(&f.header[offReadGen])) }
func (f *File) committed() *uint64 {
	return (*uint64)(unsafe.Pointer(&f.header[offCommitted]))
}

func (f *File) dataFileOff(dataPos uint64) uint64 { return headerSize + dataPos }

func mapRange(fd int, off uint64, length int, prot int) ([]byte, error) {
	return syscall.Mmap(fd, int64(off), length, prot, syscall.MAP_SHARED)
}

func fallocate(fd int, off, len int64) error {
	_, _, errno := syscall.Syscall6(sysFallocate, uintptr(fd), 0, uintptr(off), uintptr(len), 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}

func futexWait(ptr *uint32, val uint32) {
	ts := syscall.Timespec{Nsec: 10_000_000}
	_, _, _ = syscall.Syscall6(
		sysFutex,
		uintptr(unsafe.Pointer(ptr)),
		futexWaitOp,
		uintptr(val),
		uintptr(unsafe.Pointer(&ts)),
		0,
		0,
	)
}

func futexWake(ptr *uint32) {
	_, _, _ = syscall.Syscall6(sysFutex, uintptr(unsafe.Pointer(ptr)), futexWakeOp, 1, 0, 0, 0)
}

func madvise(b []byte, advice int) {
	if len(b) == 0 {
		return
	}
	_ = syscall.Madvise(b, advice)
}

func put32(b []byte, off int, v uint32) { binary.LittleEndian.PutUint32(b[off:off+4], v) }
func put64(b []byte, off int, v uint64) { binary.LittleEndian.PutUint64(b[off:off+8], v) }
func get32(b []byte, off int) uint32    { return binary.LittleEndian.Uint32(b[off : off+4]) }
func get64(b []byte, off int) uint64    { return binary.LittleEndian.Uint64(b[off : off+8]) }
