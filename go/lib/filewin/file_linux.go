//go:build linux && (amd64 || arm64)

package filewin

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"unsafe"

	"github.com/akalsi-org/polyglot-template/go/lib/atmc"
)

const (
	magic      uint64 = 0x7067745f6677696e // pgt_fwin
	version    uint32 = 1
	headerSize uint64 = 4096
	// maxReserveBytes bounds one address-space reservation.
	// Userspace addresses stop below 2^47 on four-level paging.
	maxReserveBytes = 1 << 42
	// headSuffix names the header file beside the data file.
	headSuffix = ".head"
	// sharedMemoryDir holds header files when HeadInSharedMemory is set.
	sharedMemoryDir = "/dev/shm"

	// cacheLine keeps independently written cursors apart.
	// Sharing one line makes every writer store invalidate the reader copy.
	cacheLine = 64

	// Line 0 holds identity that nobody writes after creation.
	offMagic   = 0
	offVersion = 8
	offHeader  = 12
	offEpoch   = 16

	// Line 1 belongs to the writer.
	offWritePos = 1*cacheLine + 0
	offWriteGen = 1*cacheLine + 8

	// Line 2 is reserved. Earlier versions published a reader cursor here.
	// Readers are anonymous now, so nothing writes this line.
	offReserved2 = 2 * cacheLine

	// Line 3 belongs to the growth, writeback, and eviction helpers.
	offCommitted  = 3*cacheLine + 0
	offSyncedPos  = 3*cacheLine + 8
	offEvictedPos = 3*cacheLine + 16

	futexWaitOp = 0
	futexWakeOp = 1
	madvWill    = 3
	madvDont    = 4
	// madvPopulateWrite establishes writable page-table entries now.
	// It removes the first-touch minor fault from the writer.
	madvPopulateWrite = 23
	// madvPopulateRead establishes read page-table entries now.
	madvPopulateRead = 22

	// syncFileRangeWrite starts writeback without waiting for it.
	syncFileRangeWrite = 2
	// syncFileRangeWaitAfter waits for the range to reach the device.
	syncFileRangeWaitBefore = 1
	syncFileRangeWaitAfter  = 4
	// fadviseDontneed drops clean page-cache pages for a range.
	// Dropping them keeps the page cache from filling memory.
	fadviseDontneed = 4
)

var (
	ErrFormat   = errors.New("filewin: invalid format")
	ErrMisuse   = errors.New("filewin: invalid operation")
	ErrClosed   = errors.New("filewin: closed")
	ErrTooLarge = errors.New("filewin: reservation exceeds window")
	ErrFull     = errors.New("filewin: live window is full")
	ErrBusy     = errors.New("filewin: no reader slot is free")
)

