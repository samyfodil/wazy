package wasmedge

import (
	"context"
	"encoding/binary"
	"net"

	"github.com/samyfodil/wazy/api"
	internalsys "github.com/samyfodil/wazy/internal/sys"
	"github.com/samyfodil/wazy/internal/wasip1"
	"github.com/samyfodil/wazy/internal/wasm"
)

// Function names, in the wasi_snapshot_preview1 module. A guest imports these
// by name, so they are the wire contract.
const (
	funcSockOpen        = "sock_open"
	funcSockBind        = "sock_bind"
	funcSockConnect     = "sock_connect"
	funcSockListen      = "sock_listen"
	funcSockAccept      = "sock_accept"
	funcSockSendTo      = "sock_send_to"
	funcSockRecvFrom    = "sock_recv_from"
	funcSockGetSockOpt  = "sock_getsockopt"
	funcSockSetSockOpt  = "sock_setsockopt"
	funcSockGetLocalAdd = "sock_getlocaladdr"
	funcSockGetPeerAddr = "sock_getpeeraddr"
	funcSockGetAddrInfo = "sock_getaddrinfo"
)

// fsc returns the calling module's file table.
func fsc(mod api.Module) *internalsys.FSContext {
	return mod.(*wasm.ModuleInstance).Sys.FS()
}

// lookupSocket returns the extension socket fd names, or an errno. A
// descriptor that exists but is not one of ours is ENOTSOCK, which is what a
// guest passing a file to a socket call should see.
func lookupSocket(mod api.Module, fd int32) (*socket, wasip1.Errno) {
	e, ok := fsc(mod).LookupFile(fd)
	if !ok {
		return nil, wasip1.ErrnoBadf
	}
	s, ok := e.File.(*socket)
	if !ok {
		return nil, wasip1.ErrnoNotsock
	}
	return s, wasip1.ErrnoSuccess
}

// --- address wire format ----------------------------------------------------
//
// The address parameter is not a pointer to the address bytes: it points at an
// 8-byte descriptor holding {buffer pointer, buffer length}. The length then
// selects the layout, which is how one implementation serves both versions:
//
//   - 4 or 16 bytes: V1, the raw IPv4 or IPv6 address, with the port passed
//     separately as its own i32 parameter.
//   - 128 bytes: V2, a 2-byte little-endian family followed by the address
//     data -- 4 or 16 bytes, or a NUL-terminated path for AF_UNIX.

// readAddress resolves the address a guest passed, returning it as a host
// string for the net package. port is used only by the V1 layout, which does
// not carry one.
func readAddress(mod api.Module, addrPtr uint32, port int) (host string, errno wasip1.Errno) {
	mem := mod.Memory()
	bufPtr, ok := mem.ReadUint32Le(addrPtr)
	if !ok {
		return "", wasip1.ErrnoFault
	}
	bufLen, ok := mem.ReadUint32Le(addrPtr + 4)
	if !ok {
		return "", wasip1.ErrnoFault
	}
	buf, ok := mem.Read(bufPtr, bufLen)
	if !ok {
		return "", wasip1.ErrnoFault
	}

	switch len(buf) {
	case 4:
		return net.IP(buf).String(), wasip1.ErrnoSuccess
	case 16:
		return net.IP(buf).String(), wasip1.ErrnoSuccess
	case 128:
		switch int32(binary.LittleEndian.Uint16(buf)) {
		case addressFamilyInet4:
			return net.IP(buf[2:6]).String(), wasip1.ErrnoSuccess
		case addressFamilyInet6:
			return net.IP(buf[2:18]).String(), wasip1.ErrnoSuccess
		case addressFamilyUnix:
			path := buf[2:]
			for i, b := range path {
				if b == 0 {
					return string(path[:i]), wasip1.ErrnoSuccess
				}
			}
			return "", wasip1.ErrnoInval // unterminated path
		default:
			return "", wasip1.ErrnoAfnosupport
		}
	default:
		return "", wasip1.ErrnoInval
	}
}

