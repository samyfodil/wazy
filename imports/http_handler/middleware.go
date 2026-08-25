package http_handler

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/imports/wasi_snapshot_preview1"
)

// Middleware implements the http-wasm handler ABI. It is scoped to a single
// guest binary.
type Middleware interface {
	// HandleRequest handles a request by calling FuncHandleRequest on the
	// guest.
	//
	// Note: If the CtxNext is returned with `next=1`, you must call
	// HandleResponse.
	HandleRequest(ctx context.Context) (outCtx context.Context, ctxNext CtxNext, err error)

	// HandleResponse handles a response by calling FuncHandleResponse on the
	// guest. This is only called when HandleRequest returns CtxNext with
	// `next=1`.
	//
	// The ctx and ctxNext parameters are those returned from HandleRequest.
	// Specifically, the CtxNext "ctx" field is passed as `reqCtx`. The err
	// parameter is nil unless the host erred processing the next handler.
	HandleResponse(ctx context.Context, reqCtx uint32, err error) error

	// Features are the features enabled while initializing the guest. This
	// value won't change per-request.
	Features() Features

	api.Closer
}

var _ Middleware = (*middleware)(nil)

type middleware struct {
	host         Host
	runtime      wazy.Runtime
	guestModule  wazy.CompiledModule
	moduleConfig wazy.ModuleConfig
	guestConfig  []byte
	logger       Logger
	pool         sync.Pool
	features     Features
	// instanceCounter names each guest instance. atomic.Uint64 rather than a
	// plain uint64 with atomic.AddUint64: sync/atomic needs a 64-bit-aligned
	// address on a platform whose int is 32 bits wide (GOARCH=386, arm, wasm),
	// and the compiler only guarantees that for a struct's first word. This
	// type carries its own alignment, so the field can sit anywhere.
	instanceCounter atomic.Uint64
}

func (m *middleware) Features() Features {
	return m.features
}

// NewMiddleware compiles guest, wires the "http_handler" host module to host,
// and returns a Middleware bound to that guest.
//
// The guest must export FuncHandleRequest, FuncHandleResponse and Memory. A
// guest that also imports WASI gets wazy's imports/wasi_snapshot_preview1
// instantiated automatically, unless the runtime already has it.
func NewMiddleware(ctx context.Context, guest []byte, host Host, opts ...Option) (Middleware, error) {
	o := &options{
		newRuntime:   DefaultRuntime,
		moduleConfig: wazy.NewModuleConfig(),
		logger:       NoopLogger{},
	}
	for _, opt := range opts {
		opt(o)
	}

	wr, err := o.newRuntime(ctx)
	if err != nil {
		return nil, fmt.Errorf("wasm: error creating middleware: %w", err)
	}

	m := &middleware{
		host:         host,
		runtime:      wr,
		moduleConfig: o.moduleConfig,
		guestConfig:  o.guestConfig,
		logger:       o.logger,
	}

	if m.guestModule, err = m.compileGuest(ctx, guest); err != nil {
		wr.Close(ctx)
		return nil, err
	}

	// Detect and handle any host imports or lack thereof.
	imports := detectImports(m.guestModule.ImportedFunctions())
	switch {
	case imports&importWasiP1 != 0:
		if m.runtime.Module(wasi_snapshot_preview1.ModuleName) == nil {
			if _, err = wasi_snapshot_preview1.Instantiate(ctx, m.runtime); err != nil {
				wr.Close(ctx)
				return nil, fmt.Errorf("wasm: error instantiating wasi: %w", err)
			}
		}

		fallthrough // proceed to configure any http_handler imports
	case imports&importHTTPHandler != 0:
		if _, err = m.instantiateHost(ctx); err != nil {
			wr.Close(ctx)
			return nil, fmt.Errorf("wasm: error instantiating host: %w", err)
		}
	}

	// Eagerly add one instance to the pool. Doing so helps to fail fast.
	if g, err := m.getOrCreateGuest(ctx); err != nil {
		wr.Close(ctx)
		return nil, err
	} else {
		m.pool.Put(g)
	}

	return m, nil
}

