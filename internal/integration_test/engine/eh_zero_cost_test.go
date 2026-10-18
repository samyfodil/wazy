package adhoc

import (
	"context"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/platform"
	"github.com/samyfodil/wazy/internal/testing/require"
	"github.com/samyfodil/wazy/internal/wasm"
)

// The native compiler keeps a try_table free on the normal path: a throw
// resumes the catching frame right after the call it was in, and every frame
// in between is abandoned without running its epilogue. The tests below pin
// what that relies on: nothing the catching frame needs may sit in a register
// across that call (callee-saved ones included), the catching function must
// restore its caller's callee-saved registers itself, a landing pad whose
// clauses don't match has to pass the exception on, and a host call inside a
// try body must not look like a throw.

// ehZeroCostModule builds:
//
//	(import "env" "cb" (func $cb))
//	(tag $A) (tag $B)
//	(func $deep (param $n i32) (result i32)
//	  (if (i32.eqz (local.get $n)) (then (throw $B)))
//	  ;; $n*2 ... $n*17 and an f64 live across the recursive call
//	  (f64.convert_i32_s (local.get $n))
//	  (call $deep (i32.sub (local.get $n) (i32.const 1)))
//	  ... sum them all)
//	(func $mid (param $n i32) (result i32)   ;; catches only $A: rethrows $B
//	  (block $l (try_table (catch $A $l) (drop (call $deep (local.get $n)))))
//	  (i32.const -1))
//	(func (export "run") (param $n i32) (result i32) (local $x i32)
//	  (i32.mul (local.get $n) (i32.const 13)) (i32.mul (local.get $n) (i32.const 17))
//	  (i32.mul (local.get $n) (i32.const 19))
//	  (block $c (try_table (catch $B $c)
//	    (local.set $x (i32.add (local.get $n) (i32.const 1)))
//	    (drop (call $mid (local.get $n)))
//	    (local.set $x (i32.const 999))))
//	  i32.add i32.add (i32.add (local.get $x)))          ;; 50n+1
//	(func (export "run2") (param $n i32) (result i32)
//	  ;; $n*1 ... $n*16 and an f64 live across the call
//	  (f64.convert_i32_s (local.get $n))
//	  (call $run (local.get $n))
//	  ... sum them all)                                  ;; 187n+1
//	(func (export "host") (param $n i32) (result i32) (local $x i32)
//	  (block $c (try_table (catch_all $c)
//	    (call $cb) (local.set $x (local.get $n)) (throw $A)))
//	  (local.get $x))                                    ;; n
//	(func (export "inner") (result i32)
//	  (block $c (try_table (catch_all $c) (throw $A))) (i32.const 1))
//	(func (export "thrower") (throw $A))
//	(func (export "rethrow") (param $n i32) (result i32) (local $x i32)
//	  (block $outer (result exnref) (try_table (catch_all_ref $outer)
//	    (block $c (result exnref) (try_table (catch_ref $A $c)
//	      (local.set $x (local.get $n)) (throw $A)))
//	    (local.set $x (i32.add (local.get $x) (i32.const 1)))
//	    throw_ref))
//	  drop (local.get $x))                               ;; n+1
func ehZeroCostModule() []byte {
	i32 := wasm.ValueTypeI32
	m := &wasm.Module{
		TypeSection: []wasm.FunctionType{
			{}, // 0: ()->()
			{Params: []wasm.ValueType{i32}, Results: []wasm.ValueType{i32}}, // 1: (i32)->i32
			{Results: []wasm.ValueType{i32}},                                // 2: ()->i32
		},
		ImportSection:   []wasm.Import{{Module: "env", Name: "cb", Type: wasm.ExternTypeFunc, DescFunc: 0}},
		TagSection:      []wasm.Tag{{Type: 0}, {Type: 0}},
		FunctionSection: []wasm.Index{1, 1, 1, 1, 1, 2, 0, 1},
		ExportSection: []wasm.Export{
			{Name: "run", Type: wasm.ExternTypeFunc, Index: 3},
			{Name: "run2", Type: wasm.ExternTypeFunc, Index: 4},
			{Name: "host", Type: wasm.ExternTypeFunc, Index: 5},
			{Name: "inner", Type: wasm.ExternTypeFunc, Index: 6},
			{Name: "thrower", Type: wasm.ExternTypeFunc, Index: 7},
			{Name: "rethrow", Type: wasm.ExternTypeFunc, Index: 8},
		},
	}
	const (
		get    = wasm.OpcodeLocalGet
		set    = wasm.OpcodeLocalSet
		cnst   = wasm.OpcodeI32Const
		mul    = wasm.OpcodeI32Mul
		add    = wasm.OpcodeI32Add
		end    = wasm.OpcodeEnd
		void   = 0x40
		exnref = 0x69
	)
	// More values live across a call than there are registers, so the register
	// allocator has to use the callee-saved ones too.
	const liveAcrossCall = 16
	mulN := func(k byte) []byte { return []byte{get, 0, cnst, k, mul} }

	var deep []byte
	deep = append(deep, get, 0, wasm.OpcodeI32Eqz, wasm.OpcodeIf, void, wasm.OpcodeThrow, 1, end)
	for k := byte(2); k < 2+liveAcrossCall; k++ {
		deep = append(deep, mulN(k)...)
	}
	deep = append(deep, get, 0, wasm.OpcodeF64ConvertI32S)
	deep = append(deep, get, 0, cnst, 1, wasm.OpcodeI32Sub, wasm.OpcodeCall, 1)
	deep = append(deep, set, 1, wasm.OpcodeI32TruncF64S, get, 1, add)
	for range liveAcrossCall {
		deep = append(deep, add)
	}
	deep = append(deep, end)

	mid := []byte{
		wasm.OpcodeBlock, void,
		wasm.OpcodeTryTable, void, 1, wasm.CatchKindCatch, 0, 0,
		get, 0, wasm.OpcodeCall, 1, wasm.OpcodeDrop,
		end, end,
		cnst, 0x7f, end,
	}

	var run []byte
	run = append(run, mulN(13)...)
	run = append(run, mulN(17)...)
	run = append(run, mulN(19)...)
	run = append(run, wasm.OpcodeBlock, void, wasm.OpcodeTryTable, void, 1, wasm.CatchKindCatch, 1, 0)
	run = append(run, get, 0, cnst, 1, add, set, 1)
	run = append(run, get, 0, wasm.OpcodeCall, 2, wasm.OpcodeDrop)
	run = append(run, cnst, 0xe7, 0x07, set, 1) // 999
	run = append(run, end, end, add, add, get, 1, add, end)

	var run2 []byte
	for k := byte(1); k <= liveAcrossCall; k++ {
		run2 = append(run2, mulN(k)...)
	}
	run2 = append(run2, get, 0, wasm.OpcodeF64ConvertI32S)
	run2 = append(run2, get, 0, wasm.OpcodeCall, 3)
	run2 = append(run2, set, 1, wasm.OpcodeI32TruncF64S, get, 1, add)
	for range liveAcrossCall {
		run2 = append(run2, add)
	}
	run2 = append(run2, end)

	host := []byte{
		wasm.OpcodeBlock, void,
		wasm.OpcodeTryTable, void, 1, wasm.CatchKindCatchAll, 0,
		wasm.OpcodeCall, 0, get, 0, set, 1, wasm.OpcodeThrow, 0,
		end, end,
		get, 1, end,
	}
	inner := []byte{
		wasm.OpcodeBlock, void,
		wasm.OpcodeTryTable, void, 1, wasm.CatchKindCatchAll, 0, wasm.OpcodeThrow, 0,
		end, end,
		cnst, 1, end,
	}
	thrower := []byte{wasm.OpcodeThrow, 0, end}
	rethrow := []byte{
		wasm.OpcodeBlock, exnref,
		wasm.OpcodeTryTable, void, 1, wasm.CatchKindCatchAllRef, 0,
		wasm.OpcodeBlock, exnref,
		wasm.OpcodeTryTable, void, 1, wasm.CatchKindCatchRef, 0, 0,
		get, 0, set, 1, wasm.OpcodeThrow, 0,
		end, // try_table
		wasm.OpcodeUnreachable,
		end, // block $c
		get, 1, cnst, 1, add, set, 1,
		wasm.OpcodeThrowRef,
		end, // try_table
		wasm.OpcodeUnreachable,
		end, // block $outer
		wasm.OpcodeDrop, get, 1, end,
	}

	withX := []wasm.ValueType{i32}
	m.CodeSection = []wasm.Code{
		{LocalTypes: withX, Body: deep},
		{Body: mid},
		{LocalTypes: withX, Body: run},
		{LocalTypes: withX, Body: run2},
		{LocalTypes: withX, Body: host},
		{Body: inner},
		{Body: thrower},
		{LocalTypes: withX, Body: rethrow},
	}
	return encodeModuleAdv(m)
}

