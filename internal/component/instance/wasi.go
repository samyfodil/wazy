package instance

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/samyfodil/wazy/internal/component/abi"
	"github.com/samyfodil/wazy/internal/component/binary"
)

// This file implements a host WASI 0.2 ("wasip2") surface sufficient to run
// a real rustc wasm32-wasip2 `wasi:cli/command` guest -- see
// testdata/real_hello.component.wasm and real_hello_test.go's
// TestRealHello_PrintsHelloWorld, which is the milestone proof: a genuine,
// off-the-shelf component prints "hello world" by executing real guest code
// (println! -> the preview1-to-preview2 adapter's fd_write -> here).
//
// # Scope
//
// WithWASI registers real implementations for exactly the WASI imports a
// wasi:cli/command world's critical stdio path needs:
//
//   - wasi:cli/stdout.get-stdout, wasi:cli/stderr.get-stderr: mint an
//     own<output-stream> handle (via the M4.1 handle table, resource.go)
//     whose host rep is one of two fixed constants (wasiStdoutRep/
//     wasiStderrRep) identifying which configured io.Writer a later write
//     resolves to. There is exactly one logical stdout stream and one
//     logical stderr stream per Instance, so unlike the resource-scoped
//     `output-stream` type at the WIT level, nothing here needs a
//     dynamically-allocated rep pool.
//   - wasi:cli/stdin.get-stdin: mint an own<input-stream> handle over the
//     entirety of WASIConfig.Stdin (read once, up front), reusing wasi_fs.go's
//     fsStreamNode/[method]input-stream.{read,blocking-read} machinery --
//     the same rep-resolution and EOF (stream-error::closed) path a file's
//     read-via-stream stream goes through, not a separate implementation. A
//     nil Stdin behaves as an always-empty stream (immediate EOF on the
//     first read).
//   - wasi:io/streams [method]output-stream.{check-write,write,
//     blocking-write-and-flush,blocking-flush}: resolve the borrow<
//     output-stream> self handle back to its rep, then read/write against
//     the configured Writer. write and blocking-write-and-flush share one
//     implementation (this host has no internal buffering to distinguish
//     "written" from "written and flushed"); blocking-flush is a no-op
//     success; check-write always reports a large budget (2^40 bytes),
//     since there is no real backpressure to model against a Go io.Writer.
//   - wasi:cli/exit.exit, wasi:cli/environment.{get-environment,
//     get-arguments}, wasi:filesystem/preopens.get-directories: real,
//     WIT-correct implementations, but exit always fails the call (see
//     wasiExit's doc) since wazy has no process to actually terminate, and
//     get-environment/get-arguments/get-directories return whatever
//     WASIConfig.Env/Args hold (empty by default) / an empty list (no
//     preopened directories) respectively -- these are not on run()'s stdio
//     path but real_hello's WASICalls (see graph.go) shows the CLI adapter's
//     startup does invoke get-environment/get-directories, so they must
//     behave correctly, not just instantiate; real_args.component.wasm (see
//     real_args_test.go) additionally calls get-arguments to echo argv.
//   - wasi:random/random.get-random-bytes: real randomness from
//     crypto/rand -- discovered via conformance_test.go's f05_collections
//     fixture, whose std::collections::HashMap construction reaches this
//     through wasi_snapshot_preview1's random_get (see getRandomBytes's
//     doc for why a fake/deterministic source would be the wrong fix).
//
// get-directories, in turn, returns a real preopened root descriptor
// ("/") backed by WASIConfig.FS, and wasi_fs.go registers real
// implementations for the wasi:filesystem/types + wasi:io/streams
// input-stream + wasi:cli/terminal-* funcs a real guest's
// std::fs::read_to_string reaches once it does -- see wasi_fs.go's package
// doc for the exact discovered call list and why nested own<T> handles
// (e.g. open-at's result<descriptor,error-code>) need special handling
// beyond this file's top-level-only own<T>/borrow<T> plumbing.
//
// Still deliberately left unregistered are wasi:filesystem/types'
// write-via-stream and append-via-stream (no guest fixture this package
// runs ever calls them -- read_to_string's read-only path doesn't need
// them) and wasi:sockets: the graph engine's own automatic
// trap-stub fallback (buildCanonHostModule in graph.go, using the real
// guest module's own declared core-level import type as the trap stub's
// signature) already satisfies "instantiable, but fails loud if actually
// invoked" for these -- reimplementing that here would just be a second,
// redundant copy of the same mechanism.
//
// # Nested WIT types
//
// buildHostWrapper's normal path (synthFuncDesc, in host_import.go) can only
// express a top-level param/result type list, one table slot per entry --
// it cannot represent a genuinely nested composite type, e.g.
// list<tuple<string,string>> (wasi:cli/environment's get-environment
// result), where the tuple itself needs its own resolvable type index
// distinct from its list's. Six of the funcs registered here need exactly
// that (the stream-error variant used throughout wasi:io/streams, and the
// two list<tuple<...>> results), so this file builds their binary.FuncDesc
// and abi.Resolver directly with typeTable (below) and registers them via
// withImportCustom (host_import.go) instead of the public WithImport.
// get-arguments' list<string> result, by contrast, has no nested composite
// (its element is a bare primitive TypeRef, embeddable inline) and so is
// registered through the public WithImport below like any ordinary import,
// exercising the same list/string lowering through synthFuncDesc's simpler
// path instead.

