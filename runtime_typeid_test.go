package wazy

import (
	"context"
	"testing"

	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/testing/binaryencoding"
	"github.com/samyfodil/wazy/internal/testing/require"
	"github.com/samyfodil/wazy/internal/wasm"
)

// TestInstantiateModule_typeIDsAreThisStores is a regression test for
// https://github.com/tetratelabs/wazero/issues/2511.
//
// A CompiledModule caches the type IDs the compiling store assigned, in the
// order that store saw types. Instantiating it on a different runtime -- what
// sharing a compilation cache amounts to -- meant checking imports against
// IDs that mean nothing to the instantiating store, so structurally identical
// signatures were rejected.
func TestInstantiateModule_typeIDsAreThisStores(t *testing.T) {
	ctx := context.Background()

	// Declares three types so the imported one is at a non-zero index. A store
	// that registers only the imported signature assigns it ID 0, while the
	// compiling store assigned it 2.
	guest := binaryencoding.EncodeModule(&wasm.Module{
		TypeSection: []wasm.FunctionType{
			{Params: []wasm.ValueType{wasm.ValueTypeI32}, Results: []wasm.ValueType{wasm.ValueTypeI32}},
			{Params: []wasm.ValueType{wasm.ValueTypeI64}, Results: []wasm.ValueType{wasm.ValueTypeI64}},
			// Index 2: the imported signature.
			{Params: []wasm.ValueType{wasm.ValueTypeI32, wasm.ValueTypeI64}, Results: []wasm.ValueType{wasm.ValueTypeI64}},
		},
		ImportSection:   []wasm.Import{{Module: "env", Name: "proxy", Type: wasm.ExternTypeFunc, DescFunc: 2}},
		FunctionSection: []wasm.Index{0},
		CodeSection:     []wasm.Code{{Body: []byte{wasm.OpcodeLocalGet, 0, wasm.OpcodeEnd}}},
		ExportSection:   []wasm.Export{{Name: "echo", Type: wasm.ExternTypeFunc, Index: 1}},
	})

	// Two runtimes sharing a compilation cache: this is what lets a module
	// compiled by one be instantiated by the other, and it is the pattern the
	// cache exists for.
	cache := NewCompilationCache()
	defer cache.Close(ctx)
	config := NewRuntimeConfig().WithCompilationCache(cache)

	// Compile on one runtime.
	compiler := NewRuntimeWithConfig(ctx, config)
	defer compiler.Close(ctx)
	compiled, err := compiler.CompileModule(ctx, guest)
	require.NoError(t, err)

	// Instantiate on another, whose store has only ever seen the imported
	// signature, so its IDs differ from the compiling store's.
	r := NewRuntimeWithConfig(ctx, config)
	defer r.Close(ctx)

	_, err = HostFunc2(r.NewHostModuleBuilder("env").NewFunctionBuilder(),
		func(_ context.Context, _ api.Module, x uint32, y uint64) uint64 {
			return uint64(x) + y
		}).Export("proxy").Instantiate(ctx)
	require.NoError(t, err)

	mod, err := r.InstantiateModule(ctx, compiled, NewModuleConfig())
	require.NoError(t, err)
	defer mod.Close(ctx)

	results, err := mod.ExportedFunction("echo").Call(ctx, 42)
	require.NoError(t, err)
	require.Equal(t, uint64(42), results[0])
}
