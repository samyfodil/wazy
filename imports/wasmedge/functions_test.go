package wasmedge

import (
	"context"
	"encoding/binary"
	"net"
	"strings"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/imports/wasi_snapshot_preview1"
	"github.com/samyfodil/wazy/internal/testing/binaryencoding"
	"github.com/samyfodil/wazy/internal/testing/require"
	"github.com/samyfodil/wazy/internal/wasip1"
	"github.com/samyfodil/wazy/internal/wasm"
)

// This drives the wire layer through a synthetic guest, for what the real
// guests cannot reach: bad descriptors, out-of-range pointers, malformed
// address buffers, and the encodings a loopback TCP test never produces.
//
// The guest imports each function and re-exports a passthrough, so a test can
// call it with any arguments it likes. Instantiating it is also a signature
// check: an export declared with the wrong arity fails to link here.

// hostFn is one function to import and re-export.
type hostFn struct {
	name   string
	params int
}

// v2Funcs and v1Funcs are the two tables, with their wasm arities. These are
// the arities a guest is compiled against; see testdata/sockguest.rs.
var (
	v2Funcs = []hostFn{
		{funcSockOpen, 3},
		{funcSockBind, 3},
		{funcSockConnect, 3},
		{funcSockListen, 2},
		{funcSockAccept, 3},
		{funcSockSendTo, 7},
		{funcSockRecvFrom, 8},
		{funcSockGetSockOpt, 5},
		{funcSockSetSockOpt, 5},
		{funcSockGetLocalAdd, 3},
		{funcSockGetPeerAddr, 3},
		{funcSockGetAddrInfo, 8},
		{"sock_recv", 6}, // standard WASI, used to observe the accepted socket
	}
	v1Funcs = []hostFn{
		{funcSockOpen, 3},
		{funcSockBind, 3},
		{funcSockConnect, 3},
		{funcSockListen, 2},
		{funcSockAccept, 2},
		{funcSockSendTo, 7},
		{funcSockRecvFrom, 7},
		{funcSockGetSockOpt, 5},
		{funcSockSetSockOpt, 5},
		{funcSockGetLocalAdd, 4},
		{funcSockGetPeerAddr, 4},
		{funcSockGetAddrInfo, 8},
		{"sock_recv", 6},
	}
)

// guestModule builds a module importing fns from the extension's module and
// re-exporting each as "call_<name>".
func guestModule(fns []hostFn) []byte {
	return guestModuleFrom(ModuleName, fns)
}

// guestModuleFrom is guestModule with the imported module name spelled out,
// for the tests that care where the functions come from.
func guestModuleFrom(moduleName string, fns []hostFn) []byte {
	m := &wasm.Module{MemorySection: []wasm.Memory{{Min: 2, Cap: 2, Max: 2, IsMaxEncoded: true}}}

	i32s := func(n int) []wasm.ValueType {
		types := make([]wasm.ValueType, n)
		for i := range types {
			types[i] = wasm.ValueTypeI32
		}
		return types
	}

	for i, fn := range fns {
		m.TypeSection = append(m.TypeSection, wasm.FunctionType{
			Params: i32s(fn.params), Results: []wasm.ValueType{wasm.ValueTypeI32},
		})
		m.ImportSection = append(m.ImportSection, wasm.Import{
			Module: moduleName, Name: fn.name, Type: wasm.ExternTypeFunc, DescFunc: wasm.Index(i),
		})
	}
	for i, fn := range fns {
		body := make([]byte, 0, fn.params*2+3)
		for p := 0; p < fn.params; p++ {
			body = append(body, wasm.OpcodeLocalGet, byte(p))
		}
		body = append(body, wasm.OpcodeCall, byte(i), wasm.OpcodeEnd)

		m.FunctionSection = append(m.FunctionSection, wasm.Index(i))
		m.CodeSection = append(m.CodeSection, wasm.Code{Body: body})
		m.ExportSection = append(m.ExportSection, wasm.Export{
			Name: "call_" + fn.name, Type: wasm.ExternTypeFunc,
			Index: wasm.Index(len(fns) + i),
		})
	}
	m.ExportSection = append(m.ExportSection, wasm.Export{Name: "memory", Type: wasm.ExternTypeMemory})

	return binaryencoding.EncodeModule(m)
}

type guest struct {
	t   *testing.T
	ctx context.Context
	mod api.Module
}

