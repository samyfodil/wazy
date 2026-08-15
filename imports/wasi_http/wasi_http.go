// Package wasi_http implements the pre-standard WASI-HTTP host ABI defined by
// github.com/stealthrocket/wasi-go, so guests built against its client
// bindings (the dev-wasm Go, Rust, C, AssemblyScript and dotnet examples, and
// anything else compiled against them) run on wazy unmodified.
//
// This is not the WASI 0.2 wasi:http interface. That one is part of the
// Component Model and lives in the component package; the two ABIs are
// incompatible by construction. This is a raw i32 shim spread over three host
// modules ("default-outgoing-HTTP", "types" and "streams") in which the guest
// passes pointers into its own linear memory and the host allocates return
// buffers by calling back into the guest's exported cabi_realloc.
//
// Only the outbound (client) path is implemented, matching upstream: the
// server-side "request" entry point returns 0 there too.
//
// # Wire format
//
// Three conventions repeat throughout, all little-endian:
//
//   - a returned string is an 8-byte [ptr u32][len u32] struct written to an
//     out pointer supplied by the guest;
//   - a fields (header) list is a flat array of 16-byte
//     [key_ptr][key_len][val_ptr][val_len] tuples, addressed by an 8-byte
//     [list_ptr u32][count u32] header struct;
//   - a result is a leading u32 discriminant, 0 for ok and 1 for error,
//     followed by the payload.
//
// # Differences from wasi-go
//
// The wire format is identical - that is the point - but four host-side
// behaviours are deliberately not copied:
//
//   - wasi-go calls log.Fatalf on an allocation failure, a bad method or an
//     unreadable buffer, which exits the host process. Here those trap the
//     calling guest instead, so one bad module cannot take down its embedder.
//   - The guest's context.Context is passed to the outbound request, so a
//     cancelled or deadlined guest call cancels the HTTP call with it.
//   - Dropping an incoming response closes its body, rather than leaking the
//     body and its connection.
//   - A failed stream write reports the error arm; wasi-go logs the failure
//     but still tells the guest the write succeeded.
//
// # Sandboxing
//
// Instantiating this package lets the guest make arbitrary outbound HTTP
// requests from the host, with the host's network access and no allow-list.
// Do not instantiate it for untrusted guests.
package wasi_http

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"sync"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/api"
)

// Module names the guest imports by. These strings are the ABI: they come
// from wasi-go and cannot change.
const (
	// OutgoingModuleName is the module holding the outbound dispatcher.
	OutgoingModuleName = "default-outgoing-HTTP"
	// TypesModuleName is the module holding requests, responses and fields.
	TypesModuleName = "types"
	// StreamsModuleName is the module holding body streams.
	StreamsModuleName = "streams"
)

// MustInstantiate calls Instantiate or panics.
func MustInstantiate(ctx context.Context, r wazy.Runtime) {
	if _, err := Instantiate(ctx, r); err != nil {
		panic(err)
	}
}

// Instantiate instantiates the three host modules into r, and returns a
// Closer that closes all of them.
//
// Handle tables are scoped to this call, so two runtimes never share handles.
// Within one runtime the tables are shared by every guest instance, exactly as
// in wasi-go, and are safe for concurrent use.
//
// Note: closing the wazy.Runtime has the same effect as closing the result.
func Instantiate(ctx context.Context, r wazy.Runtime) (api.Closer, error) {
	return newHost().instantiate(ctx, r)
}

func newHost() *host {
	return &host{
		streams:   map[uint32]stream{},
		fields:    map[uint32]http.Header{},
		requests:  map[uint32]*request{},
		responses: map[uint32]*response{},
		outparams: map[uint32]uint32{},
	}
}

// host holds the handle tables backing one Instantiate call.
//
// ponytail: one mutex for all five tables. Every critical section is an O(1)
// map operation - the HTTP round trip and all guest memory access happen
// outside it - so per-table locks would buy nothing measurable.
type host struct {
	mu        sync.Mutex
	nextID    uint32
	streams   map[uint32]stream
	fields    map[uint32]http.Header
	requests  map[uint32]*request
	responses map[uint32]*response
	outparams map[uint32]uint32
}

