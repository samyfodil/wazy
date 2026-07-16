// Package instance instantiates a decoded WebAssembly component and exposes
// its exported functions for calling. It covers the M3 "spine": a real
// component runs end to end, both the no-import export direction (M3 STEP 2)
// and the host-import direction (M3 STEP 3, this file plus host_import.go).
//
// # Scope
//
// Two component shapes are supported, and anything outside them fails loudly
// with a specific, named error rather than silently returning a zero value:
//
//   - No-import (M3 STEP 2): one embedded core module, instantiated as the
//     component's single core instance with no arguments, exporting functions
//     defined purely via "canon lift" of a core function reached through a
//     single core-export alias.
//
//   - Host-import (M3 STEP 3): the component imports one or more instances of
//     host functions (e.g. `test:pkg/host` exporting `log: func(msg: string)`)
//     that are lowered (canon lower) into core functions the guest core module
//     imports and calls. The caller supplies Go implementations via WithImport.
//     See host_import.go for the lowering/wrapper machinery.
//
// The host-import path also handles a component with zero WIT imports whose
// world *exports an interface* rather than a bare function: wit-component
// packages the lifted funcs into a nested "re-export shim" component (a
// second, fully embedded component -- binary.Component.NestedComponents)
// and an Instance that instantiates it, with the top-level export naming
// that instance. See resolveInstanceExport / bindInstanceExport in
// host_import.go for how that shim is resolved back to the outer canon
// lifts, and CallExport for how a caller reaches a function inside it. A
// canon lift's post-return option (e.g. on a function returning a string)
// is also wired here: the post-return core func is called with the lift's
// flat results immediately after lifting, per definitions.py's canon_lift.
//
// Still rejected in both: canon resource.* built-ins, non-func exports
// outside the re-export-shim shape above, multi-result functions,
// whole-parameter-list / result spilling to memory, string/list values when
// no linear memory is available, and any nested component beyond a pure
// re-export shim (e.g. the wasip2 CLI adapter shim, which embeds its own
// core module and instantiation graph -- out of scope here).
//
// # Decoder gaps worked around here
//
// The component binary decoder (internal/component/binary) does not retain the
// per-alias core:sort discriminator (func vs memory vs table vs global) on
// binary.AliasDef. Rather than require a binary change, this package
// disambiguates core-export aliases by probing the actually-instantiated
// module's exports (a name that resolves to a Function is a core-func alias; a
// memory alias does not). It also does not retain the func signatures declared
// inside an imported instance type, which is why the host-import direction
// takes the imported function's WIT type from the caller via WithImport rather
// than from the decoded component.
package instance

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/component/abi"
	"github.com/samyfodil/wazy/internal/component/binary"
)

// uint64SlicePool and coreValueSlicePool pool the []uint64/[]abi.CoreValue
// scratch buffers invoke/liftResult build to shuttle a call's flattened core
// values (see invoke's stack and liftResult's coreResults) -- both are pure
// scratch, fully consumed synchronously within the call that builds them
// (see their use sites for why each is safe to release the moment it's
// done), so reusing one buffer across calls instead of allocating fresh
// avoids a per-call allocation on the hottest part of the component call
// path.
//
// Pooled rather than cached per-boundExport/per-Instance: a sync.Pool's
// Get always hands back a buffer no other goroutine holds a reference to,
// so concurrent Call/CallExport calls against the same Instance (or even
// the same boundExport) never share one -- there is no shared mutable state
// for -race to catch here, unlike a per-boundExport field would be. Pool
// values are stored as pointers-to-slice (not slices directly), per the
// standard sync.Pool idiom: storing a slice value in the `any`-typed pool
// would itself box-allocate on every Put.
var (
	uint64SlicePool = sync.Pool{
		New: func() any {
			s := make([]uint64, 0, 8)
			return &s
		},
	}
	coreValueSlicePool = sync.Pool{
		New: func() any {
			s := make([]abi.CoreValue, 0, 8)
			return &s
		},
	}
)

// getUint64Slice returns a pooled []uint64 of exactly length n, ready to
// index/fill; putUint64Slice returns it (paired one-to-one) for reuse.
func getUint64Slice(n int) *[]uint64 {
	p := uint64SlicePool.Get().(*[]uint64)
	if cap(*p) < n {
		*p = make([]uint64, n)
	} else {
		*p = (*p)[:n]
	}
	return p
}

func putUint64Slice(p *[]uint64) {
	*p = (*p)[:0]
	uint64SlicePool.Put(p)
}

// getCoreValueSlice/putCoreValueSlice are getUint64Slice/putUint64Slice's
// []abi.CoreValue counterpart.
func getCoreValueSlice(n int) *[]abi.CoreValue {
	p := coreValueSlicePool.Get().(*[]abi.CoreValue)
	if cap(*p) < n {
		*p = make([]abi.CoreValue, n)
	} else {
		*p = (*p)[:n]
	}
	return p
}

func putCoreValueSlice(p *[]abi.CoreValue) {
	*p = (*p)[:0]
	coreValueSlicePool.Put(p)
}

