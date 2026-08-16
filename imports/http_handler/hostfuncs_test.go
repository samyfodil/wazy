package http_handler

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/testing/require"
)

// These drive the host functions directly, the way the guest's stack reaches
// them. The real guests (the TCK and the examples in the nethttp package)
// prove the wiring; this proves every kind, arm and panic.

// testHost records what the middleware asked of it.
type testHost struct {
	UnimplementedHost

	method, uri, protocol, sourceAddr string
	statusCode                        uint32
	features                          Features

	names  map[HeaderKind][]string
	values map[HeaderKind][]string

	set, add, removed []string // "kind name=value" traces

	requestBody  *bytes.Buffer
	responseBody *bytes.Buffer
	readerErr    error
}

func newTestHost() *testHost {
	return &testHost{
		method: "GET", uri: "/", protocol: "HTTP/1.1", sourceAddr: "1.2.3.4:5678",
		statusCode: 200,
		names: map[HeaderKind][]string{
			HeaderKindRequest:          {"Accept"},
			HeaderKindResponse:         {"Content-Type"},
			HeaderKindRequestTrailers:  {"Grpc-Status"},
			HeaderKindResponseTrailers: {"Grpc-Message"},
		},
		values: map[HeaderKind][]string{
			HeaderKindRequest:          {"text/plain"},
			HeaderKindResponse:         {"application/json"},
			HeaderKindRequestTrailers:  {"1"},
			HeaderKindResponseTrailers: {"ok"},
		},
		requestBody:  bytes.NewBufferString("request body"),
		responseBody: bytes.NewBufferString("response body"),
	}
}

func (h *testHost) EnableFeatures(_ context.Context, f Features) Features { h.features = f; return f }
func (h *testHost) GetMethod(context.Context) string                      { return h.method }
func (h *testHost) SetMethod(_ context.Context, m string)                 { h.method = m }
func (h *testHost) GetURI(context.Context) string                         { return h.uri }
func (h *testHost) SetURI(_ context.Context, u string)                    { h.uri = u }
func (h *testHost) GetProtocolVersion(context.Context) string             { return h.protocol }
func (h *testHost) GetSourceAddr(context.Context) string                  { return h.sourceAddr }
func (h *testHost) GetStatusCode(context.Context) uint32                  { return h.statusCode }
func (h *testHost) SetStatusCode(_ context.Context, s uint32)             { h.statusCode = s }

func (h *testHost) GetRequestHeaderNames(context.Context) []string { return h.names[HeaderKindRequest] }
func (h *testHost) GetResponseHeaderNames(context.Context) []string {
	return h.names[HeaderKindResponse]
}

func (h *testHost) GetRequestTrailerNames(context.Context) []string {
	return h.names[HeaderKindRequestTrailers]
}

func (h *testHost) GetResponseTrailerNames(context.Context) []string {
	return h.names[HeaderKindResponseTrailers]
}

func (h *testHost) GetRequestHeaderValues(context.Context, string) []string {
	return h.values[HeaderKindRequest]
}

func (h *testHost) GetResponseHeaderValues(context.Context, string) []string {
	return h.values[HeaderKindResponse]
}

func (h *testHost) GetRequestTrailerValues(context.Context, string) []string {
	return h.values[HeaderKindRequestTrailers]
}

func (h *testHost) GetResponseTrailerValues(context.Context, string) []string {
	return h.values[HeaderKindResponseTrailers]
}

func (h *testHost) SetRequestHeaderValue(_ context.Context, n, v string) {
	h.set = append(h.set, "request "+n+"="+v)
}

func (h *testHost) SetResponseHeaderValue(_ context.Context, n, v string) {
	h.set = append(h.set, "response "+n+"="+v)
}

func (h *testHost) SetRequestTrailerValue(_ context.Context, n, v string) {
	h.set = append(h.set, "request-trailer "+n+"="+v)
}

func (h *testHost) SetResponseTrailerValue(_ context.Context, n, v string) {
	h.set = append(h.set, "response-trailer "+n+"="+v)
}

func (h *testHost) AddRequestHeaderValue(_ context.Context, n, v string) {
	h.add = append(h.add, "request "+n+"="+v)
}

func (h *testHost) AddResponseHeaderValue(_ context.Context, n, v string) {
	h.add = append(h.add, "response "+n+"="+v)
}

func (h *testHost) AddRequestTrailerValue(_ context.Context, n, v string) {
	h.add = append(h.add, "request-trailer "+n+"="+v)
}