func (m *middleware) compileGuest(ctx context.Context, wasm []byte) (wazy.CompiledModule, error) {
	if guest, err := m.runtime.CompileModule(ctx, wasm); err != nil {
		return nil, fmt.Errorf("wasm: error compiling guest: %w", err)
	} else if handleRequest, ok := guest.ExportedFunctions()[FuncHandleRequest]; !ok {
		return nil, fmt.Errorf("wasm: guest doesn't export func[%s]", FuncHandleRequest)
	} else if len(handleRequest.ParamTypes()) != 0 || !bytes.Equal(handleRequest.ResultTypes(), []api.ValueType{api.ValueTypeI64}) {
		return nil, fmt.Errorf("wasm: guest exports the wrong signature for func[%s]. should be () -> (i64)", FuncHandleRequest)
	} else if handleResponse, ok := guest.ExportedFunctions()[FuncHandleResponse]; !ok {
		return nil, fmt.Errorf("wasm: guest doesn't export func[%s]", FuncHandleResponse)
	} else if !bytes.Equal(handleResponse.ParamTypes(), []api.ValueType{api.ValueTypeI32, api.ValueTypeI32}) || len(handleResponse.ResultTypes()) != 0 {
		return nil, fmt.Errorf("wasm: guest exports the wrong signature for func[%s]. should be (i32, 32) -> ()", FuncHandleResponse)
	} else if _, ok = guest.ExportedMemories()[Memory]; !ok {
		return nil, fmt.Errorf("wasm: guest doesn't export memory[%s]", Memory)
	} else {
		return guest, nil
	}
}

// HandleRequest implements Middleware.HandleRequest
func (m *middleware) HandleRequest(ctx context.Context) (outCtx context.Context, ctxNext CtxNext, err error) {
	g, guestErr := m.getOrCreateGuest(ctx)
	if guestErr != nil {
		err = guestErr
		return
	}

	s := &requestState{features: m.features, putPool: m.pool.Put, g: g}
	defer func() {
		if ctxNext != 0 { // will call the next handler
			if closeErr := s.closeRequest(); err == nil {
				err = closeErr
			}
		} else { // guest errored or returned the response
			if closeErr := s.Close(); err == nil {
				err = closeErr
			}
		}
	}()

	outCtx = context.WithValue(ctx, requestStateKey{}, s)
	ctxNext, err = g.handleRequest(outCtx)
	return
}

func (m *middleware) getOrCreateGuest(ctx context.Context) (*guest, error) {
	poolG := m.pool.Get()
	if poolG == nil {
		if g, createErr := m.newGuest(ctx); createErr != nil {
			return nil, createErr
		} else {
			// While closing the runtime will close the guest modules, when the
			// pool runs its own GC there are no guarantees that the guest
			// module will be closed. Hence, close it with a finalizer.
			runtime.SetFinalizer(g, func(g *guest) {
				if err := g.guest.Close(context.Background()); err != nil {
					m.logger.Log(ctx, LogLevelError, fmt.Sprintf("closing guest module: %v", err))
				} else {
					g.guest = nil
					g.handleRequestFn = nil
					g.handleResponseFn = nil
				}
			})

			poolG = g
		}
	}
	return poolG.(*guest), nil
}

// HandleResponse implements Middleware.HandleResponse
func (m *middleware) HandleResponse(ctx context.Context, reqCtx uint32, hostErr error) error {
	s := requestStateFromContext(ctx)
	defer s.Close()
	s.afterNext = true

	return s.g.handleResponse(ctx, reqCtx, hostErr)
}

// Close implements api.Closer
func (m *middleware) Close(ctx context.Context) error {
	// We don't have to close any guests as the runtime will close them.
	return m.runtime.Close(ctx)
}

type guest struct {
	guest            api.Module
	handleRequestFn  api.Function
	handleResponseFn api.Function
}

