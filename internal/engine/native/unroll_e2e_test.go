package native_test

import (
	"context"
	"encoding/binary"
	"fmt"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/internal/engine/native/frontend"
	"github.com/samyfodil/wazy/internal/testing/binaryencoding"
	"github.com/samyfodil/wazy/internal/testing/require"
	"github.com/samyfodil/wazy/internal/wasm"
)

// unrollBodies are loops the compiler lowers twice over (see frontend.UnrollHook), each walking a linked
// list whose head is the first parameter: a node is its next node's address and a value. The second
// parameter is one, which they count with, as a constant step would make the loop one with a counter.
var unrollBodies = []struct {
	name string
	body []byte
}{
	{"sum", []byte{
		wasm.OpcodeLoop, 0x40,
		wasm.OpcodeLocalGet, 2, wasm.OpcodeLocalGet, 0, wasm.OpcodeI32Load, 2, 4, wasm.OpcodeI32Add, wasm.OpcodeLocalSet, 2,
		wasm.OpcodeLocalGet, 0, wasm.OpcodeI32Load, 2, 0, wasm.OpcodeLocalTee, 0,
		wasm.OpcodeBrIf, 0,
		wasm.OpcodeEnd,
		wasm.OpcodeLocalGet, 2, wasm.OpcodeEnd,
	}},
	// The address of the first node holding 7, or 0: a value carried out of either copy.
	{"find", []byte{
		wasm.OpcodeBlock, 0x7f, // i32
		wasm.OpcodeLoop, 0x40,
		wasm.OpcodeLocalGet, 0,
		wasm.OpcodeLocalGet, 0, wasm.OpcodeI32Load, 2, 4, wasm.OpcodeI32Const, 7, wasm.OpcodeI32Eq,
		wasm.OpcodeBrIf, 1,
		wasm.OpcodeDrop,
		wasm.OpcodeLocalGet, 0, wasm.OpcodeI32Load, 2, 0, wasm.OpcodeLocalTee, 0,
		wasm.OpcodeBrIf, 0,
		wasm.OpcodeEnd,
		wasm.OpcodeI32Const, 0,
		wasm.OpcodeEnd,
		wasm.OpcodeEnd,
	}},
	// The length, the loop going round by a br_table.
	{"table", []byte{
		wasm.OpcodeBlock, 0x40,
		wasm.OpcodeLoop, 0x40,
		wasm.OpcodeLocalGet, 2, wasm.OpcodeLocalGet, 1, wasm.OpcodeI32Add, wasm.OpcodeLocalSet, 2,
		wasm.OpcodeLocalGet, 0, wasm.OpcodeI32Load, 2, 0, wasm.OpcodeLocalTee, 0,
		wasm.OpcodeI32Eqz,
		wasm.OpcodeBrTable, 1, 0, 1,
		wasm.OpcodeEnd,
		wasm.OpcodeEnd,
		wasm.OpcodeLocalGet, 2, wasm.OpcodeEnd,
	}},
	// How many values are odd, counted by an if.
	{"odd", []byte{
		wasm.OpcodeLoop, 0x40,
		wasm.OpcodeLocalGet, 0, wasm.OpcodeI32Load, 2, 4, wasm.OpcodeLocalGet, 1, wasm.OpcodeI32And,
		wasm.OpcodeIf, 0x40,
		wasm.OpcodeLocalGet, 2, wasm.OpcodeLocalGet, 1, wasm.OpcodeI32Add, wasm.OpcodeLocalSet, 2,
		wasm.OpcodeEnd,
		wasm.OpcodeLocalGet, 0, wasm.OpcodeI32Load, 2, 0, wasm.OpcodeLocalTee, 0,
		wasm.OpcodeBrIf, 0,
		wasm.OpcodeEnd,
		wasm.OpcodeLocalGet, 2, wasm.OpcodeEnd,
	}},
	// The sum, leaving from the middle of the body and going round unconditionally.
	{"middle", []byte{
		wasm.OpcodeBlock, 0x40,
		wasm.OpcodeLoop, 0x40,
		wasm.OpcodeLocalGet, 0, wasm.OpcodeI32Eqz, wasm.OpcodeBrIf, 1,
		wasm.OpcodeLocalGet, 2, wasm.OpcodeLocalGet, 0, wasm.OpcodeI32Load, 2, 4, wasm.OpcodeI32Add, wasm.OpcodeLocalSet, 2,
		wasm.OpcodeLocalGet, 0, wasm.OpcodeI32Load, 2, 0, wasm.OpcodeLocalSet, 0,
		wasm.OpcodeBr, 0,
		wasm.OpcodeEnd,
		wasm.OpcodeEnd,
		wasm.OpcodeLocalGet, 2, wasm.OpcodeEnd,
	}},
	// The list reversed in place, as CoreMark's is: the new head.
	{"reverse", []byte{
		wasm.OpcodeLoop, 0x40,
		wasm.OpcodeLocalGet, 0, wasm.OpcodeI32Load, 2, 0, wasm.OpcodeLocalSet, 3,
		wasm.OpcodeLocalGet, 0, wasm.OpcodeLocalGet, 2, wasm.OpcodeI32Store, 2, 0,
		wasm.OpcodeLocalGet, 0, wasm.OpcodeLocalSet, 2,
		wasm.OpcodeLocalGet, 3, wasm.OpcodeLocalTee, 0,
		wasm.OpcodeBrIf, 0,
		wasm.OpcodeEnd,
		wasm.OpcodeLocalGet, 2, wasm.OpcodeEnd,
	}},
}