func newGuest(t *testing.T, v Version) *guest {
	t.Helper()
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	t.Cleanup(func() { r.Close(ctx) })

	b := r.NewHostModuleBuilder(ModuleName)
	wasi_snapshot_preview1.NewFunctionExporter().ExportFunctions(b)
	NewFunctionExporter(v).ExportFunctions(b)
	_, err := b.Instantiate(ctx)
	require.NoError(t, err)

	fns := v2Funcs
	if v == V1 {
		fns = v1Funcs
	}
	mod, err := r.Instantiate(ctx, guestModule(fns))
	require.NoError(t, err)
	return &guest{t: t, ctx: ctx, mod: mod}
}

// call invokes a function through the passthrough and returns its errno.
func (g *guest) call(name string, params ...uint64) wasip1.Errno {
	g.t.Helper()
	fn := g.mod.ExportedFunction("call_" + name)
	require.NotNil(g.t, fn, name)
	results, err := fn.Call(g.ctx, params...)
	require.NoError(g.t, err, name)
	return wasip1.Errno(results[0])
}

// writeAddrDescriptor lays out the {pointer, length} pair the address
// parameter points at, and returns its address.
func (g *guest) writeAddrDescriptor(at, bufPtr, bufLen uint32) uint32 {
	g.t.Helper()
	require.True(g.t, g.mod.Memory().WriteUint32Le(at, bufPtr))
	require.True(g.t, g.mod.Memory().WriteUint32Le(at+4, bufLen))
	return at
}

// writeV2Addr writes a V2 address buffer and returns its descriptor.
func (g *guest) writeV2Addr(family int32, addr []byte) uint32 {
	g.t.Helper()
	buf := make([]byte, 128)
	binary.LittleEndian.PutUint16(buf, uint16(family))
	copy(buf[2:], addr)
	require.True(g.t, g.mod.Memory().Write(1024, buf))
	return g.writeAddrDescriptor(64, 1024, 128)
}

// u32 reads a result the host wrote.
func (g *guest) u32(ptr uint32) uint32 {
	g.t.Helper()
	v, ok := g.mod.Memory().ReadUint32Le(ptr)
	require.True(g.t, ok)
	return v
}

// openSocket opens one and returns its descriptor.
func (g *guest) openSocket(family, sockType int32) uint32 {
	g.t.Helper()
	errno := g.call(funcSockOpen, uint64(family), uint64(sockType), 32)
	require.Equal(g.t, wasip1.ErrnoSuccess, errno)
	return g.u32(32)
}

// TestFunctions_badDescriptor covers every function's descriptor check: an fd
// that does not exist is EBADF, and one that exists but is not a socket --
// stdin here -- is ENOTSOCK rather than a confusing EBADF.
func TestFunctions_badDescriptor(t *testing.T) {
	g := newGuest(t, V2)
	addr := g.writeV2Addr(addressFamilyInet4, []byte{127, 0, 0, 1})

	for _, tc := range []struct {
		name   string
		params []uint64
	}{
		{funcSockBind, []uint64{0, uint64(addr), 80}},
		{funcSockConnect, []uint64{0, uint64(addr), 80}},
		{funcSockListen, []uint64{0, 1}},
		{funcSockGetLocalAdd, []uint64{0, uint64(addr), 96}},
		{funcSockGetPeerAddr, []uint64{0, uint64(addr), 96}},
		{funcSockGetSockOpt, []uint64{0, 0, 1, 96, 4}},
		{funcSockSetSockOpt, []uint64{0, 0, 1, 96, 4}},
		{funcSockSendTo, []uint64{0, 200, 1, uint64(addr), 80, 0, 96}},
		{funcSockRecvFrom, []uint64{0, 200, 1, uint64(addr), 0, 96, 100, 104}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// fd 999 does not exist.
			bad := append([]uint64{999}, tc.params[1:]...)
			require.Equal(t, wasip1.ErrnoBadf, g.call(tc.name, bad...))

			// fd 0 is stdin: a descriptor, but not a socket.
			require.Equal(t, wasip1.ErrnoNotsock, g.call(tc.name, tc.params...))
		})
	}
}

