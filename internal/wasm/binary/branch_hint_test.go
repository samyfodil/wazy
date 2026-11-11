package binary

import (
	"testing"

	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/testing/require"
	"github.com/samyfodil/wazy/internal/wasm"
)

// The sections of a module of one function, (param i32) (result i32) with an i32 local, whose body is
// local.get 0; if (result i32) i32.const 1 else i32.const 2 end. The if is at body offset 2, which is offset
// 5 from the start of the locals declaration. wasm-tools wrote these from the text format.
var (
	branchHintHeader  = []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
	branchHintTypes   = []byte{0x01, 0x06, 0x01, 0x60, 0x01, 0x7f, 0x01, 0x7f}
	branchHintFuncs   = []byte{0x03, 0x02, 0x01, 0x00}
	branchHintExports = []byte{0x07, 0x05, 0x01, 0x01, 0x66, 0x00, 0x00}
	branchHintCode    = []byte{0x0a, 0x10, 0x01, 0x0e, 0x01, 0x01, 0x7f, 0x20, 0x00, 0x04, 0x7f, 0x41, 0x01, 0x05, 0x41, 0x02, 0x0b, 0x0b}
)

// branchHintCustom is a branch hint section giving function 0 one hint at offset with value.
func branchHintCustom(offset, value byte) []byte {
	name := []byte(branchHintSectionName)
	body := append([]byte{byte(len(name))}, name...)
	body = append(body, 0x01, 0x00, 0x01, offset, 0x01, value)
	return append([]byte{0x00, byte(len(body))}, body...)
}

func concatBytes(parts ...[]byte) (ret []byte) {
	for _, p := range parts {
		ret = append(ret, p...)
	}
	return
}

func TestDecodeModule_branchHints(t *testing.T) {
	decode := func(t *testing.T, bin []byte) *wasm.Module {
		m, err := DecodeModule(bin, api.CoreFeaturesV2, wasm.MemoryLimitPages, false, 0, false, false)
		require.NoError(t, err)
		return m
	}

	t.Run("rebased onto the body", func(t *testing.T) {
		m := decode(t, concatBytes(branchHintHeader, branchHintTypes, branchHintFuncs, branchHintExports,
			branchHintCustom(5, 0), branchHintCode))
		require.Equal(t, []wasm.BranchHint{{Offset: 2, Likely: false}}, m.CodeSection[0].BranchHints)
	})

	t.Run("likely", func(t *testing.T) {
		m := decode(t, concatBytes(branchHintHeader, branchHintTypes, branchHintFuncs, branchHintExports,
			branchHintCustom(5, 1), branchHintCode))
		require.Equal(t, []wasm.BranchHint{{Offset: 2, Likely: true}}, m.CodeSection[0].BranchHints)
	})

	t.Run("a hint on another instruction is dropped", func(t *testing.T) {
		// Offset 3 is the local.get, where wasm-tools puts a hint written before a folded (if ...).
		m := decode(t, concatBytes(branchHintHeader, branchHintTypes, branchHintFuncs, branchHintExports,
			branchHintCustom(3, 0), branchHintCode))
		require.Nil(t, m.CodeSection[0].BranchHints)
	})

	t.Run("a hint inside the locals declaration is dropped", func(t *testing.T) {
		m := decode(t, concatBytes(branchHintHeader, branchHintTypes, branchHintFuncs, branchHintExports,
			branchHintCustom(1, 0), branchHintCode))
		require.Nil(t, m.CodeSection[0].BranchHints)
	})

	t.Run("a section after the code section is ignored", func(t *testing.T) {
		m := decode(t, concatBytes(branchHintHeader, branchHintTypes, branchHintFuncs, branchHintExports,
			branchHintCode, branchHintCustom(5, 0)))
		require.Nil(t, m.CodeSection[0].BranchHints)
	})

	t.Run("indexed past the imported functions", func(t *testing.T) {
		// (import "env" "f" (func)) and the same function as above with no locals, so the if is at offset 3 of
		// function 1.
		m := decode(t, []byte{
			0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00, 0x01, 0x09, 0x02, 0x60, 0x00, 0x00, 0x60, 0x01, 0x7f,
			0x01, 0x7f, 0x02, 0x09, 0x01, 0x03, 0x65, 0x6e, 0x76, 0x01, 0x66, 0x00, 0x00, 0x03, 0x02, 0x01, 0x01,
			0x00, 0x20, 0x19, 0x6d, 0x65, 0x74, 0x61, 0x64, 0x61, 0x74, 0x61, 0x2e, 0x63, 0x6f, 0x64, 0x65, 0x2e,
			0x62, 0x72, 0x61, 0x6e, 0x63, 0x68, 0x5f, 0x68, 0x69, 0x6e, 0x74, 0x01, 0x01, 0x01, 0x03, 0x01, 0x01,
			0x0a, 0x0e, 0x01, 0x0c, 0x00, 0x20, 0x00, 0x04, 0x7f, 0x41, 0x01, 0x05, 0x41, 0x02, 0x0b, 0x0b,
		})
		require.Equal(t, []wasm.BranchHint{{Offset: 2, Likely: true}}, m.CodeSection[0].BranchHints)
	})
}

func TestDecodeBranchHintSection(t *testing.T) {
	require.Equal(t, []funcBranchHints{
		{funcIndex: 0, hints: []wasm.BranchHint{{Offset: 5, Likely: false}, {Offset: 9, Likely: true}}},
		{funcIndex: 3, hints: []wasm.BranchHint{{Offset: 2, Likely: true}}},
	}, decodeBranchHintSection([]byte{0x02, 0x00, 0x02, 0x05, 0x01, 0x00, 0x09, 0x01, 0x01, 0x03, 0x01, 0x02, 0x01, 0x01}))

	// Each of these is malformed, and the whole section is ignored.
	for name, data := range map[string][]byte{
		"empty":                   {},
		"truncated":               {0x01, 0x00, 0x01, 0x05, 0x01},
		"size other than one":     {0x01, 0x00, 0x01, 0x05, 0x02, 0x00},
		"value other than 0 or 1": {0x01, 0x00, 0x01, 0x05, 0x01, 0x02},
		"offsets out of order":    {0x01, 0x00, 0x02, 0x09, 0x01, 0x00, 0x05, 0x01, 0x00},
		"functions out of order":  {0x02, 0x03, 0x01, 0x05, 0x01, 0x00, 0x01, 0x01, 0x05, 0x01, 0x00},
		"trailing bytes":          {0x01, 0x00, 0x01, 0x05, 0x01, 0x00, 0x00},
		"count past the input":    {0x7f},
	} {
		require.Nil(t, decodeBranchHintSection(data), name)
	}
}