func (m *middleware) newGuest(ctx context.Context) (*guest, error) {
	moduleName := strconv.FormatUint(m.instanceCounter.Add(1), 10)

	g, err := m.runtime.InstantiateModule(ctx, m.guestModule, m.moduleConfig.WithName(moduleName))
	if err != nil {
		m.runtime.Close(ctx)
		return nil, fmt.Errorf("wasm: error instantiating guest: %w", err)
	}

	return &guest{
		guest:            g,
		handleRequestFn:  g.ExportedFunction(FuncHandleRequest),
		handleResponseFn: g.ExportedFunction(FuncHandleResponse),
	}, nil
}

// handleRequest calls the WebAssembly guest function FuncHandleRequest.
func (g *guest) handleRequest(ctx context.Context) (ctxNext CtxNext, err error) {
	if results, guestErr := g.handleRequestFn.Call(ctx); guestErr != nil {
		err = guestErr
	} else {
		ctxNext = CtxNext(results[0])
	}
	return
}

// handleResponse calls the WebAssembly guest function FuncHandleResponse.
func (g *guest) handleResponse(ctx context.Context, reqCtx uint32, err error) error {
	wasError := uint64(0)
	if err != nil {
		wasError = 1
	}
	_, err = g.handleResponseFn.Call(ctx, uint64(reqCtx), wasError)
	return err
}

// enableFeatures implements the WebAssembly host function FuncEnableFeatures.
func (m *middleware) enableFeatures(ctx context.Context, stack []uint64) {
	features := Features(stack[0])

	var enabled Features
	if s, ok := ctx.Value(requestStateKey{}).(*requestState); ok {
		s.features = m.host.EnableFeatures(ctx, s.features.WithEnabled(features))
		enabled = s.features
	} else {
		m.features = m.host.EnableFeatures(ctx, m.features.WithEnabled(features))
		enabled = m.features
	}

	stack[0] = uint64(enabled)
}

// getConfig implements the WebAssembly host function FuncGetConfig.
func (m *middleware) getConfig(_ context.Context, mod api.Module, stack []uint64) {
	buf := uint32(stack[0])
	bufLimit := BufLimit(stack[1])

	configLen := writeIfUnderLimit(mod.Memory(), buf, bufLimit, m.guestConfig)

	stack[0] = uint64(configLen)
}

// logEnabled implements the WebAssembly host function FuncLogEnabled.
func (m *middleware) logEnabled(_ context.Context, stack []uint64) {
	level := LogLevel(stack[0])
	if m.logger.IsEnabled(level) {
		stack[0] = 1 // true
	} else {
		stack[0] = 0 // false
	}
}

// log implements the WebAssembly host function FuncLog.
func (m *middleware) log(ctx context.Context, mod api.Module, params []uint64) {
	level := LogLevel(params[0])
	message := uint32(params[1])
	messageLen := uint32(params[2])

	if !m.logger.IsEnabled(level) {
		return
	}
	var msg string
	if messageLen > 0 {
		msg = mustReadString(mod.Memory(), "message", message, messageLen)
	}
	m.logger.Log(ctx, level, msg)
}

// getMethod implements the WebAssembly host function FuncGetMethod.
func (m *middleware) getMethod(ctx context.Context, mod api.Module, stack []uint64) {
	buf := uint32(stack[0])
	bufLimit := BufLimit(stack[1])

	method := m.host.GetMethod(ctx)
	methodLen := writeStringIfUnderLimit(mod.Memory(), buf, bufLimit, method)

	stack[0] = uint64(methodLen)
}

// setMethod implements the WebAssembly host function FuncSetMethod.
func (m *middleware) setMethod(ctx context.Context, mod api.Module, params []uint64) {
	method := uint32(params[0])
	methodLen := uint32(params[1])

	_ = mustBeforeNext(ctx, "set", "method")

	var p string
	if methodLen == 0 {
		panic("HTTP method cannot be empty")
	}
	p = mustReadString(mod.Memory(), "method", method, methodLen)
	m.host.SetMethod(ctx, p)
}