// Resource type tags this file's handle table entries are minted under --
// see resource.go's handleTable. These are opaque to the guest and only
// need to be used consistently between the func that mints a handle and the
// func(s) that later resolve one back to a rep (mirroring
// outputStreamResourceType's role in stdout_test.go).
const (
	wasiOutputStreamResType uint32 = 1
	wasiInputStreamResType  uint32 = 2
	wasiErrorResType        uint32 = 3
	wasiDescriptorResType   uint32 = 4
)

// wasiArgv0 is the synthetic argv[0] (program name) wasi:cli/environment.
// get-arguments prepends ahead of WASIConfig.Args -- see getArguments's doc.
// wazy has no real process/binary path to report, and no observed guest
// behavior (real_args.component.wasm included) inspects its value, only its
// presence as a slot to skip.
const wasiArgv0 = "wazy"

// Fixed host-side reps for the two output-stream instances WithWASI
// supports. Unlike a general resource (which can have arbitrarily many live
// instances), there is exactly one logical stdout and one logical stderr
// stream per Instance, so a single constant rep per stream -- rather than a
// dynamically-allocated pool -- is enough: every get-stdout call mints a new
// *handle* (via resources.NewOwn), but every such handle always names the
// same rep, and every write against it resolves to the same configured
// io.Writer.
const (
	wasiStdoutRep uint32 = 1
	wasiStderrRep uint32 = 2
)

// WASI 0.2 interface names, exactly as they appear in real_hello's decoded
// imports (see TestRealHello_RunReachesWASI's logged WASICalls) -- these are
// the "iface" argument WithImport/withImportCustom key their registration
// under, and must match byte-for-byte or the graph engine reports "no host
// implementation provided" and falls back to a trap stub.
const (
	wasiIfaceStderr      = "wasi:cli/stderr@0.2.3"
	wasiIfaceStdin       = "wasi:cli/stdin@0.2.3"
	wasiIfaceStdout      = "wasi:cli/stdout@0.2.3"
	wasiIfaceExit        = "wasi:cli/exit@0.2.3"
	wasiIfaceEnvironment = "wasi:cli/environment@0.2.3"
	wasiIfaceStreams     = "wasi:io/streams@0.2.3"
	wasiIfacePreopens    = "wasi:filesystem/preopens@0.2.3"

	// Added for real filesystem I/O (see wasi_fs.go).
	wasiIfaceFilesystemTypes = "wasi:filesystem/types@0.2.3"
	wasiIfaceTerminalStdin   = "wasi:cli/terminal-stdin@0.2.3"
	wasiIfaceTerminalStdout  = "wasi:cli/terminal-stdout@0.2.3"
	wasiIfaceTerminalStderr  = "wasi:cli/terminal-stderr@0.2.3"
	wasiIfaceError           = "wasi:io/error@0.2.3"

	// Added for wasi:random -- see getRandomBytes's doc.
	wasiIfaceRandom = "wasi:random/random@0.2.3"
)

