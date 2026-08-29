//go:build linux && (amd64 || arm64)

package journal

import (
	"errors"
	"syscall"
	"testing"

	"github.com/akalsi-org/polyglot-template/go/lib/hostcpu"
)

func TestPIDNamespaceMetadataValidation(t *testing.T) {
	journal := newTestJournal(t, 1<<16, 4096)
	current, err := hostcpu.CurrentPIDNamespaceIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if got := get64(journal.region.control, JournalHeaderPIDNamespaceDevOffset); got != current.Dev {
		t.Fatalf("creator namespace device = %d, want %d", got, current.Dev)
	}
	if got := get64(journal.region.control, JournalHeaderPIDNamespaceInodeOffset); got != current.Ino {
		t.Fatalf("creator namespace inode = %d, want %d", got, current.Ino)
	}

	fd, err := journal.DupFD()
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fd)
	put64(journal.region.control, JournalHeaderPIDNamespaceInodeOffset, current.Ino+1)
	attached, err := Attach(fd)
	if attached != nil {
		_ = attached.Close()
		t.Fatal("Attach accepted a different PID namespace")
	}
	if !errors.Is(err, ErrPIDNamespace) {
		t.Fatalf("Attach error = %v, want ErrPIDNamespace", err)
	}
	var mismatch *PIDNamespaceError
	if !errors.As(err, &mismatch) {
		t.Fatalf("Attach error type = %T, want *PIDNamespaceError", err)
	}
	if mismatch.CreatorDev != current.Dev || mismatch.CreatorIno != current.Ino+1 ||
		mismatch.CurrentDev != current.Dev || mismatch.CurrentIno != current.Ino {
		t.Fatalf("PID namespace diagnostics = %+v", mismatch)
	}
}
