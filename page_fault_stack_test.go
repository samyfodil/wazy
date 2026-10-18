package wazy

import (
	"encoding/base64"
	"testing"

	"github.com/samyfodil/wazy/internal/platform"
	"github.com/samyfodil/wazy/internal/testing/require"
)

// TestPageFaultOnWasmStack runs guest code that takes page faults while the
// stack pointer is on the wasm stack: each store lands on a page of linear
// memory nothing has touched yet. OpenBSD checks the stack pointer at every
// page fault and kills the thread with SIGSEGV unless it points into memory
// mapped with MAP_STACK, so a wasm stack that was ordinary heap memory made
// this crash there with "split stack overflow" after sigpanic.
//
//	(module
//	  (memory 512)
//	  (func (export "touch") (local i32)
//	    (loop
//	      (i32.store8 (local.get 0) (i32.const 1))
//	      (local.tee 0 (i32.add (local.get 0) (i32.const 4096)))
//	      (i32.const 33554432)
//	      (i32.lt_u)
//	      (br_if 0))))
func TestPageFaultOnWasmStack(t *testing.T) {
	bin, err := base64.StdEncoding.DecodeString("AGFzbQEAAAABBAFgAAADAgEABQQBAIAEBwkBBXRvdWNoAAAKIAEeAQF/A0AgAEEBOgAAIABBgCBqIgBBgICAEEkNAAsL")
	require.NoError(t, err)
	for _, tc := range []struct {
		name   string
		config func() RuntimeConfig
	}{
		{"compiler", NewRuntimeConfigCompiler},
		{"interpreter", NewRuntimeConfigInterpreter},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "compiler" && !platform.CompilerSupported() {
				t.Skip("compiler not supported here, e.g. executable mmap is unavailable")
			}
			for range 4 {
				r := NewRuntimeWithConfig(testCtx, tc.config())
				mod, err := r.Instantiate(testCtx, bin)
				require.NoError(t, err)
				_, err = mod.ExportedFunction("touch").Call(testCtx)
				require.NoError(t, err)
				require.NoError(t, r.Close(testCtx))
			}
		})
	}
}
