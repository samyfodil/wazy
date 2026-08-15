package wasi_http

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/internal/testing/require"
)

// writeTuples lays out count [key_ptr][key_len][val_ptr][val_len] tuples at
// listPtr, copying the strings themselves just past the list.
func (g *guest) writeTuples(listPtr uint32, kvs ...[2]string) uint32 {
	g.t.Helper()
	strPtr := listPtr + uint32(len(kvs))*16
	entry := make([]byte, 16)
	for i, kv := range kvs {
		keyPtr, keyLen := g.writeString(strPtr, kv[0])
		strPtr += keyLen
		valPtr, valLen := g.writeString(strPtr, kv[1])
		strPtr += valLen

		le.PutUint32(entry, keyPtr)
		le.PutUint32(entry[4:], keyLen)
		le.PutUint32(entry[8:], valPtr)
		le.PutUint32(entry[12:], valLen)
		require.True(g.t, g.mod.Memory().Write(listPtr+uint32(i)*16, entry))
	}
	return uint32(len(kvs))
}

// readEntries reads a fields-entries result written at outPtr.
func (g *guest) readEntries(outPtr uint32) map[string]string {
	g.t.Helper()
	listPtr, count := g.u32(outPtr), g.u32(outPtr+4)
	entries := make(map[string]string, count)
	for i := uint32(0); i < count; i++ {
		tuple := listPtr + i*16
		key, ok := g.mod.Memory().Read(g.u32(tuple), g.u32(tuple+4))
		require.True(g.t, ok)
		value, ok := g.mod.Memory().Read(g.u32(tuple+8), g.u32(tuple+12))
		require.True(g.t, ok)
		entries[string(key)] = string(value)
	}
	return entries
}

func TestFields(t *testing.T) {
	g := newGuest(t)

	count := g.writeTuples(64, [2]string{"Content-Type", "text/plain"}, [2]string{"X-X", "1"})
	handle := g.call1("new-fields", 64, uint64(count))
	require.NotEqual(t, uint32(0), handle)

	g.call("fields-entries", uint64(handle), 32)
	require.Equal(t, map[string]string{"Content-Type": "text/plain", "X-X": "1"}, g.readEntries(32))

	// A dropped handle is unknown: fields-entries leaves the out struct alone.
	g.call("drop-fields", uint64(handle))
	require.True(t, g.mod.Memory().Write(32, make([]byte, 8)))
	g.call("fields-entries", uint64(handle), 32)
	require.Zero(t, g.u32(32))
	require.Zero(t, g.u32(36))
}

// TestFields_repeated pins the upstream behaviour that only the first value of
// a repeated header is reported back, so a port never silently "fixes" it into
// a different wire shape.
func TestFields_repeated(t *testing.T) {
	g := newGuest(t)

	count := g.writeTuples(64, [2]string{"Set-Cookie", "a=1"}, [2]string{"Set-Cookie", "b=2"})
	handle := g.call1("new-fields", 64, uint64(count))

	g.h.mu.Lock()
	require.Equal(t, []string{"a=1", "b=2"}, g.h.fields[handle]["Set-Cookie"])
	g.h.mu.Unlock()

	g.call("fields-entries", uint64(handle), 32)
	require.Equal(t, map[string]string{"Set-Cookie": "a=1"}, g.readEntries(32))
}

func TestNewFields_outOfRange(t *testing.T) {
	g := newGuest(t)

	// A tuple count that runs off the end of memory.
	require.Zero(t, g.call1("new-fields", 64, 1<<20))

	// A key pointer that does not.
	entry := make([]byte, 16)
	le.PutUint32(entry, 1<<30) // key_ptr
	le.PutUint32(entry[4:], 4) // key_len
	require.True(t, g.mod.Memory().Write(64, entry))
	require.Zero(t, g.call1("new-fields", 64, 1))

	// Then one whose value pointer does not.
	keyPtr, keyLen := g.writeString(256, "k")
	le.PutUint32(entry, keyPtr)
	le.PutUint32(entry[4:], keyLen)
	le.PutUint32(entry[8:], 1<<30) // val_ptr
	le.PutUint32(entry[12:], 4)    // val_len
	require.True(t, g.mod.Memory().Write(64, entry))
	require.Zero(t, g.call1("new-fields", 64, 1))
}

