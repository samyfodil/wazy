package vswazero

import (
	"context"
	_ "embed"
	"testing"

	"github.com/tetratelabs/wazero"

	"github.com/samyfodil/wazy"
)

// spinWasm exports "spin" (param i64 n) (result i64): a tight countdown loop
// with a near-empty body, so the per-back-edge interrupt check (emitted only
// under WithCloseOnContextDone) dominates. This is the workload that exercises
// H6: wazy's check is two loads and a predicted-not-taken branch; wazero@main
// does a Go round-trip every iteration.
//
//go:embed testdata/spin.wasm
var spinWasm []byte

// spinIters is the loop trip count per call: large enough that the check
// overhead (one Go round-trip per checked iteration) dominates.
const spinIters = uint64(100_000)

// callable is the subset of both wazy and wazero api.Function used here.
type callable interface {
	CallWithStack(context.Context, []uint64) error
}

// BenchmarkCloseOnContextDone compares a tight loop with and without
// WithCloseOnContextDone on wazy and wazero.
//
//   - close=off: no interrupt check emitted (baseline loop cost).
//   - close=on:  wazy loads the module-closed word and branches; wazero
//     checks every iteration.
func BenchmarkCloseOnContextDone(b *testing.B) {
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

	for _, closeOn := range []bool{true, false} {
		label := "close=off"
		if closeOn {
			label = "close=on"
		}

		b.Run(label+"/runtime=wazy", func(b *testing.B) {
			cfg := wazy.NewRuntimeConfigCompiler()
			if closeOn {
				cfg = cfg.WithCloseOnContextDone(true)
			}
			r := wazy.NewRuntimeWithConfig(ctx, cfg)
			defer r.Close(ctx)
			mod, err := r.Instantiate(ctx, spinWasm)
			if err != nil {
				b.Fatal(err)
			}
			run(b, mod.ExportedFunction("spin"))
		})

		b.Run(label+"/runtime=wazero", func(b *testing.B) {
			cfg := wazero.NewRuntimeConfigCompiler()
			if closeOn {
				cfg = cfg.WithCloseOnContextDone(true)
			}
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
// env.cb (identity) each iteration — a realistic loop with a non-trivial body.
// This is the case where the interrupt check should be near-free: the host
// call dominates, so the per-iteration check is a small fraction.
//
//go:embed testdata/hostloop.wasm
var hostLoopWasm []byte

// BenchmarkHostCallLoopCloseOnContextDone measures the interrupt-check overhead
// on a loop whose body makes a host call (n=10000), with and without
// WithCloseOnContextDone, on wazy and wazero. Contrast with spin/fib: here the
// body is realistic, so the check overhead should be a few percent, not 2-3x.
func BenchmarkHostCallLoopCloseOnContextDone(b *testing.B) {
	ctx := context.Background()
	const n = uint64(10000)

	for _, closeOn := range []bool{true, false} {
		label := "close=off"
		if closeOn {
			label = "close=on"
		}
		b.Run(label+"/runtime=wazy", func(b *testing.B) {
			r, fn := newHostLoopWazy(b, hostLoopWasm, closeOn)
			defer r.Close(ctx)
			stack := make([]uint64, 1)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				stack[0] = n
				if err := fn.CallWithStack(ctx, stack); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(label+"/runtime=wazero", func(b *testing.B) {
			r, fn := newHostLoopWazero(b, hostLoopWasm, closeOn)
			defer r.Close(ctx)
			stack := make([]uint64, 1)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				stack[0] = n
				if err := fn.CallWithStack(ctx, stack); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkFibCloseOnContextDone runs the real caseWasm fibonacci export
// (fib=30) with and without WithCloseOnContextDone. Unlike the near-empty spin
// body, fibonacci is a realistic workload: its heavier body dilutes the
// per-check overhead relative to spin. Pure recursion still pays, since the
// check is emitted at function entry as well as at loop back-edges.
func BenchmarkFibCloseOnContextDone(b *testing.B) {
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

	for _, closeOn := range []bool{true, false} {
		label := "close=off"
		if closeOn {
			label = "close=on"
		}

		b.Run(label+"/runtime=wazy", func(b *testing.B) {
			var r wazy.Runtime
			if closeOn {
				r = newCaseRuntimeWazyClose(b)
			} else {
				r = newCaseRuntimeWazy(b)
			}
			defer r.Close(ctx)
			mod, err := r.Instantiate(ctx, caseWasm)
			if err != nil {
				b.Fatal(err)
			}
			run(b, mod.ExportedFunction("fibonacci"))
		})

		b.Run(label+"/runtime=wazero", func(b *testing.B) {
			var r wazero.Runtime
			if closeOn {
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
