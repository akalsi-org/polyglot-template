// Package filewin maps a contiguous sliding window into a shared file.
//
// The file is an append-only data log after a fixed header.
// The header page is mapped by itself. Writer and reader map only the data
// section. WriterWindow is larger than the reader window so growth and remap
// happen less often on the write path.
//
// Commit publishes with one release store of the write cursor. A reader
// observes that store with Peek. The writer does not wake anyone, and a
// reader does not wait in the kernel.
//
// Each process maps its own window. A remap is local. Other processes map the
// same file offset themselves.
package filewin
