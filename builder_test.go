package wazy

import (
	"context"
	"testing"

	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/testing/require"
	"github.com/samyfodil/wazy/internal/wasm"
)

// TestNewHostModuleBuilder_Compile only covers a few scenarios to avoid duplicating tests in internal/wasm/host_test.go
func TestNewHostModuleBuilder_Compile(t *testing.T) {
	i32, i64 := wasm.ValueTypeI32, wasm.ValueTypeI64

	gofunc1 := api.GoFunc(func(ctx context.Context, stack []uint64) {
		stack[0] = 0
	})
	gofunc2 := api.GoFunc(func(ctx context.Context, stack []uint64) {
		stack[0] = 0
	})

	tests := []struct {
		name     string
		input    func(Runtime) HostModuleBuilder
		expected *wasm.Module
	}{
		{
			name: "empty",
			input: func(r Runtime) HostModuleBuilder {
				return r.NewHostModuleBuilder("host")
			},
			expected: &wasm.Module{NameSection: &wasm.NameSection{ModuleName: "host"}},
		},
		{
			name: "only name",
			input: func(r Runtime) HostModuleBuilder {
				return r.NewHostModuleBuilder("env")
			},
			expected: &wasm.Module{NameSection: &wasm.NameSection{ModuleName: "env"}},
		},
		{
			name: "WithGoFunction WithName WithResultNames",
			input: func(r Runtime) HostModuleBuilder {
				return r.NewHostModuleBuilder("host").NewFunctionBuilder().
					WithGoFunction(gofunc1, wasm.ToApiValueType([]wasm.ValueType{i32}), wasm.ToApiValueType([]wasm.ValueType{i32})).
					WithName("get").WithResultNames("x").
					Export("1")
			},
			expected: &wasm.Module{
				TypeSection: []wasm.FunctionType{
					{Params: []wasm.ValueType{i32}, Results: []wasm.ValueType{i32}},
				},
				FunctionSection: []wasm.Index{0},
				CodeSection: []wasm.Code{
					{GoFunc: gofunc1},
				},
				ExportSection: []wasm.Export{
					{Name: "1", Type: wasm.ExternTypeFunc, Index: 0},
				},
				Exports: map[string]*wasm.Export{
					"1": {Name: "1", Type: wasm.ExternTypeFunc, Index: 0},
				},
				NameSection: &wasm.NameSection{
					FunctionNames: wasm.NameMap{{Index: 0, Name: "get"}},
					ResultNames:   []wasm.NameMapAssoc{{Index: 0, NameMap: wasm.NameMap{{Index: 0, Name: "x"}}}},
					ModuleName:    "host",
				},
			},
		},
		{
			name: "WithGoFunction",
			input: func(r Runtime) HostModuleBuilder {
				return r.NewHostModuleBuilder("host").
					NewFunctionBuilder().
					WithGoFunction(gofunc1, wasm.ToApiValueType([]wasm.ValueType{i32}), wasm.ToApiValueType([]wasm.ValueType{i32})).
					Export("1")
			},
			expected: &wasm.Module{
				TypeSection: []wasm.FunctionType{
					{Params: []wasm.ValueType{i32}, Results: []wasm.ValueType{i32}},
				},
				FunctionSection: []wasm.Index{0},
				CodeSection: []wasm.Code{
					{GoFunc: gofunc1},
				},
				ExportSection: []wasm.Export{
					{Name: "1", Type: wasm.ExternTypeFunc, Index: 0},
				},
				Exports: map[string]*wasm.Export{
					"1": {Name: "1", Type: wasm.ExternTypeFunc, Index: 0},
				},
				NameSection: &wasm.NameSection{
					FunctionNames: wasm.NameMap{{Index: 0, Name: "1"}},
					ModuleName:    "host",
				},
			},
		},
		{
			name: "WithGoFunction WithName WithParameterNames",
			input: func(r Runtime) HostModuleBuilder {
				return r.NewHostModuleBuilder("host").NewFunctionBuilder().
					WithGoFunction(gofunc1, wasm.ToApiValueType([]wasm.ValueType{i32}), wasm.ToApiValueType([]wasm.ValueType{i32})).
					WithName("get").WithParameterNames("x").
					Export("1")
			},
			expected: &wasm.Module{
				TypeSection: []wasm.FunctionType{
					{Params: []wasm.ValueType{i32}, Results: []wasm.ValueType{i32}},
				},
				FunctionSection: []wasm.Index{0},
				CodeSection: []wasm.Code{
					{GoFunc: gofunc1},
				},
				ExportSection: []wasm.Export{
					{Name: "1", Type: wasm.ExternTypeFunc, Index: 0},
				},
				Exports: map[string]*wasm.Export{
					"1": {Name: "1", Type: wasm.ExternTypeFunc, Index: 0},
				},
				NameSection: &wasm.NameSection{
					FunctionNames: wasm.NameMap{{Index: 0, Name: "get"}},
					LocalNames:    []wasm.NameMapAssoc{{Index: 0, NameMap: wasm.NameMap{{Index: 0, Name: "x"}}}},
					ModuleName:    "host",
				},
			},
		},
		{
			name: "WithGoFunction overwrites existing",
			input: func(r Runtime) HostModuleBuilder {
				return r.NewHostModuleBuilder("host").
					NewFunctionBuilder().
					WithGoFunction(gofunc1, wasm.ToApiValueType([]wasm.ValueType{i32}), wasm.ToApiValueType([]wasm.ValueType{i32})).
					Export("1").
					NewFunctionBuilder().
					WithGoFunction(gofunc2, wasm.ToApiValueType([]wasm.ValueType{i64}), wasm.ToApiValueType([]wasm.ValueType{i32})).
					Export("1")
			},
			expected: &wasm.Module{
				TypeSection: []wasm.FunctionType{
					{Params: []wasm.ValueType{i64}, Results: []wasm.ValueType{i32}},
				},
				FunctionSection: []wasm.Index{0},
				CodeSection: []wasm.Code{
					{GoFunc: gofunc2},
				},
				ExportSection: []wasm.Export{
					{Name: "1", Type: wasm.ExternTypeFunc, Index: 0},
				},
				Exports: map[string]*wasm.Export{
					"1": {Name: "1", Type: wasm.ExternTypeFunc, Index: 0},
				},
				NameSection: &wasm.NameSection{
					FunctionNames: wasm.NameMap{{Index: 0, Name: "1"}},
					ModuleName:    "host",
				},
			},
		},
		{
			name: "WithGoFunction twice",
			input: func(r Runtime) HostModuleBuilder {
				// Intentionally not in lexicographic order
				return r.NewHostModuleBuilder("host").
					NewFunctionBuilder().
					WithGoFunction(gofunc2, wasm.ToApiValueType([]wasm.ValueType{i64}), wasm.ToApiValueType([]wasm.ValueType{i32})).
					Export("2").
					NewFunctionBuilder().
					WithGoFunction(gofunc1, wasm.ToApiValueType([]wasm.ValueType{i32}), wasm.ToApiValueType([]wasm.ValueType{i32})).
					Export("1")
			},
			expected: &wasm.Module{
				TypeSection: []wasm.FunctionType{
					{Params: []wasm.ValueType{i64}, Results: []wasm.ValueType{i32}},
					{Params: []wasm.ValueType{i32}, Results: []wasm.ValueType{i32}},
				},
				FunctionSection: []wasm.Index{0, 1},
				CodeSection: []wasm.Code{
					{GoFunc: gofunc2},
					{GoFunc: gofunc1},
				},
				ExportSection: []wasm.Export{
					{Name: "2", Type: wasm.ExternTypeFunc, Index: 0},
					{Name: "1", Type: wasm.ExternTypeFunc, Index: 1},
				},
				Exports: map[string]*wasm.Export{
					"2": {Name: "2", Type: wasm.ExternTypeFunc, Index: 0},
					"1": {Name: "1", Type: wasm.ExternTypeFunc, Index: 1},
				},
				NameSection: &wasm.NameSection{
					FunctionNames: wasm.NameMap{{Index: 0, Name: "2"}, {Index: 1, Name: "1"}},
					ModuleName:    "host",
				},
			},
		},
	}

	for _, tt := range tests {
		tc := tt

		t.Run(tc.name, func(t *testing.T) {
			b := tc.input(NewRuntime(testCtx)).(*hostModuleBuilder)
			compiled, err := b.Compile(testCtx)
			require.NoError(t, err)
			m := compiled.(*compiledModule)

			requireHostModuleEquals(t, tc.expected, m.module)

			require.Equal(t, b.r.store.Engine, m.compiledEngine)

			// TypeIDs must be assigned to compiledModule.
			expTypeIDs, err := b.r.store.GetFunctionTypeIDs(tc.expected.TypeSection)
			require.NoError(t, err)
			require.Equal(t, expTypeIDs, m.typeIDs)

			// Built module must be instantiable by Engine.
			mod, err := b.r.InstantiateModule(testCtx, m, NewModuleConfig())
			require.NoError(t, err)

			// Closing the module shouldn't remove the compiler cache
			require.NoError(t, mod.Close(testCtx))
			require.Equal(t, uint32(1), b.r.store.Engine.CompiledModuleCount())
		})
	}
}