// Instance is an instantiated WebAssembly component: the set of instantiated
// core/host modules plus, for each component-level exported function, the
// binding needed to call it through its canon lift signature.
type Instance struct {
	resolve abi.Resolver

	// exports maps a component export name to the binding that backs it.
	exports map[string]*boundExport

	// instanceExports indexes exports a second way, by instance name then
	// member name, for CallExport's fast path -- see buildInstanceExportIndex
	// and CallExport. Built once at bind time from exports (nil if none of
	// exports' keys follow the "instance#member" convention); never mutated
	// afterward, so concurrent CallExport calls read it safely with no lock.
	instanceExports map[string]map[string]instanceExportEntry

	// closers are every module instantiated for this component (core guest
	// modules and synthetic host modules), closed in reverse order by Close.
	closers []api.Module

	// resources is this instance's resource handle table (see resource.go),
	// shared by every resource type the instance declares or imports. It is
	// always non-nil, even for instances with no resource canons, so callers
	// never need a nil check.
	resources *handleTable

	// coreModuleCount is the number of EMBEDDED core modules (binary.Component
	// CoreModules, instantiated via a Kind == 0x00 "instantiate" core
	// instance) this Instance wired up -- set only by the general graph
	// engine (graph.go); zero for the simpler single-module paths, which
	// callers can already infer have exactly one. See CoreModuleCount.
	coreModuleCount int

	// wasiCalls records, in the order the graph engine wired them, the
	// "iface.func" name of every canon lower it resolved (WASI or not) --
	// set only by the general graph engine. See WASICalls.
	wasiCalls []string

	// httpHost is the wasi:http server state, non-nil only when the component
	// was instantiated with WithWASI(WASIConfig{EnableHTTP: true}). ServeHTTP
	// drives the guest's exported incoming-handler through it.
	httpHost *wasiHTTP
}

// CoreModuleCount returns the number of embedded core modules (real,
// non-synthetic core wasm binaries from the component's CoreModules section)
// this Instance instantiated. Only meaningful for a component instantiated
// via the general graph engine (graph.go), i.e. one with more than one
// embedded core module; see needsGraphPath.
func (in *Instance) CoreModuleCount() int { return in.coreModuleCount }

// WASICalls returns the "iface.func" name of every canon-lower this
// Instance's graph engine wired up, in the order they were declared in the
// component binary. Only meaningful for a component instantiated via the
// general graph engine (graph.go).
func (in *Instance) WASICalls() []string { return in.wasiCalls }

// boundExport binds a component export to the core function that implements
// it and to the module that provides the linear memory / cabi_realloc used to
// lower parameters and lift results across the call.
type boundExport struct {
	mod      api.Module      // exports funcName; its Memory() backs lower/lift
	funcName string          // core export name to call
	fd       binary.FuncDesc // component-level func type (lift signature)

	// postReturnFuncName is the core export to call, on mod, after lifting
	// the result -- the canon lift's post-return option (CanonOpt kind
	// 0x05). Empty when the lift declares no post-return. Per
	// definitions.py's canon_lift, it is called with the same flat core
	// values the lift produced (not the lifted abi.Value), after lifting has
	// finished reading them, so the guest can free/reuse any memory the call
	// allocated (e.g. a returned string's backing bytes).
	postReturnFuncName string

	// coreFn/postReturnFn/reallocFn are api.Function handles resolved ONCE,
	// at bind time (see finalizeBoundExport), instead of via a fresh
	// mod.ExportedFunction lookup on every invoke() call. This matters
	// because the native engine allocates a whole callEngine (~10KB) per
	// ExportedFunction(name).Call -- see
	// internal/engine/native/call_engine.go's NewFunction doc, "resolve
	// once, reuse" -- and invoke() used to call ExportedFunction 2-3 times
	// per Call (core func, cabi_realloc, post-return).
	//
	// coreFn/postReturnFn may be nil (mod doesn't export the name); invoke()
	// keeps the exact same nil-check/error message it always surfaced from a
	// failed lookup, just against the cached field instead of a fresh one.
	// reallocFn is nil when mod exports no "cabi_realloc" -- reallocOf's
	// lazy nil-check semantics (fail only when actually needed) are
	// preserved by cachedReallocOf.
	coreFn       api.Function
	postReturnFn api.Function
	reallocFn    api.Function

	// The fields below cache fd's ABI flattening / type resolution, computed
	// once at bind time (finalizeBoundExport) instead of on every invoke()
	// call -- fd is immutable once bound, so LowerFlat/LiftFlat.
	// resolveTypeRef/abi.Flatten/abi.FlattenFunc always recompute the exact
	// same values for the life of the boundExport. A resolution/flatten
	// error is cached rather than failing bind, so it still surfaces from
	// the same call-time code path (with the same message) as before this
	// caching existed -- see finalizeBoundExport's doc.
	coreParamsWant, coreResultsWant []string
	flattenErr                      error

	paramTypes      []binary.TypeDesc
	paramUsesMemory []bool
	paramErrs       []error

	hasResult        bool
	tooManyResults   int // > 0: fd declares this many (>1) named results
	resultType       binary.TypeDesc
	resultUsesMemory bool
	resultFlatKinds  []string
	resultErr        error
}

// finalizeBoundExport resolves be's core func handles and precomputes its ABI
// flattening/type-resolution metadata, both exactly once. Called by every
// site that constructs a boundExport (instantiateComponent, bindFuncExport,
// bindFuncExportGraph), after be.mod/funcName/fd/postReturnFuncName are set.
//
// Deliberately never fails: a lookup or resolution that would have errored
// happens exactly as before this existed, just later -- invoke()/lowerParams/
// liftResult check the cached field (a nil api.Function, or a non-nil cached
// error) at call time and produce the identical error message a fresh
// lookup/resolve would have. This keeps bind-time behavior (and every
// existing error-surfaces-from-Call test) unchanged while still doing the
// resolution/computation only once rather than on every invoke() call.
func finalizeBoundExport(be *boundExport, resolve abi.Resolver) {
	be.coreFn = be.mod.ExportedFunction(be.funcName)
	if be.postReturnFuncName != "" {
		be.postReturnFn = be.mod.ExportedFunction(be.postReturnFuncName)
	}
	be.reallocFn = be.mod.ExportedFunction("cabi_realloc")

	be.coreParamsWant, be.coreResultsWant, be.flattenErr = abi.FlattenFunc(be.fd, resolve, "lift")

	be.paramTypes = make([]binary.TypeDesc, len(be.fd.Params))
	be.paramUsesMemory = make([]bool, len(be.fd.Params))
	be.paramErrs = make([]error, len(be.fd.Params))
	for i, p := range be.fd.Params {
		pt, err := resolveTypeRef(&p.Type, resolve)
		if err != nil {
			be.paramErrs[i] = err
			continue
		}
		be.paramTypes[i] = pt
		be.paramUsesMemory[i] = usesMemory(pt, resolve)
	}

	resultRefs := funcResultTypeRefs(be.fd)
	switch {
	case len(resultRefs) > 1:
		be.tooManyResults = len(resultRefs)
	case len(resultRefs) == 1:
		be.hasResult = true
		rt, err := resolveTypeRef(&resultRefs[0], resolve)
		if err != nil {
			be.resultErr = err
			return
		}
		be.resultType = rt
		be.resultUsesMemory = usesMemory(rt, resolve)
		flatKinds, err := abi.Flatten(rt, resolve)
		if err != nil {
			be.resultErr = err
			return
		}
		be.resultFlatKinds = flatKinds
	}
}