func (h *testHost) AddResponseTrailerValue(_ context.Context, n, v string) {
	h.add = append(h.add, "response-trailer "+n+"="+v)
}

func (h *testHost) RemoveRequestHeader(_ context.Context, n string) {
	h.removed = append(h.removed, "request "+n)
}

func (h *testHost) RemoveResponseHeader(_ context.Context, n string) {
	h.removed = append(h.removed, "response "+n)
}

func (h *testHost) RemoveRequestTrailer(_ context.Context, n string) {
	h.removed = append(h.removed, "request-trailer "+n)
}

func (h *testHost) RemoveResponseTrailer(_ context.Context, n string) {
	h.removed = append(h.removed, "response-trailer "+n)
}

func (h *testHost) RequestBodyReader(context.Context) io.ReadCloser {
	if h.readerErr != nil {
		return io.NopCloser(errReader{h.readerErr})
	}
	return io.NopCloser(h.requestBody)
}

func (h *testHost) ResponseBodyReader(context.Context) io.ReadCloser {
	return io.NopCloser(h.responseBody)
}
func (h *testHost) RequestBodyWriter(context.Context) io.Writer  { return h.requestBody }
func (h *testHost) ResponseBodyWriter(context.Context) io.Writer { return h.responseBody }

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

// testFixture is a middleware wired to a testHost, plus a guest memory.
type testFixture struct {
	t   *testing.T
	m   *middleware
	h   *testHost
	mod api.Module
	ctx context.Context
	s   *requestState
}

func newFixture(t *testing.T, features Features, afterNext bool) *testFixture {
	t.Helper()
	h := newTestHost()
	m := &middleware{host: h, logger: NoopLogger{}}
	s := &requestState{features: features, afterNext: afterNext, putPool: func(any) {}}
	return &testFixture{
		t: t, m: m, h: h, s: s, mod: newTestMemory(t),
		ctx: context.WithValue(testCtx, requestStateKey{}, s),
	}
}

// write puts s at offset and returns (offset, len) as the ABI passes them.
func (f *testFixture) write(offset uint32, s string) (uint32, uint32) {
	f.t.Helper()
	require.True(f.t, f.mod.Memory().WriteString(offset, s))
	return offset, uint32(len(s))
}

// read returns byteCount bytes at offset.
func (f *testFixture) read(offset, byteCount uint32) string {
	f.t.Helper()
	b, ok := f.mod.Memory().Read(offset, byteCount)
	require.True(f.t, ok)
	return string(b)
}

func TestGetters(t *testing.T) {
	tests := []struct {
		name     string
		fn       func(*testFixture) func(context.Context, api.Module, []uint64)
		expected string
	}{
		{"get_method", func(f *testFixture) func(context.Context, api.Module, []uint64) { return f.m.getMethod }, "GET"},
		{"get_uri", func(f *testFixture) func(context.Context, api.Module, []uint64) { return f.m.getURI }, "/"},
		{"get_protocol_version", func(f *testFixture) func(context.Context, api.Module, []uint64) {
			return f.m.getProtocolVersion
		}, "HTTP/1.1"},
		{"get_source_addr", func(f *testFixture) func(context.Context, api.Module, []uint64) {
			return f.m.getSourceAddr
		}, "1.2.3.4:5678"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, 0, false)
			fn := tc.fn(f)

			// Under the limit, the value is written.
			stack := []uint64{64, uint64(len(tc.expected))}
			fn(f.ctx, f.mod, stack)
			require.Equal(t, uint64(len(tc.expected)), stack[0])
			require.Equal(t, tc.expected, f.read(64, uint32(len(tc.expected))))

			// Over the limit, the length is reported and nothing is written.
			require.True(t, f.mod.Memory().Write(128, make([]byte, len(tc.expected))))
			stack = []uint64{128, uint64(len(tc.expected) - 1)}
			fn(f.ctx, f.mod, stack)
			require.Equal(t, uint64(len(tc.expected)), stack[0])
			require.Equal(t, strings.Repeat("\x00", len(tc.expected)), f.read(128, uint32(len(tc.expected))))
		})
	}
}

func TestGetProtocolVersion_empty(t *testing.T) {
	f := newFixture(t, 0, false)
	f.h.protocol = ""
	err := require.CapturePanic(func() { f.m.getProtocolVersion(f.ctx, f.mod, []uint64{0, 8}) })
	require.EqualError(t, err, "HTTP protocol version cannot be empty")
}

