// Package wasmedge implements the WasmEdge sockets extension to WASI preview
// 1, so a guest built against WasmEdge's socket SDKs -- the raw TCP and UDP
// access WASI preview 1 has no equivalent for -- runs on wazy.
//
// # It exports into the WASI module, not its own
//
// Unlike a normal import package, these functions belong to the module named
// "wasi_snapshot_preview1": that is where a guest imports sock_open and the
// rest from, and a runtime can instantiate that name only once. So this
// package exports *into* the same host module as the standard WASI functions,
// through the FunctionExporter that wasi_snapshot_preview1 already documents
// for extending itself.
//
// Instantiate does that composition for you:
//
//	wasmedge.Instantiate(ctx, r, wasmedge.V2)
//
// Or compose it yourself, which is what an embedder wanting a customised WASI
// surface would do:
//
//	b := r.NewHostModuleBuilder(wasi_snapshot_preview1.ModuleName)
//	wasi_snapshot_preview1.NewFunctionExporter().ExportFunctions(b)
//	wasmedge.NewFunctionExporter(wasmedge.V2).ExportFunctions(b)
//	b.Instantiate(ctx)
//
// # Versions
//
// Two versions of the extension are in the wild and guests pick whichever
// their SDK targeted, so both are supported. Detect picks the right one from a
// module's imports.
//
// # Sockets are ordinary descriptors
//
// A socket this package creates goes into the same descriptor table as files
// and satisfies wazy's socket interfaces, so the standard WASI functions work
// on it unchanged: a guest reads with sock_recv or fd_read, writes with
// sock_send or fd_write, shuts down with sock_shutdown, and closes with
// fd_close.
//
// # Sandboxing
//
// Instantiating this package lets the guest open outbound connections and
// listen for inbound ones, with the host's network access and no allow-list.
// Do not instantiate it for untrusted guests.
//
// # Attribution
//
// The wire format follows github.com/stealthrocket/wasi-go's implementation
// (Apache 2.0), which is the reference for the byte layouts, including the
// places where WasmEdge's own libraries disagree with their struct
// definitions. The backend here is Go's net package rather than the raw
// syscalls that implementation uses.
package wasmedge

import (
	"context"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/imports/wasi_snapshot_preview1"
)

// Version selects which of the two wire versions to export.
type Version int

const (
	// None exports nothing. Detect returns it for a guest that needs no
	// sockets extension.
	None Version = iota

	// V1 is the original extension. Its sock_accept predates the WASI preview
	// 1 signature and takes no fdflags, and its addresses are a bare 16-byte
	// buffer with the port passed alongside.
	V1

	// V2 is the current extension. Its sock_accept matches WASI preview 1, and
	// its addresses are 128 bytes led by a family tag, which leaves room for
	// AF_UNIX.
	V2
)

// String implements fmt.Stringer.
func (v Version) String() string {
	switch v {
	case V1:
		return "v1"
	case V2:
		return "v2"
	default:
		return "none"
	}
}

// ModuleName is the module these functions are exported into: the WASI preview
// 1 module, not a module of their own.
const ModuleName = wasi_snapshot_preview1.ModuleName

// Instantiate instantiates the WASI preview 1 module with the sockets
// extension of the given version added, and returns a Closer for it.
//
// Note: closing the wazy.Runtime has the same effect as closing the result.
func Instantiate(ctx context.Context, r wazy.Runtime, v Version) (api.Closer, error) {
	b := r.NewHostModuleBuilder(ModuleName)
	wasi_snapshot_preview1.NewFunctionExporter().ExportFunctions(b)
	NewFunctionExporter(v).ExportFunctions(b)
	return b.Instantiate(ctx)
}

// MustInstantiate calls Instantiate or panics.
func MustInstantiate(ctx context.Context, r wazy.Runtime, v Version) {
	if _, err := Instantiate(ctx, r, v); err != nil {
		panic(err)
	}
}

// FunctionExporter exports the extension's functions into a host module
// builder, which must be building ModuleName.
type FunctionExporter interface {
	ExportFunctions(wazy.HostModuleBuilder)
}

// NewFunctionExporter returns a FunctionExporter for the given version.
func NewFunctionExporter(v Version) FunctionExporter { return exporter(v) }