// TestFunctions_addressFaults covers the address descriptor itself being
// unreadable or malformed, which a guest with a bug can produce.
func TestFunctions_addressFaults(t *testing.T) {
	g := newGuest(t, V2)
	fd := g.openSocket(addressFamilyInet4, socketTypeStream)

	t.Run("descriptor out of range", func(t *testing.T) {
		require.Equal(t, wasip1.ErrnoFault,
			g.call(funcSockConnect, uint64(fd), 1<<30, 80))
	})

	t.Run("buffer out of range", func(t *testing.T) {
		addr := g.writeAddrDescriptor(64, 1<<30, 128)
		require.Equal(t, wasip1.ErrnoFault,
			g.call(funcSockConnect, uint64(fd), uint64(addr), 80))
	})

	t.Run("unsupported buffer length", func(t *testing.T) {
		// Neither a V1 (4/16) nor a V2 (128) address.
		addr := g.writeAddrDescriptor(64, 1024, 7)
		require.Equal(t, wasip1.ErrnoInval,
			g.call(funcSockConnect, uint64(fd), uint64(addr), 80))
	})

	t.Run("unknown family", func(t *testing.T) {
		addr := g.writeV2Addr(99, []byte{127, 0, 0, 1})
		require.Equal(t, wasip1.ErrnoAfnosupport,
			g.call(funcSockConnect, uint64(fd), uint64(addr), 80))
	})

	t.Run("unterminated unix path", func(t *testing.T) {
		buf := make([]byte, 128)
		binary.LittleEndian.PutUint16(buf, uint16(addressFamilyUnix))
		for i := 2; i < 128; i++ {
			buf[i] = 'x' // no NUL anywhere
		}
		require.True(t, g.mod.Memory().Write(1024, buf))
		addr := g.writeAddrDescriptor(64, 1024, 128)
		require.Equal(t, wasip1.ErrnoInval,
			g.call(funcSockConnect, uint64(fd), uint64(addr), 80))
	})
}

// TestFunctions_v2AddressRoundTrip covers the V2 encoding for both IP
// families, which the loopback tests only exercise for IPv4.
func TestFunctions_v2AddressRoundTrip(t *testing.T) {
	g := newGuest(t, V2)

	t.Run("ipv6", func(t *testing.T) {
		fd := g.openSocket(addressFamilyInet6, socketTypeStream)
		addr := g.writeV2Addr(addressFamilyInet6, []byte{
			0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, // ::1
		})
		// Bind is enough to establish the address without a peer.
		require.Equal(t, wasip1.ErrnoSuccess, g.call(funcSockBind, uint64(fd), uint64(addr), 0))
		require.Equal(t, wasip1.ErrnoSuccess, g.call(funcSockListen, uint64(fd), 1))

		out := g.writeAddrDescriptor(64, 1024, 128)
		require.Equal(t, wasip1.ErrnoSuccess, g.call(funcSockGetLocalAdd, uint64(fd), uint64(out), 96))

		buf, ok := g.mod.Memory().Read(1024, 18)
		require.True(t, ok)
		require.Equal(t, uint16(addressFamilyInet6), binary.LittleEndian.Uint16(buf))
		require.Equal(t, byte(1), buf[17]) // ::1
		require.NotEqual(t, uint32(0), g.u32(96))
	})
}

// TestFunctions_optionValueLength covers the option calls rejecting a value
// that is not the four bytes an int option needs.
func TestFunctions_optionValueLength(t *testing.T) {
	g := newGuest(t, V2)
	fd := g.openSocket(addressFamilyInet4, socketTypeStream)

	require.Equal(t, wasip1.ErrnoInval, g.call(funcSockGetSockOpt, uint64(fd), 0, 1, 96, 8))
	require.Equal(t, wasip1.ErrnoInval, g.call(funcSockSetSockOpt, uint64(fd), 0, 7, 96, 2))

	// A well-formed query works, and reports the type the socket was opened as.
	require.Equal(t, wasip1.ErrnoSuccess, g.call(funcSockGetSockOpt, uint64(fd), 0, 1, 96, 4))
	require.Equal(t, uint32(socketTypeStream), g.u32(96))
}

// TestFunctions_flagsUnsupported covers the send and receive flags: nothing
// beyond zero is expressible through the backend, so a guest asking for
// out-of-band or peek must be told rather than silently given a plain
// transfer.
func TestFunctions_flagsUnsupported(t *testing.T) {
	g := newGuest(t, V2)
	fd := g.openSocket(addressFamilyInet4, socketTypeDatagram)
	addr := g.writeV2Addr(addressFamilyInet4, []byte{127, 0, 0, 1})

	require.Equal(t, wasip1.ErrnoNotsup,
		g.call(funcSockSendTo, uint64(fd), 200, 1, uint64(addr), 80, 1, 96))
	require.Equal(t, wasip1.ErrnoNotsup,
		g.call(funcSockRecvFrom, uint64(fd), 200, 1, uint64(addr), 1, 96, 100, 104))
}