// newRequest calls new-outgoing-request with the given method enum, scheme
// option and strings, and returns the request handle.
func (g *guest) newRequest(method uint64, schemeIsSome, scheme uint64, path, query, schemeStr, authority string, headers uint32) uint32 {
	g.t.Helper()
	pathPtr, pathLen := g.writeString(512, path)
	queryPtr, queryLen := g.writeString(pathPtr+pathLen, query)
	schemePtr, schemeLen := g.writeString(queryPtr+queryLen, schemeStr)
	authPtr, authLen := g.writeString(schemePtr+schemeLen, authority)
	return g.call1("new-outgoing-request",
		method, 0, 0,
		uint64(pathPtr), uint64(pathLen),
		uint64(queryPtr), uint64(queryLen),
		schemeIsSome, scheme, uint64(schemePtr), uint64(schemeLen),
		uint64(authPtr), uint64(authLen), uint64(headers))
}

func TestNewOutgoingRequest(t *testing.T) {
	tests := []struct {
		name                      string
		method                    uint64
		schemeIsSome, scheme      uint64
		path, query, schemeStr    string
		authority, expectedMethod string
		expectedURL               string
	}{
		{
			name: "defaults to https", method: 0, schemeIsSome: 0,
			path: "/a", query: "?b=c", authority: "example.com",
			expectedMethod: "GET", expectedURL: "https://example.com/a?b=c",
		},
		{
			name: "scheme http", method: 2, schemeIsSome: 1, scheme: 0,
			path: "/post", authority: "example.com",
			expectedMethod: "POST", expectedURL: "http://example.com/post",
		},
		{
			name: "scheme https", method: 3, schemeIsSome: 1, scheme: 1,
			path: "/put", authority: "example.com",
			expectedMethod: "PUT", expectedURL: "https://example.com/put",
		},
		{
			name: "scheme other", method: 8, schemeIsSome: 1, scheme: 2, schemeStr: "gopher",
			path: "/patch", authority: "example.com",
			expectedMethod: "PATCH", expectedURL: "gopher://example.com/patch",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := newGuest(t)
			handle := g.newRequest(tc.method, tc.schemeIsSome, tc.scheme, tc.path, tc.query, tc.schemeStr, tc.authority, 7)
			require.NotEqual(t, uint32(0), handle)

			g.h.mu.Lock()
			req := g.h.requests[handle]
			g.h.mu.Unlock()
			require.Equal(t, tc.expectedMethod, req.method)
			require.Equal(t, tc.expectedURL, req.url())
			require.Equal(t, uint32(7), req.headers)
		})
	}
}

// TestNewOutgoingRequest_methodEnums walks every method in the ABI's order: an
// off-by-one in the table would send the wrong verb, which no fixture catches.
func TestNewOutgoingRequest_methodEnums(t *testing.T) {
	g := newGuest(t)
	for enum, want := range methods {
		handle := g.newRequest(uint64(enum), 0, 0, "/", "", "", "example.com", 0)
		g.h.mu.Lock()
		require.Equal(t, want, g.h.requests[handle].method, "enum %d", enum)
		g.h.mu.Unlock()
	}
}

func TestNewOutgoingRequest_unknownMethod(t *testing.T) {
	g := newGuest(t)
	err := g.callErr("new-outgoing-request", uint64(len(methods)), 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0)
	require.Contains(t, err.Error(), "unknown method enum 9")
}

