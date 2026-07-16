package instance

// Tests for CompileCache (compilecache.go) and the WithCompileCache option:
// an opt-in cache so repeated Instantiate calls against the SAME component
// skip re-compiling (re-JITting) its embedded core modules. See
// compilecache.go's package doc for the key/ownership/lifetime/Runtime-
// pairing/concurrency contract these tests exercise.

import (
	"bytes"
	"context"
	"sync"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/internal/component/binary"
)

// TestCompileCache_AdderReusesCompiledModule proves a second Instantiate of
// the SAME component bytes, through the same cache, hits the cache instead
// of compiling again: the cache's entry count stays at 1 (real_adder has one
// embedded core module) after two sequential Instantiate+Close cycles.
func TestCompileCache_AdderReusesCompiledModule(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	cache := NewCompileCache()
	defer cache.Close(ctx)

	inst1, err := Instantiate(ctx, r, realAdderWasm, WithCompileCache(cache))
	if err != nil {
		t.Fatalf("first Instantiate: %v", err)
	}
	if err := inst1.Close(ctx); err != nil {
		t.Fatalf("first Close: %v", err)
	}

	cache.mu.Lock()
	n1 := len(cache.byKey)
	cache.mu.Unlock()
	if n1 != 1 {
		t.Fatalf("cache entries after first Instantiate: got %d, want 1", n1)
	}

	inst2, err := Instantiate(ctx, r, realAdderWasm, WithCompileCache(cache))
	if err != nil {
		t.Fatalf("second Instantiate (same cache): %v", err)
	}
	defer inst2.Close(ctx)

	cache.mu.Lock()
	n2 := len(cache.byKey)
	cache.mu.Unlock()
	if n2 != 1 {
		t.Fatalf("cache entries after second Instantiate: got %d, want 1 (should have hit the cache, not grown it)", n2)
	}
}

// TestCompileCache_AdderCorrectAndIndependent proves a cached instantiation
// is functionally identical to an uncached one (add/greet both compute real
// results, not hardcoded/stale ones), and that two sequential instantiations
// sharing one cache produce independent Instances: the second gets its own
// fresh linear memory, unaffected by anything the first wrote into its own
// (the first is fully closed before the second starts, but both reuse the
// SAME underlying compiled core module code -- exactly the scenario
// WithCompileCache exists for).
func TestCompileCache_AdderCorrectAndIndependent(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	cache := NewCompileCache()
	defer cache.Close(ctx)

	inst1, err := Instantiate(ctx, r, realAdderWasm, WithCompileCache(cache))
	if err != nil {
		t.Fatalf("first Instantiate: %v", err)
	}

	results, err := inst1.CallExport(ctx, "component:adder/calc", "add", uint32(2), uint32(3))
	if err != nil {
		t.Fatalf("first instance CallExport add: %v", err)
	}
	if got, ok := results[0].(uint32); !ok || got != 5 {
		t.Fatalf("first instance add(2,3) = %v, want 5", results[0])
	}

	results, err = inst1.CallExport(ctx, "component:adder/calc", "greet", "wazy")
	if err != nil {
		t.Fatalf("first instance CallExport greet: %v", err)
	}
	if got, ok := results[0].(string); !ok || got != "hello, wazy!" {
		t.Fatalf("first instance greet(%q) = %v, want %q", "wazy", results[0], "hello, wazy!")
	}

	if err := inst1.Close(ctx); err != nil {
		t.Fatalf("first Close: %v", err)
	}

	// Second instantiation, same cache (so its core module compile is a
	// cache hit), same Runtime, after the first was fully closed.
	inst2, err := Instantiate(ctx, r, realAdderWasm, WithCompileCache(cache))
	if err != nil {
		t.Fatalf("second Instantiate: %v", err)
	}
	defer inst2.Close(ctx)

	// A different pair of inputs than the first instance used: proves the
	// second instance's guest code really executes fresh (a stale/shared
	// linear-memory bug would either compute the wrong value or reuse the
	// first instance's already-freed memory).
	results, err = inst2.CallExport(ctx, "component:adder/calc", "add", uint32(100), uint32(23))
	if err != nil {
		t.Fatalf("second instance CallExport add: %v", err)
	}
	if got, ok := results[0].(uint32); !ok || got != 123 {
		t.Fatalf("second instance add(100,23) = %v, want 123", results[0])
	}

	results, err = inst2.CallExport(ctx, "component:adder/calc", "greet", "compilecache")
	if err != nil {
		t.Fatalf("second instance CallExport greet: %v", err)
	}
	if got, ok := results[0].(string); !ok || got != "hello, compilecache!" {
		t.Fatalf("second instance greet(%q) = %v, want %q", "compilecache", results[0], "hello, compilecache!")
	}
}

