package wasip2

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
	"github.com/samyfodil/wazy/component"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
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
	wasiHTTPFutureTrailersResType   uint32 = 24
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
	trailers http.Header // request trailers (r.Trailer), read via future-trailers
	consumed bool        // incoming-request.consume may be called only once
}

// httpFields is the host state behind a fields resource: an ordered,
// duplicate-allowing header multimap (wasi:http fields semantics).
type httpFields struct {
	names  []string
	values [][]byte

	// immutable marks a fields the guest may read but not modify. types.wit
	// specifies the headers/trailers reachable from an *incoming* message
	// this way ("a child of the incoming-response ... immutable"), and gives
	// header-error a dedicated `immutable` case to report an attempted
	// write. A fields the guest constructed itself is always mutable.
	immutable bool
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
	// trailers holds the option<trailers> a guest passes to
	// outgoing-body.finish (nil when it finishes with None, the common case).
	trailers *httpFields
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
	getResources func() (*component.HandleTable, error)

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

	outRequests    map[uint32]*httpOutgoingRequest
	futures        map[uint32]*httpFuture
	inResponses    map[uint32]*httpIncomingResponse
	inBodies       map[uint32]*httpIncomingBody
	reqOptions     map[uint32]*httpRequestOptions
	futureTrailers map[uint32]*httpFutureTrailers
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
	status   uint16
	headers  *httpFields
	body     []byte
	consumed bool
}

// httpIncomingBody is the host state behind an incoming-body resource.
type httpIncomingBody struct {
	body        []byte
	streamTaken bool
	trailers    http.Header // carried from the request, read via future-trailers
}

// httpFutureTrailers is the host state behind a future-trailers resource
// (incoming-body.finish's result): the trailers are already resolved
// synchronously (this host does no real async I/O), so get returns them
// immediately. taken guards get against being called more than once.
type httpFutureTrailers struct {
	trailers http.Header
	taken    bool
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
		futureTrailers: make(map[uint32]*httpFutureTrailers),
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

func (h *wasiHTTP) incomingRequestMethod(_ context.Context, args []component.Value) ([]component.Value, error) {
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
			return []component.Value{component.VariantValue{Disc: uint32(i)}}, nil
		}
	}
	// other(string): discriminant 9, payload the raw method token.
	return []component.Value{component.VariantValue{Disc: uint32(len(httpMethodCases)), Payload: req.method}}, nil
}

func (h *wasiHTTP) incomingRequestPathWithQuery(_ context.Context, args []component.Value) ([]component.Value, error) {
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
	return []component.Value{req.pathQ}, nil
}

// incomingRequestHeaders returns the request's headers as an own<fields>
// (wasi:http/types `headers` = fields). The guest reads them with fields.get.
func (h *wasiHTTP) incomingRequestHeaders(_ context.Context, args []component.Value) ([]component.Value, error) {
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
	return []component.Value{rep2}, nil
}

// incomingRequestConsume returns the request body as an own<incoming-body>
// (result<own<incoming-body>>). May be called only once. The returned body is
// read via incoming-body.stream + input-stream.blocking-read (shared with the
// outgoing/client path).
func (h *wasiHTTP) incomingRequestConsume(_ context.Context, args []component.Value) ([]component.Value, error) {
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
		return []component.Value{component.ResultValue{IsErr: true}}, nil
	}
	req.consumed = true
	body := req.body
	trailers := req.trailers
	h.mu.Unlock()
	bodyRep := h.newInBodyRep(&httpIncomingBody{body: body, trailers: trailers})
	handle := res.NewOwn(wasiHTTPIncomingBodyResType, bodyRep)
	return []component.Value{component.ResultValue{IsErr: false, Payload: handle}}, nil
}

func (h *wasiHTTP) fieldsGet(_ context.Context, args []component.Value) ([]component.Value, error) {
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
	var out []component.Value
	for i, n := range f.names {
		if strings.ToLower(n) == lname {
			out = append(out, bytesToU8List(f.values[i]))
		}
	}
	return []component.Value{out}, nil
}

// fieldsEntries implements [method]fields.entries -> list<tuple<field-key,
// field-value>>. Every stored (name, value) pair is one entry, duplicates
// included: types.wit's `entries` reports the wire shape, so a header that
// appeared three times yields three entries rather than one joined value.
// Collapsing them is the caller's decision (and loses information -- Set-Cookie
// must never be joined), so this does not make it here.
//
// ponytail: cost is linear in the TOTAL header bytes, with a ~16x memory
// amplification, because component.Value renders list<u8> as []component.Value (one
// interface word per byte -- see component.Value's doc, and bytesToU8List, which
// fields.get has always paid the same way). Measured: 8 headers x 32B is
// ~10us/5KB, while a pathological 40 x 4096B is ~4.5ms/2.6MB. Real responses
// sit at the low end (servers cap total headers around 8-16KB), so this is
// left alone; a cheaper list<u8> would be an component.Value-wide representation
// change, not a fix here.
func (h *wasiHTTP) fieldsEntries(_ context.Context, args []component.Value) ([]component.Value, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("[method]fields.entries: expected 1 arg (self), got %d", len(args))
	}
	rep, ok := args[0].(uint32)
	if !ok {
		return nil, fmt.Errorf("[method]fields.entries: self: expected uint32 rep, got %T", args[0])
	}
	h.mu.Lock()
	f, ok := h.fields[rep]
	var out []component.Value
	if ok {
		out = make([]component.Value, 0, len(f.names))
		for i, n := range f.names {
			// tuple<field-key, field-value> -> a 2-element []component.Value.
			out = append(out, []component.Value{n, bytesToU8List(f.values[i])})
		}
	}
	h.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("[method]fields.entries: rep %d does not name a live fields", rep)
	}
	return []component.Value{out}, nil
}

// bytesToU8List renders b as a lowered list<u8>. The abi package lowers a
// raw []byte for list<u8> with a single copy (byteListValue), so the bytes
// are handed over as-is rather than boxed one interface per byte.
func bytesToU8List(b []byte) component.Value {
	return b
}

func (h *wasiHTTP) fieldsConstructor(_ context.Context, args []component.Value) ([]component.Value, error) {
	if len(args) != 0 {
		return nil, fmt.Errorf("[constructor]fields: expected 0 args, got %d", len(args))
	}
	rep := h.newFieldsRep(&httpFields{})
	// Top-level own<fields> result: allocHandleResult wraps this bare rep into
	// a guest handle under the declared result type's tag.
	return []component.Value{rep}, nil
}

func (h *wasiHTTP) fieldsSet(_ context.Context, args []component.Value) ([]component.Value, error) {
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
	immutable := ok && f.immutable
	if ok && !immutable {
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
	if immutable {
		// header-error::immutable -- a fields borrowed from an incoming
		// message is read-only, and types.wit asks for this exact case
		// rather than a silent no-op or a trap.
		return []component.Value{component.ResultValue{IsErr: true, Payload: component.VariantValue{Disc: httpHeaderErrorImmutable}}}, nil
	}
	// result<_, header-error>: Ok.
	return []component.Value{component.ResultValue{IsErr: false, Payload: nil}}, nil
}

// dropName removes every entry whose name matches, compared case-
// INSENSITIVELY: types.wit specifies that "field names should always be
// treated as case insensitive by the `fields` resource for the purposes of
// equality checking", so a set of "Content-Type" must replace an existing
// "content-type" rather than leave a duplicate behind. fieldsGet already
// compares this way.
func (f *httpFields) dropName(name string) {
	lname := strings.ToLower(name)
	names := f.names[:0]
	values := f.values[:0]
	for i, n := range f.names {
		if strings.ToLower(n) == lname {
			continue
		}
		names = append(names, n)
		values = append(values, f.values[i])
	}
	f.names, f.values = names, values
}

func (h *wasiHTTP) outgoingResponseConstructor(_ context.Context, args []component.Value) ([]component.Value, error) {
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
	return []component.Value{respRep}, nil
}

func (h *wasiHTTP) outgoingResponseSetStatusCode(_ context.Context, args []component.Value) ([]component.Value, error) {
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
	return []component.Value{component.ResultValue{IsErr: false, Payload: nil}}, nil
}

func (h *wasiHTTP) outgoingResponseBody(_ context.Context, args []component.Value) ([]component.Value, error) {
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
		return []component.Value{component.ResultValue{IsErr: true, Payload: nil}}, nil
	}
	resp.bodyTaken = true
	body := &httpOutgoingBody{}
	resp.body = body
	h.mu.Unlock()
	bodyRep := h.newBodyRep(body)
	handle := res.NewOwn(wasiHTTPOutgoingBodyResType, bodyRep)
	return []component.Value{component.ResultValue{IsErr: false, Payload: handle}}, nil
}