// Config controls file creation.
//
// The defaults hold a writer near 9 MiB and a reader near 5 MiB of resident
// memory, whatever the file grows to. Ahead plus RetainBytes bounds the
// writer's mapped residency, ReaderDropChunkBytes bounds a reader's, and
// KeepCachedBytes bounds the page cache the writer leaves behind.
type Config struct {
	Path string
	// Reserve is the address range mapped once for the whole log.
	// The file grows inside it, so no process ever remaps.
	// Address space is virtual, so a large value costs no memory.
	Reserve    uint64
	Extent     uint64
	Ahead      uint64
	MaxReserve uint64
	// SyncBytes is the unsynced byte count that starts background writeback.
	// Zero selects a default. DisableWriteback turns the helper off.
	SyncBytes        uint64
	DisableWriteback bool
	// KeepCachedBytes is how much consumed history stays in the page cache.
	// The eviction helper drops synced pages below that trailing window.
	// Without eviction the page cache grows until the kernel reclaims it.
	// Zero selects a default. DisableEvict keeps every page cached.
	KeepCachedBytes uint64
	DisableEvict    bool
	// PopulateChunkBytes bounds one MADV_POPULATE_WRITE call.
	// A short call holds the address-space lock briefly, so a reader fault
	// in the same process waits for one chunk and not the whole window.
	PopulateChunkBytes uint64
	// DropChunkBytes bounds one MADV_DONTNEED call the writer makes behind
	// the consumers. Small values keep resident set size low and each
	// translation-buffer flush short.
	DropChunkBytes uint64
	// ReaderDropChunkBytes is the same bound for a reader releasing its own
	// history. It defaults larger than DropChunkBytes: the reader competes
	// with its own drop helper for the address-space lock, so fewer and
	// larger calls cost less tail latency for nearly the same memory.
	ReaderDropChunkBytes uint64
	// DisableReaderDrop keeps every page a reader touched mapped in that
	// process. It trades resident memory for a smoother read path, because
	// the drop helper and the reader contend for the address-space lock.
	DisableReaderDrop bool
	// WorkBytes is how often the writer runs one housekeeping round: reap
	// completions, then stage whatever preallocation, page-table, writeback,
	// and eviction work has become due. One round costs one submission, so
	// this trades submission rate against how promptly the work is staged.
	WorkBytes uint64
	// WakeBytes is how often the writer bumps the counter that sleeping
	// readers wait on. It trades writer cost against wake-up delay: a
	// sleeping reader waits at most this many bytes, and the writer pays one
	// wake syscall per step, which grows with the number of sleepers.
	WakeBytes uint64
	// RetainBytes is how much history the writer keeps resident behind the
	// write cursor. It sets the writer's resident set size, and it is the
	// only reader budget: readers are anonymous, so nothing else can hold
	// history. A reader further behind still reads correct bytes, from the
	// device instead of from memory.
	RetainBytes uint64
	// EvictChunkBytes bounds one page-cache eviction step.
	// The step waits for the device, so a small value keeps that wait short.
	EvictChunkBytes uint64
	// HeadInSharedMemory keeps the header file in /dev/shm.
	// The path beside the data file becomes a symbolic link to it.
	// Cursor updates then never reach the data device.
	HeadInSharedMemory bool
}

// File is one attached data file plus its separate header mapping.
type File struct {
	path             string
	fd               int
	headFd           int
	header           []byte
	data             []byte
	reserve          uint64
	extent           uint64
	ahead            uint64
	maxReserve       uint64
	syncBytes        uint64
	disableWriteback bool
	keepCached       uint64
	disableEvict     bool
	retain           uint64
	wakeBytes        uint64
	workBytes        uint64
	noReaderDrop     bool
	populateChunk    uint64
	dropChunk        uint64
	readerDropChunk  uint64
	evictChunk       uint64
	page             uint64
	closed           bool
	localWriters     atomic.Int32
}

func pageSize() uint64 { return uint64(os.Getpagesize()) }

func alignDown(n, a uint64) uint64 { return n &^ (a - 1) }
func alignUp(n, a uint64) uint64   { return (n + a - 1) &^ (a - 1) }

