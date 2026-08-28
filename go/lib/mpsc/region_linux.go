//go:build linux && (amd64 || arm64)

package mpsc

import (
	"errors"
	"fmt"
	"math/bits"
	"os"
	"strings"
	"syscall"
	"unsafe"
)

const (
	formatMagic uint64 = 0x7067745f6d707363
	// FormatVersion is the only shared-memory format version this package accepts.
	FormatVersion uint32 = 4

	// Format-v4 header offset 48 stores the creator PID namespace stat.Dev.
	pidNamespaceDevOffset = 48
	// Format-v4 header offset 56 stores the creator PID namespace stat.Ino.
	pidNamespaceInoOffset = 56

	cacheLine             = 64
	controlSize           = 128
	shardSize             = 256
	writerSlotSize        = 8
	maxShards             = 1 << 16
	maxExtent      uint64 = ((1 << 26) - 1) * cacheLine
)

var (
	// ErrFull reports a retryable capacity shortage.
	ErrFull = errors.New("mpsc: full")
	// ErrReaderDead reports that a full queue has a proven-dead reader.
	ErrReaderDead = errors.New("mpsc: reader dead")
	// ErrTooLarge reports that a record cannot fit in the queue format.
	ErrTooLarge = errors.New("mpsc: record too large")
	// ErrPositionExhausted reports logical-position overflow.
	ErrPositionExhausted = errors.New("mpsc: logical position exhausted")
	// ErrNoWriterSlot reports that no producer ownership slot is available.
	ErrNoWriterSlot = errors.New("mpsc: no writer slot")
	// ErrFormatVersion reports a valid header with an unsupported version.
	ErrFormatVersion = errors.New("mpsc: unsupported format version")
	// ErrFormat reports invalid shared metadata or geometry.
	ErrFormat = errors.New("mpsc: invalid format")
	// ErrPIDNamespace reports a creator PID namespace mismatch.
	ErrPIDNamespace = errors.New("mpsc: PID namespace differs")
	// ErrClosed reports an operation on a closed queue.
	ErrClosed = errors.New("mpsc: closed")
)

// PIDNamespaceError reports a region created in a different PID namespace.
type PIDNamespaceError struct {
	// CreatorDev is the creator PID namespace device.
	CreatorDev uint64
	// CreatorIno is the creator PID namespace inode.
	CreatorIno uint64
	// CurrentDev is the attaching process PID namespace device.
	CurrentDev uint64
	// CurrentIno is the attaching process PID namespace inode.
	CurrentIno uint64
}

// Error returns the PID namespace mismatch description.
func (e *PIDNamespaceError) Error() string {
	return fmt.Sprintf(
		"%v: creator device %d inode %d, current device %d inode %d",
		ErrPIDNamespace, e.CreatorDev, e.CreatorIno, e.CurrentDev, e.CurrentIno,
	)
}

// Unwrap identifies the stable cross-namespace error category.
func (e *PIDNamespaceError) Unwrap() error { return ErrPIDNamespace }

// Backend selects the storage that backs a queue region.
type Backend uint8

const (
	// BackendMemfd uses an anonymous sealed memory file.
	BackendMemfd Backend = iota
	// BackendSHM creates an exclusive named file under /dev/shm.
	// Queue Close does not unlink the file.
	BackendSHM
	// BackendFile creates an exclusive file at Config.Name by default.
	BackendFile
)

// MPSCLayout selects the metadata layout for an MPSC queue.
type MPSCLayout uint8

const (
	// MPSCCompact minimizes metadata size and can increase metadata contention.
	MPSCCompact MPSCLayout = iota
	// MPSCPadded64 uses more metadata space to reduce cache-line sharing.
	MPSCPadded64
	// MPSCPadded256 uses 256-byte admission grains and the most payload slack.
	MPSCPadded256
)

// Config controls queue region creation.
// Zero Capacity selects 1 MiB, and zero Backend selects BackendMemfd.
// Zero MPSCLayout selects MPSCCompact.
type Config struct {
	// Capacity is the requested payload capacity in bytes.
	// Creation rounds it up to a supported power of two.
	Capacity uint64
	// Backend selects the region backing store.
	Backend Backend
	// Name is the BackendSHM name or BackendFile path.
	// Queue Close does not remove named backing files.
	Name string
	// DisablePreallocate skips physical backing allocation when true.
	DisablePreallocate bool
	// AllowOverwrite lets BackendFile truncate an existing file.
	// This option destroys the existing file contents.
	AllowOverwrite bool
	// MPSCLayout selects the MPSC metadata layout.
	// CreateSPSC ignores this field.
	MPSCLayout MPSCLayout
}

