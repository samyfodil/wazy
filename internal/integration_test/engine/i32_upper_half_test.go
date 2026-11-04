package adhoc

import (
	"context"
	_ "embed"
	"os"
	"os/exec"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/experimental"
	"github.com/samyfodil/wazy/internal/platform"
	"github.com/samyfodil/wazy/internal/testing/require"
)

//go:embed testdata/i32_upper_half.wasm
var i32UpperHalfWasm []byte

const i32UpperHalfChildEnv = "WAZY_I32_UPPER_HALF_CHILD"

// TestI32UpperHalf computes -8 with every kind of i32 producer, a host function's result included, and uses it as an address in the last
// page of a 4 GiB memory, again after a call. The amd64 backend lets an address's register stand for
// its zero extension, which holds only while every i32 in a register has bits 63:32 zero; a producer
// that leaves them set would make that access land outside the memory.
func TestI32UpperHalf(t *testing.T) {
	if !platform.CompilerSupported() {
		t.Skip()
	}
	if os.Getenv(i32UpperHalfChildEnv) == "" {
		// The 4 GiB memory lives in a child process, gone once it exits. Kept by this process, it
		// would make every later fork, such as TestLoopYield's, reserve another 4 GiB on a system
		// that does not overcommit.
		cmd := exec.Command(os.Args[0], "-test.run=^TestI32UpperHalf$", "-test.v")
		cmd.Env = append(os.Environ(), i32UpperHalfChildEnv+"=1")
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, string(out))
		require.Contains(t, string(out), "--- PASS: TestI32UpperHalf")
		return
	}
	ctx := context.Background()
	r := wazy.NewRuntimeWithConfig(ctx, wazy.NewRuntimeConfigCompiler().WithCoreFeatures(api.CoreFeaturesV2|experimental.CoreFeaturesThreads))
	defer r.Close(ctx)
	// A host function that hands back its i32 sign-extended to 64 bits, as Go code writing the stack
	// often does: the compiled caller must still see only its 32 bits.
	_, err := r.NewHostModuleBuilder("env").NewFunctionBuilder().
		WithGoFunction(api.GoFunc(func(_ context.Context, stack []uint64) {
			neg := int64(-8)
			stack[0] = uint64(neg)
		}), nil, []api.ValueType{api.ValueTypeI32}).
		Export("neg").Instantiate(ctx)
	require.NoError(t, err)
	mod, err := r.Instantiate(ctx, i32UpperHalfWasm)
	require.NoError(t, err)

	param := map[string]uint64{"add": 8, "mul": 8, "xor": 7}
	for name := range mod.ExportedFunctionDefinitions() {
		p, ok := param[name]
		if !ok {
			p = 1
		}
		res, err := mod.ExportedFunction(name).Call(ctx, p)
		require.NoError(t, err, name)
		require.Equal(t, []uint64{0x5a5a5a5a}, res, name)
	}
}
