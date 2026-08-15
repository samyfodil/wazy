package wasi_http

import (
	"context"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/testing/binaryencoding"
	"github.com/samyfodil/wazy/internal/testing/require"
	"github.com/samyfodil/wazy/internal/wasm"
)

// This file drives the ABI directly, through a synthetic guest, to reach what
// dapr's fixture never calls: the server-side functions, every error arm, and
// the traps.
//
// The guest imports all 27 host functions with wasi-go's signatures and
// re-exports each as a passthrough named "call_<name>". Instantiating it is
// therefore a signature test in itself: a host function declared with the
// wrong arity or value types fails to link here, which the fixture would only
// catch for the 14 functions it happens to import.

const (
	i32 = wasm.ValueTypeI32
	i64 = wasm.ValueTypeI64

	// heapBase is where the guest's bump allocator starts handing out memory.
	// Tests write their own inputs below it.
	heapBase = 4096
)

// hostFn is one host function to import and re-export.
type hostFn struct {
	module  string
	name    string
	params  []wasm.ValueType
	results []wasm.ValueType
}

func i32s(n int) []wasm.ValueType {
	types := make([]wasm.ValueType, n)
	for i := range types {
		types[i] = i32
	}
	return types
}

// hostFns lists every function this package exports, with the signature
// wasi-go's guests import it by.
var hostFns = []hostFn{
	{OutgoingModuleName, "request", i32s(14), []wasm.ValueType{i32}},
	{OutgoingModuleName, "handle", i32s(8), []wasm.ValueType{i32}},

	{TypesModuleName, "new-outgoing-request", i32s(14), []wasm.ValueType{i32}},
	{TypesModuleName, "new-fields", i32s(2), []wasm.ValueType{i32}},
	{TypesModuleName, "drop-fields", i32s(1), nil},
	{TypesModuleName, "fields-entries", i32s(2), nil},
	{TypesModuleName, "drop-outgoing-request", i32s(1), nil},
	{TypesModuleName, "outgoing-request-write", i32s(2), nil},
	{TypesModuleName, "drop-incoming-response", i32s(1), nil},
	{TypesModuleName, "incoming-response-status", i32s(1), []wasm.ValueType{i32}},
	{TypesModuleName, "incoming-response-headers", i32s(1), []wasm.ValueType{i32}},
	{TypesModuleName, "incoming-response-consume", i32s(2), nil},
	{TypesModuleName, "future-incoming-response-get", i32s(2), nil},
	{TypesModuleName, "incoming-request-method", i32s(2), nil},
	{TypesModuleName, "incoming-request-path", i32s(2), nil},
	{TypesModuleName, "incoming-request-authority", i32s(2), nil},
	{TypesModuleName, "incoming-request-headers", i32s(1), []wasm.ValueType{i32}},
	{TypesModuleName, "incoming-request-consume", i32s(2), nil},
	{TypesModuleName, "drop-incoming-request", i32s(1), nil},
	{TypesModuleName, "set-response-outparam", i32s(5), []wasm.ValueType{i32}},
	{TypesModuleName, "new-outgoing-response", i32s(2), []wasm.ValueType{i32}},
	{TypesModuleName, "outgoing-response-write", i32s(2), nil},
	{TypesModuleName, "drop-outgoing-response", i32s(1), nil},
	{TypesModuleName, "log-it", i32s(2), nil},

	{StreamsModuleName, "read", []wasm.ValueType{i32, i64, i32}, nil},
	{StreamsModuleName, "drop-input-stream", i32s(1), nil},
	{StreamsModuleName, "write", i32s(4), nil},
}

// How the synthetic guest exports cabi_realloc, so both allocator failures
// are reachable: no allocator at all, and one that traps.
type reallocKind int

const (
	reallocBump reallocKind = iota // a working bump allocator
	reallocNone                    // no cabi_realloc export
	reallocTrap                    // a cabi_realloc that traps
)

