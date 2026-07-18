package instance

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

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
	// A Some(trailers) with a bogus (wrong-type) handle fails to resolve.
	_, err = h.outgoingBodyFinish(context.Background(), []abi.Value{bodyRep, uint32(999999)})
	reqErr(t, err, "trailers:")
	_, err = h.outgoingBodyFinish(context.Background(), []abi.Value{uint32(999), nil})
	reqErr(t, err, "does not name a live outgoing-body")

	// A Some(trailers) with a real fields handle attaches the trailers.
	body2 := &httpOutgoingBody{}
	body2Rep := h.newBodyRep(body2)
	trFields := h.newFieldsRep(&httpFields{names: []string{"x-t"}, values: [][]byte{[]byte("v")}})
	trHandle := tbl.NewOwn(wasiHTTPFieldsResType, trFields)
	if _, err := h.outgoingBodyFinish(context.Background(), []abi.Value{body2Rep, trHandle}); err != nil {
		t.Fatalf("finish with trailers: %v", err)
	}
	if body2.trailers == nil || len(body2.trailers.names) != 1 || body2.trailers.names[0] != "x-t" {
		t.Fatalf("trailers not attached: %+v", body2.trailers)
	}
}

// TestHTTP_FutureTrailers covers the request/response-trailer read path
// (incoming-body.finish -> future-trailers -> get) directly, including the
// with-trailers, without-trailers, already-gotten, and error branches.
func TestHTTP_FutureTrailers(t *testing.T) {
	h := newTestHTTP()

	// incoming-body.finish carries the body's trailers into a future-trailers.
	inRep := h.newInBodyRep(&httpIncomingBody{trailers: http.Header{"X-T": {"val"}}})
	res, err := h.incomingBodyFinish(context.Background(), []abi.Value{inRep})
	if err != nil {
		t.Fatal(err)
	}
	ftRep := res[0].(uint32)

	// subscribe -> always-ready pollable.
	if _, err := h.futureTrailersSubscribe(context.Background(), []abi.Value{ftRep}); err != nil {
		t.Fatal(err)
	}

	// get -> Some(Ok(Ok(Some(fields)))).
	res, err = h.futureTrailersGet(context.Background(), []abi.Value{ftRep})
	if err != nil {
		t.Fatal(err)
	}
	outer := res[0].(abi.ResultValue)
	if outer.IsErr {
		t.Fatal("first get should be Ok, not already-gotten")
	}
	inner := outer.Payload.(abi.ResultValue)
	if inner.IsErr || inner.Payload == nil {
		t.Fatalf("inner result should be Ok(Some(fields)), got %+v", inner)
	}
	// second get -> already-gotten (outer Err).
	res, _ = h.futureTrailersGet(context.Background(), []abi.Value{ftRep})
	if !res[0].(abi.ResultValue).IsErr {
		t.Fatal("second get should be Err (already gotten)")
	}

	// no-trailers body -> Ok(Ok(None)).
	noRep := h.newInBodyRep(&httpIncomingBody{})
	fres, _ := h.incomingBodyFinish(context.Background(), []abi.Value{noRep})
	res, _ = h.futureTrailersGet(context.Background(), []abi.Value{fres[0].(uint32)})
	if inner := res[0].(abi.ResultValue).Payload.(abi.ResultValue); inner.IsErr || inner.Payload != nil {
		t.Fatalf("no-trailers get should be Ok(None), got %+v", inner)
	}

	// error branches
	_, err = h.incomingBodyFinish(context.Background(), nil)
	reqErr(t, err, "expected 1 arg")
	_, err = h.incomingBodyFinish(context.Background(), []abi.Value{"x"})
	reqErr(t, err, "expected uint32 rep")
	_, err = h.incomingBodyFinish(context.Background(), []abi.Value{uint32(99999)})
	reqErr(t, err, "does not name a live incoming-body")
	_, err = h.futureTrailersGet(context.Background(), nil)
	reqErr(t, err, "expected 1 arg")
	_, err = h.futureTrailersGet(context.Background(), []abi.Value{"x"})
	reqErr(t, err, "expected uint32 rep")
	_, err = h.futureTrailersGet(context.Background(), []abi.Value{uint32(99999)})
	reqErr(t, err, "does not name a live future-trailers")
	_, err = h.futureTrailersSubscribe(context.Background(), nil)
	reqErr(t, err, "expected 1 arg")
	_, err = h.futureTrailersSubscribe(context.Background(), []abi.Value{"x"})
	reqErr(t, err, "expected uint32 rep")
	_, err = h.futureTrailersSubscribe(context.Background(), []abi.Value{uint32(99999)})
	reqErr(t, err, "does not name a live future-trailers")
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
	in := &Instance{sched: &sched{}, httpHost: newTestHTTP(), resources: newHandleTable(), instanceExports: map[string]map[string]instanceExportEntry{}}
	u, _ := url.Parse("/x")
	_, _, _, _, err := in.serveHTTP(context.Background(), "GET", u, http.Header{}, nil, nil)
	reqErr(t, err, "does not export")
}