// TestCompileCache_HelloPrintsHelloWorld proves WithCompileCache composes
// correctly with the general multi-core-module graph engine (graph.go) and
// WithWASI: real_hello's 4 embedded core modules are wired up and executed
// through the cache exactly as without one, still printing "hello world"
// through the real WASI 0.2 host surface.
func TestCompileCache_HelloPrintsHelloWorld(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	cache := NewCompileCache()
	defer cache.Close(ctx)

	var stdout, stderr bytes.Buffer
	opts := append([]Option{WithCompileCache(cache)}, WithWASI(WASIConfig{Stdout: &stdout, Stderr: &stderr})...)
	inst, err := Instantiate(ctx, r, realHelloWasm, opts...)
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	defer inst.Close(ctx)

	if _, err := inst.Call(ctx, "wasi:cli/run@0.2.3#run"); err != nil {
		t.Fatalf("Call run(): %v (stdout so far: %q, stderr so far: %q)", err, stdout.String(), stderr.String())
	}
	if got := stdout.String(); got != "hello world\n" {
		t.Fatalf("stdout = %q, want %q (stderr: %q)", got, "hello world\n", stderr.String())
	}

	// real_hello has 4 embedded core modules, but one imports the empty module
	// name and is rewritten to the per-instantiation-unique emptyNameTarget
	// (see rewriteEmptyImportModuleName). That module deliberately bypasses the
	// cache -- its bytes differ every instantiation, so caching them would grow
	// the cache without ever hitting -- leaving exactly 3 cacheable modules.
	cache.mu.Lock()
	n := len(cache.byKey)
	cache.mu.Unlock()
	if n != 3 {
		t.Fatalf("cache entries after Instantiate(real_hello): got %d, want 3 (4 core modules, 1 empty-import-rewritten module bypasses the cache)", n)
	}
}

// TestCompileCache_HelloReinstantiateAfterCloseOnSameRuntime is
// TestRealHello_ReinstantiateAfterCloseOnSameRuntime's WithCompileCache
// counterpart: a full Instantiate+Close+Instantiate+Close cycle on one
// Runtime, sharing one cache, must keep working -- the cache must not hold
// onto anything that would make the second Instantiate collide with state
// the first left behind (e.g. private host module names), and the second
// Instantiate's core-module compiles must all be cache hits.
func TestCompileCache_HelloReinstantiateAfterCloseOnSameRuntime(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	cache := NewCompileCache()
	defer cache.Close(ctx)

	inst1, err := Instantiate(ctx, r, realHelloWasm, WithCompileCache(cache))
	if err != nil {
		t.Fatalf("first Instantiate: %v", err)
	}
	if err := inst1.Close(ctx); err != nil {
		t.Fatalf("first Close: %v", err)
	}

	cache.mu.Lock()
	n1 := len(cache.byKey)
	cache.mu.Unlock()

	inst2, err := Instantiate(ctx, r, realHelloWasm, WithCompileCache(cache))
	if err != nil {
		t.Fatalf("second Instantiate on the same Runtime+cache: %v", err)
	}
	if err := inst2.Close(ctx); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	cache.mu.Lock()
	n2 := len(cache.byKey)
	cache.mu.Unlock()
	if n2 != n1 {
		t.Fatalf("cache entries grew across a repeat Instantiate of the same component: %d -> %d, want unchanged (all hits)", n1, n2)
	}
}

// TestCompileCache_CloseDoesNotCloseThroughInstance proves the cache, not
// Instance.Close, owns the cached CompiledModule: closing an Instance built
// through a cache must not invalidate the cache entry -- a subsequent
// Instantiate reusing the same cache must still succeed (and still be a
// cache hit, i.e. the entry count must not change).
func TestCompileCache_CloseDoesNotCloseThroughInstance(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	cache := NewCompileCache()
	defer cache.Close(ctx)

	inst1, err := Instantiate(ctx, r, realAdderWasm, WithCompileCache(cache))
	if err != nil {
		t.Fatalf("first Instantiate: %v", err)
	}
	if err := inst1.Close(ctx); err != nil {
		t.Fatalf("Instance.Close: %v", err)
	}

	cache.mu.Lock()
	n := len(cache.byKey)
	cache.mu.Unlock()
	if n == 0 {
		t.Fatal("cache is empty after Instance.Close; CompiledModule should be owned by the cache, not the Instance")
	}

	// A follow-up Instantiate through the same (still-populated) cache must
	// still work -- proving the cached CompiledModule really is still valid
	// and usable after the Instance that first triggered its compile closed.
	inst2, err := Instantiate(ctx, r, realAdderWasm, WithCompileCache(cache))
	if err != nil {
		t.Fatalf("second Instantiate after first Instance.Close: %v", err)
	}
	defer inst2.Close(ctx)

	results, err := inst2.CallExport(ctx, "component:adder/calc", "add", uint32(7), uint32(8))
	if err != nil {
		t.Fatalf("CallExport add: %v", err)
	}
	if got, ok := results[0].(uint32); !ok || got != 15 {
		t.Fatalf("add(7,8) = %v, want 15", results[0])
	}
}

