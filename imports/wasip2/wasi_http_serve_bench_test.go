package wasip2

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/component"
)

// BenchmarkServeHTTP measures one request end to end through
// wasi:http/incoming-handler, which is the highest-frequency path a real
// embedder runs and the one carrying the most complicated WIT types: records
// holding options holding variants holding resources.
//
// It exists for allocs/op rather than ns/op. Per-request heap traffic is what
// makes the GC the throughput limiter here -- a trivial component call barely
// flattens anything and runs orders of magnitude faster -- so this is the guard
// that keeps the canonical ABI from quietly going back to deriving a type's
// flattened layout on every call. Read the allocs column first; ns/op on a
// shared machine is noise.
func BenchmarkServeHTTP(b *testing.B) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	inst, err := component.Instantiate(ctx, r, realHTTPIncomingWasm, WithWASI(WASIConfig{EnableHTTP: true})...)
	if err != nil {
		b.Fatalf("Instantiate: %v", err)
	}
	defer inst.Close(ctx)

	h := Handler(inst)
	req := httptest.NewRequest("GET", "/greet?name=wazy", nil)
	rec := httptest.NewRecorder()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rec.Body.Reset()
		h.ServeHTTP(rec, req)
	}
	b.StopTimer()

	// A benchmark measuring a request that failed would measure nothing.
	if rec.Code != 200 {
		b.Fatalf("status = %d, want 200", rec.Code)
	}
}
