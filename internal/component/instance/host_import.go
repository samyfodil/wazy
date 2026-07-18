package instance

import (
	"context"
	"fmt"
	"strings"

	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/component/abi"
	"github.com/samyfodil/wazy/internal/component/binary"
)

// HostFunc is a Go implementation of a component import. It receives the
// lifted component-level argument values and returns the component-level
// result values. Returning an error aborts the guest call that invoked it
// (the error surfaces from the originating Instance.Call).
type HostFunc func(ctx context.Context, args []abi.Value) ([]abi.Value, error)

// Option configures Instantiate.
type Option func(*config)

// config holds the caller-provided host import implementations, keyed by
// interface name + function name.
type config struct {
	imports map[importKey]*hostImport

	// resourceHooks are invoked with the Instance's *handleTable as soon as
	// it exists (graph.go's instantiateGraph, right after
	// newHandleTable()), letting an Option's host func closures capture a
	// reference to the same table generic lift/lower already threads
	// through -- see withResourcesHook's doc for why a HostFunc sometimes
	// needs this directly instead.
	resourceHooks []func(*handleTable)

	// resourceTags maps (imported interface, exported resource name) --
	// e.g. ("wasi:filesystem/types@0.2.3", "descriptor") -- to the
	// ResourceType tag an Option's own<T>/borrow<T> declarations (WithImport/
	// withImportCustom) use for that same resource. See withResourceTag's
	// doc for why this mapping needs to exist at all.
	resourceTags map[importKey]uint32

	// compileCache, when set via WithCompileCache, is consulted by every
	// embedded-core-module instantiation (instantiateCoreModule,
	// compilecache.go) instead of always recompiling. Nil (the default)
	// preserves the exact prior always-recompile behavior.
	compileCache *CompileCache

	// httpHost, when non-nil (set by WithWASI when WASIConfig.EnableHTTP is
	// true), is the wasi:http server state. It is copied onto the Instance so
	// (*Instance).ServeHTTP can drive the guest's exported incoming-handler.
	httpHost *wasiHTTP

	// resCanon and importedResDtors are set only when this instantiation is a
	// nested sub-component of a composition (see instantiateNestedInstances).
	// Every sub-instance has its OWN handle table (per the spec -- resource
	// tables are per-instance, so two instances of one component number handles
	// independently); a resource CROSSING a delegating import is transferred by
	// rep (lift_own/lower_own), not by a shared table.
	//
	// resCanon reduces this component's several type indices for one resource
	// (a deftype and its export alias) to a single table tag. importedResDtors
	// maps the table tag of a resource this component imports to the DEFINER's
	// destructor, so this component's resource.drop of an own<R> it received
	// runs that dtor. Both nil for a flat instantiation.
	resCanon         func(uint32) uint32
	importedResDtors map[uint32]func() api.Function

	// resourceOrigin is copied onto the resulting Instance's own field of the
	// same name (see its doc on Instance) -- populated by
	// instantiateNestedInstances as it wires each instantiate-arg, tracing a
	// resource this component receives back to the sibling that ultimately
	// defines it. nil for a flat instantiation, same as resCanon/
	// importedResDtors.
	resourceOrigin map[uint32]resourceIdentity

	// hostResDtors maps a HOST-provided resource's tag (see resourceTags) to a
	// Go destructor run when the GUEST drops an own<R> of that resource via
	// canon resource.drop -- e.g. an embedder tracking outstanding host objects.
	// Keyed by tag so the drop canon (which resolves the guest's imported
	// resource index to that tag) finds it. Set via withHostResourceDtor.
	hostResDtors map[uint32]func(ctx context.Context, rep uint32) error

	// sharedSched, when set, is the *sched every Instance of one composition
	// TREE shares (Phase 3 forced change #1 -- see Instance.sched's doc).
	// instantiateNestedInstances sets it on every subCfg it builds, to the
	// root instantiation's own in.sched, so a sibling's guestTask can be
	// parked and later resumed by ANY instance in the tree's scheduler
	// drive. nil for a flat (non-composed) instantiation, which then becomes
	// its own tree root (instantiateGraph allocates a fresh *sched).
	sharedSched *sched

	// pendingDelegates accumulates a canon lower's forward reference to a
	// nested sibling component instance that had not yet been instantiated
	// at the point THIS instance's own core-instance loop needed to bind it
	// -- trap-on-reenter.wast's shape: a top-level canon lower, wired
	// directly into the outer component's own core-instance graph (built by
	// instantiateGraph's CoreInstances loop), references a LATER-declared
	// nested component instantiate's export (only instantiated afterward, by
	// instantiateNestedInstances). computeCanonHostFunc appends here instead
	// of failing outright; instantiateGraph resolves every entry (filling in
	// its tgt.sub/tgt.be) immediately after instantiateNestedInstances
	// returns, then clears this slice -- see resolveStaticExportFuncDesc and
	// delegatingHostImportDeferred (composition.go). Always nil outside that
	// one shape.
	pendingDelegates []*pendingSiblingDelegate
}

// withHostResourceDtor registers a Go destructor for a host-provided resource,
// run when the guest drops an own<R> of it. tag is the same ResourceType the
// resource's host funcs and withResourceTag use.
func withHostResourceDtor(tag uint32, fn func(ctx context.Context, rep uint32) error) Option {
	return func(c *config) {
		if c.hostResDtors == nil {
			c.hostResDtors = make(map[uint32]func(ctx context.Context, rep uint32) error)
		}
		c.hostResDtors[tag] = fn
	}
}

type importKey struct {
	iface string
	name  string
}

// mkImportKey builds an importKey with the interface name's "@x.y.z" version
// suffix stripped, so host-import matching tolerates the wasi 0.2.x patch
// version a guest was built against. wazy registers one implementation per
// interface; a guest built with a newer wasi crate imports e.g.
// "wasi:io/streams@0.2.12" where the older fixtures import "@0.2.3", but the
// 0.2.x ABI for a given interface is frozen, so they resolve to the same impl.
// All importKey construction (both registration and lookup) goes through here
// so the two sides always agree.
func mkImportKey(iface, name string) importKey {
	if i := strings.IndexByte(iface, '@'); i >= 0 {
		iface = iface[:i]
	}
	return importKey{iface: iface, name: name}
}

