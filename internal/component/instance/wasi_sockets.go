package instance

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"syscall"

	"github.com/samyfodil/wazy/internal/component/abi"
	"github.com/samyfodil/wazy/internal/component/binary"
)

// This file extends wasi.go/wasi_fs.go's WASI 0.2 host surface with a real
// wasi:sockets (TCP-only) + wasi:io/poll implementation, backed by real Go
// net.Conns -- see WASIConfig.AllowTCP/Dialer's doc for how a caller opts in.
//
// # Discovery
//
// Instantiating testdata/real_tcp.component.wasm (a genuine rustc
// wasm32-wasip2 guest built from `std::net::TcpStream::connect(addr)` +
// write_all + read_to_end + print -- std::net DOES work end-to-end against
// wasip2's wasi:sockets, confirmed by running the same .wasm under a real
// `wasmtime run -S inherit-network` against a scratch Go TCP server before
// any of this file existed) and inspecting Instance.WASICalls() without a
// real implementation registered shows every WASI interface the guest's
// compiled glue declares an import for, in declaration order -- not
// necessarily every one it actually calls at runtime. The funcs this file
// registers are exactly the ones a real connect+write+read_to_end+print
// run reaches:
//
//   - wasi:sockets/instance-network.instance-network -> own<network> (a
//     single, stateless singleton handle -- see wasiNetworkRep's doc)
//   - wasi:sockets/tcp-create-socket.create-tcp-socket
//   - wasi:sockets/tcp [method]tcp-socket.{start-connect,finish-connect,
//     subscribe}
//   - wasi:io/poll [method]pollable.block, and the free poll() func (WIT-
//     complete even though this particular guest binary's compiled glue
//     never actually calls the list-based free func at runtime -- it awaits
//     exactly one pollable at a time via [method]pollable.block instead --
//     matching this package's existing practice of registering a real,
//     WIT-correct impl for a func immediately adjacent to ones a fixture
//     does reach, e.g. wasi_fs.go's append-via-stream)
//   - wasi:io/streams [method]input-stream.subscribe,
//     [method]output-stream.subscribe (a tcp-socket's connect pollable and
//     an input/output-stream's data-ready pollable are the same WIT type,
//     wasi:io/poll's `pollable`, so one resource tag/dispatch serves both)
//
// wasi:sockets/udp*, wasi:sockets/ip-name-lookup, and wasi:sockets/network
// itself (which has no methods of its own, only used as an opaque
// borrow<network> arg) are declared imports too (Rust's wasi-sockets glue
// links the whole package even though a single TcpStream::connect only
// calls into the tcp/instance-network/poll slice above), but nothing in
// this fixture's runtime path invokes udp/ip-name-lookup -- TcpStream::
// connect's ToSocketAddrs parses a literal "ip:port" string directly
// without ever going through the resolver -- so those stay unregistered,
// left to the graph engine's own automatic trap-stub fallback (same
// deliberate omission wasi.go's package doc already documents for
// wasi:sockets as a whole, now narrowed now that TCP itself is real).
//
// # Blocking, single-shot connect/read/write model
//
// A real WASI host's start-connect/finish-connect pair is asynchronous:
// start-connect begins a non-blocking connect, and the guest is expected to
// subscribe+block on a pollable, retrying finish-connect until it stops
// reporting error-code::would-block. This package does not need that
// complexity to be correct: start-connect itself performs the real,
// blocking net.Dial synchronously (sockets.dial, injected via
// WASIConfig.Dialer/AllowTCP) and records the outcome on the tcp-socket
// node before returning -- by the time the guest later subscribes+blocks
// and calls finish-connect, the outcome is already known, so
// finish-connect never has anything to report but the final Ok/Err
// (wasiSockErrWouldBlock, though declared, is consequently never actually
// returned by this implementation). Every pollable this package mints
// (tcp-socket connect-readiness, input/output-stream data-readiness) is
// consequently always already "ready" the moment a guest can observe it --
// [method]pollable.block and the free poll() func both have nothing to
// wait for and return immediately -- and an input-stream's read performs a
// real, synchronously-blocking net.Conn.Read (so a genuine wait for data
// off the wire happens inside read itself, not a separate poll step).
// WASIConfig.Dialer's own doc calls this out as the intentional, spec-
// legal simplification for a single-threaded host with no real concurrent
// task scheduler to suspend/resume against.
//
// # Rep numbering
//
// Every rep this file mints (tcp-socket, socket-backed input/output-stream)
// comes from wasiSockets.allocRep, a single monotonic counter starting at
// wasiSockRepBase (1<<20) -- comfortably disjoint from wasi_fs.go's
// per-purpose counters (each starting at 1 or 3) and wasi.go's fixed
// wasiStdoutRep(1)/wasiStderrRep(2), so a socket-backed input-stream rep
// can never collide with an fs-backed or stdio one even though
// [method]input-stream.read/[method]output-stream.write dispatch across
// both spaces through one shared resource type (wasiInputStreamResType/
// wasiOutputStreamResType) -- see wasi_fs.go's streamRead and wasi.go's
// writeSink, both extended by this file's sockInStreamNode/outStreamNode
// fallback. network and pollable, by contrast, need no counter at all:
// both are modeled as a single stateless singleton rep (wasiNetworkRep,
// wasiPollableRep) that every mint (instance-network; every subscribe)
// hands out again -- see their own doc comments for why no per-instance
// state is needed.
const (
	wasiNetworkResType   uint32 = 8
	wasiTCPSocketResType uint32 = 9
	wasiPollableResType  uint32 = 10

	// UDP resource tags -- see this file's "wasi:sockets/udp" section below.
	wasiUDPSocketResType              uint32 = 11
	wasiIncomingDatagramStreamResType uint32 = 12
	wasiOutgoingDatagramStreamResType uint32 = 13

	// resolve-address-stream tag -- see this file's "wasi:sockets/
	// ip-name-lookup" section below.
	wasiResolveStreamResType uint32 = 14
)

// wasiNetworkRep is the one host-side rep wasi:sockets/instance-network.
// instance-network ever hands out (wrapped in a fresh own<network> handle
// each call -- see resource.go's handleTable, which lets many handles name
// the same rep). A real OS has exactly one "the network" a program dials
// out through; this package models that literally, needing no per-instance
// state (WASIConfig.Dialer is looked up once, off the wasiSockets the
// closures already close over -- a borrow<network> arg is never actually
// inspected beyond having resolved to a live handle at all, see
// startConnect's args[1]).
const wasiNetworkRep uint32 = 1

// wasiPollableRep is the one host-side rep every pollable this package
// mints -- from tcp-socket.subscribe, input-stream.subscribe, and
// output-stream.subscribe alike -- ever names. See this file's package doc
// ("Blocking, single-shot connect/read/write model") for why every
// pollable this package can mint is already ready the instant a guest can
// observe it: there is no wait state to distinguish between two different
// pollables, so (mirroring wasiNetworkRep) a single shared rep suffices.
const wasiPollableRep uint32 = 1

// wasiSockMaxReadChunk caps a single [method]input-stream.read's
// syscall-level buffer size, independent of the guest-requested `len`
// (which a real guest may pass as a very large bound, e.g.
// std::io::Read::read_to_end's growth strategy) -- mirrors a real OS's own
// read(2) never actually filling an arbitrarily large request in one
// syscall. 64KiB is comfortably larger than any single chunk this
// package's TCP fixtures ever exchange, while bounding a single guest call
// from forcing an unreasonably large host-side allocation.
const wasiSockMaxReadChunk = 64 * 1024

// wasi:sockets/network's error-code enum, in exact WIT declaration order
// (from `wasm-tools component wit` against real_tcp.component.wasm) --
// distinct from, and not to be confused with, wasi_fs.go's
// wasiErrorCode* (wasi:filesystem/types' own, differently-ordered
// error-code enum).
const (
	wasiSockErrUnknown uint32 = iota
	wasiSockErrAccessDenied
	wasiSockErrNotSupported
	wasiSockErrInvalidArgument
	wasiSockErrOutOfMemory
	wasiSockErrTimeout
	wasiSockErrConcurrencyConflict
	wasiSockErrNotInProgress
	wasiSockErrWouldBlock
	wasiSockErrInvalidState
	wasiSockErrNewSocketLimit
	wasiSockErrAddressNotBindable
	wasiSockErrAddressInUse
	wasiSockErrRemoteUnreachable
	wasiSockErrConnectionRefused
	wasiSockErrConnectionReset
	wasiSockErrConnectionAborted
	wasiSockErrDatagramTooLarge
	wasiSockErrNameUnresolvable
	wasiSockErrTemporaryResolverFailure
	wasiSockErrPermanentResolverFailure
)

// WASI 0.2 interface names for the sockets/poll surface this file
// registers -- see wasi.go's wasiIface* constants' doc for why these must
// match byte-for-byte.
const (
	wasiIfacePoll                = "wasi:io/poll@0.2.3"
	wasiIfaceSocketsInstanceNet  = "wasi:sockets/instance-network@0.2.3"
	wasiIfaceSocketsNetwork      = "wasi:sockets/network@0.2.3"
	wasiIfaceSocketsTCP          = "wasi:sockets/tcp@0.2.3"
	wasiIfaceSocketsTCPCreateSoc = "wasi:sockets/tcp-create-socket@0.2.3"
	wasiIfaceSocketsUDP          = "wasi:sockets/udp@0.2.3"
	wasiIfaceSocketsUDPCreateSoc = "wasi:sockets/udp-create-socket@0.2.3"
	wasiIfaceSocketsIPNameLookup = "wasi:sockets/ip-name-lookup@0.2.3"
)

// tcpSockNode is one live wasi:sockets/tcp `tcp-socket`: family records the
// ip-address-family create-tcp-socket was called with (never actually
// inspected beyond being stored -- this package's Dialer dispatches on the
// resolved address string's own family, not this field); conn/dialErr hold
// start-connect's outcome (see this file's package doc's "Blocking,
// single-shot" section: start-connect performs the real net.Dial
// synchronously, so both are already settled before start-connect
// returns); started/finished gate against a guest calling start-connect or
// finish-connect more than once against the same socket, mirroring a real
// implementation's own state-machine checks.
type tcpSockNode struct {
	mu       sync.Mutex
	family   uint32
	conn     net.Conn
	dialErr  error
	started  bool
	finished bool

	// Server-side (listen) fields, the reverse of conn/dialErr: start-bind
	// performs the real net.Listen synchronously (see this file's package
	// doc's "Blocking, single-shot" section -- the same shape connect uses),
	// recording listener/bindErr; finish-bind/start-listen/finish-listen only
	// ever report that already-settled outcome. A single tcp-socket is used
	// for EITHER connect OR bind+listen, never both, so these never coexist
	// with conn/dialErr on the same node. An accepted socket (accept's
	// own<tcp-socket> half) records its conn here too, for local/remote-address.
	listener net.Listener
	bindErr  error
}

// sockInStream is one live wasi:io/streams `input-stream` backed by a real
// net.Conn -- finish-connect's own<input-stream> half. Unlike
// wasi_fs.go's fsStreamNode (an in-memory byte slice with no real
// blocking), read genuinely blocks in net.Conn.Read until at least one
// byte is available or the peer closes the connection.
type sockInStream struct {
	mu   sync.Mutex
	conn net.Conn
}

// sockOutStream is one live wasi:io/streams `output-stream` backed by the
// same net.Conn a sockInStream reads from (finish-connect mints both
// halves over one net.Conn) -- finish-connect's own<output-stream> half.
type sockOutStream struct {
	mu   sync.Mutex
	conn net.Conn
}

// resolveAddrStream is one live wasi:sockets/ip-name-lookup
// `resolve-address-stream`: resolve-addresses does the real DNS lookup
// synchronously (mirroring the blocking/single-shot model TCP connect uses --
// see this file's package doc) and reports a lookup failure right there as the
// result's Err case, so a successfully-minted stream always holds resolved
// IPs; each resolve-next-address pops the next one, returning None once
// exhausted.
type resolveAddrStream struct {
	mu   sync.Mutex
	ips  []net.IP
	next int
}

// udpSockNode is one live wasi:sockets/udp `udp-socket`: family records the
// ip-address-family create-udp-socket was called with (mirrors tcpSockNode's
// family field -- see its doc, never inspected beyond being stored);
// pconn/bindErr hold start-bind's outcome (see this file's "wasi:sockets/udp"
// section doc for why the real net.ListenPacket happens synchronously inside
// start-bind, mirroring tcpSockNode's conn/dialErr); started/finished gate
// against calling start-bind/finish-bind more than once, exactly like
// tcpSockNode's own fields.
type udpSockNode struct {
	mu       sync.Mutex
	family   uint32
	pconn    net.PacketConn
	bindErr  error
	started  bool
	finished bool
}

// incomingDatagramStream is one live wasi:sockets/udp `incoming-datagram-
// stream` -- [method]udp-socket.stream's own<incoming-datagram-stream> half.
// pconn is the same net.PacketConn the owning udp-socket bound in start-bind
// (shared with the sibling outgoingDatagramStream minted by the same stream()
// call); remote, when non-nil, restricts receive to datagrams sent from that
// exact peer (the "connected" mode `stream(some(remote-address))` requests --
// see udp.wit's `stream` doc), discarding anything else, mirroring a real
// connected UDP socket's kernel-side filtering.
type incomingDatagramStream struct {
	mu     sync.Mutex
	pconn  net.PacketConn
	remote *net.UDPAddr
}

// outgoingDatagramStream is incomingDatagramStream's send half, sharing the
// same pconn. remote, when non-nil, is both the one address `send` accepts a
// per-datagram remote-address of (or omits it and gets this default) --
// mirrors incomingDatagramStream's own remote field, and udp.wit's own
// `stream` doc for the "connected" mode's send-side restriction.
type outgoingDatagramStream struct {
	mu     sync.Mutex
	pconn  net.PacketConn
	remote *net.UDPAddr
}