// TestFunctions_iovecFaults covers the scatter/gather list pointing outside
// memory.
func TestFunctions_iovecFaults(t *testing.T) {
	g := newGuest(t, V2)
	fd := g.openSocket(addressFamilyInet4, socketTypeDatagram)
	addr := g.writeV2Addr(addressFamilyInet4, []byte{127, 0, 0, 1})

	// An iovec whose buffer is out of range.
	require.True(t, g.mod.Memory().WriteUint32Le(200, 1<<30))
	require.True(t, g.mod.Memory().WriteUint32Le(204, 8))
	require.Equal(t, wasip1.ErrnoFault,
		g.call(funcSockSendTo, uint64(fd), 200, 1, uint64(addr), 80, 0, 96))

	// The iovec array itself out of range.
	require.Equal(t, wasip1.ErrnoFault,
		g.call(funcSockSendTo, uint64(fd), 1<<30, 1, uint64(addr), 80, 0, 96))
}

// TestFunctions_v1RecvFrom covers V1's seven-parameter receive, which reports
// no source port -- the one shape the V1 guest test does not reach, since it
// uses streams.
func TestFunctions_v1RecvFrom(t *testing.T) {
	g := newGuest(t, V1)
	fd := g.openSocket(addressFamilyInet4, socketTypeDatagram)

	// Bind so the socket can receive, then send to itself.
	bindAddr := g.writeAddrDescriptor(64, 1024, 4)
	require.True(t, g.mod.Memory().Write(1024, []byte{127, 0, 0, 1}))
	require.Equal(t, wasip1.ErrnoSuccess, g.call(funcSockBind, uint64(fd), uint64(bindAddr), 0))

	// Find the port the socket was given.
	outAddr := g.writeAddrDescriptor(80, 1100, 16)
	require.Equal(t, wasip1.ErrnoSuccess,
		g.call(funcSockGetLocalAdd, uint64(fd), uint64(outAddr), 92, 96))
	require.Equal(t, uint32(4), g.u32(92)) // V1 reports the family separately
	port := g.u32(96)
	require.NotEqual(t, uint32(0), port)

	// Send a datagram to itself.
	require.True(t, g.mod.Memory().Write(300, []byte("ping")))
	require.True(t, g.mod.Memory().WriteUint32Le(200, 300))
	require.True(t, g.mod.Memory().WriteUint32Le(204, 4))
	sendAddr := g.writeAddrDescriptor(64, 1024, 4)
	require.Equal(t, wasip1.ErrnoSuccess,
		g.call(funcSockSendTo, uint64(fd), 200, 1, uint64(sendAddr), uint64(port), 0, 96))

	// Receive it: seven parameters, no port result.
	require.True(t, g.mod.Memory().WriteUint32Le(210, 400))
	require.True(t, g.mod.Memory().WriteUint32Le(214, 16))
	recvAddr := g.writeAddrDescriptor(80, 1100, 4)
	require.Equal(t, wasip1.ErrnoSuccess,
		g.call(funcSockRecvFrom, uint64(fd), 210, 1, uint64(recvAddr), 0, 100, 104))

	require.Equal(t, uint32(4), g.u32(100)) // bytes read
	got, ok := g.mod.Memory().Read(400, 4)
	require.True(t, ok)
	require.Equal(t, "ping", string(got))
}

// TestFunctions_openFaults covers sock_open reporting a bad family or an
// unwritable result pointer.
func TestFunctions_openFaults(t *testing.T) {
	g := newGuest(t, V2)

	require.Equal(t, wasip1.ErrnoAfnosupport, g.call(funcSockOpen, 99, uint64(socketTypeStream), 32))
	require.Equal(t, wasip1.ErrnoInval, g.call(funcSockOpen, uint64(addressFamilyInet4), 99, 32))
	require.Equal(t, wasip1.ErrnoFault,
		g.call(funcSockOpen, uint64(addressFamilyInet4), uint64(socketTypeStream), 1<<30))
}

