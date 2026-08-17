package wasip2

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/component"
)

// TestServeHTTPReleasesPerRequestState pins that serving a request leaves no
// host state behind.
//
// A response-outparam and the outgoing-body taken from the response are
// reachable only through the call that made them, so both must be gone once it
// returns. They were not: an instance accumulated one of each per request it
// ever served, which on a long-lived server is an unbounded leak rather than a
// performance detail. Counting after two different numbers of requests is what
// distinguishes "released" from "grows with load".
func TestServeHTTPReleasesPerRequestState(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	inst, err := component.Instantiate(ctx, r, realHTTPIncomingWasm, WithWASI(WASIConfig{EnableHTTP: true})...)
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	defer inst.Close(ctx)

	h := Handler(inst)
	host := httpHostOf(inst)

	serve := func(n int) {
		t.Helper()
		for i := 0; i < n; i++ {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("GET", "/greet?name=wazy", nil))
			if rec.Code != 200 {
				t.Fatalf("request %d: status = %d, want 200", i, rec.Code)
			}
		}
	}

	assertEmpty := func(after int) {
		t.Helper()
		host.mu.Lock()
		defer host.mu.Unlock()
		for _, m := range []struct {
			name string
			n    int
		}{
			{"outparams", len(host.outparams)},
			{"bodies", len(host.bodies)},
			{"bodyStreams", len(host.bodyStreams)},
			{"responses", len(host.responses)},
			{"fields", len(host.fields)},
			{"incoming", len(host.incoming)},
		} {
			if m.n != 0 {
				t.Errorf("after %d requests: %s holds %d entries, want 0", after, m.name, m.n)
			}
		}
	}

	serve(20)
	assertEmpty(20)

	// Ten times the load must not leave ten times the state, which is the
	// difference between "released" and "released late".
	serve(200)
	assertEmpty(220)
}
