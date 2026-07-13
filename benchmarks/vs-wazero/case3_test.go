package vswazero

import (
	"context"
	"testing"

	wasmtime "github.com/bytecodealliance/wasmtime-go/v34"
)

// caseExports are the real-compiled-code (from Go, via caseWasm) exports and
// their canonical single-i32 args: fibonacci is pure recursion; the others run
// memory-touching loops (base64, string ops, array reverse, matrix multiply).
var caseExports = []struct {
	name string
	arg  int32
}{
	{"fibonacci", 30},
	{"base64", 100},
	{"string_manipulation", 100},
	{"reverse_array", 1000},
	{"random_mat_mul", 20},
}

// wtCaseInstance instantiates caseWasm under wasmtime with WASI (fd_write) and
// the env.get_random_string stub, then runs _start (WASI command init).
func wtCaseInstance(tb testing.TB) (*wasmtime.Store, *wasmtime.Instance) {
	engine := wasmtime.NewEngine()
	store := wasmtime.NewStore(engine)
	store.SetWasi(wasmtime.NewWasiConfig())
	linker := wasmtime.NewLinker(engine)
	if err := linker.DefineWasi(); err != nil {
		tb.Fatal(err)
	}
	if err := linker.Define(store, "env", "get_random_string", wasmtime.WrapFunc(store, func(a, b int32) {})); err != nil {
		tb.Fatal(err)
	}
	m, err := wasmtime.NewModule(engine, caseWasm)
	if err != nil {
		tb.Fatal(err)
	}
	inst, err := linker.Instantiate(store, m)
	if err != nil {
		tb.Fatal(err)
	}
	if start := inst.GetFunc(store, "_start"); start != nil {
		_, _ = start.Call(store) // ignore proc_exit(0)
	}
	return store, inst
}

// BenchmarkCase3 runs caseWasm's real-compiled-code exports on all three
// runtimes (wazy / wazero / wasmtime), in-process and amortized.
func BenchmarkCase3(b *testing.B) {
	ctx := context.Background()
	for _, e := range caseExports {
		e := e
		b.Run("fn="+e.name+"/runtime=wazy", func(b *testing.B) {
			r := newCaseRuntimeWazy(b)
			defer r.Close(ctx)
			mod, err := r.Instantiate(ctx, caseWasm)
			if err != nil {
				b.Fatal(err)
			}
			fn := mod.ExportedFunction(e.name)
			stack := make([]uint64, 1)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				stack[0] = uint64(uint32(e.arg))
				if err := fn.CallWithStack(ctx, stack); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run("fn="+e.name+"/runtime=wazero", func(b *testing.B) {
			r := newCaseRuntimeWazero(b)
			defer r.Close(ctx)
			mod, err := r.Instantiate(ctx, caseWasm)
			if err != nil {
				b.Fatal(err)
			}
			fn := mod.ExportedFunction(e.name)
			stack := make([]uint64, 1)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				stack[0] = uint64(uint32(e.arg))
				if err := fn.CallWithStack(ctx, stack); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run("fn="+e.name+"/runtime=wasmtime", func(b *testing.B) {
			store, inst := wtCaseInstance(b)
			fn := inst.GetFunc(store, e.name)
			if fn == nil {
				b.Fatalf("no export %q", e.name)
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := fn.Call(store, e.arg); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
