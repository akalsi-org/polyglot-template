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

	"github.com/akalsi-org/polyglot-template/go/lib/hostcpu"
	"github.com/akalsi-org/polyglot-template/go/lib/internal/shmregion"
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
	mapping       *shmregion.Mapping
	control       []byte
	mirroredArena []byte
	capacity      uint64
	controlLen    uintptr
	claimStride   uint32
	resultStride  uint32
	planeGrain    uint32
	planeShift    uint
	planeMask     uint64
	closed        bool
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

func mappingOptions(cfg Config, layout shmregion.Layout) (shmregion.CreateOptions, error) {
	var backend shmregion.Backend
	switch cfg.Backend {
	case BackendMemfd:
		backend = shmregion.BackendMemfd
	case BackendSHM:
		if cfg.Name == "" {
			return shmregion.CreateOptions{}, fmt.Errorf("%w: empty name", ErrFormat)
		}
		if strings.Contains(cfg.Name, "..") || strings.ContainsAny(cfg.Name, "/\\") {
			return shmregion.CreateOptions{}, fmt.Errorf("%w: invalid shared memory name", ErrFormat)
		}
		backend = shmregion.BackendSHM
	case BackendFile:
		if cfg.Name == "" {
			return shmregion.CreateOptions{}, fmt.Errorf("%w: empty path", ErrFormat)
		}
		backend = shmregion.BackendFile
	default:
		return shmregion.CreateOptions{}, fmt.Errorf("%w: backend", ErrFormat)
	}
	return shmregion.CreateOptions{
		Backend:            backend,
		Name:               cfg.Name,
		Layout:             layout,
		MemfdName:          "pgt-mpsc",
		DisablePreallocate: cfg.DisablePreallocate,
		AllowOverwrite:     cfg.AllowOverwrite,
	}, nil
}

func newRegion(mapping *shmregion.Mapping) *region {
	return &region{
		mapping:       mapping,
		control:       mapping.Control(),
		mirroredArena: mapping.Arena(),
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
	options, err := mappingOptions(cfg, shmregion.Layout{ControlBytes: ctrl, ArenaBytes: cap})
	if err != nil {
		return nil, err
	}
	mapping, err := shmregion.Create(options)
	if err != nil {
		return nil, err
	}
	r := newRegion(mapping)
	namespace, err := hostcpu.CurrentPIDNamespaceIdentity()
	if err != nil {
		r.close()
		return nil, err
	}
	r.controlLen = uintptr(ctrl)
	r.setGeometry(cap, claimStride, resultStride, grain)
	put64(r.control, 0, formatMagic)
	put32(r.control, 8, FormatVersion)
	put32(r.control, 12, 1)
	put64(r.control, 16, cap)
	put32(r.control, 24, uint32(os.Getpid()))
	put32(r.control, 32, claimStride)
	put32(r.control, 36, resultStride)
	put32(r.control, 40, grain)
	put64(r.control, pidNamespaceDevOffset, namespace.Dev)
	put64(r.control, pidNamespaceInoOffset, namespace.Ino)
	return r, nil
}

func attachRegion(fd int) (*region, error) {
	if err := procfsValidationProbe(); err != nil {
		return nil, err
	}
	page := uint64(os.Getpagesize())
	probe := make([]byte, int(page))
	fileSize, err := shmregion.Probe(fd, probe)
	if errors.Is(err, shmregion.ErrProbeTooSmall) {
		return nil, ErrFormat
	}
	if err != nil {
		return nil, err
	}
	magic, version := get64(probe, 0), get32(probe, 8)
	shards, cap := get32(probe, 12), get64(probe, 16)
	claimStride, resultStride, grain := get32(probe, 32), get32(probe, 36), get32(probe, 40)
	creatorNamespace := hostcpu.PIDNamespaceIdentity{
		Dev: get64(probe, pidNamespaceDevOffset),
		Ino: get64(probe, pidNamespaceInoOffset),
	}
	if magic != formatMagic {
		return nil, ErrFormat
	}
	if version != FormatVersion {
		return nil, fmt.Errorf("%w: got %d", ErrFormatVersion, version)
	}
	currentNamespace, err := hostcpu.CurrentPIDNamespaceIdentity()
	if err != nil {
		return nil, err
	}
	if creatorNamespace != currentNamespace {
		return nil, &PIDNamespaceError{
			CreatorDev: creatorNamespace.Dev,
			CreatorIno: creatorNamespace.Ino,
			CurrentDev: currentNamespace.Dev,
			CurrentIno: currentNamespace.Ino,
		}
	}
	if shards != 1 || cap == 0 || cap&(cap-1) != 0 || cap%page != 0 || cap > maxExtent {
		return nil, ErrFormat
	}
	if !validPlaneGeometry(cap, claimStride, resultStride, grain) {
		return nil, ErrFormat
	}
	ctrl := controlBytes(cap, shards, claimStride, resultStride, grain, page)
	if fileSize != ctrl+cap {
		return nil, ErrFormat
	}
	mapping, err := shmregion.Attach(fd, shmregion.Layout{ControlBytes: ctrl, ArenaBytes: cap})
	if errors.Is(err, syscall.EINVAL) {
		return nil, ErrFormat
	}
	if err != nil {
		return nil, err
	}
	r := newRegion(mapping)
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

func (r *region) close() error {
	if r == nil || r.closed {
		return nil
	}
	r.closed = true
	err := r.mapping.Close()
	r.control = nil
	r.mirroredArena = nil
	return err
}

func (r *region) arena() []byte      { return r.mirroredArena }
func (r *region) claimBase() uintptr { return uintptr(align(planeBase(1), cacheLine)) }
func (r *region) resultBase() uintptr {
	return r.claimBase() + uintptr(align((r.capacity/uint64(r.planeGrain))*uint64(r.claimStride), cacheLine))
}
func (r *region) fdDup() (int, error) { return r.mapping.DupFD() }

func get32(b []byte, off uintptr) uint32    { return *(*uint32)(unsafe.Pointer(&b[off])) }
func get64(b []byte, off uintptr) uint64    { return *(*uint64)(unsafe.Pointer(&b[off])) }
func put32(b []byte, off uintptr, v uint32) { *(*uint32)(unsafe.Pointer(&b[off])) = v }
func put64(b []byte, off uintptr, v uint64) { *(*uint64)(unsafe.Pointer(&b[off])) = v }
