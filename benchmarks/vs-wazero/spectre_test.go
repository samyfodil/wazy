package vswazero

import (
	"testing"

	wasmtime "github.com/bytecodealliance/wasmtime-go/v34"
)

// wasmtime clamps the index with a conditional move on every guarded heap and
// table access, so that a branch mispredicted as in-bounds still reads an
// in-bounds address on the speculative path. Both mitigations are on by default
// (cranelift settings.rs: enable_heap_access_spectre_mitigation,
// enable_table_access_spectre_mitigation). wazy emits the bounds check as a
// plain conditional branch and clamps nothing, so any comparison against
// wasmtime's defaults is charging it for work wazy does not do.
//
// This measures the size of that charge: the same kernels, wasmtime against
// itself, with the two mitigations turned off. Whatever it recovers is the part
// of wazy's lead that is missing hardening rather than better code.
func wtInstanceNoSpectre(tb testing.TB, wasm []byte) (*wasmtime.Store, *wasmtime.Instance) {
	cfg := wasmtime.NewConfig()
	cfg.SetCraneliftFlag("enable_heap_access_spectre_mitigation", "false")
	cfg.SetCraneliftFlag("enable_table_access_spectre_mitigation", "false")
	engine := wasmtime.NewEngineWithConfig(cfg)
	store := wasmtime.NewStore(engine)
	m, err := wasmtime.NewModule(engine, wasm)
	if err != nil {
		tb.Fatal(err)
	}
	inst, err := wasmtime.NewInstance(store, m, nil)
	if err != nil {
		tb.Fatal(err)
	}
	return store, inst
}

func BenchmarkSpectreCost(b *testing.B) {
	for _, k := range execKernelsHeavy() {
		for _, arm := range []struct {
			name string
			inst func(testing.TB, []byte) (*wasmtime.Store, *wasmtime.Instance)
		}{
			{"wasmtime", wtInstance},
			{"wasmtime-nospectre", wtInstanceNoSpectre},
		} {
			b.Run("kernel="+k.name+"/runtime="+arm.name, func(b *testing.B) {
				store, inst := arm.inst(b, k.wasm)
				fn := inst.GetFunc(store, k.fn)
				if fn == nil {
					b.Skipf("no export %q", k.fn)
				}
				var arg any = int32(k.arg)
				if k.is64 {
					arg = int64(k.arg)
				}
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if _, err := fn.Call(store, arg); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}
