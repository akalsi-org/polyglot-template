//go:build linux && (amd64 || arm64) && (!go1.27 || go1.28)

package hostcpu

// CurrentThreadIdentity and CurrentThreadId depend on private Go 1.27 layouts.
// A toolchain upgrade must derive and review new architecture offsets first.
var _ go127RuntimeLayoutRequired