type stream struct {
	reader io.Reader
	writer io.Writer
}

type request struct {
	method    string
	path      string
	query     string
	scheme    string
	authority string
	headers   uint32 // fields handle
	body      *bytes.Buffer
}

func (r *request) url() string {
	return r.scheme + "://" + r.authority + r.path + r.query
}

type response struct {
	status  int
	header  http.Header   // lifted into fields lazily, by incomingResponseHeaders
	headers uint32        // fields handle, 0 until first asked for
	body    io.ReadCloser // nil for outgoing responses
}

// methods maps the wasi-http method enum to its name. The order is the ABI's.
var methods = [...]string{"GET", "HEAD", "POST", "PUT", "DELETE", "CONNECT", "OPTIONS", "TRACE", "PATCH"}

func (h *host) instantiate(ctx context.Context, r wazy.Runtime) (api.Closer, error) {
	var mods closers

	b := r.NewHostModuleBuilder(OutgoingModuleName)
	wazy.HostFunc14(b.NewFunctionBuilder(), h.request).Export("request")
	wazy.HostFunc8(b.NewFunctionBuilder(), h.handle).Export("handle")
	mod, err := b.Instantiate(ctx)
	if err != nil {
		return nil, err
	}
	mods = append(mods, mod)

	b = r.NewHostModuleBuilder(TypesModuleName)
	wazy.HostFunc14(b.NewFunctionBuilder(), h.newOutgoingRequest).Export("new-outgoing-request")
	wazy.HostFunc2(b.NewFunctionBuilder(), h.newFields).Export("new-fields")
	wazy.HostProc1(b.NewFunctionBuilder(), h.dropFields).Export("drop-fields")
	wazy.HostProc2(b.NewFunctionBuilder(), h.fieldsEntries).Export("fields-entries")
	wazy.HostProc1(b.NewFunctionBuilder(), h.dropOutgoingRequest).Export("drop-outgoing-request")
	wazy.HostProc2(b.NewFunctionBuilder(), h.outgoingRequestWrite).Export("outgoing-request-write")
	wazy.HostProc1(b.NewFunctionBuilder(), h.dropIncomingResponse).Export("drop-incoming-response")
	wazy.HostFunc1(b.NewFunctionBuilder(), h.incomingResponseStatus).Export("incoming-response-status")
	wazy.HostFunc1(b.NewFunctionBuilder(), h.incomingResponseHeaders).Export("incoming-response-headers")
	wazy.HostProc2(b.NewFunctionBuilder(), h.incomingResponseConsume).Export("incoming-response-consume")
	wazy.HostProc2(b.NewFunctionBuilder(), futureIncomingResponseGet).Export("future-incoming-response-get")
	wazy.HostProc2(b.NewFunctionBuilder(), h.incomingRequestMethod).Export("incoming-request-method")
	wazy.HostProc2(b.NewFunctionBuilder(), h.incomingRequestPath).Export("incoming-request-path")
	wazy.HostProc2(b.NewFunctionBuilder(), h.incomingRequestAuthority).Export("incoming-request-authority")
	wazy.HostFunc1(b.NewFunctionBuilder(), h.incomingRequestHeaders).Export("incoming-request-headers")
	wazy.HostProc2(b.NewFunctionBuilder(), incomingRequestConsume).Export("incoming-request-consume")
	wazy.HostProc1(b.NewFunctionBuilder(), h.dropIncomingRequest).Export("drop-incoming-request")
	wazy.HostFunc5(b.NewFunctionBuilder(), h.setResponseOutparam).Export("set-response-outparam")
	wazy.HostFunc2(b.NewFunctionBuilder(), h.newOutgoingResponse).Export("new-outgoing-response")
	wazy.HostProc2(b.NewFunctionBuilder(), h.outgoingResponseWrite).Export("outgoing-response-write")
	wazy.HostProc1(b.NewFunctionBuilder(), h.dropOutgoingResponse).Export("drop-outgoing-response")
	wazy.HostProc2(b.NewFunctionBuilder(), logIt).Export("log-it")
	if mod, err = b.Instantiate(ctx); err != nil {
		mods.Close(ctx)
		return nil, err
	}
	mods = append(mods, mod)

	b = r.NewHostModuleBuilder(StreamsModuleName)
	wazy.HostProc3(b.NewFunctionBuilder(), h.streamRead).Export("read")
	wazy.HostProc1(b.NewFunctionBuilder(), h.dropInputStream).Export("drop-input-stream")
	wazy.HostProc4(b.NewFunctionBuilder(), h.streamWrite).Export("write")
	if mod, err = b.Instantiate(ctx); err != nil {
		mods.Close(ctx)
		return nil, err
	}
	return append(mods, mod), nil
}

