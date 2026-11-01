package adhoc

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/platform"
	"github.com/samyfodil/wazy/internal/testing/binaryencoding"
	"github.com/samyfodil/wazy/internal/testing/require"
	"github.com/samyfodil/wazy/internal/wasm"
)

// TestContextCancel checks that a call ends when its context is done, whatever
// shape keeps it running: the call returns an error that wraps the context's,
// and the module stays usable.
func TestContextCancel(t *testing.T) {
	configs := map[string]func() wazy.RuntimeConfig{"interpreter": wazy.NewRuntimeConfigInterpreter}
	if platform.CompilerSupported() {
		configs["compiler"] = wazy.NewRuntimeConfigCompiler
	}
	shapes := map[string]func() (*wasm.Module, api.CoreFeatures){
		"tight":    loopYieldShapes["tight"],
		"counted":  loopYieldShapes["counted"],
		"calltree": loopYieldShapes["calltree"],
		"tailcall": loopYieldShapes["tailcall"],
		// (loop call $host br 0): returns to Go on every iteration, where Go code
		// can serve a preemption request before compiled code sees it.
		"hostcall": func() (*wasm.Module, api.CoreFeatures) {
			return &wasm.Module{
				TypeSection:     []wasm.FunctionType{{}},
				ImportSection:   []wasm.Import{{Module: "host", Name: "nop", Type: wasm.ExternTypeFunc, DescFunc: 0}},
				FunctionSection: []wasm.Index{0},
				CodeSection: []wasm.Code{{Body: []byte{
					wasm.OpcodeLoop, 0x40, wasm.OpcodeCall, 0, wasm.OpcodeBr, 0, wasm.OpcodeEnd, wasm.OpcodeEnd,
				}}},
				ExportSection: []wasm.Export{{Name: "run", Type: wasm.ExternTypeFunc, Index: 1}},
			}, api.CoreFeaturesV2
		},
	}
	for engine, config := range configs {
		for shape, build := range shapes {
			t.Run(engine+"/"+shape, func(t *testing.T) {
				m, features := build()
				// "ok" returns 42, to show that the module still works after a call ended.
				m.TypeSection = append(m.TypeSection, wasm.FunctionType{Results: []wasm.ValueType{wasm.ValueTypeI32}, ResultNumInUint64: 1})
				m.FunctionSection = append(m.FunctionSection, wasm.Index(len(m.TypeSection)-1))
				m.CodeSection = append(m.CodeSection, wasm.Code{Body: []byte{wasm.OpcodeI32Const, 42, wasm.OpcodeEnd}})
				m.ExportSection = append(m.ExportSection, wasm.Export{
					Name: "ok", Type: wasm.ExternTypeFunc, Index: wasm.Index(len(m.ImportSection) + len(m.CodeSection) - 1),
				})

				ctx := context.Background()
				r := wazy.NewRuntimeWithConfig(ctx, config().WithCoreFeatures(features))
				defer r.Close(ctx)
				_, err := r.NewHostModuleBuilder("host").NewFunctionBuilder().
					WithGoFunction(api.GoFunc(func(context.Context, []uint64) {}), nil, nil).Export("nop").
					Instantiate(ctx)
				require.NoError(t, err)
				mod, err := r.Instantiate(ctx, binaryencoding.EncodeModule(m))
				require.NoError(t, err)

				callCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
				defer cancel()
				start := time.Now()
				_, err = mod.ExportedFunction("run").Call(callCtx)
				took := time.Since(start)
				require.True(t, took < 10*time.Second, "took %v", took)

				require.True(t, errors.Is(err, context.DeadlineExceeded), "got %v", err)
				res, err := mod.ExportedFunction("ok").Call(ctx)
				require.NoError(t, err)
				require.Equal(t, uint64(42), res[0])
			})
		}
	}
}