// getURI implements the WebAssembly host function FuncGetURI.
func (m *middleware) getURI(ctx context.Context, mod api.Module, stack []uint64) {
	buf := uint32(stack[0])
	bufLimit := BufLimit(stack[1])

	uri := m.host.GetURI(ctx)
	uriLen := writeStringIfUnderLimit(mod.Memory(), buf, bufLimit, uri)

	stack[0] = uint64(uriLen)
}

// setURI implements the WebAssembly host function FuncSetURI.
func (m *middleware) setURI(ctx context.Context, mod api.Module, params []uint64) {
	uri := uint32(params[0])
	uriLen := uint32(params[1])

	_ = mustBeforeNext(ctx, "set", "uri")

	var p string
	if uriLen > 0 { // overwrite with empty is supported
		p = mustReadString(mod.Memory(), "uri", uri, uriLen)
	}
	m.host.SetURI(ctx, p)
}

// getProtocolVersion implements the WebAssembly host function
// FuncGetProtocolVersion.
func (m *middleware) getProtocolVersion(ctx context.Context, mod api.Module, stack []uint64) {
	buf := uint32(stack[0])
	bufLimit := BufLimit(stack[1])

	protocolVersion := m.host.GetProtocolVersion(ctx)
	if len(protocolVersion) == 0 {
		panic("HTTP protocol version cannot be empty")
	}
	protocolVersionLen := writeStringIfUnderLimit(mod.Memory(), buf, bufLimit, protocolVersion)

	stack[0] = uint64(protocolVersionLen)
}

// getHeaderNames implements the WebAssembly host function FuncGetHeaderNames.
func (m *middleware) getHeaderNames(ctx context.Context, mod api.Module, stack []uint64) {
	kind := HeaderKind(stack[0])
	buf := uint32(stack[1])
	bufLimit := BufLimit(stack[2])

	var names []string
	switch kind {
	case HeaderKindRequest:
		names = m.host.GetRequestHeaderNames(ctx)
	case HeaderKindRequestTrailers:
		names = m.host.GetRequestTrailerNames(ctx)
	case HeaderKindResponse:
		names = m.host.GetResponseHeaderNames(ctx)
	case HeaderKindResponseTrailers:
		names = m.host.GetResponseTrailerNames(ctx)
	default:
		panic("unsupported header kind: " + strconv.Itoa(int(kind)))
	}

	for i := range names {
		names[i] = strings.ToLower(names[i])
	}

	countLen := writeNULTerminated(mod.Memory(), buf, bufLimit, names)

	stack[0] = countLen
}

// getHeaderValues implements the WebAssembly host function
// FuncGetHeaderValues.
func (m *middleware) getHeaderValues(ctx context.Context, mod api.Module, stack []uint64) {
	kind := HeaderKind(stack[0])
	name := uint32(stack[1])
	nameLen := uint32(stack[2])
	buf := uint32(stack[3])
	bufLimit := BufLimit(stack[4])

	if nameLen == 0 {
		panic("HTTP header name cannot be empty")
	}
	n := mustReadString(mod.Memory(), "name", name, nameLen)

	var values []string
	switch kind {
	case HeaderKindRequest:
		values = m.host.GetRequestHeaderValues(ctx, n)
	case HeaderKindRequestTrailers:
		values = m.host.GetRequestTrailerValues(ctx, n)
	case HeaderKindResponse:
		values = m.host.GetResponseHeaderValues(ctx, n)
	case HeaderKindResponseTrailers:
		values = m.host.GetResponseTrailerValues(ctx, n)
	default:
		panic("unsupported header kind: " + strconv.Itoa(int(kind)))
	}
	countLen := writeNULTerminated(mod.Memory(), buf, bufLimit, values)

	stack[0] = countLen
}

