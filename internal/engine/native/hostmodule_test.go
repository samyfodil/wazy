package native

import (
	"context"
	"testing"
	"unsafe"

	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/testing/require"
	"github.com/samyfodil/wazy/internal/wasm"
)

func Test_writeGoFunc_hostModuleGoFuncFromOpaque(t *testing.T) {
	// buf mimics an opaque buffer: 32 bytes of header followed by three
	// 16-byte GoFunc slots. Writing/reading at a non-zero index exercises
	// hostModuleGoFuncFromOpaque's offset math (index*16 + 32) rather than
	// only landing on the first slot.
	const index = 2
	buf := make([]byte, 32+16*(index+1))
	opaque := uintptr(unsafe.Pointer(&buf[0]))
	slot := buf[32+16*index:]

	// Both interface kinds must round-trip: writeGoFunc stores the matched
	// interface's own two words and hostModuleGoFuncFromOpaque[T] reinterprets
	// them as T, so the T at read time must be the same interface kind that was
	// written. Read back with the wrong kind would hand native code a value
	// with a mismatched method set / Call signature.
	t.Run("GoFunction", func(t *testing.T) {
		var called bool
		var goFn api.GoFunction = api.GoFunc(func(context.Context, []uint64) {
			called = true
		})
		writeGoFunc(goFn, slot)
		got := hostModuleGoFuncFromOpaque[api.GoFunction](index, opaque)
		got.Call(context.Background(), nil)
		require.True(t, called)
	})

	t.Run("GoModuleFunction", func(t *testing.T) {
		var called bool
		var goFn api.GoModuleFunction = api.GoModuleFunc(func(context.Context, api.Module, []uint64) {
			called = true
		})
		writeGoFunc(goFn, slot)
		got := hostModuleGoFuncFromOpaque[api.GoModuleFunction](index, opaque)
		got.Call(context.Background(), nil, nil)
		require.True(t, called)
	})
}

func Test_writeGoFunc_panicsOnUnexpectedType(t *testing.T) {
	// Defense-in-depth at the unsafe boundary: NewHostModule already rejects a
	// GoFunc that is neither interface kind, but writeGoFunc must never store
	// two arbitrary words that hostModuleGoFuncFromOpaque would later hand to
	// native code as a bogus interface value.
	defer func() {
		require.NotNil(t, recover(), "writeGoFunc must panic on a non-GoFunc value")
	}()
	writeGoFunc("not a go func", make([]byte, 16))
}

func Test_buildHostModuleOpaque(t *testing.T) {
	// Interleave both interface kinds so each is read back at a non-zero index,
	// and so a slot of one kind sits between slots of the other.
	goFn := func() api.GoFunction { return api.GoFunc(func(context.Context, []uint64) {}) }
	goModuleFn := func() api.GoModuleFunction {
		return api.GoModuleFunc(func(context.Context, api.Module, []uint64) {})
	}
	for _, tc := range []struct {
		name      string
		m         *wasm.Module
		listeners []api.FunctionListener
	}{
		{
			name: "no listeners",
			m: &wasm.Module{
				CodeSection: []wasm.Code{
					{GoFunc: goFn()},
					{GoFunc: goModuleFn()},
				},
			},
		},
		{
			name: "listeners",
			m: &wasm.Module{
				CodeSection: []wasm.Code{
					{GoFunc: goModuleFn()},
					{GoFunc: goFn()},
					{GoFunc: goModuleFn()},
					{GoFunc: goFn()},
				},
			},
			listeners: make([]api.FunctionListener, 50),
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := buildHostModuleOpaque(tc.m, tc.listeners)
			opaque := uintptr(unsafe.Pointer(&got[0]))
			require.Equal(t, tc.m, hostModuleFromOpaque(opaque))
			if len(tc.listeners) > 0 {
				require.Equal(t, tc.listeners, hostModuleListenersSliceFromOpaque(opaque))
			}
			// Each slot must read back as the same interface kind it was stored
			// under: the read T is selected per-entry by the same type switch
			// writeGoFunc used (and that engine.go's compileHostModule uses to
			// pick the exit code), so the two must stay in lockstep here.
			for i, c := range tc.m.CodeSection {
				switch fn := c.GoFunc.(type) {
				case api.GoModuleFunction:
					require.Equal(t, fn, hostModuleGoFuncFromOpaque[api.GoModuleFunction](i, opaque))
				case api.GoFunction:
					require.Equal(t, fn, hostModuleGoFuncFromOpaque[api.GoFunction](i, opaque))
				default:
					t.Fatalf("unexpected GoFunc type %T at index %d", c.GoFunc, i)
				}
			}
		})
	}
}