// WASIConfig configures the WASI 0.2 host implementation WithWASI builds.
// Every field is optional: a nil Stdout/Stderr discards writes (io.Discard),
// a nil Stdin yields an always-empty input-stream, and a nil/empty Env
// yields an empty wasi:cli/environment.get-environment result.
type WASIConfig struct {
	Stdout io.Writer
	Stderr io.Writer
	Stdin  io.Reader

	// Env holds "KEY=VALUE" pairs (matching os.Environ()'s format) returned
	// by get-environment, split into the WIT list<tuple<string,string>>
	// shape. A malformed entry (no "=") is skipped rather than failing the
	// whole call. Order is preserved (get-environment lowers Env in order).
	Env []string

	// Args holds the command-line arguments, NOT including argv[0] (the
	// program name): wasi:cli/environment's get-arguments prepends a fixed
	// synthetic argv[0] (wasiArgv0) ahead of Args, matching the
	// wasi_snapshot_preview1 args_get convention that argv[0] is the program
	// name -- so a guest that does std::env::args().skip(1) (as
	// real_args.component.wasm does) sees exactly Args, in order, lowered
	// into the WIT list<string> shape.
	Args []string

	// FS backs the single preopened root directory ("/") wasi:filesystem/
	// preopens.get-directories returns -- see wasi_fs.go. Keys are full
	// virtual paths (e.g. "/greeting.txt", matching what a guest's
	// std::fs::read_to_string("/greeting.txt") resolves to internally: its
	// path relative to the "/" preopen, i.e. "greeting.txt", joined back
	// onto "/"); values are that file's contents. An empty (but non-nil) FS
	// is a valid, empty filesystem: every open-at without the create flag
	// fails with error-code::no-entry, exactly as a real empty directory
	// would.
	//
	// A guest that writes a file (e.g. std::fs::write) mutates this same
	// map in place -- open-at(create) adds the new entry, and every
	// subsequent write commits straight into it (see wasi_fs.go's
	// fsFileSet) -- so a caller that passes a non-nil map here can read the
	// written file straight back out of that same map after the call
	// returns, no extra plumbing needed. A nil FS cannot be written back to
	// (there is no map for the guest's writes to land in that the caller
	// could later observe): wasi_fs.go lazily allocates its own internal
	// map in that case so create/write still succeed within the run, but a
	// caller that wants to see what a guest wrote must pass a non-nil
	// (possibly empty) map instead of nil.
	FS map[string][]byte

	// AllowTCP opts into a real wasi:sockets (TCP-only) + wasi:io/poll host
	// implementation -- see wasi_sockets.go's package doc. False (the
	// default) leaves wasi:sockets/wasi:io/poll entirely unregistered, so
	// the graph engine's own automatic trap-stub fallback fails loud,
	// naming the specific iface+func, the moment a guest actually calls
	// into sockets (mirrors wasi.go's pre-existing doc on why
	// wasi:sockets was left unregistered before this field existed).
	// AllowTCP must be true for Dialer to have any effect.
	AllowTCP bool

	// Dialer, when non-nil, replaces the real net.Dial WithWASI otherwise
	// uses to satisfy a guest's wasi:sockets/tcp
	// [method]tcp-socket.start-connect -- see wasi_sockets.go's
	// startConnect. A test that wants a real TCP exchange against a
	// scratch server it controls (rather than genuine outbound network
	// access) injects a Dialer that ignores the guest-requested address
	// and always dials that server (or one that enforces loopback-only
	// addresses), without needing any change to wasi_sockets.go itself.
	// Has no effect unless AllowTCP is also true.
	Dialer func(network, address string) (net.Conn, error)

	// Listen, when non-nil, replaces the real net.Listen WithWASI otherwise
	// uses to satisfy a guest's wasi:sockets/tcp [method]tcp-socket.start-bind
	// on a socket the guest then listens on -- see wasi_sockets.go's
	// tcpStartBind/tcpAccept. Mirrors Dialer's role but for the server
	// direction: a test that wants to drive a real listening guest connects to
	// the net.Listener this returns (its Addr reveals the bound ephemeral
	// port). Has no effect unless AllowTCP is also true.
	Listen func(network, address string) (net.Listener, error)

	// AllowUDP opts into a real wasi:sockets (UDP-only) + wasi:io/poll host
	// implementation -- see wasi_sockets.go's package doc's UDP section.
	// False (the default) leaves wasi:sockets/udp*
	// entirely unregistered, so the graph engine's own automatic trap-stub
	// fallback fails loud, naming the specific iface+func, the moment a
	// guest actually calls into UDP sockets. Independent of AllowTCP: a
	// caller may enable one, both, or neither.
	AllowUDP bool

	// ListenPacket, when non-nil, replaces the real net.ListenPacket
	// WithWASI otherwise uses to satisfy a guest's wasi:sockets/udp
	// [method]udp-socket.start-bind -- see wasi_sockets.go's udpStartBind.
	// Mirrors Dialer's role for TCP: a test that wants a real UDP exchange
	// against a scratch server it controls can inject a ListenPacket that
	// enforces loopback-only binds, without needing any change to
	// wasi_sockets.go itself. Has no effect unless AllowUDP is also true.
	ListenPacket func(network, address string) (net.PacketConn, error)

	// EnableHTTP, when true, registers the wasi:http/types host functions a
	// component that EXPORTS wasi:http/incoming-handler needs (see
	// wasi_http.go). The guest is then driven via (*Instance).ServeHTTP, which
	// synthesizes each inbound request's resources and calls the guest's
	// exported handle. False (the default) leaves wasi:http unregistered, so a
	// non-http component is completely unaffected.
	EnableHTTP bool

	// HTTPClient, when non-nil, is the client wasi:http/outgoing-handler.handle
	// dispatches a guest's outbound requests through. Nil uses
	// http.DefaultClient. A test can inject one whose Transport reaches a
	// scratch backend. Has no effect unless EnableHTTP is also true.
	HTTPClient *http.Client

	// WallClock, when non-nil, is the source wasi:clocks/wall-clock.now reads
	// the current time from. Nil uses time.Now. It is the one injectable clock
	// surface (monotonic-clock stays real so std::thread::sleep genuinely
	// elapses -- see wasi_poll.go): a test pins WallClock to a fixed instant to
	// assert a guest's printed wall time deterministically.
	WallClock func() time.Time
}