// TestFunctions_addrInfoFaults covers the argument checks on getaddrinfo,
// whose result buffers all belong to the guest.
func TestFunctions_addrInfoFaults(t *testing.T) {
	g := newGuest(t, V2)

	// Neither a node nor a service.
	require.Equal(t, wasip1.ErrnoInval, g.call(funcSockGetAddrInfo, 0, 0, 0, 0, 0, 96, 1, 100))
	// No room for results.
	require.Equal(t, wasip1.ErrnoInval, g.call(funcSockGetAddrInfo, 300, 4, 0, 0, 0, 96, 0, 100))

	// A node that cannot resolve.
	require.True(t, g.mod.Memory().Write(300, []byte("no.such.host.invalid")))
	errno := g.call(funcSockGetAddrInfo, 300, 20, 0, 0, 0, 96, 1, 100)
	require.NotEqual(t, wasip1.ErrnoSuccess, errno)
}

// TestFunctions_resultFaults covers every function's result pointers being
// out of range. A guest with a bad pointer must get EFAULT rather than have
// the host silently drop the value it was asked to return.
func TestFunctions_resultFaults(t *testing.T) {
	const outOfRange = 1 << 30

	g := newGuest(t, V2)
	addr := g.writeV2Addr(addressFamilyInet4, []byte{127, 0, 0, 1})

	// A bound, listening socket, so the address getters have something to
	// report and reach their write step.
	fd := g.openSocket(addressFamilyInet4, socketTypeStream)
	require.Equal(t, wasip1.ErrnoSuccess, g.call(funcSockBind, uint64(fd), uint64(addr), 0))
	require.Equal(t, wasip1.ErrnoSuccess, g.call(funcSockListen, uint64(fd), 1))

	// A bound datagram socket for the send path.
	udp := g.openSocket(addressFamilyInet4, socketTypeDatagram)
	require.Equal(t, wasip1.ErrnoSuccess, g.call(funcSockBind, uint64(udp), uint64(addr), 0))
	require.True(t, g.mod.Memory().Write(300, []byte("x")))
	require.True(t, g.mod.Memory().WriteUint32Le(200, 300))
	require.True(t, g.mod.Memory().WriteUint32Le(204, 1))

	for _, tc := range []struct {
		name   string
		params []uint64
	}{
		{funcSockGetLocalAdd, []uint64{uint64(fd), uint64(addr), outOfRange}},
		{funcSockGetSockOpt, []uint64{uint64(fd), 0, 1, outOfRange, 4}},
		{funcSockSendTo, []uint64{uint64(udp), 200, 1, uint64(addr), 9, 0, outOfRange}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, wasip1.ErrnoFault, g.call(tc.name, tc.params...))
		})
	}

	// setsockopt reads its value through the same pointer.
	require.Equal(t, wasip1.ErrnoFault,
		g.call(funcSockSetSockOpt, uint64(fd), 0, 7, outOfRange, 4))

	// The address getters write into the guest's buffer, so a descriptor
	// pointing outside memory faults too.
	bad := g.writeAddrDescriptor(80, outOfRange, 128)
	require.Equal(t, wasip1.ErrnoFault,
		g.call(funcSockGetLocalAdd, uint64(fd), uint64(bad), 96))
}

// TestFunctions_addrInfoEntryFaults covers the result list itself being
// malformed: the guest owns those buffers, so every pointer in them is
// untrusted.
func TestFunctions_addrInfoEntryFaults(t *testing.T) {
	g := newGuest(t, V2)
	require.True(t, g.mod.Memory().Write(300, []byte("127.0.0.1")))

	// The list head points at an entry whose address pointer is null.
	require.True(t, g.mod.Memory().WriteUint32Le(400, 500)) // head -> entry at 500
	require.True(t, g.mod.Memory().WriteUint32Le(500+addrInfoOffAddress, 0))
	require.Equal(t, wasip1.ErrnoFault,
		g.call(funcSockGetAddrInfo, 300, 9, 0, 0, 0, 400, 1, 96))

	// An entry whose sockaddr claims less room than an address needs.
	require.True(t, g.mod.Memory().WriteUint32Le(500+addrInfoOffAddress, 600))
	require.True(t, g.mod.Memory().WriteUint32Le(600+4, 2)) // data length: too small
	require.True(t, g.mod.Memory().WriteUint32Le(600+8, 700))
	require.Equal(t, wasip1.ErrnoFault,
		g.call(funcSockGetAddrInfo, 300, 9, 0, 0, 0, 400, 1, 96))

	// A well-formed entry resolves, so the fault cases above are the only
	// difference.
	require.True(t, g.mod.Memory().WriteUint32Le(600+4, 26))
	require.Equal(t, wasip1.ErrnoSuccess,
		g.call(funcSockGetAddrInfo, 300, 9, 0, 0, 0, 400, 1, 96))
	require.Equal(t, uint32(1), g.u32(96))
}

