package wasmedge

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/samyfodil/wazy"
	experimentalsock "github.com/samyfodil/wazy/experimental/sock"
	"github.com/samyfodil/wazy/imports/wasi_snapshot_preview1"
	"github.com/samyfodil/wazy/internal/testing/require"
	"github.com/samyfodil/wazy/internal/wasip1"
)

// TestSockAccept_preopenFallback covers the reason this package re-exports
// sock_accept at all.
//
// The standard function only accepts on a pre-opened listener, and cannot
// serve a guest that made its own with sock_listen. Re-exporting it would
// break the pre-opened case if the override did not also handle it, so the
// override falls back for descriptors that are not ours -- and this is the
// test that says so.
func TestSockAccept_preopenFallback(t *testing.T) {
	ctx := context.Background()

	// Reserve a port, then hand it to the config: the runtime builds the
	// listener itself, so this is the only way the test knows where to dial.
	reserve, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := reserve.Addr().(*net.TCPAddr).Port
	require.NoError(t, reserve.Close())

	// A pre-opened listener arrives as descriptor 3, before any socket the
	// guest opens for itself.
	sockCfg := experimentalsock.NewConfig().WithTCPListener("127.0.0.1", port)
	ctx = experimentalsock.WithConfig(ctx, sockCfg)

	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	b := r.NewHostModuleBuilder(ModuleName)
	wasi_snapshot_preview1.NewFunctionExporter().ExportFunctions(b)
	NewFunctionExporter(V2).ExportFunctions(b)
	_, err = b.Instantiate(ctx)
	require.NoError(t, err)

	mod, err := r.InstantiateModule(ctx, mustCompileGuest(t, r, guestModule(v2Funcs)), wazy.NewModuleConfig())
	require.NoError(t, err)

	g := &guest{t: t, ctx: ctx, mod: mod}

	go func() {
		if c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", itoa(port)), 5*time.Second); err == nil {
			_, _ = c.Write([]byte("x"))
			time.Sleep(100 * time.Millisecond)
			c.Close()
		}
	}()

	// fd 3 is the pre-opened listener, which only the fallback can accept on.
	require.Equal(t, wasip1.ErrnoSuccess, g.call(funcSockAccept, 3, 0, 96))
	connFD := g.u32(96)
	require.True(t, connFD > 3, "expected a fresh descriptor, got %d", connFD)
}

// TestSockAccept_nonblock covers the fdflags parameter, which applies to the
// *accepted* connection rather than to the accept call: the returned socket
// must be non-blocking, so reading it with nothing buffered reports EAGAIN
// instead of stalling the guest. (Whether accept itself blocks follows the
// listener's own flag; TestSocket_acceptNonblock covers that.)
func TestSockAccept_nonblock(t *testing.T) {
	g := newGuest(t, V2)

	fd := g.openSocket(addressFamilyInet4, socketTypeStream)
	addr := g.writeV2Addr(addressFamilyInet4, []byte{127, 0, 0, 1})
	require.Equal(t, wasip1.ErrnoSuccess, g.call(funcSockBind, uint64(fd), uint64(addr), 0))
	require.Equal(t, wasip1.ErrnoSuccess, g.call(funcSockListen, uint64(fd), 1))

	// Find the port, then connect without sending anything.
	out := g.writeAddrDescriptor(80, 1200, 128)
	require.Equal(t, wasip1.ErrnoSuccess, g.call(funcSockGetLocalAdd, uint64(fd), uint64(out), 96))
	port := g.u32(96)

	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", itoa(int(port))), 5*time.Second)
	require.NoError(t, err)
	defer conn.Close()

	require.Equal(t, wasip1.ErrnoSuccess,
		g.call(funcSockAccept, uint64(fd), uint64(wasip1.FD_NONBLOCK), 100))
	connFD := g.u32(100)

	// Nothing was sent, so a non-blocking read reports EAGAIN rather than
	// waiting for data.
	require.True(t, g.mod.Memory().WriteUint32Le(200, 300)) // iovec buffer
	require.True(t, g.mod.Memory().WriteUint32Le(204, 16))  // iovec length
	require.Equal(t, wasip1.ErrnoAgain,
		g.call("sock_recv", uint64(connFD), 200, 1, 0, 104, 108))
}

// TestSockAccept_notListening covers accepting on a socket that never
// listened.
func TestSockAccept_notListening(t *testing.T) {
	g := newGuest(t, V2)
	fd := g.openSocket(addressFamilyInet4, socketTypeStream)
	require.Equal(t, wasip1.ErrnoInval, g.call(funcSockAccept, uint64(fd), 0, 96))
}

