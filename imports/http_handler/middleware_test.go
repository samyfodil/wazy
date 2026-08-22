package http_handler

import (
	"context"
	"errors"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/imports/wasi_snapshot_preview1"
	"github.com/samyfodil/wazy/internal/testing/binaryencoding"
	"github.com/samyfodil/wazy/internal/testing/require"
	"github.com/samyfodil/wazy/internal/wasm"
)

var testCtx = context.Background()

// guestModule builds a module exporting handle_request, handle_response and a
// memory, any of which can be bent out of contract to prove compileGuest
// rejects it.
type guestParts struct {
	omitHandleRequest   bool
	omitHandleResponse  bool
	omitMemory          bool
	badHandleRequest    bool // (i32) -> () instead of () -> (i64)
	badHandleResponse   bool // () -> () instead of (i32, i32) -> ()
	importsHTTPHandler  bool
	importsWasiSnapshot bool
	unknownHostImport   bool // imports http_handler.not_a_func: links, then fails
	trapHandleRequest   bool
	trapHandleResponse  bool
}

func newGuestModule(p guestParts) []byte {
	m := &wasm.Module{}

	handleRequest := wasm.FunctionType{Results: []wasm.ValueType{wasm.ValueTypeI64}}
	if p.badHandleRequest {
		handleRequest = wasm.FunctionType{Params: []wasm.ValueType{wasm.ValueTypeI32}}
	}
	handleResponse := wasm.FunctionType{Params: []wasm.ValueType{wasm.ValueTypeI32, wasm.ValueTypeI32}}
	if p.badHandleResponse {
		handleResponse = wasm.FunctionType{}
	}

	// An imported function, when the test wants import detection to fire.
	if p.importsHTTPHandler {
		m.TypeSection = append(m.TypeSection, wasm.FunctionType{
			Params: []wasm.ValueType{wasm.ValueTypeI32}, Results: []wasm.ValueType{wasm.ValueTypeI32},
		})
		m.ImportSection = append(m.ImportSection, wasm.Import{
			Module: HostModule, Name: FuncEnableFeatures, Type: wasm.ExternTypeFunc,
			DescFunc: wasm.Index(len(m.TypeSection) - 1),
		})
	}
	if p.unknownHostImport {
		m.TypeSection = append(m.TypeSection, wasm.FunctionType{})
		m.ImportSection = append(m.ImportSection, wasm.Import{
			Module: HostModule, Name: "not_a_func", Type: wasm.ExternTypeFunc,
			DescFunc: wasm.Index(len(m.TypeSection) - 1),
		})
	}
	if p.importsWasiSnapshot {
		m.TypeSection = append(m.TypeSection, wasm.FunctionType{
			Params: []wasm.ValueType{wasm.ValueTypeI32}, Results: []wasm.ValueType{wasm.ValueTypeI32},
		})
		m.ImportSection = append(m.ImportSection, wasm.Import{
			Module: "wasi_snapshot_preview1", Name: "fd_close", Type: wasm.ExternTypeFunc,
			DescFunc: wasm.Index(len(m.TypeSection) - 1),
		})
	}
	importedFuncs := wasm.Index(len(m.ImportSection))

	// Bodies return zeros of whatever the signature promises.
	body := func(t wasm.FunctionType) []byte {
		var b []byte
		for _, r := range t.Results {
			switch r {
			case wasm.ValueTypeI64:
				b = append(b, wasm.OpcodeI64Const, 1)
			default:
				b = append(b, wasm.OpcodeI32Const, 0)
			}
		}
		return append(b, wasm.OpcodeEnd)
	}

	if !p.omitHandleRequest {
		b := body(handleRequest)
		if p.trapHandleRequest {
			b = []byte{wasm.OpcodeUnreachable, wasm.OpcodeEnd}
		}
		m.TypeSection = append(m.TypeSection, handleRequest)
		m.FunctionSection = append(m.FunctionSection, wasm.Index(len(m.TypeSection)-1))
		m.CodeSection = append(m.CodeSection, wasm.Code{Body: b})
		m.ExportSection = append(m.ExportSection, wasm.Export{
			Name: FuncHandleRequest, Type: wasm.ExternTypeFunc,
			Index: importedFuncs + wasm.Index(len(m.FunctionSection)-1),
		})
	}
	if !p.omitHandleResponse {
		b := body(handleResponse)
		if p.trapHandleResponse {
			b = []byte{wasm.OpcodeUnreachable, wasm.OpcodeEnd}
		}
		m.TypeSection = append(m.TypeSection, handleResponse)
		m.FunctionSection = append(m.FunctionSection, wasm.Index(len(m.TypeSection)-1))
		m.CodeSection = append(m.CodeSection, wasm.Code{Body: b})
		m.ExportSection = append(m.ExportSection, wasm.Export{
			Name: FuncHandleResponse, Type: wasm.ExternTypeFunc,
			Index: importedFuncs + wasm.Index(len(m.FunctionSection)-1),
		})
	}
	if !p.omitMemory {
		m.MemorySection = []wasm.Memory{{Min: 1, Cap: 1, Max: 1, IsMaxEncoded: true}}
		m.ExportSection = append(m.ExportSection, wasm.Export{Name: Memory, Type: wasm.ExternTypeMemory})
	}

	return binaryencoding.EncodeModule(m)
}

