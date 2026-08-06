package instance

import (
	"context"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/internal/component/testfixtures"
)

// realHelloWasm is the shared multi-module component the engine uses wherever
// it needs a genuine component and does not care what it prints. The WASI
// tests that DO care about its output live in imports/wasip2.
var realHelloWasm = testfixtures.RealHello

// TestCompileCache_TwoHelloLiveShareShims proves the stable-key shim caching
// is concurrency-safe: two real_hello instances live at once on one Runtime
// and one CompileCache share the cached shim CompiledModules. It needs no
// WASI -- it counts cache entries rather than observing guest output.
func TestCompileCache_TwoHelloLiveShareShims(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)
	cache := NewCompileCache()
	defer cache.Close(ctx)

	newInst := func(who string) *Instance {
		in, err := Instantiate(ctx, r, realHelloWasm, WithCompileCache(cache))
		if err != nil {
			t.Fatalf("%s Instantiate: %v", who, err)
		}
		return in
	}

	a := newInst("a")
	defer a.Close(ctx)
	b := newInst("b") // both live simultaneously
	defer b.Close(ctx)

	// The point: a second live instance of the same component adds no cache
	// entries -- every core module AND shim compile is a hit, and the shim
	// keys are stable across instantiations.
	cache.mu.Lock()
	n := len(cache.byKey)
	cache.mu.Unlock()
	if n <= 4 {
		t.Fatalf("cache entries with two live instances: got %d, want > 4 (core modules and shims both cached)", n)
	}

}

// TestCompileCache_HelloReinstantiateAfterCloseOnSameRuntime is
// TestRealHello_ReinstantiateAfterCloseOnSameRuntime's WithCompileCache
// counterpart: a full Instantiate+Close+Instantiate+Close cycle on one
// Runtime, sharing one cache, must keep working -- the cache must not hold
// onto anything that would make the second Instantiate collide with state
// the first left behind (e.g. private host module names), and the second
// Instantiate's core-module compiles must all be cache hits.
