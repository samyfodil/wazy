package instance

// This file implements both sides of the WASI 0.2 wasi:http/proxy world:
//
//   - Server (incoming-handler): a component that EXPORTS
//     wasi:http/incoming-handler receives an HTTP request and writes a
//     response. Unlike the rest of WithWASI (host funcs the guest imports and
//     calls), the incoming-handler is an EXPORT the host calls: serveHTTP
//     synthesizes the incoming-request + response-outparam resources, invokes
//     the guest's `handle`, and reads back whatever the guest set on the
//     outparam. The response body is written through wasi:io/streams'
//     output-stream (the same path stdout uses): outgoing-body.write mints an
//     output-stream rep backed by the body buffer, and writeSink (wasi.go)
//     gains an http fallback so blocking-write-and-flush lands in it.
//
//   - Client (outgoing-handler): a component that IMPORTS
//     wasi:http/outgoing-handler makes an outbound request. handle builds a
//     Go *http.Request from the outgoing-request and dispatches it through the
//     configured http.Client (WASIConfig.HTTPClient). Because Do is
//     synchronous, the future-incoming-response is already resolved -- subscribe
//     returns the shared always-ready pollable, get returns the response
//     immediately. incoming-body.stream reuses the fs input-stream path so the
//     guest's blocking-read of the response body needs no new machinery.
//
// # Scope (ponytail)
//
// Implemented: the wasi:http/types subset a wit-bindgen proxy guest actually
// calls -- request line read (incoming-request.{method, path-with-query}),
// response write (fields, outgoing-response, outgoing-body, response-outparam),
// and the full client path (outgoing-request set-*, outgoing-handler.handle,
// future-incoming-response, incoming-response, incoming-body). Not yet (fail
// loud when reached): request/response header readback on the incoming side,
// incoming-request.consume (request body), trailers, and per-request
// request-options (timeouts).

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/samyfodil/wazy/internal/component/abi"
	"github.com/samyfodil/wazy/internal/component/binary"
)

// Resource type tags for wasi:http/types resources. See resource.go; tags
// 1-13 are already taken by streams/fs/sockets, so http starts at 14.
const (
	wasiHTTPIncomingRequestResType  uint32 = 14
	wasiHTTPFieldsResType           uint32 = 15
	wasiHTTPOutgoingResponseResType uint32 = 16
	wasiHTTPOutgoingBodyResType     uint32 = 17
	wasiHTTPResponseOutparamResType uint32 = 18
	wasiHTTPOutgoingRequestResType  uint32 = 19
	wasiHTTPFutureResType           uint32 = 20
	wasiHTTPIncomingResponseResType uint32 = 21
	wasiHTTPIncomingBodyResType     uint32 = 22
	wasiHTTPRequestOptionsResType   uint32 = 23
)

// Interface names are registered version-tolerantly (mkImportKey strips the
// "@x.y.z"): a guest built against any wasi 0.2.x patch resolves against these.
const (
	wasiIfaceHTTPTypes           = "wasi:http/types@0.2.0"
	wasiIfaceHTTPIncomingHandler = "wasi:http/incoming-handler"
	wasiIfaceHTTPOutgoingHandler = "wasi:http/outgoing-handler@0.2.0"
)

// httpBodyStreamRepBase keeps outgoing-body output-stream reps disjoint from fs
// (reps start at 3) and socket (1<<20) output-stream reps, so writeSink's
// dispatch-by-rep across all three (see writeSink's doc) stays unambiguous.
const httpBodyStreamRepBase uint32 = 1 << 24

// httpMethodCases is the wasi:http/types `method` variant's payload-less cases,
// in discriminant order; index 9 ("other", carrying the method string) follows.
var httpMethodCases = []string{
	"GET", "HEAD", "POST", "PUT", "DELETE", "CONNECT", "OPTIONS", "TRACE", "PATCH",
}

// httpIncomingRequest is the host state behind an incoming-request resource:
// the inbound request serveHTTP synthesized for the guest to read.
type httpIncomingRequest struct {
	method   string // uppercase HTTP method (e.g. "GET")
	pathQ    string // path plus "?"+rawquery, e.g. "/hello?x=1"
	headers  http.Header
	body     []byte
	consumed bool // incoming-request.consume may be called only once
}

// httpFields is the host state behind a fields resource: an ordered,
// duplicate-allowing header multimap (wasi:http fields semantics).
type httpFields struct {
	names  []string
	values [][]byte
}

// httpOutgoingResponse is the host state behind an outgoing-response resource.
type httpOutgoingResponse struct {
	status    uint16
	headers   *httpFields
	body      *httpOutgoingBody
	bodyTaken bool
}

// httpOutgoingBody is the host state behind an outgoing-body resource: the
// accumulating response body plus the output-stream rep it was written through.
type httpOutgoingBody struct {
	buf      bytes.Buffer
	finished bool
}

// httpCapture is the slot a response-outparam names: what the guest set.
type httpCapture struct {
	set     bool
	resp    *httpOutgoingResponse
	isErr   bool
	errDisc uint32
}

// wasiHTTP holds all per-Instance wasi:http server state. Every resource kind
// lives in its own rep->state map (the handle table's typeIdx tag keeps them
// from being confused, so reps need not be globally unique across kinds), plus
// bodyStreams which MUST be globally unique among output-stream reps.
type wasiHTTP struct {
	mu sync.Mutex

	// getResources yields the owning Instance's handle table, set by the
	// resource hook (see withResourcesHook): host funcs that mint a nested
	// own<T> handle need it directly, exactly like wasi_fs.go.
	getResources func() (*handleTable, error)

	nextRep   uint32
	incoming  map[uint32]*httpIncomingRequest
	fields    map[uint32]*httpFields
	responses map[uint32]*httpOutgoingResponse
	bodies    map[uint32]*httpOutgoingBody
	outparams map[uint32]*httpCapture

	nextBodyStream uint32
	bodyStreams    map[uint32]*httpOutgoingBody

	// --- outgoing (client) side ---

	// client is the http.Client outgoing-handler.handle dispatches through.
	// Set from WASIConfig in WithWASI (default http.DefaultClient); a test can
	// inject one whose Transport reaches a scratch backend.
	client *http.Client
	// newInputStreamRep mints an fs-backed input-stream rep over data (see
	// wasi_fs.go's fsStreamNode) so incoming-body.stream reuses the existing
	// [method]input-stream.blocking-read path. Set in WithWASI.
	newInputStreamRep func(data []byte) uint32

	outRequests map[uint32]*httpOutgoingRequest
	futures     map[uint32]*httpFuture
	inResponses map[uint32]*httpIncomingResponse
	inBodies    map[uint32]*httpIncomingBody
	reqOptions  map[uint32]*httpRequestOptions
}

// httpRequestOptions is the host state behind a request-options resource. Only
// the timeouts a real client guest sets are tracked; applied as an overall
// request deadline (Go's http.Client doesn't split connect vs first-byte).
type httpRequestOptions struct {
	connectTimeout   time.Duration // 0 = unset
	firstByteTimeout time.Duration // 0 = unset
}

// httpOutgoingRequest is the host state behind an outgoing-request resource,
// built up by the set-* methods before outgoing-handler.handle sends it.
type httpOutgoingRequest struct {
	method    string // uppercase, default "GET"
	scheme    string // "http"/"https"/other, default "http"
	authority string
	pathQ     string // default "/"
	headers   *httpFields
	// body is set by outgoing-request.body(); its accumulated bytes (written by
	// the guest through the shared output-stream path) become the outbound
	// request body when outgoing-handler.handle sends it. Nil for a bodyless
	// request (e.g. a forwarded GET).
	body      *httpOutgoingBody
	bodyTaken bool
}

// httpFuture is the host state behind a future-incoming-response: the outcome
// of a (synchronous, already-completed) outbound request.
type httpFuture struct {
	respRep uint32 // rep of the incoming-response, if errCode == 0
	errCode uint32 // non-zero -> the request failed with this error-code disc
	taken   bool   // get returns the outcome once, then None
}

// httpIncomingResponse is the host state behind an incoming-response resource.
type httpIncomingResponse struct {
	status  uint16
	headers *httpFields
	body    []byte
	consumed bool
}

// httpIncomingBody is the host state behind an incoming-body resource.
type httpIncomingBody struct {
	body        []byte
	streamTaken bool
}

func newWasiHTTP() *wasiHTTP {
	return &wasiHTTP{
		nextRep:        1,
		incoming:       make(map[uint32]*httpIncomingRequest),
		fields:         make(map[uint32]*httpFields),
		responses:      make(map[uint32]*httpOutgoingResponse),
		bodies:         make(map[uint32]*httpOutgoingBody),
		outparams:      make(map[uint32]*httpCapture),
		nextBodyStream: httpBodyStreamRepBase,
		bodyStreams:    make(map[uint32]*httpOutgoingBody),
		outRequests:    make(map[uint32]*httpOutgoingRequest),
		futures:        make(map[uint32]*httpFuture),
		inResponses:    make(map[uint32]*httpIncomingResponse),
		inBodies:       make(map[uint32]*httpIncomingBody),
		reqOptions:     make(map[uint32]*httpRequestOptions),
	}
}

func (h *wasiHTTP) newIncomingRep(r *httpIncomingRequest) uint32 {
	h.mu.Lock()
	defer h.mu.Unlock()
	rep := h.nextRep
	h.nextRep++
	h.incoming[rep] = r
	return rep
}

func (h *wasiHTTP) newFieldsRep(f *httpFields) uint32 {
	h.mu.Lock()
	defer h.mu.Unlock()
	rep := h.nextRep
	h.nextRep++
	h.fields[rep] = f
	return rep
}

func (h *wasiHTTP) newResponseRep(r *httpOutgoingResponse) uint32 {
	h.mu.Lock()
	defer h.mu.Unlock()
	rep := h.nextRep
	h.nextRep++
	h.responses[rep] = r
	return rep
}