// TestNewMiddleware_guestContract covers every way a guest can fail the
// contract. A guest that doesn't match must fail fast, at compile time.
func TestNewMiddleware_guestContract(t *testing.T) {
	tests := []struct {
		name          string
		guest         []byte
		expectedError string
	}{
		{
			name:          "not wasm",
			guest:         []byte("not wasm at all"),
			expectedError: "wasm: error compiling guest: invalid magic number",
		},
		{
			name:          "no handle_request",
			guest:         newGuestModule(guestParts{omitHandleRequest: true}),
			expectedError: "wasm: guest doesn't export func[handle_request]",
		},
		{
			name:          "wrong handle_request signature",
			guest:         newGuestModule(guestParts{badHandleRequest: true}),
			expectedError: "wasm: guest exports the wrong signature for func[handle_request]. should be () -> (i64)",
		},
		{
			name:          "no handle_response",
			guest:         newGuestModule(guestParts{omitHandleResponse: true}),
			expectedError: "wasm: guest doesn't export func[handle_response]",
		},
		{
			name:          "wrong handle_response signature",
			guest:         newGuestModule(guestParts{badHandleResponse: true}),
			expectedError: "wasm: guest exports the wrong signature for func[handle_response]. should be (i32, 32) -> ()",
		},
		{
			name:          "no memory",
			guest:         newGuestModule(guestParts{omitMemory: true}),
			expectedError: "wasm: guest doesn't export memory[memory]",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mw, err := NewMiddleware(testCtx, tc.guest, UnimplementedHost{})
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.expectedError)
			require.Nil(t, mw)
		})
	}
}

// TestNewMiddleware_ok proves the happy path, including that a guest is
// eagerly instantiated so failures surface from NewMiddleware, not later.
func TestNewMiddleware_ok(t *testing.T) {
	mw, err := NewMiddleware(testCtx, newGuestModule(guestParts{importsHTTPHandler: true}), UnimplementedHost{})
	require.NoError(t, err)
	defer mw.Close(testCtx)

	require.Zero(t, mw.Features())

	// The guest returns ctxNext=1: proceed to the next handler.
	outCtx, ctxNext, err := mw.HandleRequest(testCtx)
	require.NoError(t, err)
	require.Equal(t, CtxNext(1), ctxNext)
	require.NoError(t, mw.HandleResponse(outCtx, 0, nil))
}

