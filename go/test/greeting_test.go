package test

import (
	"testing"

	"github.com/akalsi-org/polyglot-template/go/lib/greeting"
	"github.com/google/go-cmp/cmp"
)

func TestGreeting(t *testing.T) {
	if diff := cmp.Diff("hello, world", greeting.Message("world")); diff != "" {
		t.Fatalf("greeting mismatch (-want +got):\n%s", diff)
	}
}
