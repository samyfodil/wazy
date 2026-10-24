package wazy

import (
	"sync"
	"testing"

	"github.com/samyfodil/wazy/internal/platform"
	"github.com/samyfodil/wazy/internal/testing/require"
)

// TestCompilationCache_concurrentCompileClose is the reproducer from
// https://github.com/tetratelabs/wazero/issues/2459: runtimes sharing a
// compilation cache compile the same module at once, and each closes its
// CompiledModule as soon as it is done. If two concurrent cache misses each
// stored the module with a count of one, the first Close dropped the entry and
// a runtime between compiling and instantiating failed with "source module
// must be compiled before instantiation".
func TestCompilationCache_concurrentCompileClose(t *testing.T) {
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
			cache := NewCompilationCache()
			defer cache.Close(testCtx)
			bin := []byte{0, 'a', 's', 'm', 1, 0, 0, 0}

			sem := make(chan struct{}, 4)
			errs := make(chan error, 1)
			var wg sync.WaitGroup
			for i := 0; i < 400; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					sem <- struct{}{}
					defer func() { <-sem }()
					r := NewRuntimeWithConfig(testCtx, tc.config().WithCompilationCache(cache))
					defer r.Close(testCtx)
					compiled, err := r.CompileModule(testCtx, bin)
					if err == nil {
						defer compiled.Close(testCtx)
						_, err = r.InstantiateModule(testCtx, compiled, NewModuleConfig().WithName(""))
					}
					if err != nil {
						select {
						case errs <- err:
						default:
						}
					}
				}()
			}
			wg.Wait()
			close(errs)
			require.NoError(t, <-errs)
		})
	}
}