// TestNewMiddleware_wasi covers the import-detection fallthrough: a guest
// importing WASI gets wazy's wasi_snapshot_preview1 instantiated for it.
func TestNewMiddleware_wasi(t *testing.T) {
	var r wazy.Runtime
	mw, err := NewMiddleware(testCtx,
		newGuestModule(guestParts{importsWasiSnapshot: true, importsHTTPHandler: true}),
		UnimplementedHost{},
		WithRuntime(func(ctx context.Context) (wazy.Runtime, error) {
			r = wazy.NewRuntime(ctx)
			return r, nil
		}))
	require.NoError(t, err)
	defer mw.Close(testCtx)

	require.NotNil(t, r.Module("wasi_snapshot_preview1"))
	require.NotNil(t, r.Module(HostModule))
}

// TestNewMiddleware_wasiAlreadyInstantiated covers a runtime that already has
// WASI: instantiating it twice would fail, so it must be detected.
func TestNewMiddleware_wasiAlreadyInstantiated(t *testing.T) {
	mw, err := NewMiddleware(testCtx,
		newGuestModule(guestParts{importsWasiSnapshot: true}),
		UnimplementedHost{},
		WithRuntime(func(ctx context.Context) (wazy.Runtime, error) {
			r := wazy.NewRuntime(ctx)
			if _, err := wasi_snapshot_preview1.Instantiate(ctx, r); err != nil {
				return nil, err
			}
			return r, nil
		}))
	require.NoError(t, err)
	defer mw.Close(testCtx)
}

// TestNewMiddleware_runtimeError covers a failing NewRuntime option.
func TestNewMiddleware_runtimeError(t *testing.T) {
	expected := errors.New("ice")
	mw, err := NewMiddleware(testCtx, newGuestModule(guestParts{}), UnimplementedHost{},
		WithRuntime(func(context.Context) (wazy.Runtime, error) { return nil, expected }))
	require.Error(t, err)
	require.Contains(t, err.Error(), "wasm: error creating middleware: ice")
	require.Nil(t, mw)
}

// TestNewMiddleware_hostModuleConflict covers instantiateHost failing because
// the runtime already exports the ABI's module name.
func TestNewMiddleware_hostModuleConflict(t *testing.T) {
	mw, err := NewMiddleware(testCtx,
		newGuestModule(guestParts{importsHTTPHandler: true}),
		UnimplementedHost{},
		WithRuntime(func(ctx context.Context) (wazy.Runtime, error) {
			r := wazy.NewRuntime(ctx)
			if _, err := r.NewHostModuleBuilder(HostModule).Instantiate(ctx); err != nil {
				return nil, err
			}
			return r, nil
		}))
	require.Error(t, err)
	require.Contains(t, err.Error(), "wasm: error instantiating host")
	require.Nil(t, mw)
}

// TestOptions covers each option's plumbing. What they actually do is proved
// end to end in the nethttp examples: WithModuleConfig carries the guest's
// stdout, WithGuestConfig feeds redact's secret, WithLogger prints the log
// guest's message.
func TestOptions(t *testing.T) {
	moduleConfig := wazy.NewModuleConfig().WithName("ignored")
	guestConfig := []byte("config")
	logger := ConsoleLogger{}
	newRuntime := func(ctx context.Context) (wazy.Runtime, error) { return wazy.NewRuntime(ctx), nil }

	o := &options{}
	for _, opt := range []Option{
		WithModuleConfig(moduleConfig),
		WithGuestConfig(guestConfig),
		WithLogger(logger),
		WithRuntime(newRuntime),
	} {
		opt(o)
	}

	require.Equal(t, moduleConfig, o.moduleConfig)
	require.Equal(t, guestConfig, o.guestConfig)
	require.Equal(t, logger, o.logger)
	require.NotNil(t, o.newRuntime)

	r, err := DefaultRuntime(testCtx)
	require.NoError(t, err)
	require.NoError(t, r.Close(testCtx))
}

