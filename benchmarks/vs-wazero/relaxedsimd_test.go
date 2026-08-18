package vswazero

import (
	"context"
	_ "embed"
	"math"
	"testing"

	wasmtime "github.com/bytecodealliance/wasmtime-go/v34"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/api"
)

//go:embed testdata/relaxedsimd.wasm
var relaxedSimdWasm []byte

// Relaxed SIMD lets a runtime return any one of several results per instruction
// so it can pick the cheapest native sequence. wazy instead returns one
// documented result everywhere, which costs it instructions on exactly three
// instructions; wasmtime takes the fast per-host route. These benchmarks price
// that difference against wasmtime, which ships relaxed SIMD as tier 1.
//
// The iteration count is fixed so a b.N-independent per-iteration cost falls out
// of ns/op: divide by relaxedIters to get the cost of one chained lowering.
const relaxedIters = 10000

var relaxedFuncs = []string{"madd", "minmax", "dot", "dotmem"}

func wazyRelaxedInstance(tb testing.TB) (context.Context, api.Module) {
	ctx := context.Background()
	r := wazy.NewRuntimeWithConfig(ctx, wazy.NewRuntimeConfig().
		WithCoreFeatures(api.CoreFeaturesV2|api.CoreFeatureRelaxedSIMD))
	tb.Cleanup(func() { r.Close(ctx) })
	mod, err := r.Instantiate(ctx, relaxedSimdWasm)
	if err != nil {
		tb.Fatal(err)
	}
	return ctx, mod
}

func wasmtimeRelaxedInstance(tb testing.TB, deterministic bool) (*wasmtime.Store, *wasmtime.Instance) {
	cfg := wasmtime.NewConfig()
	cfg.SetWasmSIMD(true)
	cfg.SetWasmRelaxedSIMD(true)
	// wasmtime defaults to the fast per-host lowerings. Its deterministic mode
	// pins the same results wazy always returns, so it is the arm that compares
	// like with like; the default arm shows what the freedom is worth.
	cfg.SetWasmRelaxedSIMDDeterministic(deterministic)
	engine := wasmtime.NewEngineWithConfig(cfg)
	store := wasmtime.NewStore(engine)
	m, err := wasmtime.NewModule(engine, relaxedSimdWasm)
	if err != nil {
		tb.Fatal(err)
	}
	inst, err := wasmtime.NewInstance(store, m, nil)
	if err != nil {
		tb.Fatal(err)
	}
	return store, inst
}

// TestRelaxedSimdParity keeps the benchmark honest: both runtimes must run the
// module and compute the same thing, or the numbers below compare nothing.
//
// "The same thing" is exact for minmax and dot, whose operands avoid the cases
// the proposal leaves open. It cannot be exact for madd: wasmtime contracts the
// multiply and the add into an FMA and wazy rounds them separately, so ten
// thousand chained iterations drift apart in the last bits by construction.
// That is the difference these benchmarks exist to price, so the check there is
// that the two stay close, not that they agree.
func TestRelaxedSimdParity(t *testing.T) {
	ctx, mod := wazyRelaxedInstance(t)
	store, inst := wasmtimeRelaxedInstance(t, true)
	for _, name := range relaxedFuncs {
		got, err := mod.ExportedFunction(name).Call(ctx, relaxedIters)
		if err != nil {
			t.Fatalf("wazy %s: %v", name, err)
		}
		want, err := inst.GetFunc(store, name).Call(store, int32(relaxedIters))
		if err != nil {
			t.Fatalf("wasmtime %s: %v", name, err)
		}
		switch v := want.(type) {
		case float32:
			gotF, wantF := api.DecodeF32(got[0]), v
			if name == "madd" {
				if d := math.Abs(float64(gotF-wantF)) / math.Abs(float64(wantF)); d > 1e-6 {
					t.Errorf("madd: wazy %v, wasmtime %v, relative difference %g", gotF, wantF, d)
				}
				continue
			}
			if got[0] != uint64(api.EncodeF32(v)) {
				t.Errorf("%s: wazy %v, wasmtime %v", name, gotF, wantF)
			}
		case int32:
			if got[0] != uint64(uint32(v)) {
				t.Errorf("%s: wazy %#x, wasmtime %#x", name, got[0], uint32(v))
			}
		default:
			t.Fatalf("%s: unexpected wasmtime result %T", name, want)
		}
	}
}

func BenchmarkRelaxedSimd(b *testing.B) {
	for _, name := range relaxedFuncs {
		b.Run("fn="+name+"/runtime=wazy", func(b *testing.B) {
			ctx, mod := wazyRelaxedInstance(b)
			fn := mod.ExportedFunction(name)
			stack := make([]uint64, 1)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				stack[0] = relaxedIters
				if err := fn.CallWithStack(ctx, stack); err != nil {
					b.Fatal(err)
				}
			}
		})
		for _, wt := range []struct {
			label         string
			deterministic bool
		}{{"wasmtime", false}, {"wasmtime-det", true}} {
			b.Run("fn="+name+"/runtime="+wt.label, func(b *testing.B) {
				store, inst := wasmtimeRelaxedInstance(b, wt.deterministic)
				fn := inst.GetFunc(store, name)
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if _, err := fn.Call(store, int32(relaxedIters)); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}