// setHeaderValue implements the WebAssembly host function FuncSetHeaderValue.
func (m *middleware) setHeaderValue(ctx context.Context, mod api.Module, params []uint64) {
	kind := HeaderKind(params[0])
	name := uint32(params[1])
	nameLen := uint32(params[2])
	value := uint32(params[3])
	valueLen := uint32(params[4])

	if nameLen == 0 {
		panic("HTTP header name cannot be empty")
	}
	mustHeaderMutable(ctx, "set", kind)
	n := mustReadString(mod.Memory(), "name", name, nameLen)
	v := mustReadString(mod.Memory(), "value", value, valueLen)

	switch kind {
	case HeaderKindRequest:
		m.host.SetRequestHeaderValue(ctx, n, v)
	case HeaderKindRequestTrailers:
		m.host.SetRequestTrailerValue(ctx, n, v)
	case HeaderKindResponse:
		m.host.SetResponseHeaderValue(ctx, n, v)
	case HeaderKindResponseTrailers:
		m.host.SetResponseTrailerValue(ctx, n, v)
	default:
		panic("unsupported header kind: " + strconv.Itoa(int(kind)))
	}
}

// addHeaderValue implements the WebAssembly host function FuncAddHeaderValue.
func (m *middleware) addHeaderValue(ctx context.Context, mod api.Module, params []uint64) {
	kind := HeaderKind(params[0])
	name := uint32(params[1])
	nameLen := uint32(params[2])
	value := uint32(params[3])
	valueLen := uint32(params[4])

	if nameLen == 0 {
		panic("HTTP header name cannot be empty")
	}
	mustHeaderMutable(ctx, "add", kind)
	n := mustReadString(mod.Memory(), "name", name, nameLen)
	v := mustReadString(mod.Memory(), "value", value, valueLen)

	switch kind {
	case HeaderKindRequest:
		m.host.AddRequestHeaderValue(ctx, n, v)
	case HeaderKindRequestTrailers:
		m.host.AddRequestTrailerValue(ctx, n, v)
	case HeaderKindResponse:
		m.host.AddResponseHeaderValue(ctx, n, v)
	case HeaderKindResponseTrailers:
		m.host.AddResponseTrailerValue(ctx, n, v)
	default:
		panic("unsupported header kind: " + strconv.Itoa(int(kind)))
	}
}

// removeHeader implements the WebAssembly host function FuncRemoveHeader.
func (m *middleware) removeHeader(ctx context.Context, mod api.Module, params []uint64) {
	kind := HeaderKind(params[0])
	name := uint32(params[1])
	nameLen := uint32(params[2])

	if nameLen == 0 {
		panic("HTTP header name cannot be empty")
	}
	mustHeaderMutable(ctx, "remove", kind)
	n := mustReadString(mod.Memory(), "name", name, nameLen)

	switch kind {
	case HeaderKindRequest:
		m.host.RemoveRequestHeader(ctx, n)
	case HeaderKindRequestTrailers:
		m.host.RemoveRequestTrailer(ctx, n)
	case HeaderKindResponse:
		m.host.RemoveResponseHeader(ctx, n)
	case HeaderKindResponseTrailers:
		m.host.RemoveResponseTrailer(ctx, n)
	default:
		panic("unsupported header kind: " + strconv.Itoa(int(kind)))
	}
}

// readBody implements the WebAssembly host function FuncReadBody.
func (m *middleware) readBody(ctx context.Context, mod api.Module, stack []uint64) {
	kind := BodyKind(stack[0])
	buf := uint32(stack[1])
	bufLimit := BufLimit(stack[2])

	var r io.ReadCloser
	switch kind {
	case BodyKindRequest:
		s := mustBeforeNextOrFeature(ctx, FeatureBufferRequest, "read", "request body")
		// Lazy create the reader.
		r = s.requestBodyReader
		if r == nil {
			r = m.host.RequestBodyReader(ctx)
			s.requestBodyReader = r
		}
	case BodyKindResponse:
		s := mustBeforeNextOrFeature(ctx, FeatureBufferResponse, "read", "response body")
		// Lazy create the reader.
		r = s.responseBodyReader
		if r == nil {
			r = m.host.ResponseBodyReader(ctx)
			s.responseBodyReader = r
		}
	default:
		panic("unsupported body kind: " + strconv.Itoa(int(kind)))
	}

	eofLen := readBody(mod, buf, bufLimit, r)

	stack[0] = eofLen
}