func TestNewHostModuleBuilder_ExportTag(t *testing.T) {
	r := NewRuntimeWithConfig(testCtx, NewRuntimeConfig().WithCoreFeatures(api.CoreFeaturesV3))
	defer r.Close(testCtx)

	// Exporting "e" a second time replaces the first, in place.
	compiled, err := r.NewHostModuleBuilder("env").
		ExportTag("e", api.ValueTypeI64).
		ExportTag("l", api.ValueTypeI32).
		ExportTag("e", api.ValueTypeI32).
		Compile(testCtx)
	require.NoError(t, err)
	m := compiled.(*compiledModule).module
	require.Equal(t, 1, len(m.TypeSection))
	require.Equal(t, []wasm.ValueType{wasm.ValueTypeI32}, m.TypeSection[0].Params)
	require.Equal(t, []wasm.Tag{{Type: 0}, {Type: 0}}, m.TagSection)
	require.Equal(t, []wasm.Export{
		{Type: wasm.ExternTypeTag, Name: "e", Index: 0},
		{Type: wasm.ExternTypeTag, Name: "l", Index: 1},
	}, m.ExportSection)

	_, err = r.InstantiateModule(testCtx, compiled, NewModuleConfig())
	require.NoError(t, err)

	t.Run("requires exception handling", func(t *testing.T) {
		r := NewRuntime(testCtx)
		defer r.Close(testCtx)
		_, err := r.NewHostModuleBuilder("env").ExportTag("e").Compile(testCtx)
		require.EqualError(t, err, `tag[env.e] invalid as feature "exception-handling" is disabled`)
	})
}