func (h *wasiHTTP) newBodyRep(b *httpOutgoingBody) uint32 {
	h.mu.Lock()
	defer h.mu.Unlock()
	rep := h.nextRep
	h.nextRep++
	h.bodies[rep] = b
	return rep
}

func (h *wasiHTTP) newOutparamRep(c *httpCapture) uint32 {
	h.mu.Lock()
	defer h.mu.Unlock()
	rep := h.nextRep
	h.nextRep++
	h.outparams[rep] = c
	return rep
}

// newBodyStreamRep mints a globally-disjoint output-stream rep naming b's
// buffer, so writeSink can route the guest's writes into it.
func (h *wasiHTTP) newBodyStreamRep(b *httpOutgoingBody) uint32 {
	h.mu.Lock()
	defer h.mu.Unlock()
	rep := h.nextBodyStream
	h.nextBodyStream++
	h.bodyStreams[rep] = b
	return rep
}

// bodyStreamWrite appends buf to the body behind output-stream rep, reporting
// found=false if rep is not an http body stream (so writeSink falls through).
func (h *wasiHTTP) bodyStreamWrite(rep uint32, buf []byte) (found bool, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	b, ok := h.bodyStreams[rep]
	if !ok {
		return false, nil
	}
	if b.finished {
		return true, fmt.Errorf("wasi:http/types: outgoing-body written after finish")
	}
	b.buf.Write(buf)
	return true, nil
}

// isBodyStreamRep reports whether rep names an http outgoing-body output-stream
// (used by writeSink/checkWrite/blockingFlush's dispatch fallback).
func (h *wasiHTTP) isBodyStreamRep(rep uint32) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, ok := h.bodyStreams[rep]
	return ok
}

// ---- host func implementations (wasi:http/types) ----

func (h *wasiHTTP) incomingRequestMethod(_ context.Context, args []abi.Value) ([]abi.Value, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("[method]incoming-request.method: expected 1 arg (self), got %d", len(args))
	}
	rep, ok := args[0].(uint32)
	if !ok {
		return nil, fmt.Errorf("[method]incoming-request.method: self: expected uint32 rep, got %T", args[0])
	}
	h.mu.Lock()
	req, ok := h.incoming[rep]
	h.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("[method]incoming-request.method: rep %d does not name a live incoming-request", rep)
	}
	up := strings.ToUpper(req.method)
	for i, name := range httpMethodCases {
		if name == up {
			return []abi.Value{abi.VariantValue{Disc: uint32(i)}}, nil
		}
	}
	// other(string): discriminant 9, payload the raw method token.
	return []abi.Value{abi.VariantValue{Disc: uint32(len(httpMethodCases)), Payload: req.method}}, nil
}

func (h *wasiHTTP) incomingRequestPathWithQuery(_ context.Context, args []abi.Value) ([]abi.Value, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("[method]incoming-request.path-with-query: expected 1 arg (self), got %d", len(args))
	}
	rep, ok := args[0].(uint32)
	if !ok {
		return nil, fmt.Errorf("[method]incoming-request.path-with-query: self: expected uint32 rep, got %T", args[0])
	}
	h.mu.Lock()
	req, ok := h.incoming[rep]
	h.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("[method]incoming-request.path-with-query: rep %d does not name a live incoming-request", rep)
	}
	// option<string>: Some(path) is the string itself; None is nil.
	return []abi.Value{req.pathQ}, nil
}

// incomingRequestHeaders returns the request's headers as an own<fields>
// (wasi:http/types `headers` = fields). The guest reads them with fields.get.
func (h *wasiHTTP) incomingRequestHeaders(_ context.Context, args []abi.Value) ([]abi.Value, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("[method]incoming-request.headers: expected 1 arg (self), got %d", len(args))
	}
	rep, ok := args[0].(uint32)
	if !ok {
		return nil, fmt.Errorf("[method]incoming-request.headers: self: expected uint32 rep, got %T", args[0])
	}
	h.mu.Lock()
	req, ok := h.incoming[rep]
	h.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("[method]incoming-request.headers: rep %d does not name a live incoming-request", rep)
	}
	f := &httpFields{}
	// http.Header is a map, so its iteration order is non-deterministic; sort by
	// header name (canonical) so fields.get and any entries() are stable. Values
	// within a name keep their order.
	names := make([]string, 0, len(req.headers))
	for name := range req.headers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		for _, v := range req.headers[name] {
			f.names = append(f.names, strings.ToLower(name))
			f.values = append(f.values, []byte(v))
		}
	}
	rep2 := h.newFieldsRep(f)
	// Top-level own<fields> result: allocHandleResult wraps the bare rep.
	return []abi.Value{rep2}, nil
}

// incomingRequestConsume returns the request body as an own<incoming-body>
// (result<own<incoming-body>>). May be called only once. The returned body is
// read via incoming-body.stream + input-stream.blocking-read (shared with the
// outgoing/client path).
func (h *wasiHTTP) incomingRequestConsume(_ context.Context, args []abi.Value) ([]abi.Value, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("[method]incoming-request.consume: expected 1 arg (self), got %d", len(args))
	}
	rep, ok := args[0].(uint32)
	if !ok {
		return nil, fmt.Errorf("[method]incoming-request.consume: self: expected uint32 rep, got %T", args[0])
	}
	res, err := h.getResources()
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	req, ok := h.incoming[rep]
	if !ok {
		h.mu.Unlock()
		return nil, fmt.Errorf("[method]incoming-request.consume: rep %d does not name a live incoming-request", rep)
	}
	if req.consumed {
		h.mu.Unlock()
		// result<own<incoming-body>>: the body can only be taken once.
		return []abi.Value{abi.ResultValue{IsErr: true}}, nil
	}
	req.consumed = true
	body := req.body
	h.mu.Unlock()
	bodyRep := h.newInBodyRep(&httpIncomingBody{body: body})
	handle := res.NewOwn(wasiHTTPIncomingBodyResType, bodyRep)
	return []abi.Value{abi.ResultValue{IsErr: false, Payload: handle}}, nil
}

func (h *wasiHTTP) fieldsGet(_ context.Context, args []abi.Value) ([]abi.Value, error) {
	if len(args) != 2 {
		return nil, fmt.Errorf("[method]fields.get: expected 2 args (self, name), got %d", len(args))
	}
	rep, ok := args[0].(uint32)
	if !ok {
		return nil, fmt.Errorf("[method]fields.get: self: expected uint32 rep, got %T", args[0])
	}
	name, ok := args[1].(string)
	if !ok {
		return nil, fmt.Errorf("[method]fields.get: name: expected string, got %T", args[1])
	}
	h.mu.Lock()
	f, ok := h.fields[rep]
	h.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("[method]fields.get: rep %d does not name a live fields", rep)
	}
	// list<field-value> = list<list<u8>>: every value stored under name (header
	// names compare case-insensitively).
	lname := strings.ToLower(name)
	var out []abi.Value
	for i, n := range f.names {
		if strings.ToLower(n) == lname {
			out = append(out, bytesToU8List(f.values[i]))
		}
	}
	return []abi.Value{out}, nil
}

// bytesToU8List renders b as a lowered list<u8> (each byte a uint32 element).
func bytesToU8List(b []byte) []abi.Value {
	out := make([]abi.Value, len(b))
	for i, x := range b {
		out[i] = uint32(x)
	}
	return out
}

func (h *wasiHTTP) fieldsConstructor(_ context.Context, args []abi.Value) ([]abi.Value, error) {
	if len(args) != 0 {
		return nil, fmt.Errorf("[constructor]fields: expected 0 args, got %d", len(args))
	}
	rep := h.newFieldsRep(&httpFields{})
	// Top-level own<fields> result: allocHandleResult wraps this bare rep into
	// a guest handle under the declared result type's tag.
	return []abi.Value{rep}, nil
}

func (h *wasiHTTP) fieldsSet(_ context.Context, args []abi.Value) ([]abi.Value, error) {
	if len(args) != 3 {
		return nil, fmt.Errorf("[method]fields.set: expected 3 args (self, name, value), got %d", len(args))
	}
	rep, ok := args[0].(uint32)
	if !ok {
		return nil, fmt.Errorf("[method]fields.set: self: expected uint32 rep, got %T", args[0])
	}
	name, ok := args[1].(string)
	if !ok {
		return nil, fmt.Errorf("[method]fields.set: name: expected string, got %T", args[1])
	}
	values, err := httpFieldValues(args[2])
	if err != nil {
		return nil, fmt.Errorf("[method]fields.set: value: %w", err)
	}
	h.mu.Lock()
	f, ok := h.fields[rep]
	if ok {
		// set replaces every existing value for name, then appends the new ones.
		f.dropName(name)
		for _, v := range values {
			f.names = append(f.names, name)
			f.values = append(f.values, v)
		}
	}
	h.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("[method]fields.set: rep %d does not name a live fields", rep)
	}
	// result<_, header-error>: Ok.
	return []abi.Value{abi.ResultValue{IsErr: false, Payload: nil}}, nil
}

func (f *httpFields) dropName(name string) {
	names := f.names[:0]
	values := f.values[:0]
	for i, n := range f.names {
		if n == name {
			continue
		}
		names = append(names, n)
		values = append(values, f.values[i])
	}
	f.names, f.values = names, values
}

func (h *wasiHTTP) outgoingResponseConstructor(_ context.Context, args []abi.Value) ([]abi.Value, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("[constructor]outgoing-response: expected 1 arg (headers), got %d", len(args))
	}
	// headers is own<fields>: liftHostArgs consumes the handle and hands us its
	// rep (TakeOwn), transferring ownership into the response.
	rep, ok := args[0].(uint32)
	if !ok {
		return nil, fmt.Errorf("[constructor]outgoing-response: headers: expected uint32 rep, got %T", args[0])
	}
	h.mu.Lock()
	f := h.fields[rep]
	if f == nil {
		f = &httpFields{}
	}
	delete(h.fields, rep)
	h.mu.Unlock()
	respRep := h.newResponseRep(&httpOutgoingResponse{status: 200, headers: f})
	return []abi.Value{respRep}, nil
}