func TestSetMethod(t *testing.T) {
	f := newFixture(t, 0, false)
	ptr, l := f.write(64, "POST")
	f.m.setMethod(f.ctx, f.mod, []uint64{uint64(ptr), uint64(l)})
	require.Equal(t, "POST", f.h.method)

	// An empty method is invalid.
	err := require.CapturePanic(func() { f.m.setMethod(f.ctx, f.mod, []uint64{0, 0}) })
	require.EqualError(t, err, "HTTP method cannot be empty")

	// After next, the request is already sent.
	after := newFixture(t, 0, true)
	ptr, l = after.write(64, "POST")
	err = require.CapturePanic(func() { after.m.setMethod(after.ctx, after.mod, []uint64{uint64(ptr), uint64(l)}) })
	require.EqualError(t, err, "can't set method after next handler")
}

func TestSetURI(t *testing.T) {
	f := newFixture(t, 0, false)
	ptr, l := f.write(64, "/a?b=c")
	f.m.setURI(f.ctx, f.mod, []uint64{uint64(ptr), uint64(l)})
	require.Equal(t, "/a?b=c", f.h.uri)

	// Unlike the method, an empty URI is allowed.
	f.m.setURI(f.ctx, f.mod, []uint64{0, 0})
	require.Equal(t, "", f.h.uri)
}

// allKinds is every HeaderKind, with the trace prefix testHost records.
var allKinds = []struct {
	kind   HeaderKind
	prefix string
}{
	{HeaderKindRequest, "request"},
	{HeaderKindResponse, "response"},
	{HeaderKindRequestTrailers, "request-trailer"},
	{HeaderKindResponseTrailers, "response-trailer"},
}

func TestGetHeaderNames(t *testing.T) {
	for _, tc := range allKinds {
		t.Run(tc.prefix, func(t *testing.T) {
			f := newFixture(t, 0, false)
			expected := strings.ToLower(f.h.names[tc.kind][0]) + "\x00"

			stack := []uint64{uint64(tc.kind), 64, uint64(len(expected))}
			f.m.getHeaderNames(f.ctx, f.mod, stack)

			// count=1, len=len(name)+NUL
			require.Equal(t, uint64(1)<<32|uint64(len(expected)), stack[0])
			require.Equal(t, expected, f.read(64, uint32(len(expected))))
		})
	}
}

func TestGetHeaderValues(t *testing.T) {
	for _, tc := range allKinds {
		t.Run(tc.prefix, func(t *testing.T) {
			f := newFixture(t, 0, false)
			name, nameLen := f.write(200, "x")
			expected := f.h.values[tc.kind][0] + "\x00"

			stack := []uint64{uint64(tc.kind), uint64(name), uint64(nameLen), 64, uint64(len(expected))}
			f.m.getHeaderValues(f.ctx, f.mod, stack)

			require.Equal(t, uint64(1)<<32|uint64(len(expected)), stack[0])
			require.Equal(t, expected, f.read(64, uint32(len(expected))))
		})
	}
}

// TestWriteNULTerminated_limits covers the two arms callers rely on: an empty
// sequence is zero, and one over the limit reports its size without writing.
func TestWriteNULTerminated_limits(t *testing.T) {
	f := newFixture(t, 0, false)

	require.Zero(t, writeNULTerminated(f.mod.Memory(), 64, 64, nil))

	// "a\0b\0" is 4 bytes; a limit of 3 writes nothing.
	require.True(t, f.mod.Memory().Write(64, make([]byte, 4)))
	countLen := writeNULTerminated(f.mod.Memory(), 64, 3, []string{"a", "b"})
	require.Equal(t, uint64(2)<<32|uint64(4), countLen)
	require.Equal(t, "\x00\x00\x00\x00", f.read(64, 4))

	// Out of range memory is a trap, not a silent truncation.
	err := require.CapturePanic(func() { writeNULTerminated(f.mod.Memory(), 1<<30, 4, []string{"a", "b"}) })
	require.EqualError(t, err, "out of memory")
}

func TestSetAddRemoveHeader(t *testing.T) {
	for _, tc := range allKinds {
		t.Run(tc.prefix, func(t *testing.T) {
			// Response kinds are only mutable after next with buffering.
			f := newFixture(t, FeatureBufferResponse, false)
			name, nameLen := f.write(64, "k")
			value, valueLen := f.write(128, "v")
			params := []uint64{uint64(tc.kind), uint64(name), uint64(nameLen), uint64(value), uint64(valueLen)}

			f.m.setHeaderValue(f.ctx, f.mod, params)
			require.Equal(t, []string{tc.prefix + " k=v"}, f.h.set)

			f.m.addHeaderValue(f.ctx, f.mod, params)
			require.Equal(t, []string{tc.prefix + " k=v"}, f.h.add)

			f.m.removeHeader(f.ctx, f.mod, params[:3])
			require.Equal(t, []string{tc.prefix + " k"}, f.h.removed)
		})
	}
}

