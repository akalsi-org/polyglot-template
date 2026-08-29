// Package filewin maps a contiguous sliding window into a shared file.
//
// The file is an append-only data log after a fixed header.
// A writer helper goroutine extends the file in linear extents and prefaults
// pages ahead of write_pos. It is not pinned.
//
// Each process maps its own window. A remap is local. Other processes map the
// same file offset themselves.
package filewin