type region struct {
	fd           int
	base         uintptr
	reservation  uintptr
	memory       []byte
	capacity     uint64
	controlLen   uintptr
	claimStride  uint32
	resultStride uint32
	planeGrain   uint32
	planeShift   uint
	planeMask    uint64
	closed       bool
}

type planeGeometry struct {
	claimStride  uint32
	resultStride uint32
	grain        uint32
}

var mpscLayoutGeometries = [...]planeGeometry{
	MPSCCompact:   {claimStride: 8, resultStride: 16, grain: 64},
	MPSCPadded64:  {claimStride: 64, resultStride: 64, grain: 64},
	MPSCPadded256: {claimStride: 64, resultStride: 64, grain: 256},
}

func mpscGeometry(layout MPSCLayout) (planeGeometry, bool) {
	if int(layout) >= len(mpscLayoutGeometries) {
		return planeGeometry{}, false
	}
	return mpscLayoutGeometries[layout], true
}

// MPSCLayoutExtent returns the canonical grain and rounded payload extent.
func MPSCLayoutExtent(layout MPSCLayout, payloadBytes uint64) (uint64, uint64, error) {
	geometry, ok := mpscGeometry(layout)
	if !ok {
		return 0, 0, ErrFormat
	}
	grain := uint64(geometry.grain)
	extent, ok := extentFor(payloadBytes, grain)
	if !ok {
		return grain, 0, ErrTooLarge
	}
	return grain, extent, nil
}

// SPSCExtent returns the rounded payload extent for the SPSC format.
func SPSCExtent(payloadBytes uint64) (uint64, error) {
	extent, ok := extentFor(payloadBytes, cacheLine)
	if !ok {
		return 0, ErrTooLarge
	}
	return extent, nil
}

func isMPSCGeometry(claimStride, resultStride, grain uint32) bool {
	candidate := planeGeometry{claimStride: claimStride, resultStride: resultStride, grain: grain}
	for _, geometry := range mpscLayoutGeometries {
		if candidate == geometry {
			return true
		}
	}
	return false
}

func align(n, a uint64) uint64 { return (n + a - 1) &^ (a - 1) }

func planeBase(shards uint32) uint64 {
	words := (uint64(shards) + 63) / 64
	return controlSize + uint64(shards)*shardSize + words*8 + uint64(shards)*writerSlotSize
}

func planeBytes(capacity uint64, claimStride, resultStride, grain uint32) uint64 {
	if claimStride == 0 && resultStride == 0 {
		return 0
	}
	cells := capacity / uint64(grain)
	return align(cells*uint64(claimStride), cacheLine) + align(cells*uint64(resultStride), cacheLine)
}

func controlBytes(capacity uint64, shards, claimStride, resultStride, grain uint32, page uint64) uint64 {
	raw := planeBase(shards)
	if claimStride != 0 || resultStride != 0 {
		raw = align(raw, cacheLine) + uint64(shards)*planeBytes(capacity, claimStride, resultStride, grain)
	}
	return align(raw, page)
}

func normalizeCapacity(n, page uint64) (uint64, error) {
	if n < page {
		n = page
	}
	if n > maxExtent {
		return 0, fmt.Errorf("%w: capacity", ErrFormat)
	}
	if n&(n-1) != 0 {
		n = 1 << bits.Len64(n)
	}
	if n > maxExtent {
		return 0, fmt.Errorf("%w: capacity", ErrFormat)
	}
	return n, nil
}

type pidNamespaceIdentity struct {
	dev uint64
	ino uint64
}

func (r *region) setGeometry(capacity uint64, claimStride, resultStride, grain uint32) {
	r.capacity = capacity
	r.claimStride = claimStride
	r.resultStride = resultStride
	r.planeGrain = grain
	if grain != 0 {
		r.planeShift = uint(bits.TrailingZeros32(grain))
		r.planeMask = capacity/uint64(grain) - 1
	}
}