func TestHeaderFuncs_emptyName(t *testing.T) {
	f := newFixture(t, 0, false)
	params := []uint64{uint64(HeaderKindRequest), 0, 0, 0, 0}

	for name, fn := range map[string]func(context.Context, api.Module, []uint64){
		"get":    f.m.getHeaderValues,
		"set":    f.m.setHeaderValue,
		"add":    f.m.addHeaderValue,
		"remove": f.m.removeHeader,
	} {
		t.Run(name, func(t *testing.T) {
			err := require.CapturePanic(func() { fn(f.ctx, f.mod, params) })
			require.EqualError(t, err, "HTTP header name cannot be empty")
		})
	}
}

func TestHeaderFuncs_unsupportedKind(t *testing.T) {
	f := newFixture(t, 0, false)
	name, nameLen := f.write(64, "k")
	params := []uint64{9, uint64(name), uint64(nameLen), uint64(name), uint64(nameLen)}

	for name, fn := range map[string]func(context.Context, api.Module, []uint64){
		"names":  f.m.getHeaderNames,
		"values": f.m.getHeaderValues,
		"set":    f.m.setHeaderValue,
		"add":    f.m.addHeaderValue,
		"remove": f.m.removeHeader,
	} {
		t.Run(name, func(t *testing.T) {
			err := require.CapturePanic(func() { fn(f.ctx, f.mod, params) })
			require.EqualError(t, err, "unsupported header kind: 9")
		})
	}
}

// TestHeaderMutable_afterNext covers the rule that a response header can only
// be changed after next when the response was buffered.
func TestHeaderMutable_afterNext(t *testing.T) {
	tests := []struct {
		kind          HeaderKind
		features      Features
		expectedError string
	}{
		{HeaderKindRequest, 0, "can't set request header after next handler"},
		{HeaderKindRequestTrailers, 0, "can't set request trailer after next handler"},
		{HeaderKindResponse, 0, "can't set response header after next handler unless buffer_response is enabled"},
		{HeaderKindResponseTrailers, 0, "can't set response trailer after next handler unless buffer_response is enabled"},
	}

	for _, tc := range tests {
		t.Run(tc.expectedError, func(t *testing.T) {
			f := newFixture(t, tc.features, true)
			name, nameLen := f.write(64, "k")
			params := []uint64{uint64(tc.kind), uint64(name), uint64(nameLen), uint64(name), uint64(nameLen)}
			err := require.CapturePanic(func() { f.m.setHeaderValue(f.ctx, f.mod, params) })
			require.EqualError(t, err, tc.expectedError)
		})
	}

	// With buffering, the response is mutable after next.
	f := newFixture(t, FeatureBufferResponse, true)
	name, nameLen := f.write(64, "k")
	value, valueLen := f.write(128, "v")
	f.m.setHeaderValue(f.ctx, f.mod, []uint64{
		uint64(HeaderKindResponse), uint64(name), uint64(nameLen), uint64(value), uint64(valueLen),
	})
	require.Equal(t, []string{"response k=v"}, f.h.set)
}

func TestReadBody(t *testing.T) {
	tests := []struct {
		name     string
		kind     BodyKind
		expected string
	}{
		{"request", BodyKindRequest, "request body"},
		{"response", BodyKindResponse, "response body"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, FeatureBufferRequest|FeatureBufferResponse, false)

			// A limit larger than the body reads it all and reports EOF.
			stack := []uint64{uint64(tc.kind), 64, 128}
			f.m.readBody(f.ctx, f.mod, stack)
			require.Equal(t, uint64(1)<<32|uint64(len(tc.expected)), stack[0])
			require.Equal(t, tc.expected, f.read(64, uint32(len(tc.expected))))

			// The reader is stateful: the next read is at EOF with no bytes.
			stack = []uint64{uint64(tc.kind), 64, 128}
			f.m.readBody(f.ctx, f.mod, stack)
			require.Equal(t, uint64(1)<<32, stack[0])
		})
	}
}

