//go:build !openbsd

package platform

// MakeStack returns a zeroed buffer of at least n bytes for JIT code to use as
// its stack. Only OpenBSD needs more than a heap allocation; see
// stack_openbsd.go.
func MakeStack(n int) []byte {
	return make([]byte, n)
}
