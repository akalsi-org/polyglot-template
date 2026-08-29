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
	// offReserved1 held a wake generation in earlier versions.
	offReserved1 = 1*cacheLine + 8

	// Line 2 is reserved. Earlier versions published a reader cursor here.
	// Readers are anonymous now, so nothing writes this line.
	offReserved2 = 2 * cacheLine

	// Line 3 belongs to the growth, writeback, and eviction helpers.
	offCommitted  = 3*cacheLine + 0
	offSyncedPos  = 3*cacheLine + 8
	offEvictedPos = 3*cacheLine + 16

	madvWill = 3
	madvDont = 4
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

	// mapFixedNoreplace demands an address and fails when it is taken.
	// MAP_FIXED would take it, unmapping whatever was there without a word.
	mapFixedNoreplace = 0x100000
	// mapAttempts bounds the retries when another thread takes the address
	// between the reservation being released and the file being mapped.
	mapAttempts = 8
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
	//
	// A short call holds the address-space lock briefly, so a reader fault in
	// the same process waits for one chunk and not the whole window. The
	// default sits where the syscall cost has amortised away: measured per
	// page this call costs 4630ns at 4 KiB, 487ns at 64 KiB and 450ns at
	// 1 MiB, so past 64 KiB the work is all per-page and a smaller chunk
	// buys smoothness for almost nothing.
	PopulateChunkBytes uint64
	// DisablePopulate leaves the writer to fault pages in as it reaches them.
	//
	// The kernel allocates and zeroes a page either way. This only decides
	// whether that happens ahead of the writer, in a batch, or under it, one
	// page at a time. Neither is free and the choice is a real trade, which
	// is why it is a knob and not a default. Measured at 4 KiB records with
	// writeback off:
	//
	//   populate ahead   2.80 GB/s   p50   175ns   p99 13.9us   max 14.8us
	//   fault under      2.83 GB/s   p50  1385ns   p99  2.3us   max  5.3us
	//
	// Populating wins the median eight times over, because the writer never
	// takes the fault. Faulting wins the tail six times over, because the
	// work arrives one page at a time instead of in a batch that can collide
	// with the writer over the address-space lock. Throughput is the same.
	//
	// Populate ahead for a low median. Fault for a low tail.
	DisablePopulate bool
	// DropChunkBytes bounds one MADV_DONTNEED call the writer makes behind
	// the consumers.
	//
	// This one does not want to be small. The translation-buffer flush
	// amortises much later than the populate call: measured per page it
	// costs 2005ns at 4 KiB, 339ns at 64 KiB, 170ns at 256 KiB and 135ns at
	// 1 MiB, and is flat above that. The default sits at that knee.
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
	workBytes        uint64
	noReaderDrop     bool
	noPopulate       bool
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
		c.PopulateChunkBytes = 256 << 10
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
		evictChunk:      cfg.EvictChunkBytes, retain: cfg.RetainBytes, workBytes: cfg.WorkBytes,
		noReaderDrop: cfg.DisableReaderDrop, noPopulate: cfg.DisablePopulate,
		page: pageSize(),
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
		evictChunk:      cfg.EvictChunkBytes, retain: cfg.RetainBytes, workBytes: cfg.WorkBytes,
		noReaderDrop: cfg.DisableReaderDrop, noPopulate: cfg.DisablePopulate,
		page: pageSize(),
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

// mapReserve maps one file range at an address the kernel chose for us.
//
// Mapping past the end of the file is allowed, which is why growth needs no
// remap later.
//
// The address comes from an anonymous reservation, whose slice is what this
// returns: taking the slice from a real mapping is what keeps an integer from
// becoming a pointer here, and go vet from needing an exception.
//
// The reservation is then released and the file mapped in its place with
// MAP_FIXED_NOREPLACE. MAP_FIXED would be shorter, and is what this did
// before: it takes the address whatever is there, unmapping it without a
// word. That is safe only for as long as the reasoning about who owns the
// range stays true, and it fails silently and catastrophically when it stops
// being true. MAP_FIXED_NOREPLACE cannot do that. It refuses an address that
// is taken, which is also why the reservation has to go first: the flag will
// not map over our own reservation either.
//
// Releasing the reservation opens a window where another thread in this
// process could take the address. That is what the flag is for. The mapping
// fails with EEXIST, and we start again somewhere else.
func mapReserve(fd int, length uint64) ([]byte, error) {
	var err error
	for attempt := 0; attempt < mapAttempts; attempt++ {
		var hole []byte
		hole, err = syscall.Mmap(-1, 0, int(length), syscall.PROT_NONE,
			syscall.MAP_PRIVATE|syscall.MAP_ANON|syscall.MAP_NORESERVE)
		if err != nil {
			return nil, err
		}
		base := uintptr(unsafe.Pointer(&hole[0]))
		if _, _, errno := syscall.Syscall(syscall.SYS_MUNMAP, base, uintptr(length), 0); errno != 0 {
			return nil, errno
		}
		got, _, errno := syscall.Syscall6(
			syscall.SYS_MMAP, base, uintptr(length),
			uintptr(syscall.PROT_READ|syscall.PROT_WRITE),
			uintptr(syscall.MAP_SHARED|mapFixedNoreplace), uintptr(fd), 0)
		if errno != 0 {
			// Somebody took the address in the window above, or this kernel
			// refused for another reason. Either way, try a different one.
			err = errno
			continue
		}
		if got != base {
			// A kernel older than 4.17 does not know the flag and ignores
			// it, which leaves the address a hint. The file landed somewhere
			// else, so the slice would describe the wrong memory.
			_, _, _ = syscall.Syscall(syscall.SYS_MUNMAP, got, uintptr(length), 0)
			err = ErrMisuse
			continue
		}
		return hole, nil
	}
	return nil, err
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