// WithWASI returns the Options that register a WASI 0.2 host implementation
// sufficient to run a wasi:cli/command guest's stdio path -- see this file's
// package doc for exactly which funcs are implemented for real versus left
// to the graph engine's automatic trap-stub fallback.
func WithWASI(cfg WASIConfig) []Option {
	stdout := cfg.Stdout
	if stdout == nil {
		stdout = io.Discard
	}
	stderr := cfg.Stderr
	if stderr == nil {
		stderr = io.Discard
	}

	// fs is shared with wasi_fs.go's wasiFilesystemOptions: output-stream is
	// one resource/handle namespace spanning both the two fixed stdio reps
	// below and the dynamically-minted write-via-stream/append-via-stream
	// reps wasi_fs.go's descriptor methods hand out (see writeSink's doc),
	// so both halves of that dispatch need the same *wasiFS; get-stdin
	// (below) also mints its input-stream reps through this same fs, reusing
	// wasi_fs.go's fsStreamNode/streamNode/streamRead machinery (the exact
	// path [method]descriptor.read-via-stream uses for file reads) instead
	// of a separate stdin-only implementation.
	fs := newWasiFS(cfg.FS)

	// sockets backs a real wasi:sockets (TCP-only) + wasi:io/poll
	// implementation (wasi_sockets.go), gated behind cfg.AllowTCP (see
	// WASIConfig.AllowTCP's doc). It is always constructed -- even when
	// AllowTCP is false -- so writeSink/checkWrite/blockingFlush/
	// wasiFilesystemOptions' streamRead (below) can unconditionally
	// consult it as a dispatch fallback without a nil check; when AllowTCP
	// is false, wasiSocketOptions is never called, so sockets.tcpSocks/
	// inStreams/outStreams simply stay empty for the run's whole lifetime
	// and every fallback lookup reports "not found", falling through to
	// each dispatch's existing "unknown rep" error exactly as before this
	// field existed.
	dial := cfg.Dialer
	if dial == nil {
		dial = func(network, address string) (net.Conn, error) { return net.Dial(network, address) }
	}
	listenPacket := cfg.ListenPacket
	if listenPacket == nil {
		listenPacket = func(network, address string) (net.PacketConn, error) { return net.ListenPacket(network, address) }
	}
	listen := cfg.Listen
	if listen == nil {
		listen = func(network, address string) (net.Listener, error) { return net.Listen(network, address) }
	}
	sockets := newWasiSockets(dial, listenPacket, listen)

	wallClock := cfg.WallClock
	if wallClock == nil {
		wallClock = time.Now
	}
	pollHost := newWasiPoll(wallClock)

	// httphost backs wasi:http server support (wasi_http.go), gated behind
	// cfg.EnableHTTP. Always constructed so writeSink/checkWrite/blockingFlush
	// can consult its outgoing-body output-streams as a dispatch fallback
	// without a nil check; when EnableHTTP is false, wasiHTTPOptions is never
	// added, so its bodyStreams map simply stays empty and every fallback
	// lookup reports "not found" -- exactly as before this existed.
	httphost := newWasiHTTP()

	// stdinBytes is the entirety of WASIConfig.Stdin, read once up front
	// (mirrors read-via-stream's own model: an fsDescNode's content is a
	// fully in-memory byte slice a stream then serves via a pos cursor --
	// see fsStreamNode's doc). A real WASI stdin is a live, potentially
	// unbounded stream, but every conformance fixture that reads stdin
	// (f20_cat/f21_wc/f22_grep/f23_upper) is invoked with a fixed, already-
	// fully-available byte string (WASIConfig.Stdin is a bytes.Reader over
	// it in every caller), so eager slurp is both correct for those and
	// consistent with the rest of this package's "no real I/O to actually
	// block on" design (see streamRead's doc). A nil Stdin reads as an
	// always-empty stream (io.ReadAll(nil-typed io.Reader) would panic, so
	// this guards explicitly, matching WASIConfig.Stdin's doc).
	var (
		stdinBytes   []byte
		stdinReadErr error
	)
	if cfg.Stdin != nil {
		// Recorded, not swallowed: surfaced the first time get-stdin is
		// actually called (below), so a guest that never touches stdin is
		// unaffected by a bad Reader.
		stdinBytes, stdinReadErr = io.ReadAll(cfg.Stdin)
	}

	writerForRep := func(rep uint32) (io.Writer, error) {
		switch rep {
		case wasiStdoutRep:
			return stdout, nil
		case wasiStderrRep:
			return stderr, nil
		default:
			return nil, fmt.Errorf("wasi:io/streams: output-stream rep %d does not name a stdout/stderr stream", rep)
		}
	}

	getStderr := func(context.Context, []abi.Value) ([]abi.Value, error) {
		return []abi.Value{wasiStderrRep}, nil
	}
	getStdout := func(context.Context, []abi.Value) ([]abi.Value, error) {
		return []abi.Value{wasiStdoutRep}, nil
	}
	getStdin := func(context.Context, []abi.Value) ([]abi.Value, error) {
		if stdinReadErr != nil {
			return nil, fmt.Errorf("wasi:cli/stdin.get-stdin: reading WASIConfig.Stdin: %w", stdinReadErr)
		}
		// Mint a real fsStreamNode over the fully-read stdin bytes, exactly
		// as [method]descriptor.read-via-stream does for a file's content
		// (wasi_fs.go) -- the rep this returns is then resolved by the very
		// same [method]input-stream.{read,blocking-read} registered in
		// wasiFilesystemOptions (both dispatch through fs.streamNode/
		// fs.streams), so EOF (stream-error::closed, once pos reaches
		// len(data)) and chunked reads work identically to a file-backed
		// stream with no separate implementation. This func is registered
		// via the plain WithImport path (not withImportCustom), so
		// allocHandleResult (host_import.go) auto-wraps the returned bare
		// rep into a real guest own<input-stream> handle -- mirrors
		// getStdout/getStderr returning their own fixed reps the same way.
		rep := fs.newStreamRep(&fsStreamNode{data: stdinBytes})
		return []abi.Value{rep}, nil
	}

	exit := func(_ context.Context, args []abi.Value) ([]abi.Value, error) {
		rv, ok := args[0].(abi.ResultValue)
		if !ok {
			return nil, fmt.Errorf("wasi:cli/exit.exit: expected a result<_,_> arg, got %T", args[0])
		}
		if rv.IsErr {
			return nil, fmt.Errorf("wasi:cli/exit.exit: guest called exit with an error status")
		}
		// wazy has no host process for a successful exit() to terminate, so
		// this aborts the originating Call with a specific, named error
		// rather than either silently continuing (wrong: the guest asked to
		// stop) or panicking the host.
		return nil, fmt.Errorf("wasi:cli/exit.exit: guest called exit(ok); wazy has no process to exit")
	}

	getEnvironment := func(context.Context, []abi.Value) ([]abi.Value, error) {
		pairs := make([]abi.Value, 0, len(cfg.Env))
		for _, kv := range cfg.Env {
			k, v, ok := strings.Cut(kv, "=")
			if !ok {
				continue
			}
			pairs = append(pairs, []abi.Value{k, v})
		}
		return []abi.Value{pairs}, nil
	}

	getArguments := func(context.Context, []abi.Value) ([]abi.Value, error) {
		// wasi:cli/environment.get-arguments returns the full argv, per the
		// wasi_snapshot_preview1 args_get convention argv[0] carries over
		// from: element 0 is the program name, and a guest following the
		// Unix convention (e.g. Rust's std::env::args().skip(1), which is
		// exactly what real_args.component.wasm does) skips it to get the
		// real arguments. WASIConfig.Args holds only those real arguments
		// (argv[1:]), so wasiArgv0 is prepended here to give guests that
		// convention something to skip.
		args := make([]abi.Value, 0, len(cfg.Args)+1)
		args = append(args, wasiArgv0)
		for _, a := range cfg.Args {
			args = append(args, a)
		}
		return []abi.Value{args}, nil
	}

	// getRandomBytes implements wasi:random/random.get-random-bytes(len:
	// u64) -> list<u8>, real (non-deterministic) randomness from
	// crypto/rand -- a discovered dependency, not a stdio/run() path func:
	// Rust's std::collections::HashMap seeds its SipHash keys by calling
	// this (via wasi_snapshot_preview1's random_get -> the preview1-to-
	// preview2 adapter) the first time a HashMap is constructed, even
	// though no guest fixture ever prints the random bytes themselves --
	// only their effect (an unpredictable but internally-consistent
	// iteration order, which a real program must not rely on; every
	// fixture that uses a HashMap sorts keys before printing precisely
	// because get-random-bytes' output is never meant to be
	// deterministic). A fixed/deterministic source would therefore satisfy
	// conformance today, but would misrepresent wasi:random/random as
	// something wazy can only fake; crypto/rand is the genuine
	// implementation.
	getRandomBytes := func(_ context.Context, args []abi.Value) ([]abi.Value, error) {
		if len(args) != 1 {
			return nil, fmt.Errorf("wasi:random/random.get-random-bytes: expected 1 arg (len), got %d", len(args))
		}
		n, ok := args[0].(uint64)
		if !ok {
			return nil, fmt.Errorf("wasi:random/random.get-random-bytes: len: expected uint64, got %T", args[0])
		}
		buf := make([]byte, n)
		if _, err := rand.Read(buf); err != nil {
			return nil, fmt.Errorf("wasi:random/random.get-random-bytes: %w", err)
		}
		out := make([]abi.Value, len(buf))
		for i, b := range buf {
			out[i] = uint32(b)
		}
		return []abi.Value{out}, nil
	}

	// writeSink resolves an output-stream rep to "how to write buf against
	// it": a stdio io.Writer (writerForRep), one of wasi_fs.go's file-write
	// streams (fs.writeStreamWrite), or -- when WASIConfig.AllowTCP is set
	// -- a socket-backed stream (sockets.outStreamNode, wasi_sockets.go).
	// The three rep ranges never collide (see newWasiFS's doc for
	// stdio/fs, and wasi_sockets.go's package doc's "Rep numbering"
	// section for why sockets' reps start at a disjoint 1<<20 base), so
	// trying them in order is unambiguous. A rep none of the three
	// recognizes is a genuinely unknown output-stream handle;
	// writerForRep's own "does not name a stdout/stderr stream" error is
	// returned for that case (matching checkWrite/blockingFlush's identical
	// fallback below) rather than fs's/sockets' differently-worded "not
	// found" errors, so all three output-stream methods report an unknown
	// rep the same way.
	writeSink := func(rep uint32, buf []byte) error {
		w, werr := writerForRep(rep)
		if werr == nil {
			_, err := w.Write(buf)
			return err
		}
		if _, found := fs.writeStreamNode(rep); found {
			return fs.writeStreamWrite(rep, buf)
		}
		if s, found := sockets.outStreamNode(rep); found {
			return s.write(buf)
		}
		if found, err := httphost.bodyStreamWrite(rep, buf); found {
			return err
		}
		return werr
	}

	checkWrite := func(_ context.Context, args []abi.Value) ([]abi.Value, error) {
		if len(args) != 1 {
			return nil, fmt.Errorf("[method]output-stream.check-write: expected 1 arg (self), got %d", len(args))
		}
		rep, ok := args[0].(uint32)
		if !ok {
			return nil, fmt.Errorf("[method]output-stream.check-write: self: expected uint32 rep, got %T", args[0])
		}
		if _, err := writerForRep(rep); err != nil {
			if _, found := fs.writeStreamNode(rep); !found {
				if _, found := sockets.outStreamNode(rep); !found {
					if !httphost.isBodyStreamRep(rep) {
						return nil, err
					}
				}
			}
		}
		// A large, fixed budget: there is no real backpressure to model
		// against a Go io.Writer, an in-memory file, or a net.Conn, so this
		// never has to make the guest wait.
		return []abi.Value{abi.ResultValue{IsErr: false, Payload: uint64(1) << 40}}, nil
	}

	write := func(_ context.Context, args []abi.Value) ([]abi.Value, error) {
		if len(args) != 2 {
			return nil, fmt.Errorf("[method]output-stream.write: expected 2 args (self, contents), got %d", len(args))
		}
		rep, ok := args[0].(uint32)
		if !ok {
			return nil, fmt.Errorf("[method]output-stream.write: self: expected uint32 rep, got %T", args[0])
		}
		buf, err := wasiBytesFromList(args[1])
		if err != nil {
			return nil, fmt.Errorf("[method]output-stream.write: contents: %w", err)
		}
		if err := writeSink(rep, buf); err != nil {
			return nil, fmt.Errorf("[method]output-stream.write: %w", err)
		}
		return []abi.Value{abi.ResultValue{IsErr: false, Payload: nil}}, nil
	}

	blockingFlush := func(_ context.Context, args []abi.Value) ([]abi.Value, error) {
		if len(args) != 1 {
			return nil, fmt.Errorf("[method]output-stream.blocking-flush: expected 1 arg (self), got %d", len(args))
		}
		rep, ok := args[0].(uint32)
		if !ok {
			return nil, fmt.Errorf("[method]output-stream.blocking-flush: self: expected uint32 rep, got %T", args[0])
		}
		if _, err := writerForRep(rep); err != nil {
			// No internal buffering on any side (stdio writes straight
			// through to the configured io.Writer; fs writes commit
			// straight into fs.files -- see writeStreamWrite's doc; socket
			// writes are unbuffered net.Conn.Write syscalls -- see
			// sockOutStream.write's doc), so flushing has nothing to do
			// beyond confirming rep actually names a live stream.
			if _, found := fs.writeStreamNode(rep); !found {
				if _, found := sockets.outStreamNode(rep); !found {
					return nil, err
				}
			}
		}
		return []abi.Value{abi.ResultValue{IsErr: false, Payload: nil}}, nil
	}

	checkWriteFD, checkWriteResolve := wasiCheckWriteSig()
	writeFD, writeResolve := wasiWriteSig()
	blockingFlushFD, blockingFlushResolve := wasiBlockingFlushSig()
	envFD, envResolve := wasiGetEnvironmentSig()

	opts := []Option{
		WithImport(wasiIfaceStderr, "get-stderr", getStderr,
			nil, []binary.TypeDesc{binary.OwnDesc{ResourceType: wasiOutputStreamResType}}),
		WithImport(wasiIfaceStdin, "get-stdin", getStdin,
			nil, []binary.TypeDesc{binary.OwnDesc{ResourceType: wasiInputStreamResType}}),
		WithImport(wasiIfaceStdout, "get-stdout", getStdout,
			nil, []binary.TypeDesc{binary.OwnDesc{ResourceType: wasiOutputStreamResType}}),
		WithImport(wasiIfaceExit, "exit", exit,
			[]binary.TypeDesc{binary.ResultDesc{}}, nil),

		WithImport(wasiIfaceEnvironment, "get-arguments", getArguments,
			nil, []binary.TypeDesc{binary.ListDesc{Element: binary.TypeRef{Primitive: "string"}}}),

		withImportCustom(wasiIfaceEnvironment, "get-environment", getEnvironment, envFD, envResolve),

		WithImport(wasiIfaceRandom, "get-random-bytes", getRandomBytes,
			[]binary.TypeDesc{binary.PrimitiveDesc{Prim: "u64"}},
			[]binary.TypeDesc{binary.ListDesc{Element: binary.TypeRef{Primitive: "u8"}}}),

		withImportCustom(wasiIfaceStreams, "[method]output-stream.check-write", checkWrite, checkWriteFD, checkWriteResolve),
		withImportCustom(wasiIfaceStreams, "[method]output-stream.write", write, writeFD, writeResolve),
		withImportCustom(wasiIfaceStreams, "[method]output-stream.blocking-write-and-flush", write, writeFD, writeResolve),
		withImportCustom(wasiIfaceStreams, "[method]output-stream.blocking-flush", blockingFlush, blockingFlushFD, blockingFlushResolve),
	}
	// wasi:io/poll (timer-aware block/poll + the pollable resource tag) and
	// wasi:clocks are registered unconditionally: they are shared by sockets,
	// http, and clocks alike, so one central timer-aware implementation
	// (wasi_poll.go) replaces the former per-interface no-op copies. Harmless
	// when a guest imports none of them (the host funcs simply go uncalled).
	opts = append(opts, wasiClockPollOptions(pollHost)...)
	opts = append(opts, wasiFilesystemOptions(fs, sockets)...)
	if cfg.AllowTCP {
		opts = append(opts, wasiSocketOptions(sockets)...)
	}
	if cfg.AllowUDP {
		opts = append(opts, wasiUDPSocketOptions(sockets)...)
	}
	if cfg.EnableHTTP {
		httphost.client = cfg.HTTPClient
		// incoming-body.stream reuses the fs input-stream path: mint an
		// fsStreamNode over the response bytes, served by the already-registered
		// [method]input-stream.blocking-read (fs.streamRead), EOF included.
		httphost.newInputStreamRep = func(b []byte) uint32 {
			return fs.newStreamRep(&fsStreamNode{data: b})
		}
		opts = append(opts, wasiHTTPOptions(httphost)...)
		opts = append(opts, wasiHTTPOutgoingOptions(httphost)...)
	}
	return opts
}