func TestNewOutgoingRequest_outOfRange(t *testing.T) {
	badPtr := uint64(1 << 30)
	for _, tc := range []struct {
		name   string
		params []uint64
	}{
		// method, method_ptr, method_len, path_ptr, path_len, query_ptr,
		// query_len, scheme_is_some, scheme, scheme_ptr, scheme_len,
		// authority_ptr, authority_len, headers
		{"path", []uint64{0, 0, 0, badPtr, 4, 0, 0, 0, 0, 0, 0, 0, 0, 0}},
		{"query", []uint64{0, 0, 0, 0, 0, badPtr, 4, 0, 0, 0, 0, 0, 0, 0}},
		{"scheme", []uint64{0, 0, 0, 0, 0, 0, 0, 1, 2, badPtr, 4, 0, 0, 0}},
		{"authority", []uint64{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, badPtr, 4, 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newGuest(t)
			require.Zero(t, g.call1("new-outgoing-request", tc.params...))
		})
	}
}

func TestOutgoingRequestWrite(t *testing.T) {
	g := newGuest(t)
	handle := g.newRequest(2, 0, 0, "/", "", "", "example.com", 0)

	g.call("outgoing-request-write", uint64(handle), 32)
	require.Zero(t, g.u32(32)) // ok
	streamHandle := g.u32(36)
	require.NotEqual(t, uint32(0), streamHandle)

	ptr, length := g.writeString(1024, "a body")
	g.call("write", uint64(streamHandle), uint64(ptr), uint64(length), 64)
	require.Zero(t, g.u32(64))          // ok
	require.Equal(t, length, g.u32(68)) // bytes written
	g.h.mu.Lock()
	require.Equal(t, "a body", g.h.requests[handle].body.String())
	g.h.mu.Unlock()

	// An unknown request reports the error arm.
	g.call("outgoing-request-write", 999, 32)
	require.Equal(t, uint32(1), g.u32(32))
	require.Zero(t, g.u32(36))
}

func TestDropRequests(t *testing.T) {
	g := newGuest(t)

	handle := g.newRequest(0, 0, 0, "/", "", "", "example.com", 0)
	g.call("drop-outgoing-request", uint64(handle))
	g.h.mu.Lock()
	_, found := g.h.requests[handle]
	g.h.mu.Unlock()
	require.False(t, found)

	// drop-incoming-request also drops the request's fields.
	count := g.writeTuples(64, [2]string{"A", "b"})
	fields := g.call1("new-fields", 64, uint64(count))
	handle = g.newRequest(0, 0, 0, "/", "", "", "example.com", fields)
	g.call("drop-incoming-request", uint64(handle))
	g.h.mu.Lock()
	_, foundReq := g.h.requests[handle]
	_, foundFields := g.h.fields[fields]
	g.h.mu.Unlock()
	require.False(t, foundReq)
	require.False(t, foundFields)

	// Dropping an unknown handle is a no-op, not a trap.
	g.call("drop-incoming-request", 999)
	g.call("drop-outgoing-request", 999)
	g.call("drop-fields", 999)
}

func TestIncomingRequest(t *testing.T) {
	g := newGuest(t)
	handle := g.newRequest(4, 0, 0, "/path", "", "", "example.com:8080", 11)

	g.call("incoming-request-method", uint64(handle), 32)
	require.Equal(t, uint32(4), g.u32(32)) // DELETE

	g.call("incoming-request-path", uint64(handle), 32)
	require.Equal(t, "/path", g.str(32))

	g.call("incoming-request-authority", uint64(handle), 32)
	require.Equal(t, "example.com:8080", g.str(32))

	require.Equal(t, uint32(11), g.call1("incoming-request-headers", uint64(handle)))

	// Serving is not implemented, so the body is the error arm.
	g.call("incoming-request-consume", uint64(handle), 32)
	require.Equal(t, uint32(1), g.u32(32))
	require.Zero(t, g.u32(36))
}

func TestIncomingRequest_unknownHandle(t *testing.T) {
	g := newGuest(t)

	require.True(t, g.mod.Memory().Write(32, make([]byte, 8)))
	for _, name := range []string{"incoming-request-method", "incoming-request-path", "incoming-request-authority"} {
		g.call(name, 999, 32)
		require.Zero(t, g.u32(32), name)
	}
	require.Zero(t, g.call1("incoming-request-headers", 999))
}

