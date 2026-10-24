package wazy

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/samyfodil/wazy/internal/platform"
	"github.com/samyfodil/wazy/internal/testing/require"
)

// warmCacheRuntime fills a cache directory with facWasm's native code, then
// returns a runtime on a fresh cache over that directory, so that compiling
// facWasm in it is a file cache hit: where the platform allows, its code is
// mapped from the cache file rather than copied.
func warmCacheRuntime(t *testing.T, ctx context.Context) (Runtime, string) {
	if !platform.CompilerSupported() {
		t.Skip("no compiler, so no file cache")
	}
	dir := t.TempDir()
	cold, err := NewCompilationCacheWithDir(dir)
	require.NoError(t, err)
	r := NewRuntimeWithConfig(ctx, NewRuntimeConfigCompiler().WithCompilationCache(cold))
	_, err = r.CompileModule(ctx, facWasm)
	require.NoError(t, err)
	require.NoError(t, r.Close(ctx))
	require.NoError(t, cold.Close(ctx))

	warm, err := NewCompilationCacheWithDir(dir)
	require.NoError(t, err)
	r = NewRuntimeWithConfig(ctx, NewRuntimeConfigCompiler().WithCompilationCache(warm))
	t.Cleanup(func() {
		require.NoError(t, r.Close(ctx))
		require.NoError(t, warm.Close(ctx))
	})
	return r, dir
}

const fac20 = uint64(2432902008176640000)

// Code mapped from a cache file stays mapped for as long as anything can run
// it: closing the CompiledModule while calls are in flight, and replacing or
// removing the cache entry under it, must not unmap it.
func TestCompilationCache_mappedCodeOutlivesCompiledModule(t *testing.T) {
	ctx := context.Background()
	r, dir := warmCacheRuntime(t, ctx)

	compiled, err := r.CompileModule(ctx, facWasm)
	require.NoError(t, err)
	mod, err := r.InstantiateModule(ctx, compiled, NewModuleConfig())
	require.NoError(t, err)
	fac := mod.ExportedFunction("fac-ssa")
	// Check the code really is mapped, where the directory allows mapping
	// code at all: a noexec /tmp, as in some CI containers, falls back to the copy.
	if goruntime.GOOS == "linux" && canMapCode(t, dir) {
		maps, err := os.ReadFile("/proc/self/maps")
		require.NoError(t, err)
		require.True(t, strings.Contains(string(maps), dir), "code is not mapped from %s", dir)
	}

	var stop atomic.Bool
	var calls atomic.Int64
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		// A Function is not safe for concurrent use, so each caller has its own.
		fac := mod.ExportedFunction("fac-ssa")
		go func() {
			defer wg.Done()
			for !stop.Load() {
				res, err := fac.Call(ctx, 20)
				if err != nil || res[0] != fac20 {
					t.Errorf("fac-ssa(20) = %v, %v", res, err)
					return
				}
				calls.Add(1)
			}
		}()
	}
	waitCalls := func() {
		for n := calls.Load() + 100; calls.Load() < n; {
			goruntime.Gosched()
		}
	}
	waitCalls()

	// Drop the engine's reference to the code.
	require.NoError(t, compiled.Close(ctx))
	goruntime.GC()
	waitCalls()

	// Replace every entry the way the file cache does, by renaming a new file
	// over it, then remove them.
	{
		var entries []string
		require.NoError(t, filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				entries = append(entries, p)
			}
			return err
		}))
		require.NotEqual(t, 0, len(entries))
		for _, p := range entries {
			require.NoError(t, os.WriteFile(p+".tmp", []byte("garbage"), 0o600))
			require.NoError(t, os.Rename(p+".tmp", p))
			require.NoError(t, os.Remove(p))
		}
	}
	goruntime.GC()
	goruntime.GC()
	waitCalls()

	stop.Store(true)
	wg.Wait()

	// The instance still runs once nothing else is running it.
	res, err := fac.Call(ctx, 20)
	require.NoError(t, err)
	require.Equal(t, fac20, res[0])
	require.NoError(t, mod.Close(ctx))
	// Nothing references the code now; collecting it releases the mapping.
	goruntime.GC()
	goruntime.GC()
}

// Two CompiledModules of the same binary share one compiled module, mapped
// once: closing one leaves the other able to instantiate and run it.
func TestCompilationCache_mappedCodeRefCount(t *testing.T) {
	ctx := context.Background()
	r, _ := warmCacheRuntime(t, ctx)

	a, err := r.CompileModule(ctx, facWasm)
	require.NoError(t, err)
	b, err := r.CompileModule(ctx, facWasm)
	require.NoError(t, err)
	require.NoError(t, a.Close(ctx))
	goruntime.GC()
	goruntime.GC()

	call := func(c CompiledModule) {
		mod, err := r.InstantiateModule(ctx, c, NewModuleConfig().WithName(""))
		require.NoError(t, err)
		res, err := mod.ExportedFunction("fac-ssa").Call(ctx, 20)
		require.NoError(t, err)
		require.Equal(t, fac20, res[0])
		require.NoError(t, mod.Close(ctx))
	}
	call(b)
	require.NoError(t, b.Close(ctx))
	goruntime.GC()
	goruntime.GC()

	// A third compile after both are closed maps the entry again.
	c, err := r.CompileModule(ctx, facWasm)
	require.NoError(t, err)
	call(c)
	require.NoError(t, c.Close(ctx))
}

// The compiler releases function bodies once it has native code, whether it
// compiled the module or found it in memory or in a file cache.
func TestCompileModule_releasesFunctionBodies(t *testing.T) {
	ctx := context.Background()
	r, _ := warmCacheRuntime(t, ctx)

	requireReleased := func(c CompiledModule) {
		for i, code := range c.(*compiledModule).module.CodeSection {
			require.Nil(t, code.Body, i)
		}
	}
	fromFile, err := r.CompileModule(ctx, facWasm)
	require.NoError(t, err)
	requireReleased(fromFile)
	fromMemory, err := r.CompileModule(ctx, facWasm)
	require.NoError(t, err)
	requireReleased(fromMemory)

	fresh := NewRuntimeWithConfig(ctx, NewRuntimeConfigCompiler())
	defer fresh.Close(ctx)
	compiled, err := fresh.CompileModule(ctx, facWasm)
	require.NoError(t, err)
	requireReleased(compiled)
	// And the module still runs.
	mod, err := fresh.InstantiateModule(ctx, compiled, NewModuleConfig())
	require.NoError(t, err)
	res, err := mod.ExportedFunction("fac-ssa").Call(ctx, 20)
	require.NoError(t, err)
	require.Equal(t, fac20, res[0])

	// The interpreter still reads them, so keeps them.
	interp := NewRuntimeWithConfig(ctx, NewRuntimeConfigInterpreter())
	defer interp.Close(ctx)
	compiled, err = interp.CompileModule(ctx, facWasm)
	require.NoError(t, err)
	require.NotNil(t, compiled.(*compiledModule).module.CodeSection[0].Body)
}

// canMapCode reports whether a file in dir can be mapped executable.
func canMapCode(t *testing.T, dir string) bool {
	p := filepath.Join(dir, "probe")
	require.NoError(t, os.WriteFile(p, []byte{0}, 0o600))
	defer os.Remove(p)
	f, err := os.Open(p)
	require.NoError(t, err)
	defer f.Close()
	code, err := platform.MapCodeSegmentFromFile(f, 0, 1)
	if err != nil {
		t.Logf("cannot map code from %s: %v", dir, err)
		return false
	}
	require.NoError(t, platform.MunmapCodeSegment(code))
	return true
}