// wasiBytesFromList converts a lifted list<u8> (see abi.Value's doc: list<T>
// -> []abi.Value, u8 -> uint32) into a []byte.
func wasiBytesFromList(v abi.Value) ([]byte, error) {
	list, ok := v.([]abi.Value)
	if !ok {
		return nil, fmt.Errorf("expected list<u8> ([]abi.Value), got %T", v)
	}
	buf := make([]byte, len(list))
	for i, b := range list {
		u, ok := b.(uint32)
		if !ok {
			return nil, fmt.Errorf("[%d]: expected uint32 (u8), got %T", i, b)
		}
		buf[i] = byte(u)
	}
	return buf, nil
}

// typeTable is a shared type-index table for building a binary.FuncDesc with
// genuinely nested composite types -- see this file's package doc ("Nested
// WIT types") for why synthFuncDesc's table (host_import.go) cannot express
// these. add appends td and returns the TypeRef that refers to it, except
// for a primitive, which is returned as a direct inline TypeRef needing no
// table entry (mirroring synthFuncDesc's mkRef).
type typeTable struct {
	entries []binary.TypeDesc
}

func (t *typeTable) add(td binary.TypeDesc) binary.TypeRef {
	if p, ok := td.(binary.PrimitiveDesc); ok {
		return binary.TypeRef{Primitive: p.Prim}
	}
	idx := uint32(len(t.entries))
	t.entries = append(t.entries, td)
	return binary.TypeRef{TypeIndex: &idx}
}