// guestModule builds the synthetic guest.
func guestModule(realloc reallocKind) []byte {
	m := &wasm.Module{MemorySection: &wasm.Memory{Min: 2, Cap: 2, Max: 2, IsMaxEncoded: true}}

	for i, fn := range hostFns {
		m.TypeSection = append(m.TypeSection, wasm.FunctionType{Params: fn.params, Results: fn.results})
		m.ImportSection = append(m.ImportSection, wasm.Import{
			Module: fn.module, Name: fn.name, Type: wasm.ExternTypeFunc, DescFunc: wasm.Index(i),
		})
	}

	// One passthrough per import: local.get 0..n-1, then call it.
	for i, fn := range hostFns {
		body := make([]byte, 0, len(fn.params)*2+3)
		for p := range fn.params {
			body = append(body, wasm.OpcodeLocalGet, byte(p))
		}
		body = append(body, wasm.OpcodeCall, byte(i), wasm.OpcodeEnd)

		m.FunctionSection = append(m.FunctionSection, wasm.Index(i))
		m.CodeSection = append(m.CodeSection, wasm.Code{Body: body})
		m.ExportSection = append(m.ExportSection, wasm.Export{
			Name: "call_" + fn.name, Type: wasm.ExternTypeFunc,
			Index: wasm.Index(len(hostFns) + i),
		})
	}
	m.ExportSection = append(m.ExportSection, wasm.Export{Name: "memory", Type: wasm.ExternTypeMemory})

	if realloc != reallocNone {
		// cabi_realloc(_, _, _, size): a bump allocator over one global.
		// Returns the old break, then advances it by size.
		body := []byte{
			wasm.OpcodeGlobalGet, 0, // result: the current break
			wasm.OpcodeGlobalGet, 0,
			wasm.OpcodeLocalGet, 3, // size
			wasm.OpcodeI32Add,
			wasm.OpcodeGlobalSet, 0,
			wasm.OpcodeEnd,
		}
		if realloc == reallocTrap {
			body = []byte{wasm.OpcodeUnreachable, wasm.OpcodeEnd}
		}

		m.GlobalSection = []wasm.Global{{
			Type: wasm.GlobalType{ValType: i32, Mutable: true},
			Init: wasm.NewConstantExpressionFromI32(heapBase),
		}}
		reallocType := wasm.Index(len(m.TypeSection))
		m.TypeSection = append(m.TypeSection, wasm.FunctionType{Params: i32s(4), Results: []wasm.ValueType{i32}})
		m.FunctionSection = append(m.FunctionSection, reallocType)
		m.CodeSection = append(m.CodeSection, wasm.Code{Body: body})
		m.ExportSection = append(m.ExportSection, wasm.Export{
			Name: "cabi_realloc", Type: wasm.ExternTypeFunc,
			Index: wasm.Index(len(hostFns) * 2),
		})
	}

	return binaryencoding.EncodeModule(m)
}

// guest is a synthetic guest bound to one host.
type guest struct {
	t   *testing.T
	ctx context.Context
	mod api.Module
	h   *host
}

// newGuest instantiates the host modules and the synthetic guest against them.
func newGuest(t *testing.T) *guest {
	t.Helper()
	return newGuestWith(t, reallocBump)
}

func newGuestWith(t *testing.T, realloc reallocKind) *guest {
	t.Helper()
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	t.Cleanup(func() { r.Close(ctx) })

	h := newHost()
	_, err := h.instantiate(ctx, r)
	require.NoError(t, err)

	mod, err := r.Instantiate(ctx, guestModule(realloc))
	require.NoError(t, err)
	return &guest{t: t, ctx: ctx, mod: mod, h: h}
}

// call invokes a host function through the guest passthrough.
func (g *guest) call(name string, params ...uint64) []uint64 {
	g.t.Helper()
	fn := g.mod.ExportedFunction("call_" + name)
	require.NotNil(g.t, fn, name)
	results, err := fn.Call(g.ctx, params...)
	require.NoError(g.t, err, name)
	return results
}

// callErr invokes a host function expecting it to trap, returning the error.
func (g *guest) callErr(name string, params ...uint64) error {
	g.t.Helper()
	fn := g.mod.ExportedFunction("call_" + name)
	require.NotNil(g.t, fn, name)
	_, err := fn.Call(g.ctx, params...)
	require.Error(g.t, err, name)
	return err
}

// call1 invokes a host function with one result.
func (g *guest) call1(name string, params ...uint64) uint32 {
	g.t.Helper()
	results := g.call(name, params...)
	require.Equal(g.t, 1, len(results))
	return uint32(results[0])
}

// writeString stores s at ptr and returns (ptr, len) for the ABI.
func (g *guest) writeString(ptr uint32, s string) (uint32, uint32) {
	g.t.Helper()
	require.True(g.t, g.mod.Memory().WriteString(ptr, s))
	return ptr, uint32(len(s))
}

// u32 reads a little-endian u32 out of guest memory.
func (g *guest) u32(ptr uint32) uint32 {
	g.t.Helper()
	v, ok := g.mod.Memory().ReadUint32Le(ptr)
	require.True(g.t, ok)
	return v
}

// str reads a [ptr][len] struct at ptr and returns the string it points to.
func (g *guest) str(ptr uint32) string {
	g.t.Helper()
	data, ok := g.mod.Memory().Read(g.u32(ptr), g.u32(ptr+4))
	require.True(g.t, ok)
	return string(data)
}