func createRegion(cfg Config, claimStride, resultStride, grain uint32) (*region, error) {
	if err := procfsValidationProbe(); err != nil {
		return nil, err
	}
	page := uint64(os.Getpagesize())
	cap, err := normalizeCapacity(cfg.Capacity, page)
	if err != nil {
		return nil, err
	}
	if !validPlaneGeometry(cap, claimStride, resultStride, grain) {
		return nil, ErrFormat
	}
	if resultStride != 0 {
		geometry, ok := mpscGeometry(cfg.MPSCLayout)
		if !ok || geometry != (planeGeometry{claimStride: claimStride, resultStride: resultStride, grain: grain}) {
			return nil, ErrFormat
		}
	}
	ctrl := controlBytes(cap, 1, claimStride, resultStride, grain, page)
	fileSize := ctrl + cap
	fd, err := openBacking(cfg, fileSize)
	if err != nil {
		return nil, err
	}
	r, err := mapRegion(fd, ctrl, cap)
	if err != nil {
		syscall.Close(fd)
		return nil, err
	}
	namespace, err := currentPIDNamespace()
	if err != nil {
		r.close()
		return nil, err
	}
	r.controlLen = uintptr(ctrl)
	r.setGeometry(cap, claimStride, resultStride, grain)
	put64(r.memory, 0, formatMagic)
	put32(r.memory, 8, FormatVersion)
	put32(r.memory, 12, 1)
	put64(r.memory, 16, cap)
	put32(r.memory, 24, uint32(os.Getpid()))
	put32(r.memory, 32, claimStride)
	put32(r.memory, 36, resultStride)
	put32(r.memory, 40, grain)
	put64(r.memory, pidNamespaceDevOffset, namespace.dev)
	put64(r.memory, pidNamespaceInoOffset, namespace.ino)
	return r, nil
}

func attachRegion(fd int) (*region, error) {
	if err := procfsValidationProbe(); err != nil {
		return nil, err
	}
	dup, err := dupCloexec(fd)
	if err != nil {
		return nil, err
	}
	fail := func(e error) (*region, error) { syscall.Close(dup); return nil, e }
	st := syscall.Stat_t{}
	if err := syscall.Fstat(dup, &st); err != nil {
		return fail(err)
	}
	page := uint64(os.Getpagesize())
	if st.Size < int64(page) {
		return fail(ErrFormat)
	}
	probe, err := syscall.Mmap(dup, 0, int(page), syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		return fail(err)
	}
	magic, version := get64(probe, 0), get32(probe, 8)
	shards, cap := get32(probe, 12), get64(probe, 16)
	claimStride, resultStride, grain := get32(probe, 32), get32(probe, 36), get32(probe, 40)
	creatorNamespace := pidNamespaceIdentity{
		dev: get64(probe, pidNamespaceDevOffset),
		ino: get64(probe, pidNamespaceInoOffset),
	}
	syscall.Munmap(probe)
	if magic != formatMagic {
		return fail(ErrFormat)
	}
	if version != FormatVersion {
		return fail(fmt.Errorf("%w: got %d", ErrFormatVersion, version))
	}
	currentNamespace, err := currentPIDNamespace()
	if err != nil {
		return fail(err)
	}
	if creatorNamespace != currentNamespace {
		return fail(&PIDNamespaceError{
			CreatorDev: creatorNamespace.dev,
			CreatorIno: creatorNamespace.ino,
			CurrentDev: currentNamespace.dev,
			CurrentIno: currentNamespace.ino,
		})
	}
	if shards != 1 || cap == 0 || cap&(cap-1) != 0 || cap%page != 0 || cap > maxExtent {
		return fail(ErrFormat)
	}
	if !validPlaneGeometry(cap, claimStride, resultStride, grain) {
		return fail(ErrFormat)
	}
	ctrl := controlBytes(cap, shards, claimStride, resultStride, grain, page)
	if uint64(st.Size) != ctrl+cap {
		return fail(ErrFormat)
	}
	r, err := mapRegion(dup, ctrl, cap)
	if err != nil {
		return fail(err)
	}
	r.controlLen = uintptr(ctrl)
	r.setGeometry(cap, claimStride, resultStride, grain)
	return r, nil
}

func validPlaneGeometry(cap uint64, claimStride, resultStride, grain uint32) bool {
	if claimStride == 0 && resultStride == 0 {
		return grain == 0
	}
	if !isMPSCGeometry(claimStride, resultStride, grain) && !(claimStride == 8 && resultStride == 0 && grain == 64) {
		return false
	}
	g := uint64(grain)
	return g <= cap && cap%g == 0
}

func currentPIDNamespace() (pidNamespaceIdentity, error) {
	var st syscall.Stat_t
	if err := syscall.Stat("/proc/self/ns/pid", &st); err != nil {
		return pidNamespaceIdentity{}, err
	}
	return pidNamespaceIdentity{dev: uint64(st.Dev), ino: st.Ino}, nil
}

func dupCloexec(fd int) (int, error) {
	const fDupfdCloexec = 1030
	r0, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), fDupfdCloexec, 0)
	if errno != 0 {
		return -1, errno
	}
	return int(r0), nil
}