// Instantiate decodes componentBytes as a WebAssembly component, instantiates
// its embedded core module(s) into r (registering caller-provided host
// implementations for any imports), and wires up the export -> canon lift ->
// core func bindings needed to call exported functions via Call.
//
// See the package doc for exactly which component shapes are supported;
// anything outside them is rejected with a descriptive error.
func Instantiate(ctx context.Context, r wazy.Runtime, componentBytes []byte, opts ...Option) (*Instance, error) {
	comp, err := binary.Decode(bytes.NewReader(componentBytes))
	if err != nil {
		return nil, fmt.Errorf("component/instance: decode component: %w", err)
	}

	cfg := newConfig(opts)

	if needsGraphPath(comp) {
		return instantiateGraph(ctx, r, comp, componentBytes, cfg)
	}
	if needsImportPath(comp) {
		return instantiateWithImports(ctx, r, comp, componentBytes, cfg)
	}
	return instantiateComponent(ctx, r, comp, componentBytes)
}

// needsGraphPath reports whether comp needs the general multi-core-module
// instantiation graph engine (graph.go) rather than instantiateWithImports.
//
// instantiateWithImports already tolerates more than one embedded core
// module (it walks comp.CoreInstances generically, Kind 0x00 or 0x01, with
// no count restriction) -- e.g. log_hello.wasm, a wit-bindgen-produced
// fixture with a main module plus a small preview1-to-preview2 adapter
// module, already exercises that and is proven working. What it cannot
// represent is:
//
//   - an inline-export core instance regrouping anything other than a func
//     (a memory or table alias) -- see graph.go's shim-based mechanism, or
//   - a core func index space where canon-produced funcs (lower,
//     resource.*) and core-level func aliases genuinely interleave --
//     instantiateWithImports assumes every canon-produced func occupies a
//     lower index than every alias-produced func (see its doc), which
//     CoreFuncSpace's declaration-order tracking can now check precisely.
//
// So this checks exactly those two structural properties, rather than
// something coarser like core-module count: a coarser check would reroute
// already-working fixtures like log_hello to the newer, less battle-tested
// engine for no reason, at real regression risk. Only a component that
// actually needs the generalization (e.g. real_hello.component.wasm, a
// genuine wasip2 CLI adapter graph) trips this.
func needsGraphPath(comp *binary.Component) bool {
	for _, ci := range comp.CoreInstances {
		if ci.Kind != 0x01 {
			continue
		}
		for _, e := range ci.Exports {
			if e.Sort != 0x00 { // not a func: memory/table/global/...
				return true
			}
		}
	}
	return !coreFuncSpacePartitioned(comp.CoreFuncSpace)
}

// coreFuncSpacePartitioned reports whether space fits instantiateWithImports'
// assumption: every CoreFuncFromCanon entry occupies a lower index than
// every CoreFuncFromAlias entry (either group may be empty). An empty space
// (no Decode-populated CoreFuncSpace, e.g. a hand-built test Component, or a
// component with no core func aliases/canons at all) trivially fits.
func coreFuncSpacePartitioned(space []binary.CoreFuncSpaceEntry) bool {
	sawAlias := false
	for _, e := range space {
		switch e.Kind {
		case binary.CoreFuncFromAlias:
			sawAlias = true
		case binary.CoreFuncFromCanon:
			if sawAlias {
				return false
			}
		}
	}
	return true
}

// needsImportPath reports whether comp needs the general, host-import-capable
// instantiation path (instantiateWithImports) rather than instantiateComponent's
// strict "one core module, one no-argument core instance, only canon lift"
// shape. Beyond components with real WIT imports, this also covers
// self-contained components that still need the general core-instance wiring
// -- most notably a component that declares its own resource type: the
// resource.new/resource.rep/resource.drop canons for it become core funcs
// grouped into their own inline-export core instance (exactly like a lowered
// import func), which the main core module then imports by instantiate-arg,
// so the component ends up with more than one core instance even though it
// has zero component-level imports. It also covers a component whose world
// exports an interface: that shape needs zero WIT imports and only canon
// lift, but still declares a nested component instance (comp.Instances) for
// the re-export shim -- see the package doc -- which only the general path
// (bindInstanceExport) resolves; instantiateComponent's strict path rejects
// any nested component instance outright.
func needsImportPath(comp *binary.Component) bool {
	if len(comp.Imports) > 0 {
		return true
	}
	if len(comp.Instances) > 0 {
		return true
	}
	for _, cn := range comp.Canons {
		if cn.Kind != 0x00 { // anything other than canon lift needs the general wiring path
			return true
		}
	}
	if len(comp.CoreInstances) != 1 || len(comp.CoreInstances[0].Args) > 0 {
		return true
	}
	return false
}