// wasiSockets holds the mutable state this file's host funcs close over:
// dial is the injected connector (WASIConfig.Dialer, defaulted to a real
// net.Dial in WithWASI -- see WASIConfig.Dialer's doc), resources is the
// owning Instance's handle table (set once via withResourcesHook, mirroring
// wasiFS.resources -- see its doc for why a nested own<T> result, e.g.
// finish-connect's tuple<own<input-stream>,own<output-stream>>, needs this
// directly), and the three rep tables + allocRep's shared counter mint and
// resolve every tcp-socket/socket-backed-stream rep this file hands out
// (see this file's package doc's "Rep numbering" section for why one
// shared, disjoint-based counter is safe across all three).
type wasiSockets struct {
	mu           sync.Mutex
	dial         func(network, address string) (net.Conn, error)
	listenPacket func(network, address string) (net.PacketConn, error)
	listen       func(network, address string) (net.Listener, error)
	resolveIP    func(ctx context.Context, name string) ([]net.IP, error)

	resources *handleTable

	nextRep       uint32
	tcpSocks      map[uint32]*tcpSockNode
	inStreams     map[uint32]*sockInStream
	outStreams    map[uint32]*sockOutStream
	resolveStream map[uint32]*resolveAddrStream

	// UDP rep tables -- see this file's "wasi:sockets/udp" section doc.
	udpSocks  map[uint32]*udpSockNode
	inDgrams  map[uint32]*incomingDatagramStream
	outDgrams map[uint32]*outgoingDatagramStream
}

// wasiSockRepBase is wasiSockets.nextRep's starting value -- see this
// file's package doc's "Rep numbering" section for why it must stay
// disjoint from wasi_fs.go's and wasi.go's own rep spaces.
const wasiSockRepBase uint32 = 1 << 20

// newWasiSockets returns a wasiSockets that dials through dial, binds UDP
// through listenPacket, and binds+listens TCP through listen (none ever nil
// by the time WithWASI constructs one -- see WASIConfig.Dialer/ListenPacket/
// Listen's own docs).
func newWasiSockets(dial func(network, address string) (net.Conn, error), listenPacket func(network, address string) (net.PacketConn, error), listen func(network, address string) (net.Listener, error), resolveIP func(ctx context.Context, name string) ([]net.IP, error)) *wasiSockets {
	return &wasiSockets{
		dial:          dial,
		listenPacket:  listenPacket,
		listen:        listen,
		resolveIP:     resolveIP,
		tcpSocks:      make(map[uint32]*tcpSockNode),
		inStreams:     make(map[uint32]*sockInStream),
		outStreams:    make(map[uint32]*sockOutStream),
		resolveStream: make(map[uint32]*resolveAddrStream),
		udpSocks:      make(map[uint32]*udpSockNode),
		inDgrams:      make(map[uint32]*incomingDatagramStream),
		outDgrams:     make(map[uint32]*outgoingDatagramStream),
		nextRep:       wasiSockRepBase,
	}
}

// setResources implements withResourcesHook's callback -- mirrors
// wasiFS.setResources's doc.
func (s *wasiSockets) setResources(t *handleTable) {
	s.mu.Lock()
	s.resources = t
	s.mu.Unlock()
}

// getResources returns the resources handleTable setResources recorded,
// failing loud if called before it ran -- mirrors wasiFS.getResources's doc.
func (s *wasiSockets) getResources() (*handleTable, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.resources == nil {
		return nil, fmt.Errorf("wasi:sockets: resources handle table not yet initialized (setResources not called)")
	}
	return s.resources, nil
}

// allocRep mints a fresh, package-wide-unique rep (see this file's package
// doc's "Rep numbering" section).
func (s *wasiSockets) allocRep() uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.nextRep
	s.nextRep++
	return r
}

func (s *wasiSockets) tcpSockNode(rep uint32) (*tcpSockNode, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.tcpSocks[rep]
	return n, ok
}

// inStreamNode resolves rep to a live socket-backed input-stream, reporting
// found=false (not an error) if rep does not name one -- mirrors
// wasi_fs.go's writeStreamNode doc: callers use this to fall through to the
// next candidate space rather than treat "not mine" as fatal.
func (s *wasiSockets) inStreamNode(rep uint32) (*sockInStream, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.inStreams[rep]
	return n, ok
}

// outStreamNode is inStreamNode's output-stream counterpart.
func (s *wasiSockets) outStreamNode(rep uint32) (*sockOutStream, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.outStreams[rep]
	return n, ok
}

// resolveStreamNode resolves rep to a live resolve-address-stream.
func (s *wasiSockets) resolveStreamNode(rep uint32) (*resolveAddrStream, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.resolveStream[rep]
	return n, ok
}

// udpSockNode resolves rep to a live udp-socket, mirroring tcpSockNode.
func (s *wasiSockets) udpSockNode(rep uint32) (*udpSockNode, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.udpSocks[rep]
	return n, ok
}

// inDatagramStreamNode resolves rep to a live incoming-datagram-stream,
// mirroring inStreamNode.
func (s *wasiSockets) inDatagramStreamNode(rep uint32) (*incomingDatagramStream, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.inDgrams[rep]
	return n, ok
}

// outDatagramStreamNode resolves rep to a live outgoing-datagram-stream,
// mirroring outStreamNode.
func (s *wasiSockets) outDatagramStreamNode(rep uint32) (*outgoingDatagramStream, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.outDgrams[rep]
	return n, ok
}

// read performs a real, blocking net.Conn.Read against s's connection, up
// to wasiSockMaxReadChunk bytes even if length requests more (see its own
// doc), and shapes the result as [method]input-stream.read's
// result<list<u8>,stream-error>: a non-empty read (even alongside a
// simultaneous io.EOF -- io.Reader's contract allows n>0 with err==io.EOF
// in the same call) is reported Ok this call, with EOF surfacing as
// stream-error::closed on the NEXT read once no more bytes remain; a
// zero-byte read alongside a non-nil error (a genuine failure, or EOF with
// nothing left) is reported as stream-error::closed -- this package has no
// last-operation-failed(error) resource to mint for a non-EOF failure (see
// wasi.go's wasiStreamErrorType doc: nothing in this package ever
// constructs that case), so any read error, not just EOF, is reported as
// "closed" rather than fabricating a bogus error-code payload -- a real
// guest observes "the stream ended" either way and does not distinguish
// further.
func (s *sockInStream) read(length uint64) ([]abi.Value, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if length > wasiSockMaxReadChunk {
		length = wasiSockMaxReadChunk
	}
	buf := make([]byte, length)
	n, err := s.conn.Read(buf)
	if n > 0 {
		return []abi.Value{abi.ResultValue{IsErr: false, Payload: wasiListFromBytes(buf[:n])}}, nil
	}
	if err != nil {
		return []abi.Value{abi.ResultValue{IsErr: true, Payload: abi.VariantValue{Disc: wasiStreamErrClosed}}}, nil
	}
	return []abi.Value{abi.ResultValue{IsErr: false, Payload: []abi.Value{}}}, nil
}

// write performs a real net.Conn.Write against s's connection. Per
// net.Conn's documented contract (Write returns a non-nil error whenever
// n < len(p)), one call either writes everything or fails -- exactly the
// "success or failure, no partial-write case to report" shape
// [method]output-stream.write's result<_,stream-error> expects, mirroring
// wasi.go's writeSink dispatch for stdio/fs writers.
func (s *sockOutStream) write(buf []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.conn.Write(buf)
	return err
}

// receive performs a real, blocking net.PacketConn.ReadFrom against s's
// socket -- exactly the same deliberate "block for real inside the operation
// that has data to wait for, rather than truly implementing async wait-then-
// retry" simplification this file's package doc documents for TCP's
// sockInStream.read (see its "Blocking, single-shot" section), now applied
// to UDP's own receive: udp.wit documents receive as required to never block
// and never report would-block, an async contract this single-threaded host
// has no task scheduler to honor faithfully -- a guest's compiled glue
// bridges that contract onto a real recv by looping receive+subscribe+
// [method]pollable.block (a no-op, see wasiPollableRep's doc) until data
// shows up; blocking for real here still produces exactly the sequence that
// loop expects to observe (immediately, once data is genuinely available),
// just without the CPU spin its outer loop would otherwise do while polling
// a host that could truly report "not yet" -- and a maxResults of 0 (a
// legitimate zero-length probe, per udp.wit's own doc) is honored literally
// as an immediate, non-blocking empty result, since there is nothing to wait
// for in that case at all.
//
// s.remote, when set (this socket's `stream` was called with
// some(remote-address) -- see udp.wit's own "connected" mode doc), discards
// any datagram not sent from that exact peer and keeps reading, mirroring a
// real connected UDP socket's kernel-side filtering; an unconnected stream
// (s.remote == nil) accepts from anyone, exactly like a real unconnected UDP
// socket's recvfrom.
func (s *incomingDatagramStream) receive(maxResults uint64) ([]abi.Value, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if maxResults == 0 {
		return []abi.Value{abi.ResultValue{IsErr: false, Payload: []abi.Value{}}}, nil
	}
	buf := make([]byte, wasiSockMaxReadChunk)
	for {
		n, raddr, err := s.pconn.ReadFrom(buf)
		if err != nil {
			return []abi.Value{abi.ResultValue{IsErr: true, Payload: wasiUDPErrToCode(err)}}, nil
		}
		udpAddr, ok := raddr.(*net.UDPAddr)
		if !ok {
			return nil, fmt.Errorf("[method]incoming-datagram-stream.receive: sender address: expected *net.UDPAddr, got %T", raddr)
		}
		if s.remote != nil && !(udpAddr.IP.Equal(s.remote.IP) && udpAddr.Port == s.remote.Port) {
			continue // not from the connected peer -- discard and keep waiting, see doc
		}
		remoteVal, err := wasiIPSocketAddrFromUDPAddr(udpAddr)
		if err != nil {
			return nil, fmt.Errorf("[method]incoming-datagram-stream.receive: %w", err)
		}
		datagram := []abi.Value{wasiListFromBytes(append([]byte(nil), buf[:n]...)), remoteVal}
		return []abi.Value{abi.ResultValue{IsErr: false, Payload: []abi.Value{datagram}}}, nil
	}
}

// send performs a real net.PacketConn.WriteTo per datagram in datagrams (the
// lifted list<outgoing-datagram>, each a 2-field record: data list<u8>,
// remote-address option<ip-socket-address> -- see abi.Value's doc: record ->
// []Value, option -> nil or the inner value directly), resolving each
// datagram's own remote-address when present, or falling back to s.remote
// (the "connected" mode default -- see udp.wit's `send` doc) when absent.
// Per send's own documented contract, sending stops at the first failure but
// only reports an error if NOTHING was sent yet (sent == 0); otherwise the
// count of datagrams actually written is reported Ok, matching "this
// function never returns an error [if] at least one datagram has been sent
// successfully".
func (s *outgoingDatagramStream) send(datagrams []abi.Value) ([]abi.Value, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var sent uint64
	var lastErr error
sendLoop:
	for i, dv := range datagrams {
		rec, ok := dv.([]abi.Value)
		if !ok || len(rec) != 2 {
			return nil, fmt.Errorf("[method]outgoing-datagram-stream.send: datagrams[%d]: expected a 2-field record, got %#v", i, dv)
		}
		data, err := wasiBytesFromList(rec[0])
		if err != nil {
			return nil, fmt.Errorf("[method]outgoing-datagram-stream.send: datagrams[%d].data: %w", i, err)
		}
		addr := s.remote
		if rec[1] != nil {
			addrStr, err := wasiIPSocketAddrToString(rec[1])
			if err != nil {
				return nil, fmt.Errorf("[method]outgoing-datagram-stream.send: datagrams[%d].remote-address: %w", i, err)
			}
			addr, err = net.ResolveUDPAddr("udp", addrStr)
			if err != nil {
				return nil, fmt.Errorf("[method]outgoing-datagram-stream.send: datagrams[%d].remote-address: resolve %q: %w", i, addrStr, err)
			}
		}
		if addr == nil {
			lastErr = fmt.Errorf("no remote-address given and stream has no default (unconnected)")
			break sendLoop
		}
		if _, err := s.pconn.WriteTo(data, addr); err != nil {
			lastErr = err
			break sendLoop
		}
		sent++
	}
	if sent == 0 && lastErr != nil {
		return []abi.Value{abi.ResultValue{IsErr: true, Payload: wasiUDPErrToCode(lastErr)}}, nil
	}
	return []abi.Value{abi.ResultValue{IsErr: false, Payload: sent}}, nil
}

// wasiTCPDialErrToCode maps a net.Dial error to the closest wasi:sockets
// error-code case. Only the two distinctions this package's fixtures can
// actually produce/observe are discriminated (a real OS-level connection
// refusal, and a dial timeout); anything else -- DNS failures cannot occur
// here since the guest is always given a literal "ip:port" (see this
// file's package doc), and no other failure mode is exercised -- falls
// back to wasiSockErrUnknown rather than guessing a more specific case
// that might not actually apply.
func wasiTCPDialErrToCode(err error) uint32 {
	if errors.Is(err, syscall.ECONNREFUSED) {
		return wasiSockErrConnectionRefused
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return wasiSockErrTimeout
	}
	return wasiSockErrUnknown
}

