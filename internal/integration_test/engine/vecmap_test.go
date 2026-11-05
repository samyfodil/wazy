package adhoc

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"math"
	"math/rand"
	"runtime"
	"testing"
	"time"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/engine/native/frontend"
	"github.com/samyfodil/wazy/internal/platform"
	"github.com/samyfodil/wazy/internal/testing/require"
	"github.com/samyfodil/wazy/internal/wasm"
)

//go:embed testdata/vecmap.wasm
var vecMapWasm []byte

// TestVecMap runs loops the compiler vectorizes against the interpreter, which does not, on the same
// memory: every element, every trap and every bit of memory afterwards must agree. The cases choose
// the aliasing and bounds that send a loop down its vector path, or keep it on the scalar one, and the
// element values include NaNs, infinities, zeros of both signs and subnormals.
func TestVecMap(t *testing.T) {
	if !platform.CompilerSupported() {
		t.Skip()
	}
	features := platform.CpuFeatures
	t.Run("avx2", func(t *testing.T) {
		if !features.Has(platform.CpuFeatureAmd64AVX2) {
			t.Skip()
		}
		testVecMap(t)
	})
	t.Run("avx", func(t *testing.T) { // 256-bit floats, 128-bit integers
		if !features.Has(platform.CpuFeatureAmd64AVX) {
			t.Skip()
		}
		platform.CpuFeatures = features &^ platform.CpuFeatureAmd64AVX2
		defer func() { platform.CpuFeatures = features }()
		testVecMap(t)
	})
	t.Run("sse", func(t *testing.T) {
		platform.CpuFeatures = features &^ (platform.CpuFeatureAmd64AVX | platform.CpuFeatureAmd64AVX2)
		defer func() { platform.CpuFeatures = features }()
		testVecMap(t)
	})
}