// instantiateComponent handles the no-import (M3 STEP 2) shape. It is split
// out from Instantiate so tests can exercise every validation branch directly
// against a hand-built binary.Component, without needing binary.Decode to
// produce each specific (often hard-to-construct) shape from real wasm bytes.
func instantiateComponent(ctx context.Context, r wazy.Runtime, comp *binary.Component, componentBytes []byte) (*Instance, error) {
	if len(comp.Imports) > 0 {
		return nil, fmt.Errorf("component/instance: component declares %d import(s); use the import-aware path", len(comp.Imports))
	}
	if len(comp.Instances) > 0 {
		return nil, fmt.Errorf("component/instance: component declares %d nested component instance(s); not supported by this milestone", len(comp.Instances))
	}
	if len(comp.CoreModules) != 1 {
		return nil, fmt.Errorf("component/instance: expected exactly 1 embedded core module, found %d; multiple core modules are not supported for no-import components", len(comp.CoreModules))
	}
	if len(comp.CoreInstances) != 1 {
		return nil, fmt.Errorf("component/instance: expected exactly 1 core instance, found %d; multiple core instances are not supported for no-import components", len(comp.CoreInstances))
	}

	ci := comp.CoreInstances[0]
	if ci.Kind != 0x00 {
		return nil, fmt.Errorf("component/instance: core instance is not a module instantiation (kind %#x); inline-export core instances are not supported for no-import components", ci.Kind)
	}
	if int(ci.ModuleIdx) != 0 {
		return nil, fmt.Errorf("component/instance: core instance references core module index %d, expected the sole core module (index 0)", ci.ModuleIdx)
	}
	if len(ci.Args) > 0 {
		return nil, fmt.Errorf("component/instance: core module instantiation takes %d argument(s); core modules with their own imports are not supported for no-import components", len(ci.Args))
	}

	cm := comp.CoreModules[0]
	coreBytes, err := coreModuleBytes(cm, componentBytes)
	if err != nil {
		return nil, err
	}

	core, err := r.InstantiateWithConfig(ctx, coreBytes, wazy.NewModuleConfig())
	if err != nil {
		return nil, fmt.Errorf("component/instance: instantiate embedded core module: %w", err)
	}

	coreFuncIdx, err := buildCoreFuncIndexSpace(comp)
	if err != nil {
		core.Close(ctx) //nolint:errcheck // best-effort cleanup; the wiring error below is what matters
		return nil, err
	}
	if err := validateCanons(comp, coreFuncIdx); err != nil {
		core.Close(ctx) //nolint:errcheck
		return nil, err
	}
	canonForExport, err := buildExportIndex(comp)
	if err != nil {
		core.Close(ctx) //nolint:errcheck
		return nil, err
	}

	resolve := typeResolver(comp)
	exports := make(map[string]*boundExport, len(canonForExport))
	for name, canonIdx := range canonForExport {
		canon := comp.Canons[canonIdx]
		td, err := comp.ResolveType(canon.TypeIdx) // validated by validateCanons
		if err != nil {
			core.Close(ctx) //nolint:errcheck
			return nil, fmt.Errorf("component/instance: export %q: resolve canon type %d: %w", name, canon.TypeIdx, err)
		}
		be := &boundExport{
			mod:      core,
			funcName: coreFuncIdx[canon.CoreFuncIdx],
			fd:       td.(binary.FuncDesc), // validated by validateCanons
		}
		finalizeBoundExport(be, resolve)
		exports[name] = be
	}

	return &Instance{resolve: resolve, exports: exports, instanceExports: buildInstanceExportIndex(exports), closers: []api.Module{core}, resources: newHandleTable()}, nil
}

// synthInstanceCounter numbers instantiations so each gets a globally-unique
// synthesized-name namespace. A plain process-global monotonic counter is
// enough: it never repeats within a process, which is all uniqueness on a
// Runtime's module registry requires.
var synthInstanceCounter atomic.Uint64

// nextSynthNamePrefix returns a fresh, unique namespace under which one
// instantiation registers the module names wazy *synthesizes* -- the root
// core%d, the empty-import anon%d, and the private priv%d host modules -- as
// opposed to the names baked into the guest's own imports (which must be used
// verbatim, see moduleNameFor).
//
// The namespace is per-INSTANTIATION, not per-component: every call to
// Instantiate gets its own, so a Runtime can hold arbitrarily many live
// component instances at once -- distinct components AND multiple instances of
// the same component -- without their synthesized names colliding (each
// component's unreferenced root would otherwise default to the same
// "wazy:component/core0"). This is compatible with CompileCache because a
// module's registered name binds at InstantiateModule time (WithName), not at
// compile time; the cache keys on the core-module bytes, which don't include
// the name, so the same cached CompiledModule is freely re-registered under a
// new unique name each instantiation.
func nextSynthNamePrefix() string {
	return fmt.Sprintf("wazy:component/i%d/", synthInstanceCounter.Add(1))
}

// coreModuleBytes returns the slice of componentBytes holding an embedded core
// module, bounds-checked.
func coreModuleBytes(cm binary.CoreModule, componentBytes []byte) ([]byte, error) {
	if cm.Offset < 0 || cm.Size < 0 || cm.Offset+cm.Size > len(componentBytes) {
		return nil, fmt.Errorf("component/instance: core module byte range [%d:%d) is out of bounds for a %d-byte component", cm.Offset, cm.Offset+cm.Size, len(componentBytes))
	}
	return componentBytes[cm.Offset : cm.Offset+cm.Size], nil
}

// typeResolver returns an abi.Resolver over the component's full type index
// space (comp.ResolveType), not just the type-section-only comp.Types --
// see binary.Component.ResolveType's doc for what a type index can name
// (a type-section deftype, or a resolvable type-sort alias) and what fails
// loud (an unresolvable alias, e.g. one exported from an imported instance,
// or an out-of-range index). abi.Resolver's contract is a nil return for
// "not found", so a ResolveType error collapses to nil here; callers that
// need the reason call comp.ResolveType directly (see e.g. validateCanons).
func typeResolver(comp *binary.Component) abi.Resolver {
	return func(idx uint32) binary.TypeDesc {
		t, err := comp.ResolveType(idx)
		if err != nil {
			return nil
		}
		return t
	}
}