// closers closes every module Instantiate created.
type closers []api.Closer

func (c closers) Close(ctx context.Context) error {
	var err error
	for _, closer := range c {
		if cerr := closer.Close(ctx); cerr != nil && err == nil {
			err = cerr
		}
	}
	return err
}

// newID returns the next never-zero handle. Zero is the ABI's "no handle".
func (h *host) newID() uint32 {
	h.nextID++
	return h.nextID
}

// --- default-outgoing-HTTP -------------------------------------------------

// request is the server-side entry point, unimplemented here as upstream.
// The parameters are an inbound request plus its HTTP options.
func (h *host) request(context.Context, api.Module, uint32, uint32, uint32, uint32, uint32, uint32, uint32,
	uint32, uint32, uint32, uint32, uint32, uint32, uint32,
) int32 {
	return 0
}

// handle performs the outbound request named by the handle and returns a
// handle to the response. Parameters b..hh are the HTTP options, which
// upstream does not implement either.
func (h *host) handle(ctx context.Context, _ api.Module, reqHandle, _, _, _, _, _, _, _ uint32) uint32 {
	h.mu.Lock()
	req, ok := h.requests[reqHandle]
	h.mu.Unlock()
	if !ok {
		return 0
	}

	var body io.Reader
	if req.body != nil {
		body = bytes.NewReader(req.body.Bytes())
	}
	// Unlike upstream, the guest's context rides along, so a cancelled or
	// deadlined guest call cancels the outbound request too.
	hreq, err := http.NewRequestWithContext(ctx, req.method, req.url(), body)
	if err != nil {
		return 0
	}
	h.mu.Lock()
	if header, found := h.fields[req.headers]; found {
		hreq.Header = header
	}
	h.mu.Unlock()

	hres, err := http.DefaultClient.Do(hreq)
	if err != nil {
		return 0
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	id := h.newID()
	h.responses[id] = &response{status: hres.StatusCode, header: hres.Header, body: hres.Body}
	return id
}

// --- types: fields ---------------------------------------------------------

// newFields reads count 16-byte [key_ptr][key_len][val_ptr][val_len] tuples
// and returns a handle to the resulting header set.
func (h *host) newFields(_ context.Context, mod api.Module, ptr, count uint32) uint32 {
	data, ok := mod.Memory().Read(ptr, count*16)
	if !ok {
		return 0
	}
	header := http.Header{}
	for i := uint32(0); i < count; i++ {
		tuple := data[i*16:]
		key, ok := readString(mod, le.Uint32(tuple), le.Uint32(tuple[4:]))
		if !ok {
			return 0
		}
		value, ok := readString(mod, le.Uint32(tuple[8:]), le.Uint32(tuple[12:]))
		if !ok {
			return 0
		}
		// Not http.Header.Add: the guest's keys go on the wire as written,
		// which is what the bindings on the other side expect.
		header[key] = append(header[key], value)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	id := h.newID()
	h.fields[id] = header
	return id
}

func (h *host) dropFields(_ context.Context, _ api.Module, handle uint32) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.fields, handle)
}

