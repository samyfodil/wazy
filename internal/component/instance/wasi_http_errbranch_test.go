package instance

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/samyfodil/wazy/internal/component/abi"
)

// newTestHTTP returns a wasiHTTP wired to a fresh handle table, as a real
// Instantiate would, so host funcs that mint nested own<T> handles work.
func newTestHTTP() *wasiHTTP {
	h := newWasiHTTP()
	tbl := newHandleTable()
	h.getResources = func() (*handleTable, error) { return tbl, nil }
	return h
}

func reqErr(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("want error containing %q, got %v", want, err)
	}
}

func TestHTTP_IncomingRequestMethod(t *testing.T) {
	h := newTestHTTP()
	rep := h.newIncomingRep(&httpIncomingRequest{method: "get"}) // lower-case: exercises ToUpper
	res, err := h.incomingRequestMethod(context.Background(), []abi.Value{rep})
	if err != nil {
		t.Fatal(err)
	}
	if vv := res[0].(abi.VariantValue); vv.Disc != 0 || vv.Payload != nil {
		t.Fatalf("GET -> %#v, want disc 0 no payload", vv)
	}
	// "other" method: non-standard token -> disc 9 with the token as payload.
	oRep := h.newIncomingRep(&httpIncomingRequest{method: "PROPFIND"})
	res, _ = h.incomingRequestMethod(context.Background(), []abi.Value{oRep})
	if vv := res[0].(abi.VariantValue); vv.Disc != 9 || vv.Payload != "PROPFIND" {
		t.Fatalf("PROPFIND -> %#v, want disc 9 payload PROPFIND", vv)
	}

	_, err = h.incomingRequestMethod(context.Background(), nil)
	reqErr(t, err, "expected 1 arg")
	_, err = h.incomingRequestMethod(context.Background(), []abi.Value{"x"})
	reqErr(t, err, "expected uint32 rep")
	_, err = h.incomingRequestMethod(context.Background(), []abi.Value{uint32(999)})
	reqErr(t, err, "does not name a live incoming-request")
}

func TestHTTP_IncomingRequestPathWithQuery(t *testing.T) {
	h := newTestHTTP()
	rep := h.newIncomingRep(&httpIncomingRequest{pathQ: "/a?b=1"})
	res, err := h.incomingRequestPathWithQuery(context.Background(), []abi.Value{rep})
	if err != nil || res[0].(string) != "/a?b=1" {
		t.Fatalf("path = %v, %v", res, err)
	}
	_, err = h.incomingRequestPathWithQuery(context.Background(), nil)
	reqErr(t, err, "expected 1 arg")
	_, err = h.incomingRequestPathWithQuery(context.Background(), []abi.Value{"x"})
	reqErr(t, err, "expected uint32 rep")
	_, err = h.incomingRequestPathWithQuery(context.Background(), []abi.Value{uint32(999)})
	reqErr(t, err, "does not name a live incoming-request")
}