// buildCoreFuncIndexSpace walks comp.Aliases to construct the core func index
// space for a no-import component, where every alias is a core-func alias off
// the sole core instance.
func buildCoreFuncIndexSpace(comp *binary.Component) ([]string, error) {
	coreFuncIdx := make([]string, 0, len(comp.Aliases))
	for i, al := range comp.Aliases {
		if al.Sort != 0x00 {
			return nil, fmt.Errorf("component/instance: alias[%d] has non-core sort %#x; only core func aliases are supported for no-import components", i, al.Sort)
		}
		if al.TargetKind != 0x01 {
			return nil, fmt.Errorf("component/instance: alias[%d] targets kind %#x; only core-export aliases (off the sole core instance) are supported for no-import components", i, al.TargetKind)
		}
		if al.CoreSort != 0x00 {
			return nil, fmt.Errorf("component/instance: alias[%d] has core:sort %#x (not func); only core func aliases are supported for no-import components", i, al.CoreSort)
		}
		if int(al.InstanceIdx) != 0 {
			return nil, fmt.Errorf("component/instance: alias[%d] references core instance %d, expected the sole core instance (index 0)", i, al.InstanceIdx)
		}
		coreFuncIdx = append(coreFuncIdx, al.Name)
	}
	return coreFuncIdx, nil
}

// validateCanons checks that every canon in a no-import component is a
// supported "canon lift" whose core func index and type index are in range.
func validateCanons(comp *binary.Component, coreFuncIdx []string) error {
	for i, cn := range comp.Canons {
		if cn.Kind != 0x00 {
			return fmt.Errorf("component/instance: canon[%d] has kind %#x; only canon lift (0x00) is supported for no-import components", i, cn.Kind)
		}
		if int(cn.CoreFuncIdx) >= len(coreFuncIdx) {
			return fmt.Errorf("component/instance: canon[%d] references core func index %d, but the core func index space only has %d entries", i, cn.CoreFuncIdx, len(coreFuncIdx))
		}
		td, err := comp.ResolveType(cn.TypeIdx)
		if err != nil {
			return fmt.Errorf("component/instance: canon[%d] type index %d: %w", i, cn.TypeIdx, err)
		}
		if _, ok := td.(binary.FuncDesc); !ok {
			return fmt.Errorf("component/instance: canon[%d] type index %d is not a func type (got %T)", i, cn.TypeIdx, td)
		}
	}
	return nil
}

// buildExportIndex maps each func-sort component export name to its index into
// comp.Canons. Because a no-import component has no import or component-level
// func alias occupying the component func index space, that space is exactly
// the canon declaration order: component func index i == comp.Canons[i].
func buildExportIndex(comp *binary.Component) (map[string]int, error) {
	canonForExport := make(map[string]int, len(comp.Exports))
	for _, exp := range comp.Exports {
		if exp.ExternType != 0x01 { // func
			return nil, fmt.Errorf("component/instance: export %q has extern kind %s (%#x); only func exports are supported by this milestone", exp.Name, api.ExternTypeName(exp.ExternType), exp.ExternType)
		}
		if int(exp.ExternIndex) >= len(comp.Canons) {
			return nil, fmt.Errorf("component/instance: export %q references func index %d, out of range of the %d-entry component func index space", exp.Name, exp.ExternIndex, len(comp.Canons))
		}
		canonForExport[exp.Name] = int(exp.ExternIndex)
	}
	return canonForExport, nil
}

// Call invokes the component-level exported function named exportName with
// args. Each arg is lowered into core wasm values per its declared parameter
// type, the underlying core function is called with the flattened arguments,
// and the raw core results are lifted back into abi.Values per the export's
// result type.
func (in *Instance) Call(ctx context.Context, exportName string, args ...abi.Value) ([]abi.Value, error) {
	be, ok := in.exports[exportName]
	if !ok {
		return nil, fmt.Errorf("component/instance: export %q not found", exportName)
	}
	return in.invoke(ctx, be, exportName, args)
}

// CallExport invokes memberName inside the exported instance instanceName.
//
// A WIT world that exports an *interface* (rather than a bare function)
// compiles to a component whose top-level export is instance-typed: the
// tooling packages the interface's lifted functions into a nested
// "re-export shim" component and an Instance (section 5) that instantiates
// it, and the top-level export names that instance -- see
// resolveInstanceExport in host_import.go. CallExport is how a caller
// reaches a function inside such an instance, e.g.
// CallExport(ctx, "component:adder/calc", "add", u32(2), u32(3)).
//
// This is sugar for Call(ctx, instanceName+"#"+memberName, args...); both
// spellings work, since instance members are bound into the same exports
// map as plain func exports.
func (in *Instance) CallExport(ctx context.Context, instanceName, memberName string, args ...abi.Value) ([]abi.Value, error) {
	// Fast path: two map reads against instanceExports, no string
	// concatenation -- instanceExportKey's "instance#member" join is a fresh
	// allocation on every call otherwise, and this is the hot path for every
	// WIT-exports-an-interface component (see the package doc). Falls back
	// to the exact same Call(instanceExportKey(...)) this always did when
	// the pair isn't found there (e.g. a no-import component, whose exports
	// never carry "#"-joined keys, or a genuinely missing export), so the
	// not-found error text is byte-identical to before this fast path
	// existed.
	if members, ok := in.instanceExports[instanceName]; ok {
		if entry, ok := members[memberName]; ok {
			return in.invoke(ctx, entry.be, entry.name, args)
		}
	}
	return in.Call(ctx, instanceExportKey(instanceName, memberName), args...)
}

// instanceExportKey builds the exports-map key for a member function inside
// an exported instance, joining the instance export name and the member
// name with "#".
func instanceExportKey(instanceName, memberName string) string {
	return instanceName + "#" + memberName
}

// instanceExportEntry pairs a boundExport with the exact "instance#member"
// string it was bound under in the flat exports map (reused verbatim, not
// rebuilt) -- see buildInstanceExportIndex.
type instanceExportEntry struct {
	be   *boundExport
	name string
}

