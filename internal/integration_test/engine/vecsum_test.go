package adhoc

import (
	"context"
	_ "embed"
	"math/rand"
	"runtime"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/engine/native/frontend"
	"github.com/samyfodil/wazy/internal/platform"
	"github.com/samyfodil/wazy/internal/testing/require"
	"github.com/samyfodil/wazy/internal/wasm"
)

//go:embed testdata/vecsum.wasm
var vecSumWasm []byte

// TestVecSum runs sums the compiler computes four terms to a vector against the interpreter, on the
// same memory: every result, every trap, and the locals the sums leave behind must agree. The cases
// put the rows in bounds, across the end of memory, and where their i32 addresses wrap.
func TestVecSum(t *testing.T) {
	if !platform.CompilerSupported() {
		t.Skip()
	}
	features := platform.CpuFeatures
	t.Run("avx", func(t *testing.T) {
		if !features.Has(platform.CpuFeatureAmd64AVX) {
			t.Skip()
		}
		testVecSum(t)
	})
	t.Run("sse", func(t *testing.T) {
		platform.CpuFeatures = features &^ (platform.CpuFeatureAmd64AVX | platform.CpuFeatureAmd64AVX2)
		defer func() { platform.CpuFeatures = features }()
		testVecSum(t)
	})
}

func testVecSum(t *testing.T) {
	ctx := context.Background()
	var sums int
	frontend.VecSumHook = func(wasm.Index, int) { sums++ }
	compiler := wazy.NewRuntimeWithConfig(ctx, wazy.NewRuntimeConfigCompiler())
	defer compiler.Close(ctx)
	_, err := compiler.CompileModule(ctx, vecSumWasm)
	frontend.VecSumHook = nil
	require.NoError(t, err)
	if runtime.GOARCH == "amd64" || runtime.GOARCH == "arm64" {
		require.Equal(t, 4, sums, "sad, sum4, dot8 and dup should be vector sums, and six, kmul and extra not")
	}
	interp := wazy.NewRuntimeWithConfig(ctx, wazy.NewRuntimeConfigInterpreter())
	defer interp.Close(ctx)

	const memSize = 2 * 65536
	mem := make([]byte, memSize)
	rand.New(rand.NewSource(1)).Read(mem)
	neg := func(v int32) uint64 { return api.EncodeI32(v) }
	for _, tc := range []struct {
		name string
		fn   string
		args []uint64
	}{
		{"sad", "sad", []uint64{0, 65536, 64, 8}},
		{"sad one row", "sad", []uint64{1000, 2000, 0, 1}},
		{"sad overlapping rows", "sad", []uint64{3, 7, 4, 50}},
		{"sad rows going down", "sad", []uint64{8192, 4096, neg(-64), 40}},
		{"sad last row in bounds", "sad", []uint64{memSize - 40 - 7*64, 0, 64, 8}},
		{"sad last row out of bounds", "sad", []uint64{memSize - 39 - 7*64, 0, 64, 8}},
		{"sad second stream out of bounds", "sad", []uint64{0, memSize - 31, 0, 3}},
		{"sad wraps into memory", "sad", []uint64{0xfffffff8, 100, 0, 2}},
		{"sad wraps partly", "sad", []uint64{0xfffffff4, 100, 0, 2}},
		{"sad wraps second row", "sad", []uint64{0xffffff00, 100, 0xf8, 2}},
		{"sum4", "sum4", []uint64{100}},
		{"sum4 at the end", "sum4", []uint64{memSize - 24}},
		{"sum4 past the end", "sum4", []uint64{memSize - 23}},
		{"sum4 wraps", "sum4", []uint64{0xfffffffc}},
		{"dot8", "dot8", []uint64{0, 8000, 3, 0xff00ff}},
		{"dot8 masked shift", "dot8", []uint64{12, 8004, 35, 0xffff}},
		{"dot8 aliased", "dot8", []uint64{64, 64, 0, ^uint64(0) >> 32}},
		{"dot8 past the end", "dot8", []uint64{0, memSize - 64, 1, 1}},
		{"dup", "dup", []uint64{40}},
		{"six", "six", []uint64{40}},
		{"kmul", "kmul", []uint64{40}},
		{"extra", "extra", []uint64{40, 400}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var results [2][]uint64
			var errs [2]error
			for i, r := range []wazy.Runtime{compiler, interp} {
				m, err := r.Instantiate(ctx, vecSumWasm)
				require.NoError(t, err)
				require.True(t, m.Memory().Write(0, mem))
				results[i], errs[i] = m.ExportedFunction(tc.fn).Call(ctx, tc.args...)
				require.NoError(t, m.Close(ctx))
			}
			require.Equal(t, errs[1] == nil, errs[0] == nil, "compiler: %v, interpreter: %v", errs[0], errs[1])
			if errs[1] != nil {
				require.Contains(t, errs[0].Error(), "out of bounds memory access")
			}
			require.Equal(t, results[1], results[0])
		})
	}
}
