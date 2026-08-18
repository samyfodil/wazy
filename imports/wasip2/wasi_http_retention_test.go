package wasip2

import (
	"context"
	"net/http"
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

// TestOutgoingHandlerReleasesPerRequestState is the client-side counterpart.
//
// These resources are the guest's, not the host's: the guest builds an
// outgoing-request, gets a future back, consumes the incoming-response, reads
// its body, and drops each handle when done. Nothing on the host side owns
// their lifetime, so releasing them is the destructor's job -- and with no
// destructor registered, every outbound request the guest ever made left a
// future, a response and a body behind.
func TestOutgoingHandlerReleasesPerRequestState(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	client := &http.Client{Transport: backendRoundTripper{t: t}}
	inst, err := component.Instantiate(ctx, r, realHTTPOutgoingWasm,
		WithWASI(WASIConfig{EnableHTTP: true, HTTPClient: client})...)
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	defer inst.Close(ctx)

	host := httpHostOf(inst)

	fetch := func(n int) {
		t.Helper()
		for i := 0; i < n; i++ {
			if _, _, _, _, err := serveHTTPCall(inst, ctx, "GET", mustURL("/trigger"), http.Header{}, nil, nil); err != nil {
				t.Fatalf("request %d: %v", i, err)
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
			{"outRequests", len(host.outRequests)},
			{"futures", len(host.futures)},
			{"inResponses", len(host.inResponses)},
			{"inBodies", len(host.inBodies)},
			{"reqOptions", len(host.reqOptions)},
			{"futureTrailers", len(host.futureTrailers)},
		} {
			if m.n != 0 {
				t.Errorf("after %d requests: %s holds %d entries, want 0", after, m.name, m.n)
			}
		}
	}

	fetch(10)
	assertEmpty(10)

	fetch(90)
	assertEmpty(100)
}