func TestHTTP_Fields(t *testing.T) {
	h := newTestHTTP()
	res, err := h.fieldsConstructor(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	rep := res[0].(uint32)
	_, err = h.fieldsConstructor(context.Background(), []abi.Value{uint32(1)})
	reqErr(t, err, "expected 0 args")

	// set, then re-set (exercises dropName replacing the prior value).
	vals := []abi.Value{[]abi.Value{uint32('a')}}
	if _, err := h.fieldsSet(context.Background(), []abi.Value{rep, "k", vals}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.fieldsSet(context.Background(), []abi.Value{rep, "k", []abi.Value{[]abi.Value{uint32('b')}}}); err != nil {
		t.Fatal(err)
	}
	// Add a second, different-named field, then re-set "k" so dropName has to
	// walk past a non-matching entry (keep) and drop the matching one.
	if _, err := h.fieldsSet(context.Background(), []abi.Value{rep, "other", []abi.Value{[]abi.Value{uint32('z')}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.fieldsSet(context.Background(), []abi.Value{rep, "k", []abi.Value{[]abi.Value{uint32('c')}}}); err != nil {
		t.Fatal(err)
	}
	f := h.fields[rep]
	if len(f.names) != 2 {
		t.Fatalf("after replace expected 2 fields, got %v", f.names)
	}
	// "other" kept, "k" replaced to "c".
	got := map[string]string{}
	for i, n := range f.names {
		got[n] = string(f.values[i])
	}
	if got["other"] != "z" || got["k"] != "c" {
		t.Fatalf("fields = %v", got)
	}

	_, err = h.fieldsSet(context.Background(), []abi.Value{rep, "k"})
	reqErr(t, err, "expected 3 args")
	_, err = h.fieldsSet(context.Background(), []abi.Value{"x", "k", vals})
	reqErr(t, err, "self: expected uint32 rep")
	_, err = h.fieldsSet(context.Background(), []abi.Value{rep, 7, vals})
	reqErr(t, err, "name: expected string")
	_, err = h.fieldsSet(context.Background(), []abi.Value{rep, "k", "notalist"})
	reqErr(t, err, "value:")
	_, err = h.fieldsSet(context.Background(), []abi.Value{uint32(999), "k", vals})
	reqErr(t, err, "does not name a live fields")
}

func TestHTTP_OutgoingResponse(t *testing.T) {
	h := newTestHTTP()
	fieldsRep := h.newFieldsRep(&httpFields{})
	res, err := h.outgoingResponseConstructor(context.Background(), []abi.Value{fieldsRep})
	if err != nil {
		t.Fatal(err)
	}
	respRep := res[0].(uint32)
	if h.responses[respRep].status != 200 {
		t.Fatal("default status should be 200")
	}
	// constructor with an unknown fields rep falls back to empty headers.
	if _, err := h.outgoingResponseConstructor(context.Background(), []abi.Value{uint32(999)}); err != nil {
		t.Fatalf("unknown fields should fall back, got %v", err)
	}
	_, err = h.outgoingResponseConstructor(context.Background(), nil)
	reqErr(t, err, "expected 1 arg")
	_, err = h.outgoingResponseConstructor(context.Background(), []abi.Value{"x"})
	reqErr(t, err, "expected uint32 rep")

	// set-status-code
	if _, err := h.outgoingResponseSetStatusCode(context.Background(), []abi.Value{respRep, uint32(404)}); err != nil {
		t.Fatal(err)
	}
	if h.responses[respRep].status != 404 {
		t.Fatal("status not updated")
	}
	_, err = h.outgoingResponseSetStatusCode(context.Background(), []abi.Value{respRep})
	reqErr(t, err, "expected 2 args")
	_, err = h.outgoingResponseSetStatusCode(context.Background(), []abi.Value{"x", uint32(200)})
	reqErr(t, err, "self: expected uint32 rep")
	_, err = h.outgoingResponseSetStatusCode(context.Background(), []abi.Value{respRep, "x"})
	reqErr(t, err, "status: expected uint32")
	_, err = h.outgoingResponseSetStatusCode(context.Background(), []abi.Value{uint32(999), uint32(200)})
	reqErr(t, err, "does not name a live outgoing-response")

	// body(): first Ok, second call Err (body already taken).
	res, err = h.outgoingResponseBody(context.Background(), []abi.Value{respRep})
	if err != nil || res[0].(abi.ResultValue).IsErr {
		t.Fatalf("first body() = %v, %v", res, err)
	}
	res, _ = h.outgoingResponseBody(context.Background(), []abi.Value{respRep})
	if !res[0].(abi.ResultValue).IsErr {
		t.Fatal("second body() should be Err")
	}
	_, err = h.outgoingResponseBody(context.Background(), nil)
	reqErr(t, err, "expected 1 arg")
	_, err = h.outgoingResponseBody(context.Background(), []abi.Value{"x"})
	reqErr(t, err, "expected uint32 rep")
	_, err = h.outgoingResponseBody(context.Background(), []abi.Value{uint32(999)})
	reqErr(t, err, "does not name a live outgoing-response")
}

func TestHTTP_OutgoingBody(t *testing.T) {
	h := newTestHTTP()
	body := &httpOutgoingBody{}
	bodyRep := h.newBodyRep(body)

	res, err := h.outgoingBodyWrite(context.Background(), []abi.Value{bodyRep})
	if err != nil || res[0].(abi.ResultValue).IsErr {
		t.Fatalf("write() = %v, %v", res, err)
	}
	// the returned handle names an output-stream rep that bodyStreamWrite routes.
	streamHandle := res[0].(abi.ResultValue).Payload.(uint32)
	tbl, _ := h.getResources()
	streamRep, _ := tbl.Rep(wasiOutputStreamResType, streamHandle)
	if found, err := h.bodyStreamWrite(streamRep, []byte("hi")); !found || err != nil {
		t.Fatalf("bodyStreamWrite = %v, %v", found, err)
	}
	if body.buf.String() != "hi" {
		t.Fatalf("body = %q", body.buf.String())
	}
	if found, _ := h.bodyStreamWrite(uint32(123), nil); found {
		t.Fatal("unknown stream rep should not be found")
	}

	_, err = h.outgoingBodyWrite(context.Background(), nil)
	reqErr(t, err, "expected 1 arg")
	_, err = h.outgoingBodyWrite(context.Background(), []abi.Value{"x"})
	reqErr(t, err, "expected uint32 rep")
	_, err = h.outgoingBodyWrite(context.Background(), []abi.Value{uint32(999)})
	reqErr(t, err, "does not name a live outgoing-body")

	// finish: Ok with nil trailers, then a write-after-finish is rejected.
	if _, err := h.outgoingBodyFinish(context.Background(), []abi.Value{bodyRep, nil}); err != nil {
		t.Fatal(err)
	}
	if found, err := h.bodyStreamWrite(streamRep, []byte("x")); !found || err == nil {
		t.Fatalf("write after finish should error, got found=%v err=%v", found, err)
	}
	_, err = h.outgoingBodyFinish(context.Background(), []abi.Value{bodyRep})
	reqErr(t, err, "expected 2 args")
	_, err = h.outgoingBodyFinish(context.Background(), []abi.Value{"x", nil})
	reqErr(t, err, "this: expected uint32 rep")
	_, err = h.outgoingBodyFinish(context.Background(), []abi.Value{bodyRep, uint32(1)})
	reqErr(t, err, "trailers are not supported")
	_, err = h.outgoingBodyFinish(context.Background(), []abi.Value{uint32(999), nil})
	reqErr(t, err, "does not name a live outgoing-body")
}

func TestHTTP_ResponseOutparamSet(t *testing.T) {
	h := newTestHTTP()
	tbl, _ := h.getResources()

	// Ok path: mint a response + its handle, then set.
	cap := &httpCapture{}
	outRep := h.newOutparamRep(cap)
	respRep := h.newResponseRep(&httpOutgoingResponse{status: 201})
	respHandle := tbl.NewOwn(wasiHTTPOutgoingResponseResType, respRep)
	if _, err := h.responseOutparamSet(context.Background(), []abi.Value{outRep, abi.ResultValue{Payload: respHandle}}); err != nil {
		t.Fatal(err)
	}
	if !cap.set || cap.resp.status != 201 {
		t.Fatalf("capture = %#v", cap)
	}

	// Err path: guest set an error-code.
	cap2 := &httpCapture{}
	outRep2 := h.newOutparamRep(cap2)
	if _, err := h.responseOutparamSet(context.Background(), []abi.Value{outRep2, abi.ResultValue{IsErr: true, Payload: abi.VariantValue{Disc: 6}}}); err != nil {
		t.Fatal(err)
	}
	if !cap2.set || !cap2.isErr || cap2.errDisc != 6 {
		t.Fatalf("err capture = %#v", cap2)
	}

	_, err := h.responseOutparamSet(context.Background(), []abi.Value{outRep})
	reqErr(t, err, "expected 2 args")
	_, err = h.responseOutparamSet(context.Background(), []abi.Value{"x", abi.ResultValue{}})
	reqErr(t, err, "param: expected uint32 rep")
	_, err = h.responseOutparamSet(context.Background(), []abi.Value{outRep, "notaresult"})
	reqErr(t, err, "expected result")
	_, err = h.responseOutparamSet(context.Background(), []abi.Value{uint32(999), abi.ResultValue{}})
	reqErr(t, err, "does not name a live response-outparam")
	// bad Ok payload type
	cap3 := &httpCapture{}
	outRep3 := h.newOutparamRep(cap3)
	_, err = h.responseOutparamSet(context.Background(), []abi.Value{outRep3, abi.ResultValue{Payload: "notahandle"}})
	reqErr(t, err, "expected outgoing-response handle")
}

func TestHTTP_ServeHTTP_NoExport(t *testing.T) {
	// An instance with an http host but no incoming-handler export fails loud.
	in := &Instance{httpHost: newTestHTTP(), resources: newHandleTable(), instanceExports: map[string]map[string]instanceExportEntry{}}
	u, _ := url.Parse("/x")
	_, _, _, err := in.serveHTTP(context.Background(), "GET", u, http.Header{}, nil)
	reqErr(t, err, "does not export")
}

// TestHTTP_ServeHTTP_500 proves the http.Handler wrapper reports a guest/setup
// failure as a 500 rather than a bogus success: an instance with no
// incoming-handler export makes serveHTTP fail, which ServeHTTP turns into 500.
func TestHTTP_ServeHTTP_500(t *testing.T) {
	in := &Instance{httpHost: newTestHTTP(), resources: newHandleTable(), instanceExports: map[string]map[string]instanceExportEntry{}}
	req := httptest.NewRequest("GET", "/x", nil)
	rec := httptest.NewRecorder()
	in.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "does not export") {
		t.Fatalf("body = %q", rec.Body.String())
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

// TestHTTP_ServeHTTP_BadBody proves a request whose body fails to read is
// reported as 400, not silently forwarded with an empty body.
func TestHTTP_ServeHTTP_BadBody(t *testing.T) {
	in := &Instance{httpHost: newTestHTTP(), resources: newHandleTable(), instanceExports: map[string]map[string]instanceExportEntry{}}
	req := httptest.NewRequest("POST", "/x", errReader{})
	rec := httptest.NewRecorder()
	in.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestHTTP_FindExportInstance(t *testing.T) {
	in := &Instance{instanceExports: map[string]map[string]instanceExportEntry{
		"wasi:http/incoming-handler@0.2.12": nil,
	}}
	if name, ok := in.findExportInstance("wasi:http/incoming-handler"); !ok || name != "wasi:http/incoming-handler@0.2.12" {
		t.Fatalf("findExportInstance = %q, %v", name, ok)
	}
	if _, ok := in.findExportInstance("wasi:http/outgoing-handler"); ok {
		t.Fatal("unexpected match")
	}
}