// writeAddress stores an address back into the guest's buffer, in whichever
// layout the buffer's length says it is. It returns the address type (4 or 6)
// for the V1 callers that report it separately.
func writeAddress(mod api.Module, addrPtr uint32, host string) (addrType int32, errno wasip1.Errno) {
	mem := mod.Memory()
	bufPtr, ok := mem.ReadUint32Le(addrPtr)
	if !ok {
		return 0, wasip1.ErrnoFault
	}
	bufLen, ok := mem.ReadUint32Le(addrPtr + 4)
	if !ok {
		return 0, wasip1.ErrnoFault
	}

	ip := net.ParseIP(host)
	switch bufLen {
	case 4, 16:
		// V1: the raw address only, with the family reported separately in the
		// addrType result. An IPv4 address is written as its 4 bytes even when
		// the guest supplied the larger buffer -- the reference does the same,
		// and a guest reading back 16 bytes would otherwise see a v4-mapped
		// v6 address it never asked for. (The reference also writes a family
		// word first and then overwrites it with the address, so no family is
		// observable in the buffer either way.)
		if ip4 := ip.To4(); ip4 != nil {
			return 4, writeBytes(mem, bufPtr, ip4)
		}
		if ip16 := ip.To16(); ip16 != nil && bufLen == 16 {
			return 6, writeBytes(mem, bufPtr, ip16)
		}
		return 0, wasip1.ErrnoInval
	case 128:
		// V2: family, then the address data.
		if ip4 := ip.To4(); ip4 != nil {
			if errno = writeFamily(mem, bufPtr, addressFamilyInet4); errno != wasip1.ErrnoSuccess {
				return 0, errno
			}
			return 4, writeBytes(mem, bufPtr+2, ip4)
		}
		if ip16 := ip.To16(); ip16 != nil {
			if errno = writeFamily(mem, bufPtr, addressFamilyInet6); errno != wasip1.ErrnoSuccess {
				return 0, errno
			}
			return 6, writeBytes(mem, bufPtr+2, ip16)
		}
		// Not an IP: a unix socket path.
		if len(host) > 125 {
			return 0, wasip1.ErrnoInval
		}
		if errno = writeFamily(mem, bufPtr, addressFamilyUnix); errno != wasip1.ErrnoSuccess {
			return 0, errno
		}
		if errno = writeBytes(mem, bufPtr+2, append([]byte(host), 0)); errno != wasip1.ErrnoSuccess {
			return 0, errno
		}
		return 0, wasip1.ErrnoSuccess
	default:
		return 0, wasip1.ErrnoInval
	}
}

func writeFamily(mem api.Memory, ptr uint32, family int32) wasip1.Errno {
	var b [2]byte
	binary.LittleEndian.PutUint16(b[:], uint16(family))
	return writeBytes(mem, ptr, b[:])
}

func writeBytes(mem api.Memory, ptr uint32, b []byte) wasip1.Errno {
	if !mem.Write(ptr, b) {
		return wasip1.ErrnoFault
	}
	return wasip1.ErrnoSuccess
}

// --- iovecs -----------------------------------------------------------------

// readIovecs gathers the guest's scatter/gather list into one buffer. The
// extension's send path is a single write, so gathering here keeps the socket
// backend free of iovec handling.
func readIovecs(mod api.Module, iovsPtr, iovsCount uint32) ([]byte, wasip1.Errno) {
	mem := mod.Memory()
	var total []byte
	for i := uint32(0); i < iovsCount; i++ {
		base := iovsPtr + i*8
		ptr, ok := mem.ReadUint32Le(base)
		if !ok {
			return nil, wasip1.ErrnoFault
		}
		length, ok := mem.ReadUint32Le(base + 4)
		if !ok {
			return nil, wasip1.ErrnoFault
		}
		if length == 0 {
			continue
		}
		b, ok := mem.Read(ptr, length)
		if !ok {
			return nil, wasip1.ErrnoFault
		}
		total = append(total, b...)
	}
	return total, wasip1.ErrnoSuccess
}