// wasiIPSocketAddrToString converts a lifted wasi:sockets/network
// `ip-socket-address` variant value (see abi.Value's doc: variant ->
// abi.VariantValue, record/tuple -> []abi.Value, u8/u16/u32 -> uint32) into
// the "host:port" string net.Dial expects. remote-address is never a
// top-level own<T>/borrow<T> (it's a plain variant), so -- unlike self/
// network -- host_import.go's generic per-arg resolution never touches it;
// this function is this package's only consumer of its raw lifted shape.
func wasiIPSocketAddrToString(v abi.Value) (string, error) {
	vv, ok := v.(abi.VariantValue)
	if !ok {
		return "", fmt.Errorf("expected an ip-socket-address variant (abi.VariantValue), got %T", v)
	}
	switch vv.Disc {
	case 0: // ipv4(ipv4-socket-address)
		rec, ok := vv.Payload.([]abi.Value)
		if !ok || len(rec) != 2 {
			return "", fmt.Errorf("ipv4-socket-address: expected a 2-field record, got %#v", vv.Payload)
		}
		port, ok := rec[0].(uint32)
		if !ok {
			return "", fmt.Errorf("ipv4-socket-address.port: expected uint32, got %T", rec[0])
		}
		addr, ok := rec[1].([]abi.Value)
		if !ok || len(addr) != 4 {
			return "", fmt.Errorf("ipv4-socket-address.address: expected a 4-tuple, got %#v", rec[1])
		}
		octets := make([]byte, 4)
		for i, o := range addr {
			b, ok := o.(uint32)
			if !ok {
				return "", fmt.Errorf("ipv4-socket-address.address[%d]: expected uint32, got %T", i, o)
			}
			octets[i] = byte(b)
		}
		return fmt.Sprintf("%d.%d.%d.%d:%d", octets[0], octets[1], octets[2], octets[3], port), nil

	case 1: // ipv6(ipv6-socket-address)
		rec, ok := vv.Payload.([]abi.Value)
		if !ok || len(rec) != 4 {
			return "", fmt.Errorf("ipv6-socket-address: expected a 4-field record, got %#v", vv.Payload)
		}
		port, ok := rec[0].(uint32)
		if !ok {
			return "", fmt.Errorf("ipv6-socket-address.port: expected uint32, got %T", rec[0])
		}
		addr, ok := rec[2].([]abi.Value)
		if !ok || len(addr) != 8 {
			return "", fmt.Errorf("ipv6-socket-address.address: expected an 8-tuple, got %#v", rec[2])
		}
		parts := make([]string, 8)
		for i, p := range addr {
			u, ok := p.(uint32)
			if !ok {
				return "", fmt.Errorf("ipv6-socket-address.address[%d]: expected uint32, got %T", i, p)
			}
			parts[i] = fmt.Sprintf("%x", u)
		}
		return fmt.Sprintf("[%s]:%d", strings.Join(parts, ":"), port), nil

	default:
		return "", fmt.Errorf("ip-socket-address: unknown variant case %d", vv.Disc)
	}
}

// wasiIPSocketAddrFromUDPAddr converts a real *net.UDPAddr (a datagram's
// sender, from net.PacketConn.ReadFrom) into the wasi:sockets/network
// `ip-socket-address` variant shape (see abi.Value's doc), the reverse
// direction of wasiIPSocketAddrToString -- needed because, unlike TCP (which
// only ever consumes an ip-socket-address the guest supplies, for
// start-connect), UDP's [method]incoming-datagram-stream.receive must
// PRODUCE one: every received datagram's own sender address. IPv6's
// scope-id field is always reported 0: net.UDPAddr's own scope is a string
// zone name (an interface name, e.g. "eth0"), not the numeric interface
// index ip-socket-address's scope-id field expects, and no fixture this
// package runs ever exercises a scoped (link-local) IPv6 address to make
// that translation worth building.
func wasiIPSocketAddrFromUDPAddr(addr *net.UDPAddr) (abi.Value, error) {
	return wasiIPSocketAddrFromIPPort(addr.IP, addr.Port)
}

// wasiIPSocketAddrFromIPPort is wasiIPSocketAddrFromUDPAddr's transport-agnostic
// core, shared with TCP's [method]tcp-socket.local-address (which reads a real
// net.Listener.Addr / net.Conn.LocalAddr *net.TCPAddr) -- see
// wasiIPSocketAddrFromUDPAddr's doc for the flow-info/scope-id=0 rationale,
// which applies identically here.
func wasiIPSocketAddrFromIPPort(ip net.IP, port int) (abi.Value, error) {
	if ip4 := ip.To4(); ip4 != nil {
		rec := []abi.Value{
			uint32(port),
			[]abi.Value{uint32(ip4[0]), uint32(ip4[1]), uint32(ip4[2]), uint32(ip4[3])},
		}
		return abi.VariantValue{Disc: 0, Payload: rec}, nil
	}
	ip16 := ip.To16()
	if ip16 == nil {
		return nil, fmt.Errorf("wasiIPSocketAddrFromIPPort: %v is neither a valid IPv4 nor IPv6 address", ip)
	}
	parts := make([]abi.Value, 8)
	for i := 0; i < 8; i++ {
		parts[i] = uint32(uint16(ip16[i*2])<<8 | uint16(ip16[i*2+1]))
	}
	rec := []abi.Value{
		uint32(port),
		uint32(0), // flow-info: not modeled by net.Addr
		parts,
		uint32(0), // scope-id: see wasiIPSocketAddrFromUDPAddr's doc
	}
	return abi.VariantValue{Disc: 1, Payload: rec}, nil
}

// wasiIPAddressValue builds the wasi:sockets/network `ip-address` variant value
// (ipv4(tuple<u8,u8,u8,u8>) | ipv6(tuple<u16 x8>)) from a net.IP -- the
// address-only shape resolve-next-address returns (no port, unlike
// ip-socket-address).
func wasiIPAddressValue(ip net.IP) (abi.Value, error) {
	if ip4 := ip.To4(); ip4 != nil {
		return abi.VariantValue{Disc: 0, Payload: []abi.Value{
			uint32(ip4[0]), uint32(ip4[1]), uint32(ip4[2]), uint32(ip4[3]),
		}}, nil
	}
	ip16 := ip.To16()
	if ip16 == nil {
		return nil, fmt.Errorf("wasiIPAddressValue: %v is neither a valid IPv4 nor IPv6 address", ip)
	}
	parts := make([]abi.Value, 8)
	for i := 0; i < 8; i++ {
		parts[i] = uint32(uint16(ip16[i*2])<<8 | uint16(ip16[i*2+1]))
	}
	return abi.VariantValue{Disc: 1, Payload: parts}, nil
}

// wasiIPAddressType interns wasi:sockets/network's `variant ip-address {
// ipv4(ipv4-address), ipv6(ipv6-address) }` into tbl and returns its TypeRef.
func wasiIPAddressType(tbl *typeTable) binary.TypeRef {
	v4 := wasiIPv4AddressType(tbl)
	v6 := wasiIPv6AddressType(tbl)
	return tbl.add(binary.VariantDesc{Cases: []binary.VariantCase{
		{Name: "ipv4", Type: &v4},
		{Name: "ipv6", Type: &v6},
	}})
}

// wasiUDPErrToCode maps a real UDP bind/send/receive error to the closest
// wasi:sockets error-code case -- mirrors wasiTCPDialErrToCode's doc for why
// only the distinctions this package's own fixtures can actually produce are
// discriminated, with anything else falling back to wasiSockErrUnknown.
func wasiUDPErrToCode(err error) uint32 {
	if errors.Is(err, syscall.EADDRINUSE) {
		return wasiSockErrAddressInUse
	}
	if errors.Is(err, syscall.EADDRNOTAVAIL) {
		return wasiSockErrAddressNotBindable
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return wasiSockErrTimeout
	}
	return wasiSockErrUnknown
}

// tcpListenerNode parses a single-self-arg method's args (self:
// borrow<tcp-socket>) and resolves the live node, sharing the boilerplate
// across start-listen/finish-listen/accept. method names the caller for error
// messages (e.g. "accept").
func tcpListenerNode(sockets *wasiSockets, method string, args []abi.Value) (*tcpSockNode, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("[method]tcp-socket.%s: expected 1 arg (self), got %d", method, len(args))
	}
	selfRep, ok := args[0].(uint32)
	if !ok {
		return nil, fmt.Errorf("[method]tcp-socket.%s: self: expected uint32 rep, got %T", method, args[0])
	}
	node, ok := sockets.tcpSockNode(selfRep)
	if !ok {
		return nil, fmt.Errorf("[method]tcp-socket.%s: tcp-socket rep %d does not name a live socket", method, selfRep)
	}
	return node, nil
}

// wasiTCPListenErrToCode maps a real net.Listen/Accept error to the closest
// wasi:sockets error-code. The bind/accept failure modes are the same ones
// wasiUDPErrToCode already discriminates (address-in-use / not-bindable /
// timeout, else unknown), so it reuses that mapper -- see its doc.
func wasiTCPListenErrToCode(err error) uint32 {
	return wasiUDPErrToCode(err)
}

