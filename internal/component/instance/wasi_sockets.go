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
	mu   sync.Mutex
	dial func(network, address string) (net.Conn, error)

	resources *handleTable

	nextRep    uint32
	tcpSocks   map[uint32]*tcpSockNode
	inStreams  map[uint32]*sockInStream
	outStreams map[uint32]*sockOutStream
}

// wasiSockRepBase is wasiSockets.nextRep's starting value -- see this
// file's package doc's "Rep numbering" section for why it must stay
// disjoint from wasi_fs.go's and wasi.go's own rep spaces.
const wasiSockRepBase uint32 = 1 << 20

// newWasiSockets returns a wasiSockets that dials through dial (never nil
// by the time WithWASI constructs one -- see its own doc).
func newWasiSockets(dial func(network, address string) (net.Conn, error)) *wasiSockets {
	return &wasiSockets{
		dial:       dial,
		tcpSocks:   make(map[uint32]*tcpSockNode),
		inStreams:  make(map[uint32]*sockInStream),
		outStreams: make(map[uint32]*sockOutStream),
		nextRep:    wasiSockRepBase,
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

	// pollableBlock implements [method]pollable.block(self:
	// borrow<pollable>) -> () -- self is already resolved/validated by
	// liftHostArgs; see this file's package doc's "Blocking, single-shot"
	// section for why every pollable this package mints is already ready,
	// making block() unconditionally an immediate no-op.
	pollableBlock := func(context.Context, []abi.Value) ([]abi.Value, error) {
		return nil, nil
	}

	// poll implements the free wasi:io/poll.poll(in: list<borrow<pollable>>)
	// -> list<u32> func -- WIT-complete even though this package's own
	// real_tcp fixture never calls it at runtime (see this file's package
	// doc). in's borrow<pollable> elements are nested inside a list, so
	// (like finish-connect's nested own<T> results) liftHostArgs/
	// resolveHandleArg never touches them -- this closure resolves each
	// handle to a rep itself, purely to validate it names a live pollable
	// (trap loud on a bogus handle, matching every other borrow<T>
	// resolution in this package), and reports every index ready: see
	// wasiPollableRep's doc for why there is no non-ready pollable this
	// package could ever mint.
	poll := func(_ context.Context, args []abi.Value) ([]abi.Value, error) {
		if len(args) != 1 {
			return nil, fmt.Errorf("wasi:io/poll.poll: expected 1 arg (in), got %d", len(args))
		}
		list, ok := args[0].([]abi.Value)
		if !ok {
			return nil, fmt.Errorf("wasi:io/poll.poll: in: expected list<borrow<pollable>> ([]abi.Value), got %T", args[0])
		}
		resources, err := sockets.getResources()
		if err != nil {
			return nil, err
		}
		out := make([]abi.Value, 0, len(list))
		for i, v := range list {
			h, ok := v.(uint32)
			if !ok {
				return nil, fmt.Errorf("wasi:io/poll.poll: in[%d]: expected uint32 handle, got %T", i, v)
			}
			if _, err := resources.Rep(wasiPollableResType, h); err != nil {
				return nil, fmt.Errorf("wasi:io/poll.poll: in[%d]: %w", i, err)
			}
			out = append(out, uint32(i))
		}
		return []abi.Value{out}, nil
	}

	instNetFD, instNetResolve := wasiInstanceNetworkSig()
	createTCPFD, createTCPResolve := wasiCreateTCPSocketSig()
	startConnectFD, startConnectResolve := wasiTCPStartConnectSig()
	finishConnectFD, finishConnectResolve := wasiTCPFinishConnectSig()
	tcpSubFD, tcpSubResolve := wasiSubscribeSig(wasiTCPSocketResType)
	inSubFD, inSubResolve := wasiSubscribeSig(wasiInputStreamResType)
	outSubFD, outSubResolve := wasiSubscribeSig(wasiOutputStreamResType)
	blockFD, blockResolve := wasiPollableBlockSig()
	pollFD, pollResolve := wasiPollSig()

	return []Option{
		withResourcesHook(sockets.setResources),

		// See withResourceTag's doc (host_import.go): without these, a
		// guest that drops an owned network/tcp-socket/pollable handle
		// trips the handle table's cross-type-confusion check, exactly the
		// same failure mode wasi_fs.go's own withResourceTag calls guard
		// against for descriptor/input-stream/output-stream/error.
		withResourceTag(wasiIfaceSocketsNetwork, "network", wasiNetworkResType),
		withResourceTag(wasiIfaceSocketsTCP, "tcp-socket", wasiTCPSocketResType),
		withResourceTag(wasiIfacePoll, "pollable", wasiPollableResType),

		withImportCustom(wasiIfaceSocketsInstanceNet, "instance-network", instanceNetwork, instNetFD, instNetResolve),
		withImportCustom(wasiIfaceSocketsTCPCreateSoc, "create-tcp-socket", createTCPSocket, createTCPFD, createTCPResolve),
		withImportCustom(wasiIfaceSocketsTCP, "[method]tcp-socket.start-connect", startConnect, startConnectFD, startConnectResolve),
		withImportCustom(wasiIfaceSocketsTCP, "[method]tcp-socket.finish-connect", finishConnect, finishConnectFD, finishConnectResolve),
		withImportCustom(wasiIfaceSocketsTCP, "[method]tcp-socket.subscribe", tcpSubscribe, tcpSubFD, tcpSubResolve),
		withImportCustom(wasiIfaceStreams, "[method]input-stream.subscribe", streamSubscribe, inSubFD, inSubResolve),
		withImportCustom(wasiIfaceStreams, "[method]output-stream.subscribe", streamSubscribe, outSubFD, outSubResolve),
		withImportCustom(wasiIfacePoll, "[method]pollable.block", pollableBlock, blockFD, blockResolve),
		withImportCustom(wasiIfacePoll, "poll", poll, pollFD, pollResolve),
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
