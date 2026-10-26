package vswazero

import (
	"context"
	_ "embed"
	"testing"

	"github.com/tetratelabs/wazero"

	"github.com/samyfodil/wazy"
)

// These benchmarks measure what it costs to be able to interrupt a running
// guest. wazy always can: its compiled code checks Go's preemption word, and a
// done context ends the call. wazero can only under WithCloseOnContextDone, so
// it runs with that option on, the comparable configuration, and off, as the
// cost of its code with no check at all.

// spinWasm exports "spin" (param i64 n) (result i64): a tight countdown loop
// with a near-empty body, so whatever check the loop makes dominates.
//
//go:embed testdata/spin.wasm
var spinWasm []byte

// spinIters is the loop trip count per call: large enough that the check
// overhead dominates.
const spinIters = uint64(100_000)

// callable is the subset of both wazy and wazero api.Function used here.
type callable interface {
	CallWithStack(context.Context, []uint64) error
}

// wazeroModes are the two configurations wazero is measured in.
var wazeroModes = []struct {
	label   string
	closeOn bool
}{
	{"runtime=wazero/close=on", true},
	{"runtime=wazero/close=off", false},
}

// BenchmarkInterruptibleSpin runs the spin kernel.
func BenchmarkInterruptibleSpin(b *testing.B) {
	ctx := context.Background()

	run := func(b *testing.B, fn callable) {
		stack := make([]uint64, 1)
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			stack[0] = spinIters
			if err := fn.CallWithStack(ctx, stack); err != nil {
				b.Fatal(err)
			}
		}
	}

	b.Run("runtime=wazy", func(b *testing.B) {
		r := wazy.NewRuntimeWithConfig(ctx, wazy.NewRuntimeConfigCompiler())
		defer r.Close(ctx)
		mod, err := r.Instantiate(ctx, spinWasm)
		if err != nil {
			b.Fatal(err)
		}
		run(b, mod.ExportedFunction("spin"))
	})
	for _, mode := range wazeroModes {
		b.Run(mode.label, func(b *testing.B) {
			cfg := wazero.NewRuntimeConfigCompiler().WithCloseOnContextDone(mode.closeOn)
			r := wazero.NewRuntimeWithConfig(ctx, cfg)
			defer r.Close(ctx)
			mod, err := r.Instantiate(ctx, spinWasm)
			if err != nil {
				b.Fatal(err)
			}
			run(b, mod.ExportedFunction("spin"))
		})
	}
}

// hostLoopWasm exports "work" (param i32 n): a loop that calls the imported
// env.cb (identity) each iteration -- a realistic loop with a non-trivial body.
// This is the case where the check should be near-free: the host call
// dominates.
//
//go:embed testdata/hostloop.wasm
var hostLoopWasm []byte

// BenchmarkInterruptibleHostCallLoop runs a loop whose body makes a host call
// (n=10000). Contrast with spin/fib: here the body is realistic, so the check
// overhead should be a few percent, not 2-3x.
func BenchmarkInterruptibleHostCallLoop(b *testing.B) {
	ctx := context.Background()
	const n = uint64(10000)

	run := func(b *testing.B, fn callable) {
		stack := make([]uint64, 1)
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			stack[0] = n
			if err := fn.CallWithStack(ctx, stack); err != nil {
				b.Fatal(err)
			}
		}
	}

	b.Run("runtime=wazy", func(b *testing.B) {
		r, fn := newHostLoopWazy(b, hostLoopWasm)
		defer r.Close(ctx)
		run(b, fn)
	})
	for _, mode := range wazeroModes {
		b.Run(mode.label, func(b *testing.B) {
			r, fn := newHostLoopWazero(b, hostLoopWasm, mode.closeOn)
			defer r.Close(ctx)
			run(b, fn)
		})
	}
}

// BenchmarkInterruptibleFib runs the real caseWasm fibonacci export (fib=30).
// Unlike the near-empty spin body, fibonacci is a realistic workload: its
// heavier body dilutes the per-check overhead. Recursion still pays, since a
// recursive function is checked on entry.
func BenchmarkInterruptibleFib(b *testing.B) {
	ctx := context.Background()
	const n = uint64(30)

	run := func(b *testing.B, fn callable) {
		stack := make([]uint64, 1)
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			stack[0] = n
			if err := fn.CallWithStack(ctx, stack); err != nil {
				b.Fatal(err)
			}
		}
	}

	b.Run("runtime=wazy", func(b *testing.B) {
		r := newCaseRuntimeWazy(b)
		defer r.Close(ctx)
		mod, err := r.Instantiate(ctx, caseWasm)
		if err != nil {
			b.Fatal(err)
		}
		run(b, mod.ExportedFunction("fibonacci"))
	})
	for _, mode := range wazeroModes {
		b.Run(mode.label, func(b *testing.B) {
			var r wazero.Runtime
			if mode.closeOn {
				r = newCaseRuntimeWazeroClose(b)
			} else {
				r = newCaseRuntimeWazero(b)
			}
			defer r.Close(ctx)
			mod, err := r.Instantiate(ctx, caseWasm)
			if err != nil {
				b.Fatal(err)
			}
			run(b, mod.ExportedFunction("fibonacci"))
		})
	}
}