// scatter fills the guest's iovecs from one buffer, returning how much was
// written.
func scatter(mod api.Module, iovsPtr, iovsCount uint32, data []byte) (uint32, wasip1.Errno) {
	mem := mod.Memory()
	var written uint32
	for i := uint32(0); i < iovsCount && len(data) > 0; i++ {
		base := iovsPtr + i*8
		ptr, ok := mem.ReadUint32Le(base)
		if !ok {
			return 0, wasip1.ErrnoFault
		}
		length, ok := mem.ReadUint32Le(base + 4)
		if !ok {
			return 0, wasip1.ErrnoFault
		}
		n := uint32(len(data))
		if n > length {
			n = length
		}
		if n == 0 {
			continue
		}
		if !mem.Write(ptr, data[:n]) {
			return 0, wasip1.ErrnoFault
		}
		data = data[n:]
		written += n
	}
	return written, wasip1.ErrnoSuccess
}

// iovecsCapacity totals the iovec lengths, to size a receive buffer.
func iovecsCapacity(mod api.Module, iovsPtr, iovsCount uint32) (uint32, wasip1.Errno) {
	mem := mod.Memory()
	var total uint32
	for i := uint32(0); i < iovsCount; i++ {
		length, ok := mem.ReadUint32Le(iovsPtr + i*8 + 4)
		if !ok {
			return 0, wasip1.ErrnoFault
		}
		total += length
	}
	return total, wasip1.ErrnoSuccess
}

// --- host functions ---------------------------------------------------------

// sockOpen implements sock_open(family, sock_type, fd_out) -> errno.
func sockOpen(_ context.Context, mod api.Module, family, sockType, fdOut uint32) uint32 {
	s, errno := newSocket(int32(family), int32(sockType))
	if errno != wasip1.ErrnoSuccess {
		return uint32(errno)
	}
	fd, ferrno := fsc(mod).InsertFile(s)
	if ferrno != 0 {
		return uint32(wasip1.ToErrno(ferrno))
	}
	if !mod.Memory().WriteUint32Le(fdOut, uint32(fd)) {
		return uint32(wasip1.ErrnoFault)
	}
	return uint32(wasip1.ErrnoSuccess)
}

// sockBind implements sock_bind(fd, addr, port) -> errno.
func sockBind(_ context.Context, mod api.Module, fd, addrPtr, port uint32) uint32 {
	s, errno := lookupSocket(mod, int32(fd))
	if errno != wasip1.ErrnoSuccess {
		return uint32(errno)
	}
	host, errno := readAddress(mod, addrPtr, int(port))
	if errno != wasip1.ErrnoSuccess {
		return uint32(errno)
	}
	return uint32(s.bind(host, int(port)))
}

// sockConnect implements sock_connect(fd, addr, port) -> errno.
func sockConnect(_ context.Context, mod api.Module, fd, addrPtr, port uint32) uint32 {
	s, errno := lookupSocket(mod, int32(fd))
	if errno != wasip1.ErrnoSuccess {
		return uint32(errno)
	}
	host, errno := readAddress(mod, addrPtr, int(port))
	if errno != wasip1.ErrnoSuccess {
		return uint32(errno)
	}
	return uint32(s.connect(host, int(port)))
}

// sockListen implements sock_listen(fd, backlog) -> errno.
func sockListen(_ context.Context, mod api.Module, fd, backlog uint32) uint32 {
	s, errno := lookupSocket(mod, int32(fd))
	if errno != wasip1.ErrnoSuccess {
		return uint32(errno)
	}
	return uint32(s.listen(int32(backlog)))
}

// sockAcceptV1 implements V1's sock_accept(fd, fd_out) -> errno, which predates
// the WASI preview 1 signature and has no fdflags.
func sockAcceptV1(ctx context.Context, mod api.Module, fd, fdOut uint32) uint32 {
	return sockAccept(ctx, mod, fd, 0, fdOut)
}