// wasiSocketOptions returns the Options implementing wasi:sockets/
// instance-network, wasi:sockets/tcp-create-socket, wasi:sockets/tcp, and
// wasi:io/poll (plus wasi:io/streams' two subscribe methods) -- see this
// file's package doc for the exact discovered call list. Only called by
// WithWASI when the caller opts in (WASIConfig.AllowTCP) -- see
// WASIConfig.AllowTCP/Dialer's doc in wasi.go.
func wasiSocketOptions(sockets *wasiSockets) []Option {
	instanceNetwork := func(context.Context, []abi.Value) ([]abi.Value, error) {
		// Top-level own<network> result: allocHandleResult (host_import.go)
		// auto-wraps this bare rep into a real guest handle, mirroring
		// wasi.go's getStdout/getStderr.
		return []abi.Value{wasiNetworkRep}, nil
	}

	createTCPSocket := func(_ context.Context, args []abi.Value) ([]abi.Value, error) {
		if len(args) != 1 {
			return nil, fmt.Errorf("wasi:sockets/tcp-create-socket.create-tcp-socket: expected 1 arg (address-family), got %d", len(args))
		}
		fam, ok := args[0].(uint32)
		if !ok {
			return nil, fmt.Errorf("wasi:sockets/tcp-create-socket.create-tcp-socket: address-family: expected uint32, got %T", args[0])
		}
		resources, err := sockets.getResources()
		if err != nil {
			return nil, err
		}
		rep := sockets.allocRep()
		sockets.mu.Lock()
		sockets.tcpSocks[rep] = &tcpSockNode{family: fam}
		sockets.mu.Unlock()
		handle := resources.NewOwn(wasiTCPSocketResType, rep)
		return []abi.Value{abi.ResultValue{IsErr: false, Payload: handle}}, nil
	}

	startConnect := func(_ context.Context, args []abi.Value) ([]abi.Value, error) {
		if len(args) != 3 {
			return nil, fmt.Errorf("[method]tcp-socket.start-connect: expected 3 args (self, network, remote-address), got %d", len(args))
		}
		selfRep, ok := args[0].(uint32)
		if !ok {
			return nil, fmt.Errorf("[method]tcp-socket.start-connect: self: expected uint32 rep, got %T", args[0])
		}
		// args[1] (network, borrow<network>) is already resolved to a rep by
		// liftHostArgs/resolveHandleArg and validated to name a live handle;
		// this package has exactly one network (wasiNetworkRep), so there is
		// nothing further to inspect -- see wasiNetworkRep's doc.
		addr, err := wasiIPSocketAddrToString(args[2])
		if err != nil {
			return nil, fmt.Errorf("[method]tcp-socket.start-connect: remote-address: %w", err)
		}
		node, ok := sockets.tcpSockNode(selfRep)
		if !ok {
			return nil, fmt.Errorf("[method]tcp-socket.start-connect: tcp-socket rep %d does not name a live socket", selfRep)
		}
		node.mu.Lock()
		defer node.mu.Unlock()
		if node.started {
			return []abi.Value{abi.ResultValue{IsErr: true, Payload: wasiSockErrConcurrencyConflict}}, nil
		}
		node.started = true
		// See this file's package doc's "Blocking, single-shot" section:
		// the real, blocking connect happens right here, synchronously --
		// finish-connect (below) only ever reports this already-settled
		// outcome.
		node.conn, node.dialErr = sockets.dial("tcp", addr)
		return []abi.Value{abi.ResultValue{IsErr: false, Payload: nil}}, nil
	}

	finishConnect := func(_ context.Context, args []abi.Value) ([]abi.Value, error) {
		if len(args) != 1 {
			return nil, fmt.Errorf("[method]tcp-socket.finish-connect: expected 1 arg (self), got %d", len(args))
		}
		selfRep, ok := args[0].(uint32)
		if !ok {
			return nil, fmt.Errorf("[method]tcp-socket.finish-connect: self: expected uint32 rep, got %T", args[0])
		}
		node, ok := sockets.tcpSockNode(selfRep)
		if !ok {
			return nil, fmt.Errorf("[method]tcp-socket.finish-connect: tcp-socket rep %d does not name a live socket", selfRep)
		}
		node.mu.Lock()
		defer node.mu.Unlock()
		if !node.started {
			return []abi.Value{abi.ResultValue{IsErr: true, Payload: wasiSockErrNotInProgress}}, nil
		}
		if node.finished {
			return []abi.Value{abi.ResultValue{IsErr: true, Payload: wasiSockErrNotInProgress}}, nil
		}
		node.finished = true
		if node.dialErr != nil {
			return []abi.Value{abi.ResultValue{IsErr: true, Payload: wasiTCPDialErrToCode(node.dialErr)}}, nil
		}
		resources, err := sockets.getResources()
		if err != nil {
			return nil, err
		}
		inRep := sockets.allocRep()
		outRep := sockets.allocRep()
		sockets.mu.Lock()
		sockets.inStreams[inRep] = &sockInStream{conn: node.conn}
		sockets.outStreams[outRep] = &sockOutStream{conn: node.conn}
		sockets.mu.Unlock()
		// tuple<own<input-stream>,own<output-stream>> nests both own<T>
		// handles inside the Ok payload of a result<> -- host_import.go's
		// generic allocHandleResult only auto-converts a TOP-LEVEL own/
		// borrow result (see its doc), so both handles are minted directly
		// here, mirroring wasi_fs.go's openAt/getDirectories.
		inHandle := resources.NewOwn(wasiInputStreamResType, inRep)
		outHandle := resources.NewOwn(wasiOutputStreamResType, outRep)
		return []abi.Value{abi.ResultValue{IsErr: false, Payload: []abi.Value{inHandle, outHandle}}}, nil
	}

	tcpSubscribe := func(_ context.Context, args []abi.Value) ([]abi.Value, error) {
		if len(args) != 1 {
			return nil, fmt.Errorf("[method]tcp-socket.subscribe: expected 1 arg (self), got %d", len(args))
		}
		selfRep, ok := args[0].(uint32)
		if !ok {
			return nil, fmt.Errorf("[method]tcp-socket.subscribe: self: expected uint32 rep, got %T", args[0])
		}
		if _, ok := sockets.tcpSockNode(selfRep); !ok {
			return nil, fmt.Errorf("[method]tcp-socket.subscribe: tcp-socket rep %d does not name a live socket", selfRep)
		}
		return []abi.Value{wasiPollableRep}, nil
	}

	// startBind performs the real net.Listen synchronously (see startConnect's
	// mirror-image comment and this file's "Blocking, single-shot" doc):
	// wasi:sockets's bind/listen split is async, but this host settles the
	// whole listen right here, so finish-bind/start-listen/finish-listen below
	// are pure already-settled reporters. args: self (borrow<tcp-socket>),
	// network (borrow<network>, validated live but not otherwise inspected --
	// see startConnect), local-address (ip-socket-address).
	startBind := func(_ context.Context, args []abi.Value) ([]abi.Value, error) {
		if len(args) != 3 {
			return nil, fmt.Errorf("[method]tcp-socket.start-bind: expected 3 args (self, network, local-address), got %d", len(args))
		}
		selfRep, ok := args[0].(uint32)
		if !ok {
			return nil, fmt.Errorf("[method]tcp-socket.start-bind: self: expected uint32 rep, got %T", args[0])
		}
		addr, err := wasiIPSocketAddrToString(args[2])
		if err != nil {
			return nil, fmt.Errorf("[method]tcp-socket.start-bind: local-address: %w", err)
		}
		node, ok := sockets.tcpSockNode(selfRep)
		if !ok {
			return nil, fmt.Errorf("[method]tcp-socket.start-bind: tcp-socket rep %d does not name a live socket", selfRep)
		}
		node.mu.Lock()
		defer node.mu.Unlock()
		if node.started || node.listener != nil {
			return []abi.Value{abi.ResultValue{IsErr: true, Payload: wasiSockErrConcurrencyConflict}}, nil
		}
		node.started = true
		node.listener, node.bindErr = sockets.listen("tcp", addr)
		return []abi.Value{abi.ResultValue{IsErr: false, Payload: nil}}, nil
	}

	finishBind := func(_ context.Context, args []abi.Value) ([]abi.Value, error) {
		if len(args) != 1 {
			return nil, fmt.Errorf("[method]tcp-socket.finish-bind: expected 1 arg (self), got %d", len(args))
		}
		selfRep, ok := args[0].(uint32)
		if !ok {
			return nil, fmt.Errorf("[method]tcp-socket.finish-bind: self: expected uint32 rep, got %T", args[0])
		}
		node, ok := sockets.tcpSockNode(selfRep)
		if !ok {
			return nil, fmt.Errorf("[method]tcp-socket.finish-bind: tcp-socket rep %d does not name a live socket", selfRep)
		}
		node.mu.Lock()
		defer node.mu.Unlock()
		if !node.started {
			return []abi.Value{abi.ResultValue{IsErr: true, Payload: wasiSockErrNotInProgress}}, nil
		}
		if node.bindErr != nil {
			return []abi.Value{abi.ResultValue{IsErr: true, Payload: wasiTCPListenErrToCode(node.bindErr)}}, nil
		}
		return []abi.Value{abi.ResultValue{IsErr: false, Payload: nil}}, nil
	}

	// startListen/finishListen are already-settled reporters: net.Listen (in
	// start-bind) already put the socket in the listening state, so both just
	// confirm the bind succeeded. A socket that never bound (no listener,
	// no bindErr) is in the wrong state for listen.
	startListen := func(_ context.Context, args []abi.Value) ([]abi.Value, error) {
		node, err := tcpListenerNode(sockets, "start-listen", args)
		if err != nil {
			return nil, err
		}
		node.mu.Lock()
		defer node.mu.Unlock()
		if node.listener == nil {
			return []abi.Value{abi.ResultValue{IsErr: true, Payload: wasiSockErrInvalidState}}, nil
		}
		return []abi.Value{abi.ResultValue{IsErr: false, Payload: nil}}, nil
	}

	finishListen := func(_ context.Context, args []abi.Value) ([]abi.Value, error) {
		node, err := tcpListenerNode(sockets, "finish-listen", args)
		if err != nil {
			return nil, err
		}
		node.mu.Lock()
		defer node.mu.Unlock()
		if node.listener == nil {
			return []abi.Value{abi.ResultValue{IsErr: true, Payload: wasiSockErrInvalidState}}, nil
		}
		return []abi.Value{abi.ResultValue{IsErr: false, Payload: nil}}, nil
	}

	// accept blocks for real in net.Listener.Accept (the same deliberate
	// synchronous block sockInStream.read/incoming-datagram-stream.receive use
	// -- see this file's "Blocking, single-shot" doc). It mints the tuple<
	// own<tcp-socket>, own<input-stream>, own<output-stream>>: the accepted
	// tcp-socket wraps the accepted net.Conn (for local/remote-address), and
	// the two streams read/write that same conn, exactly as finish-connect
	// mints its own stream pair over one conn.
	accept := func(_ context.Context, args []abi.Value) ([]abi.Value, error) {
		node, err := tcpListenerNode(sockets, "accept", args)
		if err != nil {
			return nil, err
		}
		node.mu.Lock()
		ln := node.listener
		node.mu.Unlock()
		if ln == nil {
			return []abi.Value{abi.ResultValue{IsErr: true, Payload: wasiSockErrInvalidState}}, nil
		}
		conn, aerr := ln.Accept()
		if aerr != nil {
			return []abi.Value{abi.ResultValue{IsErr: true, Payload: wasiTCPListenErrToCode(aerr)}}, nil
		}
		resources, err := sockets.getResources()
		if err != nil {
			return nil, err
		}
		sockRep := sockets.allocRep()
		inRep := sockets.allocRep()
		outRep := sockets.allocRep()
		sockets.mu.Lock()
		sockets.tcpSocks[sockRep] = &tcpSockNode{conn: conn}
		sockets.inStreams[inRep] = &sockInStream{conn: conn}
		sockets.outStreams[outRep] = &sockOutStream{conn: conn}
		sockets.mu.Unlock()
		// tuple<own,own,own> nested in the Ok payload -- minted directly here,
		// like finish-connect (allocHandleResult only auto-wraps a top-level
		// own/borrow result).
		sockHandle := resources.NewOwn(wasiTCPSocketResType, sockRep)
		inHandle := resources.NewOwn(wasiInputStreamResType, inRep)
		outHandle := resources.NewOwn(wasiOutputStreamResType, outRep)
		return []abi.Value{abi.ResultValue{IsErr: false, Payload: []abi.Value{sockHandle, inHandle, outHandle}}}, nil
	}

	// localAddress reports the bound address of a listening socket (its
	// net.Listener.Addr, e.g. the ephemeral port a bind-to-:0 guest asks for
	// back) or, for an accepted socket, its net.Conn.LocalAddr.
	localAddress := func(_ context.Context, args []abi.Value) ([]abi.Value, error) {
		if len(args) != 1 {
			return nil, fmt.Errorf("[method]tcp-socket.local-address: expected 1 arg (self), got %d", len(args))
		}
		selfRep, ok := args[0].(uint32)
		if !ok {
			return nil, fmt.Errorf("[method]tcp-socket.local-address: self: expected uint32 rep, got %T", args[0])
		}
		node, ok := sockets.tcpSockNode(selfRep)
		if !ok {
			return nil, fmt.Errorf("[method]tcp-socket.local-address: tcp-socket rep %d does not name a live socket", selfRep)
		}
		node.mu.Lock()
		var netAddr net.Addr
		switch {
		case node.listener != nil:
			netAddr = node.listener.Addr()
		case node.conn != nil:
			netAddr = node.conn.LocalAddr()
		}
		node.mu.Unlock()
		tcpAddr, ok := netAddr.(*net.TCPAddr)
		if !ok {
			return []abi.Value{abi.ResultValue{IsErr: true, Payload: wasiSockErrInvalidState}}, nil
		}
		v, err := wasiIPSocketAddrFromIPPort(tcpAddr.IP, tcpAddr.Port)
		if err != nil {
			return nil, fmt.Errorf("[method]tcp-socket.local-address: %w", err)
		}
		return []abi.Value{abi.ResultValue{IsErr: false, Payload: v}}, nil
	}

	// remoteAddress reports an accepted socket's peer (net.Conn.RemoteAddr) --
	// the address a listening guest's accept() returns alongside the socket
	// (rust prints it as "accepted from ..."). A listener socket has no peer,
	// so it reports invalid-state.
	remoteAddress := func(_ context.Context, args []abi.Value) ([]abi.Value, error) {
		if len(args) != 1 {
			return nil, fmt.Errorf("[method]tcp-socket.remote-address: expected 1 arg (self), got %d", len(args))
		}
		selfRep, ok := args[0].(uint32)
		if !ok {
			return nil, fmt.Errorf("[method]tcp-socket.remote-address: self: expected uint32 rep, got %T", args[0])
		}
		node, ok := sockets.tcpSockNode(selfRep)
		if !ok {
			return nil, fmt.Errorf("[method]tcp-socket.remote-address: tcp-socket rep %d does not name a live socket", selfRep)
		}
		node.mu.Lock()
		conn := node.conn
		node.mu.Unlock()
		if conn == nil {
			return []abi.Value{abi.ResultValue{IsErr: true, Payload: wasiSockErrInvalidState}}, nil
		}
		tcpAddr, ok := conn.RemoteAddr().(*net.TCPAddr)
		if !ok {
			return []abi.Value{abi.ResultValue{IsErr: true, Payload: wasiSockErrInvalidState}}, nil
		}
		v, err := wasiIPSocketAddrFromIPPort(tcpAddr.IP, tcpAddr.Port)
		if err != nil {
			return nil, fmt.Errorf("[method]tcp-socket.remote-address: %w", err)
		}
		return []abi.Value{abi.ResultValue{IsErr: false, Payload: v}}, nil
	}

	// setListenBacklogSize is a no-op that reports Ok: net.Listen uses the
	// OS default backlog and this host exposes no knob to change it, but a
	// guest (rust std sets a default backlog during bind) only needs the call
	// to succeed. ponytail: no-op; wire net.ListenConfig if a guest ever
	// depends on the backlog value.
	setListenBacklogSize := func(context.Context, []abi.Value) ([]abi.Value, error) {
		return []abi.Value{abi.ResultValue{IsErr: false, Payload: nil}}, nil
	}

	// resolveAddresses implements wasi:sockets/ip-name-lookup.resolve-addresses(
	// network: borrow<network>, name: string) -> result<resolve-address-stream,
	// error-code>. The real DNS lookup happens synchronously here (see
	// resolveAddrStream's doc); a failure is reported as the Err case. args[0]
	// (network) is validated live upstream, not inspected (one network -- see
	// wasiNetworkRep).
	resolveAddresses := func(ctx context.Context, args []abi.Value) ([]abi.Value, error) {
		if len(args) != 2 {
			return nil, fmt.Errorf("wasi:sockets/ip-name-lookup.resolve-addresses: expected 2 args (network, name), got %d", len(args))
		}
		name, ok := args[1].(string)
		if !ok {
			return nil, fmt.Errorf("wasi:sockets/ip-name-lookup.resolve-addresses: name: expected string, got %T", args[1])
		}
		resources, err := sockets.getResources()
		if err != nil {
			return nil, err
		}
		ips, lookupErr := sockets.resolveIP(ctx, name)
		if lookupErr != nil {
			return []abi.Value{abi.ResultValue{IsErr: true, Payload: wasiSockErrNameUnresolvable}}, nil
		}
		rep := sockets.allocRep()
		sockets.mu.Lock()
		sockets.resolveStream[rep] = &resolveAddrStream{ips: ips}
		sockets.mu.Unlock()
		handle := resources.NewOwn(wasiResolveStreamResType, rep)
		return []abi.Value{abi.ResultValue{IsErr: false, Payload: handle}}, nil
	}

	// resolveNextAddress implements [method]resolve-address-stream.
	// resolve-next-address(self) -> result<option<ip-address>, error-code>:
	// pops the next resolved IP as some(ip-address), or none once exhausted.
	resolveNextAddress := func(_ context.Context, args []abi.Value) ([]abi.Value, error) {
		if len(args) != 1 {
			return nil, fmt.Errorf("[method]resolve-address-stream.resolve-next-address: expected 1 arg (self), got %d", len(args))
		}
		selfRep, ok := args[0].(uint32)
		if !ok {
			return nil, fmt.Errorf("[method]resolve-address-stream.resolve-next-address: self: expected uint32 rep, got %T", args[0])
		}
		node, ok := sockets.resolveStreamNode(selfRep)
		if !ok {
			return nil, fmt.Errorf("[method]resolve-address-stream.resolve-next-address: rep %d does not name a live resolve-address-stream", selfRep)
		}
		node.mu.Lock()
		defer node.mu.Unlock()
		if node.next >= len(node.ips) {
			// option none: abi models option -> nil payload inside the Ok.
			return []abi.Value{abi.ResultValue{IsErr: false, Payload: nil}}, nil
		}
		ip := node.ips[node.next]
		node.next++
		addr, err := wasiIPAddressValue(ip)
		if err != nil {
			return nil, fmt.Errorf("[method]resolve-address-stream.resolve-next-address: %w", err)
		}
		// option some(ip-address): the inner value directly (see abi.Value doc).
		return []abi.Value{abi.ResultValue{IsErr: false, Payload: addr}}, nil
	}

	resolveSubscribe := func(_ context.Context, args []abi.Value) ([]abi.Value, error) {
		if len(args) != 1 {
			return nil, fmt.Errorf("[method]resolve-address-stream.subscribe: expected 1 arg (self), got %d", len(args))
		}
		selfRep, ok := args[0].(uint32)
		if !ok {
			return nil, fmt.Errorf("[method]resolve-address-stream.subscribe: self: expected uint32 rep, got %T", args[0])
		}
		if _, ok := sockets.resolveStreamNode(selfRep); !ok {
			return nil, fmt.Errorf("[method]resolve-address-stream.subscribe: rep %d does not name a live resolve-address-stream", selfRep)
		}
		return []abi.Value{wasiPollableRep}, nil
	}

	// streamSubscribe backs both [method]input-stream.subscribe and
	// [method]output-stream.subscribe: self is already resolved (and
	// validated live) by liftHostArgs before this closure runs, and every
	// pollable this package mints is the same always-ready singleton (see
	// wasiPollableRep's doc), so there is nothing left to distinguish
	// between the two methods' bodies.
	streamSubscribe := func(_ context.Context, args []abi.Value) ([]abi.Value, error) {
		if len(args) != 1 {
			return nil, fmt.Errorf("[method]stream.subscribe: expected 1 arg (self), got %d", len(args))
		}
		return []abi.Value{wasiPollableRep}, nil
	}

	instNetFD, instNetResolve := wasiInstanceNetworkSig()
	createTCPFD, createTCPResolve := wasiCreateTCPSocketSig()
	startConnectFD, startConnectResolve := wasiTCPStartConnectSig()
	finishConnectFD, finishConnectResolve := wasiTCPFinishConnectSig()
	startBindFD, startBindResolve := wasiTCPStartBindSig()
	finishBindFD, finishBindResolve := wasiTCPSelfResultSig()
	startListenFD, startListenResolve := wasiTCPSelfResultSig()
	finishListenFD, finishListenResolve := wasiTCPSelfResultSig()
	acceptFD, acceptResolve := wasiTCPAcceptSig()
	localAddrFD, localAddrResolve := wasiTCPLocalAddressSig()
	remoteAddrFD, remoteAddrResolve := wasiTCPLocalAddressSig()
	setBacklogFD, setBacklogResolve := wasiTCPSetListenBacklogSig()
	tcpSubFD, tcpSubResolve := wasiSubscribeSig(wasiTCPSocketResType)
	inSubFD, inSubResolve := wasiSubscribeSig(wasiInputStreamResType)
	outSubFD, outSubResolve := wasiSubscribeSig(wasiOutputStreamResType)
	resolveFD, resolveResolve := wasiResolveAddressesSig()
	resolveNextFD, resolveNextResolve := wasiResolveNextAddressSig()
	resolveSubFD, resolveSubResolve := wasiSubscribeSig(wasiResolveStreamResType)

	// tcpSetOpt builds the Option for one no-op tcp-socket option setter (see
	// wasiSocketSetOptOK).
	tcpSetOpt := func(name, prim string) Option {
		fd, r := wasiSocketSetOptSig(wasiTCPSocketResType, prim)
		return withImportCustom(wasiIfaceSocketsTCP, name, wasiSocketSetOptOK, fd, r)
	}

	return []Option{
		withResourcesHook(sockets.setResources),

		// See withResourceTag's doc (host_import.go): without these, a
		// guest that drops an owned network/tcp-socket handle trips the
		// handle table's cross-type-confusion check, exactly the same
		// failure mode wasi_fs.go's own withResourceTag calls guard against
		// for descriptor/input-stream/output-stream/error. (The pollable tag
		// + block/poll are registered centrally -- see wasi_poll.go.)
		withResourceTag(wasiIfaceSocketsNetwork, "network", wasiNetworkResType),
		withResourceTag(wasiIfaceSocketsTCP, "tcp-socket", wasiTCPSocketResType),
		withResourceTag(wasiIfaceSocketsIPNameLookup, "resolve-address-stream", wasiResolveStreamResType),

		withImportCustom(wasiIfaceSocketsInstanceNet, "instance-network", instanceNetwork, instNetFD, instNetResolve),
		withImportCustom(wasiIfaceSocketsIPNameLookup, "resolve-addresses", resolveAddresses, resolveFD, resolveResolve),
		withImportCustom(wasiIfaceSocketsIPNameLookup, "[method]resolve-address-stream.resolve-next-address", resolveNextAddress, resolveNextFD, resolveNextResolve),
		withImportCustom(wasiIfaceSocketsIPNameLookup, "[method]resolve-address-stream.subscribe", resolveSubscribe, resolveSubFD, resolveSubResolve),
		withImportCustom(wasiIfaceSocketsTCPCreateSoc, "create-tcp-socket", createTCPSocket, createTCPFD, createTCPResolve),
		withImportCustom(wasiIfaceSocketsTCP, "[method]tcp-socket.start-connect", startConnect, startConnectFD, startConnectResolve),
		withImportCustom(wasiIfaceSocketsTCP, "[method]tcp-socket.finish-connect", finishConnect, finishConnectFD, finishConnectResolve),
		withImportCustom(wasiIfaceSocketsTCP, "[method]tcp-socket.start-bind", startBind, startBindFD, startBindResolve),
		withImportCustom(wasiIfaceSocketsTCP, "[method]tcp-socket.finish-bind", finishBind, finishBindFD, finishBindResolve),
		withImportCustom(wasiIfaceSocketsTCP, "[method]tcp-socket.start-listen", startListen, startListenFD, startListenResolve),
		withImportCustom(wasiIfaceSocketsTCP, "[method]tcp-socket.finish-listen", finishListen, finishListenFD, finishListenResolve),
		withImportCustom(wasiIfaceSocketsTCP, "[method]tcp-socket.accept", accept, acceptFD, acceptResolve),
		withImportCustom(wasiIfaceSocketsTCP, "[method]tcp-socket.local-address", localAddress, localAddrFD, localAddrResolve),
		withImportCustom(wasiIfaceSocketsTCP, "[method]tcp-socket.remote-address", remoteAddress, remoteAddrFD, remoteAddrResolve),
		withImportCustom(wasiIfaceSocketsTCP, "[method]tcp-socket.set-listen-backlog-size", setListenBacklogSize, setBacklogFD, setBacklogResolve),
		tcpSetOpt("[method]tcp-socket.set-keep-alive-enabled", "bool"),
		tcpSetOpt("[method]tcp-socket.set-keep-alive-idle-time", "u64"),
		tcpSetOpt("[method]tcp-socket.set-keep-alive-interval", "u64"),
		tcpSetOpt("[method]tcp-socket.set-keep-alive-count", "u32"),
		tcpSetOpt("[method]tcp-socket.set-hop-limit", "u8"),
		tcpSetOpt("[method]tcp-socket.set-receive-buffer-size", "u64"),
		tcpSetOpt("[method]tcp-socket.set-send-buffer-size", "u64"),
		withImportCustom(wasiIfaceSocketsTCP, "[method]tcp-socket.subscribe", tcpSubscribe, tcpSubFD, tcpSubResolve),
		withImportCustom(wasiIfaceStreams, "[method]input-stream.subscribe", streamSubscribe, inSubFD, inSubResolve),
		withImportCustom(wasiIfaceStreams, "[method]output-stream.subscribe", streamSubscribe, outSubFD, outSubResolve),
	}
}

