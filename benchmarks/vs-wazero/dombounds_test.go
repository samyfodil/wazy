package vswazero

import (
	"context"
	_ "embed"
	"strings"
	"testing"

	"github.com/samyfodil/wazy"
)

//go:embed testdata/dombounds.wasm
var domBoundsWasm []byte

func TestDominatedBounds(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntimeWithConfig(ctx, wazy.NewRuntimeConfigCompiler())
	defer r.Close(ctx)
	mod, err := r.Instantiate(ctx, domBoundsWasm)
	if err != nil {
		t.Fatal(err)
	}
	fn := mod.ExportedFunction("work")

	// The strongest access ends exactly at the one-page memory boundary.
	stack := []uint64{1, 65536 - 72}
	if err = fn.CallWithStack(ctx, stack); err != nil {
		t.Fatalf("exact boundary: %v", err)
	}

	// Moving the same address by one byte must still trap at the dominating
	// check even though the redundant checks after the PHI were removed.
	stack[0], stack[1] = 1, 65536-71
	if err = fn.CallWithStack(ctx, stack); err == nil || !strings.Contains(err.Error(), "out of bounds memory access") {
		t.Fatalf("expected out-of-bounds trap, got %v", err)
	}
}

// BenchmarkDominatedBounds measures the redundant-PHI shape that this pass
// targets: one stronger check dominates eight weaker checks in the hot loop.
func BenchmarkDominatedBounds(b *testing.B) {
	ctx := context.Background()
	r := wazy.NewRuntimeWithConfig(ctx, wazy.NewRuntimeConfigCompiler())
	defer r.Close(ctx)
	mod, err := r.Instantiate(ctx, domBoundsWasm)
	if err != nil {
		b.Fatal(err)
	}
	fn := mod.ExportedFunction("work")
	stack := []uint64{2000, 0}
	if err = fn.CallWithStack(ctx, stack); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		stack[0], stack[1] = 2000, 0
		if err = fn.CallWithStack(ctx, stack); err != nil {
			b.Fatal(err)
		}
	}
}
