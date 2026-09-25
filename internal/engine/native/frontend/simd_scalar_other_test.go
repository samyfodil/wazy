//go:build !riscv64

package frontend

// pinVectorLowering has nothing to pin: emulateSIMD is a constant false on this
// build, so the vector lowering is the only one there is. It exists so a test
// shared with riscv64 can ask for it without knowing which architecture it is on.
func pinVectorLowering() func() { return func() {} }
