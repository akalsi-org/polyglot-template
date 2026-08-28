// Package unsafeborrow provides explicit no-copy conversions between strings
// and byte slices.
//
// The UnsafeBorrow functions share storage with their inputs. The caller keeps
// ownership of that storage. A borrowed value can escape the calling function,
// and its pointer keeps Go-managed storage live. The caller must not use these
// functions with storage whose lifetime is managed outside Go.
//
// Treat every borrowed value as read-only. Do not mutate an input byte slice
// while its borrowed string exists. Do not mutate a borrowed byte slice.
// Mutation can corrupt values, cause data races, or fault on read-only storage.
package unsafeborrow

import "unsafe"

// UnsafeBorrowString returns a string that aliases b without an allocation.
//
// The caller retains ownership of b. The caller must not mutate b while the
// returned string exists. The caller must also prevent concurrent mutation.
// The returned string keeps Go-managed backing storage live if it escapes.
func UnsafeBorrowString(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	return unsafe.String(unsafe.SliceData(b), len(b))
}

// UnsafeBorrowBytes returns a read-only byte view that aliases s.
// It returns nil when s is empty.
//
// The caller retains ownership of s. The caller must not mutate the returned
// slice. String storage can reside in read-only memory, so mutation can fault.
// The returned slice keeps Go-managed string storage live if it escapes.
func UnsafeBorrowBytes(s string) []byte {
	if len(s) == 0 {
		return nil
	}
	return unsafe.Slice(unsafe.StringData(s), len(s))
}

// CopyString returns a string with storage independent from b.
func CopyString(b []byte) string {
	return string(b)
}

// CopyBytes returns a byte slice with storage independent from s.
func CopyBytes(s string) []byte {
	return []byte(s)
}
