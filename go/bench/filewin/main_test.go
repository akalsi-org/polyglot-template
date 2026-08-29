package main

import (
	"os"
	"syscall"
	"testing"
)

func TestPercentile(t *testing.T) {
	values := []float64{1, 2, 3, 4, 5}
	if got := percentile(values, 0.50); got != 3 {
		t.Fatalf("p50 = %v, want 3", got)
	}
	if got := percentile(values, 1); got != 5 {
		t.Fatalf("max = %v, want 5", got)
	}
	if got := percentile(nil, 0.50); got != 0 {
		t.Fatalf("empty = %v, want 0", got)
	}
}

func TestParsePayloads(t *testing.T) {
	got, err := parsePayloads("64, 256,4096")
	if err != nil {
		t.Fatal(err)
	}
	want := []int{64, 256, 4096}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	if _, err := parsePayloads("4"); err == nil {
		t.Fatal("accepted a payload smaller than the timestamp")
	}
}

func TestRequireDiskRejectsRamFilesystem(t *testing.T) {
	var st syscall.Statfs_t
	if err := syscall.Statfs("/tmp", &st); err != nil {
		t.Fatal(err)
	}
	_, err := requireDisk("/tmp")
	if isRamFS(st.Type) {
		if err == nil {
			t.Fatal("tmpfs /tmp was accepted as disk")
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
}

func TestRequireDiskAcceptsWorkspace(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	name, err := requireDisk(wd)
	if err != nil {
		t.Fatal(err)
	}
	if name == "tmpfs" || name == "ramfs" {
		t.Fatalf("workspace classified as %s", name)
	}
}