// hostImport is a single registered import: its Go implementation plus the
// WIT parameter and result types the caller declared for it. The types are
// supplied by the caller because the binary decoder does not retain the func
// signatures declared inside an imported instance type (see the package doc).
//
// fn and asyncFn are mutually exclusive: WithImport sets fn (nil asyncFn);
// WithAsyncImport (async_host_import.go) sets asyncFn (nil fn). Which one is
// set must agree with whether the CONSUMING canon lower carries the async
// option -- graph.go's computeCanonHostFunc checks this at bind time and
// fails loud on a mismatch, since calling a sync HostFunc through the async
// calling convention (or vice versa) would silently misinterpret the core
// stack.
type hostImport struct {
	fn      HostFunc
	asyncFn AsyncHostFunc
	params  []binary.TypeDesc
	results []binary.TypeDesc

	// customFD/customResolve, when customFD is non-nil, replace params/results
	// entirely: buildHostWrapper uses this FuncDesc and resolver directly
	// instead of building them via synthFuncDesc. synthFuncDesc's table only
	// has one slot per top-level param/result (see its doc), so it cannot
	// express a genuinely nested composite type -- e.g. list<tuple<string,
	// string>>, where the tuple itself needs its own resolvable type index.
	// wasi.go's withImportCustom is the only caller that sets this; WithImport
	// always leaves it nil.
	customFD      *binary.FuncDesc
	customResolve abi.Resolver

	// asyncTarget, when set, routes an async-lowered call through this
	// import to a GUEST EXPORT of a sibling composition instance instead of
	// a Go AsyncHostFunc (Phase 3 guest<->guest async lower, docs/component-
	// model-async-phase3-design.md §3.1): delegatingHostImport
	// (composition.go/graph.go) sets both fn (the sync arm, unchanged) and
	// asyncTarget on the SAME hostImport, so a sibling's export can be
	// lowered either synchronously or asynchronously depending on what the
	// IMPORTER's own canon lower declares -- graph.go's computeCanonHostFunc
	// picks the arm. asyncFn and asyncTarget are mutually exclusive in
	// practice (a plain WithAsyncImport-registered hostImport never sets
	// asyncTarget, and delegatingHostImport never sets asyncFn), but nothing
	// enforces that beyond call-site discipline -- buildAsyncHostWrapper
	// checks asyncTarget first.
	asyncTarget *guestAsyncTarget

	// lineage marks a composition delegate that crosses a DIRECT static
	// parent<->child instantiation boundary: the outer's own local lift
	// handed to a child as a func instantiate-arg (outerFuncArgImport's
	// ComponentFuncFromCanonLift arm), or the outer's own canon lower of a
	// locally-nested child's export (computeCanonHostFunc's local-sibling
	// lower arm). The Component Model reference will eventually permit some
	// of these (entering_set ancestor exclusion, definitions.py:220-248),
	// but the conformance suite pins wasmtime's current behavior --
	// trap-on-reenter.wast's own "for now, trap on parent-to-child /
	// child-to-parent" -- so the call-time wrappers refuse them with the
	// spec's exact re-entrancy trap text.
	lineage bool
}

// guestAsyncTarget names the sibling composition instance/export an async
// lower through a delegating import (composition.go's delegatingHostImport)
// ultimately calls into (Phase 3, docs/component-model-async-phase3-design.md
// §3.1). provToImp is the same PROVIDER-resource-def -> IMPORTER-type-index
// mapping delegatingHostImport's sync arm uses, needed here too since
// mapArgsToProvider (async_host_import.go) performs the identical own/borrow
// crossing repToProviderHandle/providerHandleToRep do for the sync path.
type guestAsyncTarget struct {
	sub        *Instance
	be         *boundExport
	exportName string
	provToImp  func(uint32) (uint32, bool)
}

func newConfig(opts []Option) *config {
	c := &config{imports: make(map[importKey]*hostImport), resourceTags: make(map[importKey]uint32)}
	for _, o := range opts {
		o(c)
	}
	return c
}

// WithImport registers a Go implementation for a component import. iface is
// the imported interface name (e.g. "test:pkg/host"), name is the function
// name within it (e.g. "log"), and params/results are that function's WIT
// parameter and result types as abi/binary type descriptors (e.g.
// binary.PrimitiveDesc{Prim: "string"}).
func WithImport(iface, name string, fn HostFunc, params, results []binary.TypeDesc) Option {
	return func(c *config) {
		c.imports[mkImportKey(iface, name)] = &hostImport{fn: fn, params: params, results: results}
	}
}

// WithCompileCache opts Instantiate into reusing already-compiled core
// modules from cache instead of recompiling (re-JITting) them on every call.
// Pass the SAME *CompileCache across repeated Instantiate calls for the SAME
// component (and the same Runtime -- see CompileCache's doc for the
// Runtime-pairing rule) to skip redundant compilation of its embedded core
// modules; the first Instantiate against a given component populates the
// cache, later ones hit it.
//
// Omitting this option (the default) preserves the exact prior behavior:
// every Instantiate call recompiles its core modules from scratch.
func WithCompileCache(cache *CompileCache) Option {
	return func(c *config) {
		c.compileCache = cache
	}
}

// withHTTPHost records the wasi:http server state on the config so the two
// instantiation paths that support host imports can copy it onto the Instance
// (for ServeHTTP). Set by wasiHTTPOptions.
func withHTTPHost(h *wasiHTTP) Option {
	return func(c *config) { c.httpHost = h }
}

// withResourcesHook registers hook to run against the Instance's
// *handleTable as soon as it is created (before any host func is invoked).
//
// liftHostArgs/lowerHostResults (this file) resolve an own<T>/borrow<T>
// handle <-> host-rep automatically, but only for a func's top-level
// params/results (resolveHandleArg/allocHandleResult, called once per
// top-level entry) -- see their docs. A HostFunc whose own<T> is nested
// inside a composite result (e.g. wasi:filesystem/preopens.get-directories'
// list<tuple<own<descriptor>,string>>, or wasi:filesystem/types'
// [method]descriptor.open-at's result<descriptor,error-code>) must mint that
// handle itself via resources.NewOwn, since nothing upstream will do it for
// a nested position. Such a HostFunc needs its own reference to the same
// per-Instance table, which does not otherwise exist until instantiation
// begins (well after an Option's closures are built) -- withResourcesHook
// is how it gets one. Used by wasi.go's filesystem host funcs; ordinary
// WithImport-registered funcs never need it.
func withResourcesHook(hook func(*handleTable)) Option {
	return func(c *config) {
		c.resourceHooks = append(c.resourceHooks, hook)
	}
}

// runResourceHooks invokes every hook cfg registered via withResourcesHook
// against resources. Called once per Instantiate, immediately after
// newHandleTable() by the graph engine (graph.go's instantiateGraph).
func runResourceHooks(cfg *config, resources *handleTable) {
	for _, hook := range cfg.resourceHooks {
		hook(resources)
	}
}