// TestGuestConfig covers get_config, including the buf_limit retry contract:
// a limit smaller than the config writes nothing but still reports the length.
func TestGuestConfig(t *testing.T) {
	config := []byte("some config")
	m := &middleware{guestConfig: config}

	mem := newTestMemory(t)
	stack := []uint64{0, uint64(len(config))}
	m.getConfig(testCtx, mem, stack)
	require.Equal(t, uint64(len(config)), stack[0])
	got, ok := mem.Memory().Read(0, uint32(len(config)))
	require.True(t, ok)
	require.Equal(t, config, got)

	// Under the limit: report the length, write nothing.
	require.True(t, mem.Memory().Write(0, make([]byte, len(config))))
	stack = []uint64{0, uint64(len(config) - 1)}
	m.getConfig(testCtx, mem, stack)
	require.Equal(t, uint64(len(config)), stack[0])
	got, ok = mem.Memory().Read(0, uint32(len(config)))
	require.True(t, ok)
	require.Equal(t, make([]byte, len(config)), got)
}

// TestLogEnabled covers the log level check the guest uses to skip work.
func TestLogEnabled(t *testing.T) {
	m := &middleware{logger: ConsoleLogger{}}

	stack := []uint64{uint64(LogLevelInfo)}
	m.logEnabled(testCtx, stack)
	require.Equal(t, uint64(1), stack[0])

	// Debug is -1, which reaches the host sign-extended, as the guest sends it.
	debug := LogLevelDebug
	stack = []uint64{uint64(uint32(debug))}
	m.logEnabled(testCtx, stack)
	require.Zero(t, stack[0])
}

func TestFeatures_String(t *testing.T) {
	require.Equal(t, "", Features(0).String())
	require.Equal(t, "buffer_request", FeatureBufferRequest.String())
	require.Equal(t, "buffer_request|buffer_response|trailers",
		FeatureBufferRequest.WithEnabled(FeatureBufferResponse).WithEnabled(FeatureTrailers).String())
	require.False(t, Features(0).IsEnabled(FeatureTrailers))
	require.Equal(t, "", Features(1<<31).String()) // an unnamed bit
}

func TestNoopLogger(t *testing.T) {
	l := NoopLogger{}
	require.False(t, l.IsEnabled(LogLevelError))
	require.True(t, l.IsEnabled(LogLevelNone))
	l.Log(testCtx, LogLevelError, "dropped")
}

func TestUnimplementedHost(t *testing.T) {
	h := UnimplementedHost{}
	require.Equal(t, "GET", h.GetMethod(testCtx))
	require.Equal(t, "HTTP/1.1", h.GetProtocolVersion(testCtx))
	require.Equal(t, uint32(200), h.GetStatusCode(testCtx))
	require.Equal(t, "1.1.1.1:12345", h.GetSourceAddr(testCtx))
	require.Zero(t, h.EnableFeatures(testCtx, FeatureTrailers))

	// The default body reader is at EOF, and the writer discards.
	n, err := h.RequestBodyReader(testCtx).Read(make([]byte, 1))
	require.Zero(t, n)
	require.Equal(t, "EOF", err.Error())
	require.NoError(t, h.RequestBodyReader(testCtx).Close())
}

// newTestMemory returns a module with memory, to drive host functions that
// write into a guest.
func newTestMemory(t *testing.T) api.Module {
	t.Helper()
	r := wazy.NewRuntime(testCtx)
	t.Cleanup(func() { r.Close(testCtx) })

	mod, err := r.Instantiate(testCtx, binaryencoding.EncodeModule(&wasm.Module{
		MemorySection: []wasm.Memory{{Min: 1, Cap: 1, Max: 1, IsMaxEncoded: true}},
		ExportSection: []wasm.Export{{Name: Memory, Type: wasm.ExternTypeMemory}},
	}))
	require.NoError(t, err)
	return mod
}