// TestHTTP_ServeHTTP_500 proves the http.Handler wrapper reports a guest/setup
// failure as a 500 rather than a bogus success: an instance with no
// incoming-handler export makes serveHTTP fail, which ServeHTTP turns into 500.
func TestHTTP_ServeHTTP_500(t *testing.T) {
	in := &Instance{sched: &sched{}, httpHost: newTestHTTP(), resources: newHandleTable(), instanceExports: map[string]map[string]instanceExportEntry{}}
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
	in := &Instance{sched: &sched{}, httpHost: newTestHTTP(), resources: newHandleTable(), instanceExports: map[string]map[string]instanceExportEntry{}}
	req := httptest.NewRequest("POST", "/x", errReader{})
	rec := httptest.NewRecorder()
	in.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestHTTP_FindExportInstance(t *testing.T) {
	in := &Instance{sched: &sched{}, instanceExports: map[string]map[string]instanceExportEntry{
		"wasi:http/incoming-handler@0.2.12": nil,
	}}
	if name, ok := in.findExportInstance("wasi:http/incoming-handler"); !ok || name != "wasi:http/incoming-handler@0.2.12" {
		t.Fatalf("findExportInstance = %q, %v", name, ok)
	}
	if _, ok := in.findExportInstance("wasi:http/outgoing-handler"); ok {
		t.Fatal("unexpected match")
	}
}

// ---- outgoing (client) side error branches ----

func TestHTTP_OutgoingRequest(t *testing.T) {
	h := newTestHTTP()
	fRep := h.newFieldsRep(&httpFields{})
	res, err := h.outgoingRequestConstructor(context.Background(), []abi.Value{fRep})
	if err != nil {
		t.Fatal(err)
	}
	rep := res[0].(uint32)
	if r := h.outRequests[rep]; r.method != "GET" || r.scheme != "http" || r.pathQ != "/" {
		t.Fatalf("defaults = %#v", r)
	}
	// unknown fields rep falls back to empty headers
	if _, err := h.outgoingRequestConstructor(context.Background(), []abi.Value{uint32(999)}); err != nil {
		t.Fatal(err)
	}
	_, err = h.outgoingRequestConstructor(context.Background(), nil)
	reqErr(t, err, "expected 1 arg")
	_, err = h.outgoingRequestConstructor(context.Background(), []abi.Value{"x"})
	reqErr(t, err, "expected uint32 rep")

	// set-method: standard + "other"
	if _, err := h.outgoingRequestSetMethod(context.Background(), []abi.Value{rep, abi.VariantValue{Disc: 2}}); err != nil {
		t.Fatal(err)
	}
	if h.outRequests[rep].method != "POST" {
		t.Fatalf("method = %q", h.outRequests[rep].method)
	}
	if _, err := h.outgoingRequestSetMethod(context.Background(), []abi.Value{rep, abi.VariantValue{Disc: 9, Payload: "propfind"}}); err != nil {
		t.Fatal(err)
	}
	if h.outRequests[rep].method != "PROPFIND" {
		t.Fatalf("other method = %q", h.outRequests[rep].method)
	}
	_, err = h.outgoingRequestSetMethod(context.Background(), []abi.Value{rep})
	reqErr(t, err, "expected 2 args")
	_, err = h.outgoingRequestSetMethod(context.Background(), []abi.Value{"x", abi.VariantValue{}})
	reqErr(t, err, "self: expected uint32 rep")
	_, err = h.outgoingRequestSetMethod(context.Background(), []abi.Value{rep, "notavariant"})
	reqErr(t, err, "method: expected variant")
	_, err = h.outgoingRequestSetMethod(context.Background(), []abi.Value{uint32(999), abi.VariantValue{}})
	reqErr(t, err, "does not name a live outgoing-request")

	// set-path-with-query (Some + None)
	if _, err := h.outgoingRequestSetPathWithQuery(context.Background(), []abi.Value{rep, "/p?x=1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.outgoingRequestSetPathWithQuery(context.Background(), []abi.Value{rep, nil}); err != nil {
		t.Fatal(err)
	}
	if h.outRequests[rep].pathQ != "/p?x=1" {
		t.Fatalf("path = %q (None should not overwrite)", h.outRequests[rep].pathQ)
	}
	_, err = h.outgoingRequestSetPathWithQuery(context.Background(), []abi.Value{rep})
	reqErr(t, err, "expected 2 args")

	// set-authority
	if _, err := h.outgoingRequestSetAuthority(context.Background(), []abi.Value{rep, "h:1"}); err != nil {
		t.Fatal(err)
	}
	if h.outRequests[rep].authority != "h:1" {
		t.Fatal("authority not set")
	}
	_, err = h.outgoingRequestSetAuthority(context.Background(), []abi.Value{uint32(999), "h"})
	reqErr(t, err, "does not name a live outgoing-request")

	// set-scheme: HTTP, HTTPS, other, None
	for disc, want := range map[uint32]string{0: "http", 1: "https"} {
		if _, err := h.outgoingRequestSetScheme(context.Background(), []abi.Value{rep, abi.VariantValue{Disc: disc}}); err != nil {
			t.Fatal(err)
		}
		if h.outRequests[rep].scheme != want {
			t.Fatalf("scheme disc %d = %q, want %q", disc, h.outRequests[rep].scheme, want)
		}
	}
	if _, err := h.outgoingRequestSetScheme(context.Background(), []abi.Value{rep, abi.VariantValue{Disc: 2, Payload: "WS"}}); err != nil {
		t.Fatal(err)
	}
	if h.outRequests[rep].scheme != "ws" {
		t.Fatalf("other scheme = %q", h.outRequests[rep].scheme)
	}
	if _, err := h.outgoingRequestSetScheme(context.Background(), []abi.Value{rep, nil}); err != nil {
		t.Fatal(err)
	}
	_, err = h.outgoingRequestSetScheme(context.Background(), []abi.Value{rep, "notavariant"})
	reqErr(t, err, "scheme: expected variant")
	_, err = h.outgoingRequestSetScheme(context.Background(), []abi.Value{uint32(999), nil})
	reqErr(t, err, "does not name a live outgoing-request")
}

type failRT struct{}

func (failRT) RoundTrip(*http.Request) (*http.Response, error) { return nil, io.ErrUnexpectedEOF }

func TestHTTP_OutgoingHandlerHandle(t *testing.T) {
	h := newTestHTTP()
	h.client = &http.Client{Transport: backendRT{}}

	mk := func(method, scheme, authority, path string) uint32 {
		return h.newOutRequestRep(&httpOutgoingRequest{method: method, scheme: scheme, authority: authority, pathQ: path, headers: &httpFields{names: []string{"x-test"}, values: [][]byte{[]byte("1")}}})
	}
	// success
	res, err := h.outgoingHandlerHandle(context.Background(), []abi.Value{mk("GET", "http", "h", "/ok"), nil})
	if err != nil || res[0].(abi.ResultValue).IsErr {
		t.Fatalf("handle = %v, %v", res, err)
	}
	futHandle := res[0].(abi.ResultValue).Payload.(uint32)
	tbl, _ := h.getResources()
	futRep, _ := tbl.Rep(wasiHTTPFutureResType, futHandle)
	if h.futures[futRep].errCode != 0 {
		t.Fatal("expected a successful future")
	}

	// connection failure -> future carries connection-refused (disc 6)
	h.client = &http.Client{Transport: failRT{}}
	res, _ = h.outgoingHandlerHandle(context.Background(), []abi.Value{mk("GET", "http", "h", "/x"), nil})
	fh := res[0].(abi.ResultValue).Payload.(uint32)
	fr, _ := tbl.Rep(wasiHTTPFutureResType, fh)
	if h.futures[fr].errCode != 6 {
		t.Fatalf("errCode = %d, want 6", h.futures[fr].errCode)
	}

	// malformed method -> NewRequest error -> URI-invalid (disc 19)
	res, _ = h.outgoingHandlerHandle(context.Background(), []abi.Value{mk("BAD METHOD", "http", "h", "/x"), nil})
	fh = res[0].(abi.ResultValue).Payload.(uint32)
	fr, _ = tbl.Rep(wasiHTTPFutureResType, fh)
	if h.futures[fr].errCode != 19 {
		t.Fatalf("errCode = %d, want 19", h.futures[fr].errCode)
	}

	_, err = h.outgoingHandlerHandle(context.Background(), []abi.Value{mk("GET", "http", "h", "/x")})
	reqErr(t, err, "expected 2 args")
	_, err = h.outgoingHandlerHandle(context.Background(), []abi.Value{"x", nil})
	reqErr(t, err, "request: expected uint32 rep")
	// A non-nil options that isn't a live request-options handle fails loud.
	_, err = h.outgoingHandlerHandle(context.Background(), []abi.Value{mk("GET", "http", "h", "/x"), "notahandle"})
	reqErr(t, err, "options: expected request-options handle")
	_, err = h.outgoingHandlerHandle(context.Background(), []abi.Value{mk("GET", "http", "h", "/x"), uint32(999)})
	reqErr(t, err, "request-options handle")
	_, err = h.outgoingHandlerHandle(context.Background(), []abi.Value{uint32(999), nil})
	reqErr(t, err, "does not name a live outgoing-request")
}

type backendRT struct{}

func (backendRT) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("ok"))}, nil
}

