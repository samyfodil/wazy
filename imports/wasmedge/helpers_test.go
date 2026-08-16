package wasmedge

import (
	"context"
	"errors"
	"net"
	"os"
	"syscall"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/internal/testing/require"
	"github.com/samyfodil/wazy/internal/wasip1"
	"github.com/samyfodil/wazy/sys"
)

// The errno mapping is what a guest branches on -- Rust's std turns
// ECONNREFUSED into a distinct error kind -- so each arm is checked directly
// rather than through whichever conditions a test happens to provoke.
func TestToWasiErrno(t *testing.T) {
	// Wrapped the way net wraps them, since that is how they arrive.
	wrap := func(e error) error {
		return &net.OpError{Op: "dial", Net: "tcp", Err: &os.SyscallError{Syscall: "connect", Err: e}}
	}

	tests := []struct {
		name     string
		err      error
		expected wasip1.Errno
	}{
		{name: "nil is success", err: nil, expected: wasip1.ErrnoSuccess},
		{name: "closed is bad descriptor", err: net.ErrClosed, expected: wasip1.ErrnoBadf},
		{name: "wrapped closed", err: wrap(net.ErrClosed), expected: wasip1.ErrnoBadf},
		{name: "connection refused", err: wrap(syscall.ECONNREFUSED), expected: wasip1.ErrnoConnrefused},
		{name: "connection reset", err: wrap(syscall.ECONNRESET), expected: wasip1.ErrnoConnreset},
		{name: "connection aborted", err: wrap(syscall.ECONNABORTED), expected: wasip1.ErrnoConnaborted},
		{name: "address in use", err: wrap(syscall.EADDRINUSE), expected: wasip1.ErrnoAddrinuse},
		{name: "address not available", err: wrap(syscall.EADDRNOTAVAIL), expected: wasip1.ErrnoAddrnotavail},
		{name: "host unreachable", err: wrap(syscall.EHOSTUNREACH), expected: wasip1.ErrnoHostunreach},
		{name: "network unreachable", err: wrap(syscall.ENETUNREACH), expected: wasip1.ErrnoNetunreach},
		{name: "broken pipe", err: wrap(syscall.EPIPE), expected: wasip1.ErrnoPipe},
		{name: "permission denied", err: wrap(syscall.EACCES), expected: wasip1.ErrnoAcces},
		{name: "would block", err: wrap(syscall.EAGAIN), expected: wasip1.ErrnoAgain},
		{name: "invalid", err: wrap(syscall.EINVAL), expected: wasip1.ErrnoInval},
		{
			// A syscall errno with no mapping of its own is not silently
			// turned into something specific.
			name: "unmapped errno", err: wrap(syscall.ENOTTY), expected: wasip1.ErrnoIo,
		},
		{
			// The non-blocking emulation sets a deadline in the past, so a
			// timeout is EAGAIN rather than ETIMEDOUT.
			name: "timeout is EAGAIN", err: os.ErrDeadlineExceeded, expected: wasip1.ErrnoAgain,
		},
		{
			name: "dns failure", err: &net.DNSError{Err: "no such host", IsNotFound: true},
			expected: wasip1.ErrnoNoent,
		},
		{name: "anything else", err: errors.New("boom"), expected: wasip1.ErrnoIo},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.expected, toWasiErrno(tc.err))
		})
	}
}

// toFileErrno narrows to the smaller sys.Errno set the file surface uses, so
// what it cannot express has to land on EIO rather than on a wrong errno.
func TestToFileErrno(t *testing.T) {
	tests := []struct {
		errno    wasip1.Errno
		expected sys.Errno
	}{
		{wasip1.ErrnoSuccess, 0},
		{wasip1.ErrnoAgain, sys.EAGAIN},
		{wasip1.ErrnoBadf, sys.EBADF},
		{wasip1.ErrnoInval, sys.EINVAL},
		{wasip1.ErrnoAcces, sys.EACCES},
		{wasip1.ErrnoNotsup, sys.ENOTSUP},
		{wasip1.ErrnoConnrefused, sys.EIO},
		{wasip1.ErrnoAddrinuse, sys.EIO},
	}

	for _, tc := range tests {
		t.Run(wasip1.ErrnoName(uint32(tc.errno)), func(t *testing.T) {
			require.EqualErrno(t, tc.expected, toFileErrno(tc.errno))
		})
	}
}

