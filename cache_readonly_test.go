package wazy

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/expctxkeys"
	"github.com/samyfodil/wazy/internal/platform"
	"github.com/samyfodil/wazy/internal/testing/require"
)

// cacheSnapshot records every file under dir with its size and mtime, to
// check that a read-only cache leaves the directory exactly as it was.
func cacheSnapshot(t *testing.T, dir string) map[string]string {
	files := map[string]string{}
	require.NoError(t, filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		files[p] = info.Mode().String()
		// Only file mtimes: Windows updates a directory's lazily, after an
		// earlier removal in it, which isn't something this cache did.
		if !d.IsDir() {
			files[p] += " " + info.ModTime().Format(time.RFC3339Nano) + " " + strconv.FormatInt(info.Size(), 10)
		}
		return nil
	}))
	return files
}

// populatedCache compiles facWasm with a writable cache over a new directory,
// and returns the directory and the path of the one entry it wrote.
func populatedCache(t *testing.T, ctx context.Context) (dir, entry string) {
	if !platform.CompilerSupported() {
		t.Skip("no compiler, so no file cache")
	}
	dir = t.TempDir()
	c, err := NewCompilationCacheWithDir(dir)
	require.NoError(t, err)
	r := NewRuntimeWithConfig(ctx, NewRuntimeConfigCompiler().WithCompilationCache(c))
	_, err = r.CompileModule(ctx, facWasm)
	require.NoError(t, err)
	require.NoError(t, r.Close(ctx))
	require.NoError(t, c.Close(ctx))
	require.NoError(t, filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			entry = p
		}
		return err
	}))
	require.NotEqual(t, "", entry)
	return dir, entry
}

func readOnlyRuntime(t *testing.T, ctx context.Context, dir string, config RuntimeConfig) Runtime {
	c, err := NewCompilationCacheWithDirReadOnly(dir)
	require.NoError(t, err)
	r := NewRuntimeWithConfig(ctx, config.WithCompilationCache(c))
	t.Cleanup(func() {
		require.NoError(t, r.Close(ctx))
		require.NoError(t, c.Close(ctx))
	})
	return r
}

func TestCompilationCacheReadOnly_hit(t *testing.T) {
	ctx := context.Background()
	dir, _ := populatedCache(t, ctx)
	before := cacheSnapshot(t, dir)

	r := readOnlyRuntime(t, ctx, dir, NewRuntimeConfigCompiler())
	compiled, err := r.CompileModule(ctx, facWasm)
	require.NoError(t, err)
	mod, err := r.InstantiateModule(ctx, compiled, NewModuleConfig())
	require.NoError(t, err)
	res, err := mod.ExportedFunction("fac-ssa").Call(ctx, 20)
	require.NoError(t, err)
	require.Equal(t, fac20, res[0])
	// A second compile is an in-memory hit.
	_, err = r.CompileModule(ctx, facWasm)
	require.NoError(t, err)

	require.Equal(t, before, cacheSnapshot(t, dir))
}

func TestCompilationCacheReadOnly_failsWithoutCompiling(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name   string
		damage func(t *testing.T, entry string)
		expErr error
	}{
		{
			name:   "miss",
			damage: func(t *testing.T, entry string) { require.NoError(t, os.Remove(entry)) },
			expErr: ErrCompilationCacheMiss,
		},
		{
			name: "stale",
			damage: func(t *testing.T, entry string) {
				b, err := os.ReadFile(entry)
				require.NoError(t, err)
				// The version follows the 6-byte magic and its 1-byte length.
				b[7] ^= 0xff
				require.NoError(t, os.WriteFile(entry, b, 0o600))
			},
			expErr: ErrCompilationCacheStale,
		},
		{
			name: "corrupt",
			damage: func(t *testing.T, entry string) {
				b, err := os.ReadFile(entry)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(entry, b[:len(b)/2], 0o600))
			},
			expErr: ErrCompilationCacheCorrupt,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir, entry := populatedCache(t, ctx)
			tc.damage(t, entry)
			before := cacheSnapshot(t, dir)

			r := readOnlyRuntime(t, ctx, dir, NewRuntimeConfigCompiler())
			_, err := r.CompileModule(ctx, facWasm)
			require.True(t, errors.Is(err, tc.expErr), "%v", err)
			// A module with listeners takes the other path into the engine,
			// CompileModule rather than HasCompiledModule. Listeners are part
			// of its key, so that is a miss whatever the entry holds.
			_, err = r.CompileModule(withFunctionListener(ctx), facWasm)
			require.True(t, errors.Is(err, ErrCompilationCacheMiss), "%v", err)

			require.Equal(t, before, cacheSnapshot(t, dir))
		})
	}
}

func TestCompilationCacheReadOnly_directoryMustExist(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "absent")
	_, err := NewCompilationCacheWithDirReadOnly(dir)
	require.True(t, errors.Is(err, fs.ErrNotExist), "%v", err)
	_, err = os.Stat(dir)
	require.True(t, errors.Is(err, fs.ErrNotExist), "%v", err)

	// The version-specific directory must exist too.
	dir = t.TempDir()
	_, err = NewCompilationCacheWithDirReadOnly(dir)
	require.True(t, errors.Is(err, fs.ErrNotExist), "%v", err)
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Equal(t, 0, len(entries))
}

func TestCompilationCacheReadOnly_interpreter(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	// Create the version-specific directory, which is all an interpreter
	// runtime would find in it.
	c, err := NewCompilationCacheWithDir(dir)
	require.NoError(t, err)
	require.NoError(t, c.Close(ctx))

	r := readOnlyRuntime(t, ctx, dir, NewRuntimeConfigInterpreter())
	_, err = r.CompileModule(ctx, facWasm)
	require.True(t, errors.Is(err, ErrCompilationCacheUnsupported), "%v", err)

	// Host modules have no code to load, so they still work.
	_, err = r.NewHostModuleBuilder("env").
		NewFunctionBuilder().WithGoFunction(api.GoFunc(func(context.Context, []uint64) {}), nil, nil).Export("f").
		Instantiate(ctx)
	require.NoError(t, err)
}

// withFunctionListener returns ctx with a listener factory that does nothing,
// which makes CompileModule skip HasCompiledModule.
func withFunctionListener(ctx context.Context) context.Context {
	return context.WithValue(ctx, expctxkeys.FunctionListenerFactoryKey{}, api.FunctionListenerFactory(noopListenerFactory{}))
}

type noopListenerFactory struct{}

func (noopListenerFactory) NewFunctionListener(api.FunctionDefinition) api.FunctionListener {
	return nil
}
