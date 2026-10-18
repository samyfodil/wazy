package wazy

import (
	"context"
	"sync"
	"testing"

	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/testing/binaryencoding"
	"github.com/samyfodil/wazy/internal/testing/require"
	"github.com/samyfodil/wazy/internal/wasm"
)

// lazyTestModule exports a recursive fib and a function that reaches another
// only through a tail call, and holds a function nothing calls.
func lazyTestModule() []byte {
	i32 := wasm.ValueTypeI32
	return binaryencoding.EncodeModule(&wasm.Module{
		TypeSection:     []wasm.FunctionType{{Params: []wasm.ValueType{i32}, Results: []wasm.ValueType{i32}}},
		FunctionSection: []wasm.Index{0, 0, 0, 0},
		CodeSection: []wasm.Code{
			{Body: []byte{ // fib
				0x20, 0, 0x41, 2, 0x48, 0x04, 0x7f,
				0x20, 0,
				0x05,
				0x20, 0, 0x41, 1, 0x6b, 0x10, 0,
				0x20, 0, 0x41, 2, 0x6b, 0x10, 0,
				0x6a,
				0x0b, 0x0b,
			}},
			{Body: []byte{0x20, 0, 0x12, 2, 0x0b}},       // return_call 2
			{Body: []byte{0x20, 0, 0x41, 1, 0x6a, 0x0b}}, // n + 1
			{Body: []byte{0x00, 0x0b}},                   // never called
		},
		ExportSection: []wasm.Export{
			{Type: wasm.ExternTypeFunc, Name: "fib", Index: 0},
			{Type: wasm.ExternTypeFunc, Name: "tail", Index: 1},
		},
	})
}

func newLazyRuntime(t *testing.T) (context.Context, Runtime) {
	ctx := api.WithInterpreterLazyLowering(context.Background())
	r := NewRuntimeWithConfig(ctx, NewRuntimeConfigInterpreter().WithCoreFeatures(api.CoreFeaturesV3))
	t.Cleanup(func() { r.Close(ctx) })
	return ctx, r
}

// TestInterpreterLazyLowering_Concurrent has many goroutines make the first
// call to the same unlowered functions at once. Run with -race.
func TestInterpreterLazyLowering_Concurrent(t *testing.T) {
	ctx, r := newLazyRuntime(t)
	mod, err := r.Instantiate(ctx, lazyTestModule())
	require.NoError(t, err)

	const goroutines = 32
	var start, done sync.WaitGroup
	start.Add(1)
	errs := make(chan string, 2*goroutines)
	for i := range goroutines {
		done.Add(1)
		go func() {
			defer done.Done()
			fib, tail := mod.ExportedFunction("fib"), mod.ExportedFunction("tail")
			start.Wait()
			// Half start with each function, so different functions are
			// lowered at the same time through the shared compiler.
			for j := range 2 {
				if (i+j)%2 == 0 {
					if res, err := fib.Call(ctx, 15); err != nil || res[0] != 610 {
						errs <- "fib"
					}
				} else if res, err := tail.Call(ctx, 41); err != nil || res[0] != 42 {
					errs <- "tail"
				}
			}
		}()
	}
	start.Done()
	done.Wait()
	close(errs)
	for name := range errs {
		t.Fatalf("%s: wrong result or error", name)
	}
}

// TestInterpreterLazyLowering_InvalidFailsAtCompile checks that lazy lowering
// does not defer a validation error to the first call.
func TestInterpreterLazyLowering_InvalidFailsAtCompile(t *testing.T) {
	ctx, r := newLazyRuntime(t)
	bin := binaryencoding.EncodeModule(&wasm.Module{
		TypeSection:     []wasm.FunctionType{{Results: []wasm.ValueType{wasm.ValueTypeI32}}},
		FunctionSection: []wasm.Index{0},
		CodeSection:     []wasm.Code{{Body: []byte{0x42, 1, 0x0b}}}, // i64.const where i32 is due
	})
	_, err := r.CompileModule(ctx, bin)
	require.Error(t, err)
}
