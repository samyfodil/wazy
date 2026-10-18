package wazy

import (
	"encoding/base64"
	"testing"

	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/testing/require"
)

// TestModule_ExnRefClosed pins what makes a module one whose calls may put off registering with the collector:
// each case but the first adds exactly one way for its frames to hold an exnref it did not allocate.
func TestModule_ExnRefClosed(t *testing.T) {
	for _, tc := range []struct {
		name, wat, wasm string
		closed          bool
	}{
		{
			name:   "throw and catch by value",
			wat:    `(tag $t (param i32)) (func (export "f") (result i32) (block $h (result i32) (try_table (catch $t $h) (throw $t (i32.const 7))) (i32.const 0)))`,
			wasm:   "AGFzbQEAAAABCQJgAX8AYAABfwMCAQENAwEAAAcFAQFmAAAKFAESAAJ/H0ABAAAAQQcIAAtBAAsLABMEbmFtZQMGAQABAAFoCwQBAAF0",
			closed: true,
		},
		{
			name: "catch_all_ref",
			wat:  `(tag $t) (func (export "f") (block $h (result exnref) (try_table (catch_all_ref $h) (throw $t)) (return)) (drop))`,
			wasm: "AGFzbQEAAAABBAFgAAADAgEADQMBAAAHBQEBZgAAChEBDwACaR9AAQMACAALDwsaCwATBG5hbWUDBgEAAQABaAsEAQABdA==",
		},
		{
			name: "catch_ref",
			wat:  `(tag $t) (func (export "f") (block $h (result exnref) (try_table (catch_ref $t $h) (throw $t)) (return)) (drop))`,
			wasm: "AGFzbQEAAAABBAFgAAADAgEADQMBAAAHBQEBZgAAChIBEAACaR9AAQEAAAgACw8LGgsAEwRuYW1lAwYBAAEAAWgLBAEAAXQ=",
		},
		{
			name: "call_indirect",
			wat:  `(type $v (func)) (table 1 funcref) (func (export "f") (call_indirect (type $v) (i32.const 0)))`,
			wasm: "AGFzbQEAAAABBAFgAAADAgEABAQBcAABBwUBAWYAAAoJAQcAQQARAAALAAsEbmFtZQQEAQABdg==",
		},
		{
			name: "call_ref",
			wat:  `(type $v (func)) (func $g) (elem declare func $g) (func (export "f") (call_ref $v (ref.func $g)))`,
			wasm: "AGFzbQEAAAABBAFgAAADAwIAAAcFAQFmAAEJBQEDAAEACgsCAgALBgDSABQACwARBG5hbWUBBAEAAWcEBAEAAXY=",
		},
		{
			name: "exnref global",
			wat:  `(global (mut exnref) (ref.null exn))`,
			wasm: "AGFzbQEAAAAGBgFpAdBpCw==",
		},
		{
			name: "exnref local",
			wat:  `(func (local exnref))`,
			wasm: "AGFzbQEAAAABBAFgAAADAgEACgYBBAEBaQs=",
		},
		{
			name: "exnref param",
			wat:  `(func (param exnref))`,
			wasm: "AGFzbQEAAAABBQFgAWkAAwIBAAoEAQIACw==",
		},
		{
			name: "exnref table",
			wat:  `(table 1 exnref)`,
			wasm: "AGFzbQEAAAAEBAFpAAE=",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin, err := base64.StdEncoding.DecodeString(tc.wasm)
			require.NoError(t, err)
			r := NewRuntimeWithConfig(testCtx, NewRuntimeConfigInterpreter().WithCoreFeatures(api.CoreFeaturesV3))
			defer r.Close(testCtx)
			compiled, err := r.CompileModule(testCtx, bin)
			require.NoError(t, err, tc.wat)
			require.Equal(t, tc.closed, compiled.(*compiledModule).module.ExnRefClosed, tc.wat)
		})
	}
}