// TestFunctions_partialResultFaults covers the second and third result writes
// faulting after the first succeeded. A pointer four bytes from the end of
// memory is writable once and not twice, which is exactly the shape a guest
// with an off-by-one buffer produces.
func TestFunctions_partialResultFaults(t *testing.T) {
	// The guest has two pages; the last four bytes are the only ones writable
	// at that offset.
	const lastWord = 2*65536 - 4

	g := newGuest(t, V2)
	addr := g.writeV2Addr(addressFamilyInet4, []byte{127, 0, 0, 1})

	fd := g.openSocket(addressFamilyInet4, socketTypeStream)
	require.Equal(t, wasip1.ErrnoSuccess, g.call(funcSockBind, uint64(fd), uint64(addr), 0))
	require.Equal(t, wasip1.ErrnoSuccess, g.call(funcSockListen, uint64(fd), 1))

	// getlocaladdr writes the address, then the port: the port write is the
	// one that runs off the end.
	out := g.writeAddrDescriptor(80, 1200, 128)
	require.Equal(t, wasip1.ErrnoFault,
		g.call(funcSockGetLocalAdd, uint64(fd), uint64(out), lastWord+4))

	// recv_from writes the source port, then the count, then the flags. Bind a
	// datagram socket and send to itself so a datagram is waiting.
	udp := g.openSocket(addressFamilyInet4, socketTypeDatagram)
	require.Equal(t, wasip1.ErrnoSuccess, g.call(funcSockBind, uint64(udp), uint64(addr), 0))
	require.Equal(t, wasip1.ErrnoSuccess, g.call(funcSockGetLocalAdd, uint64(udp), uint64(out), 96))
	port := g.u32(96)

	require.True(t, g.mod.Memory().Write(300, []byte("ping")))
	require.True(t, g.mod.Memory().WriteUint32Le(200, 300))
	require.True(t, g.mod.Memory().WriteUint32Le(204, 4))
	require.Equal(t, wasip1.ErrnoSuccess,
		g.call(funcSockSendTo, uint64(udp), 200, 1, uint64(addr), uint64(port), 0, 96))

	require.True(t, g.mod.Memory().WriteUint32Le(210, 400))
	require.True(t, g.mod.Memory().WriteUint32Le(214, 16))
	require.Equal(t, wasip1.ErrnoFault,
		g.call(funcSockRecvFrom, uint64(udp), 210, 1, uint64(out), 0, lastWord+4, 100, 104))
}

// TestFunctions_scatterFault covers the receive path's iovec buffer being out
// of range, which is only reachable once a datagram has actually arrived.
func TestFunctions_scatterFault(t *testing.T) {
	g := newGuest(t, V2)
	addr := g.writeV2Addr(addressFamilyInet4, []byte{127, 0, 0, 1})

	udp := g.openSocket(addressFamilyInet4, socketTypeDatagram)
	require.Equal(t, wasip1.ErrnoSuccess, g.call(funcSockBind, uint64(udp), uint64(addr), 0))

	out := g.writeAddrDescriptor(80, 1200, 128)
	require.Equal(t, wasip1.ErrnoSuccess, g.call(funcSockGetLocalAdd, uint64(udp), uint64(out), 96))
	port := g.u32(96)

	require.True(t, g.mod.Memory().Write(300, []byte("ping")))
	require.True(t, g.mod.Memory().WriteUint32Le(200, 300))
	require.True(t, g.mod.Memory().WriteUint32Le(204, 4))
	require.Equal(t, wasip1.ErrnoSuccess,
		g.call(funcSockSendTo, uint64(udp), 200, 1, uint64(addr), uint64(port), 0, 96))

	// The receive iovec points outside memory, so the datagram cannot land.
	require.True(t, g.mod.Memory().WriteUint32Le(210, 1<<30))
	require.True(t, g.mod.Memory().WriteUint32Le(214, 16))
	require.Equal(t, wasip1.ErrnoFault,
		g.call(funcSockRecvFrom, uint64(udp), 210, 1, uint64(out), 0, 96, 100, 104))
}