func (h *wasiHTTP) outgoingResponseSetStatusCode(_ context.Context, args []abi.Value) ([]abi.Value, error) {
	if len(args) != 2 {
		return nil, fmt.Errorf("[method]outgoing-response.set-status-code: expected 2 args (self, status), got %d", len(args))
	}
	rep, ok := args[0].(uint32)
	if !ok {
		return nil, fmt.Errorf("[method]outgoing-response.set-status-code: self: expected uint32 rep, got %T", args[0])
	}
	status, ok := args[1].(uint32)
	if !ok {
		return nil, fmt.Errorf("[method]outgoing-response.set-status-code: status: expected uint32, got %T", args[1])
	}
	h.mu.Lock()
	resp, ok := h.responses[rep]
	if ok {
		resp.status = uint16(status)
	}
	h.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("[method]outgoing-response.set-status-code: rep %d does not name a live outgoing-response", rep)
	}
	return []abi.Value{abi.ResultValue{IsErr: false, Payload: nil}}, nil
}

func (h *wasiHTTP) outgoingResponseBody(_ context.Context, args []abi.Value) ([]abi.Value, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("[method]outgoing-response.body: expected 1 arg (self), got %d", len(args))
	}
	rep, ok := args[0].(uint32)
	if !ok {
		return nil, fmt.Errorf("[method]outgoing-response.body: self: expected uint32 rep, got %T", args[0])
	}
	res, err := h.getResources()
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	resp, ok := h.responses[rep]
	if !ok {
		h.mu.Unlock()
		return nil, fmt.Errorf("[method]outgoing-response.body: rep %d does not name a live outgoing-response", rep)
	}
	if resp.bodyTaken {
		h.mu.Unlock()
		// result<own<outgoing-body>, _>: body can only be taken once.
		return []abi.Value{abi.ResultValue{IsErr: true, Payload: nil}}, nil
	}
	resp.bodyTaken = true
	body := &httpOutgoingBody{}
	resp.body = body
	h.mu.Unlock()
	bodyRep := h.newBodyRep(body)
	handle := res.NewOwn(wasiHTTPOutgoingBodyResType, bodyRep)
	return []abi.Value{abi.ResultValue{IsErr: false, Payload: handle}}, nil
}

func (h *wasiHTTP) outgoingBodyWrite(_ context.Context, args []abi.Value) ([]abi.Value, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("[method]outgoing-body.write: expected 1 arg (self), got %d", len(args))
	}
	rep, ok := args[0].(uint32)
	if !ok {
		return nil, fmt.Errorf("[method]outgoing-body.write: self: expected uint32 rep, got %T", args[0])
	}
	res, err := h.getResources()
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	body, ok := h.bodies[rep]
	h.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("[method]outgoing-body.write: rep %d does not name a live outgoing-body", rep)
	}
	streamRep := h.newBodyStreamRep(body)
	handle := res.NewOwn(wasiOutputStreamResType, streamRep)
	// result<own<output-stream>, _>: Ok.
	return []abi.Value{abi.ResultValue{IsErr: false, Payload: handle}}, nil
}

func (h *wasiHTTP) outgoingBodyFinish(_ context.Context, args []abi.Value) ([]abi.Value, error) {
	if len(args) != 2 {
		return nil, fmt.Errorf("[static]outgoing-body.finish: expected 2 args (this, trailers), got %d", len(args))
	}
	// this: own<outgoing-body> lifted to its rep (ownership consumed).
	rep, ok := args[0].(uint32)
	if !ok {
		return nil, fmt.Errorf("[static]outgoing-body.finish: this: expected uint32 rep, got %T", args[0])
	}
	// trailers: option<own<trailers>>; nil (None) is all a normal guest sends.
	if args[1] != nil {
		return nil, fmt.Errorf("[static]outgoing-body.finish: response trailers are not supported by this milestone")
	}
	h.mu.Lock()
	body, ok := h.bodies[rep]
	if ok {
		body.finished = true
	}
	h.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("[static]outgoing-body.finish: rep %d does not name a live outgoing-body", rep)
	}
	// result<_, error-code>: Ok.
	return []abi.Value{abi.ResultValue{IsErr: false, Payload: nil}}, nil
}

func (h *wasiHTTP) responseOutparamSet(_ context.Context, args []abi.Value) ([]abi.Value, error) {
	if len(args) != 2 {
		return nil, fmt.Errorf("[static]response-outparam.set: expected 2 args (param, response), got %d", len(args))
	}
	// param: own<response-outparam> lifted to its rep (ownership consumed).
	rep, ok := args[0].(uint32)
	if !ok {
		return nil, fmt.Errorf("[static]response-outparam.set: param: expected uint32 rep, got %T", args[0])
	}
	rv, ok := args[1].(abi.ResultValue)
	if !ok {
		return nil, fmt.Errorf("[static]response-outparam.set: response: expected result<outgoing-response, error-code>, got %T", args[1])
	}

	h.mu.Lock()
	cap, ok := h.outparams[rep]
	h.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("[static]response-outparam.set: rep %d does not name a live response-outparam", rep)
	}
	cap.set = true
	if rv.IsErr {
		cap.isErr = true
		if vv, ok := rv.Payload.(abi.VariantValue); ok {
			cap.errDisc = vv.Disc
		}
		return nil, nil
	}

	// The Ok payload is own<outgoing-response> nested inside a result, so the
	// lift leaves it as a live guest handle (not a rep -- only top-level own/
	// borrow args are auto-resolved). Consume the handle to recover the rep the
	// response state is keyed under.
	respHandle, ok := rv.Payload.(uint32)
	if !ok {
		return nil, fmt.Errorf("[static]response-outparam.set: Ok payload: expected outgoing-response handle, got %T", rv.Payload)
	}
	res, err := h.getResources()
	if err != nil {
		return nil, err
	}
	respRep, err := res.TakeOwn(wasiHTTPOutgoingResponseResType, respHandle)
	if err != nil {
		return nil, fmt.Errorf("[static]response-outparam.set: Ok outgoing-response handle: %w", err)
	}
	h.mu.Lock()
	resp := h.responses[respRep]
	delete(h.responses, respRep)
	h.mu.Unlock()
	if resp == nil {
		return nil, fmt.Errorf("[static]response-outparam.set: Ok rep %d does not name a live outgoing-response", respRep)
	}
	cap.resp = resp
	return nil, nil
}

// httpFieldValues coerces a lowered list<field-value> (= list<list<u8>>) arg
// into [][]byte.
func httpFieldValues(v abi.Value) ([][]byte, error) {
	list, ok := v.([]abi.Value)
	if !ok {
		return nil, fmt.Errorf("expected list<list<u8>>, got %T", v)
	}
	out := make([][]byte, 0, len(list))
	for i, elem := range list {
		b, err := wasiBytesFromList(elem)
		if err != nil {
			return nil, fmt.Errorf("element %d: %w", i, err)
		}
		out = append(out, b)
	}
	return out, nil
}

// ---- WIT type descriptors + signatures ----

// httpMethodType interns the wasi:http/types `method` variant into tbl.
func httpMethodType(tbl *typeTable) binary.TypeRef {
	strRef := binary.TypeRef{Primitive: "string"}
	cases := make([]binary.VariantCase, 0, len(httpMethodCases)+1)
	for _, name := range []string{"get", "head", "post", "put", "delete", "connect", "options", "trace", "patch"} {
		cases = append(cases, binary.VariantCase{Name: name})
	}
	cases = append(cases, binary.VariantCase{Name: "other", Type: &strRef})
	return tbl.add(binary.VariantDesc{Cases: cases})
}

// httpHeaderErrorType interns the `header-error` variant into tbl.
func httpHeaderErrorType(tbl *typeTable) binary.TypeRef {
	return tbl.add(binary.VariantDesc{Cases: []binary.VariantCase{
		{Name: "invalid-syntax"}, {Name: "forbidden"}, {Name: "immutable"},
	}})
}

