// Package filewin maps a contiguous sliding window into a shared file.
//
// The file is an append-only data log after a fixed header.
// The header page is mapped by itself. Writer and reader map only the data
// section. WriterWindow is larger than the reader window so growth and remap
// happen less often on the write path.
//
// A writer helper goroutine extends the file in linear extents and prefaults
// pages ahead of write_pos. It is not pinned.
//
// Each process maps its own window. A remap is local. Other processes map the
// same file offset themselves.
package filewin
