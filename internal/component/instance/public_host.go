package instance

import (
	"context"

	"github.com/samyfodil/wazy/internal/component/abi"
	"github.com/samyfodil/wazy/internal/component/binary"
)

// This file is the engine-side surface an OUT-OF-TREE host implementation
// registers through -- the same surface wazy's own WASI 0.2 implementation
// uses. Everything here was previously unexported and reachable only from
// inside this package; a host living in another module needs all of it, so
// each is a thin exported form of the internal original rather than a
// parallel path. The component package re-exports these.

// HandleTable is one component instance's resource handle table: the thing
// that maps a guest-visible handle to the host representation behind it.
// A host func that mints an own<T>/borrow<T> nested inside a composite
// result must do so through this table (the engine translates only
// top-level handles automatically), which is what WithResourcesHook exists
// to hand over.
type HandleTable = handleTable

// WithImportCustom registers fn as the host implementation of iface/name
// with a hand-built signature: fd describes the WIT types and resolve maps
// its TypeRefs back to descriptors (both from a TypeTable).
//
// This is the general form of WithImport. WithImport synthesizes a FuncDesc
// from a flat list of types, which cannot express a composite whose children
// are themselves composites; passing the FuncDesc directly can express any
// WIT type, at the cost of building the type table yourself.
func WithImportCustom(iface, name string, fn HostFunc, fd binary.FuncDesc, resolve abi.Resolver) Option {
	return withImportCustom(iface, name, fn, fd, resolve)
}

// WithResourceTag declares that the resource named `name`, exported by the
// imported interface `iface`, is the resource this host implementation tags
// as `tag` when it mints handles.
//
// This is not optional for a resource-bearing interface. The guest's own
// generated bindings drop handles through a canon resource.drop carrying the
// component binary's type index, while the host mints them under a
// caller-chosen tag; without this mapping the two numberings disagree and the
// first drop trips the handle table's cross-type check.
func WithResourceTag(iface, name string, tag uint32) Option {
	return withResourceTag(iface, name, tag)
}

// WithResourcesHook registers a callback invoked once per instantiation with
// that instance's HandleTable, before any host func can run. A host
// implementation needs it to mint handles for own<T>/borrow<T> nested inside
// composite results, which the automatic top-level translation does not
// cover.
func WithResourcesHook(hook func(*HandleTable)) Option {
	return withResourcesHook(hook)
}

// WithHostResourceDtor registers the Go destructor run when the guest drops
// an owned handle of the host resource tagged `tag` -- the hook for releasing
// whatever the rep names.
func WithHostResourceDtor(tag uint32, fn func(ctx context.Context, rep uint32) error) Option {
	return withHostResourceDtor(tag, fn)
}

// WithHostState attaches an opaque value to every Instance built with this
// option, retrievable via Instance.HostState(key). It is how a host
// implementation of a stateful interface keeps per-instance state: the value
// is created once per Instantiate call and lives exactly as long as the
// Instance.
//
// key should be a package-private type, not a bare string, so two independent
// host implementations cannot collide.
func WithHostState(key, value any) Option {
	return func(c *config) {
		if c.hostState == nil {
			c.hostState = map[any]any{}
		}
		c.hostState[key] = value
	}
}

// HostState returns the value registered under key by WithHostState, or nil.
func (in *Instance) HostState(key any) any {
	if in.hostState == nil {
		return nil
	}
	return in.hostState[key]
}

// InstanceExports returns the names of the interfaces this component
// instance exports, as they appear in the binary (version suffix included).
//
// A host implementation needs this to find an export whose exact versioned
// name it cannot know in advance: a component exporting
// wasi:http/incoming-handler@0.2.3 and one exporting @0.2.0 are the same
// interface to a host that wants to drive it. The caller matches on the
// versionless prefix and uses the full name it finds here to Call.
func (in *Instance) InstanceExports() []string {
	out := make([]string, 0, len(in.instanceExports))
	for name := range in.instanceExports {
		out = append(out, name)
	}
	return out
}

// Resources returns this instance's handle table.
//
// WithResourcesHook covers the registration-time need (a host func minting a
// handle nested in a result). This covers the other direction: a host that
// DRIVES an exported interface must mint the own<T>/borrow<T> handles it
// passes in, and only has the Instance to work from -- which is exactly what
// wasi:http's server side does to hand the guest an incoming-request.
func (in *Instance) Resources() *HandleTable { return in.resources }

// Harness builds a host implementation's registered funcs without
// instantiating a component, so they can be called directly. It backs the
// public component/componenttest package -- see that package's doc for why
// this exists.
type Harness struct {
	cfg       *config
	resources *handleTable
}

// NewHarness applies opts and runs the resource hooks, exactly as
// Instantiate would before any host func executes.
func NewHarness(opts []Option) *Harness {
	c := newConfig(opts)
	res := newHandleTable()
	res.setResourceNames(c.resourceTags)
	runResourceHooks(c, res)
	return &Harness{cfg: c, resources: res}
}

// Func returns the HostFunc registered for iface/name, or nil. iface is
// matched with its version suffix stripped, matching registration.
func (h *Harness) Func(iface, name string) HostFunc {
	hi, ok := h.cfg.imports[mkImportKey(iface, name)]
	if !ok {
		return nil
	}
	return hi.fn
}

// Resources returns the handle table the hooks were given -- the same one a
// real instantiation would hand out, so handles minted by a host func under
// test can be resolved here.
func (h *Harness) Resources() *HandleTable { return h.resources }

// Registered returns every import these options registered, as
// interface name -> func names. Interface names appear with their version
// suffix already stripped, matching how registration keys them.
func (h *Harness) Registered() map[string][]string {
	out := map[string][]string{}
	for key := range h.cfg.imports {
		out[key.iface] = append(out[key.iface], key.name)
	}
	return out
}