// sockAccept implements sock_accept(fd, fdflags, fd_out) -> errno.
//
// This overrides the standard WASI function, which only accepts on a
// pre-opened listener; a guest that called sock_listen owns its own.
func sockAccept(_ context.Context, mod api.Module, fd, fdflags, fdOut uint32) uint32 {
	s, errno := lookupSocket(mod, int32(fd))
	if errno != wasip1.ErrnoSuccess {
		// Not one of ours: fall back to the standard behaviour for pre-opened
		// listeners, so both kinds of listener work under one export.
		connFD, ferrno := fsc(mod).SockAccept(int32(fd), fdflags&uint32(wasip1.FD_NONBLOCK) != 0)
		if ferrno != 0 {
			return uint32(wasip1.ToErrno(ferrno))
		}
		if !mod.Memory().WriteUint32Le(fdOut, uint32(connFD)) {
			return uint32(wasip1.ErrnoFault)
		}
		return uint32(wasip1.ErrnoSuccess)
	}

	conn, errno := s.accept()
	if errno != wasip1.ErrnoSuccess {
		return uint32(errno)
	}
	if fdflags&uint32(wasip1.FD_NONBLOCK) != 0 {
		conn.nonblock = true
	}
	connFD, ferrno := fsc(mod).InsertFile(conn)
	if ferrno != 0 {
		conn.Close()
		return uint32(wasip1.ToErrno(ferrno))
	}
	if !mod.Memory().WriteUint32Le(fdOut, uint32(connFD)) {
		return uint32(wasip1.ErrnoFault)
	}
	return uint32(wasip1.ErrnoSuccess)
}

// sockSendTo implements
// sock_send_to(fd, iovs, iovs_len, addr, port, flags, nwritten_out) -> errno.
func sockSendTo(_ context.Context, mod api.Module, fd, iovs, iovsLen, addrPtr, port, flags, nwrittenOut uint32) uint32 {
	if flags != 0 {
		return uint32(wasip1.ErrnoNotsup)
	}
	s, errno := lookupSocket(mod, int32(fd))
	if errno != wasip1.ErrnoSuccess {
		return uint32(errno)
	}
	host, errno := readAddress(mod, addrPtr, int(port))
	if errno != wasip1.ErrnoSuccess {
		return uint32(errno)
	}
	buf, errno := readIovecs(mod, iovs, iovsLen)
	if errno != wasip1.ErrnoSuccess {
		return uint32(errno)
	}
	n, errno := s.sendTo(buf, host, int(port))
	if errno != wasip1.ErrnoSuccess {
		return uint32(errno)
	}
	if !mod.Memory().WriteUint32Le(nwrittenOut, uint32(n)) {
		return uint32(wasip1.ErrnoFault)
	}
	return uint32(wasip1.ErrnoSuccess)
}

// recvFrom is the shared body of both versions of sock_recv_from. portOut is
// written only by V2, which reports the source port separately.
func recvFrom(mod api.Module, fd, iovs, iovsLen, addrPtr, iflags, nreadOut, oflagsOut uint32, portOut *uint32) uint32 {
	if iflags != 0 {
		return uint32(wasip1.ErrnoNotsup)
	}
	s, errno := lookupSocket(mod, int32(fd))
	if errno != wasip1.ErrnoSuccess {
		return uint32(errno)
	}
	capacity, errno := iovecsCapacity(mod, iovs, iovsLen)
	if errno != wasip1.ErrnoSuccess {
		return uint32(errno)
	}
	buf := make([]byte, capacity)

	n, host, port, errno := s.recvFrom(buf)
	if errno != wasip1.ErrnoSuccess {
		return uint32(errno)
	}
	written, errno := scatter(mod, iovs, iovsLen, buf[:n])
	if errno != wasip1.ErrnoSuccess {
		return uint32(errno)
	}
	if _, errno = writeAddress(mod, addrPtr, host); errno != wasip1.ErrnoSuccess {
		return uint32(errno)
	}

	mem := mod.Memory()
	if portOut != nil {
		if !mem.WriteUint32Le(*portOut, uint32(port)) {
			return uint32(wasip1.ErrnoFault)
		}
	}
	if !mem.WriteUint32Le(nreadOut, written) {
		return uint32(wasip1.ErrnoFault)
	}
	if !mem.WriteUint32Le(oflagsOut, 0) {
		return uint32(wasip1.ErrnoFault)
	}
	return uint32(wasip1.ErrnoSuccess)
}

// sockRecvFromV1 implements
// sock_recv_from(fd, iovs, iovs_len, addr, iflags, nread_out, oflags_out).
func sockRecvFromV1(_ context.Context, mod api.Module, fd, iovs, iovsLen, addrPtr, iflags, nreadOut, oflagsOut uint32) uint32 {
	return recvFrom(mod, fd, iovs, iovsLen, addrPtr, iflags, nreadOut, oflagsOut, nil)
}

