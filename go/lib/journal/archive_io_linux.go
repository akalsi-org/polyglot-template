//go:build linux && (amd64 || arm64)

package journal

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"time"
	"unsafe"
)

type archiveFile interface {
	io.Reader
	io.ReaderAt
	io.Writer
	io.Seeker
	Stat() (os.FileInfo, error)
	Fd() uintptr
	Close() error
	Sync() error
}

type archiveFileOps struct {
	mkdirAll        func(string, os.FileMode) error
	openFile        func(string, int, os.FileMode) (archiveFile, error)
	openDir         func(string) (archiveFile, error)
	readDir         func(string) ([]os.DirEntry, error)
	readFile        func(string) ([]byte, error)
	remove          func(string) error
	rename          func(string, string) error
	renameNoReplace func(string, string) error
	writeAll        func(archiveFile, []byte) error
	fdatasync       func(archiveFile) error
	fsync           func(archiveFile) error
	flock           func(archiveFile, int) error
	nowUnixNano     func() int64
}

var journalArchiveFileOps = defaultArchiveFileOps()

func defaultArchiveFileOps() archiveFileOps {
	return archiveFileOps{
		mkdirAll: os.MkdirAll,
		openFile: func(path string, flags int, mode os.FileMode) (archiveFile, error) {
			return os.OpenFile(path, flags, mode)
		},
		openDir:         func(path string) (archiveFile, error) { return os.Open(path) },
		readDir:         os.ReadDir,
		readFile:        os.ReadFile,
		remove:          os.Remove,
		rename:          os.Rename,
		renameNoReplace: renameArchiveNoReplace,
		writeAll: func(file archiveFile, bytes []byte) error {
			for len(bytes) != 0 {
				written, err := file.Write(bytes)
				if err != nil {
					return err
				}
				if written <= 0 || written > len(bytes) {
					return io.ErrShortWrite
				}
				bytes = bytes[written:]
			}
			return nil
		},
		fdatasync:   func(file archiveFile) error { return syscall.Fdatasync(int(file.Fd())) },
		fsync:       func(file archiveFile) error { return file.Sync() },
		flock:       func(file archiveFile, operation int) error { return syscall.Flock(int(file.Fd()), operation) },
		nowUnixNano: func() int64 { return timeNow().UnixNano() },
	}
}

var timeNow = func() time.Time { return time.Now() }

func canonicalArchiveDirectory(directory string) (string, error) {
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return "", fmt.Errorf("resolve archive directory: %w", err)
	}
	return filepath.Clean(absolute), nil
}

func createArchiveDirectory(ops archiveFileOps, directory string, mode os.FileMode) error {
	firstParent := filepath.Dir(directory)
	existingParent := firstParent
	for {
		file, err := ops.openDir(existingParent)
		if err == nil {
			if closeErr := file.Close(); closeErr != nil {
				return fmt.Errorf("close archive parent directory probe: %w", closeErr)
			}
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("probe archive parent directory: %w", err)
		}
		next := filepath.Dir(existingParent)
		if next == existingParent {
			return fmt.Errorf("probe archive parent directory: %w", err)
		}
		existingParent = next
	}
	if err := ops.mkdirAll(directory, mode); err != nil {
		return fmt.Errorf("create archive directory: %w", err)
	}
	for parent := directory; ; parent = filepath.Dir(parent) {
		file, err := ops.openDir(parent)
		if err != nil {
			return fmt.Errorf("open archive parent directory: %w", err)
		}
		syncErr := ops.fsync(file)
		closeErr := file.Close()
		if syncErr != nil || closeErr != nil {
			return fmt.Errorf("sync archive parent directory: %w", errors.Join(syncErr, closeErr))
		}
		if parent == existingParent {
			return nil
		}
	}
}

