package wazy

import (
	"context"
	_ "embed"
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

// gcTypeIDWasm declares a rec group with a supertype and a subtype and exports
// two ref.test results over them; see testdata/gctypeid.wat.
//
//go:embed testdata/gctypeid.wasm
var gcTypeIDWasm []byte

// TestInstantiateModule_typeIDsAreThisStores_gc is the same bug as
// TestInstantiateModule_typeIDsAreThisStores, in the form wazy's richer type
// system gives it. ref.test resolves a declared type index through the module's
// type IDs into the store-global supertype table, so a stale ID does not
// produce an error the way an import mismatch does -- it produces a wrong
// answer, silently. The module imports nothing, so the loud check that guards
// the other test cannot fire here.
func TestInstantiateModule_typeIDsAreThisStores_gc(t *testing.T) {
	ctx := context.Background()

	cache := NewCompilationCache()
	defer func() { require.NoError(t, cache.Close(ctx)) }()
	config := NewRuntimeConfig().WithCoreFeatures(api.CoreFeaturesV3).WithCompilationCache(cache)

	compiler := NewRuntimeWithConfig(ctx, config)
	defer func() { require.NoError(t, compiler.Close(ctx)) }()
	compiled, err := compiler.CompileModule(ctx, gcTypeIDWasm)
	require.NoError(t, err)

	// A second runtime whose store has seen a different set of types first, so
	// its IDs for the rec group cannot coincide with the compiling store's.
	r := NewRuntimeWithConfig(ctx, config)
	defer func() { require.NoError(t, r.Close(ctx)) }()
	_, err = HostFunc2(r.NewHostModuleBuilder("shift").NewFunctionBuilder(),
		func(_ context.Context, _ api.Module, x uint32, y uint64) uint64 { return uint64(x) + y },
	).Export("f").Instantiate(ctx)
	require.NoError(t, err)

	mod, err := r.InstantiateModule(ctx, compiled, NewModuleConfig())
	require.NoError(t, err)
	defer func() { require.NoError(t, mod.Close(ctx)) }()

	// A $b is a $a; an $a is not a $b.
	results, err := mod.ExportedFunction("b_is_a").Call(ctx)
	require.NoError(t, err)
	require.Equal(t, uint64(1), results[0])

	results, err = mod.ExportedFunction("a_is_b").Call(ctx)
	require.NoError(t, err)
	require.Equal(t, uint64(0), results[0])
}
