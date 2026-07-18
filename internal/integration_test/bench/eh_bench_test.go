package bench

// Benchmarks for the native exception-handling (try_table) compiled fast
// path and throw path. See docs/design/eh-side-table.md: at the time that
// design was written, no such benchmark existed anywhere in the repo
// (grep of benchmarks/ and internal/integration_test found none), so these
// establish the baseline the design's redesign is measured against.
//
// Modules are hand-assembled directly as wasm bytes (mirroring
// internal/integration_test/engine/eh_hammer_test.go's minimal encoder)
// rather than compiled from .wat, so this file has no external toolchain
// dependency and stays self-contained.

import (
	"context"
	"fmt"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/platform"
	"github.com/samyfodil/wazy/internal/testing/require"
	"github.com/samyfodil/wazy/internal/wasm"
)

// BenchmarkEHFastPath measures a try_table entered on every iteration of a
// hot loop, whose body calls a leaf function but never throws -- the
// "C++/Emscripten enters try_table constantly, rarely throws" pattern the
// design doc targets (see eh-side-table.md section 5's own wat example,
// which this mirrors almost verbatim: a loop wrapping a try_table
// (catch_all) around a call, accumulating the callee's results).
func BenchmarkEHFastPath(b *testing.B) {
	if !platform.CompilerSupported() {
		b.Skip()
	}
	mod := instantiateEHBenchModule(b, buildEHFastPathModule())
	hot := mod.ExportedFunction("hot")

	for _, n := range []uint64{10, 1000, 100000} {
		n := n
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := hot.Call(testCtx, n); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkEHThrowPath measures a try_table entered on every iteration of a
// hot loop whose body always throws, caught by the same try_table's
// catch clause -- the "rare but nonzero throw cost" side of the same
// design, exercising ExitCodeThrow / the throw-time side-table search and
// control transfer on every iteration instead of the enter/leave fast path.
func BenchmarkEHThrowPath(b *testing.B) {
	if !platform.CompilerSupported() {
		b.Skip()
	}
	mod := instantiateEHBenchModule(b, buildEHThrowModule())
	hot := mod.ExportedFunction("hot_throw")

	for _, n := range []uint64{10, 1000, 100000} {
		n := n
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := hot.Call(testCtx, n); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// TestEHBenchModules checks that the two benchmark modules actually compute
// the expected result -- otherwise a benchmark could report a fast "win" for
// code that silently returns garbage (e.g. a mis-reconstructed catch value or
// a throw that isn't really caught). Both "hot" and "hot_throw" accumulate
// sum(0..n-1) = n*(n-1)/2: the fast-path module by adding leaf(i)=i each
// iteration, the throw-path module by throwing i and adding the caught value.
// The n=1 throw case (acc stays 0) is the minimal single-catch smoke test.
func TestEHBenchModules(t *testing.T) {
	if !platform.CompilerSupported() {
		t.Skip()
	}
	fast := instantiateEHBenchModule(t, buildEHFastPathModule()).ExportedFunction("hot")
	thrown := instantiateEHBenchModule(t, buildEHThrowModule()).ExportedFunction("hot_throw")

	for _, n := range []uint64{0, 1, 2, 10, 1000} {
		want := int64(n) * (int64(n) - 1) / 2
		res, err := fast.Call(testCtx, n)
		require.NoError(t, err)
		require.Equal(t, uint64(want), res[0])

		res, err = thrown.Call(testCtx, n)
		require.NoError(t, err)
		require.Equal(t, uint64(want), res[0])
	}
}

func instantiateEHBenchModule(tb testing.TB, bin []byte) api.Module {
	ctx := context.Background()
	cfg := wazy.NewRuntimeConfigCompiler().
		WithCoreFeatures(api.CoreFeaturesV2 | api.CoreFeatureExceptionHandling)
	r := wazy.NewRuntimeWithConfig(ctx, cfg)
	tb.Cleanup(func() { r.Close(ctx) })
	mod, err := r.Instantiate(ctx, bin)
	require.NoError(tb, err)
	return mod
}

// buildEHFastPathModule constructs:
//
//	(module
//	  (type $sig (func (param i32) (result i32)))
//	  (type $tagty (func))
//	  (tag $t (type $tagty))
//	  (func $leaf (type $sig) (local.get 0))
//	  (func (export "hot") (type $sig) (param $n i32) (result i32)
//	    (local $i i32) (local $acc i32)
//	    (block $done
//	      (loop $L
//	        (br_if $done (i32.ge_u (local.get $i) (local.get $n)))
//	        (block $caught
//	          (try_table (catch_all $caught)
//	            (local.set $acc (i32.add (local.get $acc) (call $leaf (local.get $i))))))
//	        (local.set $i (i32.add (local.get $i) (i32.const 1)))
//	        (br $L)))
//	    (local.get $acc)))
//
// The try_table's body never throws; catch_all is dead code at runtime,
// exactly like TryTableCatchAllEmpty in internal/engine/native/testcases,
// but driven from a hot loop instead of a single call.
func buildEHFastPathModule() []byte {
	m := &wasm.Module{
		TypeSection: []wasm.FunctionType{
			{Params: []wasm.ValueType{wasm.ValueTypeI32}, Results: []wasm.ValueType{wasm.ValueTypeI32}}, // 0: (i32)->(i32)
			{}, // 1: tag type ()->()
		},
		TagSection:      []wasm.Tag{{Type: 1}},
		FunctionSection: []wasm.Index{0, 0}, // leaf, hot
		ExportSection: []wasm.Export{
			{Name: "hot", Type: wasm.ExternTypeFunc, Index: 1},
		},
	}

	leafBody := []byte{
		wasm.OpcodeLocalGet, 0,
		wasm.OpcodeEnd,
	}

	hotBody := []byte{
		wasm.OpcodeBlock, 0x40, // block $done
		wasm.OpcodeLoop, 0x40, // loop $L
		wasm.OpcodeLocalGet, 1, // i
		wasm.OpcodeLocalGet, 0, // n
		wasm.OpcodeI32GeU,
		wasm.OpcodeBrIf, 1, // br_if $done
		wasm.OpcodeBlock, 0x40, // block $caught
		wasm.OpcodeTryTable, 0x40, // try_table void
		1, wasm.CatchKindCatchAll, 0, // catch_all -> $caught (label 0)
		wasm.OpcodeLocalGet, 2, // acc
		wasm.OpcodeLocalGet, 1, // i
		wasm.OpcodeCall, 0, // call $leaf
		wasm.OpcodeI32Add,
		wasm.OpcodeLocalSet, 2, // acc = acc + leaf(i)
		wasm.OpcodeEnd, // end try_table
		wasm.OpcodeEnd, // end block $caught
		wasm.OpcodeLocalGet, 1,
		wasm.OpcodeI32Const, 1,
		wasm.OpcodeI32Add,
		wasm.OpcodeLocalSet, 1, // i++
		wasm.OpcodeBr, 0, // br $L
		wasm.OpcodeEnd,         // end loop
		wasm.OpcodeEnd,         // end block $done
		wasm.OpcodeLocalGet, 2, // return acc
		wasm.OpcodeEnd, // end function
	}

	m.CodeSection = []wasm.Code{
		{Body: leafBody},
		{LocalTypes: []wasm.ValueType{wasm.ValueTypeI32, wasm.ValueTypeI32}, Body: hotBody}, // $i, $acc
	}
	return encodeEHBenchModule(m)
}

// buildEHThrowModule constructs:
//
//	(module
//	  (type $sig (func (param i32) (result i32)))
//	  (type $tagty (func (param i32)))
//	  (tag $t (type $tagty))
//	  (func (export "hot_throw") (type $sig) (param $n i32) (result i32)
//	    (local $i i32) (local $acc i32)
//	    (block $done
//	      (loop $L
//	        (br_if $done (i32.ge_u (local.get $i) (local.get $n)))
//	        (local.set $acc (i32.add (local.get $acc)
//	          (block $caught (result i32)
//	            (try_table (result i32) (catch $t $caught)
//	              (local.get $i) (throw $t)))))
//	        (local.set $i (i32.add (local.get $i) (i32.const 1)))
//	        (br $L)))
//	    (local.get $acc)))
//
// The try_table's body throws unconditionally, caught every iteration by
// its own catch clause -- exercising the throw-time side-table search and
// control transfer instead of the (never-taken here) enter/leave fast path.
func buildEHThrowModule() []byte {
	m := &wasm.Module{
		TypeSection: []wasm.FunctionType{
			{Params: []wasm.ValueType{wasm.ValueTypeI32}, Results: []wasm.ValueType{wasm.ValueTypeI32}}, // 0: (i32)->(i32)
			{Params: []wasm.ValueType{wasm.ValueTypeI32}},                                               // 1: tag type (i32)->()
		},
		TagSection:      []wasm.Tag{{Type: 1}},
		FunctionSection: []wasm.Index{0}, // hot_throw
		ExportSection: []wasm.Export{
			{Name: "hot_throw", Type: wasm.ExternTypeFunc, Index: 0},
		},
	}

	hotThrowBody := []byte{
		wasm.OpcodeBlock, 0x40, // block $done
		wasm.OpcodeLoop, 0x40, // loop $L
		wasm.OpcodeLocalGet, 1, // i
		wasm.OpcodeLocalGet, 0, // n
		wasm.OpcodeI32GeU,
		wasm.OpcodeBrIf, 1, // br_if $done
		wasm.OpcodeLocalGet, 2, // acc (pushed now, added to the catch value below)
		wasm.OpcodeBlock, 0x7f, // block $caught (result i32)
		wasm.OpcodeTryTable, 0x7f, // try_table (result i32)
		1, wasm.CatchKindCatch, 0, 0, // catch $t (tag 0) -> $caught (label 0)
		wasm.OpcodeLocalGet, 1, // i (thrown value)
		wasm.OpcodeThrow, 0, // throw $t
		wasm.OpcodeEnd, // end try_table (unreachable normally: body always throws)
		wasm.OpcodeEnd, // end block $caught (result: caught i32)
		wasm.OpcodeI32Add,
		wasm.OpcodeLocalSet, 2, // acc = acc + caught_value
		wasm.OpcodeLocalGet, 1,
		wasm.OpcodeI32Const, 1,
		wasm.OpcodeI32Add,
		wasm.OpcodeLocalSet, 1, // i++
		wasm.OpcodeBr, 0, // br $L
		wasm.OpcodeEnd,         // end loop
		wasm.OpcodeEnd,         // end block $done
		wasm.OpcodeLocalGet, 2, // return acc
		wasm.OpcodeEnd, // end function
	}

	m.CodeSection = []wasm.Code{
		{LocalTypes: []wasm.ValueType{wasm.ValueTypeI32, wasm.ValueTypeI32}, Body: hotThrowBody}, // $i, $acc
	}
	return encodeEHBenchModule(m)
}

// encodeEHBenchModule encodes m into a valid wasm binary. Minimal encoder
// handling exactly the sections/shapes buildEHFastPathModule and
// buildEHThrowModule use (Type/Tag/Function/Export/Code, with a code
// section that can declare LocalTypes) -- adapted from
// internal/integration_test/engine/eh_hammer_test.go's encodeModule, which
// hardcoded zero locals.
func encodeEHBenchModule(m *wasm.Module) []byte {
	var buf []byte
	buf = append(buf, 0x00, 0x61, 0x73, 0x6d) // \0asm
	buf = append(buf, 0x01, 0x00, 0x00, 0x00) // version 1

	buf = appendEHSection(buf, 1, func(s []byte) []byte { // Type section
		s = appendEHUleb128(s, uint32(len(m.TypeSection)))
		for _, ft := range m.TypeSection {
			s = append(s, 0x60) // func type
			s = appendEHUleb128(s, uint32(len(ft.Params)))
			s = append(s, wasm.ToApiValueType(ft.Params)...)
			s = appendEHUleb128(s, uint32(len(ft.Results)))
			s = append(s, wasm.ToApiValueType(ft.Results)...)
		}
		return s
	})

	buf = appendEHSection(buf, 3, func(s []byte) []byte { // Function section
		s = appendEHUleb128(s, uint32(len(m.FunctionSection)))
		for _, idx := range m.FunctionSection {
			s = appendEHUleb128(s, idx)
		}
		return s
	})

	buf = appendEHSection(buf, 13, func(s []byte) []byte { // Tag section
		s = appendEHUleb128(s, uint32(len(m.TagSection)))
		for _, tag := range m.TagSection {
			s = append(s, 0x00) // attribute byte (must be 0)
			s = appendEHUleb128(s, tag.Type)
		}
		return s
	})

	buf = appendEHSection(buf, 7, func(s []byte) []byte { // Export section
		s = appendEHUleb128(s, uint32(len(m.ExportSection)))
		for _, exp := range m.ExportSection {
			s = appendEHUleb128(s, uint32(len(exp.Name)))
			s = append(s, exp.Name...)
			s = append(s, exp.Type)
			s = appendEHUleb128(s, exp.Index)
		}
		return s
	})

	buf = appendEHSection(buf, 10, func(s []byte) []byte { // Code section
		s = appendEHUleb128(s, uint32(len(m.CodeSection)))
		for _, code := range m.CodeSection {
			var funcBody []byte
			if len(code.LocalTypes) == 0 {
				funcBody = appendEHUleb128(nil, 0) // 0 local-declaration entries
			} else {
				// One local-declaration entry per local (count=1, its own
				// type) -- simplest correct encoding, no run-length
				// compression needed for these small benchmark functions.
				funcBody = appendEHUleb128(nil, uint32(len(code.LocalTypes)))
				for _, t := range code.LocalTypes {
					funcBody = appendEHUleb128(funcBody, 1)
					funcBody = append(funcBody, byte(t))
				}
			}
			funcBody = append(funcBody, code.Body...)
			s = appendEHUleb128(s, uint32(len(funcBody)))
			s = append(s, funcBody...)
		}
		return s
	})

	return buf
}

func appendEHSection(buf []byte, id byte, buildContent func([]byte) []byte) []byte {
	content := buildContent(nil)
	buf = append(buf, id)
	buf = appendEHUleb128(buf, uint32(len(content)))
	buf = append(buf, content...)
	return buf
}

func appendEHUleb128(b []byte, v uint32) []byte {
	for {
		c := byte(v & 0x7f)
		v >>= 7
		if v == 0 {
			return append(b, c)
		}
		b = append(b, c|0x80)
	}
}
