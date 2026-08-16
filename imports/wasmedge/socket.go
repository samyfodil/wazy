package wasmedge

import (
	"errors"
	"net"
	"os"
	"strconv"
	"syscall"
	"time"

	socketapi "github.com/samyfodil/wazy/internal/sock"
	"github.com/samyfodil/wazy/internal/wasip1"
	"github.com/samyfodil/wazy/sys"
)

// The extension uses WasmEdge's own enums, not the host platform's. A guest
// passes these values to sock_open, so they are part of the wire contract.
// See https://github.com/second-state/wasmedge_wasi_socket.
const (
	addressFamilyUnspec int32 = 0
	addressFamilyInet4  int32 = 1
	addressFamilyInet6  int32 = 2
	addressFamilyUnix   int32 = 3

	socketTypeAny      int32 = 0
	socketTypeDatagram int32 = 1
	socketTypeStream   int32 = 2
)

// Socket option levels and options, again WasmEdge's numbering: the SOL_SOCKET
// options are numbered from zero in declaration order rather than by the
// platform's values, and TCP_NODELAY is 15 under level 6.
const (
	levelSocket int32 = 0
	levelTCP    int32 = 6

	optReuseAddress           int32 = 0
	optQuerySocketType        int32 = 1
	optQuerySocketError       int32 = 2
	optDontRoute              int32 = 3
	optBroadcast              int32 = 4
	optSendBufferSize         int32 = 5
	optRecvBufferSize         int32 = 6
	optKeepAlive              int32 = 7
	optOOBInline              int32 = 8
	optLinger                 int32 = 9
	optRecvLowWatermark       int32 = 10
	optRecvTimeout            int32 = 11
	optSendTimeout            int32 = 12
	optQueryAcceptConnections int32 = 13
	optBindToDevice           int32 = 14

	optTCPNoDelay int32 = 15
)

// socket is a socket created by sock_open, held in the file table like any
// other descriptor.
//
// It implements socketapi.TCPConn and socketapi.TCPSock, so the *standard*
// WASI functions work on it unchanged: a guest reads with sock_recv or
// fd_read, writes with sock_send or fd_write, and closes with fd_close. The
// extension only has to add what WASI preview 1 has no equivalent for.
//
// The backend is the net package rather than raw syscalls. The extension needs
// nothing net does not already provide, and net brings portability and the
// runtime poller with it; the cost is that a few options net does not expose
// report ENOTSUP, which is the same baseline the reference implementation sets.
//
// A socket exists before it is bound, connected or listening -- sock_open
// creates one the way BSD socket(2) does -- so conn, listener and packet are
// all nil until an operation establishes one.
type socket struct {
	sys.UnimplementedFile

	family   int32
	sockType int32

	// laddr is the address recorded by sock_bind. net has no bind-then-listen
	// split, so it is applied when the socket is listened on or connected.
	laddr string

	conn     net.Conn       // stream, once connected or accepted
	listener net.Listener   // stream, once listening
	packet   net.PacketConn // datagram, once bound

	nonblock bool
	closed   bool
}

var (
	_ sys.File          = (*socket)(nil)
	_ socketapi.TCPConn = (*socket)(nil)
	_ socketapi.TCPSock = (*socket)(nil)
	_ sys.PollableFile  = (*socket)(nil)
)

// newSocket returns a socket for the given WasmEdge family and type.
func newSocket(family, sockType int32) (*socket, wasip1.Errno) {
	switch family {
	case addressFamilyInet4, addressFamilyInet6, addressFamilyUnix:
	case addressFamilyUnspec:
		family = addressFamilyInet4
	default:
		return nil, wasip1.ErrnoAfnosupport
	}
	switch sockType {
	case socketTypeStream, socketTypeDatagram:
	case socketTypeAny:
		sockType = socketTypeStream
	default:
		return nil, wasip1.ErrnoInval
	}
	return &socket{family: family, sockType: sockType}, wasip1.ErrnoSuccess
}

// network returns the net package network name for this socket.
func (f *socket) network() string {
	if f.family == addressFamilyUnix {
		if f.sockType == socketTypeDatagram {
			return "unixgram"
		}
		return "unix"
	}
	proto := "tcp"
	if f.sockType == socketTypeDatagram {
		proto = "udp"
	}
	if f.family == addressFamilyInet6 {
		return proto + "6"
	}
	return proto + "4"
}