// fieldsEntries writes the header set as a list of key/value tuples.
//
// Only the first value of a repeated header is written, matching upstream.
func (h *host) fieldsEntries(ctx context.Context, mod api.Module, handle, outPtr uint32) {
	h.mu.Lock()
	header, found := h.fields[handle]
	h.mu.Unlock()
	if !found {
		return
	}

	ptr := malloc(ctx, mod, uint32(len(header))*16)
	writeU32s(mod, outPtr, ptr, uint32(len(header)))

	entry := make([]byte, 16)
	for key, values := range header {
		var value string
		if len(values) > 0 {
			value = values[0]
		}
		le.PutUint32(entry, allocString(ctx, mod, key))
		le.PutUint32(entry[4:], uint32(len(key)))
		le.PutUint32(entry[8:], allocString(ctx, mod, value))
		le.PutUint32(entry[12:], uint32(len(value)))
		write(mod, ptr, entry)
		ptr += 16
	}
}

// --- types: requests -------------------------------------------------------

// newOutgoingRequest records a request and returns its handle. scheme is an
// option<scheme>: scheme_is_some selects whether it is present, then scheme
// discriminates HTTP (0), HTTPS (1) and other (2), the last carrying a string.
func (h *host) newOutgoingRequest(_ context.Context, mod api.Module,
	method, _, _, // method_ptr and method_len: unused, as upstream
	pathPtr, pathLen,
	queryPtr, queryLen,
	schemeIsSome, scheme, schemePtr, schemeLen,
	authorityPtr, authorityLen, headerHandle uint32,
) uint32 {
	req := &request{scheme: "https", headers: headerHandle}

	if int(method) >= len(methods) {
		// Upstream calls log.Fatalf here, which would take the host process
		// down with it. Trapping stops the guest that lied instead.
		panic(fmt.Errorf("wasi_http: unknown method enum %d", method))
	}
	req.method = methods[method]

	var ok bool
	if req.path, ok = readString(mod, pathPtr, pathLen); !ok {
		return 0
	}
	if req.query, ok = readString(mod, queryPtr, queryLen); !ok {
		return 0
	}
	if schemeIsSome == 1 {
		switch scheme {
		case 0:
			req.scheme = "http"
		case 2:
			if req.scheme, ok = readString(mod, schemePtr, schemeLen); !ok {
				return 0
			}
		}
	}
	if req.authority, ok = readString(mod, authorityPtr, authorityLen); !ok {
		return 0
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	id := h.newID()
	h.requests[id] = req
	return id
}

func (h *host) dropOutgoingRequest(_ context.Context, _ api.Module, handle uint32) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.requests, handle)
}

func (h *host) dropIncomingRequest(_ context.Context, _ api.Module, handle uint32) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if req, found := h.requests[handle]; found {
		delete(h.fields, req.headers)
		delete(h.requests, handle)
	}
}

// outgoingRequestWrite hands the guest a stream to write the request body to.
func (h *host) outgoingRequestWrite(_ context.Context, mod api.Module, handle, ptr uint32) {
	h.mu.Lock()
	req, found := h.requests[handle]
	if !found {
		h.mu.Unlock()
		writeU32s(mod, ptr, 1, 0)
		return
	}
	req.body = &bytes.Buffer{}
	id := h.newID()
	h.streams[id] = stream{writer: req.body}
	h.mu.Unlock()

	writeU32s(mod, ptr, 0, id)
}

func (h *host) incomingRequestMethod(_ context.Context, mod api.Module, handle, ptr uint32) {
	h.mu.Lock()
	req, found := h.requests[handle]
	h.mu.Unlock()
	if !found {
		return
	}
	for enum, name := range methods {
		if name == req.method {
			writeU32s(mod, ptr, uint32(enum))
			return
		}
	}
	panic(fmt.Errorf("wasi_http: unknown method %q", req.method))
}

func (h *host) incomingRequestPath(ctx context.Context, mod api.Module, handle, ptr uint32) {
	h.mu.Lock()
	req, found := h.requests[handle]
	h.mu.Unlock()
	if found {
		writeString(ctx, mod, ptr, req.path)
	}
}

func (h *host) incomingRequestAuthority(ctx context.Context, mod api.Module, handle, ptr uint32) {
	h.mu.Lock()
	req, found := h.requests[handle]
	h.mu.Unlock()
	if found {
		writeString(ctx, mod, ptr, req.authority)
	}
}

