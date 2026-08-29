//go:build linux && (amd64 || arm64)

package uring

import (
	"errors"
	"syscall"
	"unsafe"

	"github.com/akalsi-org/polyglot-template/go/lib/atmc"
)

// Operation codes this package can stage.
const (
	OpSyncFileRange = 8
	OpFallocate     = 17
	OpFadvise       = 24
	OpMadvise       = 25
)

var (
	// ErrUnsupported reports that this kernel has no such operation.
	ErrUnsupported = errors.New("uring: operation not supported")
	// ErrClosed reports use of a closed ring.
	ErrClosed = errors.New("uring: closed")
)

const (
	offSQRing = 0
	offCQRing = 0x8000000
	offSQEs   = 0x10000000

	enterGetEvents = 1
	featSingleMmap = 1 << 0

	// flagIOLink makes the next staged entry wait for this one, and run only
	// if it succeeded. It lets a caller express "write this range back, then
	// drop it" without ever blocking to sequence the two itself.
	flagIOLink = 1 << 2

	// flagAsync forces an operation to a kernel worker instead of letting
	// the kernel attempt it inline during submission.
	//
	// io_uring runs what it can inline, and for advisory memory work that
	// means the whole madvise happens inside io_uring_enter on the calling
	// thread. Submission then costs what the work costs, and the ring has
	// amortised nothing: it has batched several synchronous calls into one
	// syscall. This flag is what makes the work actually asynchronous.
	flagAsync = 1 << 4

	registerProbe = 8
	probeOps      = 256
	opSupported   = 1 << 0
)

// sqe mirrors struct io_uring_sqe.
type sqe struct {
	opcode      uint8
	flags       uint8
	ioprio      uint16
	fd          int32
	off         uint64
	addr        uint64
	length      uint32
	opFlags     uint32
	userData    uint64
	bufIndex    uint16
	personality uint16
	spliceFdIn  int32
	pad         [2]uint64
}

// cqe mirrors struct io_uring_cqe.
type cqe struct {
	userData uint64
	res      int32
	flags    uint32
}

// params mirrors struct io_uring_params.
type params struct {
	sqEntries    uint32
	cqEntries    uint32
	flags        uint32
	sqThreadCPU  uint32
	sqThreadIdle uint32
	features     uint32
	wqFd         uint32
	resv         [3]uint32

	sqOffHead        uint32
	sqOffTail        uint32
	sqOffRingMask    uint32
	sqOffRingEntries uint32
	sqOffFlags       uint32
	sqOffDropped     uint32
	sqOffArray       uint32
	sqOffResv1       uint32
	sqOffResv2       uint64

	cqOffHead        uint32
	cqOffTail        uint32
	cqOffRingMask    uint32
	cqOffRingEntries uint32
	cqOffOverflow    uint32
	cqOffCqes        uint32
	cqOffFlags       uint32
	cqOffResv1       uint32
	cqOffResv2       uint64
}

// Ring is one submission and completion queue pair.
type Ring struct {
	fd      int
	sqMap   []byte
	cqMap   []byte
	sqeMap  []byte
	oneMap  bool
	entries uint32
	closed  bool

	sqHead  *uint32
	sqTail  *uint32
	sqMask  uint32
	sqArray []uint32
	sqes    []sqe
	sqLocal uint32
	staged  uint32

	cqHead *uint32
	cqTail *uint32
	cqMask uint32
	cqes   []cqe

	// Submits counts operations handed to the kernel.
	Submits uint64
	// Dropped counts operations refused because the ring was full.
	Dropped uint64
	// Failures counts completions the kernel reported as errors, plus
	// submission calls that failed.
	Failures uint64

	// Async forces staged operations onto kernel workers rather than letting
	// the kernel run them inline during submission. See flagAsync.
	Async bool

	// OnComplete, when set, receives every completion during Reap.
	// A caller that must know an operation actually happened, rather than
	// merely that it was staged, learns it here.
	OnComplete func(tag uint64, res int32)
}

// New creates a ring with at least the requested number of entries.
// The kernel rounds the count up to a power of two.
func New(entries uint32) (*Ring, error) {
	if entries == 0 {
		entries = 64
	}
	var p params
	fd, _, errno := syscall.Syscall(sysSetup, uintptr(entries),
		uintptr(unsafe.Pointer(&p)), 0)
	if errno != 0 {
		return nil, errno
	}
	r := &Ring{fd: int(fd), entries: p.sqEntries}
	if err := r.mapRings(&p); err != nil {
		_ = r.Close()
		return nil, err
	}
	return r, nil
}

// Available reports whether this kernel can create a ring at all.
func Available() bool {
	r, err := New(2)
	if err != nil {
		return false
	}
	_ = r.Close()
	return true
}