// writeBody implements the WebAssembly host function FuncWriteBody.
func (m *middleware) writeBody(ctx context.Context, mod api.Module, params []uint64) {
	kind := BodyKind(params[0])
	buf := uint32(params[1])
	bufLen := uint32(params[2])

	var w io.Writer
	switch kind {
	case BodyKindRequest:
		s := mustBeforeNext(ctx, "write", "request body")
		// Lazy create the writer.
		w = s.requestBodyWriter
		if w == nil {
			w = m.host.RequestBodyWriter(ctx)
			s.requestBodyWriter = w
		}
	case BodyKindResponse:
		s := mustBeforeNextOrFeature(ctx, FeatureBufferResponse, "write", "response body")
		// Lazy create the writer.
		w = s.responseBodyWriter
		if w == nil {
			w = m.host.ResponseBodyWriter(ctx)
			s.responseBodyWriter = w
		}
	default:
		panic("unsupported body kind: " + strconv.Itoa(int(kind)))
	}

	writeBody(mod, buf, bufLen, w)
}

// getSourceAddr implements the WebAssembly host function FuncGetSourceAddr.
func (m *middleware) getSourceAddr(ctx context.Context, mod api.Module, stack []uint64) {
	buf := uint32(stack[0])
	bufLimit := BufLimit(stack[1])

	sourceAddr := m.host.GetSourceAddr(ctx)
	sourceAddrLen := writeStringIfUnderLimit(mod.Memory(), buf, bufLimit, sourceAddr)

	stack[0] = uint64(sourceAddrLen)
}

// getStatusCode implements the WebAssembly host function FuncGetStatusCode.
func (m *middleware) getStatusCode(ctx context.Context, results []uint64) {
	statusCode := m.host.GetStatusCode(ctx)

	results[0] = uint64(statusCode)
}

// setStatusCode implements the WebAssembly host function FuncSetStatusCode.
func (m *middleware) setStatusCode(ctx context.Context, params []uint64) {
	statusCode := uint32(params[0])

	_ = mustBeforeNextOrFeature(ctx, FeatureBufferResponse, "set", "status code")

	m.host.SetStatusCode(ctx, statusCode)
}

func writeBody(mod api.Module, buf, bufLen uint32, w io.Writer) {
	// buf_len 0 means to overwrite with nothing
	var b []byte
	if bufLen > 0 {
		b = mustRead(mod.Memory(), "body", buf, bufLen)
	}
	if _, err := w.Write(b); err != nil { // Write errs if it can't write n bytes
		panic(fmt.Errorf("error writing body: %w", err))
	}
}

func readBody(mod api.Module, buf uint32, bufLimit BufLimit, r io.Reader) (eofLen uint64) {
	// buf_limit 0 serves no purpose as implementations won't return EOF on it.
	if bufLimit == 0 {
		panic(fmt.Errorf("buf_limit==0 reading body"))
	}

	// Allocate a buf to write into directly
	b := mustRead(mod.Memory(), "body", buf, bufLimit)

	// Attempt to fill the buffer until an error occurs.
	var err error
	n := uint32(0)
	for n < bufLimit && err == nil {
		var nn int
		nn, err = r.Read(b[n:])
		n += uint32(nn)
	}

	if err == nil {
		return uint64(n) // Not EOF
	} else if err == io.EOF { // EOF is by contract, so can't be wrapped
		return uint64(1<<32) | uint64(n)
	} else {
		panic(fmt.Errorf("error reading body: %w", err))
	}
}

func mustBeforeNext(ctx context.Context, op, kind string) (s *requestState) {
	if s = requestStateFromContext(ctx); s.afterNext {
		panic(fmt.Errorf("can't %s %s after next handler", op, kind))
	}
	return
}

