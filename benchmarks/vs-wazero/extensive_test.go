package vswazero

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/tetratelabs/wazero"

	"github.com/samyfodil/wazy"
)

// caseWorkloads are real TinyGo-compiled exports of caseWasm. The memory-heavy
// ones (base64, reverse_array, random_mat_mul, string_manipulation) exercise
// bounds checks and memory ops — the paths touched by C21 (#2514) and C22
// (#2515) — unlike the pure-compute fibonacci. Args mirror the canonical
// wazero bench (internal/integration_test/bench).
var caseWorkloads = []struct {
	name string
	arg  uint64
}{
	{"fibonacci", 30},
	{"base64", 100},
	{"string_manipulation", 100},
	{"reverse_array", 1000},
	{"random_mat_mul", 20},
}

// BenchmarkCaseWorkloads runs every caseWasm workload on wazy and wazero from
// a single instantiation each. Compare runtime=wazy vs runtime=wazero.
func BenchmarkCaseWorkloads(b *testing.B) {
	ctx := context.Background()

	// wazy.
	rWazy := newCaseRuntimeWazy(b)
	modWazy, err := rWazy.Instantiate(ctx, caseWasm)
	if err != nil {
		b.Fatal(err)
	}
	// wazero.
	rWazero := newCaseRuntimeWazero(b)
	modWazero, err := rWazero.Instantiate(ctx, caseWasm)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { rWazy.Close(ctx); rWazero.Close(ctx) })

	for _, w := range caseWorkloads {
		fnWazy := modWazy.ExportedFunction(w.name)
		fnWazero := modWazero.ExportedFunction(w.name)
		arg := w.arg
		b.Run(fmt.Sprintf("%s/runtime=wazy", w.name), func(b *testing.B) {
			stack := make([]uint64, 1)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				stack[0] = arg
				if err := fnWazy.CallWithStack(ctx, stack); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(fmt.Sprintf("%s/runtime=wazero", w.name), func(b *testing.B) {
			stack := make([]uint64, 1)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				stack[0] = arg
				if err := fnWazero.CallWithStack(ctx, stack); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// compileModules are diverse real compiler outputs (TinyGo, Rust, Zig, Zig-cc)
// read from the repo tree. Compilation exercises the full backend — decode,
// SSA, lowering, regalloc, finalize — across realistic bounds-check-dense
// code, the primary target of C22 (#2515, code size) and C21 (#2514).
var compileModules = []struct {
	name string
	path string
}{
	{"greet_tinygo_370k", "../../examples/allocation/tinygo/testdata/greet.wasm"},
	{"greet_rust_10k", "../../examples/allocation/rust/testdata/greet.wasm"},
	{"greet_zig_5k", "../../examples/allocation/zig/testdata/greet.wasm"},
	{"wasi_zigcc_786k", "../../imports/wasi_snapshot_preview1/testdata/zig-cc/wasi.wasm"},
	{"wasi_cargo_104k", "../../imports/wasi_snapshot_preview1/testdata/cargo-wasi/wasi.wasm"},
}

// BenchmarkCompileModulesExtensive compiles each real module on a fresh
// runtime every iteration (no cache) on wazy and wazero.
func BenchmarkCompileModulesExtensive(b *testing.B) {
	ctx := context.Background()
	for _, mod := range compileModules {
		src, err := os.ReadFile(mod.path)
		if err != nil {
			b.Skipf("%s: %v", mod.name, err)
		}
		b.Run(fmt.Sprintf("%s/runtime=wazy", mod.name), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				r := wazy.NewRuntimeWithConfig(ctx, wazy.NewRuntimeConfigCompiler())
				if _, err := r.CompileModule(ctx, src); err != nil {
					b.Fatal(err)
				}
				r.Close(ctx)
			}
		})
		b.Run(fmt.Sprintf("%s/runtime=wazero", mod.name), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				r := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfigCompiler())
				if _, err := r.CompileModule(ctx, src); err != nil {
					b.Fatal(err)
				}
				r.Close(ctx)
			}
		})
	}
}