type exporter Version

// ExportFunctions implements FunctionExporter.
func (v exporter) ExportFunctions(b wazy.HostModuleBuilder) {
	switch Version(v) {
	case V1:
		exportCommon(b)
		// V1's accept has no fdflags, and its address getters report the
		// family in a separate result.
		wazy.HostFunc2(b.NewFunctionBuilder(), sockAcceptV1).Export(funcSockAccept)
		wazy.HostFunc7(b.NewFunctionBuilder(), sockRecvFromV1).Export(funcSockRecvFrom)
		wazy.HostFunc4(b.NewFunctionBuilder(), sockGetLocalAddrV1).Export(funcSockGetLocalAdd)
		wazy.HostFunc4(b.NewFunctionBuilder(), sockGetPeerAddrV1).Export(funcSockGetPeerAddr)
	case V2:
		exportCommon(b)
		// V2 keeps the WASI preview 1 accept signature, but it still has to be
		// re-exported: the standard one only accepts on a pre-opened listener,
		// and a guest that called sock_listen owns its own.
		wazy.HostFunc3(b.NewFunctionBuilder(), sockAccept).Export(funcSockAccept)
		wazy.HostFunc8(b.NewFunctionBuilder(), sockRecvFromV2).Export(funcSockRecvFrom)
		wazy.HostFunc3(b.NewFunctionBuilder(), sockGetLocalAddrV2).Export(funcSockGetLocalAdd)
		wazy.HostFunc3(b.NewFunctionBuilder(), sockGetPeerAddrV2).Export(funcSockGetPeerAddr)
	}
}

// exportCommon exports the functions both versions share unchanged.
func exportCommon(b wazy.HostModuleBuilder) {
	wazy.HostFunc3(b.NewFunctionBuilder(), sockOpen).Export(funcSockOpen)
	wazy.HostFunc3(b.NewFunctionBuilder(), sockBind).Export(funcSockBind)
	wazy.HostFunc3(b.NewFunctionBuilder(), sockConnect).Export(funcSockConnect)
	wazy.HostFunc2(b.NewFunctionBuilder(), sockListen).Export(funcSockListen)
	wazy.HostFunc7(b.NewFunctionBuilder(), sockSendTo).Export(funcSockSendTo)
	wazy.HostFunc5(b.NewFunctionBuilder(), sockGetSockOpt).Export(funcSockGetSockOpt)
	wazy.HostFunc5(b.NewFunctionBuilder(), sockSetSockOpt).Export(funcSockSetSockOpt)
	wazy.HostFunc8(b.NewFunctionBuilder(), sockGetAddrInfo).Export(funcSockGetAddrInfo)
}

// Detect reports which version of the extension a module needs, from the
// functions it imports.
//
// The two versions share function names, so the name alone is not enough: they
// are told apart by signature, which is the only thing that cannot drift from
// what the guest was compiled against. sock_getlocaladdr is the clearest
// discriminator -- four parameters in V1, three in V2 -- with sock_recv_from
// as a fallback for a guest that imports one but not the other.
func Detect(imports []api.FunctionDefinition) Version {
	found := None
	for _, f := range imports {
		moduleName, name, ok := f.Import()
		if !ok || moduleName != ModuleName {
			continue
		}
		switch name {
		case funcSockGetLocalAdd, funcSockGetPeerAddr:
			switch len(f.ParamTypes()) {
			case 4:
				return V1
			case 3:
				return V2
			}
		case funcSockRecvFrom:
			switch len(f.ParamTypes()) {
			case 7:
				return V1
			case 8:
				return V2
			}
		case funcSockAccept:
			// Shared with standard WASI, so it only narrows the answer: two
			// parameters can only be V1.
			if len(f.ParamTypes()) == 2 {
				return V1
			}
		case funcSockOpen, funcSockBind, funcSockConnect, funcSockListen,
			funcSockSendTo, funcSockGetSockOpt, funcSockSetSockOpt, funcSockGetAddrInfo:
			// Identical in both versions: the guest needs the extension, but
			// these do not say which. Keep looking, and fall back to V2, the
			// version current SDKs emit.
			if found == None {
				found = V2
			}
		}
	}
	return found
}