// httpErrorCodeType interns the (large, frozen) wasi:http/types `error-code`
// variant into tbl. Every case is reproduced faithfully so result<_,
// error-code> and result<own<outgoing-response>, error-code> flatten to the
// exact core shape the guest's bindings expect -- even though the incoming
// milestone never actually constructs an error-code value (it always sets Ok).
func httpErrorCodeType(tbl *typeTable) binary.TypeRef {
	optStr := tbl.add(binary.OptionDesc{Element: binary.TypeRef{Primitive: "string"}})
	optU8 := tbl.add(binary.OptionDesc{Element: binary.TypeRef{Primitive: "u8"}})
	optU16 := tbl.add(binary.OptionDesc{Element: binary.TypeRef{Primitive: "u16"}})
	optU32 := tbl.add(binary.OptionDesc{Element: binary.TypeRef{Primitive: "u32"}})
	optU64 := tbl.add(binary.OptionDesc{Element: binary.TypeRef{Primitive: "u64"}})

	dnsErr := tbl.add(binary.RecordDesc{Fields: []binary.RecordField{
		{Name: "rcode", Type: optStr},
		{Name: "info-code", Type: optU16},
	}})
	tlsAlert := tbl.add(binary.RecordDesc{Fields: []binary.RecordField{
		{Name: "alert-id", Type: optU8},
		{Name: "alert-message", Type: optStr},
	}})
	fieldSize := tbl.add(binary.RecordDesc{Fields: []binary.RecordField{
		{Name: "field-name", Type: optStr},
		{Name: "field-size", Type: optU32},
	}})
	optFieldSize := tbl.add(binary.OptionDesc{Element: fieldSize})

	c := func(name string) binary.VariantCase { return binary.VariantCase{Name: name} }
	cp := func(name string, ref binary.TypeRef) binary.VariantCase {
		r := ref
		return binary.VariantCase{Name: name, Type: &r}
	}
	cases := []binary.VariantCase{
		c("DNS-timeout"),
		cp("DNS-error", dnsErr),
		c("destination-not-found"),
		c("destination-unavailable"),
		c("destination-IP-prohibited"),
		c("destination-IP-unroutable"),
		c("connection-refused"),
		c("connection-terminated"),
		c("connection-timeout"),
		c("connection-read-timeout"),
		c("connection-write-timeout"),
		c("connection-limit-reached"),
		c("TLS-protocol-error"),
		c("TLS-certificate-error"),
		cp("TLS-alert-received", tlsAlert),
		c("HTTP-request-denied"),
		c("HTTP-request-length-required"),
		cp("HTTP-request-body-size", optU64),
		c("HTTP-request-method-invalid"),
		c("HTTP-request-URI-invalid"),
		c("HTTP-request-URI-too-long"),
		cp("HTTP-request-header-section-size", optU32),
		cp("HTTP-request-header-size", optFieldSize),
		cp("HTTP-request-trailer-section-size", optU32),
		cp("HTTP-request-trailer-size", fieldSize),
		c("HTTP-response-incomplete"),
		cp("HTTP-response-header-section-size", optU32),
		cp("HTTP-response-header-size", fieldSize),
		cp("HTTP-response-body-size", optU64),
		cp("HTTP-response-trailer-section-size", optU32),
		cp("HTTP-response-trailer-size", fieldSize),
		cp("HTTP-response-transfer-coding", optStr),
		cp("HTTP-response-content-coding", optStr),
		c("HTTP-response-timeout"),
		c("HTTP-upgrade-failed"),
		c("HTTP-protocol-error"),
		c("loop-detected"),
		c("configuration-error"),
		cp("internal-error", optStr),
	}
	return tbl.add(binary.VariantDesc{Cases: cases})
}

func httpMethodSig() (binary.FuncDesc, abi.Resolver) {
	tbl := &typeTable{}
	selfRef := tbl.add(binary.BorrowDesc{ResourceType: wasiHTTPIncomingRequestResType})
	methodRef := httpMethodType(tbl)
	return binary.FuncDesc{
		Params:  []binary.FuncParam{{Name: "self", Type: selfRef}},
		Results: binary.FuncResults{Unnamed: &methodRef},
	}, tbl.resolver()
}

func httpPathWithQuerySig() (binary.FuncDesc, abi.Resolver) {
	tbl := &typeTable{}
	selfRef := tbl.add(binary.BorrowDesc{ResourceType: wasiHTTPIncomingRequestResType})
	optRef := tbl.add(binary.OptionDesc{Element: binary.TypeRef{Primitive: "string"}})
	return binary.FuncDesc{
		Params:  []binary.FuncParam{{Name: "self", Type: selfRef}},
		Results: binary.FuncResults{Unnamed: &optRef},
	}, tbl.resolver()
}

func httpFieldsConstructorSig() (binary.FuncDesc, abi.Resolver) {
	tbl := &typeTable{}
	ownRef := tbl.add(binary.OwnDesc{ResourceType: wasiHTTPFieldsResType})
	return binary.FuncDesc{Results: binary.FuncResults{Unnamed: &ownRef}}, tbl.resolver()
}

func httpIncomingRequestHeadersSig() (binary.FuncDesc, abi.Resolver) {
	tbl := &typeTable{}
	selfRef := tbl.add(binary.BorrowDesc{ResourceType: wasiHTTPIncomingRequestResType})
	ownRef := tbl.add(binary.OwnDesc{ResourceType: wasiHTTPFieldsResType})
	return binary.FuncDesc{
		Params:  []binary.FuncParam{{Name: "self", Type: selfRef}},
		Results: binary.FuncResults{Unnamed: &ownRef},
	}, tbl.resolver()
}

func httpIncomingRequestConsumeSig() (binary.FuncDesc, abi.Resolver) {
	tbl := &typeTable{}
	selfRef := tbl.add(binary.BorrowDesc{ResourceType: wasiHTTPIncomingRequestResType})
	okRef := tbl.add(binary.OwnDesc{ResourceType: wasiHTTPIncomingBodyResType})
	resRef := tbl.add(binary.ResultDesc{Ok: &okRef})
	return binary.FuncDesc{
		Params:  []binary.FuncParam{{Name: "self", Type: selfRef}},
		Results: binary.FuncResults{Unnamed: &resRef},
	}, tbl.resolver()
}

func httpFieldsGetSig() (binary.FuncDesc, abi.Resolver) {
	tbl := &typeTable{}
	selfRef := tbl.add(binary.BorrowDesc{ResourceType: wasiHTTPFieldsResType})
	listRef := tbl.add(binary.ListDesc{Element: tbl.add(binary.ListDesc{Element: binary.TypeRef{Primitive: "u8"}})})
	return binary.FuncDesc{
		Params:  []binary.FuncParam{{Name: "self", Type: selfRef}, {Name: "name", Type: binary.TypeRef{Primitive: "string"}}},
		Results: binary.FuncResults{Unnamed: &listRef},
	}, tbl.resolver()
}

func httpFieldsSetSig() (binary.FuncDesc, abi.Resolver) {
	tbl := &typeTable{}
	selfRef := tbl.add(binary.BorrowDesc{ResourceType: wasiHTTPFieldsResType})
	valueRef := tbl.add(binary.ListDesc{Element: tbl.add(binary.ListDesc{Element: binary.TypeRef{Primitive: "u8"}})})
	errRef := httpHeaderErrorType(tbl)
	resRef := tbl.add(binary.ResultDesc{Err: &errRef})
	return binary.FuncDesc{
		Params: []binary.FuncParam{
			{Name: "self", Type: selfRef},
			{Name: "name", Type: binary.TypeRef{Primitive: "string"}},
			{Name: "value", Type: valueRef},
		},
		Results: binary.FuncResults{Unnamed: &resRef},
	}, tbl.resolver()
}

func httpOutgoingResponseConstructorSig() (binary.FuncDesc, abi.Resolver) {
	tbl := &typeTable{}
	headersRef := tbl.add(binary.OwnDesc{ResourceType: wasiHTTPFieldsResType})
	ownRef := tbl.add(binary.OwnDesc{ResourceType: wasiHTTPOutgoingResponseResType})
	return binary.FuncDesc{
		Params:  []binary.FuncParam{{Name: "headers", Type: headersRef}},
		Results: binary.FuncResults{Unnamed: &ownRef},
	}, tbl.resolver()
}

func httpSetStatusCodeSig() (binary.FuncDesc, abi.Resolver) {
	tbl := &typeTable{}
	selfRef := tbl.add(binary.BorrowDesc{ResourceType: wasiHTTPOutgoingResponseResType})
	resRef := tbl.add(binary.ResultDesc{})
	return binary.FuncDesc{
		Params: []binary.FuncParam{
			{Name: "self", Type: selfRef},
			{Name: "status-code", Type: binary.TypeRef{Primitive: "u16"}},
		},
		Results: binary.FuncResults{Unnamed: &resRef},
	}, tbl.resolver()
}

func httpOutgoingResponseBodySig() (binary.FuncDesc, abi.Resolver) {
	tbl := &typeTable{}
	selfRef := tbl.add(binary.BorrowDesc{ResourceType: wasiHTTPOutgoingResponseResType})
	okRef := tbl.add(binary.OwnDesc{ResourceType: wasiHTTPOutgoingBodyResType})
	resRef := tbl.add(binary.ResultDesc{Ok: &okRef})
	return binary.FuncDesc{
		Params:  []binary.FuncParam{{Name: "self", Type: selfRef}},
		Results: binary.FuncResults{Unnamed: &resRef},
	}, tbl.resolver()
}

func httpOutgoingBodyWriteSig() (binary.FuncDesc, abi.Resolver) {
	tbl := &typeTable{}
	selfRef := tbl.add(binary.BorrowDesc{ResourceType: wasiHTTPOutgoingBodyResType})
	okRef := tbl.add(binary.OwnDesc{ResourceType: wasiOutputStreamResType})
	resRef := tbl.add(binary.ResultDesc{Ok: &okRef})
	return binary.FuncDesc{
		Params:  []binary.FuncParam{{Name: "self", Type: selfRef}},
		Results: binary.FuncResults{Unnamed: &resRef},
	}, tbl.resolver()
}

func httpOutgoingBodyFinishSig() (binary.FuncDesc, abi.Resolver) {
	tbl := &typeTable{}
	thisRef := tbl.add(binary.OwnDesc{ResourceType: wasiHTTPOutgoingBodyResType})
	trailersRef := tbl.add(binary.OptionDesc{Element: tbl.add(binary.OwnDesc{ResourceType: wasiHTTPFieldsResType})})
	errRef := httpErrorCodeType(tbl)
	resRef := tbl.add(binary.ResultDesc{Err: &errRef})
	return binary.FuncDesc{
		Params: []binary.FuncParam{
			{Name: "this", Type: thisRef},
			{Name: "trailers", Type: trailersRef},
		},
		Results: binary.FuncResults{Unnamed: &resRef},
	}, tbl.resolver()
}

func httpResponseOutparamSetSig() (binary.FuncDesc, abi.Resolver) {
	tbl := &typeTable{}
	paramRef := tbl.add(binary.OwnDesc{ResourceType: wasiHTTPResponseOutparamResType})
	okRef := tbl.add(binary.OwnDesc{ResourceType: wasiHTTPOutgoingResponseResType})
	errRef := httpErrorCodeType(tbl)
	respRef := tbl.add(binary.ResultDesc{Ok: &okRef, Err: &errRef})
	return binary.FuncDesc{
		Params: []binary.FuncParam{
			{Name: "param", Type: paramRef},
			{Name: "response", Type: respRef},
		},
	}, tbl.resolver()
}