// withResourceTag records that the resource named name, exported by
// imported interface iface (e.g. ("wasi:filesystem/types@0.2.3",
// "descriptor")), is the same logical resource an Option's own<T>/
// borrow<T> declarations tag with ResourceType tag.
//
// # Why this mapping has to exist
//
// A resource type has two entirely separate numberings in play at once:
//
//   - The real component binary's own type index (whatever canon.TypeIdx
//     names for a `canon resource.new/drop/rep` core func the GUEST calls
//     directly -- e.g. when its generated bindings drop an owned
//     descriptor handle). This index is specific to one particular .wasm
//     file's type section/alias layout and cannot be known in advance.
//
//   - The caller-chosen ResourceType tag an Option's own<T>/borrow<T>
//     TypeDesc uses (WithImport/withImportCustom): since this package's
//     decoder cannot retain an imported instance type's nested func
//     signatures (see wasi.go's package doc), WithImport's caller supplies
//     the FuncDesc by hand, including a self-chosen, wasm-binary-agnostic
//     resource tag (e.g. wasi.go's wasiOutputStreamResType) -- the SAME
//     tag reused across every host func that mints or resolves a handle
//     for that resource, entirely independent of any one guest binary.
//
// Both numberings key into the very same handleTable (resource.go), which
// cross-checks a handle's minting tag against every later operation's tag
// (mirroring the Canonical ABI's `trap_if(h.rt is not rt)`). Left alone,
// that check trips the moment BOTH numberings touch the same handle: a
// WithImport-registered host func mints an own<descriptor> handle tagged
// with wasi.go's constant, and the guest later drops it via a
// resource.drop canon tagged with the real (unrelated) wasm type index --
// two different numbers naming what both sides intend as the same
// resource. This was invisible until a real guest's compiled glue
// (rustc's wasi_snapshot_preview1 adapter, populating its preopens table)
// actually dropped an owned host-resource handle -- no earlier fixture's
// guest code did.
//
// withResourceTag closes that gap: resourceCanonHostFunc/
// resourceCanonHostFuncGraph, given a resource.new/drop/rep canon, try to
// trace its TypeIdx back to an (iface, name) pair the same way
// importInterfaceName resolves a lowered import's target (only possible
// when the type index names a type-sort alias exporting from an *imported*
// instance -- see effectiveResourceTypeIdx); if that succeeds and iface+
// name is registered here, the REAL wasm-level index is translated to tag
// before touching resources, so both numberings agree. A resource this
// package doesn't recognize (not registered, or genuinely guest-defined,
// e.g. real_adder's own resource type) falls back to the raw TypeIdx
// unchanged -- exactly today's existing, working behavior.
func withResourceTag(iface, name string, tag uint32) Option {
	return func(c *config) {
		c.resourceTags[mkImportKey(iface, name)] = tag
	}
}

// effectiveResourceTypeIdx translates canon.TypeIdx to the caller-chosen
// ResourceType tag registered (via withResourceTag) for the imported
// resource it names, if any; otherwise it returns canon.TypeIdx unchanged.
// See withResourceTag's doc for why this translation must happen at all.
func effectiveResourceTypeIdx(comp *binary.Component, cfg *config, typeIdx uint32) uint32 {
	iface, name, ok := resolveImportedResourceName(comp, typeIdx)
	if !ok {
		return typeIdx
	}
	if tag, ok := cfg.resourceTags[mkImportKey(iface, name)]; ok {
		return tag
	}
	return typeIdx
}

// resolveImportedResourceName reports the (imported interface, exported
// resource name) a component type index names, when that index is a
// type-sort alias exporting a name from one of this component's *imported*
// instances -- the shape a real WASI guest's `alias export $ifaceInst
// "descriptor" (type ...)` produces (comp.ResolveType cannot follow this
// alias structurally, per typespace.go's doc, but the alias's own Name +
// InstanceIdx fields are enough to identify which import+name it targets
// without needing the type's full structural definition). Reports ok=false
// for anything else: a locally-defined (guest-owned) resource type, an
// alias of some other shape, or a Component with no TypeSpace (e.g. a
// hand-built test Component that never went through Decode).
func resolveImportedResourceName(comp *binary.Component, typeIdx uint32) (iface, name string, ok bool) {
	if int(typeIdx) >= len(comp.TypeSpace) {
		return "", "", false
	}
	entry := comp.TypeSpace[typeIdx]
	if entry.Kind != binary.TypeSpaceAlias || int(entry.Alias) >= len(comp.Aliases) {
		return "", "", false
	}
	al := comp.Aliases[entry.Alias]
	if al.Sort != 0x03 || al.TargetKind != 0x00 { // type-sort export alias
		return "", "", false
	}
	ifaceName, err := importInterfaceName(comp, al.InstanceIdx)
	if err != nil {
		return "", "", false
	}
	return ifaceName, al.Name, true
}

// aliasTarget is a resolved (instance index, export name) pair naming what an
// alias brings into scope.
type aliasTarget struct {
	instIdx uint32
	name    string
}

// hostFuncDef is a built host function: its wazy adapter plus the core param
// and result value types it declares (which must match the guest's import).
type hostFuncDef struct {
	fn      api.GoModuleFunction
	params  []api.ValueType
	results []api.ValueType
}

// resourceCanonHostFunc builds the fixed-signature host func for a
// resource.new / resource.drop / resource.rep canon, operating on resources.
// Per the Canonical ABI these three core funcs always take/return plain i32
// handles/reps -- there is no FuncDesc/WIT type involved at this layer (own
// and borrow only appear at the *component* level, in the func types of
// exports/imports that use these canon-produced core funcs) -- so this
// bypasses the abi package entirely and talks to the handle table directly.
func resourceCanonHostFunc(comp *binary.Component, cfg *config, resources *handleTable, name string, canon binary.Canon) (hostFuncDef, error) {
	td, err := comp.ResolveType(canon.TypeIdx)
	if err != nil {
		return hostFuncDef{}, fmt.Errorf("component/instance: inline export %q: canon references type %d: %w", name, canon.TypeIdx, err)
	}
	if _, ok := td.(binary.ResourceDesc); !ok {
		return hostFuncDef{}, fmt.Errorf("component/instance: inline export %q: canon type %d is not a resource type (got %T)", name, canon.TypeIdx, td)
	}
	typeIdx := effectiveResourceTypeIdx(comp, cfg, canon.TypeIdx)

	switch canon.Kind {
	case 0x02: // resource.new: rep:i32 -> handle:i32
		fn := api.GoModuleFunc(func(_ context.Context, _ api.Module, stack []uint64) {
			rep := api.DecodeU32(stack[0])
			stack[0] = api.EncodeU32(resources.NewOwn(typeIdx, rep))
		})
		return hostFuncDef{fn: fn, params: []api.ValueType{api.ValueTypeI32}, results: []api.ValueType{api.ValueTypeI32}}, nil

	case 0x03: // resource.drop: handle:i32 -> ()
		fn := api.GoModuleFunc(func(_ context.Context, _ api.Module, stack []uint64) {
			h := api.DecodeU32(stack[0])
			if err := resources.Drop(typeIdx, h); err != nil {
				panic(fmt.Errorf("component/instance: resource.drop (type %d): %w", typeIdx, err))
			}
		})
		return hostFuncDef{fn: fn, params: []api.ValueType{api.ValueTypeI32}, results: nil}, nil

	case 0x04: // resource.rep: handle:i32 -> rep:i32
		fn := api.GoModuleFunc(func(_ context.Context, _ api.Module, stack []uint64) {
			h := api.DecodeU32(stack[0])
			rep, err := resources.Rep(typeIdx, h)
			if err != nil {
				panic(fmt.Errorf("component/instance: resource.rep (type %d): %w", typeIdx, err))
			}
			stack[0] = api.EncodeU32(rep)
		})
		return hostFuncDef{fn: fn, params: []api.ValueType{api.ValueTypeI32}, results: []api.ValueType{api.ValueTypeI32}}, nil

	default:
		return hostFuncDef{}, fmt.Errorf("component/instance: inline export %q: unsupported resource canon kind %#x", name, canon.Kind)
	}
}