func (h *host) incomingRequestHeaders(_ context.Context, _ api.Module, handle uint32) uint32 {
	h.mu.Lock()
	defer h.mu.Unlock()
	if req, found := h.requests[handle]; found {
		return req.headers
	}
	return 0
}

// incomingRequestConsume reports "no body": serving is not implemented.
func incomingRequestConsume(_ context.Context, mod api.Module, _, ptr uint32) {
	writeU32s(mod, ptr, 1, 0)
}

// --- types: responses ------------------------------------------------------

func (h *host) incomingResponseStatus(_ context.Context, _ api.Module, handle uint32) int32 {
	h.mu.Lock()
	defer h.mu.Unlock()
	if res, found := h.responses[handle]; found {
		return int32(res.status)
	}
	return 0
}

func (h *host) incomingResponseHeaders(_ context.Context, _ api.Module, handle uint32) uint32 {
	h.mu.Lock()
	defer h.mu.Unlock()
	res, found := h.responses[handle]
	if !found {
		return 0
	}
	if res.headers == 0 {
		res.headers = h.newID()
		h.fields[res.headers] = res.header
	}
	return res.headers
}

// incomingResponseConsume hands the guest a stream over the response body.
func (h *host) incomingResponseConsume(_ context.Context, mod api.Module, handle, ptr uint32) {
	h.mu.Lock()
	res, found := h.responses[handle]
	if !found || res.body == nil {
		h.mu.Unlock()
		writeU32s(mod, ptr, 1)
		return
	}
	id := h.newID()
	h.streams[id] = stream{reader: res.body}
	h.mu.Unlock()

	writeU32s(mod, ptr, 0, id)
}

func (h *host) dropIncomingResponse(_ context.Context, _ api.Module, handle uint32) {
	h.mu.Lock()
	res, found := h.responses[handle]
	delete(h.responses, handle)
	h.mu.Unlock()

	// Upstream leaks the body, and with it the connection. Close it.
	if found && res.body != nil {
		res.body.Close()
	}
}

// futureIncomingResponseGet resolves the future. handle is issued by an
// already-completed call to handle, so the response is simply itself:
// [is_some=1][is_err=0][handle].
func futureIncomingResponseGet(_ context.Context, mod api.Module, handle, ptr uint32) {
	writeU32s(mod, ptr, 1, 0, handle)
}

func (h *host) newOutgoingResponse(_ context.Context, _ api.Module, status, headers uint32) uint32 {
	h.mu.Lock()
	defer h.mu.Unlock()
	id := h.newID()
	h.responses[id] = &response{status: int(status), headers: headers}
	return id
}

// outgoingResponseWrite hands the guest a stream for the response body.
//
// Serving is not implemented, so nothing ever reads what the guest writes;
// the stream exists so a guest that writes a body still links and runs.
func (h *host) outgoingResponseWrite(_ context.Context, mod api.Module, handle, ptr uint32) {
	h.mu.Lock()
	_, found := h.responses[handle]
	if !found {
		h.mu.Unlock()
		writeU32s(mod, ptr, 1, 0)
		return
	}
	id := h.newID()
	h.streams[id] = stream{writer: io.Discard}
	h.mu.Unlock()

	writeU32s(mod, ptr, 0, id)
}

func (h *host) dropOutgoingResponse(_ context.Context, _ api.Module, handle uint32) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.responses, handle)
}