// TestIncomingRequestMethod_unknown covers the trap for a method with no enum.
// Only a host-side request can hold one, since new-outgoing-request validates.
func TestIncomingRequestMethod_unknown(t *testing.T) {
	g := newGuest(t)
	handle := g.newRequest(0, 0, 0, "/", "", "", "example.com", 0)

	g.h.mu.Lock()
	g.h.requests[handle].method = "BREW"
	g.h.mu.Unlock()

	err := g.callErr("incoming-request-method", uint64(handle), 32)
	require.Contains(t, err.Error(), `unknown method "BREW"`)
}

func TestOutgoingResponse(t *testing.T) {
	g := newGuest(t)

	handle := g.call1("new-outgoing-response", 201, 5)
	require.NotEqual(t, uint32(0), handle)
	g.h.mu.Lock()
	require.Equal(t, 201, g.h.responses[handle].status)
	require.Equal(t, uint32(5), g.h.responses[handle].headers)
	g.h.mu.Unlock()

	// The write stream is real, but discards: serving is not implemented.
	g.call("outgoing-response-write", uint64(handle), 32)
	require.Zero(t, g.u32(32))
	streamHandle := g.u32(36)
	ptr, length := g.writeString(1024, "discarded")
	g.call("write", uint64(streamHandle), uint64(ptr), uint64(length), 64)
	require.Zero(t, g.u32(64))
	require.Equal(t, length, g.u32(68))

	g.call("drop-outgoing-response", uint64(handle))
	g.call("outgoing-response-write", uint64(handle), 32)
	require.Equal(t, uint32(1), g.u32(32))
	require.Zero(t, g.u32(36))
}

func TestSetResponseOutparam(t *testing.T) {
	g := newGuest(t)

	require.Zero(t, g.call1("set-response-outparam", 3, 0, 42, 0, 0))
	g.h.mu.Lock()
	require.Equal(t, uint32(42), g.h.outparams[3])
	g.h.mu.Unlock()

	// The error arm reports 1 and records nothing.
	require.Equal(t, uint32(1), g.call1("set-response-outparam", 4, 1, 43, 0, 0))
	g.h.mu.Lock()
	_, found := g.h.outparams[4]
	g.h.mu.Unlock()
	require.False(t, found)
}

func TestRequest_serverStubReturnsZero(t *testing.T) {
	g := newGuest(t)
	require.Zero(t, g.call1("request", 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0))
}

func TestLogIt(t *testing.T) {
	g := newGuest(t)
	ptr, length := g.writeString(1024, "") // empty: keeps test output clean
	g.call("log-it", uint64(ptr), uint64(length))
	// An out-of-range string is dropped rather than trapping.
	g.call("log-it", 1<<30, 4)
}

func TestFutureIncomingResponseGet(t *testing.T) {
	g := newGuest(t)
	g.call("future-incoming-response-get", 7, 32)
	require.Equal(t, uint32(1), g.u32(32)) // is_some
	require.Zero(t, g.u32(36))             // ok
	require.Equal(t, uint32(7), g.u32(40)) // the response handle
}

// --- responses and streams, driven from the host side ----------------------

// closeTracker reports whether the response body was closed.
type closeTracker struct {
	io.Reader
	closed bool
}

func (c *closeTracker) Close() error { c.closed = true; return nil }

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("boom") }

// addResponse registers a response as a completed outbound call would.
func (g *guest) addResponse(res *response) uint32 {
	g.t.Helper()
	g.h.mu.Lock()
	defer g.h.mu.Unlock()
	id := g.h.newID()
	g.h.responses[id] = res
	return id
}

