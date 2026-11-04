package frontend

import (
	_ "embed"
	"strings"
	"testing"

	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/engine/native/nativeapi"
	"github.com/samyfodil/wazy/internal/engine/native/ssa"
	"github.com/samyfodil/wazy/internal/testing/require"
	"github.com/samyfodil/wazy/internal/wasm"
	"github.com/samyfodil/wazy/internal/wasm/binary"
)

// derivedAddressWasm is internal/integration_test/engine/testdata/derived_address.wasm, built from
// the derived_address.wat beside it.
//
//go:embed testdata/derived_address.wasm
var derivedAddressWasm []byte

func TestCompiler_derivedSafeAddress(t *testing.T) {
	m, err := binary.DecodeModule(derivedAddressWasm, api.CoreFeaturesV2, 1, false, 0, false, false)
	require.NoError(t, err)
	require.NoError(t, m.Validate(api.CoreFeaturesV2))
	// The bounds checks left in each function: x + k needs none of its own when k plus its size is
	// within the 24 bytes the access through x proved.
	for i, checks := range map[int]int{0: 1, 1: 2, 2: 2, 3: 1, 5: 1} {
		b := ssa.NewBuilder()
		offset := nativeapi.NewModuleContextOffsetData(m, false)
		fc := NewFrontendCompiler(m, b, &offset, false, false)
		code := &m.CodeSection[i]
		fc.Init(wasm.Index(i), m.FunctionSection[i], &m.TypeSection[m.FunctionSection[i]], code.LocalTypes, code.Body, false, 0)
		fc.LowerToSSA()
		require.Equal(t, checks, strings.Count(fc.formatBuilder(), "memory_out_of_bounds"), "function %d", i)
	}
}
