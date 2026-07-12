package test

import (
	"testing"

	"github.com/akalsi-org/polyglot-template/go/lib/greeting"
)

func TestGreeting(t *testing.T) {
	if got := greeting.Message("world"); got != "hello, world" {
		t.Fatalf("unexpected greeting: %q", got)
	}
}
