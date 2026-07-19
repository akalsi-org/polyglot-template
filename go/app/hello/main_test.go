package main

import (
	"io"
	"os"
	"testing"
)

func TestMainPrintsGreeting(t *testing.T) {
	old := os.Stdout
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = write
	defer func() { os.Stdout = old }()

	main()
	if err := write.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(read)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello, polyglot\n" {
		t.Fatalf("main output = %q", got)
	}
}