// Supported reports whether this kernel implements an operation.
// Callers must fall back to the synchronous syscall when it does not.
func (r *Ring) Supported(op uint8) bool {
	var buf [16 + probeOps*8]byte
	_, _, errno := syscall.Syscall6(sysRegister, uintptr(r.fd), registerProbe,
		uintptr(unsafe.Pointer(&buf[0])), probeOps, 0, 0)
	if errno != 0 {
		return false
	}
	lastOp := buf[0]
	if op > lastOp {
		return false
	}
	// Each probe op is 8 bytes: op, resv, flags(u16), resv2(u32).
	flags := uint16(buf[16+int(op)*8+2]) | uint16(buf[16+int(op)*8+3])<<8
	return flags&opSupported != 0
}

func mapRange(fd int, offset int64, length int) ([]byte, error) {
	return syscall.Mmap(fd, offset, length,
		syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED|syscall.MAP_POPULATE)
}

func (r *Ring) mapRings(p *params) error {
	sqBytes := int(p.sqOffArray) + int(p.sqEntries)*4
	cqBytes := int(p.cqOffCqes) + int(p.cqEntries)*int(unsafe.Sizeof(cqe{}))
	r.oneMap = p.features&featSingleMmap != 0
	if r.oneMap {
		// One mapping backs both rings. Size it for the larger of the two.
		if cqBytes > sqBytes {
			sqBytes = cqBytes
		}
	}
	sq, err := mapRange(r.fd, offSQRing, sqBytes)
	if err != nil {
		return err
	}
	r.sqMap = sq
	if r.oneMap {
		r.cqMap = sq
	} else {
		cq, err := mapRange(r.fd, offCQRing, cqBytes)
		if err != nil {
			return err
		}
		r.cqMap = cq
	}
	sqes, err := mapRange(r.fd, offSQEs, int(p.sqEntries)*int(unsafe.Sizeof(sqe{})))
	if err != nil {
		return err
	}
	r.sqeMap = sqes

	r.sqHead = (*uint32)(unsafe.Pointer(&r.sqMap[p.sqOffHead]))
	r.sqTail = (*uint32)(unsafe.Pointer(&r.sqMap[p.sqOffTail]))
	r.sqMask = *(*uint32)(unsafe.Pointer(&r.sqMap[p.sqOffRingMask]))
	r.sqArray = unsafe.Slice((*uint32)(unsafe.Pointer(&r.sqMap[p.sqOffArray])), int(p.sqEntries))
	r.sqes = unsafe.Slice((*sqe)(unsafe.Pointer(&r.sqeMap[0])), int(p.sqEntries))

	r.cqHead = (*uint32)(unsafe.Pointer(&r.cqMap[p.cqOffHead]))
	r.cqTail = (*uint32)(unsafe.Pointer(&r.cqMap[p.cqOffTail]))
	r.cqMask = *(*uint32)(unsafe.Pointer(&r.cqMap[p.cqOffRingMask]))
	r.cqes = unsafe.Slice((*cqe)(unsafe.Pointer(&r.cqMap[p.cqOffCqes])), int(p.cqEntries))

	r.sqLocal = atmc.LoadAcquireU32(r.sqTail)
	return nil
}

// Entries reports the ring's submission capacity.
func (r *Ring) Entries() uint32 { return r.entries }

// Free reports how many submission slots are open.
// Only the goroutine that stages may call it: it reads that goroutine's own
// view of the tail.
func (r *Ring) Free() uint32 {
	return r.entries - (r.sqLocal - atmc.LoadAcquireU32(r.sqHead))
}

// Inflight reports operations the kernel still owes a completion.
// It reads only shared ring words, so either goroutine may call it.
func (r *Ring) Inflight() uint32 {
	return atmc.LoadAcquireU32(r.sqTail) - atmc.LoadAcquireU32(r.cqHead)
}

// Pending reports entries staged but not yet handed to the kernel.
// It reads only shared ring words, so either goroutine may call it.
func (r *Ring) Pending() uint32 {
	return atmc.LoadAcquireU32(r.sqTail) - atmc.LoadAcquireU32(r.sqHead)
}

// push stages one entry. It reports false when the ring is full.
// Tag sets the identifier reported to OnComplete for the last staged entry.
func (r *Ring) Tag(tag uint64) {
	if r.staged == 0 {
		return
	}
	r.sqes[(r.sqLocal-1)&r.sqMask].userData = tag
}