// buildInstanceExportIndex partitions exports into a two-level index keyed
// by instance name then member name, for every entry whose key follows the
// instanceExportKey "instance#member" convention (i.e. contains "#"). It
// lets CallExport look up a boundExport with two map reads and no string
// concatenation. Built once at bind time (called from every site that
// constructs an Instance); entries with no "#" (plain func exports) are
// simply omitted, since CallExport is never used to reach them directly.
func buildInstanceExportIndex(exports map[string]*boundExport) map[string]map[string]instanceExportEntry {
	var out map[string]map[string]instanceExportEntry
	for key, be := range exports {
		i := strings.IndexByte(key, '#')
		if i < 0 {
			continue
		}
		if out == nil {
			out = make(map[string]map[string]instanceExportEntry)
		}
		instanceName, memberName := key[:i], key[i+1:]
		members := out[instanceName]
		if members == nil {
			members = make(map[string]instanceExportEntry)
			out[instanceName] = members
		}
		members[memberName] = instanceExportEntry{be: be, name: key}
	}
	return out
}

func (in *Instance) invoke(ctx context.Context, be *boundExport, exportName string, args []abi.Value) ([]abi.Value, error) {
	fd := be.fd
	if len(args) != len(fd.Params) {
		return nil, fmt.Errorf("component/instance: export %q takes %d parameter(s), got %d", exportName, len(fd.Params), len(args))
	}

	// be.coreFn/flattenErr etc. were resolved/computed once at bind time
	// (finalizeBoundExport) rather than here on every call -- see
	// boundExport's doc.
	if be.coreFn == nil {
		return nil, fmt.Errorf("component/instance: core module has no exported function %q (referenced by canon lift for export %q)", be.funcName, exportName)
	}
	if be.flattenErr != nil {
		return nil, fmt.Errorf("component/instance: export %q: flatten func type: %w", exportName, be.flattenErr)
	}

	mem, memAvailable := memoryBytesOf(be.mod)
	realloc := cachedReallocOf(ctx, be)

	// coreArgsPtr's buffer, like stack below, is pure scratch local to this
	// call (lowerParams only ever appends into it; nothing downstream of
	// invoke retains it), so it's fetched from the pool empty (len 0, cap
	// from a prior call) and handed to lowerParams to append into directly,
	// instead of lowerParams allocating its own backing array.
	coreArgsPtr := coreValueSlicePool.Get().(*[]abi.CoreValue)
	*coreArgsPtr = (*coreArgsPtr)[:0]
	coreArgs, err := in.lowerParams(be, args, mem, memAvailable, realloc, exportName, *coreArgsPtr)
	if err != nil {
		coreValueSlicePool.Put(coreArgsPtr)
		return nil, err
	}
	*coreArgsPtr = coreArgs
	if len(coreArgs) != len(be.coreParamsWant) {
		putCoreValueSlice(coreArgsPtr)
		return nil, fmt.Errorf("component/instance: export %q: parameter list flattens to %d core value(s) but the core signature expects %d; whole-parameter-list spilling to memory is not supported by this milestone", exportName, len(coreArgs), len(be.coreParamsWant))
	}

	// stack is pure scratch: it only exists to hand coreArgs' bits to
	// be.coreFn.Call as a []uint64, and the native engine's callEngine.Call
	// copies params into its own buffer before doing anything else (see
	// call_engine.go), so nothing retains a reference to it once Call
	// returns -- safe to pool rather than allocate fresh every call. See
	// uint64SlicePool's doc for the concurrency argument.
	stackPtr := getUint64Slice(len(coreArgs))
	stack := *stackPtr
	for i, cv := range coreArgs {
		stack[i] = cv.Bits
	}
	putCoreValueSlice(coreArgsPtr) // coreArgs' bits are now copied into stack; done with it

	rawResults, err := be.coreFn.Call(ctx, stack...)
	putUint64Slice(stackPtr)
	if err != nil {
		return nil, fmt.Errorf("component/instance: export %q: call core func %q: %w", exportName, be.funcName, err)
	}

	results, err := in.liftResult(be, rawResults, mem, memAvailable, exportName)
	if err != nil {
		return nil, err
	}

	// Post-return runs after lifting has finished reading rawResults (e.g. a
	// returned string's bytes), so the guest can safely free/reuse that
	// memory. Per definitions.py's canon_lift, it is called with the same
	// flat core values the lift produced.
	if be.postReturnFuncName != "" {
		if be.postReturnFn == nil {
			return nil, fmt.Errorf("component/instance: export %q: post-return core func %q not found", exportName, be.postReturnFuncName)
		}
		// Pooled for the same reason as stack above: pure scratch, copied
		// from rawResults only to satisfy Call's variadic signature, and
		// released the moment Call returns since callEngine.Call copies
		// params into its own buffer before doing anything else.
		prArgsPtr := getUint64Slice(len(rawResults))
		copy(*prArgsPtr, rawResults)
		_, err := be.postReturnFn.Call(ctx, (*prArgsPtr)...)
		putUint64Slice(prArgsPtr)
		if err != nil {
			return nil, fmt.Errorf("component/instance: export %q: post-return %q: %w", exportName, be.postReturnFuncName, err)
		}
	}

	return results, nil
}