// resolver returns the abi.Resolver over t's current entries.
func (t *typeTable) resolver() abi.Resolver {
	return func(idx uint32) binary.TypeDesc {
		if int(idx) >= len(t.entries) {
			return nil
		}
		return t.entries[idx]
	}
}

// wasiStreamErrorType interns wasi:io/streams' `stream-error` variant --
// variant { last-operation-failed(error), closed } -- into tbl and returns
// its TypeRef. Shared by every output-stream method's result type. The
// last-operation-failed payload (the wasi:io/error `error` resource) is
// interned as own<error>, tagged wasiErrorResType; this implementation never
// actually constructs a stream-error::last-operation-failed value (every
// registered output-stream method always returns Ok), so no handle is ever
// minted under that tag -- it exists purely so the type structure resolves
// for Flatten (see abi.Flatten's variant case, which needs every case's
// payload type to compute the joined flat width).
func wasiStreamErrorType(tbl *typeTable) binary.TypeRef {
	errRef := tbl.add(binary.OwnDesc{ResourceType: wasiErrorResType})
	return tbl.add(binary.VariantDesc{Cases: []binary.VariantCase{
		{Name: "last-operation-failed", Type: &errRef},
		{Name: "closed"},
	}})
}

// wasiCheckWriteSig builds the FuncDesc/resolver for
// [method]output-stream.check-write(self: borrow<output-stream>) ->
// result<u64, stream-error>.
func wasiCheckWriteSig() (binary.FuncDesc, abi.Resolver) {
	tbl := &typeTable{}
	selfRef := tbl.add(binary.BorrowDesc{ResourceType: wasiOutputStreamResType})
	errRef := wasiStreamErrorType(tbl)
	okRef := binary.TypeRef{Primitive: "u64"}
	resultRef := tbl.add(binary.ResultDesc{Ok: &okRef, Err: &errRef})
	fd := binary.FuncDesc{
		Params:  []binary.FuncParam{{Name: "self", Type: selfRef}},
		Results: binary.FuncResults{Unnamed: &resultRef},
	}
	return fd, tbl.resolver()
}

