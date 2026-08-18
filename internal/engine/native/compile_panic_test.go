package native

import (
	"runtime"
	"strings"
	"testing"

	"github.com/samyfodil/wazy/internal/testing/require"
	"github.com/samyfodil/wazy/internal/wasm"
)

// The compiler asserts with panics, and CompileModule turns them into errors so
// a shape wazy cannot lower rejects one module instead of killing the process
// compiling it. See compilePanicError.
func TestCompilePanicError(t *testing.T) {
	mod := &wasm.Module{NameSection: &wasm.NameSection{ModuleName: "greet"}}

	t.Run("names the module and keeps the stack", func(t *testing.T) {
		err := compilePanicError(mod, "TODO: lowering Vfoo")
		require.Error(t, err)
		require.True(t, strings.Contains(err.Error(), `"greet"`), err.Error())
		require.True(t, strings.Contains(err.Error(), "TODO: lowering Vfoo"), err.Error())
		// The stack is what makes a wazy bug debuggable from a user's report.
		require.True(t, strings.Contains(err.Error(), "compilePanicError"), err.Error())
	})

	t.Run("survives a module with no name section", func(t *testing.T) {
		require.Error(t, compilePanicError(&wasm.Module{}, "boom"))
	})

	t.Run("re-raises a runtime error", func(t *testing.T) {
		var recovered interface{}
		func() {
			defer func() { recovered = recover() }()
			var s []int
			defer func() {
				// A nil-map write, index out of range and the like mean the
				// compiler's own state is wrong, so they must not be reduced to
				// "this module was rejected".
				_ = compilePanicError(mod, recover())
			}()
			_ = s[1]
		}()
		_, ok := recovered.(runtime.Error)
		require.True(t, ok, "expected the runtime error to be re-raised, got %v", recovered)
	})
}