// importInterfaceName maps a component instance index to the imported
// interface's name. The component instance index space is the imported
// instances in import order (nested component instances are already rejected).
func importInterfaceName(comp *binary.Component, instIdx uint32) (string, error) {
	count := uint32(0)
	for _, im := range comp.Imports {
		if im.ExternType != 0x05 { // instance
			continue
		}
		if count == instIdx {
			return im.Name, nil
		}
		count++
	}
	return "", fmt.Errorf("component instance index %d out of range of %d imported instances", instIdx, count)
}

// resolvePostReturnFunc looks for a post-return option (CanonOpt kind 0x05)
// on canon and, if present, resolves its core func index through the same
// core func index space bindFuncExport used for the lift itself (canon-
// produced funcs first, then coreFuncAliases), returning the core export
// name to call. It fails loud if the post-return func targets a different
// core instance than the lift's own core func (mainInstIdx) -- cross-instance
// post-return is not needed by any shape this package supports and would
// otherwise silently call the wrong module's export. Returns "", nil if the
// lift declares no post-return.
func resolvePostReturnFunc(canon binary.Canon, coreFuncAliases []aliasTarget, numProducedCoreFuncs int, mainInstIdx uint32) (string, error) {
	for _, opt := range canon.Opts {
		if opt.Kind != 0x05 { // post-return
			continue
		}
		cfi := int(opt.Idx)
		if cfi < numProducedCoreFuncs {
			return "", fmt.Errorf("post-return core func %d is a canon-produced func (lower/resource.*) rather than a real core export; unsupported", cfi)
		}
		ai := cfi - numProducedCoreFuncs
		if ai >= len(coreFuncAliases) {
			return "", fmt.Errorf("post-return core func %d out of range of the core func index space (%d canon-produced funcs + %d aliases)", cfi, numProducedCoreFuncs, len(coreFuncAliases))
		}
		tgt := coreFuncAliases[ai]
		if tgt.instIdx != mainInstIdx {
			return "", fmt.Errorf("post-return core func targets core instance %d, but the lift's core func targets core instance %d; cross-instance post-return is not supported", tgt.instIdx, mainInstIdx)
		}
		return tgt.name, nil
	}
	return "", nil
}

// validateShimComponent rejects any nested component that is not a pure
// re-export shim: every func it exports must resolve directly to one of its
// own func imports (see shimFuncImportNames), with nothing else in the
// nested component able to produce a func-sort index. A func-sort alias is
// also rejected even though the shims this milestone targets never emit one
// -- allowing it would silently mis-index shimFuncImportNames, which only
// accounts for imports.
func validateShimComponent(nested *binary.Component) error {
	if len(nested.CoreModules) > 0 || len(nested.CoreInstances) > 0 || len(nested.Canons) > 0 ||
		len(nested.Instances) > 0 || len(nested.NestedComponents) > 0 {
		return fmt.Errorf("not a pure re-export shim (has core module(s), core instance(s), canon(s), nested instance(s), or further nested component(s))")
	}
	for _, al := range nested.Aliases {
		if al.Sort == 0x01 { // func-sort alias: would occupy the func index space
			return fmt.Errorf("not a pure re-export shim (has a func-sort alias)")
		}
	}
	return nil
}

// shimFuncImportNames returns nested's func-sort import names in the order
// they occupy the func index space -- i.e. every import whose ExternType is
// func (0x01), in declaration order. validateShimComponent guarantees these
// are the shim's only possible func-sort producers, so a func export's
// ExternIndex indexes directly into this list.
func shimFuncImportNames(nested *binary.Component) []string {
	var names []string
	for _, im := range nested.Imports {
		if im.ExternType == 0x01 {
			names = append(names, im.Name)
		}
	}
	return names
}