// splitAddr feeds the wire format, so every address kind net can hand back has
// to render, including the ones with no port.
func TestSplitAddr(t *testing.T) {
	tests := []struct {
		name         string
		addr         net.Addr
		expectedHost string
		expectedPort int
	}{
		{
			name: "tcp", addr: &net.TCPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 8080},
			expectedHost: "10.0.0.1", expectedPort: 8080,
		},
		{
			name: "udp", addr: &net.UDPAddr{IP: net.IPv6loopback, Port: 53},
			expectedHost: "::1", expectedPort: 53,
		},
		{
			// A unix socket's path goes where the host does, with no port.
			name: "unix", addr: &net.UnixAddr{Name: "/tmp/s", Net: "unix"},
			expectedHost: "/tmp/s", expectedPort: 0,
		},
		{name: "nil", addr: nil, expectedHost: "", expectedPort: 0},
		{
			// An address type from outside net still parses if it looks like
			// host:port.
			name: "other with port", addr: stringAddr("192.0.2.1:99"),
			expectedHost: "192.0.2.1", expectedPort: 99,
		},
		{
			name: "other without port", addr: stringAddr("some-endpoint"),
			expectedHost: "some-endpoint", expectedPort: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			host, port := splitAddr(tc.addr)
			require.Equal(t, tc.expectedHost, host)
			require.Equal(t, tc.expectedPort, port)
		})
	}
}

type stringAddr string

func (a stringAddr) Network() string { return "custom" }
func (a stringAddr) String() string  { return string(a) }

// resolveAddrString picks the resolver from the socket's own family and type,
// which is the only thing that keeps a unix path from being parsed as host:port.
func TestSocket_resolveAddrString(t *testing.T) {
	tests := []struct {
		name             string
		family, sockType int32
		addr             string
		expectedType     string
		expectedErrno    wasip1.Errno
	}{
		{
			name: "tcp", family: addressFamilyInet4, sockType: socketTypeStream,
			addr: "127.0.0.1:80", expectedType: "*net.TCPAddr",
		},
		{
			name: "udp", family: addressFamilyInet4, sockType: socketTypeDatagram,
			addr: "127.0.0.1:80", expectedType: "*net.UDPAddr",
		},
		{
			name: "unix", family: addressFamilyUnix, sockType: socketTypeStream,
			addr: "/tmp/s", expectedType: "*net.UnixAddr",
		},
		{
			name: "unresolvable", family: addressFamilyInet4, sockType: socketTypeStream,
			addr: "not-an-address", expectedErrno: wasip1.ErrnoIo,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := &socket{family: tc.family, sockType: tc.sockType}
			addr, errno := s.resolveAddrString(tc.addr)
			if tc.expectedErrno != wasip1.ErrnoSuccess {
				require.Equal(t, tc.expectedErrno, errno)
				return
			}
			require.Equal(t, wasip1.ErrnoSuccess, errno)
			require.Equal(t, tc.expectedType, typeName(addr))
		})
	}
}

func typeName(v any) string {
	switch v.(type) {
	case *net.TCPAddr:
		return "*net.TCPAddr"
	case *net.UDPAddr:
		return "*net.UDPAddr"
	case *net.UnixAddr:
		return "*net.UnixAddr"
	}
	return "unknown"
}

// TestMustInstantiate covers the convenience wrapper, including the panic that
// is the whole reason it exists.
func TestMustInstantiate(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	MustInstantiate(ctx, r, V2)

	// The module name can only be instantiated once, so a second call is the
	// error path.
	err := requirePanic(t, func() { MustInstantiate(ctx, r, V2) })
	require.Contains(t, err, ModuleName)
}