func TestIncomingResponse(t *testing.T) {
	g := newGuest(t)
	body := &closeTracker{Reader: strings.NewReader("hello")}
	handle := g.addResponse(&response{
		status: 418,
		header: http.Header{"Content-Type": []string{"text/plain"}},
		body:   body,
	})

	require.Equal(t, uint32(418), g.call1("incoming-response-status", uint64(handle)))

	// Headers are lifted into fields once, then the same handle comes back.
	fields := g.call1("incoming-response-headers", uint64(handle))
	require.NotEqual(t, uint32(0), fields)
	require.Equal(t, fields, g.call1("incoming-response-headers", uint64(handle)))
	g.call("fields-entries", uint64(fields), 32)
	require.Equal(t, map[string]string{"Content-Type": "text/plain"}, g.readEntries(32))

	// Consuming yields a stream over the body.
	g.call("incoming-response-consume", uint64(handle), 32)
	require.Zero(t, g.u32(32))
	streamHandle := g.u32(36)

	g.call("read", uint64(streamHandle), 16, 64)
	require.Zero(t, g.u32(64))                    // ok
	require.Equal(t, uint32(5), g.u32(72))        // length
	data, ok := g.mod.Memory().Read(g.u32(68), 5) // ptr
	require.True(t, ok)
	require.Equal(t, "hello", string(data))
	require.Equal(t, uint32(1), g.u32(76)) // more to read

	// At EOF the "more" flag clears.
	g.call("read", uint64(streamHandle), 16, 64)
	require.Zero(t, g.u32(64))
	require.Zero(t, g.u32(72))
	require.Zero(t, g.u32(76))

	// Dropping the response closes the body, which upstream leaks.
	g.call("drop-incoming-response", uint64(handle))
	require.True(t, body.closed)
	require.Zero(t, g.call1("incoming-response-status", uint64(handle)))
}

func TestIncomingResponse_unknownHandle(t *testing.T) {
	g := newGuest(t)

	require.Zero(t, g.call1("incoming-response-status", 999))
	require.Zero(t, g.call1("incoming-response-headers", 999))

	g.call("incoming-response-consume", 999, 32)
	require.Equal(t, uint32(1), g.u32(32))

	// A response with no body (an outgoing one) is the same error arm.
	handle := g.addResponse(&response{status: 200})
	g.call("incoming-response-consume", uint64(handle), 32)
	require.Equal(t, uint32(1), g.u32(32))

	// Dropping an unknown response is a no-op.
	g.call("drop-incoming-response", 999)
}

func TestStreamRead_errors(t *testing.T) {
	g := newGuest(t)

	// Unknown stream.
	g.call("read", 999, 8, 64)
	require.Equal(t, uint32(1), g.u32(64))

	// A write-only stream is not readable.
	handle := g.addResponse(&response{status: 200})
	g.call("outgoing-response-write", uint64(handle), 32)
	g.call("read", uint64(g.u32(36)), 8, 64)
	require.Equal(t, uint32(1), g.u32(64))

	// A reader that fails mid-stream.
	g.h.mu.Lock()
	id := g.h.newID()
	g.h.streams[id] = stream{reader: errReader{}}
	g.h.mu.Unlock()
	g.call("read", uint64(id), 8, 64)
	require.Equal(t, uint32(1), g.u32(64))

	// Dropping the stream makes it unknown again.
	body := &closeTracker{Reader: strings.NewReader("x")}
	resHandle := g.addResponse(&response{status: 200, body: body})
	g.call("incoming-response-consume", uint64(resHandle), 32)
	streamHandle := g.u32(36)
	g.call("drop-input-stream", uint64(streamHandle))
	g.call("read", uint64(streamHandle), 8, 64)
	require.Equal(t, uint32(1), g.u32(64))
}

func TestStreamWrite_errors(t *testing.T) {
	g := newGuest(t)

	// Unknown stream.
	ptr, length := g.writeString(1024, "data")
	g.call("write", 999, uint64(ptr), uint64(length), 64)
	require.Equal(t, uint32(1), g.u32(64))
	require.Zero(t, g.u32(68))

	// Source bytes out of range.
	g.call("write", 999, 1<<30, 4, 64)
	require.Equal(t, uint32(1), g.u32(64))

	// A read-only stream is not writable.
	body := &closeTracker{Reader: strings.NewReader("x")}
	resHandle := g.addResponse(&response{status: 200, body: body})
	g.call("incoming-response-consume", uint64(resHandle), 32)
	g.call("write", uint64(g.u32(36)), uint64(ptr), uint64(length), 64)
	require.Equal(t, uint32(1), g.u32(64))

	// A writer that fails.
	g.h.mu.Lock()
	id := g.h.newID()
	g.h.streams[id] = stream{writer: errWriter{}}
	g.h.mu.Unlock()
	g.call("write", uint64(id), uint64(ptr), uint64(length), 64)
	require.Equal(t, uint32(1), g.u32(64))
	require.Equal(t, uint32(2), g.u32(68)) // partial write is reported
}