// buildHostWrapper builds the wazy GoModuleFunction that adapts a HostFunc to
// the guest's lowered core calling convention: it lifts the flat core args
// (reading strings/lists from the calling module's memory, and resolving
// own<T>/borrow<T> handles to their host rep via resources), calls the
// HostFunc, and lowers the results back into the core return slots (again
// allocating a fresh handle for any own<T>/borrow<T> result).
//
// memOverride/reallocOverride, when non-nil, replace the module wazy passes
// the returned GoModuleFunc at call time as the source of linear memory /
// cabi_realloc for lift/lower, rather than deriving them from that runtime
// caller. Per the Canonical ABI, a canon lower's memory/realloc are static
// options fixed by the component binary (CanonOpt kinds 0x03/0x04) -- they
// need not be, and in a real multi-module graph often are not, the same
// module that literally executes the `call` instruction reaching this func.
// The graph engine's buildCanonHostModule (graph.go) resolves and passes
// these explicitly for exactly that reason: real_hello.component.wasm wires
// its WASI imports through an indirect call-table trampoline module (see
// graph.go's package doc) that has no memory of its own, so relying on the
// runtime caller would silently read/write nothing (see
// canonMemoryAndRealloc). A lowered import whose consumer is a real core
// module with its own memory passes nil, nil, since there the runtime caller
// already is the right module.
func buildHostWrapper(in *Instance, iface, funcName string, hi *hostImport, resources *handleTable, memOverride api.Module, reallocOverride api.Function) (api.GoModuleFunction, []api.ValueType, []api.ValueType, error) {
	var fd binary.FuncDesc
	var resolve abi.Resolver
	if hi.customFD != nil {
		fd, resolve = *hi.customFD, hi.customResolve
	} else {
		fd, resolve = synthFuncDesc(hi.params, hi.results)
	}

	rawParams, err := flattenRefs(fd.Params, resolve)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("component/instance: import %q func %q params: %w", iface, funcName, err)
	}
	// Whole-parameter-list spill: beyond MaxFlatParams, abi.FlattenFunc
	// (below) already collapses the core signature to a single i32 pointer
	// into the CALLING module's memory holding the whole param list stored
	// as a tuple (its own "Apply spill-to-memory rules" step) -- the guest
	// side already emits exactly this ABI unconditionally, so the only gap
	// was this wrapper's own arg-reading closure never understanding it (see
	// paramsSpill below and liftHostArgsSpilled). paramTupleDesc's Elements
	// reuse fd.Params' own TypeRefs directly, the same construction
	// instance.go's computeBoundExportABI uses for its (lift-direction)
	// paramTuple.
	paramsSpill := len(rawParams) > abi.MaxFlatParams
	var paramTupleDesc binary.TupleDesc
	if paramsSpill {
		elems := make([]binary.TypeRef, len(fd.Params))
		for i, p := range fd.Params {
			elems[i] = p.Type
		}
		paramTupleDesc = binary.TupleDesc{Elements: elems}
	}
	rawResults, err := flattenResultRefs(fd, resolve)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("component/instance: import %q func %q results: %w", iface, funcName, err)
	}
	// A result wider than MaxFlatResults is not returned on the core stack:
	// per the Canonical ABI's flatten_functype in "lower" context (exactly
	// what abi.FlattenFunc computes below), the guest instead appends one
	// extra i32 out-pointer parameter -- a buffer it already allocated -- and
	// expects the full (non-flat) value Store()d there, with no core return
	// values at all. outPtrIdx names that parameter's position on the
	// incoming stack (the flat width of the real params -- ONE slot when
	// paramsSpill, since the whole list already collapsed to its own
	// pointer -- since the out-pointer is appended after them); -1 means no
	// spilling is needed.
	outPtrIdx := -1
	if len(rawResults) > abi.MaxFlatResults {
		if paramsSpill {
			outPtrIdx = 1
		} else {
			outPtrIdx = len(rawParams)
		}
	}

	coreParams, coreResults, err := abi.FlattenFunc(fd, resolve, "lower")
	if err != nil {
		return nil, nil, nil, fmt.Errorf("component/instance: import %q func %q: flatten: %w", iface, funcName, err)
	}
	apiParams, err := toApiValueTypes(coreParams)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("component/instance: import %q func %q params: %w", iface, funcName, err)
	}
	apiResults, err := toApiValueTypes(coreResults)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("component/instance: import %q func %q results: %w", iface, funcName, err)
	}

	// Precompute the lift/lower plans ONCE, at bind time: the per-param type
	// resolution + flatten + memory/borrow facts, and the result type facts.
	// These are invariant for a fixed import signature, so the per-call closure
	// below skips them entirely (see liftHostArgsPlanned/lowerHostResultsPlanned)
	// -- the guest->host WASI path is the highest-frequency ABI direction and was
	// the only one still re-deriving all of this every call.
	paramPlans, err := buildHostParamPlans(fd, resolve)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("component/instance: import %q func %q params: %w", iface, funcName, err)
	}
	resultCount, resultPT, resultUsesMem, err := buildHostResultPlan(fd, resolve)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("component/instance: import %q func %q results: %w", iface, funcName, err)
	}

	fn := api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		if hi.lineage {
			panic(fmt.Errorf("component/instance: host import %q %q: wasm trap: cannot enter component instance", iface, funcName))
		}
		memMod := mod
		if memOverride != nil {
			memMod = memOverride
		}
		var args []abi.Value
		var lent []lentHandle
		if paramsSpill {
			args, lent, err = liftHostArgsSpilled(in, paramPlans, paramTupleDesc, resolve, api.DecodeU32(stack[0]), memMod, resources)
		} else {
			args, lent, err = liftHostArgsPlanned(in, paramPlans, resolve, stack, memMod, resources)
		}
		if err != nil {
			panic(fmt.Errorf("component/instance: host import %q %q: %w", iface, funcName, err))
		}
		// Release each borrow<T> arg's lend now that the call is about to run
		// and complete -- borrow lifetime is exactly this call.
		defer func() {
			for _, l := range lent {
				_ = resources.Unlend(l.tag, l.handle) //nolint:errcheck // best-effort release
			}
		}()
		// A sync canon lower whose callee is an async lift (callback or
		// stackful) is a potential block (docs/component-model-async-
		// stackful-design.md §4.3): a composition delegate
		// (hi.asyncTarget != nil, composition.go's delegatingHostImport)
		// reached from a core module's instantiation-time start function
		// must trap BEFORE the callee's core code ever runs -- the
		// callee's own blockingTask check fires too late (only once its
		// core code actually reaches a blocking builtin, or not at all if
		// it never does). Scoped to the instantiation window
		// (in.sched.instantiating) so host-entry sync invokes of an async
		// export keep today's drive-and-deadlock-detect behavior; zero
		// blast radius outside Instantiate.
		var blockingCaller taskBlocker
		if tgt := hi.asyncTarget; in != nil && tgt != nil && (tgt.be.asyncCallback || tgt.be.stackful) {
			// t.syncImplicit (a REAL plain-sync-lift call chain, installed by
			// invokeEntered whenever this instance's syncTaskNeeded is set --
			// e.g. by binding a thread.* canon, design
			// docs/component-model-async-threads-design-fable.md §8.3) always
			// traps here, matching blockingTask's own classification
			// uniformly: trap-if-block-and-sync's trap-if-sync-call-async1/2
			// (wast:290/292) are plain sync lifts reaching exactly this sync-
			// lower-into-async-lift site. activeTask == nil (no task at all --
			// syncTaskNeeded was never set for this instance) is narrower: it
			// only traps during the instantiation window (a start func can
			// never block); outside instantiation a host-entry sync call with
			// no task keeps the pre-existing drive-and-deadlock-detect
			// behavior other suites rely on.
			if t := in.activeTask; (t != nil && t.syncImplicit) || (in.sched.instantiating && t == nil) {
				panic(fmt.Errorf("component/instance: host import %q %q: cannot block a synchronous task before returning", iface, funcName))
			}
			// §4.4 (generalized by Feature 1 §1.4): when the CALLER is
			// itself a stackful task, OR a promoted callback task currently
			// inside a segment, route through startDelegatedFromBlocker
			// (parks instead of driving) rather than hi.fn's nested
			// sched.drive, which would frame-hold the caller's own
			// continuation beneath the callee's suspension --
			// async-calls-sync's confirmed livelock. A host-entry sync call
			// or a non-promoted callback-caller's own nested drive is
			// unaffected (t.blocker() nil there) and keeps using hi.fn
			// exactly as before.
			if t := in.activeTask; t != nil {
				blockingCaller = t.blocker()
			}
		}

		// Bracket the actual Go call: this is one of requireSchedulable's
		// (stream_host.go) two "provably on the driving goroutine" cases --
		// a StreamWriter/StreamReader/FutureWriter/FutureReader call made
		// synchronously from inside a sync host import is legal.
		if in != nil {
			in.inHostCall++
		}
		var results []abi.Value
		if blockingCaller != nil {
			results, err = startDelegatedFromBlocker(ctx, blockingCaller, hi.asyncTarget, args)
		} else {
			results, err = hi.fn(ctx, args)
		}
		if in != nil {
			in.inHostCall--
		}
		if err != nil {
			panic(fmt.Errorf("component/instance: host import %q %q: %w", iface, funcName, err))
		}
		var realloc abi.Realloc
		if reallocOverride != nil {
			realloc = reallocOfFunc(ctx, reallocOverride)
		}
		if err := lowerHostResultsPlanned(ctx, resultCount, resultPT, resultUsesMem, resolve, results, stack, memMod, resources, outPtrIdx, realloc); err != nil {
			panic(fmt.Errorf("component/instance: host import %q %q: %w", iface, funcName, err))
		}
	})
	return fn, apiParams, apiResults, nil
}

// synthFuncDesc builds a binary.FuncDesc plus a resolver from the caller's
// param/result type descriptors, so the abi package's FuncDesc-based
// operations (FlattenFunc, LiftFlat, LowerFlat) can be reused unchanged.
// Composite (non-primitive) descriptors are placed in a local type table the
// resolver closes over.
func synthFuncDesc(params, results []binary.TypeDesc) (binary.FuncDesc, abi.Resolver) {
	var table []binary.TypeDesc
	mkRef := func(td binary.TypeDesc) binary.TypeRef {
		if p, ok := td.(binary.PrimitiveDesc); ok {
			return binary.TypeRef{Primitive: p.Prim}
		}
		idx := uint32(len(table))
		table = append(table, td)
		return binary.TypeRef{TypeIndex: &idx}
	}

	fd := binary.FuncDesc{}
	for i, p := range params {
		fd.Params = append(fd.Params, binary.FuncParam{Name: fmt.Sprintf("p%d", i), Type: mkRef(p)})
	}
	for i, rres := range results {
		fd.Results.Named = append(fd.Results.Named, binary.FuncResult{Name: fmt.Sprintf("r%d", i), Type: mkRef(rres)})
	}
	resolve := func(idx uint32) binary.TypeDesc {
		if int(idx) >= len(table) {
			return nil
		}
		return table[idx]
	}
	return fd, resolve
}

