// Package componenttest provides utilities for testing a host implementation
// of a component interface.
//
// A host func registered with component.WithImport or WithImportCustom is
// normally only reachable by instantiating a guest that imports it and
// getting the guest to call it. That is the right end-to-end test, and a poor
// unit test: covering one error branch — a malformed argument, an exhausted
// resource, a filesystem that returns EPERM — would mean compiling a guest
// component per branch.
//
// Harness closes that gap. It applies the same Options an Instantiate call
// would, runs the resource hooks, and hands back the registered funcs so a
// test can call them directly with lifted argument values and assert on the
// values they return.
//
//	h := componenttest.New(component.WithImportCustom("acme:api/host@1.0.0", "lookup", fn, fd, tbl.Resolver()))
//	got, err := h.Func("acme:api/host@1.0.0", "lookup")(ctx, []component.Value{...})
//
// What it does NOT do is exercise the Canonical ABI: nothing is lifted from
// or lowered into guest memory, so a signature that is wrong about how a
// value is laid out still passes here. Keep at least one real-guest test per
// interface; use this for the branches underneath it.
package componenttest

import (
	"github.com/samyfodil/wazy/component"
	"github.com/samyfodil/wazy/internal/component/instance"
)

// Harness holds host funcs registered from a set of Options, callable
// without a guest.
type Harness struct {
	h *instance.Harness
}

// New applies opts and runs the resource hooks, in the same order
// component.Instantiate does before any host func can run.
func New(opts ...component.Option) *Harness {
	return &Harness{h: instance.NewHarness(opts)}
}

// Func returns the host func registered for iface/name, or nil if none is.
// iface is matched with its "@x.y.z" suffix stripped, the same way
// registration matches, so the version passed here need not be exact.
func (h *Harness) Func(iface, name string) component.HostFunc {
	return h.h.Func(iface, name)
}

// MustFunc is Func, panicking when nothing is registered — for a test that
// would otherwise nil-panic one line later with no indication of which
// registration is missing.
func (h *Harness) MustFunc(iface, name string) component.HostFunc {
	fn := h.Func(iface, name)
	if fn == nil {
		panic("componenttest: no host func registered for " + iface + " " + name)
	}
	return fn
}

// Resources returns the handle table the hooks were given. It is the same
// table a real instantiation would use, so a handle a host func under test
// mints can be resolved back to its rep here — which is how a test asserts
// that an own<T> result names what it should.
func (h *Harness) Resources() *component.HandleTable { return h.h.Resources() }

// Registered returns every import the Options registered, as interface name
// -> func names, with version suffixes stripped. Useful for asserting that a
// host implementation registers exactly the surface it claims to -- wazy's
// own WASI conformance test uses it to diff the registered surface against
// the reference WIT.
func (h *Harness) Registered() map[string][]string { return h.h.Registered() }
