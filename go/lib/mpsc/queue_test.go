//go:build linux && (amd64 || arm64)

package mpsc

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"

	"github.com/akalsi-org/polyglot-template/go/lib/hostcpu"
)

func init() {
	procfsValidationProbe = func() error { return nil }
}

func testConfig() Config { return Config{Capacity: 4096, Backend: BackendMemfd} }

func TestCreateAndAttachFailClosedWhenProcfsValidationFails(t *testing.T) {
	type queueDescriptor interface {
		DupFD() (int, error)
		Close() error
	}
	tests := []struct {
		name   string
		create func(Config) (queueDescriptor, error)
		attach func(int) (queueDescriptor, error)
	}{
		{
			name: "MPSC",
			create: func(cfg Config) (queueDescriptor, error) {
				q, err := CreateMPSC(cfg)
				if q == nil {
					return nil, err
				}
				return q, err
			},
			attach: func(fd int) (queueDescriptor, error) {
				q, err := AttachMPSC(fd)
				if q == nil {
					return nil, err
				}
				return q, err
			},
		},
		{
			name: "SPSC",
			create: func(cfg Config) (queueDescriptor, error) {
				q, err := CreateSPSC(cfg)
				if q == nil {
					return nil, err
				}
				return q, err
			},
			attach: func(fd int) (queueDescriptor, error) {
				q, err := AttachSPSC(fd)
				if q == nil {
					return nil, err
				}
				return q, err
			},
		},
	}

	oldProbe := procfsValidationProbe
	defer func() { procfsValidationProbe = oldProbe }()

	for _, test := range tests {
		t.Run(test.name+" create", func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "queue")
			procfsValidationProbe = func() error { return hostcpu.ErrProcfsPIDNamespace }
			q, err := test.create(Config{Capacity: 4096, Backend: BackendFile, Name: path})
			if q != nil || !errors.Is(err, hostcpu.ErrProcfsPIDNamespace) {
				t.Fatalf("create: queue=%v err=%v", q, err)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("backing file exists after rejected create: %v", err)
			}
		})

		t.Run(test.name+" attach", func(t *testing.T) {
			procfsValidationProbe = func() error { return hostcpu.ErrProcfsPIDNamespace }
			peer, err := test.attach(-1)
			if peer != nil || !errors.Is(err, hostcpu.ErrProcfsPIDNamespace) {
				t.Fatalf("attach before descriptor access: queue=%v err=%v", peer, err)
			}

			procfsValidationProbe = func() error { return nil }
			q, err := test.create(testConfig())
			if err != nil {
				t.Fatal(err)
			}
			fd, err := q.DupFD()
			if err != nil {
				t.Fatal(err)
			}
			defer syscall.Close(fd)
			var before [controlSize]byte
			if _, err := syscall.Pread(fd, before[:], 0); err != nil {
				t.Fatal(err)
			}
			procfsValidationProbe = func() error { return hostcpu.ErrProcfsPIDNamespace }
			peer, err = test.attach(fd)
			if peer != nil || !errors.Is(err, hostcpu.ErrProcfsPIDNamespace) {
				t.Fatalf("attach: queue=%v err=%v", peer, err)
			}
			var after [controlSize]byte
			if _, err := syscall.Pread(fd, after[:], 0); err != nil {
				t.Fatal(err)
			}
			if before != after {
				t.Fatal("rejected attach mutated shared ownership metadata")
			}
			if err := q.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMPSCBasicAbortWrapAndAttach(t *testing.T) {
	q, err := CreateMPSC(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	fd, err := q.dupFD()
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fd)
	peer, err := AttachMPSC(fd)
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	w, err := q.newWriter()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	rd, err := peer.newReader()
	if err != nil {
		t.Fatal(err)
	}
	defer rd.Close()
	for i := 0; i < 80; i++ {
		r, s := w.Reserve(70)
		if s != statusOK {
			t.Fatalf("reserve %d: %v", i, s)
		}
		for j := range r.Bytes() {
			r.Bytes()[j] = byte(i)
		}
		if i == 17 {
			if err := w.Abort(r); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := w.Commit(r, 33); err != nil {
			t.Fatal(err)
		}
		got, ok, err := rd.Peek()
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			t.Fatalf("missing %d", i)
		}
		if !bytes.Equal(got.Bytes(), bytes.Repeat([]byte{byte(i)}, 33)) {
			t.Fatalf("payload %d", i)
		}
		if err := rd.Pop(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMPSCMultipleWriters(t *testing.T) {
	q, err := CreateMPSC(Config{Capacity: 1 << 16, Backend: BackendMemfd})
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	rd, err := q.newReader()
	if err != nil {
		t.Fatal(err)
	}
	defer rd.Close()
	const writers = 4
	const each = 200
	var wg sync.WaitGroup
	wg.Add(writers)
	for id := 0; id < writers; id++ {
		go func() {
			defer wg.Done()
			w, e := q.newWriter()
			if e != nil {
				t.Error(e)
				return
			}
			defer w.Close()
			for n := 0; n < each; {
				p := []byte{byte(id), byte(n), byte(n >> 8)}
				if w.Write(p) == statusOK {
					n++
				}
			}
		}()
	}
	seen := make([]int, writers)
	for total := 0; total < writers*each; {
		rec, ok, e := rd.Peek()
		if e != nil {
			t.Fatal(e)
		}
		if !ok {
			continue
		}
		p := rec.Bytes()
		id := int(p[0])
		n := int(p[1]) | int(p[2])<<8
		if n != seen[id] {
			t.Fatalf("writer %d: got %d want %d", id, n, seen[id])
		}
		seen[id]++
		total++
		if e := rd.Pop(); e != nil {
			t.Fatal(e)
		}
	}
	wg.Wait()
}

func TestAttachRejectsOtherVersionWithoutMutation(t *testing.T) {
	q, err := CreateMPSC(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	fd, err := q.dupFD()
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fd)
	var v [4]byte
	v[0] = 3
	if _, err = syscall.Pwrite(fd, v[:], 8); err != nil {
		t.Fatal(err)
	}
	before := make([]byte, 128)
	if _, err = syscall.Pread(fd, before, 0); err != nil {
		t.Fatal(err)
	}
	_, err = AttachMPSC(fd)
	if !errors.Is(err, ErrFormatVersion) {
		t.Fatalf("got %v", err)
	}
	after := make([]byte, 128)
	if _, err = syscall.Pread(fd, after, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("attach mutated mapping")
	}
}

func TestSPSCShortCommitAbortAndCapacity(t *testing.T) {
	q, err := CreateSPSC(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	w, err := q.newWriter()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	rd, err := q.newReader()
	if err != nil {
		t.Fatal(err)
	}
	defer rd.Close()
	r, s := w.Reserve(120)
	if s != statusOK {
		t.Fatal(s)
	}
	copy(r.Bytes(), []byte("hello"))
	if err = w.Commit(r, 5); err != nil {
		t.Fatal(err)
	}
	rec, ok, err := rd.Peek()
	if err != nil {
		t.Fatal(err)
	}
	if !ok || string(rec.Bytes()) != "hello" {
		t.Fatalf("got %q", rec.Bytes())
	}
	if err = rd.Pop(); err != nil {
		t.Fatal(err)
	}
	r, s = w.Reserve(10)
	if s != statusOK {
		t.Fatal(s)
	}
	if err = w.Abort(r); err != nil {
		t.Fatal(err)
	}
	if rec, ok, err = rd.Peek(); err != nil || ok {
		t.Fatalf("abort published: %v %v", rec, err)
	}
	r, s = w.Reserve(int(q.Capacity()))
	if s != statusOK {
		t.Fatalf("full reservation: %v", s)
	}
	if err = w.Abort(r); err != nil {
		t.Fatal(err)
	}
}