func (c Config) normalize() (Config, error) {
	page := pageSize()
	if c.Path == "" {
		return Config{}, fmt.Errorf("%w: path", ErrFormat)
	}
	if c.Reserve == 0 {
		c.Reserve = 1 << 40
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
	if c.SyncBytes == 0 {
		c.SyncBytes = 8 << 20
	}
	if c.KeepCachedBytes == 0 {
		c.KeepCachedBytes = 8 << 20
	}
	if c.PopulateChunkBytes == 0 {
		c.PopulateChunkBytes = 1 << 20
	}
	if c.DropChunkBytes == 0 {
		c.DropChunkBytes = 1 << 20
	}
	if c.ReaderDropChunkBytes == 0 {
		c.ReaderDropChunkBytes = 512 << 10
	}
	if c.EvictChunkBytes == 0 {
		c.EvictChunkBytes = 8 << 20
	}
	if c.WorkBytes == 0 {
		c.WorkBytes = 1 << 20
	}
	c.WorkBytes = alignUp(c.WorkBytes, page)
	if c.WakeBytes == 0 {
		c.WakeBytes = 64 << 10
	}
	c.WakeBytes = alignUp(c.WakeBytes, page)
	if c.RetainBytes == 0 {
		c.RetainBytes = 1 << 20
	}
	c.RetainBytes = alignUp(c.RetainBytes, page)
	c.PopulateChunkBytes = alignUp(c.PopulateChunkBytes, page)
	c.DropChunkBytes = alignUp(c.DropChunkBytes, page)
	c.ReaderDropChunkBytes = alignUp(c.ReaderDropChunkBytes, page)
	c.EvictChunkBytes = alignUp(c.EvictChunkBytes, page)
	c.SyncBytes = alignUp(c.SyncBytes, page)
	c.KeepCachedBytes = alignUp(c.KeepCachedBytes, page)
	c.Reserve = alignUp(c.Reserve, page)
	c.Extent = alignUp(c.Extent, page)
	c.Ahead = alignUp(c.Ahead, page)
	if c.Reserve < page || c.Reserve > uint64(maxReserveBytes) ||
		c.Extent < page || c.MaxReserve > c.Reserve/2 {
		return Config{}, fmt.Errorf("%w: reserve", ErrFormat)
	}
	return c, nil
}

// Create creates the data file and its separate header file.
// The data file holds log bytes from offset zero.
func Create(cfg Config) (*File, error) {
	cfg, err := cfg.normalize()
	if err != nil {
		return nil, err
	}
	headPath, err := createHeadPath(cfg)
	if err != nil {
		return nil, err
	}
	headFd, err := syscall.Open(headPath, syscall.O_RDWR|syscall.O_CREAT|syscall.O_EXCL|syscall.O_CLOEXEC, 0600)
	if err != nil {
		_ = syscall.Unlink(cfg.Path + headSuffix)
		return nil, err
	}
	fd, err := syscall.Open(cfg.Path, syscall.O_RDWR|syscall.O_CREAT|syscall.O_EXCL|syscall.O_CLOEXEC, 0600)
	if err != nil {
		syscall.Close(headFd)
		_ = syscall.Unlink(headPath)
		_ = syscall.Unlink(cfg.Path + headSuffix)
		return nil, err
	}
	fail := func(err error) (*File, error) {
		syscall.Close(fd)
		syscall.Close(headFd)
		_ = syscall.Unlink(cfg.Path)
		_ = syscall.Unlink(headPath)
		_ = syscall.Unlink(cfg.Path + headSuffix)
		return nil, err
	}
	if err := syscall.Ftruncate(headFd, int64(headerSize)); err != nil {
		return fail(err)
	}
	initial := cfg.Extent
	if err := syscall.Ftruncate(fd, int64(initial)); err != nil {
		return fail(err)
	}
	if err := fallocate(fd, 0, int64(initial)); err != nil && !errors.Is(err, syscall.ENOSYS) && !errors.Is(err, syscall.EOPNOTSUPP) {
		return fail(err)
	}
	header, err := mapRange(headFd, 0, int(headerSize), syscall.PROT_READ|syscall.PROT_WRITE)
	if err != nil {
		return fail(err)
	}
	data, err := mapReserve(fd, cfg.Reserve)
	if err != nil {
		_ = syscall.Munmap(header)
		return fail(err)
	}
	put64(header, offMagic, magic)
	put32(header, offVersion, version)
	put32(header, offHeader, uint32(headerSize))
	put64(header, offCommitted, initial)
	put64(header, offEpoch, 1)
	return &File{
		path: cfg.Path, fd: fd, headFd: headFd, header: header, data: data,
		reserve: cfg.Reserve, extent: cfg.Extent, ahead: cfg.Ahead,
		maxReserve: cfg.MaxReserve, syncBytes: cfg.SyncBytes, disableWriteback: cfg.DisableWriteback,
		keepCached: cfg.KeepCachedBytes, disableEvict: cfg.DisableEvict,
		populateChunk: cfg.PopulateChunkBytes, dropChunk: cfg.DropChunkBytes,
		readerDropChunk: cfg.ReaderDropChunkBytes,
		evictChunk:      cfg.EvictChunkBytes, retain: cfg.RetainBytes, wakeBytes: cfg.WakeBytes, workBytes: cfg.WorkBytes,
		noReaderDrop: cfg.DisableReaderDrop,
		page:         pageSize(),
	}, nil
}

// createHeadPath returns the header path and links it beside the data file.
// A shared-memory header keeps cursor writes off the data device.
func createHeadPath(cfg Config) (string, error) {
	link := cfg.Path + headSuffix
	if !cfg.HeadInSharedMemory {
		return link, nil
	}
	absolute, err := filepath.Abs(cfg.Path)
	if err != nil {
		return "", err
	}
	name := strings.ReplaceAll(strings.TrimPrefix(absolute, "/"), "/", "_")
	target := filepath.Join(sharedMemoryDir, name+headSuffix)
	if err := syscall.Symlink(target, link); err != nil {
		return "", err
	}
	return target, nil
}

// Open attaches to an existing data file and its header file.
func Open(path string, cfg Config) (*File, error) {
	cfg.Path = path
	cfg, err := cfg.normalize()
	if err != nil {
		return nil, err
	}
	headFd, err := syscall.Open(path+headSuffix, syscall.O_RDWR|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_CLOEXEC, 0)
	if err != nil {
		syscall.Close(headFd)
		return nil, err
	}
	header, err := mapRange(headFd, 0, int(headerSize), syscall.PROT_READ|syscall.PROT_WRITE)
	if err != nil {
		syscall.Close(fd)
		syscall.Close(headFd)
		return nil, err
	}
	if get64(header, offMagic) != magic || get32(header, offVersion) != version || get32(header, offHeader) != uint32(headerSize) {
		_ = syscall.Munmap(header)
		syscall.Close(fd)
		syscall.Close(headFd)
		return nil, ErrFormat
	}
	data, err := mapReserve(fd, cfg.Reserve)
	if err != nil {
		_ = syscall.Munmap(header)
		syscall.Close(fd)
		syscall.Close(headFd)
		return nil, err
	}
	return &File{
		path: path, fd: fd, headFd: headFd, header: header, data: data,
		reserve: cfg.Reserve, extent: cfg.Extent, ahead: cfg.Ahead,
		maxReserve: cfg.MaxReserve, syncBytes: cfg.SyncBytes, disableWriteback: cfg.DisableWriteback,
		keepCached: cfg.KeepCachedBytes, disableEvict: cfg.DisableEvict,
		populateChunk: cfg.PopulateChunkBytes, dropChunk: cfg.DropChunkBytes,
		readerDropChunk: cfg.ReaderDropChunkBytes,
		evictChunk:      cfg.EvictChunkBytes, retain: cfg.RetainBytes, wakeBytes: cfg.WakeBytes, workBytes: cfg.WorkBytes,
		noReaderDrop: cfg.DisableReaderDrop,
		page:         pageSize(),
	}, nil
}

// Close unmaps the header and closes the descriptor.
func (f *File) Close() error {
	if f == nil || f.closed {
		return nil
	}
	f.closed = true
	err := syscall.Munmap(f.data)
	if unmapErr := syscall.Munmap(f.header); err == nil {
		err = unmapErr
	}
	f.data = nil
	if closeErr := syscall.Close(f.fd); err == nil {
		err = closeErr
	}
	if closeErr := syscall.Close(f.headFd); err == nil {
		err = closeErr
	}
	f.header = nil
	return err
}

func (f *File) Extent() uint64 {
	if f == nil {
		return 0
	}
	return f.extent
}

func (f *File) writePos() *uint64 { return (*uint64)(unsafe.Pointer(&f.header[offWritePos])) }
func (f *File) writeGen() *uint32 { return (*uint32)(unsafe.Pointer(&f.header[offWriteGen])) }

func (f *File) committed() *uint64 {
	return (*uint64)(unsafe.Pointer(&f.header[offCommitted]))
}

func (f *File) syncedPos() *uint64 {
	return (*uint64)(unsafe.Pointer(&f.header[offSyncedPos]))
}

func (f *File) evictedPos() *uint64 {
	return (*uint64)(unsafe.Pointer(&f.header[offEvictedPos]))
}

// dataFileOff maps a log position to its data-file offset.
// The data file starts at zero, so the two are equal.
func (f *File) dataFileOff(dataPos uint64) uint64 { return dataPos }

func (f *File) addLocalWriter()    { f.localWriters.Add(1) }
func (f *File) removeLocalWriter() { f.localWriters.Add(-1) }

// dropFloor is the highest position the helpers may release.
// It follows the write cursor alone. Readers are anonymous: they publish
// nothing, they are never counted, and they never hold the floor.
// Releasing history costs a lagging reader page faults, never data, because
// the bytes stay in the file.
func (f *File) dropFloor() uint64 {
	write := atmc.LoadAcquireU64(f.writePos())
	if write < f.retain {
		return 0
	}
	return write - f.retain
}

// WritePos returns the published write cursor.
func (f *File) WritePos() uint64 {
	if f == nil || f.closed {
		return 0
	}
	return atmc.LoadAcquireU64(f.writePos())
}

// Committed returns the file length the growth helper has published.
func (f *File) Committed() uint64 {
	if f == nil || f.closed {
		return 0
	}
	return atmc.LoadAcquireU64(f.committed())
}

// SyncedPos returns the offset whose writeback this process has started.
func (f *File) SyncedPos() uint64 {
	if f == nil || f.closed {
		return 0
	}
	return atmc.LoadAcquireU64(f.syncedPos())
}

// Reserve returns the mapped address range for the log.
func (f *File) Reserve() uint64 {
	if f == nil {
		return 0
	}
	return f.reserve
}

func mapRange(fd int, off uint64, length int, prot int) ([]byte, error) {
	return syscall.Mmap(fd, int64(off), length, prot, syscall.MAP_SHARED)
}

// mapReserve maps one file range over a private address reservation.
// The reservation gives the slice, so no integer becomes a pointer here.
// Mapping past the end of the file is allowed. Growth then needs no remap.
func mapReserve(fd int, length uint64) ([]byte, error) {
	hole, err := syscall.Mmap(-1, 0, int(length), syscall.PROT_NONE,
		syscall.MAP_PRIVATE|syscall.MAP_ANON|syscall.MAP_NORESERVE)
	if err != nil {
		return nil, err
	}
	base := uintptr(unsafe.Pointer(&hole[0]))
	_, _, errno := syscall.Syscall6(
		syscall.SYS_MMAP, base, uintptr(length),
		uintptr(syscall.PROT_READ|syscall.PROT_WRITE),
		uintptr(syscall.MAP_SHARED|syscall.MAP_FIXED), uintptr(fd), 0)
	if errno != 0 {
		_ = syscall.Munmap(hole)
		return nil, errno
	}
	return hole, nil
}

// fadvise applies one advice to a file range.
func fadvise(fd int, off, length int64, advice int) error {
	_, _, errno := syscall.Syscall6(
		sysFadvise64, uintptr(fd), uintptr(off), uintptr(length), uintptr(advice), 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}

// syncFileRange starts writeback for one range and does not wait.
func syncFileRange(fd int, off, nbytes int64, flags int) error {
	_, _, errno := syscall.Syscall6(
		sysSyncFileRange, uintptr(fd), uintptr(off), uintptr(nbytes), uintptr(flags), 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
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
	_, _, _ = syscall.Syscall6(sysFutex, uintptr(unsafe.Pointer(ptr)), futexWakeOp, ^uintptr(0)>>1, 0, 0, 0)
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