func TestEHZeroCost(t *testing.T) {
	ctx := context.Background()
	bin := ehZeroCostModule()
	base := api.CoreFeaturesV2 | api.CoreFeatureExceptionHandling

	configs := map[string]wazy.RuntimeConfig{"interpreter": wazy.NewRuntimeConfigInterpreter().WithCoreFeatures(base)}
	if platform.CompilerSupported() {
		configs["compiler"] = wazy.NewRuntimeConfigCompiler().WithCoreFeatures(base)
	}
	for name, cfg := range configs {
		t.Run(name, func(t *testing.T) {
			r := wazy.NewRuntimeWithConfig(ctx, cfg)
			defer r.Close(ctx)

			var cbCalls int
			_, err := wazy.HostProc0(r.NewHostModuleBuilder("env").NewFunctionBuilder(), func(ctx context.Context, mod api.Module) {
				cbCalls++
				// A throw caught entirely inside a nested call must not leak into this frame's try_table.
				res, err := mod.ExportedFunction("inner").Call(ctx)
				require.NoError(t, err)
				require.Equal(t, uint64(1), res[0])
				// Nor may one that escapes the nested call: it surfaces here as an error.
				_, err = mod.ExportedFunction("thrower").Call(ctx)
				require.Error(t, err)
			}).Export("cb").Instantiate(ctx)
			require.NoError(t, err)

			mod, err := r.InstantiateWithConfig(ctx, bin, wazy.NewModuleConfig().WithStartFunctions())
			require.NoError(t, err)

			for _, n := range []uint64{1, 2, 5, 40} {
				res, err := mod.ExportedFunction("run").Call(ctx, n)
				require.NoError(t, err)
				require.Equal(t, 50*n+1, res[0], "run(%d)", n)

				res, err = mod.ExportedFunction("run2").Call(ctx, n)
				require.NoError(t, err)
				require.Equal(t, 187*n+1, res[0], "run2(%d)", n)

				res, err = mod.ExportedFunction("host").Call(ctx, n)
				require.NoError(t, err)
				require.Equal(t, n, res[0], "host(%d)", n)

				res, err = mod.ExportedFunction("rethrow").Call(ctx, n)
				require.NoError(t, err)
				require.Equal(t, n+1, res[0], "rethrow(%d)", n)
			}
			require.Equal(t, 4, cbCalls)
		})
	}
}