// lowerParams lowers each component-level argument into its flattened core
// values, in parameter order, using be's precomputed per-param type/
// usesMemory/error (see finalizeBoundExport) instead of recomputing them.
// dst is the (possibly pool-provided, possibly nil) buffer to append into --
// see invoke's coreArgsPtr -- so lowerParams itself never has to allocate
// the backing array; callers that don't care (e.g. tests exercising an
// error branch directly) can just pass nil, which behaves exactly like the
// old var coreArgs []abi.CoreValue starting point.
func (in *Instance) lowerParams(be *boundExport, args []abi.Value, mem []byte, memAvailable bool, realloc abi.Realloc, exportName string, dst []abi.CoreValue) ([]abi.CoreValue, error) {
	coreArgs := dst
	var err error
	for i, p := range be.fd.Params {
		if err := be.paramErrs[i]; err != nil {
			return nil, fmt.Errorf("component/instance: export %q param %d (%s): %w", exportName, i, p.Name, err)
		}
		pt := be.paramTypes[i]
		if be.paramUsesMemory[i] && !memAvailable {
			return nil, fmt.Errorf("component/instance: export %q param %d (%s) requires linear memory (string/list), but the core module exports no memory", exportName, i, p.Name)
		}
		coreArgs, err = abi.LowerFlatInto(coreArgs, args[i], pt, in.resolve, realloc, mem)
		if err != nil {
			return nil, fmt.Errorf("component/instance: export %q param %d (%s): lower: %w", exportName, i, p.Name, err)
		}
	}
	return coreArgs, nil
}

// liftResult lifts the raw core call results back into a single abi.Value per
// be's declared result type, using be's precomputed result type/usesMemory/
// flatKinds/error (see finalizeBoundExport) instead of recomputing them.
// Multi-result functions and results that require memory when none is
// available both fail loudly.
func (in *Instance) liftResult(be *boundExport, rawResults []uint64, mem []byte, memAvailable bool, exportName string) ([]abi.Value, error) {
	if be.tooManyResults > 0 {
		return nil, fmt.Errorf("component/instance: export %q has %d named results; multiple component-level results are not supported by this milestone", exportName, be.tooManyResults)
	}
	if !be.hasResult {
		if len(rawResults) != 0 {
			return nil, fmt.Errorf("component/instance: export %q: core func returned %d value(s) for a 0-result signature", exportName, len(rawResults))
		}
		return nil, nil
	}
	if be.resultErr != nil {
		return nil, fmt.Errorf("component/instance: export %q result: %w", exportName, be.resultErr)
	}

	rt := be.resultType
	if be.resultUsesMemory && !memAvailable {
		return nil, fmt.Errorf("component/instance: export %q result requires linear memory (string/list), but the core module exports no memory", exportName)
	}

	flatKinds := be.resultFlatKinds

	// Per the Canonical ABI (definitions.py's flatten_functype), when a
	// lift's result flattens to more than MAX_FLAT_RESULTS (1) core values
	// -- e.g. a string result, which flattens to (ptr, len) -- the core
	// function instead returns a single i32: a pointer into its own linear
	// memory where it wrote the result using the type's normal (non-flat)
	// store/load representation, not the flat value sequence. abi.LiftFlat's
	// own spill path exists for this same pattern but is gated on
	// MaxFlatParams (16, the *parameter* limit), since it has no way to know
	// it's being used in a result context here; MaxFlatResults (1) is the
	// correct threshold for a function result, so that case is handled
	// directly rather than by calling LiftFlat.
	if len(be.coreResultsWant) == abi.MaxFlatResults && len(flatKinds) > abi.MaxFlatResults {
		// The spill mechanism itself needs linear memory as scratch space
		// for the pointer indirection, regardless of whether rt's own type
		// otherwise needs memory (e.g. a plain record of two u64s doesn't,
		// per usesMemory, but still can't flatten to a single core value).
		if !memAvailable {
			return nil, fmt.Errorf("component/instance: export %q result flattens to %d core value(s) that must be returned via a memory pointer, but the core module exports no memory", exportName, len(flatKinds))
		}
		if len(rawResults) != 1 {
			return nil, fmt.Errorf("component/instance: export %q: core func returned %d value(s) for a spilled (pointer) result, expected 1", exportName, len(rawResults))
		}
		val, err := abi.Load(mem, uint32(rawResults[0]), rt, in.resolve)
		if err != nil {
			return nil, fmt.Errorf("component/instance: export %q result: load spilled result: %w", exportName, err)
		}
		return []abi.Value{val}, nil
	}
	if len(flatKinds) != len(be.coreResultsWant) {
		return nil, fmt.Errorf("component/instance: export %q result flattens to %d core value(s), exceeding the flat-result limit (core signature returns %d value(s), a spilled memory pointer); spilled results are not supported by this milestone", exportName, len(flatKinds), len(be.coreResultsWant))
	}
	if len(rawResults) != len(flatKinds) {
		return nil, fmt.Errorf("component/instance: export %q: core func returned %d value(s), expected %d", exportName, len(rawResults), len(flatKinds))
	}

	// coreResults is pure scratch, discarded as soon as abi.LiftFlat returns
	// -- LiftFlat only ever reads through it (via a CoreValueIter) to
	// produce val, a plain lifted Go value that never aliases the
	// CoreValue slice or its backing array -- so it's safe to pool. See
	// coreValueSlicePool's doc for the concurrency argument.
	coreResultsPtr := getCoreValueSlice(len(rawResults))
	coreResults := *coreResultsPtr
	for i, u := range rawResults {
		coreResults[i] = abi.CoreValue{Kind: flatKinds[i], Bits: u}
	}

	val, err := abi.LiftFlat(coreResults, rt, in.resolve, mem)
	putCoreValueSlice(coreResultsPtr)
	if err != nil {
		return nil, fmt.Errorf("component/instance: export %q result: lift: %w", exportName, err)
	}
	return []abi.Value{val}, nil
}