// # wasi:sockets/udp
//
// wasiUDPSocketOptions mirrors wasiSocketOptions above, but for UDP: a real
// wasi:sockets/udp + wasi:sockets/udp-create-socket implementation backed by
// net.PacketConn (WASIConfig.ListenPacket, defaulted to net.ListenPacket in
// WithWASI -- see its doc), only registered when WASIConfig.AllowUDP opts
// in.
//
// # Discovery
//
// Built the same way wasi_sockets.go's TCP half originally was (see this
// file's top-of-file package doc): running testdata/real_udp.component.wasm
// (a genuine rustc wasm32-wasip2 guest built from `std::net::UdpSocket::
// bind` + `send_to` + `recv_from` + print -- confirmed to work end-to-end
// under a real `wasmtime run -S inherit-network` against a scratch Go UDP
// server before any of this implementation existed) against a host with
// wasi:sockets/tcp registered but wasi:sockets/udp deliberately left
// unregistered traps loud, one call at a time, naming exactly the sequence
// a real bind+send_to+recv_from run reaches:
//
//   - wasi:sockets/udp-create-socket.create-udp-socket
//   - wasi:sockets/udp [method]udp-socket.{start-bind,finish-bind,%stream,
//     subscribe}
//   - wasi:sockets/udp [method]outgoing-datagram-stream.{check-send,send,
//     subscribe}
//   - wasi:sockets/udp [method]incoming-datagram-stream.{receive,subscribe}
//   - wasi:io/poll [method]pollable.block (already registered by
//     wasiSocketOptions -- UDP's pollable is the exact same always-ready
//     singleton, see wasiPollableRep's doc, so no separate registration is
//     needed for it here)
//
// # Blocking, single-shot bind/send/receive model
//
// Mirrors TCP's own documented simplification (see this file's package
// doc's "Blocking, single-shot connect/read/write model" section) applied
// to UDP's own async shape: start-bind performs the real, blocking
// net.ListenPacket synchronously and records the outcome on the udp-socket
// node before returning, so finish-bind never has anything to report but
// the already-settled Ok/Err; every pollable this half mints is the same
// always-ready singleton wasiSocketOptions' TCP half already registers
// [method]pollable.block for. The one place this genuinely differs from
// TCP: udp.wit's own receive is documented to never block and never report
// would-block (an explicitly non-blocking, poll-then-retry contract) --
// incomingDatagramStream.receive's own doc explains why blocking for real
// inside receive is still the correct choice for this single-threaded host.
func wasiUDPSocketOptions(sockets *wasiSockets) []Option {
	// instance-network is registered here too, not just by wasiSocketOptions'
	// TCP half: WASIConfig.AllowUDP may be set independently of AllowTCP
	// (see its own doc), and [method]udp-socket.start-bind takes a
	// borrow<network> arg exactly like [method]tcp-socket.start-connect
	// does -- a guest that only ever touches UDP still needs
	// wasi:sockets/instance-network.instance-network registered. Both
	// halves register the identical closure/tag under the identical
	// iface+name key when AllowTCP and AllowUDP are both set; the config
	// map just keeps whichever was registered last, which is harmless since
	// both are the exact same behavior (see wasiNetworkRep's doc: there is
	// only ever one network).
	instanceNetwork := func(context.Context, []abi.Value) ([]abi.Value, error) {
		return []abi.Value{wasiNetworkRep}, nil
	}

	createUDPSocket := func(_ context.Context, args []abi.Value) ([]abi.Value, error) {
		if len(args) != 1 {
			return nil, fmt.Errorf("wasi:sockets/udp-create-socket.create-udp-socket: expected 1 arg (address-family), got %d", len(args))
		}
		fam, ok := args[0].(uint32)
		if !ok {
			return nil, fmt.Errorf("wasi:sockets/udp-create-socket.create-udp-socket: address-family: expected uint32, got %T", args[0])
		}
		resources, err := sockets.getResources()
		if err != nil {
			return nil, err
		}
		rep := sockets.allocRep()
		sockets.mu.Lock()
		sockets.udpSocks[rep] = &udpSockNode{family: fam}
		sockets.mu.Unlock()
		handle := resources.NewOwn(wasiUDPSocketResType, rep)
		return []abi.Value{abi.ResultValue{IsErr: false, Payload: handle}}, nil
	}

	udpStartBind := func(_ context.Context, args []abi.Value) ([]abi.Value, error) {
		if len(args) != 3 {
			return nil, fmt.Errorf("[method]udp-socket.start-bind: expected 3 args (self, network, local-address), got %d", len(args))
		}
		selfRep, ok := args[0].(uint32)
		if !ok {
			return nil, fmt.Errorf("[method]udp-socket.start-bind: self: expected uint32 rep, got %T", args[0])
		}
		// args[1] (network, borrow<network>) -- see startConnect's identical
		// comment above: this package has exactly one network, nothing
		// further to inspect.
		addr, err := wasiIPSocketAddrToString(args[2])
		if err != nil {
			return nil, fmt.Errorf("[method]udp-socket.start-bind: local-address: %w", err)
		}
		node, ok := sockets.udpSockNode(selfRep)
		if !ok {
			return nil, fmt.Errorf("[method]udp-socket.start-bind: udp-socket rep %d does not name a live socket", selfRep)
		}
		node.mu.Lock()
		defer node.mu.Unlock()
		if node.started {
			return []abi.Value{abi.ResultValue{IsErr: true, Payload: wasiSockErrConcurrencyConflict}}, nil
		}
		node.started = true
		// See this func's doc's "Blocking, single-shot" section: the real,
		// blocking bind happens right here, synchronously -- finish-bind
		// (below) only ever reports this already-settled outcome.
		node.pconn, node.bindErr = sockets.listenPacket("udp", addr)
		return []abi.Value{abi.ResultValue{IsErr: false, Payload: nil}}, nil
	}

	udpFinishBind := func(_ context.Context, args []abi.Value) ([]abi.Value, error) {
		if len(args) != 1 {
			return nil, fmt.Errorf("[method]udp-socket.finish-bind: expected 1 arg (self), got %d", len(args))
		}
		selfRep, ok := args[0].(uint32)
		if !ok {
			return nil, fmt.Errorf("[method]udp-socket.finish-bind: self: expected uint32 rep, got %T", args[0])
		}
		node, ok := sockets.udpSockNode(selfRep)
		if !ok {
			return nil, fmt.Errorf("[method]udp-socket.finish-bind: udp-socket rep %d does not name a live socket", selfRep)
		}
		node.mu.Lock()
		defer node.mu.Unlock()
		if !node.started {
			return []abi.Value{abi.ResultValue{IsErr: true, Payload: wasiSockErrNotInProgress}}, nil
		}
		if node.finished {
			return []abi.Value{abi.ResultValue{IsErr: true, Payload: wasiSockErrNotInProgress}}, nil
		}
		node.finished = true
		if node.bindErr != nil {
			return []abi.Value{abi.ResultValue{IsErr: true, Payload: wasiUDPErrToCode(node.bindErr)}}, nil
		}
		return []abi.Value{abi.ResultValue{IsErr: false, Payload: nil}}, nil
	}

	udpStream := func(_ context.Context, args []abi.Value) ([]abi.Value, error) {
		if len(args) != 2 {
			return nil, fmt.Errorf("[method]udp-socket.stream: expected 2 args (self, remote-address), got %d", len(args))
		}
		selfRep, ok := args[0].(uint32)
		if !ok {
			return nil, fmt.Errorf("[method]udp-socket.stream: self: expected uint32 rep, got %T", args[0])
		}
		node, ok := sockets.udpSockNode(selfRep)
		if !ok {
			return nil, fmt.Errorf("[method]udp-socket.stream: udp-socket rep %d does not name a live socket", selfRep)
		}
		node.mu.Lock()
		if node.pconn == nil {
			node.mu.Unlock()
			return []abi.Value{abi.ResultValue{IsErr: true, Payload: wasiSockErrInvalidState}}, nil
		}
		pconn := node.pconn
		node.mu.Unlock()

		var remote *net.UDPAddr
		if args[1] != nil { // option<ip-socket-address>: nil (none) or the variant directly (see abi.Value's doc)
			addrStr, err := wasiIPSocketAddrToString(args[1])
			if err != nil {
				return nil, fmt.Errorf("[method]udp-socket.stream: remote-address: %w", err)
			}
			remote, err = net.ResolveUDPAddr("udp", addrStr)
			if err != nil {
				return nil, fmt.Errorf("[method]udp-socket.stream: remote-address: resolve %q: %w", addrStr, err)
			}
		}

		resources, err := sockets.getResources()
		if err != nil {
			return nil, err
		}
		inRep := sockets.allocRep()
		outRep := sockets.allocRep()
		sockets.mu.Lock()
		sockets.inDgrams[inRep] = &incomingDatagramStream{pconn: pconn, remote: remote}
		sockets.outDgrams[outRep] = &outgoingDatagramStream{pconn: pconn, remote: remote}
		sockets.mu.Unlock()
		// tuple<own<incoming-datagram-stream>,own<outgoing-datagram-stream>>
		// -- see finishConnect's identical comment above: both handles are
		// minted directly here since host_import.go's generic
		// allocHandleResult only auto-converts a top-level own/borrow.
		inHandle := resources.NewOwn(wasiIncomingDatagramStreamResType, inRep)
		outHandle := resources.NewOwn(wasiOutgoingDatagramStreamResType, outRep)
		return []abi.Value{abi.ResultValue{IsErr: false, Payload: []abi.Value{inHandle, outHandle}}}, nil
	}

	udpSubscribe := func(_ context.Context, args []abi.Value) ([]abi.Value, error) {
		if len(args) != 1 {
			return nil, fmt.Errorf("[method]udp-socket.subscribe: expected 1 arg (self), got %d", len(args))
		}
		selfRep, ok := args[0].(uint32)
		if !ok {
			return nil, fmt.Errorf("[method]udp-socket.subscribe: self: expected uint32 rep, got %T", args[0])
		}
		if _, ok := sockets.udpSockNode(selfRep); !ok {
			return nil, fmt.Errorf("[method]udp-socket.subscribe: udp-socket rep %d does not name a live socket", selfRep)
		}
		return []abi.Value{wasiPollableRep}, nil
	}

	// udpLocalAddress implements [method]udp-socket.local-address(self) ->
	// result<ip-socket-address, error-code>: the bound net.PacketConn's local
	// address (a server guest prints it to learn its ephemeral :0 port).
	udpLocalAddress := func(_ context.Context, args []abi.Value) ([]abi.Value, error) {
		if len(args) != 1 {
			return nil, fmt.Errorf("[method]udp-socket.local-address: expected 1 arg (self), got %d", len(args))
		}
		selfRep, ok := args[0].(uint32)
		if !ok {
			return nil, fmt.Errorf("[method]udp-socket.local-address: self: expected uint32 rep, got %T", args[0])
		}
		node, ok := sockets.udpSockNode(selfRep)
		if !ok {
			return nil, fmt.Errorf("[method]udp-socket.local-address: udp-socket rep %d does not name a live socket", selfRep)
		}
		node.mu.Lock()
		pconn := node.pconn
		node.mu.Unlock()
		if pconn == nil {
			return []abi.Value{abi.ResultValue{IsErr: true, Payload: wasiSockErrInvalidState}}, nil
		}
		udpAddr, ok := pconn.LocalAddr().(*net.UDPAddr)
		if !ok {
			return []abi.Value{abi.ResultValue{IsErr: true, Payload: wasiSockErrInvalidState}}, nil
		}
		v, err := wasiIPSocketAddrFromUDPAddr(udpAddr)
		if err != nil {
			return nil, fmt.Errorf("[method]udp-socket.local-address: %w", err)
		}
		return []abi.Value{abi.ResultValue{IsErr: false, Payload: v}}, nil
	}

	incomingReceive := func(_ context.Context, args []abi.Value) ([]abi.Value, error) {
		if len(args) != 2 {
			return nil, fmt.Errorf("[method]incoming-datagram-stream.receive: expected 2 args (self, max-results), got %d", len(args))
		}
		selfRep, ok := args[0].(uint32)
		if !ok {
			return nil, fmt.Errorf("[method]incoming-datagram-stream.receive: self: expected uint32 rep, got %T", args[0])
		}
		maxResults, ok := args[1].(uint64)
		if !ok {
			return nil, fmt.Errorf("[method]incoming-datagram-stream.receive: max-results: expected uint64, got %T", args[1])
		}
		node, ok := sockets.inDatagramStreamNode(selfRep)
		if !ok {
			return nil, fmt.Errorf("[method]incoming-datagram-stream.receive: incoming-datagram-stream rep %d does not name a live stream", selfRep)
		}
		return node.receive(maxResults)
	}

	incomingSubscribe := func(_ context.Context, args []abi.Value) ([]abi.Value, error) {
		if len(args) != 1 {
			return nil, fmt.Errorf("[method]incoming-datagram-stream.subscribe: expected 1 arg (self), got %d", len(args))
		}
		selfRep, ok := args[0].(uint32)
		if !ok {
			return nil, fmt.Errorf("[method]incoming-datagram-stream.subscribe: self: expected uint32 rep, got %T", args[0])
		}
		if _, ok := sockets.inDatagramStreamNode(selfRep); !ok {
			return nil, fmt.Errorf("[method]incoming-datagram-stream.subscribe: incoming-datagram-stream rep %d does not name a live stream", selfRep)
		}
		return []abi.Value{wasiPollableRep}, nil
	}

	outgoingCheckSend := func(_ context.Context, args []abi.Value) ([]abi.Value, error) {
		if len(args) != 1 {
			return nil, fmt.Errorf("[method]outgoing-datagram-stream.check-send: expected 1 arg (self), got %d", len(args))
		}
		selfRep, ok := args[0].(uint32)
		if !ok {
			return nil, fmt.Errorf("[method]outgoing-datagram-stream.check-send: self: expected uint32 rep, got %T", args[0])
		}
		if _, ok := sockets.outDatagramStreamNode(selfRep); !ok {
			return nil, fmt.Errorf("[method]outgoing-datagram-stream.check-send: outgoing-datagram-stream rep %d does not name a live stream", selfRep)
		}
		// A large, fixed budget: there is no real backpressure to model
		// against a net.PacketConn -- mirrors wasi.go's checkWrite doc for
		// the identical reasoning on the TCP/stdio output-stream side.
		return []abi.Value{abi.ResultValue{IsErr: false, Payload: uint64(1) << 20}}, nil
	}

	outgoingSend := func(_ context.Context, args []abi.Value) ([]abi.Value, error) {
		if len(args) != 2 {
			return nil, fmt.Errorf("[method]outgoing-datagram-stream.send: expected 2 args (self, datagrams), got %d", len(args))
		}
		selfRep, ok := args[0].(uint32)
		if !ok {
			return nil, fmt.Errorf("[method]outgoing-datagram-stream.send: self: expected uint32 rep, got %T", args[0])
		}
		datagrams, ok := args[1].([]abi.Value)
		if !ok {
			return nil, fmt.Errorf("[method]outgoing-datagram-stream.send: datagrams: expected list<outgoing-datagram> ([]abi.Value), got %T", args[1])
		}
		node, ok := sockets.outDatagramStreamNode(selfRep)
		if !ok {
			return nil, fmt.Errorf("[method]outgoing-datagram-stream.send: outgoing-datagram-stream rep %d does not name a live stream", selfRep)
		}
		return node.send(datagrams)
	}

	outgoingSubscribe := func(_ context.Context, args []abi.Value) ([]abi.Value, error) {
		if len(args) != 1 {
			return nil, fmt.Errorf("[method]outgoing-datagram-stream.subscribe: expected 1 arg (self), got %d", len(args))
		}
		selfRep, ok := args[0].(uint32)
		if !ok {
			return nil, fmt.Errorf("[method]outgoing-datagram-stream.subscribe: self: expected uint32 rep, got %T", args[0])
		}
		if _, ok := sockets.outDatagramStreamNode(selfRep); !ok {
			return nil, fmt.Errorf("[method]outgoing-datagram-stream.subscribe: outgoing-datagram-stream rep %d does not name a live stream", selfRep)
		}
		return []abi.Value{wasiPollableRep}, nil
	}

	instNetFD, instNetResolve := wasiInstanceNetworkSig()
	createUDPFD, createUDPResolve := wasiCreateUDPSocketSig()
	startBindFD, startBindResolve := wasiUDPStartBindSig()
	finishBindFD, finishBindResolve := wasiUDPFinishBindSig()
	streamFD, streamResolve := wasiUDPStreamSig()
	udpSubFD, udpSubResolve := wasiSubscribeSig(wasiUDPSocketResType)
	udpLocalAddrFD, udpLocalAddrResolve := wasiLocalAddressSig(wasiUDPSocketResType)
	receiveFD, receiveResolve := wasiIncomingReceiveSig()
	inDgramSubFD, inDgramSubResolve := wasiSubscribeSig(wasiIncomingDatagramStreamResType)
	checkSendFD, checkSendResolve := wasiOutgoingCheckSendSig()
	sendFD, sendResolve := wasiOutgoingSendSig()
	outDgramSubFD, outDgramSubResolve := wasiSubscribeSig(wasiOutgoingDatagramStreamResType)

	udpSetOpt := func(name, prim string) Option {
		fd, r := wasiSocketSetOptSig(wasiUDPSocketResType, prim)
		return withImportCustom(wasiIfaceSocketsUDP, name, wasiSocketSetOptOK, fd, r)
	}

	return []Option{
		withResourcesHook(sockets.setResources),

		// See withResourceTag's doc (host_import.go) -- same reasoning as
		// wasiSocketOptions' identical block for TCP's own resources. (The
		// pollable tag + block/poll are registered centrally, see wasi_poll.go.)
		withResourceTag(wasiIfaceSocketsNetwork, "network", wasiNetworkResType),
		withResourceTag(wasiIfaceSocketsUDP, "udp-socket", wasiUDPSocketResType),
		withResourceTag(wasiIfaceSocketsUDP, "incoming-datagram-stream", wasiIncomingDatagramStreamResType),
		withResourceTag(wasiIfaceSocketsUDP, "outgoing-datagram-stream", wasiOutgoingDatagramStreamResType),

		withImportCustom(wasiIfaceSocketsInstanceNet, "instance-network", instanceNetwork, instNetFD, instNetResolve),
		withImportCustom(wasiIfaceSocketsUDPCreateSoc, "create-udp-socket", createUDPSocket, createUDPFD, createUDPResolve),
		withImportCustom(wasiIfaceSocketsUDP, "[method]udp-socket.start-bind", udpStartBind, startBindFD, startBindResolve),
		withImportCustom(wasiIfaceSocketsUDP, "[method]udp-socket.finish-bind", udpFinishBind, finishBindFD, finishBindResolve),
		withImportCustom(wasiIfaceSocketsUDP, "[method]udp-socket.stream", udpStream, streamFD, streamResolve),
		withImportCustom(wasiIfaceSocketsUDP, "[method]udp-socket.subscribe", udpSubscribe, udpSubFD, udpSubResolve),
		withImportCustom(wasiIfaceSocketsUDP, "[method]udp-socket.local-address", udpLocalAddress, udpLocalAddrFD, udpLocalAddrResolve),
		udpSetOpt("[method]udp-socket.set-unicast-hop-limit", "u8"),
		udpSetOpt("[method]udp-socket.set-receive-buffer-size", "u64"),
		udpSetOpt("[method]udp-socket.set-send-buffer-size", "u64"),
		withImportCustom(wasiIfaceSocketsUDP, "[method]incoming-datagram-stream.receive", incomingReceive, receiveFD, receiveResolve),
		withImportCustom(wasiIfaceSocketsUDP, "[method]incoming-datagram-stream.subscribe", incomingSubscribe, inDgramSubFD, inDgramSubResolve),
		withImportCustom(wasiIfaceSocketsUDP, "[method]outgoing-datagram-stream.check-send", outgoingCheckSend, checkSendFD, checkSendResolve),
		withImportCustom(wasiIfaceSocketsUDP, "[method]outgoing-datagram-stream.send", outgoingSend, sendFD, sendResolve),
		withImportCustom(wasiIfaceSocketsUDP, "[method]outgoing-datagram-stream.subscribe", outgoingSubscribe, outDgramSubFD, outDgramSubResolve),
	}
}