// wasiHTTPOptions registers the wasi:http/types host functions the incoming
// milestone implements, plus the resource-type tags that map the guest's own
// type indices to wazy's (see withResourceTag).
func wasiHTTPOptions(h *wasiHTTP) []Option {
	methodFD, methodR := httpMethodSig()
	pathFD, pathR := httpPathWithQuerySig()
	fieldsCtorFD, fieldsCtorR := httpFieldsConstructorSig()
	fieldsSetFD, fieldsSetR := httpFieldsSetSig()
	respCtorFD, respCtorR := httpOutgoingResponseConstructorSig()
	statusFD, statusR := httpSetStatusCodeSig()
	bodyFD, bodyR := httpOutgoingResponseBodySig()
	writeFD, writeR := httpOutgoingBodyWriteSig()
	finishFD, finishR := httpOutgoingBodyFinishSig()
	setFD, setR := httpResponseOutparamSetSig()
	reqHeadersFD, reqHeadersR := httpIncomingRequestHeadersSig()
	reqConsumeFD, reqConsumeR := httpIncomingRequestConsumeSig()
	fieldsGetFD, fieldsGetR := httpFieldsGetSig()

	return []Option{
		withResourcesHook(func(t *handleTable) {
			h.getResources = func() (*handleTable, error) { return t, nil }
		}),
		withHTTPHost(h),

		withResourceTag(wasiIfaceHTTPTypes, "incoming-request", wasiHTTPIncomingRequestResType),
		withResourceTag(wasiIfaceHTTPTypes, "fields", wasiHTTPFieldsResType),
		withResourceTag(wasiIfaceHTTPTypes, "outgoing-response", wasiHTTPOutgoingResponseResType),
		withResourceTag(wasiIfaceHTTPTypes, "outgoing-body", wasiHTTPOutgoingBodyResType),
		withResourceTag(wasiIfaceHTTPTypes, "response-outparam", wasiHTTPResponseOutparamResType),

		withImportCustom(wasiIfaceHTTPTypes, "[method]incoming-request.method", h.incomingRequestMethod, methodFD, methodR),
		withImportCustom(wasiIfaceHTTPTypes, "[method]incoming-request.path-with-query", h.incomingRequestPathWithQuery, pathFD, pathR),
		withImportCustom(wasiIfaceHTTPTypes, "[method]incoming-request.headers", h.incomingRequestHeaders, reqHeadersFD, reqHeadersR),
		withImportCustom(wasiIfaceHTTPTypes, "[method]incoming-request.consume", h.incomingRequestConsume, reqConsumeFD, reqConsumeR),
		withImportCustom(wasiIfaceHTTPTypes, "[constructor]fields", h.fieldsConstructor, fieldsCtorFD, fieldsCtorR),
		withImportCustom(wasiIfaceHTTPTypes, "[method]fields.get", h.fieldsGet, fieldsGetFD, fieldsGetR),
		withImportCustom(wasiIfaceHTTPTypes, "[method]fields.set", h.fieldsSet, fieldsSetFD, fieldsSetR),
		withImportCustom(wasiIfaceHTTPTypes, "[constructor]outgoing-response", h.outgoingResponseConstructor, respCtorFD, respCtorR),
		withImportCustom(wasiIfaceHTTPTypes, "[method]outgoing-response.set-status-code", h.outgoingResponseSetStatusCode, statusFD, statusR),
		withImportCustom(wasiIfaceHTTPTypes, "[method]outgoing-response.body", h.outgoingResponseBody, bodyFD, bodyR),
		withImportCustom(wasiIfaceHTTPTypes, "[method]outgoing-body.write", h.outgoingBodyWrite, writeFD, writeR),
		withImportCustom(wasiIfaceHTTPTypes, "[static]outgoing-body.finish", h.outgoingBodyFinish, finishFD, finishR),
		withImportCustom(wasiIfaceHTTPTypes, "[static]response-outparam.set", h.responseOutparamSet, setFD, setR),
	}
}

// ---- driver: call the guest's exported incoming-handler ----

// ServeHTTP drives the guest component's exported wasi:http/incoming-handler
// with r and writes the response the guest produces to w, making an
// EnableHTTP-instantiated component usable as a net/http.Handler. Any failure
// (no http support, guest didn't set a response, guest signaled an error-code)
// is reported as 500.
func (in *Instance) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body []byte
	if r.Body != nil { // a bodyless request (e.g. a bridged GET) leaves Body nil
		b, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read request body: "+err.Error(), http.StatusBadRequest)
			return
		}
		body = b
	}
	status, header, respBody, err := in.serveHTTP(r.Context(), r.Method, r.URL, r.Header, body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for k, vs := range header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(int(status))
	_, _ = w.Write(respBody)
}

// serveHTTP is ServeHTTP's core: it mints the incoming-request +
// response-outparam resources, invokes the guest's exported handle, and reads
// back the response the guest set. Split out (taking already-decomposed request
// parts) so tests can drive one request without a live net/http server.
func (in *Instance) serveHTTP(ctx context.Context, method string, u *url.URL, headers http.Header, reqBody []byte) (status uint16, respHeader http.Header, respBody []byte, err error) {
	if in.httpHost == nil {
		return 0, nil, nil, fmt.Errorf("component/instance: ServeHTTP: instance was not created with WithWASI(WASIConfig{EnableHTTP: true})")
	}
	handlerInstance, ok := in.findExportInstance(wasiIfaceHTTPIncomingHandler)
	if !ok {
		return 0, nil, nil, fmt.Errorf("component/instance: ServeHTTP: component does not export %s", wasiIfaceHTTPIncomingHandler)
	}

	pathQ := u.Path
	if pathQ == "" {
		pathQ = "/"
	}
	if u.RawQuery != "" {
		pathQ += "?" + u.RawQuery
	}
	req := &httpIncomingRequest{method: strings.ToUpper(method), pathQ: pathQ, headers: headers.Clone(), body: reqBody}
	reqRep := in.httpHost.newIncomingRep(req)
	reqHandle := in.resources.NewOwn(wasiHTTPIncomingRequestResType, reqRep)

	capture := &httpCapture{}
	outRep := in.httpHost.newOutparamRep(capture)
	outHandle := in.resources.NewOwn(wasiHTTPResponseOutparamResType, outRep)

	if _, err := in.CallExport(ctx, handlerInstance, "handle", reqHandle, outHandle); err != nil {
		return 0, nil, nil, fmt.Errorf("component/instance: ServeHTTP: guest handle: %w", err)
	}
	if !capture.set {
		return 0, nil, nil, fmt.Errorf("component/instance: ServeHTTP: guest handle returned without setting a response")
	}
	if capture.isErr {
		return 0, nil, nil, fmt.Errorf("component/instance: ServeHTTP: guest set response error-code (discriminant %d)", capture.errDisc)
	}
	resp := capture.resp
	hdr := http.Header{}
	if resp.headers != nil {
		for i, name := range resp.headers.names {
			hdr.Add(name, string(resp.headers.values[i]))
		}
	}
	if resp.body != nil {
		respBody = resp.body.buf.Bytes()
	}
	return resp.status, hdr, respBody, nil
}

// findExportInstance returns the full exported-instance name whose
// version-stripped form equals prefix (e.g. "wasi:http/incoming-handler"),
// tolerating the "@x.y.z" the guest's export carries. ok is false if no such
// export exists.
func (in *Instance) findExportInstance(prefix string) (string, bool) {
	for name := range in.instanceExports {
		versionless := name
		if i := strings.IndexByte(versionless, '@'); i >= 0 {
			versionless = versionless[:i]
		}
		if versionless == prefix {
			return name, true
		}
	}
	return "", false
}

// ================= outgoing (client) side =================

func (h *wasiHTTP) newOutRequestRep(r *httpOutgoingRequest) uint32 {
	h.mu.Lock()
	defer h.mu.Unlock()
	rep := h.nextRep
	h.nextRep++
	h.outRequests[rep] = r
	return rep
}

func (h *wasiHTTP) newFutureRep(f *httpFuture) uint32 {
	h.mu.Lock()
	defer h.mu.Unlock()
	rep := h.nextRep
	h.nextRep++
	h.futures[rep] = f
	return rep
}

func (h *wasiHTTP) newInResponseRep(r *httpIncomingResponse) uint32 {
	h.mu.Lock()
	defer h.mu.Unlock()
	rep := h.nextRep
	h.nextRep++
	h.inResponses[rep] = r
	return rep
}

func (h *wasiHTTP) newInBodyRep(b *httpIncomingBody) uint32 {
	h.mu.Lock()
	defer h.mu.Unlock()
	rep := h.nextRep
	h.nextRep++
	h.inBodies[rep] = b
	return rep
}

func (h *wasiHTTP) outgoingRequestConstructor(_ context.Context, args []abi.Value) ([]abi.Value, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("[constructor]outgoing-request: expected 1 arg (headers), got %d", len(args))
	}
	rep, ok := args[0].(uint32)
	if !ok {
		return nil, fmt.Errorf("[constructor]outgoing-request: headers: expected uint32 rep, got %T", args[0])
	}
	h.mu.Lock()
	f := h.fields[rep]
	if f == nil {
		f = &httpFields{}
	}
	delete(h.fields, rep)
	h.mu.Unlock()
	reqRep := h.newOutRequestRep(&httpOutgoingRequest{method: "GET", scheme: "http", pathQ: "/", headers: f})
	return []abi.Value{reqRep}, nil
}

