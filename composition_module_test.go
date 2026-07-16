package wazy

import (
	"context"
	"testing"

	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/testing/binaryencoding"
	"github.com/samyfodil/wazy/internal/wasm"
)

// TestModuleComposition is the plain-core-module analogue of the component
// composition test: load module A (exports a()->5), then module B that imports
// A and exports b()->a()+10, then module C that imports BOTH A and B and exports
// c()->a()+b(). Calling C.c drives C->B->A and C->A directly, entirely through
// native wasm import linkage resolved from the Runtime's module registry (no
// host bridge). Confirms multi-module composition on one Runtime is unaffected
// by the component engine's anonymous-internals changes.
func TestModuleComposition(t *testing.T) {
	ctx := context.Background()
	r := NewRuntime(ctx)
	defer r.Close(ctx)

	vToI32 := wasm.FunctionType{Results: []wasm.ValueType{wasm.ValueTypeI32}}

	// A: func a() i32 { return 5 }
	modA := &wasm.Module{
		TypeSection:     []wasm.FunctionType{vToI32},
		FunctionSection: []wasm.Index{0},
		CodeSection:     []wasm.Code{{Body: []byte{wasm.OpcodeI32Const, 5, wasm.OpcodeEnd}}},
		ExportSection:   []wasm.Export{{Name: "a", Type: wasm.ExternTypeFunc, Index: 0}},
	}
	// B imports A.a; func b() i32 { return a() + 10 } (a is func index 0)
	modB := &wasm.Module{
		TypeSection:         []wasm.FunctionType{vToI32},
		ImportFunctionCount: 1,
		ImportSection:       []wasm.Import{{Type: wasm.ExternTypeFunc, Module: "A", Name: "a", DescFunc: 0}},
		FunctionSection:     []wasm.Index{0},
		CodeSection:         []wasm.Code{{Body: []byte{wasm.OpcodeCall, 0, wasm.OpcodeI32Const, 10, wasm.OpcodeI32Add, wasm.OpcodeEnd}}},
		ExportSection:       []wasm.Export{{Name: "b", Type: wasm.ExternTypeFunc, Index: 1}},
	}
	// C imports A.a and B.b; func c() i32 { return a() + b() } (a=0, b=1)
	modC := &wasm.Module{
		TypeSection:         []wasm.FunctionType{vToI32},
		ImportFunctionCount: 2,
		ImportSection: []wasm.Import{
			{Type: wasm.ExternTypeFunc, Module: "A", Name: "a", DescFunc: 0},
			{Type: wasm.ExternTypeFunc, Module: "B", Name: "b", DescFunc: 0},
		},
		FunctionSection: []wasm.Index{0},
		CodeSection:     []wasm.Code{{Body: []byte{wasm.OpcodeCall, 0, wasm.OpcodeCall, 1, wasm.OpcodeI32Add, wasm.OpcodeEnd}}},
		ExportSection:   []wasm.Export{{Name: "c", Type: wasm.ExternTypeFunc, Index: 2}},
	}

	a, err := r.InstantiateWithConfig(ctx, binaryencoding.EncodeModule(modA), NewModuleConfig().WithName("A"))
	if err != nil {
		t.Fatalf("load A: %v", err)
	}
	b, err := r.InstantiateWithConfig(ctx, binaryencoding.EncodeModule(modB), NewModuleConfig().WithName("B"))
	if err != nil {
		t.Fatalf("load B (imports A): %v", err)
	}
	c, err := r.InstantiateWithConfig(ctx, binaryencoding.EncodeModule(modC), NewModuleConfig().WithName("C"))
	if err != nil {
		t.Fatalf("load C (imports A and B): %v", err)
	}

	call := func(m api.Module, fn string) uint32 {
		res, err := m.ExportedFunction(fn).Call(ctx)
		if err != nil {
			t.Fatalf("%s: %v", fn, err)
		}
		return uint32(res[0])
	}
	if got := call(a, "a"); got != 5 {
		t.Fatalf("A.a = %d, want 5", got)
	}
	if got := call(b, "b"); got != 15 { // a()+10
		t.Fatalf("B.b = %d, want 15 (calls A)", got)
	}
	if got := call(c, "c"); got != 20 { // a()+b() = 5+15
		t.Fatalf("C.c = %d, want 20 (calls A directly and B, which calls A)", got)
	}
}
