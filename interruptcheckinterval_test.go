package wazy

import (
	"context"
	_ "embed"
	"testing"
	"time"

	"github.com/samyfodil/wazy/internal/platform"
	"github.com/samyfodil/wazy/internal/testing/require"
)

//go:embed testdata/interrupt.wasm
var interruptWasm []byte

func TestSetInterruptCheckInterval(t *testing.T) {
	ctx := context.Background()

	// The compiler subtests below force NewRuntimeConfigCompiler; on a GOARCH
	// without a compiler backend (anything but amd64/arm64 -- e.g. a riscv64
	// interpreter run) compiling panics "unsupported architecture" (isa_other.go).
	// Skip them there; the interpreter subtest at the end still runs everywhere.
	requireCompiler := func(t *testing.T) {
		if !platform.CompilerSupported() {
			t.Skip("compiler engine not supported on this GOARCH; interpreter-only run")
		}
	}

	t.Run("retunes and stays cancellable", func(t *testing.T) {
		requireCompiler(t)
		r := NewRuntimeWithConfig(ctx, NewRuntimeConfigCompiler().WithCloseOnContextDone(true))
		defer r.Close(ctx)
		mod, err := r.Instantiate(ctx, interruptWasm)
		require.NoError(t, err)

		fn := mod.ExportedFunction("forever")

		// Retune to a large interval, then confirm the loop still yields and
		// honors a context deadline (the retuned mask is used and correct).
		require.NoError(t, SetInterruptCheckInterval(fn, 4096))

		deadlined, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
		defer cancel()
		_, err = fn.Call(deadlined)
		require.Error(t, err) // interrupted, not an infinite hang

		// interval 1 (check every iteration) is also valid.
		require.NoError(t, SetInterruptCheckInterval(fn, 1))
	})

	t.Run("rejects invalid interval", func(t *testing.T) {
		requireCompiler(t)
		r := NewRuntimeWithConfig(ctx, NewRuntimeConfigCompiler().WithCloseOnContextDone(true))
		defer r.Close(ctx)
		mod, err := r.Instantiate(ctx, interruptWasm)
		require.NoError(t, err)
		// 3 is not 0 or a power of two.
		err = SetInterruptCheckInterval(mod.ExportedFunction("spin"), 3)
		require.Error(t, err)
	})

	t.Run("errors when module not compiled with close-on-context-done", func(t *testing.T) {
		requireCompiler(t)
		r := NewRuntimeWithConfig(ctx, NewRuntimeConfigCompiler())
		defer r.Close(ctx)
		mod, err := r.Instantiate(ctx, interruptWasm)
		require.NoError(t, err)
		// No interrupt-check machinery was emitted, so retuning is unavailable.
		err = SetInterruptCheckInterval(mod.ExportedFunction("spin"), 64)
		require.Error(t, err)
	})

	t.Run("interval survives the file cache", func(t *testing.T) {
		requireCompiler(t)
		// A module reloaded from the on-disk cache must retain its compiled
		// interval (serialized in the cache blob), not reset to 0 — otherwise the
		// setter would wrongly reject it as "not compiled for it".
		dir := t.TempDir()
		{
			cc, err := NewCompilationCacheWithDir(dir)
			require.NoError(t, err)
			r := NewRuntimeWithConfig(ctx, NewRuntimeConfigCompiler().
				WithCompilationCache(cc).WithCloseOnContextDone(true))
			_, err = r.CompileModule(ctx, interruptWasm)
			require.NoError(t, err)
			require.NoError(t, r.Close(ctx))
		}
		// Fresh runtime, same cache dir: this instance is hydrated from disk.
		cc, err := NewCompilationCacheWithDir(dir)
		require.NoError(t, err)
		r := NewRuntimeWithConfig(ctx, NewRuntimeConfigCompiler().
			WithCompilationCache(cc).WithCloseOnContextDone(true))
		defer r.Close(ctx)
		mod, err := r.Instantiate(ctx, interruptWasm)
		require.NoError(t, err)
		// Would error if the reloaded interval were 0 (the bug this guards).
		require.NoError(t, SetInterruptCheckInterval(mod.ExportedFunction("spin"), 4096))
	})

	t.Run("errors on engine without support (interpreter)", func(t *testing.T) {
		r := NewRuntimeWithConfig(ctx, NewRuntimeConfigInterpreter().WithCloseOnContextDone(true))
		defer r.Close(ctx)
		mod, err := r.Instantiate(ctx, interruptWasm)
		require.NoError(t, err)
		err = SetInterruptCheckInterval(mod.ExportedFunction("spin"), 64)
		require.Error(t, err)
	})
}