// TestUnrolledLoopsE2E runs each of unrollBodies over lists of every length up to ten, and over lists that
// end in a node out of bounds, on the compiler and on the interpreter, which must agree: on the result,
// on trapping, and on the memory the loops leave.
func TestUnrolledLoopsE2E(t *testing.T) {
	i32 := wasm.ValueTypeI32
	m := &wasm.Module{
		TypeSection:   []wasm.FunctionType{{Params: []wasm.ValueType{i32, i32}, Results: []wasm.ValueType{i32}}},
		MemorySection: []wasm.Memory{{Min: 1}},
	}
	for i, b := range unrollBodies {
		m.FunctionSection = append(m.FunctionSection, 0)
		m.CodeSection = append(m.CodeSection, wasm.Code{LocalTypes: []wasm.ValueType{i32, i32}, Body: b.body})
		m.ExportSection = append(m.ExportSection, wasm.Export{Name: b.name, Type: wasm.ExternTypeFunc, Index: wasm.Index(i)})
	}
	bin := binaryencoding.EncodeModule(m)

	ctx := context.Background()
	unrolled := 0
	frontend.UnrollHook = func() { unrolled++ }
	defer func() { frontend.UnrollHook = nil }()
	compiler := wazy.NewRuntimeWithConfig(ctx, wazy.NewRuntimeConfigCompiler())
	defer compiler.Close(ctx)
	compiled, err := compiler.CompileModule(ctx, bin)
	require.NoError(t, err)
	require.Equal(t, len(unrollBodies), unrolled)
	interpreter := wazy.NewRuntimeWithConfig(ctx, wazy.NewRuntimeConfigInterpreter())
	defer interpreter.Close(ctx)
	interpreted, err := interpreter.CompileModule(ctx, bin)
	require.NoError(t, err)

	// list lays out n nodes, the i-th at 16+16i with the value 3i+n%5, and returns the head; with end set,
	// the last node's next is an address whose value is past the memory's end.
	list := func(mem []byte, n int, end bool) uint64 {
		clear(mem[:4096])
		for i := 0; i < n; i++ {
			at := 16 + 16*i
			next := uint32(at + 16)
			if i == n-1 {
				next = 0
				if end {
					next = uint32(len(mem) - 6)
				}
			}
			binary.LittleEndian.PutUint32(mem[at:], next)
			binary.LittleEndian.PutUint32(mem[at+4:], uint32(3*i+n%5))
		}
		if n == 0 {
			return 0
		}
		return 16
	}
	for _, b := range unrollBodies {
		for n := 0; n <= 10; n++ {
			for _, end := range []bool{false, true} {
				if end && n == 0 {
					continue
				}
				t.Run(fmt.Sprintf("%s/%d/%v", b.name, n, end), func(t *testing.T) {
					var results [2][]uint64
					var errs [2]error
					var mems [2][]byte
					for k, mod := range []wazy.CompiledModule{compiled, interpreted} {
						r := compiler
						if k == 1 {
							r = interpreter
						}
						inst, err := r.InstantiateModule(ctx, mod, wazy.NewModuleConfig().WithName(""))
						require.NoError(t, err)
						mem, _ := inst.Memory().Read(0, inst.Memory().Size())
						head := list(mem, n, end)
						results[k], errs[k] = inst.ExportedFunction(b.name).Call(ctx, head, 1)
						mems[k] = append([]byte(nil), mem[:4096]...)
						require.NoError(t, inst.Close(ctx))
					}
					require.Equal(t, errs[1] != nil, errs[0] != nil, "compiler %v, interpreter %v", errs[0], errs[1])
					require.Equal(t, results[1], results[0])
					require.Equal(t, mems[1], mems[0])
				})
			}
		}
	}
}