// TestCompileCache_Concurrent stresses CompileCache's own concurrency
// contract directly (see compilecache.go's doc): many goroutines racing a
// getOrCompile of the SAME bytes must all succeed, all observe a valid,
// usable CompiledModule, and leave exactly one entry behind -- run under
// `go test -race` to catch any data race over the shared map.
func TestCompileCache_Concurrent(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	cache := NewCompileCache()
	defer cache.Close(ctx)

	comp, err := binary.Decode(bytes.NewReader(realAdderWasm))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(comp.CoreModules) == 0 {
		t.Fatal("real_adder decoded with no embedded core modules")
	}
	coreBytes, err := coreModuleBytes(comp.CoreModules[0], realAdderWasm)
	if err != nil {
		t.Fatalf("coreModuleBytes: %v", err)
	}

	const n = 32
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = cache.getOrCompile(ctx, r, coreBytes)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: getOrCompile: %v", i, err)
		}
	}

	cache.mu.Lock()
	got := len(cache.byKey)
	cache.mu.Unlock()
	if got != 1 {
		t.Fatalf("cache entries after %d concurrent getOrCompile calls on identical bytes: got %d, want 1", n, got)
	}
}

// TestCompileCache_ConcurrentDistinctKeys stresses CompileCache.getOrCompile
// with MULTIPLE distinct byte keys hit concurrently (real_adder's one core
// module plus all 4 of real_hello's), rather than TestCompileCache_Concurrent's
// single-key race -- this exercises the mutex guarding map inserts/lookups
// across different keys, not just the "wait for the winner" branch. Not
// routed through the public Instantiate API: two different components can't
// be live on one Runtime at once today regardless of caching (both default
// to the same synthesized root module name, "wazy:component/core0" -- a
// pre-existing Runtime-naming limitation, not something this cache changes
// or needs to work around), so this goes straight at the cache, which is
// what actually needs the concurrency guarantee. Run under `go test -race`.
func TestCompileCache_ConcurrentDistinctKeys(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	cache := NewCompileCache()
	defer cache.Close(ctx)

	var keys [][]byte
	for _, wasm := range [][]byte{realAdderWasm, realHelloWasm} {
		comp, err := binary.Decode(bytes.NewReader(wasm))
		if err != nil {
			t.Fatalf("Decode: %v", err)
		}
		for _, cm := range comp.CoreModules {
			b, err := coreModuleBytes(cm, wasm)
			if err != nil {
				t.Fatalf("coreModuleBytes: %v", err)
			}
			// One of real_hello's core modules imports under the empty
			// module name wit-component uses for its shared indirect-call
			// table -- wazy's decoder rejects that outright until
			// rewriteEmptyImportModuleName patches it (see graph.go's
			// emptyNameTarget). Irrelevant to what this test is stressing
			// (the cache's own concurrency safety across distinct keys), so
			// just skip anything that doesn't compile as-is rather than
			// reproducing graph.go's rewrite-target-naming logic here.
			if _, err := r.CompileModule(ctx, b); err != nil {
				continue
			}
			keys = append(keys, b)
		}
	}
	if len(keys) < 2 {
		t.Fatalf("expected at least 2 directly-compilable distinct core module byte keys across real_adder+real_hello, got %d", len(keys))
	}

	const roundsPerKey = 8
	var wg sync.WaitGroup
	errs := make(chan error, len(keys)*roundsPerKey)
	for _, key := range keys {
		for i := 0; i < roundsPerKey; i++ {
			wg.Add(1)
			go func(key []byte) {
				defer wg.Done()
				if _, err := cache.getOrCompile(ctx, r, key); err != nil {
					errs <- err
				}
			}(key)
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent getOrCompile: %v", err)
		}
	}

	cache.mu.Lock()
	got := len(cache.byKey)
	cache.mu.Unlock()
	if got != len(keys) {
		t.Fatalf("cache entries after concurrent getOrCompile over %d distinct keys: got %d, want %d", len(keys), got, len(keys))
	}
}