// wasiWriteSig builds the FuncDesc/resolver for
// [method]output-stream.write(self: borrow<output-stream>, contents:
// list<u8>) -> result<_, stream-error> -- also reused as-is for
// blocking-write-and-flush, which has the identical WIT signature.
func wasiWriteSig() (binary.FuncDesc, abi.Resolver) {
	tbl := &typeTable{}
	selfRef := tbl.add(binary.BorrowDesc{ResourceType: wasiOutputStreamResType})
	contentsRef := tbl.add(binary.ListDesc{Element: binary.TypeRef{Primitive: "u8"}})
	errRef := wasiStreamErrorType(tbl)
	resultRef := tbl.add(binary.ResultDesc{Err: &errRef})
	fd := binary.FuncDesc{
		Params:  []binary.FuncParam{{Name: "self", Type: selfRef}, {Name: "contents", Type: contentsRef}},
		Results: binary.FuncResults{Unnamed: &resultRef},
	}
	return fd, tbl.resolver()
}

// wasiBlockingFlushSig builds the FuncDesc/resolver for
// [method]output-stream.blocking-flush(self: borrow<output-stream>) ->
// result<_, stream-error>.
func wasiBlockingFlushSig() (binary.FuncDesc, abi.Resolver) {
	tbl := &typeTable{}
	selfRef := tbl.add(binary.BorrowDesc{ResourceType: wasiOutputStreamResType})
	errRef := wasiStreamErrorType(tbl)
	resultRef := tbl.add(binary.ResultDesc{Err: &errRef})
	fd := binary.FuncDesc{
		Params:  []binary.FuncParam{{Name: "self", Type: selfRef}},
		Results: binary.FuncResults{Unnamed: &resultRef},
	}
	return fd, tbl.resolver()
}