func requirePanic(t *testing.T, f func()) (msg string) {
	t.Helper()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected a panic")
		}
		if err, ok := r.(error); ok {
			msg = err.Error()
			return
		}
		msg = "not an error"
	}()
	f()
	return
}

// TestDetect_shared covers a guest that imports only the functions the two
// versions share: it needs the extension, and V2 is the answer because the
// signatures cannot say otherwise.
func TestDetect_shared(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	shared := []hostFn{{funcSockOpen, 3}, {funcSockBind, 3}, {funcSockConnect, 3}}
	c, err := r.CompileModule(ctx, guestModule(shared))
	require.NoError(t, err)
	require.Equal(t, V2, Detect(c.ImportedFunctions()))
}

// TestDetect_otherModule covers imports from a module that is not ours, which
// must not be read as a request for the extension.
func TestDetect_otherModule(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	// A host module of our own with a colliding function name.
	b := r.NewHostModuleBuilder("some_other_module")
	wazy.HostFunc3(b.NewFunctionBuilder(), sockOpen).Export(funcSockOpen)
	_, err := b.Instantiate(ctx)
	require.NoError(t, err)

	c, err := r.CompileModule(ctx, guestModuleFrom("some_other_module", []hostFn{{funcSockOpen, 3}}))
	require.NoError(t, err)
	require.Equal(t, None, Detect(c.ImportedFunctions()))
}

// TestVersion_String covers the names an embedder logs.
func TestVersion_String(t *testing.T) {
	require.Equal(t, "none", None.String())
	require.Equal(t, "v1", V1.String())
	require.Equal(t, "v2", V2.String())
	// Anything else means no extension, which is what an embedder acts on.
	require.Equal(t, "none", Version(9).String())
}

// TestDetect_signatures covers picking the version apart by signature, which is
// the only thing that cannot drift from what the guest was compiled against.
func TestDetect_signatures(t *testing.T) {
	tests := []struct {
		name     string
		imports  []hostFn
		expected Version
	}{
		{
			// The clearest discriminator, in both directions.
			name:     "getlocaladdr with four params is V1",
			imports:  []hostFn{{funcSockGetLocalAdd, 4}},
			expected: V1,
		},
		{
			name:     "getlocaladdr with three params is V2",
			imports:  []hostFn{{funcSockGetLocalAdd, 3}},
			expected: V2,
		},
		{
			name:     "getpeeraddr with four params is V1",
			imports:  []hostFn{{funcSockGetPeerAddr, 4}},
			expected: V1,
		},
		{
			// The fallback, for a guest importing one getter but not the other.
			name:     "recv_from with seven params is V1",
			imports:  []hostFn{{funcSockRecvFrom, 7}},
			expected: V1,
		},
		{
			name:     "recv_from with eight params is V2",
			imports:  []hostFn{{funcSockRecvFrom, 8}},
			expected: V2,
		},
		{
			// sock_accept is shared with standard WASI, so only the two-param
			// form says anything.
			name:     "two-param accept is V1",
			imports:  []hostFn{{funcSockAccept, 2}},
			expected: V1,
		},
		{
			// The three-param form is the standard WASI signature, so on its
			// own it does not mean the extension is wanted.
			name:     "three-param accept alone means nothing",
			imports:  []hostFn{{funcSockAccept, 3}},
			expected: None,
		},
		{
			// A getter with an arity from neither version does not decide it;
			// the shared functions still do.
			name:     "unknown arity falls back to the shared functions",
			imports:  []hostFn{{funcSockGetLocalAdd, 9}, {funcSockOpen, 3}},
			expected: V2,
		},
		{
			name:     "only standard WASI",
			imports:  []hostFn{{"fd_write", 4}, {"sock_recv", 6}},
			expected: None,
		},
	}

	ctx := context.Background()
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := wazy.NewRuntime(ctx)
			defer r.Close(ctx)
			c, err := r.CompileModule(ctx, guestModule(tc.imports))
			require.NoError(t, err)
			require.Equal(t, tc.expected, Detect(c.ImportedFunctions()))
		})
	}
}