// TestReadBody_partial covers a read that fills the buffer without EOF.
func TestReadBody_partial(t *testing.T) {
	f := newFixture(t, FeatureBufferRequest, false)

	stack := []uint64{uint64(BodyKindRequest), 64, 7}
	f.m.readBody(f.ctx, f.mod, stack)
	require.Equal(t, uint64(7), stack[0]) // no EOF bit
	require.Equal(t, "request", f.read(64, 7))
}

func TestReadBody_errors(t *testing.T) {
	f := newFixture(t, FeatureBufferRequest, false)

	t.Run("zero buf_limit", func(t *testing.T) {
		err := require.CapturePanic(func() {
			f.m.readBody(f.ctx, f.mod, []uint64{uint64(BodyKindRequest), 64, 0})
		})
		require.EqualError(t, err, "buf_limit==0 reading body")
	})

	t.Run("unsupported kind", func(t *testing.T) {
		err := require.CapturePanic(func() {
			f.m.readBody(f.ctx, f.mod, []uint64{9, 64, 8})
		})
		require.EqualError(t, err, "unsupported body kind: 9")
	})

	t.Run("reader error", func(t *testing.T) {
		f := newFixture(t, FeatureBufferRequest, false)
		f.h.readerErr = errors.New("ice")
		err := require.CapturePanic(func() {
			f.m.readBody(f.ctx, f.mod, []uint64{uint64(BodyKindRequest), 64, 8})
		})
		require.EqualError(t, err, "error reading body: ice")
	})

	t.Run("after next without buffering", func(t *testing.T) {
		f := newFixture(t, 0, true)
		err := require.CapturePanic(func() {
			f.m.readBody(f.ctx, f.mod, []uint64{uint64(BodyKindResponse), 64, 8})
		})
		require.EqualError(t, err, "can't read response body after next handler unless buffer_response is enabled")
	})
}

func TestWriteBody(t *testing.T) {
	f := newFixture(t, FeatureBufferResponse, false)
	f.h.requestBody.Reset()
	f.h.responseBody.Reset()

	ptr, l := f.write(64, "written")

	f.m.writeBody(f.ctx, f.mod, []uint64{uint64(BodyKindRequest), uint64(ptr), uint64(l)})
	require.Equal(t, "written", f.h.requestBody.String())

	f.m.writeBody(f.ctx, f.mod, []uint64{uint64(BodyKindResponse), uint64(ptr), uint64(l)})
	require.Equal(t, "written", f.h.responseBody.String())

	// A zero length overwrites with nothing, rather than erring.
	f.m.writeBody(f.ctx, f.mod, []uint64{uint64(BodyKindRequest), uint64(ptr), 0})

	err := require.CapturePanic(func() {
		f.m.writeBody(f.ctx, f.mod, []uint64{9, uint64(ptr), uint64(l)})
	})
	require.EqualError(t, err, "unsupported body kind: 9")

	// Writing the request body after next is too late.
	after := newFixture(t, 0, true)
	ptr, l = after.write(64, "written")
	err = require.CapturePanic(func() {
		after.m.writeBody(after.ctx, after.mod, []uint64{uint64(BodyKindRequest), uint64(ptr), uint64(l)})
	})
	require.EqualError(t, err, "can't write request body after next handler")
}

func TestWriteBody_writerError(t *testing.T) {
	f := newFixture(t, 0, false)
	f.s.requestBodyWriter = errWriter{}
	ptr, l := f.write(64, "written")

	err := require.CapturePanic(func() {
		f.m.writeBody(f.ctx, f.mod, []uint64{uint64(BodyKindRequest), uint64(ptr), uint64(l)})
	})
	require.EqualError(t, err, "error writing body: ice")
}

type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, errors.New("ice") }

func TestStatusCode(t *testing.T) {
	f := newFixture(t, 0, false)

	stack := make([]uint64, 1)
	f.m.getStatusCode(f.ctx, stack)
	require.Equal(t, uint64(200), stack[0])

	f.m.setStatusCode(f.ctx, []uint64{404})
	require.Equal(t, uint32(404), f.h.statusCode)

	// After next, overwriting the status needs a buffered response.
	after := newFixture(t, 0, true)
	err := require.CapturePanic(func() { after.m.setStatusCode(after.ctx, []uint64{500}) })
	require.EqualError(t, err, "can't set status code after next handler unless buffer_response is enabled")
}