// sockRecvFromV2 implements the same with the source port reported separately:
// sock_recv_from(fd, iovs, iovs_len, addr, iflags, port_out, nread_out, oflags_out).
func sockRecvFromV2(_ context.Context, mod api.Module, fd, iovs, iovsLen, addrPtr, iflags, portOut, nreadOut, oflagsOut uint32) uint32 {
	return recvFrom(mod, fd, iovs, iovsLen, addrPtr, iflags, nreadOut, oflagsOut, &portOut)
}

// sockGetSockOpt implements
// sock_getsockopt(fd, level, option, value_out, value_len) -> errno.
func sockGetSockOpt(_ context.Context, mod api.Module, fd, level, option, valueOut, valueLen uint32) uint32 {
	s, errno := lookupSocket(mod, int32(fd))
	if errno != wasip1.ErrnoSuccess {
		return uint32(errno)
	}
	if valueLen != 4 { // only int-valued options are expressible
		return uint32(wasip1.ErrnoInval)
	}
	value, errno := s.getOpt(int32(level), int32(option))
	if errno != wasip1.ErrnoSuccess {
		return uint32(errno)
	}
	if !mod.Memory().WriteUint32Le(valueOut, uint32(value)) {
		return uint32(wasip1.ErrnoFault)
	}
	return uint32(wasip1.ErrnoSuccess)
}

// sockSetSockOpt implements
// sock_setsockopt(fd, level, option, value, value_len) -> errno.
func sockSetSockOpt(_ context.Context, mod api.Module, fd, level, option, valuePtr, valueLen uint32) uint32 {
	s, errno := lookupSocket(mod, int32(fd))
	if errno != wasip1.ErrnoSuccess {
		return uint32(errno)
	}
	if valueLen != 4 {
		return uint32(wasip1.ErrnoInval)
	}
	value, ok := mod.Memory().ReadUint32Le(valuePtr)
	if !ok {
		return uint32(wasip1.ErrnoFault)
	}
	return uint32(s.setOpt(int32(level), int32(option), int32(value)))
}

// localOrPeerAddr is the shared body of the four address getters. addrTypeOut
// is written only by V1, which reports the family separately; V2 encodes it in
// the address buffer.
func localOrPeerAddr(mod api.Module, fd, addrPtr uint32, addrTypeOut *uint32, portOut uint32, peer bool) uint32 {
	s, errno := lookupSocket(mod, int32(fd))
	if errno != wasip1.ErrnoSuccess {
		return uint32(errno)
	}

	var host string
	var port int
	if peer {
		host, port, errno = s.peerAddr()
	} else {
		host, port, errno = s.localAddr()
	}
	if errno != wasip1.ErrnoSuccess {
		return uint32(errno)
	}

	addrType, errno := writeAddress(mod, addrPtr, host)
	if errno != wasip1.ErrnoSuccess {
		return uint32(errno)
	}

	mem := mod.Memory()
	if addrTypeOut != nil {
		if !mem.WriteUint32Le(*addrTypeOut, uint32(addrType)) {
			return uint32(wasip1.ErrnoFault)
		}
	}
	if !mem.WriteUint32Le(portOut, uint32(port)) {
		return uint32(wasip1.ErrnoFault)
	}
	return uint32(wasip1.ErrnoSuccess)
}

func sockGetLocalAddrV1(_ context.Context, mod api.Module, fd, addrPtr, addrTypeOut, portOut uint32) uint32 {
	return localOrPeerAddr(mod, fd, addrPtr, &addrTypeOut, portOut, false)
}

func sockGetPeerAddrV1(_ context.Context, mod api.Module, fd, addrPtr, addrTypeOut, portOut uint32) uint32 {
	return localOrPeerAddr(mod, fd, addrPtr, &addrTypeOut, portOut, true)
}

func sockGetLocalAddrV2(_ context.Context, mod api.Module, fd, addrPtr, portOut uint32) uint32 {
	return localOrPeerAddr(mod, fd, addrPtr, nil, portOut, false)
}

func sockGetPeerAddrV2(_ context.Context, mod api.Module, fd, addrPtr, portOut uint32) uint32 {
	return localOrPeerAddr(mod, fd, addrPtr, nil, portOut, true)
}
