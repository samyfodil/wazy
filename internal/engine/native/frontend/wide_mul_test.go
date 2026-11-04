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

// wideMulWasm is internal/integration_test/engine/testdata/wide_mul.wasm, built from the
// wide_mul.wat beside it, which says where each function comes from.
//
//go:embed testdata/wide_mul.wasm
var wideMulWasm []byte

func TestCompiler_fuseWideMul(t *testing.T) {
	m, err := binary.DecodeModule(wideMulWasm, api.CoreFeaturesV2, 1, false, 0, false, false)
	require.NoError(t, err)
	for i, tc := range []struct {
		name                   string
		umulhi, imul, shifts32 int
	}{
		// hi = a*bh + ah*b + umulhi(a, b), lo = a*b.
		{name: "multi3", umulhi: 1, imul: 3},
		// hi = ah*b + umulhi(a, b), lo = a*b.
		{name: "multi3_4", umulhi: 1, imul: 2},
		{name: "gomul", umulhi: 1, imul: 1},
		// Four 32x32 products, the carries and the halves all stay.
		{name: "notmul", imul: 4, shifts32: 5},
		{name: "swapmul", umulhi: 1, imul: 1},
		// A low half shifted by 31 is not x*y, so its three products and shifts all stay.
		{name: "lo31", imul: 3, shifts32: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := ssa.NewBuilder()
			offset := nativeapi.NewModuleContextOffsetData(m, false)
			fc := NewFrontendCompiler(m, b, &offset, false, false)
			typeIndex := m.FunctionSection[i]
			code := &m.CodeSection[i]
			fc.Init(wasm.Index(i), typeIndex, &m.TypeSection[typeIndex], code.LocalTypes, code.Body, false, 0)
			fc.LowerToSSA()
			b.RunPasses()

			ssaText := fc.formatBuilder()
			require.Equal(t, tc.umulhi, strings.Count(ssaText, "= Umulhi "), ssaText)
			require.Equal(t, tc.imul, strings.Count(ssaText, "= Imul "), ssaText)
			require.Equal(t, tc.shifts32, strings.Count(ssaText, "= Ushr "), ssaText)
		})
	}
}