// setResponseOutparam records which response answers which outparam. err
// selects the error arm, whose detail message this ABI discards.
func (h *host) setResponseOutparam(_ context.Context, _ api.Module, res, err, resOut, _, _ uint32) uint32 {
	if err == 1 {
		return 1
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.outparams[res] = resOut
	return 0
}

// logIt prints a guest debug string, as upstream does, on the host's stdout.
func logIt(_ context.Context, mod api.Module, ptr, length uint32) {
	if s, ok := readString(mod, ptr, length); ok {
		fmt.Print(s)
	}
}

// --- streams ---------------------------------------------------------------

// streamRead reads up to length bytes and writes
// [is_err=0][ptr][len][more] to outPtr, where more is 0 at EOF.
func (h *host) streamRead(ctx context.Context, mod api.Module, handle uint32, length uint64, outPtr uint32) {
	h.mu.Lock()
	s, found := h.streams[handle]
	h.mu.Unlock()
	if !found || s.reader == nil {
		writeU32s(mod, outPtr, 1, 0, 0, 0)
		return
	}

	buf := make([]byte, length)
	n, err := s.reader.Read(buf)
	if err != nil && err != io.EOF {
		writeU32s(mod, outPtr, 1, 0, 0, 0)
		return
	}
	more := uint32(1)
	if err == io.EOF {
		more = 0
	}

	ptr := malloc(ctx, mod, uint32(n))
	write(mod, ptr, buf[:n])
	writeU32s(mod, outPtr, 0, ptr, uint32(n), more)
}

func (h *host) dropInputStream(_ context.Context, _ api.Module, handle uint32) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.streams, handle)
}

// streamWrite writes the guest's bytes and reports [is_err][n] to resultPtr.
func (h *host) streamWrite(_ context.Context, mod api.Module, handle, ptr, length, resultPtr uint32) {
	data, ok := mod.Memory().Read(ptr, length)
	if !ok {
		writeU32s(mod, resultPtr, 1, 0)
		return
	}
	h.mu.Lock()
	s, found := h.streams[handle]
	h.mu.Unlock()
	if !found || s.writer == nil {
		writeU32s(mod, resultPtr, 1, 0)
		return
	}
	n, err := s.writer.Write(data)
	if err != nil {
		writeU32s(mod, resultPtr, 1, uint32(n))
		return
	}
	writeU32s(mod, resultPtr, 0, uint32(n))
}

// --- guest memory ----------------------------------------------------------

var le = binary.LittleEndian

// malloc allocates size bytes in the guest by calling its exported
// cabi_realloc(0, 0, align=4, size). Guests built by wit-bindgen-era tooling
// all export it; one that does not cannot be served, so this traps.
func malloc(ctx context.Context, mod api.Module, size uint32) uint32 {
	realloc := mod.ExportedFunction("cabi_realloc")
	if realloc == nil {
		panic(fmt.Errorf("wasi_http: guest %q does not export cabi_realloc", mod.Name()))
	}
	stack := [4]uint64{0, 0, 4, uint64(size)}
	if err := realloc.CallWithStack(ctx, stack[:]); err != nil {
		panic(fmt.Errorf("wasi_http: cabi_realloc(%d) failed: %w", size, err))
	}
	return uint32(stack[0])
}

// allocString copies s into fresh guest memory and returns its address.
func allocString(ctx context.Context, mod api.Module, s string) uint32 {
	ptr := malloc(ctx, mod, uint32(len(s)))
	if !mod.Memory().WriteString(ptr, s) {
		panic(fmt.Errorf("wasi_http: writing %d bytes at %d is out of range", len(s), ptr))
	}
	return ptr
}

// writeString writes s into guest memory as an 8-byte [ptr][len] struct.
func writeString(ctx context.Context, mod api.Module, outPtr uint32, s string) {
	writeU32s(mod, outPtr, allocString(ctx, mod, s), uint32(len(s)))
}

// readString reads a guest string. It reports false if the range is out of
// bounds, which callers turn into the ABI's error value.
func readString(mod api.Module, ptr, length uint32) (string, bool) {
	data, ok := mod.Memory().Read(ptr, length)
	if !ok {
		return "", false
	}
	return string(data), true
}

// writeU32s writes little-endian u32s at ptr. Out of range means the guest
// gave a bad out pointer, which is not recoverable: trap.
func writeU32s(mod api.Module, ptr uint32, values ...uint32) {
	var buf [16]byte // the widest result in this ABI is 4 u32s
	for i, v := range values {
		le.PutUint32(buf[i*4:], v)
	}
	write(mod, ptr, buf[:len(values)*4])
}

func write(mod api.Module, ptr uint32, data []byte) {
	if !mod.Memory().Write(ptr, data) {
		panic(fmt.Errorf("wasi_http: writing %d bytes at %d is out of range", len(data), ptr))
	}
}