func testVecMap(t *testing.T) {
	ctx := context.Background()
	var vectorized int
	frontend.VecPlanHook = func(_ wasm.Index, _ int, ok bool, _ string) {
		if ok {
			vectorized++
		}
	}
	compiler := wazy.NewRuntimeWithConfig(ctx, wazy.NewRuntimeConfigCompiler())
	defer compiler.Close(ctx)
	_, err := compiler.CompileModule(ctx, vecMapWasm)
	frontend.VecPlanHook = nil
	require.NoError(t, err)
	if runtime.GOARCH == "amd64" {
		require.Equal(t, 10, vectorized, "every loop of the module but counter's should be vectorized")
	}
	interp := wazy.NewRuntimeWithConfig(ctx, wazy.NewRuntimeConfigInterpreter())
	defer interp.Close(ctx)

	const memSize = 2 * 65536
	rng := rand.New(rand.NewSource(1))
	special := []uint64{
		math.Float64bits(math.NaN()), 0x7ff4000000000001, math.Float64bits(math.Inf(1)), math.Float64bits(math.Inf(-1)),
		0, 1 << 63, 1, math.Float64bits(-1.5), 0x000fffffffffffff,
	}
	fill := func() []byte {
		b := make([]byte, memSize)
		for i := 0; i+8 <= len(b); i += 8 {
			var v uint64
			if rng.Intn(8) == 0 {
				v = special[rng.Intn(len(special))]
			} else {
				v = math.Float64bits(rng.NormFloat64() * 1000)
			}
			if rng.Intn(2) == 0 { // An f32 view of the same memory gets interesting values too.
				v = uint64(math.Float32bits(float32(rng.NormFloat64()*100)))<<32 | uint64(math.Float32bits(float32(rng.NormFloat64())))
			}
			for k := 0; k < 8; k++ {
				b[i+k] = byte(v >> (8 * k))
			}
		}
		return b
	}

	type call struct {
		fn   string
		args []uint64
	}
	f64 := func(f float64) uint64 { return math.Float64bits(f) }
	cases := []struct {
		name string
		call call
	}{
		{"axpy disjoint", call{"axpy", []uint64{0, 16384, 32768, 1000, f64(1.25)}}},
		{"axpy short", call{"axpy", []uint64{0, 16384, 32768, 9, f64(-3)}}},
		{"axpy one", call{"axpy", []uint64{0, 16384, 32768, 1, f64(2)}}},
		{"axpy zero count runs once", call{"axpy", []uint64{0, 16384, 32768, 0, f64(2)}}},
		{"axpy dst is a", call{"axpy", []uint64{16384, 16384, 32768, 1000, f64(0.5)}}},
		{"axpy dst overlaps a forward", call{"axpy", []uint64{16392, 16384, 32768, 1000, f64(0.5)}}},
		{"axpy dst overlaps a backward", call{"axpy", []uint64{16376, 16384, 32768, 1000, f64(0.5)}}},
		{"axpy dst overlaps b", call{"axpy", []uint64{32760, 16384, 32768, 1000, f64(0.5)}}},
		{"axpy misaligned", call{"axpy", []uint64{3, 16389, 32771, 999, f64(7)}}},
		{"axpy runs off the end", call{"axpy", []uint64{memSize - 400, 16384, 32768, 1000, f64(1)}}},
		{"axpy reads off the end", call{"axpy", []uint64{0, memSize - 800, 32768, 1000, f64(1)}}},
		{"axpy chunks", call{"axpy", []uint64{0, 40000, 80000, 4999, f64(1.5)}}},
		{"inplace32", call{"inplace32", []uint64{4096, 3001}}},
		{"inplace32 one", call{"inplace32", []uint64{4096, 1}}},
		{"inplace32 runs off the end", call{"inplace32", []uint64{memSize - 400, 1000}}},
		{"unrolled", call{"unrolled", []uint64{0, 16384, 50000, 600}}},
		{"unrolled scale in the output", call{"unrolled", []uint64{0, 16384, 4000, 600}}},
		{"unrolled scale in the input", call{"unrolled", []uint64{0, 16384, 16384 + 800, 600}}},
		{"unrolled in place", call{"unrolled", []uint64{16384, 16384, 50000, 600}}},
		{"isum", call{"isum", []uint64{0, 1000}}},
		{"isum short", call{"isum", []uint64{8, 3}}},
		{"isum zero", call{"isum", []uint64{0, 0}}},
		{"isum negative count", call{"isum", []uint64{0, api.EncodeI32(-5)}}},
		{"isum chunks", call{"isum", []uint64{0, 16000}}},
		{"isum runs off the end", call{"isum", []uint64{memSize - 800, 1000}}},
		{"ixor", call{"ixor", []uint64{4, 3001}}},
		{"ixor zero", call{"ixor", []uint64{4, 0}}},
		{"ixor misaligned", call{"ixor", []uint64{3, 777}}},
		{"ixor runs off the end", call{"ixor", []uint64{memSize - 40, 100}}},
		{"iand", call{"iand", []uint64{0, 1000}}},
		{"iand to the end", call{"iand", []uint64{memSize - 4004, 1000}}},
		{"iand runs off the end", call{"iand", []uint64{memSize - 4000, 1000}}},
		{"imap32", call{"imap32", []uint64{0, 16384, 32768, 1000, 7}}},
		{"imap32 in place", call{"imap32", []uint64{16384, 16384, 32768, 1000, 7}}},
		{"imap32 dst overlaps a", call{"imap32", []uint64{16388, 16384, 32768, 1000, 0}}},
		{"imap32 dst is b", call{"imap32", []uint64{32768, 16384, 32768, 999, 1}}},
		{"imap32 runs off the end", call{"imap32", []uint64{memSize - 400, 16384, 32768, 1000, 1}}},
		{"imap64", call{"imap64", []uint64{0, 16384, 32768, 1000, 0xff00ff00ff00ff}}},
		{"imap64 in place", call{"imap64", []uint64{16384, 16384, 32768, 1000, ^uint64(0)}}},
		{"imap64 dst is y", call{"imap64", []uint64{32768, 16384, 32768, 1000, 0x1234}}},
		{"imap64 dst overlaps x", call{"imap64", []uint64{16376, 16384, 32768, 1000, 3}}},
		{"wrapped", call{"wrapped", []uint64{0, 1000}}},
		{"wrapped wraps", call{"wrapped", []uint64{api.EncodeI32(-8), 1000}}},
		{"wrapped wraps later", call{"wrapped", []uint64{api.EncodeI32(-800), 1000}}},
		{"unused", call{"unused", []uint64{0, 16384, 32768, 1000}}},
		{"unused load runs off the end", call{"unused", []uint64{0, 16384, memSize - 80, 1000}}},
		{"counter", call{"counter", []uint64{0, 1000, 1000}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mem := fill()
			var results [2][]uint64
			var errs [2]error
			var mems [2][]byte
			for i, r := range []wazy.Runtime{compiler, interp} {
				m, err := r.Instantiate(ctx, vecMapWasm)
				require.NoError(t, err)
				require.True(t, m.Memory().Write(0, mem))
				results[i], errs[i] = m.ExportedFunction(tc.call.fn).Call(ctx, tc.call.args...)
				out, ok := m.Memory().Read(0, memSize)
				require.True(t, ok)
				mems[i] = append([]byte(nil), out...)
				require.NoError(t, m.Close(ctx))
			}
			require.Equal(t, errs[1] == nil, errs[0] == nil, "compiler: %v, interpreter: %v", errs[0], errs[1])
			require.Equal(t, len(results[1]), len(results[0]))
			for i := range results[0] {
				require.Equal(t, results[1][i], results[0][i])
			}
			if !bytes.Equal(mems[0], mems[1]) {
				for i := range mems[0] {
					if mems[0][i] != mems[1][i] {
						t.Fatalf("memory differs first at %d: compiler %#x, interpreter %#x", i, mems[0][i], mems[1][i])
					}
				}
			}
		})
	}
}

// TestVecMapCancel cancels a vector map loop: the chunks it runs in check for it, so a long one stops.
func TestVecMapCancel(t *testing.T) {
	if !platform.CompilerSupported() {
		t.Skip()
	}
	ctx := context.Background()
	r := wazy.NewRuntimeWithConfig(ctx, wazy.NewRuntimeConfigCompiler().WithMemoryLimitPages(65536))
	defer r.Close(ctx)
	m, err := r.Instantiate(ctx, vecMapWasm)
	require.NoError(t, err)
	_, ok := m.Memory().Grow(8192 - 2) // 512 MiB: plenty of elements for one call to take a while.
	require.True(t, ok)
	f := m.ExportedFunction("inplace32")
	cctx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		var err error
		for i := 0; i < 1000 && err == nil; i++ {
			_, err = f.Call(cctx, 0, api.EncodeU32((8192*65536-8)/4))
		}
		done <- err
	}()
	select {
	case err := <-done:
		require.True(t, errors.Is(err, context.DeadlineExceeded), "got %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("a cancelled vector map loop did not stop")
	}
}
