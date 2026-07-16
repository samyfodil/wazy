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

	// sharedResources and resCanon are set only when this instantiation is a
	// nested sub-component of a composition (see instantiateNestedInstances).
	// sharedResources is the one handle table every composed sub-instance
	// shares; resCanon maps this sub-instance's local resource type indices to
	// the composition-global id it and its siblings agree on. Both nil for a
	// flat instantiation, which then makes its own table and tags by raw index.
	sharedResources *handleTable
	resCanon        func(uint32) uint32

	// resBase is this sub-instance's global-id base (its own defined resources
	// get ids resBase+defIndex); resBaseNext is the composition-wide allocator
	// that hands out a distinct base to each sub-instance. Both zero/nil for a
	// flat instantiation.
	resBase     uint32
	resBaseNext *uint32
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
type hostImport struct {
	fn      HostFunc
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
func buildHostWrapper(iface, funcName string, hi *hostImport, resources *handleTable, memOverride api.Module, reallocOverride api.Function) (api.GoModuleFunction, []api.ValueType, []api.ValueType, error) {
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
	if len(rawParams) > abi.MaxFlatParams {
		return nil, nil, nil, fmt.Errorf("component/instance: import %q func %q has %d flat params, exceeding the flat limit; whole-parameter-list spilling is not supported by this milestone", iface, funcName, len(rawParams))
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
	// incoming stack (the flat width of the real params, since the
	// out-pointer is appended after them); -1 means no spilling is needed.
	outPtrIdx := -1
	if len(rawResults) > abi.MaxFlatResults {
		outPtrIdx = len(rawParams)
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

	fn := api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		memMod := mod
		if memOverride != nil {
			memMod = memOverride
		}
		args, err := liftHostArgs(fd, resolve, stack, memMod, resources)
		if err != nil {
			panic(fmt.Errorf("component/instance: host import %q %q: %w", iface, funcName, err))
		}
		results, err := hi.fn(ctx, args)
		if err != nil {
			panic(fmt.Errorf("component/instance: host import %q %q: %w", iface, funcName, err))
		}
		var realloc abi.Realloc
		if reallocOverride != nil {
			realloc = reallocOfFunc(ctx, reallocOverride)
		}
		if err := lowerHostResults(ctx, fd, resolve, results, stack, memMod, resources, outPtrIdx, realloc); err != nil {
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

// liftHostArgs lifts the flat core arguments on the stack into component-level
// argument values, per fd's parameter types, reading string/list backing data
// from the calling module's memory. own<T>/borrow<T> params are lifted by abi
// as a bare handle (uint32, per abi.Value's documented mapping); this
// function then resolves that handle to the host rep it names via resources,
// so the HostFunc receives the rep, not the raw handle -- see
// resolveHandleArg.
func liftHostArgs(fd binary.FuncDesc, resolve abi.Resolver, stack []uint64, mod api.Module, resources *handleTable) ([]abi.Value, error) {
	mem, memAvailable := memoryBytesOf(mod)
	args := make([]abi.Value, len(fd.Params))
	pos := 0
	for i, p := range fd.Params {
		pt, err := resolveTypeRef(&p.Type, resolve)
		if err != nil {
			return nil, fmt.Errorf("param %d: %w", i, err)
		}
		flat, err := abi.Flatten(pt, resolve)
		if err != nil {
			return nil, fmt.Errorf("param %d: %w", i, err)
		}
		if usesMemory(pt, resolve) && !memAvailable {
			return nil, fmt.Errorf("param %d requires linear memory (string/list), but the calling module has none", i)
		}
		cvs := make([]abi.CoreValue, len(flat))
		for k := range flat {
			if pos+k >= len(stack) {
				return nil, fmt.Errorf("param %d: core stack underflow (need %d values, have %d)", i, pos+len(flat), len(stack))
			}
			cvs[k] = abi.CoreValue{Kind: flat[k], Bits: stack[pos+k]}
		}
		v, err := abi.LiftFlat(cvs, pt, resolve, mem)
		if err != nil {
			return nil, fmt.Errorf("param %d: lift: %w", i, err)
		}
		v, err = resolveHandleArg(resources, nil, pt, v)
		if err != nil {
			return nil, fmt.Errorf("param %d: %w", i, err)
		}
		args[i] = v
		pos += len(flat)
	}
	return args, nil
}

// resolveHandleArg translates a lifted own<T>/borrow<T> argument -- a bare
// guest handle (uint32), per abi's own/borrow-as-i32 mapping -- into the host
// rep it names, via resources. own consumes the handle (ownership transfers
// to the host, mirroring lift_own); borrow only reads it (lift_borrow),
// leaving the handle valid in the guest's table. Any other type passes v
// through unchanged.
func resolveHandleArg(resources *handleTable, canon func(uint32) uint32, pt binary.TypeDesc, v abi.Value) (abi.Value, error) {
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
	refs := funcResultTypeRefs(fd)
	if len(results) != len(refs) {
		return fmt.Errorf("returned %d result(s), but the import declares %d", len(results), len(refs))
	}
	if len(refs) == 0 {
		return nil
	}
	if len(refs) > 1 {
		return fmt.Errorf("declares %d results; multiple host-func results are not supported by this milestone", len(refs))
	}

	rt, err := resolveTypeRef(&refs[0], resolve)
	if err != nil {
		return fmt.Errorf("result: %w", err)
	}
	mem, memAvailable := memoryBytesOf(mod)
	if usesMemory(rt, resolve) && !memAvailable {
		return fmt.Errorf("result requires linear memory (string/list), but the calling module has none")
	}
	resultVal, err := allocHandleResult(resources, rt, results[0])
	if err != nil {
		return fmt.Errorf("result: %w", err)
	}
	if realloc == nil {
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
