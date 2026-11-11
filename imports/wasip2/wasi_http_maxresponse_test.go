package wasip2

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/component"
)

// bodyStreamWrite enforces the cap exactly: a write that reaches it is accepted, the
// first byte past it is refused, and a refused write leaves the buffer untouched.
func TestBodyStreamWrite_MaxResponseBytes(t *testing.T) {
	h := newWasiHTTP()
	h.maxResponseBytes = 10
	b := &httpOutgoingBody{}
	h.newBodyRep(b)
	rep := h.newBodyStreamRep(b)

	for _, step := range []struct {
		write   string
		wantErr bool
		wantLen int
	}{
		{"hello!", false, 6},
		{"1234", false, 10}, // exactly at the cap
		{"x", true, 10},     // one byte past it
		{"", false, 10},     // an empty write is fine
	} {
		found, err := h.bodyStreamWrite(rep, []byte(step.write))
		if !found {
			t.Fatalf("write %q: body stream not found", step.write)
		}
		if (err != nil) != step.wantErr {
			t.Fatalf("write %q: err = %v, want error: %v", step.write, err, step.wantErr)
		}
		if b.buf.Len() != step.wantLen {
			t.Fatalf("write %q: buffered %d bytes, want %d", step.write, b.buf.Len(), step.wantLen)
		}
	}

	// A single write larger than the cap is refused whole.
	b2 := &httpOutgoingBody{}
	h.newBodyRep(b2)
	rep2 := h.newBodyStreamRep(b2)
	if _, err := h.bodyStreamWrite(rep2, make([]byte, 11)); err == nil {
		t.Fatal("an 11 byte write under a 10 byte cap was accepted")
	}
	if b2.buf.Len() != 0 {
		t.Fatalf("refused write left %d bytes buffered", b2.buf.Len())
	}
}

// With no cap configured nothing changes.
func TestBodyStreamWrite_NoCapByDefault(t *testing.T) {
	h := newWasiHTTP()
	b := &httpOutgoingBody{}
	h.newBodyRep(b)
	rep := h.newBodyStreamRep(b)
	if _, err := h.bodyStreamWrite(rep, make([]byte, 1<<20)); err != nil {
		t.Fatalf("unlimited body refused a write: %v", err)
	}
}

// WASIConfig.MaxResponseBytes reaches the host: the same guest answers 200 with room and 500
// when the cap is smaller than its response.
func TestRealHTTP_MaxResponseBytes(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	for _, tc := range []struct {
		name  string
		limit int64
		want  int
	}{
		{"unlimited", 0, 200},
		{"room to spare", 1 << 20, 200},
		{"smaller than the response", 8, 500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inst, err := component.Instantiate(ctx, r, realHTTPIncomingWasm,
				WithWASI(WASIConfig{EnableHTTP: true, MaxResponseBytes: tc.limit})...)
			if err != nil {
				t.Fatalf("Instantiate: %v", err)
			}
			defer inst.Close(ctx)

			rec := httptest.NewRecorder()
			Handler(inst).ServeHTTP(rec, httptest.NewRequest("GET", "/greet?name=wazy", nil))
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (body %q)", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}