func TestHTTP_FutureAndResponse(t *testing.T) {
	h := newTestHTTP()
	tbl, _ := h.getResources()
	// subscribe
	fRep := h.newFutureRep(&httpFuture{})
	if _, err := h.futureSubscribe(context.Background(), []abi.Value{fRep}); err != nil {
		t.Fatal(err)
	}
	_, err := h.futureSubscribe(context.Background(), nil)
	reqErr(t, err, "expected 1 arg")
	_, err = h.futureSubscribe(context.Background(), []abi.Value{"x"})
	reqErr(t, err, "expected uint32 rep")

	// get: Ok path, then None (taken)
	respRep := h.newInResponseRep(&httpIncomingResponse{status: 200, body: []byte("hi")})
	okFut := h.newFutureRep(&httpFuture{respRep: respRep})
	res, err := h.futureGet(context.Background(), []abi.Value{okFut})
	if err != nil {
		t.Fatal(err)
	}
	outer := res[0].(abi.ResultValue)
	inner := outer.Payload.(abi.ResultValue)
	if inner.IsErr {
		t.Fatal("inner should be Ok")
	}
	res, _ = h.futureGet(context.Background(), []abi.Value{okFut})
	if res[0] != nil {
		t.Fatal("second get should be None")
	}
	// get: Err path (transport error-code)
	errFut := h.newFutureRep(&httpFuture{errCode: 6})
	res, _ = h.futureGet(context.Background(), []abi.Value{errFut})
	inner = res[0].(abi.ResultValue).Payload.(abi.ResultValue)
	if !inner.IsErr || inner.Payload.(abi.VariantValue).Disc != 6 {
		t.Fatalf("err future inner = %#v", inner)
	}
	_, err = h.futureGet(context.Background(), nil)
	reqErr(t, err, "expected 1 arg")
	_, err = h.futureGet(context.Background(), []abi.Value{"x"})
	reqErr(t, err, "expected uint32 rep")
	_, err = h.futureGet(context.Background(), []abi.Value{uint32(999)})
	reqErr(t, err, "does not name a live future")

	// incoming-response.status
	sres, err := h.incomingResponseStatus(context.Background(), []abi.Value{respRep})
	if err != nil || sres[0].(uint32) != 200 {
		t.Fatalf("status = %v, %v", sres, err)
	}
	_, err = h.incomingResponseStatus(context.Background(), nil)
	reqErr(t, err, "expected 1 arg")
	_, err = h.incomingResponseStatus(context.Background(), []abi.Value{"x"})
	reqErr(t, err, "expected uint32 rep")
	_, err = h.incomingResponseStatus(context.Background(), []abi.Value{uint32(999)})
	reqErr(t, err, "does not name a live incoming-response")

	// consume: Ok, then Err (already consumed)
	cres, err := h.incomingResponseConsume(context.Background(), []abi.Value{respRep})
	if err != nil || cres[0].(abi.ResultValue).IsErr {
		t.Fatalf("consume = %v, %v", cres, err)
	}
	bodyHandle := cres[0].(abi.ResultValue).Payload.(uint32)
	cres, _ = h.incomingResponseConsume(context.Background(), []abi.Value{respRep})
	if !cres[0].(abi.ResultValue).IsErr {
		t.Fatal("second consume should be Err")
	}
	_, err = h.incomingResponseConsume(context.Background(), nil)
	reqErr(t, err, "expected 1 arg")
	_, err = h.incomingResponseConsume(context.Background(), []abi.Value{"x"})
	reqErr(t, err, "expected uint32 rep")
	_, err = h.incomingResponseConsume(context.Background(), []abi.Value{uint32(999)})
	reqErr(t, err, "does not name a live incoming-response")

	// incoming-body.stream: needs a backing minter
	bodyRep, _ := tbl.Rep(wasiHTTPIncomingBodyResType, bodyHandle)
	streams := map[uint32][]byte{}
	next := uint32(1)
	h.newInputStreamRep = func(b []byte) uint32 { r := next; next++; streams[r] = b; return r }
	stres, err := h.incomingBodyStream(context.Background(), []abi.Value{bodyRep})
	if err != nil || stres[0].(abi.ResultValue).IsErr {
		t.Fatalf("stream = %v, %v", stres, err)
	}
	stres, _ = h.incomingBodyStream(context.Background(), []abi.Value{bodyRep})
	if !stres[0].(abi.ResultValue).IsErr {
		t.Fatal("second stream should be Err")
	}
	_, err = h.incomingBodyStream(context.Background(), nil)
	reqErr(t, err, "expected 1 arg")
	_, err = h.incomingBodyStream(context.Background(), []abi.Value{"x"})
	reqErr(t, err, "expected uint32 rep")
	_, err = h.incomingBodyStream(context.Background(), []abi.Value{uint32(999)})
	reqErr(t, err, "does not name a live incoming-body")

	// no backing configured -> fail loud
	h.newInputStreamRep = nil
	nbRep := h.newInBodyRep(&httpIncomingBody{body: []byte("x")})
	_, err = h.incomingBodyStream(context.Background(), []abi.Value{nbRep})
	reqErr(t, err, "no input-stream backing")
}