// address renders host and port the way net wants them. For AF_UNIX the host
// is the socket path and the port is ignored.
func (f *socket) address(host string, port int) string {
	if f.family == addressFamilyUnix {
		return host
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}

// bind records the local address. net offers no bind-without-listen for
// streams, so for those it is applied later; a datagram socket is opened now,
// since it can receive with no further call.
func (f *socket) bind(host string, port int) wasip1.Errno {
	if f.closed {
		return wasip1.ErrnoBadf
	}
	f.laddr = f.address(host, port)

	if f.sockType != socketTypeDatagram {
		return wasip1.ErrnoSuccess
	}
	if f.packet != nil {
		return wasip1.ErrnoInval // already bound
	}
	pc, err := net.ListenPacket(f.network(), f.laddr)
	if err != nil {
		return toWasiErrno(err)
	}
	f.packet = pc
	return wasip1.ErrnoSuccess
}

// listen starts accepting connections. The backlog is advisory: net does not
// expose it, and the platform caps it regardless.
func (f *socket) listen(int32) wasip1.Errno {
	if f.closed {
		return wasip1.ErrnoBadf
	}
	if f.sockType == socketTypeDatagram {
		return wasip1.ErrnoNotsup
	}
	if f.listener != nil {
		return wasip1.ErrnoInval
	}
	laddr := f.laddr
	if laddr == "" {
		laddr = ":0"
	}
	l, err := net.Listen(f.network(), laddr)
	if err != nil {
		return toWasiErrno(err)
	}
	f.listener = l
	return wasip1.ErrnoSuccess
}

// Accept implements the same method as documented on socketapi.TCPSock.
func (f *socket) Accept() (socketapi.TCPConn, sys.Errno) {
	conn, errno := f.accept()
	if errno != wasip1.ErrnoSuccess {
		return nil, sys.EBADF
	}
	return conn, 0
}

// accept returns the next connection as a new socket, ready for the file
// table.
func (f *socket) accept() (*socket, wasip1.Errno) {
	if f.closed {
		return nil, wasip1.ErrnoBadf
	}
	if f.listener == nil {
		return nil, wasip1.ErrnoInval
	}
	if f.nonblock {
		// net has no non-blocking accept; a deadline already in the past turns
		// "nothing pending" into a timeout, which is EAGAIN to the guest.
		if l, ok := f.listener.(interface{ SetDeadline(time.Time) error }); ok {
			_ = l.SetDeadline(time.Now())
			defer func() { _ = l.SetDeadline(time.Time{}) }()
		}
	}
	conn, err := f.listener.Accept()
	if err != nil {
		return nil, toWasiErrno(err)
	}
	return &socket{family: f.family, sockType: f.sockType, conn: conn}, wasip1.ErrnoSuccess
}

// connect dials the peer, from the bound local address if one was set.
func (f *socket) connect(host string, port int) wasip1.Errno {
	if f.closed {
		return wasip1.ErrnoBadf
	}
	if f.conn != nil {
		return wasip1.ErrnoIsconn
	}
	d := net.Dialer{}
	if f.laddr != "" {
		if a, errno := f.resolveAddrString(f.laddr); errno == wasip1.ErrnoSuccess {
			d.LocalAddr = a
		}
	}
	conn, err := d.Dial(f.network(), f.address(host, port))
	if err != nil {
		return toWasiErrno(err)
	}
	f.conn = conn
	// A connected datagram socket reads and writes through conn, so drop any
	// unconnected handle bind created.
	if f.packet != nil {
		f.packet.Close()
		f.packet = nil
	}
	return wasip1.ErrnoSuccess
}

// Read implements the same method as documented on sys.File.
func (f *socket) Read(buf []byte) (int, sys.Errno) {
	if f.closed {
		return 0, sys.EBADF
	}
	if len(buf) == 0 {
		return 0, 0
	}
	if f.conn == nil {
		if f.packet != nil { // an unconnected datagram socket reads with recv_from
			n, _, _, errno := f.recvFrom(buf)
			return n, toFileErrno(errno)
		}
		return 0, sys.EBADF
	}
	if f.nonblock {
		if err := f.conn.SetReadDeadline(time.Now()); err != nil {
			return 0, sys.EIO
		}
		defer func() { _ = f.conn.SetReadDeadline(time.Time{}) }()
	}
	n, err := f.conn.Read(buf)
	if err != nil {
		return n, toFileErrno(toWasiErrno(err))
	}
	return n, 0
}

// Write implements the same method as documented on sys.File.
func (f *socket) Write(buf []byte) (int, sys.Errno) {
	if f.closed {
		return 0, sys.EBADF
	}
	if len(buf) == 0 {
		return 0, 0
	}
	if f.conn == nil {
		return 0, sys.EBADF
	}
	n, err := f.conn.Write(buf)
	if err != nil {
		return n, toFileErrno(toWasiErrno(err))
	}
	return n, 0
}

// Recvfrom implements the same method as documented on socketapi.TCPConn. It
// exists for the standard sock_recv path, which uses it for MSG_PEEK.
func (f *socket) Recvfrom(p []byte, flags int) (int, sys.Errno) {
	if flags != 0 {
		return 0, sys.ENOTSUP // only a plain receive is expressible through net
	}
	return f.Read(p)
}

// sendTo writes to an explicit destination, for unconnected datagram sockets.
func (f *socket) sendTo(buf []byte, host string, port int) (int, wasip1.Errno) {
	if f.closed {
		return 0, wasip1.ErrnoBadf
	}
	// A connected socket ignores the destination, as sendto(2) does.
	if f.conn != nil {
		n, errno := f.Write(buf)
		return n, wasip1.ToErrno(errno)
	}
	if f.packet == nil {
		// Sending before binding is allowed: open on any port now.
		pc, err := net.ListenPacket(f.network(), ":0")
		if err != nil {
			return 0, toWasiErrno(err)
		}
		f.packet = pc
	}
	addr, errno := f.resolveAddr(host, port)
	if errno != wasip1.ErrnoSuccess {
		return 0, errno
	}
	n, err := f.packet.WriteTo(buf, addr)
	if err != nil {
		return n, toWasiErrno(err)
	}
	return n, wasip1.ErrnoSuccess
}

// recvFrom reads a datagram and reports its source.
func (f *socket) recvFrom(buf []byte) (n int, host string, port int, errno wasip1.Errno) {
	if f.closed {
		return 0, "", 0, wasip1.ErrnoBadf
	}
	if f.conn != nil {
		// Connected: the peer is known, so report it.
		var ferrno sys.Errno
		n, ferrno = f.Read(buf)
		if ferrno != 0 {
			return 0, "", 0, wasip1.ToErrno(ferrno)
		}
		host, port = splitAddr(f.conn.RemoteAddr())
		return n, host, port, wasip1.ErrnoSuccess
	}
	if f.packet == nil {
		return 0, "", 0, wasip1.ErrnoNotconn
	}
	if f.nonblock {
		_ = f.packet.SetReadDeadline(time.Now())
		defer func() { _ = f.packet.SetReadDeadline(time.Time{}) }()
	}
	n, addr, err := f.packet.ReadFrom(buf)
	if err != nil {
		return n, "", 0, toWasiErrno(err)
	}
	host, port = splitAddr(addr)
	return n, host, port, wasip1.ErrnoSuccess
}

// localAddr reports the address this socket is bound to.
func (f *socket) localAddr() (host string, port int, errno wasip1.Errno) {
	switch {
	case f.conn != nil:
		host, port = splitAddr(f.conn.LocalAddr())
	case f.listener != nil:
		host, port = splitAddr(f.listener.Addr())
	case f.packet != nil:
		host, port = splitAddr(f.packet.LocalAddr())
	default:
		return "", 0, wasip1.ErrnoNotconn
	}
	return host, port, wasip1.ErrnoSuccess
}

// peerAddr reports the address this socket is connected to.
func (f *socket) peerAddr() (host string, port int, errno wasip1.Errno) {
	if f.conn == nil {
		return "", 0, wasip1.ErrnoNotconn
	}
	host, port = splitAddr(f.conn.RemoteAddr())
	return host, port, wasip1.ErrnoSuccess
}

// setOpt supports the options net exposes and reports ENOTSUP for the rest,
// which is the baseline the reference implementation sets.
func (f *socket) setOpt(level, option, value int32) wasip1.Errno {
	if f.closed {
		return wasip1.ErrnoBadf
	}
	if level == levelTCP {
		if option != optTCPNoDelay {
			return wasip1.ErrnoNotsup
		}
		tc, ok := f.conn.(*net.TCPConn)
		if !ok {
			return wasip1.ErrnoNotsup
		}
		return toWasiErrno(tc.SetNoDelay(value != 0))
	}
	if level != levelSocket {
		return wasip1.ErrnoNotsup
	}

	switch option {
	case optKeepAlive:
		tc, ok := f.conn.(*net.TCPConn)
		if !ok {
			return wasip1.ErrnoNotsup
		}
		return toWasiErrno(tc.SetKeepAlive(value != 0))
	case optSendBufferSize:
		return f.setBuffer(true, int(value))
	case optRecvBufferSize:
		return f.setBuffer(false, int(value))
	case optReuseAddress:
		// net sets SO_REUSEADDR on listeners itself, so accept and ignore it
		// rather than failing a guest that sets it out of habit.
		return wasip1.ErrnoSuccess
	default:
		// Linger, RecvTimeout, SendTimeout, BindToDevice, DontRoute, Broadcast,
		// OOBInline and RecvLowWatermark are not reachable through net.
		return wasip1.ErrnoNotsup
	}
}

// getOpt answers the options that need no raw socket.
func (f *socket) getOpt(level, option int32) (int32, wasip1.Errno) {
	if f.closed {
		return 0, wasip1.ErrnoBadf
	}
	if level == levelSocket {
		switch option {
		case optQuerySocketType:
			return f.sockType, wasip1.ErrnoSuccess
		case optQueryAcceptConnections:
			if f.listener != nil {
				return 1, wasip1.ErrnoSuccess
			}
			return 0, wasip1.ErrnoSuccess
		case optQuerySocketError:
			return 0, wasip1.ErrnoSuccess // net reports errors per call, never pending
		}
	}
	return 0, wasip1.ErrnoNotsup
}

func (f *socket) setBuffer(send bool, size int) wasip1.Errno {
	type bufSetter interface {
		SetReadBuffer(int) error
		SetWriteBuffer(int) error
	}
	var bs bufSetter
	var ok bool
	switch {
	case f.conn != nil:
		bs, ok = f.conn.(bufSetter)
	case f.packet != nil:
		bs, ok = f.packet.(bufSetter)
	default:
		return wasip1.ErrnoNotconn
	}
	if !ok {
		return wasip1.ErrnoNotsup
	}
	if send {
		return toWasiErrno(bs.SetWriteBuffer(size))
	}
	return toWasiErrno(bs.SetReadBuffer(size))
}

// Shutdown implements the same method as documented on socketapi.TCPConn.
func (f *socket) Shutdown(how int) sys.Errno {
	if f.closed {
		return sys.EBADF
	}
	type halfCloser interface {
		CloseRead() error
		CloseWrite() error
	}
	hc, ok := f.conn.(halfCloser)
	if !ok {
		return f.Close()
	}
	switch how {
	case socketapi.SHUT_RD:
		return toFileErrno(toWasiErrno(hc.CloseRead()))
	case socketapi.SHUT_WR:
		return toFileErrno(toWasiErrno(hc.CloseWrite()))
	default:
		return f.Close()
	}
}

// Close implements the same method as documented on sys.File.
func (f *socket) Close() sys.Errno {
	if f.closed {
		return 0
	}
	f.closed = true
	var err error
	if f.conn != nil {
		err = f.conn.Close()
	}
	if f.listener != nil {
		if lerr := f.listener.Close(); err == nil {
			err = lerr
		}
	}
	if f.packet != nil {
		if perr := f.packet.Close(); err == nil {
			err = perr
		}
	}
	return toFileErrno(toWasiErrno(err))
}

// IsNonblock implements the same method as documented on sys.PollableFile.
func (f *socket) IsNonblock() bool { return f.nonblock }

// SetNonblock implements the same method as documented on sys.PollableFile.
//
// net always runs through the runtime poller, so this is emulated with zero
// deadlines at each operation rather than by flipping O_NONBLOCK on the
// descriptor, which keeps behaviour identical across platforms.
func (f *socket) SetNonblock(enable bool) sys.Errno {
	f.nonblock = enable
	return 0
}

// Poll implements the same method as documented on sys.Pollable. wazy's own
// socket files report ENOSYS here too.
func (f *socket) Poll(sys.Pflag, int32) (bool, sys.Errno) { return false, sys.ENOSYS }

// Stat implements the same method as documented on sys.File.
func (f *socket) Stat() (sys.Stat_t, sys.Errno) {
	return sys.Stat_t{Mode: os.ModeSocket, Nlink: 1}, 0
}

// IsDir implements the same method as documented on sys.File.
func (f *socket) IsDir() (bool, sys.Errno) { return false, sys.ENOTDIR }

func (f *socket) resolveAddr(host string, port int) (net.Addr, wasip1.Errno) {
	return f.resolveAddrString(f.address(host, port))
}

func (f *socket) resolveAddrString(addr string) (net.Addr, wasip1.Errno) {
	switch {
	case f.family == addressFamilyUnix:
		a, err := net.ResolveUnixAddr(f.network(), addr)
		return a, toWasiErrno(err)
	case f.sockType == socketTypeDatagram:
		a, err := net.ResolveUDPAddr(f.network(), addr)
		return a, toWasiErrno(err)
	default:
		a, err := net.ResolveTCPAddr(f.network(), addr)
		return a, toWasiErrno(err)
	}
}

// splitAddr renders a net.Addr as the host and port the wire format wants.
func splitAddr(addr net.Addr) (host string, port int) {
	switch a := addr.(type) {
	case *net.TCPAddr:
		return a.IP.String(), a.Port
	case *net.UDPAddr:
		return a.IP.String(), a.Port
	case *net.UnixAddr:
		return a.Name, 0
	case nil:
		return "", 0
	}
	h, p, err := net.SplitHostPort(addr.String())
	if err != nil {
		return addr.String(), 0
	}
	n, _ := strconv.Atoi(p)
	return h, n
}

// toWasiErrno maps an error from net to the errno the guest sees. Guests
// written against sockets branch on these -- Rust's std maps ECONNREFUSED and
// friends to distinct error kinds -- so the mapping is by syscall errno rather
// than collapsing everything to EIO.
func toWasiErrno(err error) wasip1.Errno {
	if err == nil {
		return wasip1.ErrnoSuccess
	}
	// A deadline already in the past is how non-blocking mode is emulated, so
	// it is EAGAIN rather than a timeout.
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return wasip1.ErrnoAgain
	}
	if errors.Is(err, net.ErrClosed) {
		return wasip1.ErrnoBadf
	}
	var se syscall.Errno
	if errors.As(err, &se) {
		switch se {
		case syscall.ECONNREFUSED:
			return wasip1.ErrnoConnrefused
		case syscall.ECONNRESET:
			return wasip1.ErrnoConnreset
		case syscall.ECONNABORTED:
			return wasip1.ErrnoConnaborted
		case syscall.EADDRINUSE:
			return wasip1.ErrnoAddrinuse
		case syscall.EADDRNOTAVAIL:
			return wasip1.ErrnoAddrnotavail
		case syscall.EHOSTUNREACH:
			return wasip1.ErrnoHostunreach
		case syscall.ENETUNREACH:
			return wasip1.ErrnoNetunreach
		case syscall.EPIPE:
			return wasip1.ErrnoPipe
		case syscall.EACCES:
			return wasip1.ErrnoAcces
		case syscall.EAGAIN:
			return wasip1.ErrnoAgain
		case syscall.EINVAL:
			return wasip1.ErrnoInval
		}
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return wasip1.ErrnoNoent
	}
	return wasip1.ErrnoIo
}

// toFileErrno narrows a WASI errno to the sys.Errno set the sys.File methods
// return. wazy's sys.Errno covers the file surface, not the socket one, so
// socket-specific conditions land on the nearest file equivalent -- callers
// that need the exact errno take it from the socket methods instead.
func toFileErrno(errno wasip1.Errno) sys.Errno {
	switch errno {
	case wasip1.ErrnoSuccess:
		return 0
	case wasip1.ErrnoAgain:
		return sys.EAGAIN
	case wasip1.ErrnoBadf:
		return sys.EBADF
	case wasip1.ErrnoInval:
		return sys.EINVAL
	case wasip1.ErrnoAcces:
		return sys.EACCES
	case wasip1.ErrnoNotsup:
		return sys.ENOTSUP
	default:
		return sys.EIO
	}
}