func renameArchiveNoReplace(oldPath, newPath string) error {
	oldBytes, err := syscall.BytePtrFromString(oldPath)
	if err != nil {
		return err
	}
	newBytes, err := syscall.BytePtrFromString(newPath)
	if err != nil {
		return err
	}
	atFDCWD := ^uintptr(99)
	renameat2 := uintptr(316)
	if runtime.GOARCH == "arm64" {
		renameat2 = 276
	}
	_, _, errno := syscall.Syscall6(
		renameat2,
		atFDCWD,
		uintptr(unsafe.Pointer(oldBytes)),
		atFDCWD,
		uintptr(unsafe.Pointer(newBytes)),
		1,
		0,
	)
	if errno == 0 {
		return nil
	}
	if errno != syscall.ENOSYS && errno != syscall.EINVAL && errno != syscall.EOPNOTSUPP {
		return errno
	}
	if err := os.Link(oldPath, newPath); err != nil {
		return err
	}
	if err := os.Remove(oldPath); err != nil {
		return fmt.Errorf("remove linked temporary file: %w", err)
	}
	return nil
}

type durableTempOptions struct {
	operation string
	noReplace bool
	dataOnly  bool
}

func publishDurableTemp(
	ops archiveFileOps,
	tempPath string,
	finalPath string,
	options durableTempOptions,
	write func(archiveFile) error,
	validate func(archiveFile) error,
) (err error) {
	file, err := ops.openFile(tempPath, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create %s temporary file: %w", options.operation, err)
	}
	tempPresent := true
	fileOpen := true
	defer func() {
		var closeErr error
		if fileOpen {
			closeErr = file.Close()
		}
		if tempPresent {
			removeErr := ops.remove(tempPath)
			if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) && err == nil {
				err = fmt.Errorf("remove %s temporary file: %w", options.operation, removeErr)
			}
		}
		if closeErr != nil && err == nil {
			err = fmt.Errorf("close %s temporary file: %w", options.operation, closeErr)
		}
	}()
	if err = write(file); err != nil {
		return fmt.Errorf("write %s: %w", options.operation, err)
	}
	if validate != nil {
		if _, err = file.Seek(0, io.SeekStart); err != nil {
			return fmt.Errorf("seek %s for validation: %w", options.operation, err)
		}
		if err = validate(file); err != nil {
			return fmt.Errorf("validate %s: %w", options.operation, err)
		}
	}
	if options.dataOnly {
		err = ops.fdatasync(file)
	} else {
		err = ops.fsync(file)
	}
	if err != nil {
		return fmt.Errorf("sync %s: %w", options.operation, err)
	}
	if err = file.Close(); err != nil {
		return fmt.Errorf("close %s: %w", options.operation, err)
	}
	fileOpen = false
	if options.noReplace {
		err = ops.renameNoReplace(tempPath, finalPath)
	} else {
		err = ops.rename(tempPath, finalPath)
	}
	if err != nil {
		return fmt.Errorf("publish %s: %w", options.operation, err)
	}
	tempPresent = false
	return nil
}

func openArchiveLock(ops archiveFileOps, directory string) (archiveFile, error) {
	path := filepath.Join(directory, archiveLockName)
	file, err := ops.openFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	return file, nil
}

func lockArchiveExclusive(ops archiveFileOps, lock archiveFile) error {
	if err := ops.flock(lock, syscall.LOCK_EX); err != nil {
		return fmt.Errorf("lock archive: %w", err)
	}
	return nil
}

func lockArchiveShared(ops archiveFileOps, lock archiveFile) error {
	if err := ops.flock(lock, syscall.LOCK_SH); err != nil {
		return fmt.Errorf("lock archive: %w", err)
	}
	return nil
}

func unlockArchive(ops archiveFileOps, lock archiveFile) error {
	if lock == nil {
		return nil
	}
	if err := ops.flock(lock, syscall.LOCK_UN); err != nil {
		return fmt.Errorf("unlock archive: %w", err)
	}
	return nil
}