func (h *wasiHTTP) outgoingBodyWrite(_ context.Context, args []component.Value) ([]component.Value, error) {
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
	return []component.Value{component.ResultValue{IsErr: false, Payload: handle}}, nil
}

func (h *wasiHTTP) outgoingBodyFinish(_ context.Context, args []component.Value) ([]component.Value, error) {
	if len(args) != 2 {
		return nil, fmt.Errorf("[static]outgoing-body.finish: expected 2 args (this, trailers), got %d", len(args))
	}
	// this: own<outgoing-body> lifted to its rep (ownership consumed).
	rep, ok := args[0].(uint32)
	if !ok {
		return nil, fmt.Errorf("[static]outgoing-body.finish: this: expected uint32 rep, got %T", args[0])
	}
	// trailers: option<own<trailers>>. None (nil) is the common case; Some
	// carries an own<fields> handle nested inside the option, which
	// liftHostArgs does NOT auto-resolve (only top-level own/borrow args are),
	// so it arrives as a handle to TakeOwn here -- mirroring
	// response-outparam.set's own nested-own handling.
	var trailers *httpFields
	if args[1] != nil {
		handle, ok := args[1].(uint32)
		if !ok {
			return nil, fmt.Errorf("[static]outgoing-body.finish: trailers: expected uint32 handle, got %T", args[1])
		}
		res, err := h.getResources()
		if err != nil {
			return nil, err
		}
		trailerRep, err := res.TakeOwn(wasiHTTPFieldsResType, handle)
		if err != nil {
			return nil, fmt.Errorf("[static]outgoing-body.finish: trailers: %w", err)
		}
		h.mu.Lock()
		trailers = h.fields[trailerRep]
		delete(h.fields, trailerRep)
		h.mu.Unlock()
	}
	h.mu.Lock()
	body, ok := h.bodies[rep]
	if ok {
		body.finished = true
		body.trailers = trailers
	}
	h.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("[static]outgoing-body.finish: rep %d does not name a live outgoing-body", rep)
	}
	// result<_, error-code>: Ok.
	return []component.Value{component.ResultValue{IsErr: false, Payload: nil}}, nil
}