// outRequest resolves an outgoing-request rep or returns a wrong-rep error.
func (h *wasiHTTP) outRequest(rep uint32, fn string) (*httpOutgoingRequest, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	r, ok := h.outRequests[rep]
	if !ok {
		return nil, fmt.Errorf("%s: rep %d does not name a live outgoing-request", fn, rep)
	}
	return r, nil
}

func (h *wasiHTTP) outgoingRequestSetMethod(_ context.Context, args []abi.Value) ([]abi.Value, error) {
	if len(args) != 2 {
		return nil, fmt.Errorf("[method]outgoing-request.set-method: expected 2 args, got %d", len(args))
	}
	rep, ok := args[0].(uint32)
	if !ok {
		return nil, fmt.Errorf("[method]outgoing-request.set-method: self: expected uint32 rep, got %T", args[0])
	}
	vv, ok := args[1].(abi.VariantValue)
	if !ok {
		return nil, fmt.Errorf("[method]outgoing-request.set-method: method: expected variant, got %T", args[1])
	}
	r, err := h.outRequest(rep, "[method]outgoing-request.set-method")
	if err != nil {
		return nil, err
	}
	if int(vv.Disc) < len(httpMethodCases) {
		r.method = httpMethodCases[vv.Disc]
	} else if s, ok := vv.Payload.(string); ok {
		r.method = strings.ToUpper(s)
	}
	return []abi.Value{abi.ResultValue{IsErr: false}}, nil
}

// optString extracts a lowered option<string> (nil = None, string = Some).
func optString(v abi.Value) (string, bool) {
	s, ok := v.(string)
	return s, ok
}

func (h *wasiHTTP) outgoingRequestSetPathWithQuery(_ context.Context, args []abi.Value) ([]abi.Value, error) {
	rep, err := httpSelfRep(args, "[method]outgoing-request.set-path-with-query")
	if err != nil {
		return nil, err
	}
	r, err := h.outRequest(rep, "[method]outgoing-request.set-path-with-query")
	if err != nil {
		return nil, err
	}
	if s, ok := optString(args[1]); ok {
		r.pathQ = s
	}
	return []abi.Value{abi.ResultValue{IsErr: false}}, nil
}

func (h *wasiHTTP) outgoingRequestSetScheme(_ context.Context, args []abi.Value) ([]abi.Value, error) {
	rep, err := httpSelfRep(args, "[method]outgoing-request.set-scheme")
	if err != nil {
		return nil, err
	}
	r, err := h.outRequest(rep, "[method]outgoing-request.set-scheme")
	if err != nil {
		return nil, err
	}
	if args[1] != nil { // Some(scheme)
		vv, ok := args[1].(abi.VariantValue)
		if !ok {
			return nil, fmt.Errorf("[method]outgoing-request.set-scheme: scheme: expected variant, got %T", args[1])
		}
		switch vv.Disc {
		case 0:
			r.scheme = "http"
		case 1:
			r.scheme = "https"
		default:
			if s, ok := vv.Payload.(string); ok {
				r.scheme = strings.ToLower(s)
			}
		}
	}
	return []abi.Value{abi.ResultValue{IsErr: false}}, nil
}

func (h *wasiHTTP) outgoingRequestSetAuthority(_ context.Context, args []abi.Value) ([]abi.Value, error) {
	rep, err := httpSelfRep(args, "[method]outgoing-request.set-authority")
	if err != nil {
		return nil, err
	}
	r, err := h.outRequest(rep, "[method]outgoing-request.set-authority")
	if err != nil {
		return nil, err
	}
	if s, ok := optString(args[1]); ok {
		r.authority = s
	}
	return []abi.Value{abi.ResultValue{IsErr: false}}, nil
}

// outgoingRequestBody returns an own<outgoing-body> the guest writes the
// outbound request body into (via the shared output-stream path). Its bytes are
// sent as the request body by outgoing-handler.handle. result<own<outgoing-body>>.
func (h *wasiHTTP) outgoingRequestBody(_ context.Context, args []abi.Value) ([]abi.Value, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("[method]outgoing-request.body: expected 1 arg (self), got %d", len(args))
	}
	rep, ok := args[0].(uint32)
	if !ok {
		return nil, fmt.Errorf("[method]outgoing-request.body: self: expected uint32 rep, got %T", args[0])
	}
	res, err := h.getResources()
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	r, ok := h.outRequests[rep]
	if !ok {
		h.mu.Unlock()
		return nil, fmt.Errorf("[method]outgoing-request.body: rep %d does not name a live outgoing-request", rep)
	}
	if r.bodyTaken {
		h.mu.Unlock()
		return []abi.Value{abi.ResultValue{IsErr: true}}, nil // body can only be taken once
	}
	r.bodyTaken = true
	body := &httpOutgoingBody{}
	r.body = body
	h.mu.Unlock()
	bodyRep := h.newBodyRep(body)
	handle := res.NewOwn(wasiHTTPOutgoingBodyResType, bodyRep)
	return []abi.Value{abi.ResultValue{IsErr: false, Payload: handle}}, nil
}

func (h *wasiHTTP) newReqOptionsRep(o *httpRequestOptions) uint32 {
	h.mu.Lock()
	defer h.mu.Unlock()
	rep := h.nextRep
	h.nextRep++
	h.reqOptions[rep] = o
	return rep
}

func (h *wasiHTTP) requestOptionsConstructor(_ context.Context, args []abi.Value) ([]abi.Value, error) {
	if len(args) != 0 {
		return nil, fmt.Errorf("[constructor]request-options: expected 0 args, got %d", len(args))
	}
	rep := h.newReqOptionsRep(&httpRequestOptions{})
	return []abi.Value{rep}, nil // top-level own<request-options>
}

// requestOptionsSetTimeout implements set-connect-timeout / set-first-byte-timeout
// (both self: borrow<request-options>, duration: option<u64 ns> -> result).
func (h *wasiHTTP) requestOptionsSetTimeout(fn string, connect bool) HostFunc {
	return func(_ context.Context, args []abi.Value) ([]abi.Value, error) {
		if len(args) != 2 {
			return nil, fmt.Errorf("%s: expected 2 args (self, duration), got %d", fn, len(args))
		}
		rep, ok := args[0].(uint32)
		if !ok {
			return nil, fmt.Errorf("%s: self: expected uint32 rep, got %T", fn, args[0])
		}
		h.mu.Lock()
		o, ok := h.reqOptions[rep]
		if ok && args[1] != nil { // Some(duration): u64 nanoseconds
			if ns, okd := args[1].(uint64); okd {
				d := time.Duration(ns) //nolint:gosec // ns is a wasm-supplied duration
				if connect {
					o.connectTimeout = d
				} else {
					o.firstByteTimeout = d
				}
			}
		}
		h.mu.Unlock()
		if !ok {
			return nil, fmt.Errorf("%s: rep %d does not name a live request-options", fn, rep)
		}
		return []abi.Value{abi.ResultValue{IsErr: false}}, nil
	}
}

// httpSelfRep validates a (self, arg) 2-arg method whose self is a resource rep.
func httpSelfRep(args []abi.Value, fn string) (uint32, error) {
	if len(args) != 2 {
		return 0, fmt.Errorf("%s: expected 2 args, got %d", fn, len(args))
	}
	rep, ok := args[0].(uint32)
	if !ok {
		return 0, fmt.Errorf("%s: self: expected uint32 rep, got %T", fn, args[0])
	}
	return rep, nil
}

// outgoingHandlerHandle sends the outgoing-request through the host http.Client
// and returns a future-incoming-response (already resolved, since the Do is
// synchronous). result<own<future-incoming-response>, error-code>.
func (h *wasiHTTP) outgoingHandlerHandle(ctx context.Context, args []abi.Value) ([]abi.Value, error) {
	if len(args) != 2 {
		return nil, fmt.Errorf("wasi:http/outgoing-handler.handle: expected 2 args (request, options), got %d", len(args))
	}
	// request: own<outgoing-request> lifted to its rep (ownership consumed).
	reqRep, ok := args[0].(uint32)
	if !ok {
		return nil, fmt.Errorf("wasi:http/outgoing-handler.handle: request: expected uint32 rep, got %T", args[0])
	}
	res, err := h.getResources()
	if err != nil {
		return nil, err
	}

	// args[1] is option<own<request-options>>. Some(handle): consume it and
	// apply its timeout as an overall request deadline.
	var timeout time.Duration
	if args[1] != nil {
		optHandle, ok := args[1].(uint32)
		if !ok {
			return nil, fmt.Errorf("wasi:http/outgoing-handler.handle: options: expected request-options handle, got %T", args[1])
		}
		optRep, err := res.TakeOwn(wasiHTTPRequestOptionsResType, optHandle)
		if err != nil {
			return nil, fmt.Errorf("wasi:http/outgoing-handler.handle: request-options handle: %w", err)
		}
		h.mu.Lock()
		if o := h.reqOptions[optRep]; o != nil {
			// Go's http.Client has no separate connect/first-byte timeout; use
			// the larger as the overall deadline.
			timeout = o.connectTimeout
			if o.firstByteTimeout > timeout {
				timeout = o.firstByteTimeout
			}
		}
		delete(h.reqOptions, optRep)
		h.mu.Unlock()
	}

	h.mu.Lock()
	r, ok := h.outRequests[reqRep]
	delete(h.outRequests, reqRep)
	client := h.client
	h.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("wasi:http/outgoing-handler.handle: request rep %d does not name a live outgoing-request", reqRep)
	}
	if client == nil {
		client = http.DefaultClient
	}

	fut := &httpFuture{}
	pathQ := r.pathQ
	if pathQ == "" {
		pathQ = "/"
	}
	rawURL := r.scheme + "://" + r.authority + pathQ
	var reqBody io.Reader
	if r.body != nil {
		reqBody = bytes.NewReader(r.body.buf.Bytes())
	}
	reqCtx := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		reqCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	hreq, err := http.NewRequestWithContext(reqCtx, r.method, rawURL, reqBody)
	if err != nil {
		// Malformed request: report as HTTP-request-URI-invalid (disc 19).
		fut.errCode = 19
	} else {
		if r.headers != nil {
			for i, name := range r.headers.names {
				hreq.Header.Add(name, string(r.headers.values[i]))
			}
		}
		hresp, derr := client.Do(hreq)
		if derr != nil {
			// Connection failure: connection-refused (disc 6).
			fut.errCode = 6
		} else {
			bodyBytes, _ := io.ReadAll(hresp.Body)
			_ = hresp.Body.Close()
			respHeaders := &httpFields{}
			for name, vs := range hresp.Header {
				for _, v := range vs {
					respHeaders.names = append(respHeaders.names, strings.ToLower(name))
					respHeaders.values = append(respHeaders.values, []byte(v))
				}
			}
			//nolint:gosec // HTTP status codes are always within uint16 range.
			respRep := h.newInResponseRep(&httpIncomingResponse{status: uint16(hresp.StatusCode), headers: respHeaders, body: bodyBytes})
			fut.respRep = respRep
		}
	}
	futRep := h.newFutureRep(fut)
	handle := res.NewOwn(wasiHTTPFutureResType, futRep)
	return []abi.Value{abi.ResultValue{IsErr: false, Payload: handle}}, nil
}

