package wazy

import (
	"context"
	"sync"
	"testing"

	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/testing/require"
)

// (module (import "B" "run" (func $run)) (func (export "go") (call $run)))
// Deliberately declares NO memory of its own: that is the point.
var callsBModule = []byte{
	0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00,
	0x01, 0x04, 0x01, 0x60, 0x00, 0x00,
	0x02, 0x09, 0x01,
	0x01, 'B',
	0x03, 'r', 'u', 'n',
	0x00, 0x00,
	0x03, 0x02, 0x01, 0x00,
	0x07, 0x06, 0x01,
	0x02, 'g', 'o', 0x00, 0x01,
	0x0a, 0x06, 0x01,
	0x04, 0x00,
	0x10, 0x00, // call $run
	0x0b,
}

// Only a MEMORY import used to register as a holder of the exporting module's
// memory. Importing a FUNCTION gets you the same reach -- A's call executes B's
// code against B's memory -- with nothing holding it, so closing B while that
// call ran pooled the buffer under it.
//
// Deterministic: the survivor reads B's write, no race detector needed.
func TestCrossModuleCallHoldsTheCalleesMemory(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  RuntimeConfig
	}{
		{"compiler", NewRuntimeConfig()},
		{"interpreter", NewRuntimeConfigInterpreter()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			r := NewRuntimeWithConfig(ctx, tc.cfg)
			defer r.Close(ctx)

			entered, release := make(chan struct{}), make(chan struct{})
			_, err := r.NewHostModuleBuilder("host").NewFunctionBuilder().
				WithGoModuleFunction(api.GoModuleFunc(func(context.Context, api.Module, []uint64) {
					close(entered)
					<-release
				}), nil, nil).
				Export("block").Instantiate(ctx)
			require.NoError(t, err)

			// B owns the memory and does the store.
			b, err := r.InstantiateWithConfig(ctx, lateCallModule,
				NewModuleConfig().WithName("B"))
			require.NoError(t, err)

			// A imports B.run and nothing else.
			a, err := r.InstantiateWithConfig(ctx, callsBModule,
				NewModuleConfig().WithName("A"))
			require.NoError(t, err)

			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, _ = a.ExportedFunction("go").Call(ctx)
			}()

			<-entered
			require.NoError(t, b.Close(ctx)) // B closes while its code runs, via A

			survivor, err := r.InstantiateWithConfig(ctx, lateCallModule,
				NewModuleConfig().WithName("survivor").WithStartFunctions())
			require.NoError(t, err)
			mem := survivor.Memory()
			require.True(t, mem.Write(0, []byte{0xAA, 0xAA, 0xAA, 0xAA}))

			close(release)
			wg.Wait()

			got, ok := mem.ReadUint32Le(0)
			require.True(t, ok)
			if got != 0xAAAAAAAA {
				t.Fatalf("a call reached through a function import wrote into another live instance: got %#x, want 0xAAAAAAAA", got)
			}
		})
	}
}
