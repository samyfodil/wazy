package instance

// Profiling-only benchmarks for the WASI 0.2 component runtime's call path
// (Instantiate + Call/CallExport). See internal/component/instance/{instance.go,
// host_import.go, graph.go} for the wiring these benchmarks exercise, and
// internal/component/abi/{flat.go,memory.go,layout.go} for the data-driven
// ABI (LowerFlat/LiftFlat/Store/Load) underneath Call.
//
// These benchmarks exist to MEASURE, not to optimize: no production code
// changed to support them. real_adder.component.wasm exports
// component:adder/calc with add(u32,u32)->u32 and greet(string)->string;
// real_hello.component.wasm is the heavier multi-core-module wasip2 CLI
// guest (see real_hello_test.go's doc) used to measure the general graph
// engine's instantiation cost.

import (
	"context"
	"testing"

	"github.com/samyfodil/wazy"
)

// BenchmarkInstantiate measures Instantiate(real_adder) end to end: component
// decode, embedded core module compile+instantiate, and the export -> canon
// lift -> core func binding wiring (instance.go's instantiateWithImports,
// since real_adder declares a nested re-export shim -- see needsImportPath).
func BenchmarkInstantiate(b *testing.B) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		inst, err := Instantiate(ctx, r, realAdderWasm)
		if err != nil {
			b.Fatalf("Instantiate: %v", err)
		}
		b.StopTimer()
		if err := inst.Close(ctx); err != nil {
			b.Fatalf("Close: %v", err)
		}
		b.StartTimer()
	}
}

// BenchmarkInstantiateHello measures Instantiate(real_hello): the general
// multi-core-module graph engine (graph.go's instantiateGraph), which wires 4
// embedded core modules through 17 core:instance definitions plus 15
// canon-lower host funcs (trap stubs, since no WASI implementation is
// registered here) -- see real_hello_test.go's doc. This isolates the graph
// engine's extra cost (CompileModule pre-pass to discover needed func types,
// passthrough shim module encoding, etc.) over the simpler adder path.
//
// A fresh Runtime is created (and torn down) outside the timed section on
// every iteration, rather than reusing one Runtime across b.N the way
// BenchmarkInstantiate does. Profiling this benchmark surfaced a real
// resource-registration leak in graph.go's instantiateGraph: each
// canon-produced core func (a lowered-import trap stub or resource.*
// canon, built by buildCanonHostModule) is instantiated as its own private
// host module and registered on the Runtime under a name from
// nextPrivateName ("wazy:component/priv1", "priv2", ...), but that host
// module is never appended to the closers slice Instance.Close walks -- only
// the passthrough shim that imports from it is. So on a real_hello
// instantiation (which has such canons) the private names are never freed,
// and reusing one Runtime across iterations reliably fails the second
// Instantiate call with "module ... has already been instantiated". This is
// a genuine bug worth fixing separately; this benchmark works around it
// (rather than masking it) by paying for a new Runtime every iteration,
// off the clock.
func BenchmarkInstantiateHello(b *testing.B) {
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		r := wazy.NewRuntime(ctx)
		b.StartTimer()

		inst, err := Instantiate(ctx, r, realHelloWasm)
		if err != nil {
			b.Fatalf("Instantiate: %v", err)
		}

		b.StopTimer()
		if err := inst.Close(ctx); err != nil {
			b.Fatalf("Close: %v", err)
		}
		if err := r.Close(ctx); err != nil {
			b.Fatalf("Runtime Close: %v", err)
		}
		b.StartTimer()
	}
}

// BenchmarkCallAdd isolates per-call ABI overhead for the simplest possible
// signature -- add(u32,u32)->u32, no memory involved -- on a single
// pre-instantiated real_adder Instance: lowerParams (abi.LowerFlat x2),
// the core call itself, and liftResult (abi.LiftFlat) for one u32 result.
func BenchmarkCallAdd(b *testing.B) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	inst, err := Instantiate(ctx, r, realAdderWasm)
	if err != nil {
		b.Fatalf("Instantiate: %v", err)
	}
	defer inst.Close(ctx)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		results, err := inst.CallExport(ctx, "component:adder/calc", "add", uint32(2), uint32(3))
		if err != nil {
			b.Fatalf("CallExport add: %v", err)
		}
		if got, ok := results[0].(uint32); !ok || got != 5 {
			b.Fatalf("add(2,3) = %v, want 5", results[0])
		}
	}
}

// BenchmarkCallGreet isolates the allocation-heavy string round-trip path:
// lowering a string parameter into guest memory via cabi_realloc
// (abi.LowerFlat's lowerFlatString -> allocStoreString), the core call, the
// spilled string result's memory load (liftResult's spill branch, since a
// string result flattens to more than MaxFlatResults core values), and the
// canon lift's post-return call (cabi_post_...) that lets the guest free the
// result buffer -- see instance.go's invoke doc.
//
// real_adder's compiled guest allocator cannot actually sustain unbounded
// greet() calls on one Instance: profiling this benchmark found it traps
// ("cabi_realloc: wasm error: unreachable") deterministically on the 8193rd
// call, even though the guest's linear memory size never grows past its
// initial 21 pages the whole time -- i.e. this is the guest's own allocator
// (likely a wee_alloc-style free-list allocator, a common wit-bindgen
// example default) exhausting some internal fixed-capacity bookkeeping, not
// a memory-growth leak and not a wazy-side bug. refreshEvery re-instantiates
// well under that ceiling, off the clock, so this benchmark is safe at any
// -benchtime.
func BenchmarkCallGreet(b *testing.B) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	const refreshEvery = 4096

	newInst := func() *Instance {
		inst, err := Instantiate(ctx, r, realAdderWasm)
		if err != nil {
			b.Fatalf("Instantiate: %v", err)
		}
		return inst
	}

	inst := newInst()
	defer func() { inst.Close(ctx) }()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if i > 0 && i%refreshEvery == 0 {
			b.StopTimer()
			if err := inst.Close(ctx); err != nil {
				b.Fatalf("Close: %v", err)
			}
			inst = newInst()
			b.StartTimer()
		}

		results, err := inst.CallExport(ctx, "component:adder/calc", "greet", "wazy")
		if err != nil {
			b.Fatalf("CallExport greet: %v", err)
		}
		if got, ok := results[0].(string); !ok || got != "hello, wazy!" {
			b.Fatalf("greet(%q) = %v, want %q", "wazy", results[0], "hello, wazy!")
		}
	}
}