func (h *wasiHTTP) futureSubscribe(_ context.Context, args []abi.Value) ([]abi.Value, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("[method]future-incoming-response.subscribe: expected 1 arg (self), got %d", len(args))
	}
	if _, ok := args[0].(uint32); !ok {
		return nil, fmt.Errorf("[method]future-incoming-response.subscribe: self: expected uint32 rep, got %T", args[0])
	}
	res, err := h.getResources()
	if err != nil {
		return nil, err
	}
	// Every future is already resolved (Do is synchronous), so subscribe hands
	// back the shared always-ready pollable (see wasiPollableRep). Top-level
	// own<pollable> result -> return the handle (this is a nested-free result).
	handle := res.NewOwn(wasiPollableResType, wasiPollableRep)
	return []abi.Value{handle}, nil
}

func (h *wasiHTTP) futureGet(_ context.Context, args []abi.Value) ([]abi.Value, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("[method]future-incoming-response.get: expected 1 arg (self), got %d", len(args))
	}
	rep, ok := args[0].(uint32)
	if !ok {
		return nil, fmt.Errorf("[method]future-incoming-response.get: self: expected uint32 rep, got %T", args[0])
	}
	res, err := h.getResources()
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	fut, ok := h.futures[rep]
	if !ok {
		h.mu.Unlock()
		return nil, fmt.Errorf("[method]future-incoming-response.get: rep %d does not name a live future", rep)
	}
	if fut.taken {
		h.mu.Unlock()
		// option<...>: None -- the outcome has already been retrieved.
		return []abi.Value{nil}, nil
	}
	fut.taken = true
	errCode, respRep := fut.errCode, fut.respRep
	h.mu.Unlock()

	// Shape: option<result<result<incoming-response, error-code>>>. The outer
	// result models "future already retrieved" (Err) -- always Ok here. The
	// inner result carries the incoming-response or the transport error-code.
	var inner abi.ResultValue
	if errCode != 0 {
		inner = abi.ResultValue{IsErr: true, Payload: abi.VariantValue{Disc: errCode}}
	} else {
		handle := res.NewOwn(wasiHTTPIncomingResponseResType, respRep)
		inner = abi.ResultValue{IsErr: false, Payload: handle}
	}
	outer := abi.ResultValue{IsErr: false, Payload: inner}
	return []abi.Value{outer}, nil // Some(outer)
}

func (h *wasiHTTP) incomingResponseStatus(_ context.Context, args []abi.Value) ([]abi.Value, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("[method]incoming-response.status: expected 1 arg (self), got %d", len(args))
	}
	rep, ok := args[0].(uint32)
	if !ok {
		return nil, fmt.Errorf("[method]incoming-response.status: self: expected uint32 rep, got %T", args[0])
	}
	h.mu.Lock()
	r, ok := h.inResponses[rep]
	h.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("[method]incoming-response.status: rep %d does not name a live incoming-response", rep)
	}
	return []abi.Value{uint32(r.status)}, nil
}

func (h *wasiHTTP) incomingResponseConsume(_ context.Context, args []abi.Value) ([]abi.Value, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("[method]incoming-response.consume: expected 1 arg (self), got %d", len(args))
	}
	rep, ok := args[0].(uint32)
	if !ok {
		return nil, fmt.Errorf("[method]incoming-response.consume: self: expected uint32 rep, got %T", args[0])
	}
	res, err := h.getResources()
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	r, ok := h.inResponses[rep]
	if !ok {
		h.mu.Unlock()
		return nil, fmt.Errorf("[method]incoming-response.consume: rep %d does not name a live incoming-response", rep)
	}
	if r.consumed {
		h.mu.Unlock()
		return []abi.Value{abi.ResultValue{IsErr: true}}, nil // body already taken
	}
	r.consumed = true
	body := r.body
	h.mu.Unlock()
	bodyRep := h.newInBodyRep(&httpIncomingBody{body: body})
	handle := res.NewOwn(wasiHTTPIncomingBodyResType, bodyRep)
	return []abi.Value{abi.ResultValue{IsErr: false, Payload: handle}}, nil
}

func (h *wasiHTTP) incomingBodyStream(_ context.Context, args []abi.Value) ([]abi.Value, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("[method]incoming-body.stream: expected 1 arg (self), got %d", len(args))
	}
	rep, ok := args[0].(uint32)
	if !ok {
		return nil, fmt.Errorf("[method]incoming-body.stream: self: expected uint32 rep, got %T", args[0])
	}
	res, err := h.getResources()
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	b, ok := h.inBodies[rep]
	if !ok {
		h.mu.Unlock()
		return nil, fmt.Errorf("[method]incoming-body.stream: rep %d does not name a live incoming-body", rep)
	}
	if b.streamTaken {
		h.mu.Unlock()
		return []abi.Value{abi.ResultValue{IsErr: true}}, nil // stream can only be taken once
	}
	b.streamTaken = true
	body := b.body
	mint := h.newInputStreamRep
	h.mu.Unlock()
	if mint == nil {
		return nil, fmt.Errorf("[method]incoming-body.stream: no input-stream backing configured")
	}
	// Reuse the fs-backed input-stream path: the returned rep is served by the
	// already-registered [method]input-stream.blocking-read (fs.streamRead),
	// including EOF (stream-error::closed) once the guest reads all the bytes.
	streamRep := mint(body)
	handle := res.NewOwn(wasiInputStreamResType, streamRep)
	return []abi.Value{abi.ResultValue{IsErr: false, Payload: handle}}, nil
}

// ---- outgoing WIT type descriptors + signatures ----

// httpSchemeType interns the wasi:http/types `scheme` variant {HTTP, HTTPS,
// other(string)} into tbl.
func httpSchemeType(tbl *typeTable) binary.TypeRef {
	strRef := binary.TypeRef{Primitive: "string"}
	return tbl.add(binary.VariantDesc{Cases: []binary.VariantCase{
		{Name: "HTTP"}, {Name: "HTTPS"}, {Name: "other", Type: &strRef},
	}})
}

func httpOutgoingRequestConstructorSig() (binary.FuncDesc, abi.Resolver) {
	tbl := &typeTable{}
	headersRef := tbl.add(binary.OwnDesc{ResourceType: wasiHTTPFieldsResType})
	ownRef := tbl.add(binary.OwnDesc{ResourceType: wasiHTTPOutgoingRequestResType})
	return binary.FuncDesc{
		Params:  []binary.FuncParam{{Name: "headers", Type: headersRef}},
		Results: binary.FuncResults{Unnamed: &ownRef},
	}, tbl.resolver()
}

func httpOutgoingRequestBodySig() (binary.FuncDesc, abi.Resolver) {
	tbl := &typeTable{}
	selfRef := tbl.add(binary.BorrowDesc{ResourceType: wasiHTTPOutgoingRequestResType})
	okRef := tbl.add(binary.OwnDesc{ResourceType: wasiHTTPOutgoingBodyResType})
	resRef := tbl.add(binary.ResultDesc{Ok: &okRef})
	return binary.FuncDesc{
		Params:  []binary.FuncParam{{Name: "self", Type: selfRef}},
		Results: binary.FuncResults{Unnamed: &resRef},
	}, tbl.resolver()
}

func httpRequestOptionsConstructorSig() (binary.FuncDesc, abi.Resolver) {
	tbl := &typeTable{}
	ownRef := tbl.add(binary.OwnDesc{ResourceType: wasiHTTPRequestOptionsResType})
	return binary.FuncDesc{Results: binary.FuncResults{Unnamed: &ownRef}}, tbl.resolver()
}

// httpSetTimeoutSig: (self: borrow<request-options>, duration: option<u64 ns>) -> result.
func httpSetTimeoutSig() (binary.FuncDesc, abi.Resolver) {
	tbl := &typeTable{}
	selfRef := tbl.add(binary.BorrowDesc{ResourceType: wasiHTTPRequestOptionsResType})
	durRef := tbl.add(binary.OptionDesc{Element: binary.TypeRef{Primitive: "u64"}})
	resRef := tbl.add(binary.ResultDesc{})
	return binary.FuncDesc{
		Params:  []binary.FuncParam{{Name: "self", Type: selfRef}, {Name: "duration", Type: durRef}},
		Results: binary.FuncResults{Unnamed: &resRef},
	}, tbl.resolver()
}

