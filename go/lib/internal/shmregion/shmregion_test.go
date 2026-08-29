//go:build linux && (amd64 || arm64)

package shmregion

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestCreateProbeAndAttach(t *testing.T) {
	page := uint64(os.Getpagesize())
	layout := Layout{ControlBytes: page, ArenaBytes: page}
	mapping, err := Create(CreateOptions{
		Backend:   BackendMemfd,
		MemfdName: "shmregion-test",
		Layout:    layout,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer mapping.Close()

	mapping.Control()[0] = 17
	mapping.Arena()[11] = 29
	if got := mapping.Arena()[int(page)+11]; got != 29 {
		t.Fatalf("mirrored byte: got %d, want 29", got)
	}

	fd, err := mapping.DupFD()
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fd)
	if _, err := syscall.Seek(fd, 7, 0); err != nil {
		t.Fatal(err)
	}
	var header [16]byte
	fileSize, err := Probe(fd, header[:])
	if err != nil {
		t.Fatal(err)
	}
	if fileSize != 2*page {
		t.Fatalf("file size: got %d, want %d", fileSize, 2*page)
	}
	if header[0] != 17 {
		t.Fatalf("control byte: got %d, want 17", header[0])
	}
	if offset, err := syscall.Seek(fd, 0, 1); err != nil || offset != 7 {
		t.Fatalf("file offset: got %d, error %v", offset, err)
	}

	peer, err := Attach(fd, layout)
	if err != nil {
		t.Fatal(err)
	}
	if peer.Control()[0] != 17 || peer.Arena()[11] != 29 {
		t.Fatal("attached mapping does not share bytes")
	}
	if err := peer.Close(); err != nil {
		t.Fatal(err)
	}
	if !peer.Closed() {
		t.Fatal("closed mapping reports open")
	}
	if err := peer.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

func TestCreateRejectsInvalidBackingNames(t *testing.T) {
	page := uint64(os.Getpagesize())
	layout := Layout{ControlBytes: page, ArenaBytes: page}
	tests := []struct {
		name    string
		backend Backend
		path    string
	}{
		{name: "empty shared memory name", backend: BackendSHM},
		{name: "shared memory path", backend: BackendSHM, path: "dir/name"},
		{name: "shared memory backslash", backend: BackendSHM, path: `dir\name`},
		{name: "shared memory traversal", backend: BackendSHM, path: "name..suffix"},
		{name: "shared memory NUL", backend: BackendSHM, path: "name\x00suffix"},
		{name: "empty file path", backend: BackendFile},
		{name: "unknown backend", backend: Backend(255), path: "name"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mapping, err := Create(CreateOptions{
				Backend: test.backend,
				Name:    test.path,
				Layout:  layout,
			})
			if mapping != nil {
				mapping.Close()
				t.Fatal("Create returned a mapping for invalid options")
			}
			if !errors.Is(err, syscall.EINVAL) {
				t.Fatalf("Create error: got %v, want %v", err, syscall.EINVAL)
			}
		})
	}
}

func TestCreateFailureUnlinksExclusiveFileForRetry(t *testing.T) {
	page := uint64(os.Getpagesize())
	layout := Layout{ControlBytes: page, ArenaBytes: page}
	tests := []struct {
		name   string
		inject func() func()
	}{
		{
			name: "ftruncate",
			inject: func() func() {
				original := ftruncateBacking
				ftruncateBacking = func(int, int64) error { return syscall.ENOSPC }
				return func() { ftruncateBacking = original }
			},
		},
		{
			name: "fallocate",
			inject: func() func() {
				original := fallocateBacking
				fallocateBacking = func(int, uint32, int64, int64) error { return syscall.ENOSPC }
				return func() { fallocateBacking = original }
			},
		},
		{
			name: "mapping",
			inject: func() func() {
				original := mapBacking
				mapBacking = func(int, Layout) (*Mapping, error) { return nil, syscall.ENOSPC }
				return func() { mapBacking = original }
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "region")
			options := CreateOptions{Backend: BackendFile, Name: path, Layout: layout}
			restore := test.inject()
			mapping, err := Create(options)
			restore()
			if mapping != nil {
				mapping.Close()
				t.Fatal("Create returned a mapping after an injected failure")
			}
			if !errors.Is(err, syscall.ENOSPC) {
				t.Fatalf("Create error: got %v, want %v", err, syscall.ENOSPC)
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("failed creation left path %q: %v", path, err)
			}

			mapping, err = Create(options)
			if err != nil {
				t.Fatalf("retry Create: %v", err)
			}
			if err := mapping.Close(); err != nil {
				t.Fatalf("retry Close: %v", err)
			}
		})
	}
}