// TestWriteAddress covers the wire layouts directly, including the ones a
// loopback IPv4 test never produces.
func TestWriteAddress(t *testing.T) {
	g := newGuest(t, V2)
	mem := g.mod.Memory()

	tests := []struct {
		name          string
		bufLen        uint32
		host          string
		expectedType  int32
		expectedBytes []byte
		expectedErrno wasip1.Errno
	}{
		{
			// V1 with the small buffer: the four raw bytes, no family.
			name: "v1 ipv4", bufLen: 4, host: "10.0.0.1",
			expectedType: 4, expectedBytes: []byte{10, 0, 0, 1},
		},
		{
			// V1 with the large buffer still writes IPv4 as four bytes, so a
			// guest never sees a v4-mapped address it did not ask for.
			name: "v1 ipv4 in a 16-byte buffer", bufLen: 16, host: "10.0.0.1",
			expectedType: 4, expectedBytes: []byte{10, 0, 0, 1},
		},
		{
			name: "v1 ipv6", bufLen: 16, host: "::1",
			expectedType:  6,
			expectedBytes: []byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1},
		},
		{
			// IPv6 does not fit the small buffer, and truncating it would be
			// worse than refusing.
			name: "v1 ipv6 in a 4-byte buffer", bufLen: 4, host: "::1",
			expectedErrno: wasip1.ErrnoInval,
		},
		{
			// V1 has nowhere to put a path, so a unix address cannot be
			// reported through it.
			name: "v1 cannot carry a path", bufLen: 16, host: "/tmp/s",
			expectedErrno: wasip1.ErrnoInval,
		},
		{
			name: "v2 ipv4", bufLen: 128, host: "10.0.0.1", expectedType: 4,
			// Family first, little-endian, then the address.
			expectedBytes: []byte{1, 0, 10, 0, 0, 1},
		},
		{
			name: "v2 ipv6", bufLen: 128, host: "::1", expectedType: 6,
			expectedBytes: []byte{2, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1},
		},
		{
			// The type result is V1's separate family word, which has no
			// value for a path: V2 guests read the family out of the buffer,
			// where it is written as AF_UNIX.
			name: "v2 unix path", bufLen: 128, host: "/tmp/s", expectedType: 0,
			expectedBytes: append([]byte{3, 0}, append([]byte("/tmp/s"), 0)...),
		},
		{
			// A path that cannot fit alongside the family word.
			name: "v2 path too long", bufLen: 128, host: "/" + strings.Repeat("a", 130),
			expectedErrno: wasip1.ErrnoInval,
		},
		{
			// A length neither version uses.
			name: "unknown layout", bufLen: 7, host: "10.0.0.1",
			expectedErrno: wasip1.ErrnoInval,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Clear the buffer so a short write is visible.
			require.True(t, mem.Write(2048, make([]byte, 256)))
			addr := g.writeAddrDescriptor(64, 2048, tc.bufLen)

			addrType, errno := writeAddress(g.mod, addr, tc.host)
			if tc.expectedErrno != wasip1.ErrnoSuccess {
				require.Equal(t, tc.expectedErrno, errno)
				return
			}
			require.Equal(t, wasip1.ErrnoSuccess, errno)
			require.Equal(t, tc.expectedType, addrType)

			got, ok := mem.Read(2048, uint32(len(tc.expectedBytes)))
			require.True(t, ok)
			require.Equal(t, tc.expectedBytes, got)
		})
	}

	t.Run("buffer out of range", func(t *testing.T) {
		addr := g.writeAddrDescriptor(64, 1<<30, 128)
		_, errno := writeAddress(g.mod, addr, "10.0.0.1")
		require.Equal(t, wasip1.ErrnoFault, errno)
	})

	t.Run("descriptor out of range", func(t *testing.T) {
		_, errno := writeAddress(g.mod, 1<<30, "10.0.0.1")
		require.Equal(t, wasip1.ErrnoFault, errno)

		// The length half of the descriptor is a separate read, so the
		// descriptor straddling the end of memory faults too.
		_, errno = writeAddress(g.mod, 2*65536-4, "10.0.0.1")
		require.Equal(t, wasip1.ErrnoFault, errno)
	})
}