func httpSetMethodSig() (binary.FuncDesc, abi.Resolver) {
	tbl := &typeTable{}
	selfRef := tbl.add(binary.BorrowDesc{ResourceType: wasiHTTPOutgoingRequestResType})
	methodRef := httpMethodType(tbl)
	resRef := tbl.add(binary.ResultDesc{})
	return binary.FuncDesc{
		Params:  []binary.FuncParam{{Name: "self", Type: selfRef}, {Name: "method", Type: methodRef}},
		Results: binary.FuncResults{Unnamed: &resRef},
	}, tbl.resolver()
}

// httpSetOptStringSig builds set-path-with-query / set-authority: (self:
// borrow<outgoing-request>, v: option<string>) -> result.
func httpSetOptStringSig() (binary.FuncDesc, abi.Resolver) {
	tbl := &typeTable{}
	selfRef := tbl.add(binary.BorrowDesc{ResourceType: wasiHTTPOutgoingRequestResType})
	optRef := tbl.add(binary.OptionDesc{Element: binary.TypeRef{Primitive: "string"}})
	resRef := tbl.add(binary.ResultDesc{})
	return binary.FuncDesc{
		Params:  []binary.FuncParam{{Name: "self", Type: selfRef}, {Name: "v", Type: optRef}},
		Results: binary.FuncResults{Unnamed: &resRef},
	}, tbl.resolver()
}

func httpSetSchemeSig() (binary.FuncDesc, abi.Resolver) {
	tbl := &typeTable{}
	selfRef := tbl.add(binary.BorrowDesc{ResourceType: wasiHTTPOutgoingRequestResType})
	optRef := tbl.add(binary.OptionDesc{Element: httpSchemeType(tbl)})
	resRef := tbl.add(binary.ResultDesc{})
	return binary.FuncDesc{
		Params:  []binary.FuncParam{{Name: "self", Type: selfRef}, {Name: "scheme", Type: optRef}},
		Results: binary.FuncResults{Unnamed: &resRef},
	}, tbl.resolver()
}

func httpOutgoingHandlerSig() (binary.FuncDesc, abi.Resolver) {
	tbl := &typeTable{}
	reqRef := tbl.add(binary.OwnDesc{ResourceType: wasiHTTPOutgoingRequestResType})
	optRef := tbl.add(binary.OptionDesc{Element: tbl.add(binary.OwnDesc{ResourceType: wasiHTTPRequestOptionsResType})})
	okRef := tbl.add(binary.OwnDesc{ResourceType: wasiHTTPFutureResType})
	errRef := httpErrorCodeType(tbl)
	resRef := tbl.add(binary.ResultDesc{Ok: &okRef, Err: &errRef})
	return binary.FuncDesc{
		Params:  []binary.FuncParam{{Name: "request", Type: reqRef}, {Name: "options", Type: optRef}},
		Results: binary.FuncResults{Unnamed: &resRef},
	}, tbl.resolver()
}

func httpFutureGetSig() (binary.FuncDesc, abi.Resolver) {
	tbl := &typeTable{}
	selfRef := tbl.add(binary.BorrowDesc{ResourceType: wasiHTTPFutureResType})
	respRef := tbl.add(binary.OwnDesc{ResourceType: wasiHTTPIncomingResponseResType})
	errRef := httpErrorCodeType(tbl)
	innerRef := tbl.add(binary.ResultDesc{Ok: &respRef, Err: &errRef})
	outerRef := tbl.add(binary.ResultDesc{Ok: &innerRef})
	optRef := tbl.add(binary.OptionDesc{Element: outerRef})
	return binary.FuncDesc{
		Params:  []binary.FuncParam{{Name: "self", Type: selfRef}},
		Results: binary.FuncResults{Unnamed: &optRef},
	}, tbl.resolver()
}

func httpIncomingResponseStatusSig() (binary.FuncDesc, abi.Resolver) {
	tbl := &typeTable{}
	selfRef := tbl.add(binary.BorrowDesc{ResourceType: wasiHTTPIncomingResponseResType})
	statusRef := binary.TypeRef{Primitive: "u16"}
	return binary.FuncDesc{
		Params:  []binary.FuncParam{{Name: "self", Type: selfRef}},
		Results: binary.FuncResults{Unnamed: &statusRef},
	}, tbl.resolver()
}

func httpIncomingResponseConsumeSig() (binary.FuncDesc, abi.Resolver) {
	tbl := &typeTable{}
	selfRef := tbl.add(binary.BorrowDesc{ResourceType: wasiHTTPIncomingResponseResType})
	okRef := tbl.add(binary.OwnDesc{ResourceType: wasiHTTPIncomingBodyResType})
	resRef := tbl.add(binary.ResultDesc{Ok: &okRef})
	return binary.FuncDesc{
		Params:  []binary.FuncParam{{Name: "self", Type: selfRef}},
		Results: binary.FuncResults{Unnamed: &resRef},
	}, tbl.resolver()
}

func httpIncomingBodyStreamSig() (binary.FuncDesc, abi.Resolver) {
	tbl := &typeTable{}
	selfRef := tbl.add(binary.BorrowDesc{ResourceType: wasiHTTPIncomingBodyResType})
	okRef := tbl.add(binary.OwnDesc{ResourceType: wasiInputStreamResType})
	resRef := tbl.add(binary.ResultDesc{Ok: &okRef})
	return binary.FuncDesc{
		Params:  []binary.FuncParam{{Name: "self", Type: selfRef}},
		Results: binary.FuncResults{Unnamed: &resRef},
	}, tbl.resolver()
}

// wasiHTTPOutgoingOptions registers the client-side (outgoing-handler) host
// funcs plus the wasi:io/poll pollable.block/poll a synchronous future still
// makes the guest call. Registered only when EnableHTTP; the pollable funcs are
// no-ops here (every future this package mints is already resolved), matching
// the always-ready model wasi_sockets.go uses.
func wasiHTTPOutgoingOptions(h *wasiHTTP) []Option {
	reqCtorFD, reqCtorR := httpOutgoingRequestConstructorSig()
	methodFD, methodR := httpSetMethodSig()
	pathFD, pathR := httpSetOptStringSig()
	authFD, authR := httpSetOptStringSig()
	schemeFD, schemeR := httpSetSchemeSig()
	handleFD, handleR := httpOutgoingHandlerSig()
	subFD, subR := wasiSubscribeSig(wasiHTTPFutureResType)
	getFD, getR := httpFutureGetSig()
	statusFD, statusR := httpIncomingResponseStatusSig()
	consumeFD, consumeR := httpIncomingResponseConsumeSig()
	streamFD, streamR := httpIncomingBodyStreamSig()
	reqBodyFD, reqBodyR := httpOutgoingRequestBodySig()
	optCtorFD, optCtorR := httpRequestOptionsConstructorSig()
	setTimeoutFD, setTimeoutR := httpSetTimeoutSig()

	return []Option{
		withResourceTag(wasiIfaceHTTPTypes, "outgoing-request", wasiHTTPOutgoingRequestResType),
		withResourceTag(wasiIfaceHTTPTypes, "future-incoming-response", wasiHTTPFutureResType),
		withResourceTag(wasiIfaceHTTPTypes, "incoming-response", wasiHTTPIncomingResponseResType),
		withResourceTag(wasiIfaceHTTPTypes, "incoming-body", wasiHTTPIncomingBodyResType),
		withResourceTag(wasiIfaceHTTPTypes, "request-options", wasiHTTPRequestOptionsResType),
		// (The pollable tag + block/poll are registered centrally, see wasi_poll.go.)

		withImportCustom(wasiIfaceHTTPTypes, "[constructor]outgoing-request", h.outgoingRequestConstructor, reqCtorFD, reqCtorR),
		withImportCustom(wasiIfaceHTTPTypes, "[method]outgoing-request.body", h.outgoingRequestBody, reqBodyFD, reqBodyR),
		withImportCustom(wasiIfaceHTTPTypes, "[constructor]request-options", h.requestOptionsConstructor, optCtorFD, optCtorR),
		withImportCustom(wasiIfaceHTTPTypes, "[method]request-options.set-connect-timeout", h.requestOptionsSetTimeout("[method]request-options.set-connect-timeout", true), setTimeoutFD, setTimeoutR),
		withImportCustom(wasiIfaceHTTPTypes, "[method]request-options.set-first-byte-timeout", h.requestOptionsSetTimeout("[method]request-options.set-first-byte-timeout", false), setTimeoutFD, setTimeoutR),
		withImportCustom(wasiIfaceHTTPTypes, "[method]outgoing-request.set-method", h.outgoingRequestSetMethod, methodFD, methodR),
		withImportCustom(wasiIfaceHTTPTypes, "[method]outgoing-request.set-path-with-query", h.outgoingRequestSetPathWithQuery, pathFD, pathR),
		withImportCustom(wasiIfaceHTTPTypes, "[method]outgoing-request.set-scheme", h.outgoingRequestSetScheme, schemeFD, schemeR),
		withImportCustom(wasiIfaceHTTPTypes, "[method]outgoing-request.set-authority", h.outgoingRequestSetAuthority, authFD, authR),
		withImportCustom(wasiIfaceHTTPOutgoingHandler, "handle", h.outgoingHandlerHandle, handleFD, handleR),
		withImportCustom(wasiIfaceHTTPTypes, "[method]future-incoming-response.subscribe", h.futureSubscribe, subFD, subR),
		withImportCustom(wasiIfaceHTTPTypes, "[method]future-incoming-response.get", h.futureGet, getFD, getR),
		withImportCustom(wasiIfaceHTTPTypes, "[method]incoming-response.status", h.incomingResponseStatus, statusFD, statusR),
		withImportCustom(wasiIfaceHTTPTypes, "[method]incoming-response.consume", h.incomingResponseConsume, consumeFD, consumeR),
		withImportCustom(wasiIfaceHTTPTypes, "[method]incoming-body.stream", h.incomingBodyStream, streamFD, streamR),
	}
}

