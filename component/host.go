package component

import (
	"context"

	"github.com/samyfodil/wazy/internal/component/instance"
)

// This file is what a host implementation outside wazy registers through.
// It is the same surface wazy's own WASI 0.2 implementation uses -- not a
// reduced one -- so any interface WASI can implement, an embedder can.

// HandleTable is one instance's resource handle table. Obtain it with
// WithResourcesHook; use it to mint own<T>/borrow<T> handles that sit nested
// inside a composite result, which the engine's automatic top-level handle
// translation does not reach.
type HandleTable = instance.HandleTable

// WithImportCustom registers fn as the host implementation of iface/name
// with a hand-built signature -- the general form of WithImport, and the only
// one that can express a nested composite. Build fd and resolve from a
// TypeTable:
//
//	tbl := component.NewTypeTable()
//	fd := tbl.Func([]component.TypeRef{component.Prim("string")},
//		tbl.Result(tbl.List(component.Prim("string")), component.Prim("u32")))
//	opt := component.WithImportCustom("acme:api/host@1.0.0", "lookup", fn, fd, tbl.Resolver())
//
// iface is matched with its "@x.y.z" version suffix stripped, so one
// registration serves every patch version of an interface.
func WithImportCustom(iface, name string, fn HostFunc, fd FuncDesc, resolve Resolver) Option {
	return instance.WithImportCustom(iface, name, fn, fd, resolve)
}

// WithResourceTag declares that the resource `name`, exported by the imported
// interface `iface`, is the one this host tags as `tag` when minting handles.
//
// Required for any resource-bearing interface: the guest drops handles through
// a canon carrying the component binary's own type index, while the host mints
// them under a tag of its choosing. Without this mapping the two numberings
// disagree and the first drop trips the handle table's cross-type check.
func WithResourceTag(iface, name string, tag uint32) Option {
	return instance.WithResourceTag(iface, name, tag)
}

// WithResourcesHook registers a callback run once per instantiation, with
// that instance's HandleTable, before any host func executes. This is how a
// host implementation gets the table it needs to mint nested handles.
func WithResourcesHook(hook func(*HandleTable)) Option {
	return instance.WithResourcesHook(hook)
}

// WithHostResourceDtor registers the Go destructor run when the guest drops an
// owned handle of the host resource tagged `tag`.
func WithHostResourceDtor(tag uint32, fn func(ctx context.Context, rep uint32) error) Option {
	return instance.WithHostResourceDtor(tag, fn)
}

// WithHostState attaches an opaque value to every Instance built with this
// option, retrievable with Instance.HostState. It is how a host
// implementation of a stateful interface keeps state that lives exactly as
// long as one Instance -- the shape wazy's own wasi:http server side uses to
// find its request/response tables from ServeHTTP.
//
// Use a package-private key type, not a bare string, so two independent host
// implementations cannot collide:
//
//	type myHostKey struct{}
//	component.WithHostState(myHostKey{}, newMyHost())
//	h := inst.HostState(myHostKey{}).(*myHost)
func WithHostState(key, value any) Option {
	return instance.WithHostState(key, value)
}

// InstanceExports is re-exported on Instance itself; see
// instance.Instance.InstanceExports. Listed here so the host-implementation
// surface is documented in one place: it is how a host finds an exported
// interface whose version suffix it does not know in advance.
