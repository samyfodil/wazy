package instance

// This file implements the server (incoming-handler) side of the WASI 0.2
// wasi:http/proxy world: enough of wasi:http/types for a real rustc
// wasm32-wasip2 component that EXPORTS wasi:http/incoming-handler to receive an
// HTTP request and write a response. Unlike the rest of WithWASI (which
// registers host functions the guest imports and calls), the incoming-handler
// is an EXPORT the host calls: serveHTTP synthesizes the incoming-request +
// response-outparam resources, invokes the guest's `handle`, and reads back
// whatever the guest set on the outparam.
//
// The response body is written through wasi:io/streams' output-stream, which
// WithWASI already implements (the same path stdout uses): outgoing-body.write
// mints an output-stream rep backed by the body buffer, and writeSink (wasi.go)
// gains an http fallback so the guest's blocking-write-and-flush lands in it.
//
// # Scope (ponytail: this is the incoming milestone, not all of wasi:http)
//
// Implemented: incoming-request.{method, path-with-query}; fields
// constructor + set; outgoing-response constructor + set-status-code + body;
// outgoing-body.{write, finish}; response-outparam.set. This is exactly what a
// wit-bindgen incoming-handler guest calls to read the request line and write a
// response. Not yet implemented (fail loud when a guest reaches for them):
// request/response headers readback, incoming-request.consume (request body),
// trailers, and the entire outgoing-handler (client) side.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

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
)

// Interface names are registered version-tolerantly (mkImportKey strips the
// "@x.y.z"): a guest built against any wasi 0.2.x patch resolves against these.
const (
	wasiIfaceHTTPTypes           = "wasi:http/types@0.2.0"
	wasiIfaceHTTPIncomingHandler = "wasi:http/incoming-handler"
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
	method  string // uppercase HTTP method (e.g. "GET")
	pathQ   string // path plus "?"+rawquery, e.g. "/hello?x=1"
	headers http.Header
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
		withImportCustom(wasiIfaceHTTPTypes, "[constructor]fields", h.fieldsConstructor, fieldsCtorFD, fieldsCtorR),
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
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read request body: "+err.Error(), http.StatusBadRequest)
		return
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
	req := &httpIncomingRequest{method: strings.ToUpper(method), pathQ: pathQ, headers: headers.Clone()}
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
