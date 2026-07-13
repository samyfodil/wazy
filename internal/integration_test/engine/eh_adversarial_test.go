package adhoc

// Adversarial exception-handling tests for the native compiler's landing-pad
// live-state reconstruction (docs/design/eh-side-table.md section 4.1). Each
// module below deliberately arranges for a value the landing pad must observe
// to have a *throw-time* value that differs from its entry-time value and to be
// the kind of value the register allocator is free to keep in a callee-saved
// register across the try body (a mutated local -- category 3; a computed
// operand-floor value consumed by the catch continuation -- category 4; a local
// kept live across a call inside the try body; a value threaded through deeply
// nested same-function try_tables). If the compiled catch path reconstructs any
// of these from the wrong source, the result is *silent* corruption, so every
// case is cross-checked against the interpreter, which is the in-tree oracle for
// the exact per-clause target stack depth and locals contract.
//
// These are hand-assembled (no .wat toolchain dependency) mirroring
// eh_bench_test.go / eh_hammer_test.go.

import (
	"context"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/experimental"
	"github.com/samyfodil/wazy/internal/platform"
	"github.com/samyfodil/wazy/internal/testing/require"
	"github.com/samyfodil/wazy/internal/wasm"
)

// runEHAdvBothEngines instantiates bin on the interpreter (oracle) and, when
// supported, the compiler, calls fn(args) on each, requires no error, requires
// the result equals want, and (belt and suspenders) requires the two engines
// agree.
func runEHAdvBothEngines(t *testing.T, bin []byte, fn string, args []uint64, want uint64) {
	t.Helper()
	ctx := context.Background()

	call := func(cfg wazy.RuntimeConfig) (uint64, bool) {
		r := wazy.NewRuntimeWithConfig(ctx, cfg)
		defer r.Close(ctx)
		mod, err := r.InstantiateWithConfig(ctx, bin, wazy.NewModuleConfig().WithStartFunctions())
		require.NoError(t, err)
		res, err := mod.ExportedFunction(fn).Call(ctx, args...)
		require.NoError(t, err)
		return res[0], true
	}

	base := api.CoreFeaturesV2 | experimental.CoreFeaturesExceptionHandling
	interpRes, _ := call(wazy.NewRuntimeConfigInterpreter().WithCoreFeatures(base))
	require.Equal(t, want, interpRes, "interpreter (oracle) result")

	if platform.CompilerSupported() {
		compRes, _ := call(wazy.NewRuntimeConfigCompiler().WithCoreFeatures(base))
		require.Equal(t, want, compRes, "compiler result")
		require.Equal(t, interpRes, compRes, "interpreter vs compiler disagreement")
	}
}

// TestEHAdversarialMultiLocalThrow (category 3, several mixed-type locals):
// a try body mutates three locals of different types to throw-time values and
// then throws; the catch continuation reads all three. The handler must see the
// throw-time values (100/200/300 -> 600), never the entry-time ones (11/22/33).
func TestEHAdversarialMultiLocalThrow(t *testing.T) {
	// (func (export "run") (result i64)
	//   (local $a i32) (local $b i64) (local $c i32)
	//   (local.set $a (i32.const 11)) (local.set $b (i64.const 22)) (local.set $c (i32.const 33))
	//   (block $caught
	//     (try_table (catch_all $caught)
	//       (local.set $a (i32.const 100)) (local.set $b (i64.const 200)) (local.set $c (i32.const 300))
	//       (throw $t)))
	//   (i64.add (i64.add (i64.extend_i32_u (local.get $a)) (local.get $b)) (i64.extend_i32_u (local.get $c))))
	m := &wasm.Module{
		TypeSection: []wasm.FunctionType{
			{Results: []wasm.ValueType{wasm.ValueTypeI64}}, // 0: () -> i64
			{}, // 1: tag () -> ()
		},
		TagSection:      []wasm.Tag{{Type: 1}},
		FunctionSection: []wasm.Index{0},
		ExportSection:   []wasm.Export{{Name: "run", Type: wasm.ExternTypeFunc, Index: 0}},
	}
	var body []byte
	body = append(body, wasm.OpcodeI32Const)
	body = appendSleb128(body, 11)
	body = append(body, wasm.OpcodeLocalSet, 0)
	body = append(body, wasm.OpcodeI64Const)
	body = appendSleb128(body, 22)
	body = append(body, wasm.OpcodeLocalSet, 1)
	body = append(body, wasm.OpcodeI32Const)
	body = appendSleb128(body, 33)
	body = append(body, wasm.OpcodeLocalSet, 2)
	body = append(body, wasm.OpcodeBlock, 0x40)
	body = append(body, wasm.OpcodeTryTable, 0x40, 1, wasm.CatchKindCatchAll, 0)
	body = append(body, wasm.OpcodeI32Const)
	body = appendSleb128(body, 100)
	body = append(body, wasm.OpcodeLocalSet, 0)
	body = append(body, wasm.OpcodeI64Const)
	body = appendSleb128(body, 200)
	body = append(body, wasm.OpcodeLocalSet, 1)
	body = append(body, wasm.OpcodeI32Const)
	body = appendSleb128(body, 300)
	body = append(body, wasm.OpcodeLocalSet, 2)
	body = append(body, wasm.OpcodeThrow, 0)
	body = append(body, wasm.OpcodeEnd) // end try_table
	body = append(body, wasm.OpcodeEnd) // end block $caught
	body = append(body, wasm.OpcodeLocalGet, 0, wasm.OpcodeI64ExtendI32U)
	body = append(body, wasm.OpcodeLocalGet, 1, wasm.OpcodeI64Add)
	body = append(body, wasm.OpcodeLocalGet, 2, wasm.OpcodeI64ExtendI32U, wasm.OpcodeI64Add)
	body = append(body, wasm.OpcodeEnd)

	m.CodeSection = []wasm.Code{{
		LocalTypes: []wasm.ValueType{wasm.ValueTypeI32, wasm.ValueTypeI64, wasm.ValueTypeI32},
		Body:       body,
	}}
	runEHAdvBothEngines(t, encodeModuleAdv(m), "run", nil, 600)
}

