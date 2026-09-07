package frontend

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/engine/native/nativeapi"
	"github.com/samyfodil/wazy/internal/engine/native/ssa"
	"github.com/samyfodil/wazy/internal/leb128"
	"github.com/samyfodil/wazy/internal/testing/require"
	"github.com/samyfodil/wazy/internal/wasm"
)

// TestCompiler_lowerMemoryFill_dispatch pins which arm of the memory.fill
// lowering each operand shape takes. The byte-level e2e tests cannot do this:
// every arm writes the same bytes, so collapsing them all back to one generic
// loop would leave the whole suite green while giving up the optimization.
var blockHeader = regexp.MustCompile(`(?m)^blk\d+:`)

func TestCompiler_lowerMemoryFill_dispatch(t *testing.T) {
	// i32.const size / i32.const value produce a constant operand; local.get
	// produces one the compiler cannot see through.
	constOp := func(v uint32) []byte {
		return append([]byte{wasm.OpcodeI32Const}, leb128.EncodeInt32(int32(v))...)
	}
	localOp := func(i byte) []byte { return []byte{wasm.OpcodeLocalGet, i} }

	for _, tc := range []struct {
		name        string
		value, size []byte
		// blocks is how many basic blocks the lowering leaves behind: 1 for
		// straight-line code, more once a loop or a run-time dispatch is needed.
		blocks int
		// memclr is whether a call to the Go runtime's memclr is emitted at all.
		memclr bool
		// byteTail is whether a single-byte store is emitted at all: the bottom
		// rung of the scalar ladder, which only a region that may be shorter
		// than one vector reaches.
		byteTail bool
		// stores is the exact number of 128-bit stores, when they are unrolled.
		stores int
	}{
		{
			// Under the constant ceiling: straight-line stores, no branch, no call.
			name:  "constant size, constant value, inline",
			value: constOp(0), size: constOp(memoryFillInlineMaxBytes),
			blocks: 1, stores: memoryFillInlineMaxBytes / 16,
		},
		{
			// A size that is not a multiple of 16 gets an overlapping last store.
			name:  "constant size not a multiple of 16",
			value: constOp(0xab), size: constOp(100),
			blocks: 1, stores: 100/16 + 1,
		},
		{
			// A dynamic value still folds to straight-line stores: only the
			// size decides the shape.
			name:  "constant size, dynamic value, inline",
			value: localOp(0), size: constOp(64),
			blocks: 1, stores: 4,
		},
		{
			// Zero and large: memclr directly, with no run-time dispatch.
			name:  "constant size, constant zero, memclr",
			value: constOp(0), size: constOp(memoryFillMemclrMinBytes),
			blocks: 1, memclr: true,
		},
		{
			// The same size with a known nonzero byte can never reach memclr.
			name:  "constant size, constant nonzero, loops",
			value: constOp(1), size: constOp(memoryFillMemclrMinBytes),
			blocks: 7,
		},
		{
			// Over the inline ceiling but under the memclr one: loops only.
			name:  "constant size between the thresholds",
			value: constOp(0), size: constOp(memoryFillInlineMaxBytes + 1),
			blocks: 7,
		},
		{
			// Nothing is known: the full shape, gate and memclr arm included.
			name:  "dynamic size and value",
			value: localOp(0), size: localOp(1),
			blocks: 18, memclr: true, byteTail: true,
		},
		{
			// A constant zero byte folds half the dispatch away, but the size
			// still has to be tested at run time.
			name:  "dynamic size, constant zero value",
			value: constOp(0), size: localOp(0),
			blocks: 18, memclr: true, byteTail: true,
		},
		{
			// A constant nonzero byte folds the memclr arm away entirely.
			name:  "dynamic size, constant nonzero value",
			value: constOp(2), size: localOp(0),
			blocks: 17, byteTail: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := append([]byte{wasm.OpcodeI32Const, 0}, tc.value...)
			body = append(body, tc.size...)
			body = append(body, wasm.OpcodeMiscPrefix, byte(wasm.OpcodeMiscMemoryFill), 0, wasm.OpcodeEnd)

			m := &wasm.Module{
				TypeSection:     []wasm.FunctionType{{Params: []wasm.ValueType{wasm.ValueTypeI32, wasm.ValueTypeI32}}},
				MemorySection:   []wasm.Memory{{Min: 1, Cap: 1, Max: 1, IsMaxEncoded: true}},
				FunctionSection: []wasm.Index{0},
				CodeSection:     []wasm.Code{{Body: body}},
			}
			require.NoError(t, m.Validate(api.CoreFeaturesV2))

			b := ssa.NewBuilder()
			offset := nativeapi.NewModuleContextOffsetData(m, false)
			fc := NewFrontendCompiler(m, b, &offset, false, false, false)
			fc.Init(0, 0, &m.TypeSection[0], nil, body, false, 0)
			fc.LowerToSSA()
			got := fc.formatBuilder()

			require.Equal(t, tc.blocks, len(blockHeader.FindAllString(got, -1)), got)
			// The memclr call is an indirect call through the execution
			// context; its slot is what distinguishes it from memmove.
			memclrLoad := fmt.Sprintf("Load v0, %#x", nativeapi.ExecutionContextOffsetMemclrAddress.U32())
			require.Equal(t, tc.memclr, strings.Contains(got, memclrLoad), got)
			// Istore8 is only ever the byte tail here: every other store is a vector.
			require.Equal(t, tc.byteTail, strings.Contains(got, "Istore8"), got)
			if tc.stores > 0 {
				require.Equal(t, tc.stores, strings.Count(got, "Store v"), got)
			}
		})
	}
}