func mustBeforeNextOrFeature(ctx context.Context, feature Features, op, kind string) (s *requestState) {
	if s = requestStateFromContext(ctx); !s.afterNext {
		// Assume this is serving a response from the guest.
	} else if s.features.IsEnabled(feature) {
		// Assume the guest is overwriting the response from next.
	} else {
		panic(fmt.Errorf("can't %s %s after next handler unless %s is enabled",
			op, kind, feature))
	}
	return
}

func mustHeaderMutable(ctx context.Context, op string, kind HeaderKind) {
	switch kind {
	case HeaderKindRequest:
		_ = mustBeforeNext(ctx, op, "request header")
	case HeaderKindRequestTrailers:
		_ = mustBeforeNext(ctx, op, "request trailer")
	case HeaderKindResponse:
		_ = mustBeforeNextOrFeature(ctx, FeatureBufferResponse, op, "response header")
	case HeaderKindResponseTrailers:
		_ = mustBeforeNextOrFeature(ctx, FeatureBufferResponse, op, "response trailer")
	default:
		panic("unsupported header kind: " + strconv.Itoa(int(kind)))
	}
}

// writeNULTerminated writes the NUL-terminated sequence to memory, if it fits
// under bufLimit, and returns the CountLen either way.
func writeNULTerminated(mem api.Memory, buf uint32, bufLimit BufLimit, input []string) (countLen CountLen) {
	count := uint32(len(input))
	if count == 0 {
		return
	}

	byteCount := count // NUL terminator count
	for _, s := range input {
		byteCount += uint32(len(s))
	}

	countLen = CountLen(count)<<32 | CountLen(byteCount)

	if byteCount > bufLimit {
		return // the guest can retry with a larger limit
	}

	// Write the NUL-terminated string to memory directly.
	s, ok := mem.Read(buf, byteCount)
	if !ok {
		panic("out of memory") // the guest passed a region outside memory.
	}

	b := bytes.NewBuffer(s)
	b.Reset()
	for _, h := range input {
		b.WriteString(h)
		b.WriteByte(0)
	}
	return
}

// mustReadString is a convenience function that casts mustRead
func mustReadString(mem api.Memory, fieldName string, offset, byteCount uint32) string {
	if byteCount == 0 {
		return ""
	}
	return string(mustRead(mem, fieldName, offset, byteCount))
}

var emptyBody = make([]byte, 0)

// mustRead is like api.Memory except that it panics if the offset and
// byteCount are out of range.
func mustRead(mem api.Memory, fieldName string, offset, byteCount uint32) []byte {
	if byteCount == 0 {
		return emptyBody
	}
	buf, ok := mem.Read(offset, byteCount)
	if !ok {
		panic(fmt.Errorf("out of memory reading %s", fieldName))
	}
	return buf
}

func writeIfUnderLimit(mem api.Memory, offset, limit BufLimit, v []byte) (vLen uint32) {
	vLen = uint32(len(v))
	if vLen > limit {
		return // caller can retry with a larger limit
	} else if vLen == 0 {
		return // nothing to write
	}
	mem.Write(offset, v)
	return
}

func writeStringIfUnderLimit(mem api.Memory, offset, limit BufLimit, v string) (vLen uint32) {
	vLen = uint32(len(v))
	if vLen > limit {
		return // caller can retry with a larger limit
	} else if vLen == 0 {
		return // nothing to write
	}
	mem.WriteString(offset, v)
	return
}

const i32, i64 = api.ValueTypeI32, api.ValueTypeI64