func TestLog(t *testing.T) {
	var logged []string
	f := newFixture(t, 0, false)
	f.m.logger = recordingLogger{&logged}

	ptr, l := f.write(64, "hello")
	f.m.log(f.ctx, f.mod, []uint64{uint64(LogLevelInfo), uint64(ptr), uint64(l)})
	require.Equal(t, []string{"hello"}, logged)

	// An empty message still logs.
	f.m.log(f.ctx, f.mod, []uint64{uint64(LogLevelInfo), 0, 0})
	require.Equal(t, []string{"hello", ""}, logged)

	// A level the logger drops never reads memory.
	f.m.logger = NoopLogger{}
	f.m.log(f.ctx, f.mod, []uint64{uint64(LogLevelInfo), 1 << 30, 8})
}

type recordingLogger struct{ logged *[]string }

func (recordingLogger) IsEnabled(LogLevel) bool { return true }
func (l recordingLogger) Log(_ context.Context, _ LogLevel, msg string) {
	*l.logged = append(*l.logged, msg)
}

// TestEnableFeatures covers both scopes: during a request the features are
// per-request, otherwise they are the middleware's.
func TestEnableFeatures(t *testing.T) {
	t.Run("request scope", func(t *testing.T) {
		f := newFixture(t, 0, false)
		stack := []uint64{uint64(FeatureBufferResponse)}
		f.m.enableFeatures(f.ctx, stack)
		require.Equal(t, uint64(FeatureBufferResponse), stack[0])
		require.Equal(t, FeatureBufferResponse, f.s.features)
		require.Zero(t, f.m.features) // not the middleware's
	})

	t.Run("init scope", func(t *testing.T) {
		f := newFixture(t, 0, false)
		stack := []uint64{uint64(FeatureTrailers)}
		f.m.enableFeatures(testCtx, stack) // no request state
		require.Equal(t, uint64(FeatureTrailers), stack[0])
		require.Equal(t, FeatureTrailers, f.m.features)
	})
}

func TestMustRead_outOfRange(t *testing.T) {
	f := newFixture(t, 0, false)
	err := require.CapturePanic(func() { mustRead(f.mod.Memory(), "body", 1<<30, 8) })
	require.EqualError(t, err, "out of memory reading body")

	// A zero byteCount is the empty body, not a failure.
	require.Equal(t, []byte{}, mustRead(f.mod.Memory(), "body", 1<<30, 0))
	require.Equal(t, "", mustReadString(f.mod.Memory(), "body", 1<<30, 0))
}

// TestUnimplementedHost_all calls every default, since a Host embedding it
// inherits whatever these return.
func TestUnimplementedHost_all(t *testing.T) {
	h := UnimplementedHost{}
	ctx := testCtx

	h.SetMethod(ctx, "POST")
	h.SetURI(ctx, "/")
	h.SetStatusCode(ctx, 500)
	require.Equal(t, "", h.GetURI(ctx))
	require.Nil(t, h.GetRequestHeaderNames(ctx))
	require.Nil(t, h.GetRequestHeaderValues(ctx, "k"))
	require.Nil(t, h.GetRequestTrailerNames(ctx))
	require.Nil(t, h.GetRequestTrailerValues(ctx, "k"))
	require.Nil(t, h.GetResponseHeaderNames(ctx))
	require.Nil(t, h.GetResponseHeaderValues(ctx, "k"))
	require.Nil(t, h.GetResponseTrailerNames(ctx))
	require.Nil(t, h.GetResponseTrailerValues(ctx, "k"))

	h.SetRequestHeaderValue(ctx, "k", "v")
	h.AddRequestHeaderValue(ctx, "k", "v")
	h.RemoveRequestHeader(ctx, "k")
	h.SetRequestTrailerValue(ctx, "k", "v")
	h.AddRequestTrailerValue(ctx, "k", "v")
	h.RemoveRequestTrailer(ctx, "k")
	h.SetResponseHeaderValue(ctx, "k", "v")
	h.AddResponseHeaderValue(ctx, "k", "v")
	h.RemoveResponseHeader(ctx, "k")
	h.SetResponseTrailerValue(ctx, "k", "v")
	h.AddResponseTrailerValue(ctx, "k", "v")
	h.RemoveResponseTrailer(ctx, "k")

	n, err := h.RequestBodyWriter(ctx).Write([]byte("discarded"))
	require.NoError(t, err)
	require.Equal(t, 9, n)
	n, err = h.ResponseBodyWriter(ctx).Write([]byte("discarded"))
	require.NoError(t, err)
	require.Equal(t, 9, n)

	require.NoError(t, h.ResponseBodyReader(ctx).Close())
}
