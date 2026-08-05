// Package component runs WebAssembly Component Model components -- and the WASI
// 0.2 (wasip2) world built on it -- on a wazy Runtime.
//
// Where the core wazy package instantiates core modules, this package
// instantiates *components*: genuine wasm32-wasip2 binaries produced by rustc,
// wasm-tools, and friends. It decodes the component, wires its multi-module
// graph (nested instances, canonical lift/lower of the Canonical ABI, resource
// lifetimes), and -- with WithWASI -- provides the WASI 0.2 host interfaces
// (wasi:cli, clocks, filesystem, io, random, sockets, http).
//
// Typical use: build a Runtime, instantiate a component with the WASI surface
// wired to your stdio/filesystem/args, call an export, then Close.
//
//	r := wazy.NewRuntime(ctx)
//	defer r.Close(ctx)
//
//	inst, err := component.Instantiate(ctx, r, componentWasm,
//		component.WithWASI(component.WASIConfig{Stdout: os.Stdout})...)
//	if err != nil {
//		return err
//	}
//	defer inst.Close(ctx)
//
//	// A wasi:cli/command component: run its entry point.
//	_, err = inst.Call(ctx, "wasi:cli/run@0.2.3#run")
//
// Call arguments and results are Go values (uint32, int64, string, []any for
// lists/records, and uint32 handles for resources), matching the Canonical
// ABI's lifting of the component's WIT types.
//
// This API is young and, like the rest of wazy, makes no stability promise yet.
package component

import (
	"context"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/internal/component/abi"
	"github.com/samyfodil/wazy/internal/component/instance"
)

// Instance is a live component instance. Call its exports with Call /
// CallExport, and release it with Close. A wasi:http/incoming-handler component
// also satisfies http.Handler via ServeHTTP.
type Instance = instance.Instance

// PendingCall is a live CallAsync invocation, suspended awaiting external
// import completions. See Instance.CallAsync.
type PendingCall = instance.PendingCall

// Option configures Instantiate. WithWASI and WithCompileCache produce Options.
type Option = instance.Option

// CompileCache amortizes a component's decode and its embedded core modules'
// compilation across repeated Instantiate calls of the same component bytes.
// Safe for concurrent use. Pair one with a single Runtime and Close it when
// done. See WithCompileCache and NewCompileCache.
type CompileCache = instance.CompileCache

// Instantiate decodes componentBytes as a WebAssembly Component Model component
// and instantiates it on r, returning a live Instance. Pass WithWASI to give it
// the WASI 0.2 host surface, and WithCompileCache to reuse compilation work
// across repeated instantiations. Close the returned Instance when done.
func Instantiate(ctx context.Context, r wazy.Runtime, componentBytes []byte, opts ...Option) (*Instance, error) {
	return instance.Instantiate(ctx, r, componentBytes, opts...)
}

// WithCompileCache reuses cache across this and future Instantiate calls of the
// same component bytes, skipping the repeated decode + core-module compile.
func WithCompileCache(cache *CompileCache) Option { return instance.WithCompileCache(cache) }

// NewCompileCache returns an empty CompileCache ready to pass to
// WithCompileCache. Close it (CompileCache.Close) alongside the Runtime it is
// paired with.
func NewCompileCache() *CompileCache { return instance.NewCompileCache() }

// Value is a component-level call value: a Go value matching the Canonical
// ABI's lifting of a WIT type (uint32, int64, float64, string, []any for
// lists/records/tuples, uint32 for resource handles). It is the element type of
// Call/CallExport arguments and results and of host-import args/results.
type Value = abi.Value

// TypeDesc, PrimitiveDesc, and the rest of the WIT type vocabulary live in
// types.go.

// HostFunc implements a synchronous component import: it receives the lifted
// arguments and returns the lifted results (or an error, which traps the guest
// call). Register it with WithImport.
type HostFunc = instance.HostFunc

// AsyncHostFunc implements an async-lowered component import. It receives the
// lifted arguments and an *AsyncCall used to deliver the result -- synchronously
// (call.Resolve before returning) or later, from any goroutine, once the
// call was started via Instance.CallAsync. Register it with WithAsyncImport.
type AsyncHostFunc = instance.AsyncHostFunc

// AsyncCall is the completion handle an AsyncHostFunc receives. Call Resolve
// with the import's results (or ResolveCancelled). Under CallAsync, Resolve may
// be called from another goroutine after the AsyncHostFunc returns -- that is
// how external I/O completions drive a component forward.
type AsyncCall = instance.AsyncCall

// WithImport registers fn as the component's synchronous import iface/name, with
// the given WIT param/result types. iface is the interface name (e.g.
// "wasi:cli/environment") or "" for a top-level import; name is the function
// (or "" for a bare top-level func import).
func WithImport(iface, name string, fn HostFunc, params, results []TypeDesc) Option {
	return instance.WithImport(iface, name, fn, params, results)
}

// WithAsyncImport registers fn as the component's async-lowered import
// iface/name. Pair it with Instance.CallAsync so fn may complete the call later,
// from another goroutine (real I/O), via AsyncCall.Resolve.
func WithAsyncImport(iface, name string, fn AsyncHostFunc, params, results []TypeDesc) Option {
	return instance.WithAsyncImport(iface, name, fn, params, results)
}