// wasiIPv4AddressType interns wasi:sockets/network's `ipv4-address`
// (`type ipv4-address = tuple<u8,u8,u8,u8>`) into tbl and returns its
// TypeRef.
func wasiIPv4AddressType(tbl *typeTable) binary.TypeRef {
	u8 := binary.TypeRef{Primitive: "u8"}
	return tbl.add(binary.TupleDesc{Elements: []binary.TypeRef{u8, u8, u8, u8}})
}

// wasiIPv4SocketAddressType interns `record ipv4-socket-address { port: u16,
// address: ipv4-address }` into tbl and returns its TypeRef.
func wasiIPv4SocketAddressType(tbl *typeTable) binary.TypeRef {
	addrRef := wasiIPv4AddressType(tbl)
	return tbl.add(binary.RecordDesc{Fields: []binary.RecordField{
		{Name: "port", Type: binary.TypeRef{Primitive: "u16"}},
		{Name: "address", Type: addrRef},
	}})
}

// wasiIPv6AddressType interns `type ipv6-address =
// tuple<u16,u16,u16,u16,u16,u16,u16,u16>` into tbl and returns its TypeRef.
func wasiIPv6AddressType(tbl *typeTable) binary.TypeRef {
	u16 := binary.TypeRef{Primitive: "u16"}
	return tbl.add(binary.TupleDesc{Elements: []binary.TypeRef{u16, u16, u16, u16, u16, u16, u16, u16}})
}