// TestWriteAddress_unix covers the V2 layout carrying an AF_UNIX path, the
// case the wider address buffer exists for.
func TestWriteAddress_unix(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s")

	ln, err := net.Listen("unix", path)
	require.NoError(t, err)
	defer ln.Close()
	go func() {
		if c, err := ln.Accept(); err == nil {
			time.Sleep(100 * time.Millisecond)
			c.Close()
		}
	}()

	g := newGuest(t, V2)

	// A V2 address holding the socket path.
	buf := make([]byte, 128)
	buf[0] = byte(addressFamilyUnix)
	copy(buf[2:], path)
	require.True(t, g.mod.Memory().Write(1024, buf))
	addr := g.writeAddrDescriptor(64, 1024, 128)

	fd := g.openSocket(addressFamilyUnix, socketTypeStream)
	require.Equal(t, wasip1.ErrnoSuccess, g.call(funcSockConnect, uint64(fd), uint64(addr), 0))

	// Reading the peer back writes the path into the same layout.
	out := g.writeAddrDescriptor(80, 1200, 128)
	require.Equal(t, wasip1.ErrnoSuccess, g.call(funcSockGetPeerAddr, uint64(fd), uint64(out), 96))

	got, ok := g.mod.Memory().Read(1200, uint32(2+len(path)))
	require.True(t, ok)
	require.Equal(t, uint16(addressFamilyUnix), uint16(got[0])|uint16(got[1])<<8)
	require.Equal(t, path, string(got[2:]))
}

// TestResolve covers the name resolution behind sock_getaddrinfo, including
// the service forms and the family filter.
func TestResolve(t *testing.T) {
	t.Run("numeric service", func(t *testing.T) {
		ips, port, errno := resolve("127.0.0.1", "8080", addressFamilyUnspec)
		require.Equal(t, wasip1.ErrnoSuccess, errno)
		require.Equal(t, 8080, port)
		require.Equal(t, 1, len(ips))
	})

	t.Run("named service", func(t *testing.T) {
		_, port, errno := resolve("127.0.0.1", "http", addressFamilyUnspec)
		require.Equal(t, wasip1.ErrnoSuccess, errno)
		require.Equal(t, 80, port)
	})

	t.Run("unknown service", func(t *testing.T) {
		_, _, errno := resolve("127.0.0.1", "definitely-not-a-service", addressFamilyUnspec)
		require.Equal(t, wasip1.ErrnoInval, errno)
	})

	t.Run("no host is the wildcard", func(t *testing.T) {
		ips, port, errno := resolve("", "80", addressFamilyUnspec)
		require.Equal(t, wasip1.ErrnoSuccess, errno)
		require.Equal(t, 80, port)
		require.True(t, ips[0].Equal(net.IPv4zero))

		ips, _, errno = resolve("", "80", addressFamilyInet6)
		require.Equal(t, wasip1.ErrnoSuccess, errno)
		require.True(t, ips[0].Equal(net.IPv6unspecified))
	})

	t.Run("literal ipv6", func(t *testing.T) {
		ips, _, errno := resolve("::1", "", addressFamilyUnspec)
		require.Equal(t, wasip1.ErrnoSuccess, errno)
		require.True(t, ips[0].Equal(net.IPv6loopback))
	})

	t.Run("family filter drops the other family", func(t *testing.T) {
		// localhost resolves to both families on most hosts; asking for one
		// must never return the other.
		ips, _, errno := resolve("localhost", "", addressFamilyInet4)
		if errno == wasip1.ErrnoSuccess {
			for _, ip := range ips {
				require.NotNil(t, ip.To4(), "expected only IPv4, got %v", ip)
			}
		}
		ips, _, errno = resolve("localhost", "", addressFamilyInet6)
		if errno == wasip1.ErrnoSuccess {
			for _, ip := range ips {
				require.Nil(t, ip.To4(), "expected only IPv6, got %v", ip)
			}
		}
	})

	t.Run("unresolvable", func(t *testing.T) {
		_, _, errno := resolve("no.such.host.invalid", "", addressFamilyUnspec)
		require.NotEqual(t, wasip1.ErrnoSuccess, errno)
	})
}

func mustCompileGuest(t *testing.T, r wazy.Runtime, bin []byte) wazy.CompiledModule {
	t.Helper()
	c, err := r.CompileModule(context.Background(), bin)
	require.NoError(t, err)
	return c
}
