package wasip2

import (
	"bytes"
	"context"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/component"
	"github.com/samyfodil/wazy/internal/component/testfixtures"
)

// This lives here rather than with the compile cache because it needs a WASI
// host to observe what the cached component actually printed. The cache's own
// behavior -- decode reuse, entry counts, shim stability -- is tested in
// internal/component/instance, without WASI.

var cacheHelloWasm = testfixtures.RealHello

func TestCompileCache_HelloPrintsHelloWorld(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	cache := component.NewCompileCache()
	defer cache.Close(ctx)

	var stdout, stderr bytes.Buffer
	opts := append([]component.Option{component.WithCompileCache(cache)}, WithWASI(WASIConfig{Stdout: &stdout, Stderr: &stderr})...)
	inst, err := component.Instantiate(ctx, r, cacheHelloWasm, opts...)
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

	// real_hello caches its 4 embedded core modules AND its ~13 regrouping shims
	// (their FromModule refs are all component-constant now -- embedded-module keys
	// plus the stable canon-group keys -- so shimBytes are identical every
	// instantiation; see instantiateGraph). The exact count is an implementation
	// detail; what matters is that a SECOND Instantiate of the same component on
	// the same cache adds NOTHING -- every core-module AND shim compile is a hit.

	inst2, err := component.Instantiate(ctx, r, cacheHelloWasm, opts...)
	if err != nil {
		t.Fatalf("second Instantiate: %v", err)
	}
	defer inst2.Close(ctx)
}

// TestCompileCache_TwoHelloLiveShareShims proves the item-6 stable-key shim
// caching is concurrency-safe. Two real_hello instances are LIVE at once on one
// Runtime and one CompileCache: they share the cached shim CompiledModules
// (bytes are stable), yet each shim must resolve ITS OWN merged canon host
// module -- both register under the SAME component-constant canon-group key but
// in SEPARATE per-instance resolver maps (keyToInst). The host modules keep
// per-instantiation-unique global names, so nothing collides in the store. Both
// instances must independently print "hello world".
// TestCompileCache_ShimBytesStableAcrossInstantiations pins the CM10 invariant
// that core-instance identity keys (wazy:core-instance/<static index>, the
// resolver key every passthrough shim names its alias sources by) must not
// break: they are COMPONENT-CONSTANT, so a second Instantiate of the same
// component produces byte-identical shim bytes and adds nothing to the compile
// cache. A per-instantiation key (a global module name, say) would grow byKey
// by one entry per shim on every Instantiate.