// Close releases every module instantiated for this component (in reverse
// order of instantiation). It does not close the Runtime passed to
// Instantiate, which the caller owns.
func (in *Instance) Close(ctx context.Context) error {
	var firstErr error
	for i := len(in.closers) - 1; i >= 0; i-- {
		if err := in.closers[i].Close(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// memoryBytesOf returns a read/write view of mod's linear memory, if it has
// one. It uses Memory() (not ExportedMemory) so that a guest module which
// imports rather than exports its memory is still handled.
//
// wazy's Module.Memory() returns a typed-nil *MemoryInstance (a non-nil
// api.Memory whose Size()/Read() panic) when the module has no memory, so the
// nil-interface check alone is not enough; the recover degrades that panic to
// the same "no memory" result.
func memoryBytesOf(mod api.Module) (buf []byte, ok bool) {
	defer func() {
		if recover() != nil {
			buf, ok = nil, false
		}
	}()
	m := mod.Memory()
	if m == nil {
		return nil, false
	}
	buf, ok = m.Read(0, m.Size())
	if !ok {
		return nil, false
	}
	return buf, true
}

// reallocOf returns the abi.Realloc backed by mod's "cabi_realloc" export, or
// one that fails loudly if mod doesn't export it.
func reallocOf(ctx context.Context, mod api.Module) abi.Realloc {
	fn := mod.ExportedFunction("cabi_realloc")
	if fn == nil {
		return func(uint32, uint32, uint32, uint32) (uint32, error) {
			return 0, fmt.Errorf("component/instance: memory allocation requires a %q export on the core module, which is not present", "cabi_realloc")
		}
	}
	return reallocOfFunc(ctx, fn)
}

// cachedReallocOf is reallocOf's boundExport-caching counterpart: it builds
// the abi.Realloc from be.reallocFn -- resolved once at bind time by
// finalizeBoundExport -- instead of a fresh mod.ExportedFunction("cabi_realloc")
// lookup on every invoke() call. Preserves reallocOf's exact lazy-failure
// message/semantics when the module exports no cabi_realloc (be.reallocFn ==
// nil): the error only surfaces if the returned Realloc is actually invoked.
func cachedReallocOf(ctx context.Context, be *boundExport) abi.Realloc {
	if be.reallocFn == nil {
		return func(uint32, uint32, uint32, uint32) (uint32, error) {
			return 0, fmt.Errorf("component/instance: memory allocation requires a %q export on the core module, which is not present", "cabi_realloc")
		}
	}
	return reallocOfFunc(ctx, be.reallocFn)
}

// reallocOfFunc returns the abi.Realloc that calls fn -- an already-resolved
// api.Function -- with the standard cabi_realloc(origPtr, origSize, align,
// newSize) -> newPtr signature. Factored out of reallocOf so a caller that
// has already resolved the exact realloc func to call (e.g. buildHostWrapper,
// via a canon lower's own "realloc" CanonOpt rather than a fixed "cabi_realloc"
// name on the runtime caller -- see its doc) can reuse the same call/error
// handling without going through mod.ExportedFunction("cabi_realloc") again.
func reallocOfFunc(ctx context.Context, fn api.Function) abi.Realloc {
	return func(origPtr, origSize, align, newSize uint32) (uint32, error) {
		res, err := fn.Call(ctx, uint64(origPtr), uint64(origSize), uint64(align), uint64(newSize))
		if err != nil {
			return 0, fmt.Errorf("cabi_realloc: %w", err)
		}
		if len(res) != 1 {
			return 0, fmt.Errorf("cabi_realloc returned %d value(s), expected 1", len(res))
		}
		return uint32(res[0]), nil
	}
}

// funcResultTypeRefs normalizes FuncResults (unnamed-or-named) into a slice of
// TypeRefs: 0 entries for no results, 1 for a single unnamed or named result,
// or more for multiple named results (which invoke rejects).
func funcResultTypeRefs(fd binary.FuncDesc) []binary.TypeRef {
	if fd.Results.Unnamed != nil {
		return []binary.TypeRef{*fd.Results.Unnamed}
	}
	refs := make([]binary.TypeRef, len(fd.Results.Named))
	for i, r := range fd.Results.Named {
		refs[i] = r.Type
	}
	return refs
}

// resolveTypeRef resolves a TypeRef (primitive or type-table index) to its
// binary.TypeDesc, failing loud instead of returning a nil descriptor.
func resolveTypeRef(ref *binary.TypeRef, resolve abi.Resolver) (binary.TypeDesc, error) {
	if ref.Primitive != "" {
		return binary.PrimitiveDesc{Prim: ref.Primitive}, nil
	}
	if ref.TypeIndex == nil {
		return nil, fmt.Errorf("type reference has neither a primitive name nor a type index")
	}
	t := resolve(*ref.TypeIndex)
	if t == nil {
		return nil, fmt.Errorf("type index %d not found", *ref.TypeIndex)
	}
	return t, nil
}

// usesMemory reports whether a value of type t needs linear memory to lower or
// lift (directly, as a string/list, or transitively through a
// record/tuple/variant/option/result field).
func usesMemory(t binary.TypeDesc, resolve abi.Resolver) bool {
	switch d := t.(type) {
	case binary.PrimitiveDesc:
		return d.Prim == "string"
	case binary.ListDesc:
		return true
	case binary.RecordDesc:
		for _, f := range d.Fields {
			if ft, err := resolveTypeRef(&f.Type, resolve); err == nil && usesMemory(ft, resolve) {
				return true
			}
		}
		return false
	case binary.TupleDesc:
		for _, e := range d.Elements {
			if et, err := resolveTypeRef(&e, resolve); err == nil && usesMemory(et, resolve) {
				return true
			}
		}
		return false
	case binary.VariantDesc:
		for _, c := range d.Cases {
			if c.Type == nil {
				continue
			}
			if ct, err := resolveTypeRef(c.Type, resolve); err == nil && usesMemory(ct, resolve) {
				return true
			}
		}
		return false
	case binary.OptionDesc:
		et, err := resolveTypeRef(&d.Element, resolve)
		return err == nil && usesMemory(et, resolve)
	case binary.ResultDesc:
		if d.Ok != nil {
			if okT, err := resolveTypeRef(d.Ok, resolve); err == nil && usesMemory(okT, resolve) {
				return true
			}
		}
		if d.Err != nil {
			if errT, err := resolveTypeRef(d.Err, resolve); err == nil && usesMemory(errT, resolve) {
				return true
			}
		}
		return false
	default:
		return false
	}
}