func TestHTTP_IncomingRequestHeaders(t *testing.T) {
	h := newTestHTTP()
	hdr := http.Header{}
	hdr.Add("X-Echo", "v1")
	hdr.Add("X-Echo", "v2")
	hdr.Set("A-Header", "a")
	rep := h.newIncomingRep(&httpIncomingRequest{headers: hdr})
	res, err := h.incomingRequestHeaders(context.Background(), []abi.Value{rep})
	if err != nil {
		t.Fatal(err)
	}
	fRep := res[0].(uint32)
	f := h.fields[fRep]
	// sorted by name; x-echo keeps v1 then v2; names lower-cased.
	got := map[string][]string{}
	for i, n := range f.names {
		got[n] = append(got[n], string(f.values[i]))
	}
	if len(got["x-echo"]) != 2 || got["x-echo"][0] != "v1" || got["x-echo"][1] != "v2" {
		t.Fatalf("x-echo = %v", got["x-echo"])
	}
	if got["a-header"][0] != "a" {
		t.Fatalf("a-header = %v", got["a-header"])
	}
	_, err = h.incomingRequestHeaders(context.Background(), nil)
	reqErr(t, err, "expected 1 arg")
	_, err = h.incomingRequestHeaders(context.Background(), []abi.Value{"x"})
	reqErr(t, err, "expected uint32 rep")
	_, err = h.incomingRequestHeaders(context.Background(), []abi.Value{uint32(999)})
	reqErr(t, err, "does not name a live incoming-request")
}