func (h *wasiHTTP) responseOutparamSet(_ context.Context, args []component.Value) ([]component.Value, error) {
	if len(args) != 2 {
		return nil, fmt.Errorf("[static]response-outparam.set: expected 2 args (param, response), got %d", len(args))
	}
	// param: own<response-outparam> lifted to its rep (ownership consumed).
	rep, ok := args[0].(uint32)
	if !ok {
		return nil, fmt.Errorf("[static]response-outparam.set: param: expected uint32 rep, got %T", args[0])
	}
	rv, ok := args[1].(component.ResultValue)
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
		if vv, ok := rv.Payload.(component.VariantValue); ok {
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
func httpFieldValues(v component.Value) ([][]byte, error) {
	list, ok := v.([]component.Value)
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
func httpMethodType(tbl *typeTable) component.TypeRef {
	strRef := component.TypeRef{Primitive: "string"}
	cases := make([]component.VariantCase, 0, len(httpMethodCases)+1)
	for _, name := range []string{"get", "head", "post", "put", "delete", "connect", "options", "trace", "patch"} {
		cases = append(cases, component.VariantCase{Name: name})
	}
	cases = append(cases, component.VariantCase{Name: "other", Type: &strRef})
	return tbl.add(component.VariantDesc{Cases: cases})
}

// header-error variant case indices, in types.wit declaration order.
const httpHeaderErrorImmutable uint32 = 2

// httpHeaderErrorType interns the `header-error` variant into tbl.
func httpHeaderErrorType(tbl *typeTable) component.TypeRef {
	return tbl.add(component.VariantDesc{Cases: []component.VariantCase{
		{Name: "invalid-syntax"}, {Name: "forbidden"}, {Name: "immutable"},
	}})
}

// httpErrorCodeType interns the (large, frozen) wasi:http/types `error-code`
// variant into tbl. Every case is reproduced faithfully so result<_,
// error-code> and result<own<outgoing-response>, error-code> flatten to the
// exact core shape the guest's bindings expect -- even though the incoming
// milestone never actually constructs an error-code value (it always sets Ok).
func httpErrorCodeType(tbl *typeTable) component.TypeRef {
	optStr := tbl.add(component.OptionDesc{Element: component.TypeRef{Primitive: "string"}})
	optU8 := tbl.add(component.OptionDesc{Element: component.TypeRef{Primitive: "u8"}})
	optU16 := tbl.add(component.OptionDesc{Element: component.TypeRef{Primitive: "u16"}})
	optU32 := tbl.add(component.OptionDesc{Element: component.TypeRef{Primitive: "u32"}})
	optU64 := tbl.add(component.OptionDesc{Element: component.TypeRef{Primitive: "u64"}})

	dnsErr := tbl.add(component.RecordDesc{Fields: []component.RecordField{
		{Name: "rcode", Type: optStr},
		{Name: "info-code", Type: optU16},
	}})
	tlsAlert := tbl.add(component.RecordDesc{Fields: []component.RecordField{
		{Name: "alert-id", Type: optU8},
		{Name: "alert-message", Type: optStr},
	}})
	fieldSize := tbl.add(component.RecordDesc{Fields: []component.RecordField{
		{Name: "field-name", Type: optStr},
		{Name: "field-size", Type: optU32},
	}})
	optFieldSize := tbl.add(component.OptionDesc{Element: fieldSize})

	c := func(name string) component.VariantCase { return component.VariantCase{Name: name} }
	cp := func(name string, ref component.TypeRef) component.VariantCase {
		r := ref
		return component.VariantCase{Name: name, Type: &r}
	}
	cases := []component.VariantCase{
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
	return tbl.add(component.VariantDesc{Cases: cases})
}

func httpMethodSig() (component.FuncDesc, component.Resolver) {
	tbl := &typeTable{}
	selfRef := tbl.add(component.BorrowDesc{ResourceType: wasiHTTPIncomingRequestResType})
	methodRef := httpMethodType(tbl)
	return component.FuncDesc{
		Params:  []component.FuncParam{{Name: "self", Type: selfRef}},
		Results: component.FuncResults{Unnamed: &methodRef},
	}, tbl.resolver()
}

func httpPathWithQuerySig() (component.FuncDesc, component.Resolver) {
	tbl := &typeTable{}
	selfRef := tbl.add(component.BorrowDesc{ResourceType: wasiHTTPIncomingRequestResType})
	optRef := tbl.add(component.OptionDesc{Element: component.TypeRef{Primitive: "string"}})
	return component.FuncDesc{
		Params:  []component.FuncParam{{Name: "self", Type: selfRef}},
		Results: component.FuncResults{Unnamed: &optRef},
	}, tbl.resolver()
}

func httpFieldsConstructorSig() (component.FuncDesc, component.Resolver) {
	tbl := &typeTable{}
	ownRef := tbl.add(component.OwnDesc{ResourceType: wasiHTTPFieldsResType})
	return component.FuncDesc{Results: component.FuncResults{Unnamed: &ownRef}}, tbl.resolver()
}

func httpIncomingRequestHeadersSig() (component.FuncDesc, component.Resolver) {
	tbl := &typeTable{}
	selfRef := tbl.add(component.BorrowDesc{ResourceType: wasiHTTPIncomingRequestResType})
	ownRef := tbl.add(component.OwnDesc{ResourceType: wasiHTTPFieldsResType})
	return component.FuncDesc{
		Params:  []component.FuncParam{{Name: "self", Type: selfRef}},
		Results: component.FuncResults{Unnamed: &ownRef},
	}, tbl.resolver()
}

func httpIncomingRequestConsumeSig() (component.FuncDesc, component.Resolver) {
	tbl := &typeTable{}
	selfRef := tbl.add(component.BorrowDesc{ResourceType: wasiHTTPIncomingRequestResType})
	okRef := tbl.add(component.OwnDesc{ResourceType: wasiHTTPIncomingBodyResType})
	resRef := tbl.add(component.ResultDesc{Ok: &okRef})
	return component.FuncDesc{
		Params:  []component.FuncParam{{Name: "self", Type: selfRef}},
		Results: component.FuncResults{Unnamed: &resRef},
	}, tbl.resolver()
}

func httpFieldsGetSig() (component.FuncDesc, component.Resolver) {
	tbl := &typeTable{}
	selfRef := tbl.add(component.BorrowDesc{ResourceType: wasiHTTPFieldsResType})
	listRef := tbl.add(component.ListDesc{Element: tbl.add(component.ListDesc{Element: component.TypeRef{Primitive: "u8"}})})
	return component.FuncDesc{
		Params:  []component.FuncParam{{Name: "self", Type: selfRef}, {Name: "name", Type: component.TypeRef{Primitive: "string"}}},
		Results: component.FuncResults{Unnamed: &listRef},
	}, tbl.resolver()
}

func httpFieldsSetSig() (component.FuncDesc, component.Resolver) {
	tbl := &typeTable{}
	selfRef := tbl.add(component.BorrowDesc{ResourceType: wasiHTTPFieldsResType})
	valueRef := tbl.add(component.ListDesc{Element: tbl.add(component.ListDesc{Element: component.TypeRef{Primitive: "u8"}})})
	errRef := httpHeaderErrorType(tbl)
	resRef := tbl.add(component.ResultDesc{Err: &errRef})
	return component.FuncDesc{
		Params: []component.FuncParam{
			{Name: "self", Type: selfRef},
			{Name: "name", Type: component.TypeRef{Primitive: "string"}},
			{Name: "value", Type: valueRef},
		},
		Results: component.FuncResults{Unnamed: &resRef},
	}, tbl.resolver()
}

func httpOutgoingResponseConstructorSig() (component.FuncDesc, component.Resolver) {
	tbl := &typeTable{}
	headersRef := tbl.add(component.OwnDesc{ResourceType: wasiHTTPFieldsResType})
	ownRef := tbl.add(component.OwnDesc{ResourceType: wasiHTTPOutgoingResponseResType})
	return component.FuncDesc{
		Params:  []component.FuncParam{{Name: "headers", Type: headersRef}},
		Results: component.FuncResults{Unnamed: &ownRef},
	}, tbl.resolver()
}

func httpSetStatusCodeSig() (component.FuncDesc, component.Resolver) {
	tbl := &typeTable{}
	selfRef := tbl.add(component.BorrowDesc{ResourceType: wasiHTTPOutgoingResponseResType})
	resRef := tbl.add(component.ResultDesc{})
	return component.FuncDesc{
		Params: []component.FuncParam{
			{Name: "self", Type: selfRef},
			{Name: "status-code", Type: component.TypeRef{Primitive: "u16"}},
		},
		Results: component.FuncResults{Unnamed: &resRef},
	}, tbl.resolver()
}

func httpOutgoingResponseBodySig() (component.FuncDesc, component.Resolver) {
	tbl := &typeTable{}
	selfRef := tbl.add(component.BorrowDesc{ResourceType: wasiHTTPOutgoingResponseResType})
	okRef := tbl.add(component.OwnDesc{ResourceType: wasiHTTPOutgoingBodyResType})
	resRef := tbl.add(component.ResultDesc{Ok: &okRef})
	return component.FuncDesc{
		Params:  []component.FuncParam{{Name: "self", Type: selfRef}},
		Results: component.FuncResults{Unnamed: &resRef},
	}, tbl.resolver()
}

func httpOutgoingBodyWriteSig() (component.FuncDesc, component.Resolver) {
	tbl := &typeTable{}
	selfRef := tbl.add(component.BorrowDesc{ResourceType: wasiHTTPOutgoingBodyResType})
	okRef := tbl.add(component.OwnDesc{ResourceType: wasiOutputStreamResType})
	resRef := tbl.add(component.ResultDesc{Ok: &okRef})
	return component.FuncDesc{
		Params:  []component.FuncParam{{Name: "self", Type: selfRef}},
		Results: component.FuncResults{Unnamed: &resRef},
	}, tbl.resolver()
}

func httpOutgoingBodyFinishSig() (component.FuncDesc, component.Resolver) {
	tbl := &typeTable{}
	thisRef := tbl.add(component.OwnDesc{ResourceType: wasiHTTPOutgoingBodyResType})
	trailersRef := tbl.add(component.OptionDesc{Element: tbl.add(component.OwnDesc{ResourceType: wasiHTTPFieldsResType})})
	errRef := httpErrorCodeType(tbl)
	resRef := tbl.add(component.ResultDesc{Err: &errRef})
	return component.FuncDesc{
		Params: []component.FuncParam{
			{Name: "this", Type: thisRef},
			{Name: "trailers", Type: trailersRef},
		},
		Results: component.FuncResults{Unnamed: &resRef},
	}, tbl.resolver()
}

// httpIncomingBodyFinishSig builds the FuncDesc/resolver for
// [static]incoming-body.finish(this: own<incoming-body>) ->
// own<future-trailers>.
func httpIncomingBodyFinishSig() (component.FuncDesc, component.Resolver) {
	tbl := &typeTable{}
	thisRef := tbl.add(component.OwnDesc{ResourceType: wasiHTTPIncomingBodyResType})
	ftRef := tbl.add(component.OwnDesc{ResourceType: wasiHTTPFutureTrailersResType})
	return component.FuncDesc{
		Params:  []component.FuncParam{{Name: "this", Type: thisRef}},
		Results: component.FuncResults{Unnamed: &ftRef},
	}, tbl.resolver()
}

// httpFutureTrailersGetSig builds the FuncDesc/resolver for
// [method]future-trailers.get(self: borrow<future-trailers>) ->
// option<result<result<option<trailers>, error-code>>>.
func httpFutureTrailersGetSig() (component.FuncDesc, component.Resolver) {
	tbl := &typeTable{}
	selfRef := tbl.add(component.BorrowDesc{ResourceType: wasiHTTPFutureTrailersResType})
	optTrailersRef := tbl.add(component.OptionDesc{Element: tbl.add(component.OwnDesc{ResourceType: wasiHTTPFieldsResType})})
	errRef := httpErrorCodeType(tbl)
	innerRef := tbl.add(component.ResultDesc{Ok: &optTrailersRef, Err: &errRef})
	outerRef := tbl.add(component.ResultDesc{Ok: &innerRef})
	optOuterRef := tbl.add(component.OptionDesc{Element: outerRef})
	return component.FuncDesc{
		Params:  []component.FuncParam{{Name: "self", Type: selfRef}},
		Results: component.FuncResults{Unnamed: &optOuterRef},
	}, tbl.resolver()
}

func httpResponseOutparamSetSig() (component.FuncDesc, component.Resolver) {
	tbl := &typeTable{}
	paramRef := tbl.add(component.OwnDesc{ResourceType: wasiHTTPResponseOutparamResType})
	okRef := tbl.add(component.OwnDesc{ResourceType: wasiHTTPOutgoingResponseResType})
	errRef := httpErrorCodeType(tbl)
	respRef := tbl.add(component.ResultDesc{Ok: &okRef, Err: &errRef})
	return component.FuncDesc{
		Params: []component.FuncParam{
			{Name: "param", Type: paramRef},
			{Name: "response", Type: respRef},
		},
	}, tbl.resolver()
}

// wasiHTTPOptions registers the wasi:http/types host functions the incoming
// milestone implements, plus the resource-type tags that map the guest's own
// type indices to wazy's (see withResourceTag).
func wasiHTTPOptions(h *wasiHTTP) []component.Option {
	methodFD, methodR := httpMethodSig()
	pathFD, pathR := httpPathWithQuerySig()
	fieldsCtorFD, fieldsCtorR := httpFieldsConstructorSig()
	fieldsSetFD, fieldsSetR := httpFieldsSetSig()
	fieldsEntriesFD, fieldsEntriesR := httpFieldsEntriesSig()
	respCtorFD, respCtorR := httpOutgoingResponseConstructorSig()
	statusFD, statusR := httpSetStatusCodeSig()
	bodyFD, bodyR := httpOutgoingResponseBodySig()
	writeFD, writeR := httpOutgoingBodyWriteSig()
	finishFD, finishR := httpOutgoingBodyFinishSig()
	setFD, setR := httpResponseOutparamSetSig()
	reqHeadersFD, reqHeadersR := httpIncomingRequestHeadersSig()
	reqConsumeFD, reqConsumeR := httpIncomingRequestConsumeSig()
	fieldsGetFD, fieldsGetR := httpFieldsGetSig()
	inBodyFinishFD, inBodyFinishR := httpIncomingBodyFinishSig()
	ftSubFD, ftSubR := wasiSubscribeSig(wasiHTTPFutureTrailersResType)
	ftGetFD, ftGetR := httpFutureTrailersGetSig()

	return []component.Option{
		component.WithResourcesHook(func(t *component.HandleTable) {
			h.getResources = func() (*component.HandleTable, error) { return t, nil }
		}),
		component.WithHostState(httpHostKey{}, h),

		component.WithResourceTag(wasiIfaceHTTPTypes, "incoming-request", wasiHTTPIncomingRequestResType),
		component.WithResourceTag(wasiIfaceHTTPTypes, "fields", wasiHTTPFieldsResType),
		component.WithResourceTag(wasiIfaceHTTPTypes, "outgoing-response", wasiHTTPOutgoingResponseResType),
		component.WithResourceTag(wasiIfaceHTTPTypes, "outgoing-body", wasiHTTPOutgoingBodyResType),
		component.WithResourceTag(wasiIfaceHTTPTypes, "response-outparam", wasiHTTPResponseOutparamResType),
		component.WithResourceTag(wasiIfaceHTTPTypes, "future-trailers", wasiHTTPFutureTrailersResType),

		component.WithImportCustom(wasiIfaceHTTPTypes, "[static]incoming-body.finish", h.incomingBodyFinish, inBodyFinishFD, inBodyFinishR),
		component.WithImportCustom(wasiIfaceHTTPTypes, "[method]future-trailers.subscribe", h.futureTrailersSubscribe, ftSubFD, ftSubR),
		component.WithImportCustom(wasiIfaceHTTPTypes, "[method]future-trailers.get", h.futureTrailersGet, ftGetFD, ftGetR),
		component.WithImportCustom(wasiIfaceHTTPTypes, "[method]incoming-request.method", h.incomingRequestMethod, methodFD, methodR),
		component.WithImportCustom(wasiIfaceHTTPTypes, "[method]incoming-request.path-with-query", h.incomingRequestPathWithQuery, pathFD, pathR),
		component.WithImportCustom(wasiIfaceHTTPTypes, "[method]incoming-request.headers", h.incomingRequestHeaders, reqHeadersFD, reqHeadersR),
		component.WithImportCustom(wasiIfaceHTTPTypes, "[method]incoming-request.consume", h.incomingRequestConsume, reqConsumeFD, reqConsumeR),
		component.WithImportCustom(wasiIfaceHTTPTypes, "[constructor]fields", h.fieldsConstructor, fieldsCtorFD, fieldsCtorR),
		component.WithImportCustom(wasiIfaceHTTPTypes, "[method]fields.get", h.fieldsGet, fieldsGetFD, fieldsGetR),
		component.WithImportCustom(wasiIfaceHTTPTypes, "[method]fields.set", h.fieldsSet, fieldsSetFD, fieldsSetR),
		component.WithImportCustom(wasiIfaceHTTPTypes, "[method]fields.entries", h.fieldsEntries, fieldsEntriesFD, fieldsEntriesR),
		component.WithImportCustom(wasiIfaceHTTPTypes, "[constructor]outgoing-response", h.outgoingResponseConstructor, respCtorFD, respCtorR),
		component.WithImportCustom(wasiIfaceHTTPTypes, "[method]outgoing-response.set-status-code", h.outgoingResponseSetStatusCode, statusFD, statusR),
		component.WithImportCustom(wasiIfaceHTTPTypes, "[method]outgoing-response.body", h.outgoingResponseBody, bodyFD, bodyR),
		component.WithImportCustom(wasiIfaceHTTPTypes, "[method]outgoing-body.write", h.outgoingBodyWrite, writeFD, writeR),
		component.WithImportCustom(wasiIfaceHTTPTypes, "[static]outgoing-body.finish", h.outgoingBodyFinish, finishFD, finishR),
		component.WithImportCustom(wasiIfaceHTTPTypes, "[static]response-outparam.set", h.responseOutparamSet, setFD, setR),
	}
}

// ---- driver: call the guest's exported incoming-handler ----

// ServeHTTP drives the guest component's exported wasi:http/incoming-handler
// with r and writes the response the guest produces to w, making an
// EnableHTTP-instantiated component usable as a net/http.Handler. Any failure
// (no http support, guest didn't set a response, guest signaled an error-code)
// is reported as 500.
func serveHTTPRequest(in *component.Instance, w http.ResponseWriter, r *http.Request) {
	var body []byte
	if r.Body != nil { // a bodyless request (e.g. a bridged GET) leaves Body nil
		b, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read request body: "+err.Error(), http.StatusBadRequest)
			return
		}
		body = b
	}
	status, header, respBody, trailer, err := serveHTTPCall(in, r.Context(), r.Method, r.URL, r.Header, body, r.Trailer)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for k, vs := range header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	// Declare trailer names up front (net/http requires this before
	// WriteHeader for a trailer to be sent), then set their values after the
	// body -- the standard Go server-side trailer protocol.
	for k := range trailer {
		w.Header().Add("Trailer", k)
	}
	w.WriteHeader(int(status))
	_, _ = w.Write(respBody)
	for k, vs := range trailer {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
}

// serveHTTP is ServeHTTP's core: it mints the incoming-request +
// response-outparam resources, invokes the guest's exported handle, and reads
// back the response the guest set. Split out (taking already-decomposed request
// parts) so tests can drive one request without a live net/http server.
func serveHTTPCall(in *component.Instance, ctx context.Context, method string, u *url.URL, headers http.Header, reqBody []byte, reqTrailer http.Header) (status uint16, respHeader http.Header, respBody []byte, respTrailer http.Header, err error) {
	if httpHostOf(in) == nil {
		return 0, nil, nil, nil, fmt.Errorf("component/instance: ServeHTTP: instance was not created with WithWASI(WASIConfig{EnableHTTP: true})")
	}
	handlerInstance, ok := findExportInstance(in, wasiIfaceHTTPIncomingHandler)
	if !ok {
		return 0, nil, nil, nil, fmt.Errorf("component/instance: ServeHTTP: component does not export %s", wasiIfaceHTTPIncomingHandler)
	}

	pathQ := u.Path
	if pathQ == "" {
		pathQ = "/"
	}
	if u.RawQuery != "" {
		pathQ += "?" + u.RawQuery
	}
	req := &httpIncomingRequest{method: strings.ToUpper(method), pathQ: pathQ, headers: headers.Clone(), body: reqBody, trailers: reqTrailer.Clone()}
	reqRep := httpHostOf(in).newIncomingRep(req)
	reqHandle := in.Resources().NewOwn(wasiHTTPIncomingRequestResType, reqRep)

	capture := &httpCapture{}
	outRep := httpHostOf(in).newOutparamRep(capture)
	outHandle := in.Resources().NewOwn(wasiHTTPResponseOutparamResType, outRep)

	if _, err := in.CallExport(ctx, handlerInstance, "handle", reqHandle, outHandle); err != nil {
		return 0, nil, nil, nil, fmt.Errorf("component/instance: ServeHTTP: guest handle: %w", err)
	}
	if !capture.set {
		return 0, nil, nil, nil, fmt.Errorf("component/instance: ServeHTTP: guest handle returned without setting a response")
	}
	if capture.isErr {
		return 0, nil, nil, nil, fmt.Errorf("component/instance: ServeHTTP: guest set response error-code (discriminant %d)", capture.errDisc)
	}
	resp := capture.resp
	hdr := http.Header{}
	if resp.headers != nil {
		for i, name := range resp.headers.names {
			hdr.Add(name, string(resp.headers.values[i]))
		}
	}
	var trailer http.Header
	if resp.body != nil {
		respBody = resp.body.buf.Bytes()
		if resp.body.trailers != nil {
			trailer = http.Header{}
			for i, name := range resp.body.trailers.names {
				trailer.Add(name, string(resp.body.trailers.values[i]))
			}
		}
	}
	return resp.status, hdr, respBody, trailer, nil
}

// findExportInstance returns the full exported-instance name whose
// version-stripped form equals prefix (e.g. "wasi:http/incoming-handler"),
// tolerating the "@x.y.z" the guest's export carries. ok is false if no such
// export exists.
func findExportInstance(in *component.Instance, prefix string) (string, bool) {
	for _, name := range in.InstanceExports() {
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

func (h *wasiHTTP) outgoingRequestConstructor(_ context.Context, args []component.Value) ([]component.Value, error) {
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
	return []component.Value{reqRep}, nil
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

func (h *wasiHTTP) outgoingRequestSetMethod(_ context.Context, args []component.Value) ([]component.Value, error) {
	if len(args) != 2 {
		return nil, fmt.Errorf("[method]outgoing-request.set-method: expected 2 args, got %d", len(args))
	}
	rep, ok := args[0].(uint32)
	if !ok {
		return nil, fmt.Errorf("[method]outgoing-request.set-method: self: expected uint32 rep, got %T", args[0])
	}
	vv, ok := args[1].(component.VariantValue)
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
	return []component.Value{component.ResultValue{IsErr: false}}, nil
}

// optString extracts a lowered option<string> (nil = None, string = Some).
func optString(v component.Value) (string, bool) {
	s, ok := v.(string)
	return s, ok
}

func (h *wasiHTTP) outgoingRequestSetPathWithQuery(_ context.Context, args []component.Value) ([]component.Value, error) {
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
	return []component.Value{component.ResultValue{IsErr: false}}, nil
}

func (h *wasiHTTP) outgoingRequestSetScheme(_ context.Context, args []component.Value) ([]component.Value, error) {
	rep, err := httpSelfRep(args, "[method]outgoing-request.set-scheme")
	if err != nil {
		return nil, err
	}
	r, err := h.outRequest(rep, "[method]outgoing-request.set-scheme")
	if err != nil {
		return nil, err
	}
	if args[1] != nil { // Some(scheme)
		vv, ok := args[1].(component.VariantValue)
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
	return []component.Value{component.ResultValue{IsErr: false}}, nil
}

func (h *wasiHTTP) outgoingRequestSetAuthority(_ context.Context, args []component.Value) ([]component.Value, error) {
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
	return []component.Value{component.ResultValue{IsErr: false}}, nil
}

// outgoingRequestBody returns an own<outgoing-body> the guest writes the
// outbound request body into (via the shared output-stream path). Its bytes are
// sent as the request body by outgoing-handler.handle. result<own<outgoing-body>>.
func (h *wasiHTTP) outgoingRequestBody(_ context.Context, args []component.Value) ([]component.Value, error) {
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
		return []component.Value{component.ResultValue{IsErr: true}}, nil // body can only be taken once
	}
	r.bodyTaken = true
	body := &httpOutgoingBody{}
	r.body = body
	h.mu.Unlock()
	bodyRep := h.newBodyRep(body)
	handle := res.NewOwn(wasiHTTPOutgoingBodyResType, bodyRep)
	return []component.Value{component.ResultValue{IsErr: false, Payload: handle}}, nil
}

func (h *wasiHTTP) newReqOptionsRep(o *httpRequestOptions) uint32 {
	h.mu.Lock()
	defer h.mu.Unlock()
	rep := h.nextRep
	h.nextRep++
	h.reqOptions[rep] = o
	return rep
}

func (h *wasiHTTP) requestOptionsConstructor(_ context.Context, args []component.Value) ([]component.Value, error) {
	if len(args) != 0 {
		return nil, fmt.Errorf("[constructor]request-options: expected 0 args, got %d", len(args))
	}
	rep := h.newReqOptionsRep(&httpRequestOptions{})
	return []component.Value{rep}, nil // top-level own<request-options>
}

// requestOptionsSetTimeout implements set-connect-timeout / set-first-byte-timeout
// (both self: borrow<request-options>, duration: option<u64 ns> -> result).
func (h *wasiHTTP) requestOptionsSetTimeout(fn string, connect bool) component.HostFunc {
	return func(_ context.Context, args []component.Value) ([]component.Value, error) {
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
		return []component.Value{component.ResultValue{IsErr: false}}, nil
	}
}

// httpSelfRep validates a (self, arg) 2-arg method whose self is a resource rep.
func httpSelfRep(args []component.Value, fn string) (uint32, error) {
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
func (h *wasiHTTP) outgoingHandlerHandle(ctx context.Context, args []component.Value) ([]component.Value, error) {
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
			// immutable: these are the headers that actually arrived, and
			// types.wit makes an incoming message's fields read-only (see
			// incomingResponseHeaders).
			respHeaders := &httpFields{immutable: true}
			// types.wit requires entries "in the original casing and in the
			// order in which they will be serialized for transport", so the
			// name is stored as net/http gives it (canonical MIME casing --
			// Go has already discarded the origin server's exact bytes, and
			// its canonical form is far closer to the original than a forced
			// lower-casing) and the names are sorted, because ranging a
			// map would hand the guest a DIFFERENT order on every call.
			// Values keep their relative order within a name, which is the
			// part that actually carries meaning (Set-Cookie).
			names := make([]string, 0, len(hresp.Header))
			for name := range hresp.Header {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				for _, v := range hresp.Header[name] {
					respHeaders.names = append(respHeaders.names, name)
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
	return []component.Value{component.ResultValue{IsErr: false, Payload: handle}}, nil
}

func (h *wasiHTTP) futureSubscribe(_ context.Context, args []component.Value) ([]component.Value, error) {
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
	return []component.Value{handle}, nil
}

func (h *wasiHTTP) futureGet(_ context.Context, args []component.Value) ([]component.Value, error) {
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
		return []component.Value{nil}, nil
	}
	fut.taken = true
	errCode, respRep := fut.errCode, fut.respRep
	h.mu.Unlock()

	// Shape: option<result<result<incoming-response, error-code>>>. The outer
	// result models "future already retrieved" (Err) -- always Ok here. The
	// inner result carries the incoming-response or the transport error-code.
	var inner component.ResultValue
	if errCode != 0 {
		inner = component.ResultValue{IsErr: true, Payload: component.VariantValue{Disc: errCode}}
	} else {
		handle := res.NewOwn(wasiHTTPIncomingResponseResType, respRep)
		inner = component.ResultValue{IsErr: false, Payload: handle}
	}
	outer := component.ResultValue{IsErr: false, Payload: inner}
	return []component.Value{outer}, nil // Some(outer)
}

func (h *wasiHTTP) incomingResponseStatus(_ context.Context, args []component.Value) ([]component.Value, error) {
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
	return []component.Value{uint32(r.status)}, nil
}

// incomingResponseHeaders implements [method]incoming-response.headers ->
// own<fields>. The returned fields is the response's OWN headers object, not a
// copy: types.wit calls it "a child of the incoming-response", so a guest
// reading it must see what actually arrived on the wire. It is marked
// immutable at construction (see newInResponseRep's caller), so a guest that
// tries to write through it gets header-error::immutable rather than silently
// mutating a received message.
//
// The child-must-be-dropped-before-the-parent rule types.wit states is not
// enforced: a guest that leaks the handle merely holds a rep whose parent is
// gone, which costs nothing here and traps loudly on use.
func (h *wasiHTTP) incomingResponseHeaders(_ context.Context, args []component.Value) ([]component.Value, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("[method]incoming-response.headers: expected 1 arg (self), got %d", len(args))
	}
	rep, ok := args[0].(uint32)
	if !ok {
		return nil, fmt.Errorf("[method]incoming-response.headers: self: expected uint32 rep, got %T", args[0])
	}
	h.mu.Lock()
	r, ok := h.inResponses[rep]
	h.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("[method]incoming-response.headers: rep %d does not name a live incoming-response", rep)
	}
	fields := r.headers
	if fields == nil {
		// A response synthesized without headers still owes the guest a
		// readable (empty) fields rather than a trap.
		fields = &httpFields{immutable: true}
	}
	// Top-level own<fields> result: allocHandleResult wraps this bare rep.
	return []component.Value{h.newFieldsRep(fields)}, nil
}

func (h *wasiHTTP) incomingResponseConsume(_ context.Context, args []component.Value) ([]component.Value, error) {
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
		return []component.Value{component.ResultValue{IsErr: true}}, nil // body already taken
	}
	r.consumed = true
	body := r.body
	h.mu.Unlock()
	bodyRep := h.newInBodyRep(&httpIncomingBody{body: body})
	handle := res.NewOwn(wasiHTTPIncomingBodyResType, bodyRep)
	return []component.Value{component.ResultValue{IsErr: false, Payload: handle}}, nil
}

func (h *wasiHTTP) incomingBodyStream(_ context.Context, args []component.Value) ([]component.Value, error) {
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
		return []component.Value{component.ResultValue{IsErr: true}}, nil // stream can only be taken once
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
	return []component.Value{component.ResultValue{IsErr: false, Payload: handle}}, nil
}

func (h *wasiHTTP) newFutureTrailersRep(f *httpFutureTrailers) uint32 {
	h.mu.Lock()
	defer h.mu.Unlock()
	rep := h.nextRep
	h.nextRep++
	h.futureTrailers[rep] = f
	return rep
}

// incomingBodyFinish implements [static]incoming-body.finish(this:
// incoming-body) -> future-trailers (std's IncomingBody::finish). The body's
// trailers (carried from the request/response) are already resolved, so the
// returned future-trailers is immediately ready.
func (h *wasiHTTP) incomingBodyFinish(_ context.Context, args []component.Value) ([]component.Value, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("[static]incoming-body.finish: expected 1 arg (this), got %d", len(args))
	}
	// this: own<incoming-body> lifted to its rep (ownership consumed).
	rep, ok := args[0].(uint32)
	if !ok {
		return nil, fmt.Errorf("[static]incoming-body.finish: this: expected uint32 rep, got %T", args[0])
	}
	h.mu.Lock()
	b, ok := h.inBodies[rep]
	var trailers http.Header
	if ok {
		trailers = b.trailers
		delete(h.inBodies, rep)
	}
	h.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("[static]incoming-body.finish: rep %d does not name a live incoming-body", rep)
	}
	ftRep := h.newFutureTrailersRep(&httpFutureTrailers{trailers: trailers})
	// Top-level own<future-trailers> result: auto-wrapped by allocHandleResult.
	return []component.Value{ftRep}, nil
}

// futureTrailersSubscribe implements [method]future-trailers.subscribe(self)
// -> pollable: the trailers are already resolved, so it returns the
// always-ready pollable (the central poll host treats it as immediately ready).
func (h *wasiHTTP) futureTrailersSubscribe(_ context.Context, args []component.Value) ([]component.Value, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("[method]future-trailers.subscribe: expected 1 arg (self), got %d", len(args))
	}
	rep, ok := args[0].(uint32)
	if !ok {
		return nil, fmt.Errorf("[method]future-trailers.subscribe: self: expected uint32 rep, got %T", args[0])
	}
	h.mu.Lock()
	_, ok = h.futureTrailers[rep]
	h.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("[method]future-trailers.subscribe: rep %d does not name a live future-trailers", rep)
	}
	return []component.Value{wasiPollableRep}, nil
}

// futureTrailersGet implements [method]future-trailers.get(self) ->
// option<result<result<option<trailers>, error-code>>>. The trailers are
// already resolved, so it always returns Some on the first call (None means
// "not ready yet", which never happens here); the outer result's Err arm
// signals "already gotten" on a second call. The inner
// result<option<trailers>, error-code> is Ok(Some(fields)) when the
// request/response carried trailers, else Ok(None).
func (h *wasiHTTP) futureTrailersGet(_ context.Context, args []component.Value) ([]component.Value, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("[method]future-trailers.get: expected 1 arg (self), got %d", len(args))
	}
	rep, ok := args[0].(uint32)
	if !ok {
		return nil, fmt.Errorf("[method]future-trailers.get: self: expected uint32 rep, got %T", args[0])
	}
	res, err := h.getResources()
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	ft, ok := h.futureTrailers[rep]
	if !ok {
		h.mu.Unlock()
		return nil, fmt.Errorf("[method]future-trailers.get: rep %d does not name a live future-trailers", rep)
	}
	if ft.taken {
		h.mu.Unlock()
		// option some( result err ) -- "already gotten".
		return []component.Value{component.ResultValue{IsErr: true}}, nil
	}
	ft.taken = true
	trailers := ft.trailers
	h.mu.Unlock()

	// Build the option<trailers> inner value: Some(fields handle) or None (nil).
	var trailersOpt component.Value
	if len(trailers) > 0 {
		f := &httpFields{}
		for name, vals := range trailers {
			for _, v := range vals {
				f.names = append(f.names, strings.ToLower(name))
				f.values = append(f.values, []byte(v))
			}
		}
		fieldsRep := h.newFieldsRep(f)
		// Nested own<trailers> (inside option/result): mint the handle directly.
		trailersOpt = res.NewOwn(wasiHTTPFieldsResType, fieldsRep)
	}
	// option some( result ok( result ok( option<trailers> ) ) )
	inner := component.ResultValue{IsErr: false, Payload: trailersOpt}
	outer := component.ResultValue{IsErr: false, Payload: inner}
	return []component.Value{outer}, nil
}

// ---- outgoing WIT type descriptors + signatures ----

// httpSchemeType interns the wasi:http/types `scheme` variant {HTTP, HTTPS,
// other(string)} into tbl.
func httpSchemeType(tbl *typeTable) component.TypeRef {
	strRef := component.TypeRef{Primitive: "string"}
	return tbl.add(component.VariantDesc{Cases: []component.VariantCase{
		{Name: "HTTP"}, {Name: "HTTPS"}, {Name: "other", Type: &strRef},
	}})
}

func httpOutgoingRequestConstructorSig() (component.FuncDesc, component.Resolver) {
	tbl := &typeTable{}
	headersRef := tbl.add(component.OwnDesc{ResourceType: wasiHTTPFieldsResType})
	ownRef := tbl.add(component.OwnDesc{ResourceType: wasiHTTPOutgoingRequestResType})
	return component.FuncDesc{
		Params:  []component.FuncParam{{Name: "headers", Type: headersRef}},
		Results: component.FuncResults{Unnamed: &ownRef},
	}, tbl.resolver()
}

func httpOutgoingRequestBodySig() (component.FuncDesc, component.Resolver) {
	tbl := &typeTable{}
	selfRef := tbl.add(component.BorrowDesc{ResourceType: wasiHTTPOutgoingRequestResType})
	okRef := tbl.add(component.OwnDesc{ResourceType: wasiHTTPOutgoingBodyResType})
	resRef := tbl.add(component.ResultDesc{Ok: &okRef})
	return component.FuncDesc{
		Params:  []component.FuncParam{{Name: "self", Type: selfRef}},
		Results: component.FuncResults{Unnamed: &resRef},
	}, tbl.resolver()
}

func httpRequestOptionsConstructorSig() (component.FuncDesc, component.Resolver) {
	tbl := &typeTable{}
	ownRef := tbl.add(component.OwnDesc{ResourceType: wasiHTTPRequestOptionsResType})
	return component.FuncDesc{Results: component.FuncResults{Unnamed: &ownRef}}, tbl.resolver()
}

// httpSetTimeoutSig: (self: borrow<request-options>, duration: option<u64 ns>) -> result.
func httpSetTimeoutSig() (component.FuncDesc, component.Resolver) {
	tbl := &typeTable{}
	selfRef := tbl.add(component.BorrowDesc{ResourceType: wasiHTTPRequestOptionsResType})
	durRef := tbl.add(component.OptionDesc{Element: component.TypeRef{Primitive: "u64"}})
	resRef := tbl.add(component.ResultDesc{})
	return component.FuncDesc{
		Params:  []component.FuncParam{{Name: "self", Type: selfRef}, {Name: "duration", Type: durRef}},
		Results: component.FuncResults{Unnamed: &resRef},
	}, tbl.resolver()
}

func httpSetMethodSig() (component.FuncDesc, component.Resolver) {
	tbl := &typeTable{}
	selfRef := tbl.add(component.BorrowDesc{ResourceType: wasiHTTPOutgoingRequestResType})
	methodRef := httpMethodType(tbl)
	resRef := tbl.add(component.ResultDesc{})
	return component.FuncDesc{
		Params:  []component.FuncParam{{Name: "self", Type: selfRef}, {Name: "method", Type: methodRef}},
		Results: component.FuncResults{Unnamed: &resRef},
	}, tbl.resolver()
}

// httpSetOptStringSig builds set-path-with-query / set-authority: (self:
// borrow<outgoing-request>, v: option<string>) -> result.
func httpSetOptStringSig() (component.FuncDesc, component.Resolver) {
	tbl := &typeTable{}
	selfRef := tbl.add(component.BorrowDesc{ResourceType: wasiHTTPOutgoingRequestResType})
	optRef := tbl.add(component.OptionDesc{Element: component.TypeRef{Primitive: "string"}})
	resRef := tbl.add(component.ResultDesc{})
	return component.FuncDesc{
		Params:  []component.FuncParam{{Name: "self", Type: selfRef}, {Name: "v", Type: optRef}},
		Results: component.FuncResults{Unnamed: &resRef},
	}, tbl.resolver()
}

func httpSetSchemeSig() (component.FuncDesc, component.Resolver) {
	tbl := &typeTable{}
	selfRef := tbl.add(component.BorrowDesc{ResourceType: wasiHTTPOutgoingRequestResType})
	optRef := tbl.add(component.OptionDesc{Element: httpSchemeType(tbl)})
	resRef := tbl.add(component.ResultDesc{})
	return component.FuncDesc{
		Params:  []component.FuncParam{{Name: "self", Type: selfRef}, {Name: "scheme", Type: optRef}},
		Results: component.FuncResults{Unnamed: &resRef},
	}, tbl.resolver()
}

func httpOutgoingHandlerSig() (component.FuncDesc, component.Resolver) {
	tbl := &typeTable{}
	reqRef := tbl.add(component.OwnDesc{ResourceType: wasiHTTPOutgoingRequestResType})
	optRef := tbl.add(component.OptionDesc{Element: tbl.add(component.OwnDesc{ResourceType: wasiHTTPRequestOptionsResType})})
	okRef := tbl.add(component.OwnDesc{ResourceType: wasiHTTPFutureResType})
	errRef := httpErrorCodeType(tbl)
	resRef := tbl.add(component.ResultDesc{Ok: &okRef, Err: &errRef})
	return component.FuncDesc{
		Params:  []component.FuncParam{{Name: "request", Type: reqRef}, {Name: "options", Type: optRef}},
		Results: component.FuncResults{Unnamed: &resRef},
	}, tbl.resolver()
}

func httpFutureGetSig() (component.FuncDesc, component.Resolver) {
	tbl := &typeTable{}
	selfRef := tbl.add(component.BorrowDesc{ResourceType: wasiHTTPFutureResType})
	respRef := tbl.add(component.OwnDesc{ResourceType: wasiHTTPIncomingResponseResType})
	errRef := httpErrorCodeType(tbl)
	innerRef := tbl.add(component.ResultDesc{Ok: &respRef, Err: &errRef})
	outerRef := tbl.add(component.ResultDesc{Ok: &innerRef})
	optRef := tbl.add(component.OptionDesc{Element: outerRef})
	return component.FuncDesc{
		Params:  []component.FuncParam{{Name: "self", Type: selfRef}},
		Results: component.FuncResults{Unnamed: &optRef},
	}, tbl.resolver()
}

func httpIncomingResponseStatusSig() (component.FuncDesc, component.Resolver) {
	tbl := &typeTable{}
	selfRef := tbl.add(component.BorrowDesc{ResourceType: wasiHTTPIncomingResponseResType})
	statusRef := component.TypeRef{Primitive: "u16"}
	return component.FuncDesc{
		Params:  []component.FuncParam{{Name: "self", Type: selfRef}},
		Results: component.FuncResults{Unnamed: &statusRef},
	}, tbl.resolver()
}

// httpFieldsEntriesSig builds [method]fields.entries(self: borrow<fields>) ->
// list<tuple<field-key, field-value>>, i.e. list<tuple<string, list<u8>>> --
// the first list<tuple<...>> result in this file. TupleDesc needs its own
// interned TypeRef (its element types cannot be expressed inline), which is
// exactly what typeTable is for.
func httpFieldsEntriesSig() (component.FuncDesc, component.Resolver) {
	tbl := &typeTable{}
	selfRef := tbl.add(component.BorrowDesc{ResourceType: wasiHTTPFieldsResType})
	valueRef := tbl.add(component.ListDesc{Element: component.TypeRef{Primitive: "u8"}})
	pairRef := tbl.add(component.TupleDesc{Elements: []component.TypeRef{{Primitive: "string"}, valueRef}})
	listRef := tbl.add(component.ListDesc{Element: pairRef})
	return component.FuncDesc{
		Params:  []component.FuncParam{{Name: "self", Type: selfRef}},
		Results: component.FuncResults{Unnamed: &listRef},
	}, tbl.resolver()
}

// httpIncomingResponseHeadersSig builds [method]incoming-response.headers(
// self: borrow<incoming-response>) -> own<fields>.
func httpIncomingResponseHeadersSig() (component.FuncDesc, component.Resolver) {
	tbl := &typeTable{}
	selfRef := tbl.add(component.BorrowDesc{ResourceType: wasiHTTPIncomingResponseResType})
	headersRef := tbl.add(component.OwnDesc{ResourceType: wasiHTTPFieldsResType})
	return component.FuncDesc{
		Params:  []component.FuncParam{{Name: "self", Type: selfRef}},
		Results: component.FuncResults{Unnamed: &headersRef},
	}, tbl.resolver()
}

func httpIncomingResponseConsumeSig() (component.FuncDesc, component.Resolver) {
	tbl := &typeTable{}
	selfRef := tbl.add(component.BorrowDesc{ResourceType: wasiHTTPIncomingResponseResType})
	okRef := tbl.add(component.OwnDesc{ResourceType: wasiHTTPIncomingBodyResType})
	resRef := tbl.add(component.ResultDesc{Ok: &okRef})
	return component.FuncDesc{
		Params:  []component.FuncParam{{Name: "self", Type: selfRef}},
		Results: component.FuncResults{Unnamed: &resRef},
	}, tbl.resolver()
}

func httpIncomingBodyStreamSig() (component.FuncDesc, component.Resolver) {
	tbl := &typeTable{}
	selfRef := tbl.add(component.BorrowDesc{ResourceType: wasiHTTPIncomingBodyResType})
	okRef := tbl.add(component.OwnDesc{ResourceType: wasiInputStreamResType})
	resRef := tbl.add(component.ResultDesc{Ok: &okRef})
	return component.FuncDesc{
		Params:  []component.FuncParam{{Name: "self", Type: selfRef}},
		Results: component.FuncResults{Unnamed: &resRef},
	}, tbl.resolver()
}

// wasiHTTPOutgoingOptions registers the client-side (outgoing-handler) host
// funcs plus the wasi:io/poll pollable.block/poll a synchronous future still
// makes the guest call. Registered only when EnableHTTP; the pollable funcs are
// no-ops here (every future this package mints is already resolved), matching
// the always-ready model wasi_sockets.go uses.
func wasiHTTPOutgoingOptions(h *wasiHTTP) []component.Option {
	reqCtorFD, reqCtorR := httpOutgoingRequestConstructorSig()
	methodFD, methodR := httpSetMethodSig()
	pathFD, pathR := httpSetOptStringSig()
	authFD, authR := httpSetOptStringSig()
	schemeFD, schemeR := httpSetSchemeSig()
	handleFD, handleR := httpOutgoingHandlerSig()
	subFD, subR := wasiSubscribeSig(wasiHTTPFutureResType)
	getFD, getR := httpFutureGetSig()
	statusFD, statusR := httpIncomingResponseStatusSig()
	respHeadersFD, respHeadersR := httpIncomingResponseHeadersSig()
	consumeFD, consumeR := httpIncomingResponseConsumeSig()
	streamFD, streamR := httpIncomingBodyStreamSig()
	reqBodyFD, reqBodyR := httpOutgoingRequestBodySig()
	optCtorFD, optCtorR := httpRequestOptionsConstructorSig()
	setTimeoutFD, setTimeoutR := httpSetTimeoutSig()

	return []component.Option{
		component.WithResourceTag(wasiIfaceHTTPTypes, "outgoing-request", wasiHTTPOutgoingRequestResType),
		component.WithResourceTag(wasiIfaceHTTPTypes, "future-incoming-response", wasiHTTPFutureResType),
		component.WithResourceTag(wasiIfaceHTTPTypes, "incoming-response", wasiHTTPIncomingResponseResType),
		component.WithResourceTag(wasiIfaceHTTPTypes, "incoming-body", wasiHTTPIncomingBodyResType),
		component.WithResourceTag(wasiIfaceHTTPTypes, "request-options", wasiHTTPRequestOptionsResType),
		// (The pollable tag + block/poll are registered centrally, see wasi_poll.go.)

		component.WithImportCustom(wasiIfaceHTTPTypes, "[constructor]outgoing-request", h.outgoingRequestConstructor, reqCtorFD, reqCtorR),
		component.WithImportCustom(wasiIfaceHTTPTypes, "[method]outgoing-request.body", h.outgoingRequestBody, reqBodyFD, reqBodyR),
		component.WithImportCustom(wasiIfaceHTTPTypes, "[constructor]request-options", h.requestOptionsConstructor, optCtorFD, optCtorR),
		component.WithImportCustom(wasiIfaceHTTPTypes, "[method]request-options.set-connect-timeout", h.requestOptionsSetTimeout("[method]request-options.set-connect-timeout", true), setTimeoutFD, setTimeoutR),
		component.WithImportCustom(wasiIfaceHTTPTypes, "[method]request-options.set-first-byte-timeout", h.requestOptionsSetTimeout("[method]request-options.set-first-byte-timeout", false), setTimeoutFD, setTimeoutR),
		component.WithImportCustom(wasiIfaceHTTPTypes, "[method]outgoing-request.set-method", h.outgoingRequestSetMethod, methodFD, methodR),
		component.WithImportCustom(wasiIfaceHTTPTypes, "[method]outgoing-request.set-path-with-query", h.outgoingRequestSetPathWithQuery, pathFD, pathR),
		component.WithImportCustom(wasiIfaceHTTPTypes, "[method]outgoing-request.set-scheme", h.outgoingRequestSetScheme, schemeFD, schemeR),
		component.WithImportCustom(wasiIfaceHTTPTypes, "[method]outgoing-request.set-authority", h.outgoingRequestSetAuthority, authFD, authR),
		component.WithImportCustom(wasiIfaceHTTPOutgoingHandler, "handle", h.outgoingHandlerHandle, handleFD, handleR),
		component.WithImportCustom(wasiIfaceHTTPTypes, "[method]future-incoming-response.subscribe", h.futureSubscribe, subFD, subR),
		component.WithImportCustom(wasiIfaceHTTPTypes, "[method]future-incoming-response.get", h.futureGet, getFD, getR),
		component.WithImportCustom(wasiIfaceHTTPTypes, "[method]incoming-response.status", h.incomingResponseStatus, statusFD, statusR),
		component.WithImportCustom(wasiIfaceHTTPTypes, "[method]incoming-response.headers", h.incomingResponseHeaders, respHeadersFD, respHeadersR),
		component.WithImportCustom(wasiIfaceHTTPTypes, "[method]incoming-response.consume", h.incomingResponseConsume, consumeFD, consumeR),
		component.WithImportCustom(wasiIfaceHTTPTypes, "[method]incoming-body.stream", h.incomingBodyStream, streamFD, streamR),
	}
}

// httpHostKey identifies this package's per-instance wasi:http server state
// in the Instance's host-state map. A private zero-size type, so no other
// host implementation can collide with it.
type httpHostKey struct{}

// httpHostOf returns the wasi:http server state attached to in, or nil when
// the component was not instantiated with EnableHTTP.
func httpHostOf(in *component.Instance) *wasiHTTP {
	h, _ := in.HostState(httpHostKey{}).(*wasiHTTP)
	return h
}

// Handler adapts a component that exports wasi:http/incoming-handler to
// net/http. Instantiate it with Config{EnableHTTP: true}; the returned
// handler synthesizes each inbound request into the guest's WASI types and
// writes back what it produces.
func Handler(in *component.Instance) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveHTTPRequest(in, w, r)
	})
}