func TestCreateFailureUnlinksExclusiveSHMForRetry(t *testing.T) {
	page := uint64(os.Getpagesize())
	name := fmt.Sprintf("shmregion-test-%d-%s", os.Getpid(), filepath.Base(t.TempDir()))
	path := "/dev/shm/" + name
	t.Cleanup(func() { _ = os.Remove(path) })
	options := CreateOptions{
		Backend:            BackendSHM,
		Name:               name,
		Layout:             Layout{ControlBytes: page, ArenaBytes: page},
		DisablePreallocate: true,
	}
	original := mapBacking
	mapBacking = func(int, Layout) (*Mapping, error) { return nil, syscall.ENOSPC }
	mapping, err := Create(options)
	mapBacking = original
	if mapping != nil {
		mapping.Close()
		t.Fatal("Create returned a mapping after an injected failure")
	}
	if !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("Create error: got %v, want %v", err, syscall.ENOSPC)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed creation left path %q: %v", path, err)
	}

	mapping, err = Create(options)
	if err != nil {
		t.Fatalf("retry Create: %v", err)
	}
	if err := mapping.Close(); err != nil {
		t.Fatalf("retry Close: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove shared memory path: %v", err)
	}
}

func TestCreateFailureDoesNotUnlinkOverwriteFile(t *testing.T) {
	page := uint64(os.Getpagesize())
	path := filepath.Join(t.TempDir(), "region")
	if err := os.WriteFile(path, []byte("existing"), 0600); err != nil {
		t.Fatal(err)
	}
	options := CreateOptions{
		Backend:            BackendFile,
		Name:               path,
		Layout:             Layout{ControlBytes: page, ArenaBytes: page},
		DisablePreallocate: true,
		AllowOverwrite:     true,
	}
	original := mapBacking
	mapBacking = func(int, Layout) (*Mapping, error) { return nil, syscall.ENOSPC }
	mapping, err := Create(options)
	mapBacking = original
	if mapping != nil {
		mapping.Close()
		t.Fatal("Create returned a mapping after an injected failure")
	}
	if !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("Create error: got %v, want %v", err, syscall.ENOSPC)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("overwrite failure removed path %q: %v", path, err)
	}

	mapping, err = Create(options)
	if err != nil {
		t.Fatalf("retry Create: %v", err)
	}
	if err := mapping.Close(); err != nil {
		t.Fatalf("retry Close: %v", err)
	}
}

func TestCreateMemfdSealFailureAllowsRetry(t *testing.T) {
	page := uint64(os.Getpagesize())
	options := CreateOptions{
		Backend:            BackendMemfd,
		MemfdName:          "shmregion-seal-failure",
		Layout:             Layout{ControlBytes: page, ArenaBytes: page},
		DisablePreallocate: true,
	}
	original := sealMemfd
	sealMemfd = func(int) error { return syscall.ENOSPC }
	mapping, err := Create(options)
	sealMemfd = original
	if mapping != nil {
		mapping.Close()
		t.Fatal("Create returned a mapping after an injected seal failure")
	}
	if !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("Create error: got %v, want %v", err, syscall.ENOSPC)
	}

	mapping, err = Create(options)
	if err != nil {
		t.Fatalf("retry Create: %v", err)
	}
	if err := mapping.Close(); err != nil {
		t.Fatalf("retry Close: %v", err)
	}
}

func TestProbeRejectsShortFile(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "probe")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := Probe(int(file.Fd()), make([]byte, 1)); !errors.Is(err, ErrProbeTooSmall) {
		t.Fatalf("Probe error: got %v, want %v", err, ErrProbeTooSmall)
	}
}