type errWriter struct{}

func (errWriter) Write(p []byte) (int, error) { return 2, errors.New("boom") }

// --- handle ----------------------------------------------------------------

func TestHandle(t *testing.T) {
	var gotMethod, gotPath, gotHeader, gotBody string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotHeader = r.Method, r.URL.RequestURI(), r.Header.Get("X-Test")
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.WriteHeader(http.StatusCreated)
	}))
	defer ts.Close()

	g := newGuest(t)
	authority := strings.TrimPrefix(ts.URL, "http://")

	count := g.writeTuples(64, [2]string{"X-Test", "yes"})
	fields := g.call1("new-fields", 64, uint64(count))
	reqHandle := g.newRequest(2, 1, 0, "/path", "?q=1", "", authority, fields) // POST

	g.call("outgoing-request-write", uint64(reqHandle), 32)
	ptr, length := g.writeString(1024, "payload")
	g.call("write", uint64(g.u32(36)), uint64(ptr), uint64(length), 64)

	resHandle := g.call1("handle", uint64(reqHandle), 0, 0, 0, 0, 0, 0, 0)
	require.NotEqual(t, uint32(0), resHandle)
	require.Equal(t, uint32(201), g.call1("incoming-response-status", uint64(resHandle)))

	require.Equal(t, "POST", gotMethod)
	require.Equal(t, "/path?q=1", gotPath)
	require.Equal(t, "yes", gotHeader)
	require.Equal(t, "payload", gotBody)
}

func TestHandle_errors(t *testing.T) {
	g := newGuest(t)

	// An unknown request handle.
	require.Zero(t, g.call1("handle", 999, 0, 0, 0, 0, 0, 0, 0))

	// A URL that cannot be parsed.
	bad := g.newRequest(0, 1, 0, "/", "", "", "exa mple.com", 0)
	require.Zero(t, g.call1("handle", uint64(bad), 0, 0, 0, 0, 0, 0, 0))

	// A host that is not listening.
	unreachable := g.newRequest(0, 1, 0, "/", "", "", "127.0.0.1:1", 0)
	require.Zero(t, g.call1("handle", uint64(unreachable), 0, 0, 0, 0, 0, 0, 0))
}

