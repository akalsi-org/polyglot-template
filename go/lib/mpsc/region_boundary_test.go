//go:build linux && (amd64 || arm64)

package mpsc

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"unsafe"
)

func TestAttachChecksMagicBeforeVersion(t *testing.T) {
	q, err := CreateMPSC(Config{Capacity: 4096})
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	fd, err := q.DupFD()
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fd)

	header := make([]byte, 12)
	put64(header, 0, formatMagic+1)
	put32(header, 8, FormatVersion+1)
	if _, err := syscall.Pwrite(fd, header, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := AttachMPSC(fd); !errors.Is(err, ErrFormat) || errors.Is(err, ErrFormatVersion) {
		t.Fatalf("attach error: %v", err)
	}
}

func TestBackendFilePreservesExistingFileByDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue")
	original := []byte("preserve this data")
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}

	_, err := CreateMPSC(Config{Capacity: 4096, Backend: BackendFile, Name: path})
	if !errors.Is(err, syscall.EEXIST) {
		t.Fatalf("create error: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(original) {
		t.Fatalf("file changed: got %q", got)
	}
}

func TestBackendFileAllowOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue")
	if err := os.WriteFile(path, []byte("old data"), 0600); err != nil {
		t.Fatal(err)
	}

	q, err := CreateMPSC(Config{
		Capacity:       4096,
		Backend:        BackendFile,
		Name:           path,
		AllowOverwrite: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	fd, err := q.DupFD()
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fd)
	peer, err := AttachMPSC(fd)
	if err != nil {
		t.Fatal(err)
	}
	if err := peer.Close(); err != nil {
		t.Fatal(err)
	}
}

func requireCloseOnExec(t *testing.T, fd int) {
	t.Helper()
	const fGetFD = 1
	flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), fGetFD, 0)
	if errno != 0 {
		t.Fatal(errno)
	}
	if flags&syscall.FD_CLOEXEC == 0 {
		t.Fatalf("descriptor %d does not have FD_CLOEXEC", fd)
	}
}