// wasiIPv6SocketAddressType interns `record ipv6-socket-address { port: u16,
// flow-info: u32, address: ipv6-address, scope-id: u32 }` into tbl and
// returns its TypeRef, in exact WIT declaration order.
func wasiIPv6SocketAddressType(tbl *typeTable) binary.TypeRef {
	addrRef := wasiIPv6AddressType(tbl)
	return tbl.add(binary.RecordDesc{Fields: []binary.RecordField{
		{Name: "port", Type: binary.TypeRef{Primitive: "u16"}},
		{Name: "flow-info", Type: binary.TypeRef{Primitive: "u32"}},
		{Name: "address", Type: addrRef},
		{Name: "scope-id", Type: binary.TypeRef{Primitive: "u32"}},
	}})
}

// wasiIPSocketAddressType interns `variant ip-socket-address {
// ipv4(ipv4-socket-address), ipv6(ipv6-socket-address) }` into tbl and
// returns its TypeRef.
func wasiIPSocketAddressType(tbl *typeTable) binary.TypeRef {
	v4 := wasiIPv4SocketAddressType(tbl)
	v6 := wasiIPv6SocketAddressType(tbl)
	return tbl.add(binary.VariantDesc{Cases: []binary.VariantCase{
		{Name: "ipv4", Type: &v4},
		{Name: "ipv6", Type: &v6},
	}})
}

// wasiIPAddressFamilyType interns `enum ip-address-family { ipv4, ipv6 }`
// into tbl and returns its TypeRef.
func wasiIPAddressFamilyType(tbl *typeTable) binary.TypeRef {
	return tbl.add(binary.EnumDesc{Cases: []string{"ipv4", "ipv6"}})
}

// wasiSocketsErrorCodeType interns wasi:sockets/network's `error-code` enum
// into tbl and returns its TypeRef, in exact WIT declaration order -- see
// this file's wasiSockErr* constants, which must stay in lockstep with
// this list's order.
func wasiSocketsErrorCodeType(tbl *typeTable) binary.TypeRef {
	return tbl.add(binary.EnumDesc{Cases: []string{
		"unknown", "access-denied", "not-supported", "invalid-argument", "out-of-memory",
		"timeout", "concurrency-conflict", "not-in-progress", "would-block", "invalid-state",
		"new-socket-limit", "address-not-bindable", "address-in-use", "remote-unreachable",
		"connection-refused", "connection-reset", "connection-aborted", "datagram-too-large",
		"name-unresolvable", "temporary-resolver-failure", "permanent-resolver-failure",
	}})
}

// wasiInstanceNetworkSig builds the FuncDesc/resolver for
// wasi:sockets/instance-network.instance-network() -> own<network>.
func wasiInstanceNetworkSig() (binary.FuncDesc, abi.Resolver) {
	tbl := &typeTable{}
	okRef := tbl.add(binary.OwnDesc{ResourceType: wasiNetworkResType})
	fd := binary.FuncDesc{Results: binary.FuncResults{Unnamed: &okRef}}
	return fd, tbl.resolver()
}

// wasiCreateTCPSocketSig builds the FuncDesc/resolver for
// wasi:sockets/tcp-create-socket.create-tcp-socket(address-family:
// ip-address-family) -> result<own<tcp-socket>, error-code>.
func wasiCreateTCPSocketSig() (binary.FuncDesc, abi.Resolver) {
	tbl := &typeTable{}
	famRef := wasiIPAddressFamilyType(tbl)
	okRef := tbl.add(binary.OwnDesc{ResourceType: wasiTCPSocketResType})
	errRef := wasiSocketsErrorCodeType(tbl)
	resultRef := tbl.add(binary.ResultDesc{Ok: &okRef, Err: &errRef})
	fd := binary.FuncDesc{
		Params:  []binary.FuncParam{{Name: "address-family", Type: famRef}},
		Results: binary.FuncResults{Unnamed: &resultRef},
	}
	return fd, tbl.resolver()
}

// wasiTCPStartConnectSig builds the FuncDesc/resolver for
// [method]tcp-socket.start-connect(self: borrow<tcp-socket>, network:
// borrow<network>, remote-address: ip-socket-address) -> result<_,
// error-code>.
func wasiTCPStartConnectSig() (binary.FuncDesc, abi.Resolver) {
	tbl := &typeTable{}
	selfRef := tbl.add(binary.BorrowDesc{ResourceType: wasiTCPSocketResType})
	netRef := tbl.add(binary.BorrowDesc{ResourceType: wasiNetworkResType})
	addrRef := wasiIPSocketAddressType(tbl)
	errRef := wasiSocketsErrorCodeType(tbl)
	resultRef := tbl.add(binary.ResultDesc{Err: &errRef})
	fd := binary.FuncDesc{
		Params: []binary.FuncParam{
			{Name: "self", Type: selfRef},
			{Name: "network", Type: netRef},
			{Name: "remote-address", Type: addrRef},
		},
		Results: binary.FuncResults{Unnamed: &resultRef},
	}
	return fd, tbl.resolver()
}

// wasiTCPFinishConnectSig builds the FuncDesc/resolver for
// [method]tcp-socket.finish-connect(self: borrow<tcp-socket>) ->
// result<tuple<own<input-stream>,own<output-stream>>, error-code>.
func wasiTCPFinishConnectSig() (binary.FuncDesc, abi.Resolver) {
	tbl := &typeTable{}
	selfRef := tbl.add(binary.BorrowDesc{ResourceType: wasiTCPSocketResType})
	inRef := tbl.add(binary.OwnDesc{ResourceType: wasiInputStreamResType})
	outRef := tbl.add(binary.OwnDesc{ResourceType: wasiOutputStreamResType})
	tupleRef := tbl.add(binary.TupleDesc{Elements: []binary.TypeRef{inRef, outRef}})
	errRef := wasiSocketsErrorCodeType(tbl)
	resultRef := tbl.add(binary.ResultDesc{Ok: &tupleRef, Err: &errRef})
	fd := binary.FuncDesc{
		Params:  []binary.FuncParam{{Name: "self", Type: selfRef}},
		Results: binary.FuncResults{Unnamed: &resultRef},
	}
	return fd, tbl.resolver()
}

// wasiTCPStartBindSig builds the FuncDesc/resolver for
// [method]tcp-socket.start-bind(self: borrow<tcp-socket>, network:
// borrow<network>, local-address: ip-socket-address) -> result<_, error-code>.
func wasiTCPStartBindSig() (binary.FuncDesc, abi.Resolver) {
	tbl := &typeTable{}
	selfRef := tbl.add(binary.BorrowDesc{ResourceType: wasiTCPSocketResType})
	netRef := tbl.add(binary.BorrowDesc{ResourceType: wasiNetworkResType})
	addrRef := wasiIPSocketAddressType(tbl)
	errRef := wasiSocketsErrorCodeType(tbl)
	resultRef := tbl.add(binary.ResultDesc{Err: &errRef})
	fd := binary.FuncDesc{
		Params: []binary.FuncParam{
			{Name: "self", Type: selfRef},
			{Name: "network", Type: netRef},
			{Name: "local-address", Type: addrRef},
		},
		Results: binary.FuncResults{Unnamed: &resultRef},
	}
	return fd, tbl.resolver()
}

// wasiTCPSelfResultSig builds the FuncDesc/resolver for a `method(self:
// borrow<tcp-socket>) -> result<_, error-code>` method -- shared by
// finish-bind, start-listen, and finish-listen (identical signatures).
func wasiTCPSelfResultSig() (binary.FuncDesc, abi.Resolver) {
	tbl := &typeTable{}
	selfRef := tbl.add(binary.BorrowDesc{ResourceType: wasiTCPSocketResType})
	errRef := wasiSocketsErrorCodeType(tbl)
	resultRef := tbl.add(binary.ResultDesc{Err: &errRef})
	fd := binary.FuncDesc{
		Params:  []binary.FuncParam{{Name: "self", Type: selfRef}},
		Results: binary.FuncResults{Unnamed: &resultRef},
	}
	return fd, tbl.resolver()
}

// wasiTCPAcceptSig builds the FuncDesc/resolver for
// [method]tcp-socket.accept(self: borrow<tcp-socket>) ->
// result<tuple<own<tcp-socket>, own<input-stream>, own<output-stream>>,
// error-code>.
func wasiTCPAcceptSig() (binary.FuncDesc, abi.Resolver) {
	tbl := &typeTable{}
	selfRef := tbl.add(binary.BorrowDesc{ResourceType: wasiTCPSocketResType})
	sockRef := tbl.add(binary.OwnDesc{ResourceType: wasiTCPSocketResType})
	inRef := tbl.add(binary.OwnDesc{ResourceType: wasiInputStreamResType})
	outRef := tbl.add(binary.OwnDesc{ResourceType: wasiOutputStreamResType})
	tupleRef := tbl.add(binary.TupleDesc{Elements: []binary.TypeRef{sockRef, inRef, outRef}})
	errRef := wasiSocketsErrorCodeType(tbl)
	resultRef := tbl.add(binary.ResultDesc{Ok: &tupleRef, Err: &errRef})
	fd := binary.FuncDesc{
		Params:  []binary.FuncParam{{Name: "self", Type: selfRef}},
		Results: binary.FuncResults{Unnamed: &resultRef},
	}
	return fd, tbl.resolver()
}

// wasiTCPLocalAddressSig builds the FuncDesc/resolver for
// [method]tcp-socket.local-address(self: borrow<tcp-socket>) ->
// result<ip-socket-address, error-code>.
func wasiTCPLocalAddressSig() (binary.FuncDesc, abi.Resolver) {
	return wasiLocalAddressSig(wasiTCPSocketResType)
}

// wasiSocketSetOptSig builds the FuncDesc/resolver for a socket option setter
// `set-*(self: borrow<selfResType>, value: paramPrim) -> result<_,
// error-code>` -- the shape every tcp-socket/udp-socket setsockopt-style method
// shares (only the borrowed self type and the value's primitive differ).
func wasiSocketSetOptSig(selfResType uint32, paramPrim string) (binary.FuncDesc, abi.Resolver) {
	tbl := &typeTable{}
	selfRef := tbl.add(binary.BorrowDesc{ResourceType: selfResType})
	errRef := wasiSocketsErrorCodeType(tbl)
	resultRef := tbl.add(binary.ResultDesc{Err: &errRef})
	fd := binary.FuncDesc{
		Params: []binary.FuncParam{
			{Name: "self", Type: selfRef},
			{Name: "value", Type: binary.TypeRef{Primitive: paramPrim}},
		},
		Results: binary.FuncResults{Unnamed: &resultRef},
	}
	return fd, tbl.resolver()
}

// wasiSocketSetOptOK is the shared no-op body for every socket option setter:
// these are best-effort advisory hints (keep-alive, buffer sizes, hop limits)
// the WASI sockets spec permits a host to ignore, and Go's net package exposes
// no portable knob for most of them, so wazy accepts and ignores them (returns
// Ok) rather than trapping -- keeping any real guest that tweaks socket options
// running. ponytail: wire net.Dialer/ListenConfig knobs only if a guest ever
// depends on an option actually taking effect.
func wasiSocketSetOptOK(context.Context, []abi.Value) ([]abi.Value, error) {
	return []abi.Value{abi.ResultValue{IsErr: false, Payload: nil}}, nil
}

// wasiLocalAddressSig builds the FuncDesc/resolver for a `local-address(self:
// borrow<selfResType>) -> result<ip-socket-address, error-code>` method --
// shared by tcp-socket (local + remote) and udp-socket, which differ only in
// the borrowed self type (using the wrong type trips the handle table's
// cross-type-confusion check).
func wasiLocalAddressSig(selfResType uint32) (binary.FuncDesc, abi.Resolver) {
	tbl := &typeTable{}
	selfRef := tbl.add(binary.BorrowDesc{ResourceType: selfResType})
	addrRef := wasiIPSocketAddressType(tbl)
	errRef := wasiSocketsErrorCodeType(tbl)
	resultRef := tbl.add(binary.ResultDesc{Ok: &addrRef, Err: &errRef})
	fd := binary.FuncDesc{
		Params:  []binary.FuncParam{{Name: "self", Type: selfRef}},
		Results: binary.FuncResults{Unnamed: &resultRef},
	}
	return fd, tbl.resolver()
}