// TestHandle_contextCancel proves the guest's context reaches the outbound
// request, which upstream's http.NewRequest does not do.
func TestHandle_contextCancel(t *testing.T) {
	started := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	defer ts.Close()

	g := newGuest(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	handle := g.newRequest(0, 1, 0, "/", "", "", strings.TrimPrefix(ts.URL, "http://"), 0)
	go func() {
		<-started
		cancel()
	}()

	fn := g.mod.ExportedFunction("call_handle")
	results, err := fn.Call(ctx, uint64(handle), 0, 0, 0, 0, 0, 0, 0)
	require.NoError(t, err)
	require.Zero(t, uint32(results[0]))
}

// --- allocator and out-of-range traps --------------------------------------

func TestMalloc_missingRealloc(t *testing.T) {
	g := newGuestWith(t, reallocNone)

	count := g.writeTuples(64, [2]string{"A", "b"})
	handle := g.call1("new-fields", 64, uint64(count))
	err := g.callErr("fields-entries", uint64(handle), 32)
	require.Contains(t, err.Error(), "does not export cabi_realloc")
}

func TestMalloc_reallocTraps(t *testing.T) {
	g := newGuestWith(t, reallocTrap)

	count := g.writeTuples(64, [2]string{"A", "b"})
	handle := g.call1("new-fields", 64, uint64(count))
	err := g.callErr("fields-entries", uint64(handle), 32)
	require.Contains(t, err.Error(), "cabi_realloc(16) failed")
}

func TestWrite_outOfRange(t *testing.T) {
	g := newGuest(t)
	// The out pointer is past the end of the guest's two pages.
	err := g.callErr("future-incoming-response-get", 7, 2*65536-4)
	require.Contains(t, err.Error(), "out of range")
}

// TestAllocString_outOfRange drives allocString's failure arm by pointing the
// bump allocator past the end of memory before a string is written.
func TestAllocString_outOfRange(t *testing.T) {
	g := newGuest(t)
	handle := g.newRequest(0, 0, 0, "/some-path", "", "", "example.com", 0)

	require.True(t, g.mod.Memory().WriteUint32Le(0, 0)) // touch memory, keep the guest honest
	g.h.mu.Lock()
	g.h.requests[handle].path = strings.Repeat("x", 128*1024) // larger than the 2 pages
	g.h.mu.Unlock()

	err := g.callErr("incoming-request-path", uint64(handle), 32)
	require.Contains(t, err.Error(), "out of range")
}

// --- instantiation ---------------------------------------------------------

func TestInstantiate(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	closer, err := Instantiate(ctx, r)
	require.NoError(t, err)

	// Every module the ABI names is present.
	for _, name := range []string{OutgoingModuleName, TypesModuleName, StreamsModuleName} {
		require.NotNil(t, r.Module(name), name)
	}

	require.NoError(t, closer.Close(ctx))
	for _, name := range []string{OutgoingModuleName, TypesModuleName, StreamsModuleName} {
		require.Nil(t, r.Module(name), name)
	}
}

// TestInstantiate_conflict covers the partial-failure path: modules already
// instantiated must be closed before the error returns.
func TestInstantiate_conflict(t *testing.T) {
	for _, taken := range []string{OutgoingModuleName, TypesModuleName, StreamsModuleName} {
		t.Run(taken, func(t *testing.T) {
			ctx := context.Background()
			r := wazy.NewRuntime(ctx)
			defer r.Close(ctx)

			// Claim one of the three names first.
			_, err := r.NewHostModuleBuilder(taken).Instantiate(ctx)
			require.NoError(t, err)

			_, err = Instantiate(ctx, r)
			require.Error(t, err)

			// Whatever was instantiated before the clash is gone again, so a
			// second attempt fails the same way rather than on a leftover.
			_, err = Instantiate(ctx, r)
			require.Error(t, err)
		})
	}
}

func TestMustInstantiate(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	MustInstantiate(ctx, r)

	err := require.CapturePanic(func() { MustInstantiate(ctx, r) })
	require.Error(t, err)
}

// TestClosers_firstError checks the aggregate closer reports the first error
// and still closes the rest.
func TestClosers_firstError(t *testing.T) {
	first, second := errors.New("first"), errors.New("second")
	var closed int
	c := closers{
		closerFunc(func() error { closed++; return nil }),
		closerFunc(func() error { closed++; return first }),
		closerFunc(func() error { closed++; return second }),
	}
	require.Equal(t, first, c.Close(context.Background()))
	require.Equal(t, 3, closed)
}

type closerFunc func() error

func (f closerFunc) Close(context.Context) error { return f() }

// TestBufferReuse guards writeU32s' shared buffer: a 4-value write followed by
// a 2-value write must not leave the third and fourth values behind.
func TestBufferReuse(t *testing.T) {
	g := newGuest(t)
	g.call("future-incoming-response-get", 0xAAAA, 32) // writes 3 u32s
	require.True(t, g.mod.Memory().Write(32, bytes.Repeat([]byte{0xFF}, 12)))
	g.call("incoming-response-consume", 999, 32) // writes 1 u32
	require.Equal(t, uint32(1), g.u32(32))
	require.Equal(t, uint32(0xFFFFFFFF), g.u32(36)) // untouched
}