// TestNewHostModuleBuilder_Instantiate ensures Runtime.InstantiateModule is called on success.
func TestNewHostModuleBuilder_Instantiate(t *testing.T) {
	r := NewRuntime(testCtx)
	m, err := r.NewHostModuleBuilder("env").Instantiate(testCtx)
	require.NoError(t, err)

	// Calling ExportedFunction should fail.
	err = require.CapturePanic(func() { m.ExportedFunction("any") })
	require.EqualError(t, err, `calling ExportedFunction is forbidden on host modules. See the note on ExportedFunction interface`)

	// If this was instantiated, it would be added to the store under the same name
	require.Equal(t, r.Module("env"), m)

	// Closing the module should remove the compiler cache
	require.NoError(t, m.Close(testCtx))
	require.Zero(t, r.(*runtime).store.Engine.CompiledModuleCount())
}

// TestNewHostModuleBuilder_Instantiate_Errors ensures errors propagate from Runtime.InstantiateModule
func TestNewHostModuleBuilder_Instantiate_Errors(t *testing.T) {
	r := NewRuntime(testCtx)
	_, err := r.NewHostModuleBuilder("env").Instantiate(testCtx)
	require.NoError(t, err)

	_, err = r.NewHostModuleBuilder("env").Instantiate(testCtx)
	require.EqualError(t, err, "module[env] has already been instantiated")
}

// requireHostModuleEquals is redefined from internal/wasm/host_test.go to avoid an import cycle extracting it.
func requireHostModuleEquals(t *testing.T, expected, actual *wasm.Module) {
	// `require.Equal(t, expected, actual)` fails reflect pointers don't match, so brute compare:
	for i := range expected.TypeSection {
		tp := &expected.TypeSection[i]
		tp.CacheNumInUint64()
		// Host module assembly caches each type's key, so a later lookup from guest execution never writes to a
		// shared FunctionType. See wasm.FunctionType.CacheKey.
		tp.CacheKey()
	}
	wasm.CanonicalizeTypes(expected.TypeSection)
	require.Equal(t, expected.TypeSection, actual.TypeSection)
	require.Equal(t, expected.ImportSection, actual.ImportSection)
	require.Equal(t, expected.FunctionSection, actual.FunctionSection)
	require.Equal(t, expected.TableSection, actual.TableSection)
	require.Equal(t, expected.MemorySection, actual.MemorySection)
	require.Equal(t, expected.GlobalSection, actual.GlobalSection)
	require.Equal(t, expected.ExportSection, actual.ExportSection)
	require.Equal(t, expected.Exports, actual.Exports)
	require.Equal(t, expected.StartSection, actual.StartSection)
	require.Equal(t, expected.ElementSection, actual.ElementSection)
	require.Equal(t, expected.DataSection, actual.DataSection)
	require.Equal(t, expected.TagSection, actual.TagSection)
	require.Equal(t, expected.NameSection, actual.NameSection)

	// Special case because reflect.Value can't be compared with Equals
	// TODO: This is copy/paste with /internal/wasm/host_test.go
	require.Equal(t, len(expected.CodeSection), len(actual.CodeSection))
	for i, c := range expected.CodeSection {
		actualCode := actual.CodeSection[i]
		require.Equal(t, c.GoFunc, actualCode.GoFunc)

		// Not wasm
		require.Nil(t, actualCode.Body)
		require.Nil(t, actualCode.LocalTypes)
	}
}