// flattenRefs concatenates the flat core types of each param.
func flattenRefs(params []binary.FuncParam, resolve abi.Resolver) ([]string, error) {
	var out []string
	for _, p := range params {
		pt, err := resolveTypeRef(&p.Type, resolve)
		if err != nil {
			return nil, err
		}
		f, err := abi.Flatten(pt, resolve)
		if err != nil {
			return nil, err
		}
		out = append(out, f...)
	}
	return out, nil
}

// flattenResultRefs concatenates the flat core types of each result.
func flattenResultRefs(fd binary.FuncDesc, resolve abi.Resolver) ([]string, error) {
	var out []string
	for _, ref := range funcResultTypeRefs(fd) {
		rt, err := resolveTypeRef(&ref, resolve)
		if err != nil {
			return nil, err
		}
		f, err := abi.Flatten(rt, resolve)
		if err != nil {
			return nil, err
		}
		out = append(out, f...)
	}
	return out, nil
}

// hostParamPlan is the bind-time-precomputed lift plan for one host-import
// param: the type resolution, flattening, memory-need and borrow bookkeeping
// that liftHostArgs otherwise re-derives on every guest->host call, hoisted out
// of the hot path (buildHostWrapper computes these once; see liftHostArgsPlanned).
type hostParamPlan struct {
	pt       binary.TypeDesc
	flat     []string // precomputed abi.Flatten(pt)
	usesMem  bool
	isBorrow bool
	borrowRT uint32 // valid when isBorrow
}

// buildHostParamPlans precomputes the per-param lift plan for fd. Errors
// (type-resolve / flatten) surface here at bind time, exactly where
// flattenRefs already validates the same params.
func buildHostParamPlans(fd binary.FuncDesc, resolve abi.Resolver) ([]hostParamPlan, error) {
	plans := make([]hostParamPlan, len(fd.Params))
	for i := range fd.Params {
		pt, err := resolveTypeRef(&fd.Params[i].Type, resolve)
		if err != nil {
			return nil, fmt.Errorf("param %d: %w", i, err)
		}
		if typeContainsAsyncValueNested(pt, resolve, 0) {
			return nil, fmt.Errorf("param %d: stream/future/error-context nested inside a composite type is not supported by this milestone", i)
		}
		flat, err := abi.Flatten(pt, resolve)
		if err != nil {
			return nil, fmt.Errorf("param %d: %w", i, err)
		}
		pp := hostParamPlan{pt: pt, flat: flat, usesMem: usesMemory(pt, resolve)}
		if bd, ok := pt.(binary.BorrowDesc); ok {
			pp.isBorrow, pp.borrowRT = true, bd.ResourceType
		}
		plans[i] = pp
	}
	return plans, nil
}

// liftHostArgs lifts the flat core arguments on the stack into component-level
// argument values, per fd's parameter types. Convenience entry (used by tests
// and non-hot callers) that builds the param plans then defers to
// liftHostArgsPlanned; the hot guest->host wrapper builds the plans once at bind
// time instead (see buildHostWrapper).
func liftHostArgs(in *Instance, fd binary.FuncDesc, resolve abi.Resolver, stack []uint64, mod api.Module, resources *handleTable) ([]abi.Value, []lentHandle, error) {
	plans, err := buildHostParamPlans(fd, resolve)
	if err != nil {
		return nil, nil, err
	}
	return liftHostArgsPlanned(in, plans, resolve, stack, mod, resources)
}

// liftHostArgsPlanned is liftHostArgs with the per-param resolve/flatten/
// memory/borrow facts precomputed (plans). own<T>/borrow<T> params are lifted
// by abi as a bare handle (uint32, per abi.Value's documented mapping); this
// then resolves that handle to the host rep it names via resources, so the
// HostFunc receives the rep, not the raw handle -- see resolveHandleArg.
// resolve is still needed for LiftFlat's composite tree-walk.
func liftHostArgsPlanned(in *Instance, plans []hostParamPlan, resolve abi.Resolver, stack []uint64, mod api.Module, resources *handleTable) ([]abi.Value, []lentHandle, error) {
	mem, memAvailable := memoryBytesOf(mod)
	args := make([]abi.Value, len(plans))
	var lent []lentHandle
	pos := 0
	for i := range plans {
		pp := &plans[i]
		if pp.usesMem && !memAvailable {
			return nil, lent, fmt.Errorf("param %d requires linear memory (string/list), but the calling module has none", i)
		}
		cvs := make([]abi.CoreValue, len(pp.flat))
		for k := range pp.flat {
			if pos+k >= len(stack) {
				return nil, lent, fmt.Errorf("param %d: core stack underflow (need %d values, have %d)", i, pos+len(pp.flat), len(stack))
			}
			cvs[k] = abi.CoreValue{Kind: pp.flat[k], Bits: stack[pos+k]}
		}
		v, err := abi.LiftFlat(cvs, pp.pt, resolve, mem)
		if err != nil {
			return nil, lent, fmt.Errorf("param %d: lift: %w", i, err)
		}
		// Lifting a borrow<T> arg lends the resource for the call's duration, so
		// taking an own<T> of the SAME resource later in this arg list (or the
		// call) traps -- "cannot remove owned resource while borrowed". The lend
		// is released after the call (see the wrapper's Unlend). Done before
		// resolveHandleArg so a same-arg-list own-take sees the lend.
		if pp.isBorrow {
			if h, ok := v.(uint32); ok {
				if err := resources.Lend(pp.borrowRT, h); err == nil {
					lent = append(lent, lentHandle{pp.borrowRT, h})
				}
			}
		}
		v, err = resolveHandleArg(in, resources, nil, pp.pt, v)
		if err != nil {
			return nil, lent, fmt.Errorf("param %d: %w", i, err)
		}
		args[i] = v
		pos += len(pp.flat)
	}
	return args, lent, nil
}