// TestReadGuestString covers the node and service strings, which arrive with a
// length that may or may not count a trailing NUL.
func TestReadGuestString(t *testing.T) {
	g := newGuest(t, V2)
	require.True(t, g.mod.Memory().Write(3000, []byte("localhost\x00")))

	t.Run("nul terminated", func(t *testing.T) {
		s, errno := readGuestString(g.mod, 3000, 10)
		require.Equal(t, wasip1.ErrnoSuccess, errno)
		require.Equal(t, "localhost", s)
	})

	t.Run("not nul terminated", func(t *testing.T) {
		s, errno := readGuestString(g.mod, 3000, 9)
		require.Equal(t, wasip1.ErrnoSuccess, errno)
		require.Equal(t, "localhost", s)
	})

	t.Run("empty is not a fault", func(t *testing.T) {
		// A guest omits the service by passing a zero length, so this cannot
		// be an error even though the pointer is never read.
		s, errno := readGuestString(g.mod, 0, 0)
		require.Equal(t, wasip1.ErrnoSuccess, errno)
		require.Equal(t, "", s)
	})

	t.Run("out of range", func(t *testing.T) {
		_, errno := readGuestString(g.mod, 1<<30, 4)
		require.Equal(t, wasip1.ErrnoFault, errno)
	})
}

// TestWriteAddrInfo covers the entry the guest allocated being unusable, which
// has to fault rather than write past what it reserved.
func TestWriteAddrInfo(t *testing.T) {
	g := newGuest(t, V2)
	mem := g.mod.Memory()

	// A well-formed entry: the sockaddr at 4200, its data at 4300.
	setup := func(dataLen uint32) uint32 {
		entry := uint32(4000)
		require.True(t, mem.Write(entry, make([]byte, addrInfoSize)))
		require.True(t, mem.WriteUint32Le(entry+addrInfoOffAddress, 4200))
		require.True(t, mem.WriteUint32Le(4200+4, dataLen))
		require.True(t, mem.WriteUint32Le(4200+8, 4300))
		return entry
	}

	t.Run("ipv6 needs the full length", func(t *testing.T) {
		// 14 is corrected to 26, which fits an IPv6 address.
		require.Equal(t, wasip1.ErrnoSuccess,
			writeAddrInfo(g.mod, setup(14), net.IPv6loopback, 443))

		got, ok := mem.Read(4300, 18)
		require.True(t, ok)
		require.Equal(t, []byte{1, 187}, got[:2]) // 443, network byte order
		require.Equal(t, net.IPv6loopback, net.IP(got[2:18]))
	})

	t.Run("data too small", func(t *testing.T) {
		// Six bytes holds an IPv4 address and port, but not an IPv6 one.
		require.Equal(t, wasip1.ErrnoSuccess,
			writeAddrInfo(g.mod, setup(6), net.IPv4(127, 0, 0, 1), 80))
		require.Equal(t, wasip1.ErrnoFault,
			writeAddrInfo(g.mod, setup(6), net.IPv6loopback, 80))
	})

	t.Run("null address pointer", func(t *testing.T) {
		entry := setup(26)
		require.True(t, mem.WriteUint32Le(entry+addrInfoOffAddress, 0))
		require.Equal(t, wasip1.ErrnoFault,
			writeAddrInfo(g.mod, entry, net.IPv4(127, 0, 0, 1), 80))
	})

	t.Run("address pointer out of range", func(t *testing.T) {
		entry := setup(26)
		require.True(t, mem.WriteUint32Le(entry+addrInfoOffAddress, 1<<30))
		require.Equal(t, wasip1.ErrnoFault,
			writeAddrInfo(g.mod, entry, net.IPv4(127, 0, 0, 1), 80))
	})

	t.Run("data pointer out of range", func(t *testing.T) {
		entry := setup(26)
		require.True(t, mem.WriteUint32Le(4200+8, 1<<30))
		require.Equal(t, wasip1.ErrnoFault,
			writeAddrInfo(g.mod, entry, net.IPv4(127, 0, 0, 1), 80))
	})

	t.Run("entry out of range", func(t *testing.T) {
		require.Equal(t, wasip1.ErrnoFault,
			writeAddrInfo(g.mod, 1<<30, net.IPv4(127, 0, 0, 1), 80))
	})
}
