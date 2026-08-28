//go:build linux && (amd64 || arm64)

package mpsc

import (
	"errors"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/akalsi-org/polyglot-template/go/lib/hostcpu"
)

func TestSPSCShortCommitReclaimsReservedSlack(t *testing.T) {
	q, err := CreateSPSC(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	producer, err := q.AttachProducer()
	if err != nil {
		t.Fatal(err)
	}
	consumer, err := q.AttachConsumer()
	if err != nil {
		t.Fatal(err)
	}
	first, err := producer.Reserve(120)
	if err != nil {
		t.Fatal(err)
	}
	firstPos := first.pos
	copy(first.Bytes(), "short")
	if err := producer.Commit(first, 5); err != nil {
		t.Fatal(err)
	}
	second, err := producer.Reserve(1)
	if err != nil {
		t.Fatal(err)
	}
	if second.pos != firstPos+64 {
		t.Fatalf("second position: got %d want %d", second.pos, firstPos+64)
	}
	second.Bytes()[0] = 9
	if err := producer.Commit(second, 1); err != nil {
		t.Fatal(err)
	}
	record, ok, err := consumer.Peek()
	if err != nil || !ok || string(record.UnsafeBytes()) != "short" {
		t.Fatalf("first peek: %v %v", ok, err)
	}
	if err := consumer.Pop(); err != nil {
		t.Fatal(err)
	}
	record, ok, err = consumer.Peek()
	if err != nil || !ok || len(record.UnsafeBytes()) != 1 || record.UnsafeBytes()[0] != 9 {
		t.Fatalf("second peek: %v %v", ok, err)
	}
	if err := consumer.Pop(); err != nil {
		t.Fatal(err)
	}
	if err := consumer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := producer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestAttachWrongMagicReturnsFormat(t *testing.T) {
	q, err := CreateMPSC(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	fd, err := q.DupFD()
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fd)
	var zero [8]byte
	if _, err := syscall.Pwrite(fd, zero[:], 0); err != nil {
		t.Fatal(err)
	}
	_, err = AttachMPSC(fd)
	if !errors.Is(err, ErrFormat) || errors.Is(err, ErrFormatVersion) {
		t.Fatalf("got %v", err)
	}
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestDefaultPreallocationAndDisableOption(t *testing.T) {
	q, err := CreateMPSC(Config{Capacity: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	fd, err := q.DupFD()
	if err != nil {
		t.Fatal(err)
	}
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		t.Fatal(err)
	}
	syscall.Close(fd)
	if st.Blocks == 0 {
		t.Fatal("default creation did not preallocate backing blocks")
	}
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
	q, err = CreateMPSC(Config{Capacity: 1 << 20, DisablePreallocate: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestMPSCHotExtentValidationMatchesGeometry(t *testing.T) {
	for layout, geometry := range mpscLayoutGeometries {
		q, err := CreateMPSC(Config{Capacity: 4096, MPSCLayout: MPSCLayout(layout)})
		if err != nil {
			t.Fatal(err)
		}
		grain := uint64(geometry.grain)
		if q.hot.grainMask != grain-1 {
			t.Fatalf("layout %d grain mask: got %d want %d", layout, q.hot.grainMask, grain-1)
		}
		if q.hot.maxNeed != q.Capacity()-grain {
			t.Fatalf("layout %d max need: got %d want %d", layout, q.hot.maxNeed, q.Capacity()-grain)
		}
		for extent := uint64(0); extent <= q.Capacity()+grain; extent++ {
			want := extent != 0 && extent%grain == 0 && extent <= q.Capacity()-grain
			if got := q.hot.validExtent(extent); got != want {
				t.Fatalf("layout %d extent %d: got %t want %t", layout, extent, got, want)
			}
		}
		for _, extent := range []uint64{maxExtent, maxExtent + 1, ^uint64(0)} {
			want := extent != 0 && extent%grain == 0 && extent <= q.Capacity()-grain
			if got := q.hot.validExtent(extent); got != want {
				t.Fatalf("layout %d large extent %d: got %t want %t", layout, extent, got, want)
			}
		}
		if err := q.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPublicLayoutExtentHelpersUseCanonicalGeometry(t *testing.T) {
	for layout, geometry := range mpscLayoutGeometries {
		grain, extent, err := MPSCLayoutExtent(MPSCLayout(layout), 65)
		if err != nil {
			t.Fatal(err)
		}
		if grain != uint64(geometry.grain) {
			t.Fatalf("layout %d grain: got %d want %d", layout, grain, geometry.grain)
		}
		want, ok := extentFor(65, uint64(geometry.grain))
		if !ok || extent != want {
			t.Fatalf("layout %d extent: got %d want %d", layout, extent, want)
		}
	}
	if _, _, err := MPSCLayoutExtent(MPSCLayout(len(mpscLayoutGeometries)), 1); !errors.Is(err, ErrFormat) {
		t.Fatalf("unsupported layout: %v", err)
	}
	if grain, _, err := MPSCLayoutExtent(MPSCCompact, maxExtent+1); grain != cacheLine || !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversized MPSC extent: grain=%d err=%v", grain, err)
	}
	if extent, err := SPSCExtent(0); err != nil || extent != cacheLine {
		t.Fatalf("zero SPSC extent: extent=%d err=%v", extent, err)
	}
	if extent, err := SPSCExtent(65); err != nil || extent != 2*cacheLine {
		t.Fatalf("rounded SPSC extent: extent=%d err=%v", extent, err)
	}
	if _, err := SPSCExtent(maxExtent + 1); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversized SPSC extent: %v", err)
	}
}

func TestMPSCLayoutGeometry(t *testing.T) {
	for layout, want := range mpscLayoutGeometries {
		q, err := CreateMPSC(Config{Capacity: 4096, MPSCLayout: MPSCLayout(layout)})
		if err != nil {
			t.Fatal(err)
		}
		created := planeGeometry{claimStride: q.r.claimStride, resultStride: q.r.resultStride, grain: q.r.planeGrain}
		if created != want {
			t.Fatalf("layout %d create: got %+v want %+v", layout, created, want)
		}
		fd, err := q.DupFD()
		if err != nil {
			t.Fatal(err)
		}
		peer, err := AttachMPSC(fd)
		syscall.Close(fd)
		if err != nil {
			t.Fatal(err)
		}
		attached := planeGeometry{claimStride: peer.r.claimStride, resultStride: peer.r.resultStride, grain: peer.r.planeGrain}
		if attached != want {
			t.Fatalf("layout %d attach: got %+v want %+v", layout, attached, want)
		}
		if err := peer.Close(); err != nil {
			t.Fatal(err)
		}
		if err := q.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSameTIDConsumerExclusion(t *testing.T) {
	m, err := CreateMPSC(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	mc, err := m.AttachConsumer()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.AttachConsumer(); !errors.Is(err, ErrBusy) {
		t.Fatalf("MPSC second consumer: %v", err)
	}
	if err := mc.Close(); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := CreateSPSC(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	sc, err := s.AttachConsumer()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AttachConsumer(); !errors.Is(err, ErrBusy) {
		t.Fatalf("SPSC second consumer: %v", err)
	}
	if err := sc.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestLogicalPositionExhaustion(t *testing.T) {
	q, err := CreateMPSC(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	producer, err := q.NewProducer()
	if err != nil {
		t.Fatal(err)
	}
	p := ^uint64(0) - 31
	atomicStore := q.ptr64(controlSize)
	*atomicStore = p
	producer.core.readCache = p
	if _, err := producer.Reserve(1); !errors.Is(err, ErrPositionExhausted) {
		t.Fatalf("got %v", err)
	}
	if err := producer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestReaderRestartRedeliversUnpoppedRecord(t *testing.T) {
	q, err := CreateMPSC(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	producer, err := q.NewProducer()
	if err != nil {
		t.Fatal(err)
	}
	if err := producer.Write([]byte("survives")); err != nil {
		t.Fatal(err)
	}
	cmd := startFaultChild(t, q, "peek")
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	consumer, err := q.AttachConsumer()
	if err != nil {
		t.Fatal(err)
	}
	record, ok, err := consumer.Peek()
	if err != nil || !ok || string(record.UnsafeBytes()) != "survives" {
		t.Fatalf("restart peek: %v %v", ok, err)
	}
	if err := consumer.Pop(); err != nil {
		t.Fatal(err)
	}
	if err := consumer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := producer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSPSCStaleOwnerTakeoverWhenDrained(t *testing.T) {
	q, err := CreateSPSC(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	bitmap := q.ptr64(controlSize + shardSize)
	owner := q.ptr32(controlSize + shardSize + 8)
	generation := q.ptr32(controlSize + shardSize + 12)
	atomic.StoreUint64(bitmap, 1)
	atomic.StoreUint32(owner, deadTid(t))
	before := atomic.LoadUint32(generation)
	producer, err := q.AttachProducer()
	if err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadUint32(generation); got != before+1 {
		t.Fatalf("generation: got %d want %d", got, before+1)
	}
	if err := producer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSPSCIncompleteOwnerPublicationTakeover(t *testing.T) {
	q, err := CreateSPSC(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	atomic.StoreUint64(q.ptr64(controlSize+shardSize), 1)
	atomic.StoreUint32(q.ptr32(controlSize+shardSize+8), 0)
	producer, err := q.AttachProducer()
	if err != nil {
		t.Fatal(err)
	}
	if err := producer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentCloseAndProducerAttach(t *testing.T) {
	for i := 0; i < 100; i++ {
		q, err := CreateMPSC(testConfig())
		if err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		attachResult := make(chan error, 1)
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			producer, err := q.NewProducer()
			if err == nil {
				err = producer.Close()
			}
			attachResult <- err
		}()
		close(start)
		closeErr := q.Close()
		attachErr := <-attachResult
		wg.Wait()
		switch {
		case closeErr == nil:
			if attachErr != nil && !errors.Is(attachErr, ErrClosed) {
				t.Fatalf("iteration %d: close succeeded, attach returned %v", i, attachErr)
			}
			if _, err := q.NewProducer(); !errors.Is(err, ErrClosed) {
				t.Fatalf("iteration %d: attach after successful close returned %v", i, err)
			}
		case errors.Is(closeErr, ErrBusy):
			if attachErr != nil {
				t.Fatalf("iteration %d: close was busy, attach returned %v", i, attachErr)
			}
			if err := q.Close(); err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatalf("iteration %d: close returned %v", i, closeErr)
		}
	}
}

func TestConcurrentDupFDAndClose(t *testing.T) {
	for i := 0; i < 200; i++ {
		m, err := CreateMPSC(testConfig())
		if err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		result := make(chan struct {
			fd  int
			err error
		}, 1)
		go func() {
			<-start
			fd, err := m.DupFD()
			result <- struct {
				fd  int
				err error
			}{fd, err}
		}()
		close(start)
		if err := m.Close(); err != nil {
			t.Fatal(err)
		}
		got := <-result
		if got.err == nil {
			if err := syscall.Close(got.fd); err != nil {
				t.Fatal(err)
			}
		} else if !errors.Is(got.err, ErrClosed) {
			t.Fatalf("MPSC iteration %d: DupFD returned %v", i, got.err)
		}

		s, err := CreateSPSC(testConfig())
		if err != nil {
			t.Fatal(err)
		}
		start = make(chan struct{})
		result = make(chan struct {
			fd  int
			err error
		}, 1)
		go func() {
			<-start
			fd, err := s.DupFD()
			result <- struct {
				fd  int
				err error
			}{fd, err}
		}()
		close(start)
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		got = <-result
		if got.err == nil {
			if err := syscall.Close(got.fd); err != nil {
				t.Fatal(err)
			}
		} else if !errors.Is(got.err, ErrClosed) {
			t.Fatalf("SPSC iteration %d: DupFD returned %v", i, got.err)
		}
	}
}

func TestInvalidSHMNames(t *testing.T) {
	for _, name := range []string{"..", "../escape", "a/b", `a\\b`, "a..b"} {
		q, err := CreateMPSC(Config{Capacity: 4096, Backend: BackendSHM, Name: name})
		if q != nil || !errors.Is(err, ErrFormat) {
			t.Fatalf("name %q: queue %v, error %v", name, q, err)
		}
	}
}

func TestDupFDRejectsClosedQueue(t *testing.T) {
	m, err := CreateMPSC(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := m.DupFD(); !errors.Is(err, ErrClosed) {
		t.Fatalf("MPSC DupFD after close: %v", err)
	}
	s, err := CreateSPSC(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DupFD(); !errors.Is(err, ErrClosed) {
		t.Fatalf("SPSC DupFD after close: %v", err)
	}
}

func TestMPSCRejectsLengthBeyondExtent(t *testing.T) {
	q, err := CreateMPSC(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	consumer, err := q.AttachConsumer()
	if err != nil {
		t.Fatal(err)
	}
	atomic.StoreUint64(q.claim(0), packrecord(64, stateCleared, 1))
	length, tag := q.result(0)
	atomic.StoreUint64(length, 65)
	atomic.StoreUint64(tag, commitTag(0))
	if _, _, err := consumer.Peek(); !errors.Is(err, ErrFormat) {
		t.Fatalf("peek malformed length: %v", err)
	}
	if err := consumer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestMPSCAdmissionReturnsContended(t *testing.T) {
	q, err := CreateMPSC(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	producer, err := q.NewProducer()
	if err != nil {
		t.Fatal(err)
	}
	atomic.StoreUint64(q.claim(0), packrecord(64, stateClaimed, uint32(hostcpu.CurrentThreadId())))
	atomic.StoreUint64(q.claim(64), freeWord(64))
	if _, err := producer.Reserve(1); !errors.Is(err, ErrContended) {
		t.Fatalf("reserve: %v", err)
	}
	if err := producer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestFullLivenessUsesObservationBackoff(t *testing.T) {
	m, err := CreateMPSC(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	mp, err := m.NewProducer()
	if err != nil {
		t.Fatal(err)
	}
	mc, err := m.AttachConsumer()
	if err != nil {
		t.Fatal(err)
	}
	if err := mp.Write(make([]byte, m.Capacity()-64)); err != nil {
		t.Fatal(err)
	}
	if _, err := mp.Reserve(1); !errors.Is(err, ErrFull) {
		t.Fatalf("MPSC full: %v", err)
	}
	if mp.core.fullStreak != 1 {
		t.Fatalf("MPSC full observations: %d", mp.core.fullStreak)
	}
	_, ok, err := mc.Peek()
	if err != nil || !ok {
		t.Fatalf("MPSC peek: %v %v", ok, err)
	}
	if err := mc.Pop(); err != nil {
		t.Fatal(err)
	}
	if err := mc.Close(); err != nil {
		t.Fatal(err)
	}
	if err := mp.Close(); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := CreateSPSC(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	sp, err := s.AttachProducer()
	if err != nil {
		t.Fatal(err)
	}
	sc, err := s.AttachConsumer()
	if err != nil {
		t.Fatal(err)
	}
	if err := sp.Write(make([]byte, s.Capacity())); err != nil {
		t.Fatal(err)
	}
	if _, err := sp.Reserve(1); !errors.Is(err, ErrFull) {
		t.Fatalf("SPSC full: %v", err)
	}
	if sp.core.fullStreak != 1 {
		t.Fatalf("SPSC full observations: %d", sp.core.fullStreak)
	}
	_, ok, err = sc.Peek()
	if err != nil || !ok {
		t.Fatalf("SPSC peek: %v %v", ok, err)
	}
	if err := sc.Pop(); err != nil {
		t.Fatal(err)
	}
	if err := sc.Close(); err != nil {
		t.Fatal(err)
	}
	if err := sp.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestMPSCWalkPositionExhaustion(t *testing.T) {
	q, err := CreateMPSC(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	producer, err := q.NewProducer()
	if err != nil {
		t.Fatal(err)
	}
	start := ^uint64(0) - 191
	atomic.StoreUint64(q.ptr64(controlSize), start)
	atomic.StoreUint64(q.ptr64(controlSize+128), start)
	producer.core.readCache = start
	atomic.StoreUint64(q.claim(start), packrecord(128, stateAborted, 1))
	atomic.StoreUint64(q.claim(start+128), packrecord(64, stateAborted, 1))
	if _, err := producer.Reserve(1); !errors.Is(err, ErrPositionExhausted) {
		t.Fatalf("reserve near MaxUint64: %v", err)
	}
	if err := producer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestClaimedDeadWriterPromotesSuccessor(t *testing.T) {
	q, err := CreateMPSC(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	consumer, err := q.AttachConsumer()
	if err != nil {
		t.Fatal(err)
	}
	atomic.StoreUint64(q.claim(0), packrecord(64, stateClaimed, deadTid(t)))
	for i := 0; i < 130; i++ {
		if record, ok, err := consumer.Peek(); err != nil || ok || record.UnsafeBytes() != nil {
			t.Fatalf("recovery peek %d: %v %v", i, ok, err)
		}
	}
	if got := atomic.LoadUint64(q.claim(0)) & stateMask; got != stateAborted {
		t.Fatalf("recovered state: %d", got)
	}
	if got := atomic.LoadUint64(q.claim(64)); got != freeWord(64) {
		t.Fatalf("successor word: got %#x want %#x", got, freeWord(64))
	}
	producer, err := q.NewProducer()
	if err != nil {
		t.Fatal(err)
	}
	if err := producer.Write([]byte("after")); err != nil {
		t.Fatal(err)
	}
	record, ok, err := consumer.Peek()
	if err != nil || !ok || string(record.UnsafeBytes()) != "after" {
		t.Fatalf("delivered successor: %v %v", ok, err)
	}
	if err := consumer.Pop(); err != nil {
		t.Fatal(err)
	}
	if err := producer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := consumer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCompletedSpansRejectReuse(t *testing.T) {
	q, err := CreateMPSC(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	producer, err := q.NewProducer()
	if err != nil {
		t.Fatal(err)
	}
	consumer, err := q.AttachConsumer()
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := producer.Reserve(1)
	if err != nil {
		t.Fatal(err)
	}
	reservation.Bytes()[0] = 1
	if err := producer.Commit(reservation, 1); err != nil {
		t.Fatal(err)
	}
	if err := producer.Commit(reservation, 1); !errors.Is(err, ErrMisuse) {
		t.Fatalf("second commit: %v", err)
	}
	_, ok, err := consumer.Peek()
	if err != nil || !ok {
		t.Fatalf("peek: %v %v", ok, err)
	}
	if err := consumer.Pop(); err != nil {
		t.Fatal(err)
	}
	if err := consumer.Pop(); !errors.Is(err, ErrMisuse) {
		t.Fatalf("second pop: %v", err)
	}
	if err := consumer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := producer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
}