// wasiGetEnvironmentSig builds the FuncDesc/resolver for
// wasi:cli/environment.get-environment() -> list<tuple<string,string>>.
func wasiGetEnvironmentSig() (binary.FuncDesc, abi.Resolver) {
	tbl := &typeTable{}
	tupleRef := tbl.add(binary.TupleDesc{Elements: []binary.TypeRef{
		{Primitive: "string"}, {Primitive: "string"},
	}})
	listRef := tbl.add(binary.ListDesc{Element: tupleRef})
	fd := binary.FuncDesc{Results: binary.FuncResults{Unnamed: &listRef}}
	return fd, tbl.resolver()
}

// wasiGetDirectoriesSig builds the FuncDesc/resolver for
// wasi:filesystem/preopens.get-directories() ->
// list<tuple<own<descriptor>,string>>.
func wasiGetDirectoriesSig() (binary.FuncDesc, abi.Resolver) {
	tbl := &typeTable{}
	descRef := tbl.add(binary.OwnDesc{ResourceType: wasiDescriptorResType})
	tupleRef := tbl.add(binary.TupleDesc{Elements: []binary.TypeRef{
		descRef, {Primitive: "string"},
	}})
	listRef := tbl.add(binary.ListDesc{Element: tupleRef})
	fd := binary.FuncDesc{Results: binary.FuncResults{Unnamed: &listRef}}
	return fd, tbl.resolver()
}

// withImportCustom is WithImport's counterpart for a signature that needs a
// hand-built FuncDesc/resolver (see hostImport's customFD doc) instead of
// the flat []binary.TypeDesc params/results WithImport's public API takes.
// Used only within this package by WithWASI.
func withImportCustom(iface, name string, fn HostFunc, fd binary.FuncDesc, resolve abi.Resolver) Option {
	return func(c *config) {
		c.imports[mkImportKey(iface, name)] = &hostImport{fn: fn, customFD: &fd, customResolve: resolve}
	}
}