func TestHTTP_IncomingRequestConsume(t *testing.T) {
	h := newTestHTTP()
	rep := h.newIncomingRep(&httpIncomingRequest{body: []byte("hi")})
	res, err := h.incomingRequestConsume(context.Background(), []abi.Value{rep})
	if err != nil || res[0].(abi.ResultValue).IsErr {
		t.Fatalf("consume = %v, %v", res, err)
	}
	// second consume -> Err (body already taken)
	res, _ = h.incomingRequestConsume(context.Background(), []abi.Value{rep})
	if !res[0].(abi.ResultValue).IsErr {
		t.Fatal("second consume should be Err")
	}
	_, err = h.incomingRequestConsume(context.Background(), nil)
	reqErr(t, err, "expected 1 arg")
	_, err = h.incomingRequestConsume(context.Background(), []abi.Value{"x"})
	reqErr(t, err, "expected uint32 rep")
	_, err = h.incomingRequestConsume(context.Background(), []abi.Value{uint32(999)})
	reqErr(t, err, "does not name a live incoming-request")
}

func TestHTTP_FieldsGet(t *testing.T) {
	h := newTestHTTP()
	rep := h.newFieldsRep(&httpFields{names: []string{"x-a", "x-a", "x-b"}, values: [][]byte{[]byte("1"), []byte("2"), []byte("3")}})
	// case-insensitive; multiple values in order.
	res, err := h.fieldsGet(context.Background(), []abi.Value{rep, "X-A"})
	if err != nil {
		t.Fatal(err)
	}
	list := res[0].([]abi.Value)
	if len(list) != 2 {
		t.Fatalf("x-a values = %d, want 2", len(list))
	}
	if b, _ := wasiBytesFromList(list[0]); string(b) != "1" {
		t.Fatalf("first value = %q", b)
	}
	// missing -> empty list
	res, _ = h.fieldsGet(context.Background(), []abi.Value{rep, "nope"})
	if len(res[0].([]abi.Value)) != 0 {
		t.Fatal("missing header should be empty list")
	}
	_, err = h.fieldsGet(context.Background(), []abi.Value{rep})
	reqErr(t, err, "expected 2 args")
	_, err = h.fieldsGet(context.Background(), []abi.Value{"x", "n"})
	reqErr(t, err, "self: expected uint32 rep")
	_, err = h.fieldsGet(context.Background(), []abi.Value{rep, 7})
	reqErr(t, err, "name: expected string")
	_, err = h.fieldsGet(context.Background(), []abi.Value{uint32(999), "n"})
	reqErr(t, err, "does not name a live fields")
}