func openBacking(cfg Config, size uint64) (int, error) {
	var fd int
	var err error
	switch cfg.Backend {
	case BackendMemfd:
		r0, _, errno := syscall.Syscall(sysMemfdCreate, uintptr(unsafe.Pointer(unsafe.StringData("pgt-mpsc\x00"))), 0x0001|0x0002, 0)
		if errno != 0 {
			return -1, errno
		}
		fd = int(r0)
	case BackendSHM:
		if cfg.Name == "" {
			return -1, fmt.Errorf("%w: empty name", ErrFormat)
		}
		if strings.Contains(cfg.Name, "..") || strings.ContainsAny(cfg.Name, "/\\") {
			return -1, fmt.Errorf("%w: invalid shared memory name", ErrFormat)
		}
		fd, err = syscall.Open("/dev/shm/"+cfg.Name, syscall.O_RDWR|syscall.O_CREAT|syscall.O_EXCL|syscall.O_CLOEXEC, 0600)
		if err != nil {
			return -1, err
		}
	case BackendFile:
		if cfg.Name == "" {
			return -1, fmt.Errorf("%w: empty path", ErrFormat)
		}
		flags := syscall.O_RDWR | syscall.O_CREAT | syscall.O_CLOEXEC
		if cfg.AllowOverwrite {
			flags |= syscall.O_TRUNC
		} else {
			flags |= syscall.O_EXCL
		}
		fd, err = syscall.Open(cfg.Name, flags, 0600)
		if err != nil {
			return -1, err
		}
	default:
		return -1, fmt.Errorf("%w: backend", ErrFormat)
	}
	if err = syscall.Ftruncate(fd, int64(size)); err != nil {
		syscall.Close(fd)
		return -1, err
	}
	if !cfg.DisablePreallocate {
		if err = syscall.Fallocate(fd, 0, 0, int64(size)); err != nil {
			syscall.Close(fd)
			return -1, err
		}
	}
	if cfg.Backend == BackendMemfd {
		const fAddSeals = 1033
		const seals = 0x0002 | 0x0004 | 0x0001
		if _, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), fAddSeals, seals); errno != 0 {
			syscall.Close(fd)
			return -1, errno
		}
	}
	return fd, nil
}

func mapRegion(fd int, ctrl, cap uint64) (*region, error) {
	span := uintptr(ctrl + 2*cap)
	hole, err := syscall.Mmap(-1, 0, int(span), syscall.PROT_NONE, syscall.MAP_PRIVATE|syscall.MAP_ANON|0x4000)
	if err != nil {
		return nil, err
	}
	base := uintptr(unsafe.Pointer(&hole[0]))
	unmap := func() { _ = syscall.Munmap(hole) }
	fixed := func(at, length, off uintptr) error {
		p, _, e := syscall.Syscall6(syscall.SYS_MMAP, base+at, length, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED|syscall.MAP_FIXED, uintptr(fd), off)
		if e != 0 {
			return e
		}
		if p != base+at {
			return syscall.EFAULT
		}
		return nil
	}
	if err := fixed(0, uintptr(ctrl), 0); err != nil {
		unmap()
		return nil, err
	}
	if err := fixed(uintptr(ctrl), uintptr(cap), uintptr(ctrl)); err != nil {
		unmap()
		return nil, err
	}
	if err := fixed(uintptr(ctrl+cap), uintptr(cap), uintptr(ctrl)); err != nil {
		unmap()
		return nil, err
	}
	return &region{fd: fd, base: base, reservation: span, memory: hole}, nil
}

func (r *region) close() error {
	if r == nil || r.closed {
		return nil
	}
	r.closed = true
	e1 := syscall.Munmap(r.memory)
	e2 := syscall.Close(r.fd)
	r.memory = nil
	if e1 != nil {
		return e1
	}
	return e2
}

func (r *region) arena() []byte      { return r.memory[r.controlLen : r.controlLen+uintptr(2*r.capacity)] }
func (r *region) claimBase() uintptr { return uintptr(align(planeBase(1), cacheLine)) }
func (r *region) resultBase() uintptr {
	return r.claimBase() + uintptr(align((r.capacity/uint64(r.planeGrain))*uint64(r.claimStride), cacheLine))
}
func (r *region) fdDup() (int, error) { return dupCloexec(r.fd) }

func get32(b []byte, off uintptr) uint32    { return *(*uint32)(unsafe.Pointer(&b[off])) }
func get64(b []byte, off uintptr) uint64    { return *(*uint64)(unsafe.Pointer(&b[off])) }
func put32(b []byte, off uintptr, v uint32) { *(*uint32)(unsafe.Pointer(&b[off])) = v }
func put64(b []byte, off uintptr, v uint64) { *(*uint64)(unsafe.Pointer(&b[off])) = v }