// liftHostArgsSpilled is liftHostArgsPlanned's counterpart for a param list
// that flattens beyond abi.MaxFlatParams (see paramsSpill's construction in
// buildHostWrapper): the core func's one real param is a pointer into the
// CALLING module's memory holding the whole param list stored as a tuple,
// mirroring the LIFT side's whole-parameter-list spill (boundExport.
// paramsSpill/lowerParams's abi.SpillValue) in reverse -- this reads the
// tuple back out of memory instead of writing it, since this wrapper is the
// LOWER side receiving a caller's already-spilled args. Per-param
// post-processing (borrow lend, resolveHandleArg) is identical to
// liftHostArgsPlanned's, just sourced from the loaded tuple's elements
// instead of per-plan LiftFlat calls -- see async_host_import.go's
// liftAsyncHostArgsSpilled for the async-lower twin of this function.
func liftHostArgsSpilled(in *Instance, plans []hostParamPlan, tupleDesc binary.TupleDesc, resolve abi.Resolver, ptr uint32, mod api.Module, resources *handleTable) ([]abi.Value, []lentHandle, error) {
	mem, memAvailable := memoryBytesOf(mod)
	if !memAvailable {
		return nil, nil, fmt.Errorf("parameter list spills to memory (flattens beyond the flat limit), but the calling module has no memory")
	}
	tupleVal, err := abi.Load(mem, ptr, tupleDesc, resolve)
	if err != nil {
		return nil, nil, fmt.Errorf("spilled parameter list: %w", err)
	}
	raw, ok := tupleVal.([]abi.Value)
	if !ok || len(raw) != len(plans) {
		return nil, nil, fmt.Errorf("spilled parameter list: expected %d value(s), got %T", len(plans), tupleVal)
	}
	args := make([]abi.Value, len(plans))
	var lent []lentHandle
	for i := range plans {
		pp := &plans[i]
		v := raw[i]
		if pp.isBorrow {
			if h, ok := v.(uint32); ok {
				if err := resources.Lend(pp.borrowRT, h); err == nil {
					lent = append(lent, lentHandle{pp.borrowRT, h})
				}
			}
		}
		v, err = resolveHandleArg(in, resources, nil, pp.pt, v)
		if err != nil {
			return nil, lent, fmt.Errorf("param %d: %w", i, err)
		}
		args[i] = v
	}
	return args, lent, nil
}

// lentHandle records a borrow<T> arg's resource lend so the host wrapper can
// release it (Unlend) once the call returns.
type lentHandle struct {
	tag    uint32
	handle uint32
}

// resolveHandleArg translates a lifted own<T>/borrow<T> argument -- a bare
// guest handle (uint32), per abi's own/borrow-as-i32 mapping -- into the host
// rep it names, via resources. own consumes the handle (ownership transfers
// to the host, mirroring lift_own); borrow only reads it (lift_borrow),
// leaving the handle valid in the guest's table. Any other type passes v
// through unchanged.
//
// in is the owning Instance, threaded through only so a StreamDesc/FutureDesc
// case (Phase 2) can build a *StreamReader/*FutureReader that knows which
// instance's scheduler it belongs to (requireSchedulable, stream_host.go).
// nil is safe: requireSchedulable treats a nil Instance as unchecked, which
// only pure resource-handle unit tests (no stream/future args) ever hit.
func resolveHandleArg(in *Instance, resources *handleTable, canon func(uint32) uint32, pt binary.TypeDesc, v abi.Value) (abi.Value, error) {
	tag := func(rt uint32) uint32 {
		if canon != nil {
			return canon(rt)
		}
		return rt
	}
	switch d := pt.(type) {
	case binary.OwnDesc:
		h, ok := v.(uint32)
		if !ok {
			return nil, fmt.Errorf("own<%d> arg: expected a uint32 handle, got %T", d.ResourceType, v)
		}
		rep, err := resources.TakeOwn(tag(d.ResourceType), h)
		if err != nil {
			return nil, fmt.Errorf("own<%d> arg: %w", d.ResourceType, err)
		}
		return rep, nil
	case binary.BorrowDesc:
		h, ok := v.(uint32)
		if !ok {
			return nil, fmt.Errorf("borrow<%d> arg: expected a uint32 handle, got %T", d.ResourceType, v)
		}
		rep, err := resources.Rep(tag(d.ResourceType), h)
		if err != nil {
			return nil, fmt.Errorf("borrow<%d> arg: %w", d.ResourceType, err)
		}
		return rep, nil

	case binary.StreamDesc:
		// A stream<T> import ARG lift takes (removes) the guest's readable
		// end and hands the host a *StreamReader wrapping its shared object
		// -- the host is now "the other end" (design doc §2.2/§3.1). Needs
		// an *Instance for the reader's requireSchedulable bracket, so this
		// case is a no-op when resources belongs to a resources-only test
		// harness with no owning Instance (inst is nil): callers that reach
		// this path for real always have one (liftHostArgsPlanned/
		// liftAsyncHostArgsPlanned pass the owning Instance via resolveHandleArgInst).
		h, ok := v.(uint32)
		if !ok {
			return nil, fmt.Errorf("stream arg: expected a uint32 handle, got %T", v)
		}
		var elemDesc binary.TypeDesc
		if d.Element != nil {
			var eerr error
			var resolveFn abi.Resolver
			if in != nil {
				resolveFn = in.resolve
			}
			if elemDesc, eerr = resolveTypeRef(d.Element, resolveFn); eerr != nil {
				return nil, fmt.Errorf("stream arg: element type: %w", eerr)
			}
		}
		shared, err := takeReadableStreamEnd(in, resources, elemDesc, h)
		if err != nil {
			return nil, fmt.Errorf("stream arg: %w", err)
		}
		return &StreamReader{in: in, shared: shared, state: copyIdle}, nil

	case binary.FutureDesc:
		h, ok := v.(uint32)
		if !ok {
			return nil, fmt.Errorf("future arg: expected a uint32 handle, got %T", v)
		}
		var elemDesc binary.TypeDesc
		if d.Element != nil {
			var eerr error
			var resolveFn abi.Resolver
			if in != nil {
				resolveFn = in.resolve
			}
			if elemDesc, eerr = resolveTypeRef(d.Element, resolveFn); eerr != nil {
				return nil, fmt.Errorf("future arg: element type: %w", eerr)
			}
		}
		shared, err := takeReadableFutureEnd(in, resources, elemDesc, h)
		if err != nil {
			return nil, fmt.Errorf("future arg: %w", err)
		}
		return &FutureReader{in: in, shared: shared, state: copyIdle}, nil

	case binary.PrimitiveDesc:
		if d.Prim != "error-context" {
			return v, nil
		}
		h, ok := v.(uint32)
		if !ok {
			return nil, fmt.Errorf("error-context arg: expected a uint32 handle, got %T", v)
		}
		// Lift is a GET, not a remove: the sender keeps its own handle.
		raw, ok2 := resources.getEntry(h)
		ec, isEC := raw.(*errorContext)
		if !ok2 || !isEC {
			return nil, fmt.Errorf("error-context arg: handle %d is not an error-context", h)
		}
		return ec, nil

	default:
		return v, nil
	}
}

