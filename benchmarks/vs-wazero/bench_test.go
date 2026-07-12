package vswazero

import (
	"fmt"
	"math"
	"testing"
)

// hostCallOffset/hostCallVal are the fixed inputs for the host-call workload:
// the f32 bits of hostCallVal are written to guest memory at hostCallOffset,
// and each guest export reads them back through its host function.
const (
	hostCallOffset = uint32(100)
	hostCallVal    = float32(1.1234)
)

// runtimeSetups pairs a runtime label with its host-call fixture constructor.
// Declared once so every driver iterates the two runtimes in the same order.
var hostCallRuntimes = []struct {
	name  string
	setup func(testing.TB) hostCallEnv
}{
	{"wazy", setupHostCallWazy},
	{"wazero", setupHostCallWazero},
}

// BenchmarkHostCall is the headline benchmark: a guest function that forwards a
// u32 offset to an imported host function which reads 4 bytes of guest memory
// and returns them as an f32. It sweeps:
//
//   - host: "gomodule" (WithGoModuleFunction on both) vs "typed"
//     (wazy HostFunc1 / wazero WithFunc reflection)
//   - op:   Call (allocates a result slice) vs CallWithStack (reuses a stack)
//   - runtime: wazy vs wazero
func BenchmarkHostCall(b *testing.B) {
	bits := math.Float32bits(hostCallVal)
	for _, rt := range hostCallRuntimes {
		env := rt.setup(b)
		env.writeMem(hostCallOffset, bits)
		for _, host := range []string{"gomodule", "typed"} {
			fn := env.fns[host]
			b.Run(fmt.Sprintf("host=%s/op=Call/runtime=%s", host, rt.name), func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					res, err := fn.Call(benchCtx, uint64(hostCallOffset))
					if err != nil {
						b.Fatal(err)
					}
					if uint32(res[0]) != bits {
						b.Fatalf("got %#x, want %#x", uint32(res[0]), bits)
					}
				}
			})
			b.Run(fmt.Sprintf("host=%s/op=CallWithStack/runtime=%s", host, rt.name), func(b *testing.B) {
				b.ReportAllocs()
				stack := make([]uint64, 1)
				for i := 0; i < b.N; i++ {
					stack[0] = uint64(hostCallOffset)
					if err := fn.CallWithStack(benchCtx, stack); err != nil {
						b.Fatal(err)
					}
					if uint32(stack[0]) != bits {
						b.Fatalf("got %#x, want %#x", uint32(stack[0]), bits)
					}
				}
			})
		}
		env.close()
	}
}

// BenchmarkCompile compiles the real TinyGo caseWasm module (compiler engine,
// no compilation cache: every iteration recompiles).
func BenchmarkCompile(b *testing.B) {
	b.Run("runtime=wazy", benchCompileWazy)
	b.Run("runtime=wazero", benchCompileWazero)
}

// BenchmarkInstantiate measures per-instance cost of caseWasm after a single
// compile (start function skipped).
func BenchmarkInstantiate(b *testing.B) {
	b.Run("runtime=wazy", benchInstantiateWazy)
	b.Run("runtime=wazero", benchInstantiateWazero)
}

// BenchmarkExecute runs the pure-compute fibonacci export of caseWasm.
func BenchmarkExecute(b *testing.B) {
	for _, rt := range []struct {
		name  string
		setup func(testing.TB) (benchFn, func())
	}{
		{"wazy", setupExecuteWazy},
		{"wazero", setupExecuteWazero},
	} {
		fn, cleanup := rt.setup(b)
		for _, n := range []uint64{20, 30} {
			b.Run(fmt.Sprintf("fib=%d/runtime=%s", n, rt.name), func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					if _, err := fn.Call(benchCtx, n); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
		cleanup()
	}
}