func (r *Ring) push(op uint8, fd int32, addr uint64, length uint32, off uint64, opFlags uint32) bool {
	if r.closed || r.Free() == 0 {
		r.Dropped++
		return false
	}
	idx := r.sqLocal & r.sqMask
	e := &r.sqes[idx]
	*e = sqe{}
	e.opcode = op
	e.fd = fd
	e.addr = addr
	e.length = length
	e.off = off
	e.opFlags = opFlags
	if r.Async {
		e.flags |= flagAsync
	}
	r.sqArray[idx] = idx
	r.sqLocal++
	r.staged++
	// Publish the entry as it is staged, not at submission. That lets the
	// thread that stages be a different one from the thread that submits:
	// staging is a handful of memory writes and submission is a system call,
	// and on the measured kernel the call costs what the work costs.
	atmc.StoreReleaseU32(r.sqTail, r.sqLocal)
	return true
}

// Madvise stages madvise over b. It reports false when the ring is full.
func (r *Ring) Madvise(b []byte, advice int) bool {
	if len(b) == 0 {
		return true
	}
	// io_madvise_prep rejects a nonzero off, so the length goes in len.
	return r.push(OpMadvise, -1, uint64(uintptr(unsafe.Pointer(&b[0]))),
		uint32(len(b)), 0, uint32(advice))
}

// Fadvise stages posix_fadvise over a file range.
func (r *Ring) Fadvise(fd int, off, length int64, advice int) bool {
	return r.push(OpFadvise, int32(fd), 0, uint32(length), uint64(off), uint32(advice))
}

// SyncFileRange stages writeback over a file range.
func (r *Ring) SyncFileRange(fd int, off, length int64, flags int) bool {
	return r.push(OpSyncFileRange, int32(fd), 0, uint32(length), uint64(off), uint32(flags))
}

// Fallocate stages preallocation over a file range.
// Mode zero also extends the file, so no truncate is needed.
func (r *Ring) Fallocate(fd int, mode int, off, length int64) bool {
	return r.push(OpFallocate, int32(fd), uint64(length), 0, uint64(off), uint32(mode))
}

// Link ties the last staged operation to the next one. The kernel runs them
// in order and skips the rest of the chain if one fails.
func (r *Ring) Link() {
	if r.staged == 0 {
		return
	}
	r.sqes[(r.sqLocal-1)&r.sqMask].flags |= flagIOLink
}

// Submit stages nothing and hands whatever is staged to the kernel.
//
// It may be called from a different goroutine than the one that stages, and
// that is the point. Staging writes to the submission ring; this makes the
// system call. Only this call is expensive, so a caller that must not block
// stages on its own thread and leaves this to another.
//
// The caller that stages owns the submission ring and its tail. This call
// owns nothing but the syscall, so the two never write the same word.
func (r *Ring) Submit() error {
	if r.closed {
		return ErrClosed
	}
	head := atmc.LoadAcquireU32(r.sqHead)
	tail := atmc.LoadAcquireU32(r.sqTail)
	n := tail - head
	if n == 0 {
		return nil
	}
	_, _, errno := syscall.Syscall6(sysEnter, uintptr(r.fd), uintptr(n), 0, 0, 0, 0)
	if errno != 0 {
		r.Failures++
		return errno
	}
	r.Submits += uint64(n)
	return nil
}

// Reap consumes finished completions. It never blocks and makes no syscall.
func (r *Ring) Reap() int {
	if r.closed {
		return 0
	}
	head := atmc.LoadRelaxedU32(r.cqHead)
	tail := atmc.LoadAcquireU32(r.cqTail)
	n := 0
	for head != tail {
		c := &r.cqes[head&r.cqMask]
		if c.res < 0 {
			r.Failures++
		}
		if r.OnComplete != nil {
			r.OnComplete(c.userData, c.res)
		}
		head++
		n++
	}
	if n != 0 {
		atmc.StoreReleaseU32(r.cqHead, head)
	}
	return n
}

// Drain waits until every submitted operation has completed.
func (r *Ring) Drain() {
	if r.closed {
		return
	}
	_ = r.Submit()
	for r.Inflight() != 0 {
		if r.Reap() != 0 {
			continue
		}
		if r.Inflight() == 0 {
			return
		}
		_, _, errno := syscall.Syscall6(sysEnter, uintptr(r.fd), 0, 1, enterGetEvents, 0, 0)
		if errno != 0 {
			return
		}
	}
}

// Close drains the ring and releases it.
func (r *Ring) Close() error {
	if r == nil || r.closed {
		return nil
	}
	r.Drain()
	r.closed = true
	var err error
	unmap := func(b []byte) {
		if b == nil {
			return
		}
		if e := syscall.Munmap(b); err == nil {
			err = e
		}
	}
	unmap(r.sqeMap)
	if !r.oneMap {
		unmap(r.cqMap)
	}
	unmap(r.sqMap)
	r.sqMap, r.cqMap, r.sqeMap = nil, nil, nil
	if e := syscall.Close(r.fd); err == nil {
		err = e
	}
	return err
}