// allocHandleResult translates a HostFunc's own<T>/borrow<T> result -- a
// host rep (uint32) -- into a freshly minted guest handle for it, via
// resources (mirrors lower_own / lower_borrow). Any other type passes v
// through unchanged.
func allocHandleResult(resources *handleTable, rt binary.TypeDesc, v abi.Value) (abi.Value, error) {
	switch d := rt.(type) {
	case binary.OwnDesc:
		rep, ok := v.(uint32)
		if !ok {
			return nil, fmt.Errorf("own<%d> result: expected a uint32 rep, got %T", d.ResourceType, v)
		}
		return resources.NewOwn(d.ResourceType, rep), nil
	case binary.BorrowDesc:
		rep, ok := v.(uint32)
		if !ok {
			return nil, fmt.Errorf("borrow<%d> result: expected a uint32 rep, got %T", d.ResourceType, v)
		}
		return resources.NewBorrow(d.ResourceType, rep), nil

	case binary.StreamDesc:
		// An import's stream<T> RESULT: the host CREATED the stream (via
		// NewStream, whose second return value is the *sharedStream
		// identity) and hands it to the guest -- mint a fresh readable end
		// in the guest's table (mirrors lower_stream). Also accepts a
		// *StreamReader directly (a reader the host is transferring away),
		// per the design doc's "(or a writer-created reader arg)" note.
		var shared *sharedStream
		switch sv := v.(type) {
		case *sharedStream:
			shared = sv
		case *StreamReader:
			shared = sv.shared
		default:
			return nil, fmt.Errorf("stream result: expected a *sharedStream (from NewStream) or *StreamReader, got %T", v)
		}
		return resources.addEntry(&streamEnd{side: sideReadable, state: copyIdle, shared: shared}), nil

	case binary.FutureDesc:
		var shared *sharedFuture
		switch sv := v.(type) {
		case *sharedFuture:
			shared = sv
		case *FutureReader:
			shared = sv.shared
		default:
			return nil, fmt.Errorf("future result: expected a *sharedFuture (from NewFuture) or *FutureReader, got %T", v)
		}
		return resources.addEntry(&futureEnd{side: sideReadable, state: copyIdle, shared: shared}), nil

	case binary.PrimitiveDesc:
		if d.Prim != "error-context" {
			return v, nil
		}
		switch ev := v.(type) {
		case *errorContext:
			return resources.addEntry(ev), nil
		case string:
			return resources.addEntry(&errorContext{debugMessage: ev}), nil
		default:
			return nil, fmt.Errorf("error-context result: expected a *errorContext or string, got %T", v)
		}

	default:
		return v, nil
	}
}

// lowerHostResults lowers the HostFunc's result values back into the core
// return slots at the front of the stack, per fd's result types. An
// own<T>/borrow<T> result is expected as a host rep (uint32) and is
// allocated a fresh guest handle via resources before being lowered -- see
// allocHandleResult. outPtrIdx, when >= 0, names the stack slot holding the
// out-pointer buildHostWrapper's caller pre-computed for a result too wide
// to return flat (see its doc): the result is Store()d into guest memory
// there instead of written to the (in that case, empty) core return slots.
// realloc, when non-nil, overrides the default reallocOf(ctx, mod) (see
// buildHostWrapper's memOverride/reallocOverride doc for why a caller in a
// multi-module graph may need to supply the canon's own declared realloc
// rather than deriving one from mod).
func lowerHostResults(ctx context.Context, fd binary.FuncDesc, resolve abi.Resolver, results []abi.Value, stack []uint64, mod api.Module, resources *handleTable, outPtrIdx int, realloc abi.Realloc) error {
	resultCount, rt, resultUsesMem, err := buildHostResultPlan(fd, resolve)
	if err != nil {
		return err
	}
	return lowerHostResultsPlanned(ctx, resultCount, rt, resultUsesMem, resolve, results, stack, mod, resources, outPtrIdx, realloc)
}

// buildHostResultPlan precomputes the result-lower facts for fd: the declared
// result count, and (when exactly one) the resolved result type and whether it
// needs linear memory. resultCount 0 or >1 is left for lowerHostResultsPlanned
// to handle at call time, exactly as before (the >1 case is a milestone limit,
// not a bind-time error).
func buildHostResultPlan(fd binary.FuncDesc, resolve abi.Resolver) (resultCount int, rt binary.TypeDesc, usesMem bool, err error) {
	refs := funcResultTypeRefs(fd)
	resultCount = len(refs)
	if resultCount != 1 {
		return resultCount, nil, false, nil
	}
	rt, err = resolveTypeRef(&refs[0], resolve)
	if err != nil {
		return resultCount, nil, false, fmt.Errorf("result: %w", err)
	}
	if typeContainsAsyncValueNested(rt, resolve, 0) {
		return resultCount, nil, false, fmt.Errorf("result: stream/future/error-context nested inside a composite type is not supported by this milestone")
	}
	return resultCount, rt, usesMemory(rt, resolve), nil
}

// lowerHostResultsPlanned is lowerHostResults with the result count / resolved
// type / memory-need precomputed (see buildHostResultPlan). resolve is still
// needed for LowerFlat/Store's composite tree-walk.
func lowerHostResultsPlanned(ctx context.Context, resultCount int, rt binary.TypeDesc, resultUsesMem bool, resolve abi.Resolver, results []abi.Value, stack []uint64, mod api.Module, resources *handleTable, outPtrIdx int, realloc abi.Realloc) error {
	if len(results) != resultCount {
		return fmt.Errorf("returned %d result(s), but the import declares %d", len(results), resultCount)
	}
	if resultCount == 0 {
		return nil
	}
	if resultCount > 1 {
		return fmt.Errorf("declares %d results; multiple host-func results are not supported by this milestone", resultCount)
	}

	mem, memAvailable := memoryBytesOf(mod)
	if resultUsesMem && !memAvailable {
		return fmt.Errorf("result requires linear memory (string/list), but the calling module has none")
	}
	resultVal, err := allocHandleResult(resources, rt, results[0])
	if err != nil {
		return fmt.Errorf("result: %w", err)
	}
	if realloc.Call == nil {
		realloc = reallocOf(ctx, mod)
	}

	if outPtrIdx >= 0 {
		if !memAvailable {
			return fmt.Errorf("result must be returned via a memory pointer (too wide to flatten), but the calling module has no memory")
		}
		if outPtrIdx >= len(stack) {
			return fmt.Errorf("result: out-pointer stack slot %d out of range (stack has %d value(s))", outPtrIdx, len(stack))
		}
		ptr := api.DecodeU32(stack[outPtrIdx])
		if err := abi.Store(mem, ptr, rt, resultVal, resolve, realloc); err != nil {
			return fmt.Errorf("result: store spilled result: %w", err)
		}
		return nil
	}

	flat, err := abi.LowerFlat(resultVal, rt, resolve, realloc, mem)
	if err != nil {
		return fmt.Errorf("result: lower: %w", err)
	}
	for k, cv := range flat {
		if k >= len(stack) {
			return fmt.Errorf("result: core stack overflow (need %d values, have %d)", len(flat), len(stack))
		}
		stack[k] = cv.Bits
	}
	return nil
}

// toApiValueTypes maps flat core type names to wazy api.ValueTypes. An empty
// input maps to nil (wazy's convention for no params / no results).
func toApiValueTypes(kinds []string) ([]api.ValueType, error) {
	if len(kinds) == 0 {
		return nil, nil
	}
	out := make([]api.ValueType, len(kinds))
	for i, k := range kinds {
		switch k {
		case "i32":
			out[i] = api.ValueTypeI32
		case "i64":
			out[i] = api.ValueTypeI64
		case "f32":
			out[i] = api.ValueTypeF32
		case "f64":
			out[i] = api.ValueTypeF64
		default:
			return nil, fmt.Errorf("unknown core type %q", k)
		}
	}
	return out, nil
}