// wasiTCPSetListenBacklogSig builds the FuncDesc/resolver for
// [method]tcp-socket.set-listen-backlog-size(self: borrow<tcp-socket>,
// value: u64) -> result<_, error-code>.
func wasiTCPSetListenBacklogSig() (binary.FuncDesc, abi.Resolver) {
	tbl := &typeTable{}
	selfRef := tbl.add(binary.BorrowDesc{ResourceType: wasiTCPSocketResType})
	errRef := wasiSocketsErrorCodeType(tbl)
	resultRef := tbl.add(binary.ResultDesc{Err: &errRef})
	fd := binary.FuncDesc{
		Params: []binary.FuncParam{
			{Name: "self", Type: selfRef},
			{Name: "value", Type: binary.TypeRef{Primitive: "u64"}},
		},
		Results: binary.FuncResults{Unnamed: &resultRef},
	}
	return fd, tbl.resolver()
}

// wasiResolveAddressesSig builds the FuncDesc/resolver for
// wasi:sockets/ip-name-lookup.resolve-addresses(network: borrow<network>,
// name: string) -> result<own<resolve-address-stream>, error-code>.
func wasiResolveAddressesSig() (binary.FuncDesc, abi.Resolver) {
	tbl := &typeTable{}
	netRef := tbl.add(binary.BorrowDesc{ResourceType: wasiNetworkResType})
	okRef := tbl.add(binary.OwnDesc{ResourceType: wasiResolveStreamResType})
	errRef := wasiSocketsErrorCodeType(tbl)
	resultRef := tbl.add(binary.ResultDesc{Ok: &okRef, Err: &errRef})
	fd := binary.FuncDesc{
		Params: []binary.FuncParam{
			{Name: "network", Type: netRef},
			{Name: "name", Type: binary.TypeRef{Primitive: "string"}},
		},
		Results: binary.FuncResults{Unnamed: &resultRef},
	}
	return fd, tbl.resolver()
}

// wasiResolveNextAddressSig builds the FuncDesc/resolver for
// [method]resolve-address-stream.resolve-next-address(self:
// borrow<resolve-address-stream>) -> result<option<ip-address>, error-code>.
func wasiResolveNextAddressSig() (binary.FuncDesc, abi.Resolver) {
	tbl := &typeTable{}
	selfRef := tbl.add(binary.BorrowDesc{ResourceType: wasiResolveStreamResType})
	addrRef := wasiIPAddressType(tbl)
	optRef := tbl.add(binary.OptionDesc{Element: addrRef})
	errRef := wasiSocketsErrorCodeType(tbl)
	resultRef := tbl.add(binary.ResultDesc{Ok: &optRef, Err: &errRef})
	fd := binary.FuncDesc{
		Params:  []binary.FuncParam{{Name: "self", Type: selfRef}},
		Results: binary.FuncResults{Unnamed: &resultRef},
	}
	return fd, tbl.resolver()
}

// wasiSubscribeSig builds the FuncDesc/resolver for a `subscribe(self:
// borrow<selfResType>) -> pollable` method -- shared by
// [method]tcp-socket.subscribe, [method]input-stream.subscribe, and
// [method]output-stream.subscribe (only the borrowed self type differs).
func wasiSubscribeSig(selfResType uint32) (binary.FuncDesc, abi.Resolver) {
	tbl := &typeTable{}
	selfRef := tbl.add(binary.BorrowDesc{ResourceType: selfResType})
	pollRef := tbl.add(binary.OwnDesc{ResourceType: wasiPollableResType})
	fd := binary.FuncDesc{
		Params:  []binary.FuncParam{{Name: "self", Type: selfRef}},
		Results: binary.FuncResults{Unnamed: &pollRef},
	}
	return fd, tbl.resolver()
}

// wasiPollableBlockSig builds the FuncDesc/resolver for
// [method]pollable.block(self: borrow<pollable>) -> () -- no Results
// (funcResultTypeRefs on a zero-value FuncResults reports 0 results,
// exercised by lowerHostResults' own `len(refs) == 0` early-return; see
// resourceCanonHostFunc's sibling doc in host_import.go for the general
// "zero core return values" shape).
func wasiPollableBlockSig() (binary.FuncDesc, abi.Resolver) {
	tbl := &typeTable{}
	selfRef := tbl.add(binary.BorrowDesc{ResourceType: wasiPollableResType})
	fd := binary.FuncDesc{Params: []binary.FuncParam{{Name: "self", Type: selfRef}}}
	return fd, tbl.resolver()
}

// wasiPollSig builds the FuncDesc/resolver for the free wasi:io/poll.
// poll(in: list<borrow<pollable>>) -> list<u32>.
func wasiPollSig() (binary.FuncDesc, abi.Resolver) {
	tbl := &typeTable{}
	borrowRef := tbl.add(binary.BorrowDesc{ResourceType: wasiPollableResType})
	listRef := tbl.add(binary.ListDesc{Element: borrowRef})
	outListRef := tbl.add(binary.ListDesc{Element: binary.TypeRef{Primitive: "u32"}})
	fd := binary.FuncDesc{
		Params:  []binary.FuncParam{{Name: "in", Type: listRef}},
		Results: binary.FuncResults{Unnamed: &outListRef},
	}
	return fd, tbl.resolver()
}

// wasiIncomingDatagramType interns wasi:sockets/udp's `record
// incoming-datagram { data: list<u8>, remote-address: ip-socket-address }`
// into tbl and returns its TypeRef, in exact WIT declaration order.
func wasiIncomingDatagramType(tbl *typeTable) binary.TypeRef {
	dataRef := tbl.add(binary.ListDesc{Element: binary.TypeRef{Primitive: "u8"}})
	addrRef := wasiIPSocketAddressType(tbl)
	return tbl.add(binary.RecordDesc{Fields: []binary.RecordField{
		{Name: "data", Type: dataRef},
		{Name: "remote-address", Type: addrRef},
	}})
}

// wasiOutgoingDatagramType interns wasi:sockets/udp's `record
// outgoing-datagram { data: list<u8>, remote-address:
// option<ip-socket-address> }` into tbl and returns its TypeRef, in exact
// WIT declaration order.
func wasiOutgoingDatagramType(tbl *typeTable) binary.TypeRef {
	dataRef := tbl.add(binary.ListDesc{Element: binary.TypeRef{Primitive: "u8"}})
	addrRef := wasiIPSocketAddressType(tbl)
	optAddrRef := tbl.add(binary.OptionDesc{Element: addrRef})
	return tbl.add(binary.RecordDesc{Fields: []binary.RecordField{
		{Name: "data", Type: dataRef},
		{Name: "remote-address", Type: optAddrRef},
	}})
}

// wasiCreateUDPSocketSig builds the FuncDesc/resolver for
// wasi:sockets/udp-create-socket.create-udp-socket(address-family:
// ip-address-family) -> result<own<udp-socket>, error-code>.
func wasiCreateUDPSocketSig() (binary.FuncDesc, abi.Resolver) {
	tbl := &typeTable{}
	famRef := wasiIPAddressFamilyType(tbl)
	okRef := tbl.add(binary.OwnDesc{ResourceType: wasiUDPSocketResType})
	errRef := wasiSocketsErrorCodeType(tbl)
	resultRef := tbl.add(binary.ResultDesc{Ok: &okRef, Err: &errRef})
	fd := binary.FuncDesc{
		Params:  []binary.FuncParam{{Name: "address-family", Type: famRef}},
		Results: binary.FuncResults{Unnamed: &resultRef},
	}
	return fd, tbl.resolver()
}

// wasiUDPStartBindSig builds the FuncDesc/resolver for
// [method]udp-socket.start-bind(self: borrow<udp-socket>, network:
// borrow<network>, local-address: ip-socket-address) -> result<_,
// error-code>.
func wasiUDPStartBindSig() (binary.FuncDesc, abi.Resolver) {
	tbl := &typeTable{}
	selfRef := tbl.add(binary.BorrowDesc{ResourceType: wasiUDPSocketResType})
	netRef := tbl.add(binary.BorrowDesc{ResourceType: wasiNetworkResType})
	addrRef := wasiIPSocketAddressType(tbl)
	errRef := wasiSocketsErrorCodeType(tbl)
	resultRef := tbl.add(binary.ResultDesc{Err: &errRef})
	fd := binary.FuncDesc{
		Params: []binary.FuncParam{
			{Name: "self", Type: selfRef},
			{Name: "network", Type: netRef},
			{Name: "local-address", Type: addrRef},
		},
		Results: binary.FuncResults{Unnamed: &resultRef},
	}
	return fd, tbl.resolver()
}

// wasiUDPFinishBindSig builds the FuncDesc/resolver for
// [method]udp-socket.finish-bind(self: borrow<udp-socket>) -> result<_,
// error-code>.
func wasiUDPFinishBindSig() (binary.FuncDesc, abi.Resolver) {
	tbl := &typeTable{}
	selfRef := tbl.add(binary.BorrowDesc{ResourceType: wasiUDPSocketResType})
	errRef := wasiSocketsErrorCodeType(tbl)
	resultRef := tbl.add(binary.ResultDesc{Err: &errRef})
	fd := binary.FuncDesc{
		Params:  []binary.FuncParam{{Name: "self", Type: selfRef}},
		Results: binary.FuncResults{Unnamed: &resultRef},
	}
	return fd, tbl.resolver()
}

// wasiUDPStreamSig builds the FuncDesc/resolver for
// [method]udp-socket.%stream(self: borrow<udp-socket>, remote-address:
// option<ip-socket-address>) -> result<tuple<own<incoming-datagram-stream>,
// own<outgoing-datagram-stream>>, error-code>.
func wasiUDPStreamSig() (binary.FuncDesc, abi.Resolver) {
	tbl := &typeTable{}
	selfRef := tbl.add(binary.BorrowDesc{ResourceType: wasiUDPSocketResType})
	addrRef := wasiIPSocketAddressType(tbl)
	optAddrRef := tbl.add(binary.OptionDesc{Element: addrRef})
	inRef := tbl.add(binary.OwnDesc{ResourceType: wasiIncomingDatagramStreamResType})
	outRef := tbl.add(binary.OwnDesc{ResourceType: wasiOutgoingDatagramStreamResType})
	tupleRef := tbl.add(binary.TupleDesc{Elements: []binary.TypeRef{inRef, outRef}})
	errRef := wasiSocketsErrorCodeType(tbl)
	resultRef := tbl.add(binary.ResultDesc{Ok: &tupleRef, Err: &errRef})
	fd := binary.FuncDesc{
		Params: []binary.FuncParam{
			{Name: "self", Type: selfRef},
			{Name: "remote-address", Type: optAddrRef},
		},
		Results: binary.FuncResults{Unnamed: &resultRef},
	}
	return fd, tbl.resolver()
}

// wasiIncomingReceiveSig builds the FuncDesc/resolver for
// [method]incoming-datagram-stream.receive(self:
// borrow<incoming-datagram-stream>, max-results: u64) ->
// result<list<incoming-datagram>, error-code>.
func wasiIncomingReceiveSig() (binary.FuncDesc, abi.Resolver) {
	tbl := &typeTable{}
	selfRef := tbl.add(binary.BorrowDesc{ResourceType: wasiIncomingDatagramStreamResType})
	datagramRef := wasiIncomingDatagramType(tbl)
	listRef := tbl.add(binary.ListDesc{Element: datagramRef})
	errRef := wasiSocketsErrorCodeType(tbl)
	resultRef := tbl.add(binary.ResultDesc{Ok: &listRef, Err: &errRef})
	fd := binary.FuncDesc{
		Params: []binary.FuncParam{
			{Name: "self", Type: selfRef},
			{Name: "max-results", Type: binary.TypeRef{Primitive: "u64"}},
		},
		Results: binary.FuncResults{Unnamed: &resultRef},
	}
	return fd, tbl.resolver()
}

// wasiOutgoingCheckSendSig builds the FuncDesc/resolver for
// [method]outgoing-datagram-stream.check-send(self:
// borrow<outgoing-datagram-stream>) -> result<u64, error-code>.
func wasiOutgoingCheckSendSig() (binary.FuncDesc, abi.Resolver) {
	tbl := &typeTable{}
	selfRef := tbl.add(binary.BorrowDesc{ResourceType: wasiOutgoingDatagramStreamResType})
	errRef := wasiSocketsErrorCodeType(tbl)
	okRef := binary.TypeRef{Primitive: "u64"}
	resultRef := tbl.add(binary.ResultDesc{Ok: &okRef, Err: &errRef})
	fd := binary.FuncDesc{
		Params:  []binary.FuncParam{{Name: "self", Type: selfRef}},
		Results: binary.FuncResults{Unnamed: &resultRef},
	}
	return fd, tbl.resolver()
}

// wasiOutgoingSendSig builds the FuncDesc/resolver for
// [method]outgoing-datagram-stream.send(self:
// borrow<outgoing-datagram-stream>, datagrams: list<outgoing-datagram>) ->
// result<u64, error-code>.
func wasiOutgoingSendSig() (binary.FuncDesc, abi.Resolver) {
	tbl := &typeTable{}
	selfRef := tbl.add(binary.BorrowDesc{ResourceType: wasiOutgoingDatagramStreamResType})
	datagramRef := wasiOutgoingDatagramType(tbl)
	datagramsRef := tbl.add(binary.ListDesc{Element: datagramRef})
	errRef := wasiSocketsErrorCodeType(tbl)
	okRef := binary.TypeRef{Primitive: "u64"}
	resultRef := tbl.add(binary.ResultDesc{Ok: &okRef, Err: &errRef})
	fd := binary.FuncDesc{
		Params: []binary.FuncParam{
			{Name: "self", Type: selfRef},
			{Name: "datagrams", Type: datagramsRef},
		},
		Results: binary.FuncResults{Unnamed: &resultRef},
	}
	return fd, tbl.resolver()
}