func (m *middleware) instantiateHost(ctx context.Context) (api.Module, error) {
	return m.runtime.NewHostModuleBuilder(HostModule).
		NewFunctionBuilder().
		WithGoFunction(api.GoFunc(m.enableFeatures), []api.ValueType{i32}, []api.ValueType{i32}).
		WithParameterNames("features").Export(FuncEnableFeatures).
		NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(m.getConfig), []api.ValueType{i32, i32}, []api.ValueType{i32}).
		WithParameterNames("buf", "buf_limit").Export(FuncGetConfig).
		NewFunctionBuilder().
		WithGoFunction(api.GoFunc(m.logEnabled), []api.ValueType{i32}, []api.ValueType{i32}).
		WithParameterNames("level").Export(FuncLogEnabled).
		NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(m.log), []api.ValueType{i32, i32, i32}, []api.ValueType{}).
		WithParameterNames("level", "message", "message_len").Export(FuncLog).
		NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(m.getMethod), []api.ValueType{i32, i32}, []api.ValueType{i32}).
		WithParameterNames("buf", "buf_limit").Export(FuncGetMethod).
		NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(m.setMethod), []api.ValueType{i32, i32}, []api.ValueType{}).
		WithParameterNames("method", "method_len").Export(FuncSetMethod).
		NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(m.getURI), []api.ValueType{i32, i32}, []api.ValueType{i32}).
		WithParameterNames("buf", "buf_limit").Export(FuncGetURI).
		NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(m.setURI), []api.ValueType{i32, i32}, []api.ValueType{}).
		WithParameterNames("uri", "uri_len").Export(FuncSetURI).
		NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(m.getProtocolVersion), []api.ValueType{i32, i32}, []api.ValueType{i32}).
		WithParameterNames("buf", "buf_limit").Export(FuncGetProtocolVersion).
		NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(m.getHeaderNames), []api.ValueType{i32, i32, i32}, []api.ValueType{i64}).
		WithParameterNames("kind", "buf", "buf_limit").Export(FuncGetHeaderNames).
		NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(m.getHeaderValues), []api.ValueType{i32, i32, i32, i32, i32}, []api.ValueType{i64}).
		WithParameterNames("kind", "name", "name_len", "buf", "buf_limit").Export(FuncGetHeaderValues).
		NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(m.setHeaderValue), []api.ValueType{i32, i32, i32, i32, i32}, []api.ValueType{}).
		WithParameterNames("kind", "name", "name_len", "value", "value_len").Export(FuncSetHeaderValue).
		NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(m.addHeaderValue), []api.ValueType{i32, i32, i32, i32, i32}, []api.ValueType{}).
		WithParameterNames("kind", "name", "name_len", "value", "value_len").Export(FuncAddHeaderValue).
		NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(m.removeHeader), []api.ValueType{i32, i32, i32}, []api.ValueType{}).
		WithParameterNames("kind", "name", "name_len").Export(FuncRemoveHeader).
		NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(m.readBody), []api.ValueType{i32, i32, i32}, []api.ValueType{i64}).
		WithParameterNames("kind", "buf", "buf_limit").Export(FuncReadBody).
		NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(m.writeBody), []api.ValueType{i32, i32, i32}, []api.ValueType{}).
		WithParameterNames("kind", "body", "body_len").Export(FuncWriteBody).
		NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(m.getSourceAddr), []api.ValueType{i32, i32}, []api.ValueType{i32}).
		WithParameterNames("buf", "buf_limit").Export(FuncGetSourceAddr).
		NewFunctionBuilder().
		WithGoFunction(api.GoFunc(m.getStatusCode), []api.ValueType{}, []api.ValueType{i32}).
		WithParameterNames().Export(FuncGetStatusCode).
		NewFunctionBuilder().
		WithGoFunction(api.GoFunc(m.setStatusCode), []api.ValueType{i32}, []api.ValueType{}).
		WithParameterNames("status_code").Export(FuncSetStatusCode).
		Instantiate(ctx)
}

type imports uint

const (
	importWasiP1 imports = 1 << iota
	importHTTPHandler
)

func detectImports(importedFns []api.FunctionDefinition) (imports imports) {
	for _, f := range importedFns {
		moduleName, _, _ := f.Import()
		switch moduleName {
		case HostModule:
			imports |= importHTTPHandler
		case wasi_snapshot_preview1.ModuleName:
			imports |= importWasiP1
		}
	}
	return
}