func TestHTTP_OutgoingRequestBody(t *testing.T) {
	h := newTestHTTP()
	rep := h.newOutRequestRep(&httpOutgoingRequest{method: "POST", scheme: "http", pathQ: "/"})
	res, err := h.outgoingRequestBody(context.Background(), []abi.Value{rep})
	if err != nil || res[0].(abi.ResultValue).IsErr {
		t.Fatalf("body() = %v, %v", res, err)
	}
	if h.outRequests[rep].body == nil {
		t.Fatal("outgoing-request.body should set r.body")
	}
	// second body() -> Err (already taken)
	res, _ = h.outgoingRequestBody(context.Background(), []abi.Value{rep})
	if !res[0].(abi.ResultValue).IsErr {
		t.Fatal("second body() should be Err")
	}
	_, err = h.outgoingRequestBody(context.Background(), nil)
	reqErr(t, err, "expected 1 arg")
	_, err = h.outgoingRequestBody(context.Background(), []abi.Value{"x"})
	reqErr(t, err, "expected uint32 rep")
	_, err = h.outgoingRequestBody(context.Background(), []abi.Value{uint32(999)})
	reqErr(t, err, "does not name a live outgoing-request")
}

func TestHTTP_RequestOptions(t *testing.T) {
	h := newTestHTTP()
	res, err := h.requestOptionsConstructor(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	rep := res[0].(uint32)
	_, err = h.requestOptionsConstructor(context.Background(), []abi.Value{uint32(1)})
	reqErr(t, err, "expected 0 args")

	setConn := h.requestOptionsSetTimeout("set-connect-timeout", true)
	setFB := h.requestOptionsSetTimeout("set-first-byte-timeout", false)
	if _, err := setConn(context.Background(), []abi.Value{rep, uint64(3000000000)}); err != nil {
		t.Fatal(err)
	}
	if _, err := setFB(context.Background(), []abi.Value{rep, uint64(7000000000)}); err != nil {
		t.Fatal(err)
	}
	if h.reqOptions[rep].connectTimeout != 3*time.Second || h.reqOptions[rep].firstByteTimeout != 7*time.Second {
		t.Fatalf("timeouts = %v", h.reqOptions[rep])
	}
	// None (nil) leaves it unset (no panic).
	if _, err := setConn(context.Background(), []abi.Value{rep, nil}); err != nil {
		t.Fatal(err)
	}
	_, err = setConn(context.Background(), []abi.Value{rep})
	reqErr(t, err, "expected 2 args")
	_, err = setConn(context.Background(), []abi.Value{"x", uint64(1)})
	reqErr(t, err, "self: expected uint32 rep")
	_, err = setConn(context.Background(), []abi.Value{uint32(999), uint64(1)})
	reqErr(t, err, "does not name a live request-options")
}