// TestEHAdversarialOperandFloor (category 4): a computed value F = n*7+3 is left
// on the operand stack *below* the try_table, and the catch continuation adds it
// to the caught exception parameter. F is a genuine SSA value (not a local) that
// the register allocator keeps live across the try body -- exactly the operand
// floor that FloorSize accounts for. Handler must recover F correctly:
// result = (n*7+3) + 999.
func TestEHAdversarialOperandFloor(t *testing.T) {
	// (func (export "run") (param $n i32) (result i32)
	//   (i32.add (i32.mul (local.get $n) (i32.const 7)) (i32.const 3))     ;; floor F
	//   (block $caught (result i32)
	//     (try_table (result i32) (catch $t $caught)
	//       (i32.const 999) (throw $t)))
	//   i32.add)                                                           ;; F + caught
	m := &wasm.Module{
		TypeSection: []wasm.FunctionType{
			{Params: []wasm.ValueType{wasm.ValueTypeI32}, Results: []wasm.ValueType{wasm.ValueTypeI32}}, // 0: (i32)->i32
			{Params: []wasm.ValueType{wasm.ValueTypeI32}},                                               // 1: tag (i32)->()
		},
		TagSection:      []wasm.Tag{{Type: 1}},
		FunctionSection: []wasm.Index{0},
		ExportSection:   []wasm.Export{{Name: "run", Type: wasm.ExternTypeFunc, Index: 0}},
	}
	var body []byte
	body = append(body, wasm.OpcodeLocalGet, 0, wasm.OpcodeI32Const)
	body = appendSleb128(body, 7)
	body = append(body, wasm.OpcodeI32Mul, wasm.OpcodeI32Const)
	body = appendSleb128(body, 3)
	body = append(body, wasm.OpcodeI32Add) // F on stack
	body = append(body, wasm.OpcodeBlock, 0x7f)
	body = append(body, wasm.OpcodeTryTable, 0x7f, 1, wasm.CatchKindCatch, 0, 0)
	body = append(body, wasm.OpcodeI32Const)
	body = appendSleb128(body, 999)
	body = append(body, wasm.OpcodeThrow, 0)
	body = append(body, wasm.OpcodeEnd) // end try_table
	body = append(body, wasm.OpcodeEnd) // end block $caught
	body = append(body, wasm.OpcodeI32Add)
	body = append(body, wasm.OpcodeEnd)

	m.CodeSection = []wasm.Code{{Body: body}}
	bin := encodeModuleAdv(m)
	for _, n := range []uint64{0, 1, 5, 123} {
		runEHAdvBothEngines(t, bin, "run", []uint64{n}, n*7+3+999)
	}
}

