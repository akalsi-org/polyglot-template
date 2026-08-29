//go:build linux && (amd64 || arm64)

package atmc

// Relax gives the processor a spin-loop hint.
//
// Go has no intrinsic for this instruction, so this function uses assembly.
//
//go:noescape
func Relax()

// LoadBarrier prevents later memory operations from moving before this call.
//
// Go has no intrinsic for a standalone acquire fence, so this function uses assembly.
//
//go:noescape
func LoadBarrier()

// StoreBarrier prevents earlier memory operations from moving after this call.
//
// Go has no intrinsic for a standalone release fence, so this function uses assembly.
//
//go:noescape
func StoreBarrier()

// FullBarrier orders all earlier memory operations before all later memory operations.
//
// Go has no intrinsic for a standalone full fence, so this function uses assembly.
//
//go:noescape
func FullBarrier()