func TestDuplicatedDescriptorsAreCloseOnExec(t *testing.T) {
	q, err := CreateMPSC(Config{Capacity: 4096})
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	fd, err := q.DupFD()
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fd)
	requireCloseOnExec(t, fd)

	peer, err := AttachMPSC(fd)
	if err != nil {
		t.Fatal(err)
	}
	requireCloseOnExec(t, peer.r.fd)
	if err := peer.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestLargePaddedLayoutInitializesGeometry(t *testing.T) {
	q, err := CreateMPSC(Config{
		Capacity:           1 << 26,
		DisablePreallocate: true,
		MPSCLayout:         MPSCPadded256,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	if q.r.planeShift != 8 {
		t.Fatalf("plane shift: got %d, want 8", q.r.planeShift)
	}
	if want := q.Capacity()/256 - 1; q.r.planeMask != want {
		t.Fatalf("plane mask: got %d, want %d", q.r.planeMask, want)
	}
}

func TestPlaneAddressingMatchesDivisionGeometry(t *testing.T) {
	for _, layout := range []MPSCLayout{MPSCCompact, MPSCPadded64, MPSCPadded256} {
		q, err := CreateMPSC(Config{Capacity: 4096, MPSCLayout: layout})
		if err != nil {
			t.Fatal(err)
		}
		grain := uint64(q.r.planeGrain)
		for _, pos := range []uint64{0, grain, 4096 - grain, 4096, 8192 + grain} {
			cell := (pos / grain) & (q.r.capacity/grain - 1)
			wantClaim := q.ptr64(q.r.claimBase() + uintptr(cell)*uintptr(q.r.claimStride))
			if got := q.claim(pos); got != wantClaim {
				t.Fatalf("layout %d claim position %d", layout, pos)
			}
			wantResult := q.ptr64(q.r.resultBase() + uintptr(cell)*uintptr(q.r.resultStride))
			gotResult, _ := q.result(pos)
			if gotResult != wantResult {
				t.Fatalf("layout %d result position %d", layout, pos)
			}
		}
		if err := q.Close(); err != nil {
			t.Fatal(err)
		}
	}

	q, err := CreateSPSC(Config{Capacity: 4096})
	if err != nil {
		t.Fatal(err)
	}
	for _, pos := range []uint64{0, 64, 4032, 4096, 8256} {
		cell := (pos / 64) & (q.r.capacity/64 - 1)
		want := q.ptr64(q.r.claimBase() + uintptr(cell)*8)
		if got := q.slot(pos); got != want {
			t.Fatalf("SPSC slot position %d", pos)
		}
	}
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestBoundPointersAreAlignedAndEquivalent(t *testing.T) {
	for _, layout := range []MPSCLayout{MPSCCompact, MPSCPadded64, MPSCPadded256} {
		q, err := CreateMPSC(Config{Capacity: 4096, MPSCLayout: layout})
		if err != nil {
			t.Fatal(err)
		}
		base := unsafe.Pointer(&q.r.memory[0])
		checks := []struct {
			name  string
			got   unsafe.Pointer
			want  unsafe.Pointer
			align uintptr
		}{
			{"hint", unsafe.Pointer(q.hot.hint), unsafe.Add(base, controlSize), 8},
			{"read position", unsafe.Pointer(q.hot.readPos), unsafe.Add(base, controlSize+128), 8},
			{"reader Tid", unsafe.Pointer(q.hot.readerTid), unsafe.Add(base, 28), 4},
			{"claim base", q.hot.claimBase, unsafe.Add(base, q.r.claimBase()), 8},
			{"result base", q.hot.resultBase, unsafe.Add(base, q.r.resultBase()), 8},
			{"arena base", q.hot.arenaBase, unsafe.Add(base, q.r.controlLen), 8},
		}
		for _, check := range checks {
			if check.got != check.want {
				t.Fatalf("layout %d %s address differs", layout, check.name)
			}
			if uintptr(check.got)%check.align != 0 {
				t.Fatalf("layout %d %s is not %d-byte aligned", layout, check.name, check.align)
			}
		}
		grain := uint64(q.r.planeGrain)
		for _, pos := range []uint64{0, grain, q.r.capacity - grain, q.r.capacity, 2*q.r.capacity + grain} {
			cell := (pos / grain) & (q.r.capacity/grain - 1)
			if got, want := q.hot.claim(pos), q.ptr64(q.r.claimBase()+uintptr(cell)*uintptr(q.r.claimStride)); got != want {
				t.Fatalf("layout %d claim position %d differs", layout, pos)
			}
			gotLength, gotTag := q.hot.result(pos)
			wantLength := q.ptr64(q.r.resultBase() + uintptr(cell)*uintptr(q.r.resultStride))
			if gotLength != wantLength || gotTag != (*uint64)(unsafe.Add(unsafe.Pointer(wantLength), 8)) {
				t.Fatalf("layout %d result position %d differs", layout, pos)
			}
		}
		gotPayload := q.hot.payload(q.r.capacity-32, 64)
		wantPayload := q.r.arena()[q.r.capacity-32 : q.r.capacity+32]
		if &gotPayload[0] != &wantPayload[0] || &gotPayload[63] != &wantPayload[63] {
			t.Fatalf("layout %d mirrored arena address differs", layout)
		}
		if err := q.Close(); err != nil {
			t.Fatal(err)
		}
	}

	q, err := CreateSPSC(Config{Capacity: 4096})
	if err != nil {
		t.Fatal(err)
	}
	base := unsafe.Pointer(&q.r.memory[0])
	checks := []struct {
		name  string
		got   unsafe.Pointer
		want  unsafe.Pointer
		align uintptr
	}{
		{"tail", unsafe.Pointer(q.hot.tail), unsafe.Add(base, controlSize), 8},
		{"read position", unsafe.Pointer(q.hot.readPos), unsafe.Add(base, controlSize+128), 8},
		{"writer bitmap", unsafe.Pointer(q.hot.writerBitmap), unsafe.Add(base, controlSize+shardSize), 8},
		{"reader Tid", unsafe.Pointer(q.hot.readerTid), unsafe.Add(base, 28), 4},
		{"writer owner", unsafe.Pointer(q.hot.writerOwner), unsafe.Add(base, controlSize+shardSize+8), 4},
		{"generation", unsafe.Pointer(q.hot.generation), unsafe.Add(base, controlSize+shardSize+12), 4},
		{"slot base", q.hot.slotBase, unsafe.Add(base, q.r.claimBase()), 8},
		{"arena base", q.hot.arenaBase, unsafe.Add(base, q.r.controlLen), 8},
	}
	for _, check := range checks {
		if check.got != check.want {
			t.Fatalf("SPSC %s address differs", check.name)
		}
		if uintptr(check.got)%check.align != 0 {
			t.Fatalf("SPSC %s is not %d-byte aligned", check.name, check.align)
		}
	}
	for _, pos := range []uint64{0, 64, 4032, 4096, 8256} {
		cell := (pos / 64) & (q.r.capacity/64 - 1)
		if got, want := q.hot.slot(pos), q.ptr64(q.r.claimBase()+uintptr(cell)*8); got != want {
			t.Fatalf("SPSC slot position %d differs", pos)
		}
	}
	gotPayload := q.hot.payload(q.r.capacity-32, 64)
	wantPayload := q.r.arena()[q.r.capacity-32 : q.r.capacity+32]
	if &gotPayload[0] != &wantPayload[0] || &gotPayload[63] != &wantPayload[63] {
		t.Fatal("SPSC mirrored arena address differs")
	}
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPIDNamespaceMetadataValidation(t *testing.T) {
	q, err := CreateMPSC(Config{Capacity: 4096})
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	fd, err := q.DupFD()
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fd)

	current, err := currentPIDNamespace()
	if err != nil {
		t.Fatal(err)
	}
	header := make([]byte, 64)
	if _, err := syscall.Pread(fd, header, 0); err != nil {
		t.Fatal(err)
	}
	if got := get64(header, pidNamespaceDevOffset); got != current.dev {
		t.Fatalf("creator namespace device: got %d, want %d", got, current.dev)
	}
	if got := get64(header, pidNamespaceInoOffset); got != current.ino {
		t.Fatalf("creator namespace inode: got %d, want %d", got, current.ino)
	}

	for _, tc := range []struct {
		name       string
		offset     uintptr
		foreignDev uint64
		foreignIno uint64
	}{
		{name: "device", offset: pidNamespaceDevOffset, foreignDev: current.dev + 1, foreignIno: current.ino},
		{name: "inode", offset: pidNamespaceInoOffset, foreignDev: current.dev, foreignIno: current.ino + 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			foreign := get64(header, tc.offset) + 1
			var encoded [8]byte
			put64(encoded[:], 0, foreign)
			if _, err := syscall.Pwrite(fd, encoded[:], int64(tc.offset)); err != nil {
				t.Fatal(err)
			}
			_, err := AttachMPSC(fd)
			if !errors.Is(err, ErrPIDNamespace) {
				t.Fatalf("attach error: %v", err)
			}
			var namespaceError *PIDNamespaceError
			if !errors.As(err, &namespaceError) {
				t.Fatalf("attach error type: %T", err)
			}
			if namespaceError.CreatorDev != tc.foreignDev || namespaceError.CreatorIno != tc.foreignIno ||
				namespaceError.CurrentDev != current.dev || namespaceError.CurrentIno != current.ino {
				t.Fatalf("namespace error: %+v", namespaceError)
			}
			put64(encoded[:], 0, get64(header, tc.offset))
			if _, err := syscall.Pwrite(fd, encoded[:], int64(tc.offset)); err != nil {
				t.Fatal(err)
			}
		})
	}
}