// TestEHAdversarialLocalAcrossCall (category 3 under call pressure): a local $x
// is assigned inside the try body, kept live across a call to a leaf function,
// re-assigned (again across a call), and then the body throws. The handler reads
// $x, which must hold its throw-time value n+15 -- the case where regalloc most
// wants to keep the value in a callee-saved register spanning the enter call.
func TestEHAdversarialLocalAcrossCall(t *testing.T) {
	// (func $leaf (param i32) (result i32) (local.get 0))
	// (func (export "run") (param $n i32) (result i32)
	//   (local $x i32)
	//   (local.set $x (i32.const 0))
	//   (block $caught
	//     (try_table (catch_all $caught)
	//       (local.set $x (i32.add (local.get $n) (i32.const 5)))
	//       (drop (call $leaf (local.get $n)))
	//       (local.set $x (i32.add (local.get $x) (call $leaf (i32.const 10))))
	//       (throw $t)))
	//   (local.get $x))
	m := &wasm.Module{
		TypeSection: []wasm.FunctionType{
			{Params: []wasm.ValueType{wasm.ValueTypeI32}, Results: []wasm.ValueType{wasm.ValueTypeI32}}, // 0: (i32)->i32
			{}, // 1: tag ()->()
		},
		TagSection:      []wasm.Tag{{Type: 1}},
		FunctionSection: []wasm.Index{0, 0}, // $leaf, run
		ExportSection:   []wasm.Export{{Name: "run", Type: wasm.ExternTypeFunc, Index: 1}},
	}
	leaf := []byte{wasm.OpcodeLocalGet, 0, wasm.OpcodeEnd}

	var body []byte
	body = append(body, wasm.OpcodeI32Const, 0, wasm.OpcodeLocalSet, 1) // $x = 0 (local index 1)
	body = append(body, wasm.OpcodeBlock, 0x40)
	body = append(body, wasm.OpcodeTryTable, 0x40, 1, wasm.CatchKindCatchAll, 0)
	// $x = n + 5
	body = append(body, wasm.OpcodeLocalGet, 0, wasm.OpcodeI32Const, 5, wasm.OpcodeI32Add, wasm.OpcodeLocalSet, 1)
	// drop(call leaf(n))
	body = append(body, wasm.OpcodeLocalGet, 0, wasm.OpcodeCall, 0, wasm.OpcodeDrop)
	// $x = x + call leaf(10)
	body = append(body, wasm.OpcodeLocalGet, 1, wasm.OpcodeI32Const, 10, wasm.OpcodeCall, 0, wasm.OpcodeI32Add, wasm.OpcodeLocalSet, 1)
	body = append(body, wasm.OpcodeThrow, 0)
	body = append(body, wasm.OpcodeEnd) // end try_table
	body = append(body, wasm.OpcodeEnd) // end block
	body = append(body, wasm.OpcodeLocalGet, 1, wasm.OpcodeEnd)

	m.CodeSection = []wasm.Code{
		{Body: leaf},
		{LocalTypes: []wasm.ValueType{wasm.ValueTypeI32}, Body: body},
	}
	bin := encodeModuleAdv(m)
	for _, n := range []uint64{0, 1, 7, 100} {
		runEHAdvBothEngines(t, bin, "run", []uint64{n}, n+15)
	}
}

