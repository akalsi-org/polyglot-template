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
	defer read.Close()

	// Drain concurrently: a pipe holds only ~64KB before writes block, so
	// reading after main() returns deadlocks the moment the program under
	// test prints more than that.
	type readResult struct {
		data []byte
		err  error
	}
	done := make(chan readResult, 1)
	go func() {
		data, err := io.ReadAll(read)
		done <- readResult{data: data, err: err}
	}()

	main()
	if err := write.Close(); err != nil {
		t.Fatal(err)
	}
	result := <-done
	if result.err != nil {
		t.Fatal(result.err)
	}
	got := result.data
	if string(got) != "hello, polyglot\n" {
		t.Fatalf("main output = %q", got)
	}
}
