package unsafeborrow_test

import (
	"testing"
	"unsafe"

	"github.com/akalsi-org/polyglot-template/go/lib/unsafeborrow"
)

var (
	stringSink string
	bytesSink  []byte
)

func TestUnsafeBorrowStringAliasesInput(t *testing.T) {
	input := []byte("alpha")
	got := unsafeborrow.UnsafeBorrowString(input)

	if unsafe.StringData(got) != unsafe.SliceData(input) {
		t.Fatal("UnsafeBorrowString did not alias its input")
	}

	input[0] = 'A'
	if got != "Alpha" {
		t.Fatalf("borrowed string did not observe input mutation: got %q", got)
	}
}

func TestUnsafeBorrowBytesAliasesInput(t *testing.T) {
	input := string([]byte("alpha"))
	got := unsafeborrow.UnsafeBorrowBytes(input)

	if unsafe.SliceData(got) != unsafe.StringData(input) {
		t.Fatal("UnsafeBorrowBytes did not alias its input")
	}
	if string(got) != input {
		t.Fatalf("borrowed bytes differ from input: got %q, want %q", got, input)
	}
}

func TestCopyStringDoesNotAliasInput(t *testing.T) {
	input := []byte("alpha")
	got := unsafeborrow.CopyString(input)
	input[0] = 'A'

	if got != "alpha" {
		t.Fatalf("copied string changed after input mutation: got %q", got)
	}
}

func TestCopyBytesDoesNotAliasInput(t *testing.T) {
	inputBytes := []byte("alpha")
	input := string(inputBytes)
	got := unsafeborrow.CopyBytes(input)
	got[0] = 'A'

	if input != "alpha" {
		t.Fatalf("input string changed after copy mutation: got %q", input)
	}
}

func TestEmptyValues(t *testing.T) {
	if got := unsafeborrow.UnsafeBorrowString(nil); got != "" {
		t.Fatalf("UnsafeBorrowString(nil) = %q, want empty", got)
	}
	if got := unsafeborrow.UnsafeBorrowBytes(""); len(got) != 0 {
		t.Fatalf("len(UnsafeBorrowBytes(empty)) = %d, want 0", len(got))
	}
	if got := unsafeborrow.CopyString(nil); got != "" {
		t.Fatalf("CopyString(nil) = %q, want empty", got)
	}
	if got := unsafeborrow.CopyBytes(""); len(got) != 0 {
		t.Fatalf("len(CopyBytes(empty)) = %d, want 0", len(got))
	}
}

func TestUnsafeBorrowStringDoesNotAllocate(t *testing.T) {
	input := []byte("allocation test input")
	allocs := testing.AllocsPerRun(1000, func() {
		stringSink = unsafeborrow.UnsafeBorrowString(input)
	})
	if allocs != 0 {
		t.Fatalf("UnsafeBorrowString allocated %v times per call, want 0", allocs)
	}
}

func TestUnsafeBorrowBytesDoesNotAllocate(t *testing.T) {
	input := string([]byte("allocation test input"))
	allocs := testing.AllocsPerRun(1000, func() {
		bytesSink = unsafeborrow.UnsafeBorrowBytes(input)
	})
	if allocs != 0 {
		t.Fatalf("UnsafeBorrowBytes allocated %v times per call, want 0", allocs)
	}
}