// TestEHAdversarialDeepNesting: three same-function try_tables nested, each
// catching a distinct tag. A $t1 is thrown from the innermost body; the inner
// two (catch $t3, catch $t2) do not match and are unwound past, and the
// outermost (catch $t1) catches. A local $x is bumped in each body (last set to
// 4 in the innermost body immediately before the throw); the outer handler must
// observe 4 -- exercising innermost-first dispatch, multi-scope unwind, and the
// shared (ReuseLocals) locals home across nested scopes.
func TestEHAdversarialDeepNesting(t *testing.T) {
	m := &wasm.Module{
		TypeSection: []wasm.FunctionType{
			{Results: []wasm.ValueType{wasm.ValueTypeI32}}, // 0: () -> i32
			{}, // 1: tag ()->()
		},
		TagSection:      []wasm.Tag{{Type: 1}, {Type: 1}, {Type: 1}}, // $t1,$t2,$t3
		FunctionSection: []wasm.Index{0},
		ExportSection:   []wasm.Export{{Name: "run", Type: wasm.ExternTypeFunc, Index: 0}},
	}
	setX := func(b []byte, v byte) []byte {
		return append(b, wasm.OpcodeI32Const, v, wasm.OpcodeLocalSet, 0)
	}
	var body []byte
	body = setX(body, 1)
	body = append(body, wasm.OpcodeBlock, 0x40) // $c1
	body = append(body, wasm.OpcodeTryTable, 0x40, 1, wasm.CatchKindCatch, 0 /*tag $t1*/, 0 /*label $c1*/)
	body = setX(body, 2)
	body = append(body, wasm.OpcodeBlock, 0x40) // $c2
	body = append(body, wasm.OpcodeTryTable, 0x40, 1, wasm.CatchKindCatch, 1 /*tag $t2*/, 0 /*label $c2*/)
	body = setX(body, 3)
	body = append(body, wasm.OpcodeBlock, 0x40) // $c3
	body = append(body, wasm.OpcodeTryTable, 0x40, 1, wasm.CatchKindCatch, 2 /*tag $t3*/, 0 /*label $c3*/)
	body = setX(body, 4)
	body = append(body, wasm.OpcodeThrow, 0 /*$t1*/)
	body = append(body, wasm.OpcodeEnd)         // end innermost try_table
	body = append(body, wasm.OpcodeEnd)         // end block $c3
	body = append(body, wasm.OpcodeUnreachable) // $c3 only reached if $t3 caught (never for $t1)
	body = append(body, wasm.OpcodeEnd)         // end middle try_table
	body = append(body, wasm.OpcodeEnd)         // end block $c2
	body = append(body, wasm.OpcodeUnreachable) // $c2 only reached if $t2 caught
	body = append(body, wasm.OpcodeEnd)         // end outer try_table
	body = append(body, wasm.OpcodeEnd)         // end block $c1
	body = append(body, wasm.OpcodeLocalGet, 0, wasm.OpcodeEnd)

	m.CodeSection = []wasm.Code{{LocalTypes: []wasm.ValueType{wasm.ValueTypeI32}, Body: body}}
	runEHAdvBothEngines(t, encodeModuleAdv(m), "run", nil, 4)
}

// encodeModuleAdv encodes a wasm.Module supporting multiple types/tags and
// per-function locals. Section shapes match encodeModule (eh_hammer_test.go)
// but with a locals-aware code section.
func encodeModuleAdv(m *wasm.Module) []byte {
	var buf []byte
	buf = append(buf, 0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00)

	buf = appendSection(buf, 1, func(s []byte) []byte {
		s = appendUleb128(s, uint32(len(m.TypeSection)))
		for _, ft := range m.TypeSection {
			s = append(s, 0x60)
			s = appendUleb128(s, uint32(len(ft.Params)))
			s = append(s, wasm.ToApiValueType(ft.Params)...)
			s = appendUleb128(s, uint32(len(ft.Results)))
			s = append(s, wasm.ToApiValueType(ft.Results)...)
		}
		return s
	})
	buf = appendSection(buf, 3, func(s []byte) []byte {
		s = appendUleb128(s, uint32(len(m.FunctionSection)))
		for _, idx := range m.FunctionSection {
			s = appendUleb128(s, idx)
		}
		return s
	})
	buf = appendSection(buf, 13, func(s []byte) []byte {
		s = appendUleb128(s, uint32(len(m.TagSection)))
		for _, tag := range m.TagSection {
			s = append(s, 0x00)
			s = appendUleb128(s, tag.Type)
		}
		return s
	})
	buf = appendSection(buf, 7, func(s []byte) []byte {
		s = appendUleb128(s, uint32(len(m.ExportSection)))
		for _, exp := range m.ExportSection {
			s = appendUleb128(s, uint32(len(exp.Name)))
			s = append(s, exp.Name...)
			s = append(s, exp.Type)
			s = appendUleb128(s, exp.Index)
		}
		return s
	})
	buf = appendSection(buf, 10, func(s []byte) []byte {
		s = appendUleb128(s, uint32(len(m.CodeSection)))
		for _, code := range m.CodeSection {
			var fb []byte
			if len(code.LocalTypes) == 0 {
				fb = appendUleb128(nil, 0)
			} else {
				fb = appendUleb128(nil, uint32(len(code.LocalTypes)))
				for _, ty := range code.LocalTypes {
					fb = appendUleb128(fb, 1)
					fb = append(fb, byte(ty))
				}
			}
			fb = append(fb, code.Body...)
			s = appendUleb128(s, uint32(len(fb)))
			s = append(s, fb...)
		}
		return s
	})
	return buf
}
