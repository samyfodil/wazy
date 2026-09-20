package wazy

import (
	"context"
	"testing"

	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/testing/require"
)

// (module
//
//	(import "host" "block" (func $block))
//	(memory (export "mem") 1)
//	(func (export "run") (call $block) (i32.store (i32.const 0) (i32.const 1))))
var lateCallModule = []byte{
	0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00,
	0x01, 0x04, 0x01, 0x60, 0x00, 0x00,
	0x02, 0x0e, 0x01,
	0x04, 'h', 'o', 's', 't',
	0x05, 'b', 'l', 'o', 'c', 'k',
	0x00, 0x00,
	0x03, 0x02, 0x01, 0x00,
	0x05, 0x03, 0x01, 0x00, 0x01,
	0x07, 0x0d, 0x02,
	0x03, 'm', 'e', 'm', 0x02, 0x00,
	0x03, 'r', 'u', 'n', 0x00, 0x01,
	0x0a, 0x0d, 0x01,
	0x0b, 0x00,
	0x10, 0x00, // call $block
	0x41, 0x00, // i32.const 0
	0x41, 0x01, // i32.const 1
	0x36, 0x02, 0x00, // i32.store
	0x0b,
}

// An api.Function handle taken before a close still worked after it, running
// against a buffer already back in the pool -- so its store landed in whatever
// instance the pool handed that array to next.
//
// Deterministic: the survivor reads the closed module's write, no race
// detector needed.
func TestCallOnClosedModuleDoesNotReachAnotherInstance(t *testing.T) {
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

			_, err := r.NewHostModuleBuilder("host").NewFunctionBuilder().
				WithGoModuleFunction(api.GoModuleFunc(func(context.Context, api.Module, []uint64) {}), nil, nil).
				Export("block").Instantiate(ctx)
			require.NoError(t, err)

			victim, err := r.InstantiateWithConfig(ctx, lateCallModule,
				NewModuleConfig().WithName("victim"))
			require.NoError(t, err)

			saved := victim.ExportedFunction("run") // handle kept across the close
			require.NoError(t, victim.Close(ctx))

			survivor, err := r.InstantiateWithConfig(ctx, lateCallModule,
				NewModuleConfig().WithName("survivor").WithStartFunctions())
			require.NoError(t, err)
			mem := survivor.Memory()
			require.True(t, mem.Write(0, []byte{0xAA, 0xAA, 0xAA, 0xAA}))

			// The call must be refused rather than run on reclaimed storage.
			_, err = saved.Call(ctx)
			require.Error(t, err, "a call on a closed module must fail")

			got, ok := mem.ReadUint32Le(0)
			require.True(t, ok)
			if got != 0xAAAAAAAA {
				t.Fatalf("a call on a CLOSED module wrote into another live instance: got %#x, want 0xAAAAAAAA", got)
			}
		})
	}
}
