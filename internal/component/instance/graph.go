package instance

import (
	"context"
	"fmt"
	"strings"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/experimental"
	"github.com/samyfodil/wazy/internal/component/abi"
	"github.com/samyfodil/wazy/internal/component/binary"
	"github.com/samyfodil/wazy/internal/expctxkeys"
	"github.com/samyfodil/wazy/internal/wasm"
)

// graphEmptyImportKey is the stable resolver key the graph rewrites a guest's
// empty import module name to (the decoder rejects an empty name outright). It
// is per-COMPONENT-constant, not per-instantiation: the ImportResolver maps it
// to this instance's anon target, so the rewritten bytes stay identical across
// instantiations and keep hitting the compile cache. It is never registered as
// a global module name (the anon target is anonymous), so it cannot collide.
const graphEmptyImportKey = "wazy:component/empty-import"

// This file implements the general multi-core-module instantiation graph
// engine: a component with more than one embedded core module (binary.Decode
// gives this away as len(comp.CoreModules) > 1 -- see needsGraphPath in
// instance.go), wired together by a chain of core:instance definitions
// (section 2) -- some instantiating a real embedded core module with named
// arguments resolved from earlier core instances, others just regrouping
// already-defined core items (funcs, a memory, a table) under new names for
// a later instantiate-arg to consume.
//
// # Algorithm
//
// 1. Build the component func index space (compFuncAliases + liftCanonIdxs)
//    the standard construction -- this is unaffected by the
//    core-level generalization below (see its doc for why aliases always
//    precede lifts structurally).
// 2. Build the core memory and core table index spaces by filtering
//    comp.Aliases by CoreSort, in the slice's own (already section-order-
//    preserving) order -- no canon ever produces a memory or table, so no
//    interleaving concern there (see corefuncspace.go's doc).
// 3. Require comp.CoreFuncSpace (populated by Decode -- see
//    corefuncspace.go) for the core FUNC index space, since that space can
//    genuinely interleave alias- and canon-produced entries in a real
//    multi-module graph, which only CoreFuncSpace's declaration-order
//    tracking can resolve correctly.
// 4. Determine whether any core:instance is referenced, via a "with"
//    instantiate-arg, under the empty-string name -- wit-component's wasip2
//    CLI adapter does this for the shared indirect-call-table wiring (see
//    importrewrite.go's doc) -- and if so, pick a synthesized replacement
//    name for it, since wazy's module registry treats "" as anonymous (not
//    resolvable by another module's by-name import).
// 5. Pre-compile every embedded core module (via Runtime.CompileModule, with
//    the empty-name rewrite already applied) to discover the core-level
//    (i32/i64/f32/f64) signature every real import clause declares, keyed by
//    (module name, field name). This is the only source of truth this
//    package has for a lowered import's core-level type when no caller-
//    supplied WithImport is registered (see resolveInlineExportFuncItem):
//    the WIT-level type isn't decoded (see the package doc on instance.go),
//    but the real core module that will eventually import the lowered
//    func's re-export already commits to an exact core-level type, and that
//    binary is right here.
// 6. Walk comp.CoreInstances in order, exactly mirroring the binary's own
//    core instance index space:
//      - Kind 0x00 (instantiate): instantiate the named embedded core
//        module for real via Runtime.InstantiateWithConfig, registered under
//        the wazy name its "with" args reference it by (or a synthesized
//        key if none do -- see moduleKeyForGraph). Its own imports resolve
//        automatically, by that same by-name mechanism, against whichever
//        earlier core instances (real or shim) were registered under the
//        names it imports from.
//      - Kind 0x01 (inline exports): build a "passthrough shim" -- a tiny
//        hand-encoded real core wasm module (see shim.go) that imports each
//        entry from wherever it really originates and re-exports it under
//        the group's target name. A func entry is either an alias of an
//        already-instantiated module's own export (resolved via
//        ExportedFunction(...).Definition() for its exact signature) or a
//        canon that produces a brand-new core func (canon lower, or one of
//        the three resource canons): the latter gets a fresh, uniquely
//        named single-func Go host module built first (see
//        resolveInlineExportFuncItem), and the shim imports *that*. A memory
//        or table entry is always a pure alias -- no canon ever produces
//        one -- resolved the same way, giving the shim's re-export the exact
//        same underlying MemoryInstance/TableInstance identity as its
//        source (see shim.go's doc for why this matters and how a real,
//        import-then-export core module achieves it for free).
// 7. Bind the component's own exports (func or instance-typed, the latter
//    for a WIT world that exports an interface -- see host_import.go's
//    bindInstanceExport, whose core-agnostic logic this package reuses
//    unchanged where possible) via a coreFuncTarget closure that resolves an
//    absolute core func index through comp.CoreFuncSpace, exactly as step 6
//    resolved each inline-export entry.
//
// The trivial single-module, no-import path (instantiateComponent in
// instance.go) is handled separately; every other component -- host-import or
// multi-core -- is instantiated here (see Instantiate's routing).

// coreFuncSig is a core-level (i32/i64/f32/f64) function signature,
// discovered from a real embedded core module's own import declaration --
// see discoverNeededFuncTypes.
type coreFuncSig struct {
	params  []api.ValueType
	results []api.ValueType
}

// instantiateGraph is the general multi-core-module instantiation graph
// engine -- see this file's package doc for the algorithm.
func instantiateGraph(ctx context.Context, r wazy.Runtime, comp *binary.Component, componentBytes []byte, cfg *config) (*Instance, error) {
	for _, im := range comp.Imports {
		// Type imports (extern sort 0x03) carry no runtime obligation -- they
		// are type-equality constraints (e.g. cargo-component re-imports a
		// world's `use`d types: `import "point" (type (eq N))`), resolved from
		// the component's own type space, with nothing for the host to provide.
		// The instance-index space (importInterfaceName, host_import.go) already
		// counts only 0x05 imports, so skipping these here keeps indexing intact.
		if im.ExternType == 0x03 {
			continue
		}
		// A top-level func import (0x01) -- a nested component parameterized by a
		// func, satisfied by a `(with "x" (func ...))` instantiate-arg wired into
		// cfg.imports (see instantiateNestedInstances), keyed by the import name.
		// Only allowed when actually provided; an unsatisfied func import is still
		// rejected (a top-level component importing a func wazy can't supply).
		if im.ExternType == 0x01 {
			if _, ok := cfg.imports[mkImportKey(im.Name, "")]; ok {
				continue
			}
		}
		if im.ExternType != 0x05 { // instance
			return nil, fmt.Errorf("component/instance: import %q has extern kind %s (%#x); only instance imports are supported", im.Name, api.ExternTypeName(im.ExternType), im.ExternType)
		}
	}
	// A hand-built (non-decoded) Component with aliases/canons never had its
	// core func index space built, so the graph engine can't resolve a core
	// func through it -- reject those. A genuinely decoded component whose
	// CoreFuncSpace is empty is fine: it just has no core-func-producing
	// aliases/canons (e.g. a purely type-level nested component that only
	// aliases and re-exports a type), and the graph path never does a
	// CoreFuncSpace lookup for it.
	if !comp.Decoded && (len(comp.Aliases) > 0 || len(comp.Canons) > 0) {
		return nil, fmt.Errorf("component/instance: the graph engine requires a component decoded via binary.Decode (hand-built binary.Component values with aliases/canons are not supported by this path)")
	}

	resolve := typeResolver(comp)
	synthPrefix := nextSynthNamePrefix()

	// Component func index space, in definition order across sections (func
	// imports / func aliases / canon lifts / func exports) -- see
	// binary.ComponentFuncSpace. The previous ad-hoc [aliases]++[lifts]
	// reconstruction ignored func imports and export-created entries, so a
	// component whose func exports interleave with its lifts (standard
	// cargo-component/wit-bindgen output) bound every export to the wrong lift.
	var componentFunc func(idx uint32) (isLift bool, liftCanonIdx int, at aliasTarget, err error)
	componentFunc = func(idx uint32) (isLift bool, liftCanonIdx int, at aliasTarget, err error) {
		if int(idx) >= len(comp.ComponentFuncSpace) {
			return false, 0, aliasTarget{}, fmt.Errorf("component func index %d out of range of the %d-entry component func index space", idx, len(comp.ComponentFuncSpace))
		}
		e := comp.ComponentFuncSpace[idx]
		switch e.Kind {
		case binary.ComponentFuncFromCanonLift:
			return true, int(e.Canon), aliasTarget{}, nil
		case binary.ComponentFuncFromAlias:
			al := comp.Aliases[e.Alias]
			if al.TargetKind != 0x00 { // only export-aliases of imported/nested instances are bindable
				return false, 0, aliasTarget{}, fmt.Errorf("component func index %d is a %#x-kind func alias; only instance-export func aliases are supported", idx, al.TargetKind)
			}
			return false, 0, aliasTarget{instIdx: al.InstanceIdx, name: al.Name}, nil
		case binary.ComponentFuncFromExport:
			// A func export aliases the func it exports; resolve through to it
			// (that func -- typically a lift -- appears earlier, so no cycle).
			return componentFunc(comp.Exports[e.Export].ExternIndex)
		default: // ComponentFuncFromImport
			return false, 0, aliasTarget{}, fmt.Errorf("component func index %d names an imported func; component-level func imports are not supported", idx)
		}
	}

	// Core memory and table index spaces: no canon ever produces either, so
	// filtering Aliases in the slice's own order is already correct.
	var coreMemSpace, coreTableSpace []aliasTarget
	for _, al := range comp.Aliases {
		if al.Sort != 0x00 || al.TargetKind != 0x01 {
			continue
		}
		switch al.CoreSort {
		case 0x02: // memory
			coreMemSpace = append(coreMemSpace, aliasTarget{instIdx: al.InstanceIdx, name: al.Name})
		case 0x01: // table
			coreTableSpace = append(coreTableSpace, aliasTarget{instIdx: al.InstanceIdx, name: al.Name})
		}
	}

	refNames := make(map[uint32][]string)
	for _, ci := range comp.CoreInstances {
		if ci.Kind == 0x00 {
			for _, arg := range ci.Args {
				refNames[arg.InstanceIdx] = append(refNames[arg.InstanceIdx], arg.Name)
			}
		}
	}

	// wit-component's wasip2 CLI adapter groups its shared indirect-call
	// table under the empty-string "with" name -- see importrewrite.go. Find
	// it (there is at most one per component). It gets a STABLE resolver key,
	// not a per-instantiation name: the guest's empty import is rewritten to
	// this constant (the decoder rejects an empty import module name), and the
	// ImportResolver maps it to the anon instance. Stable => the rewritten
	// guest bytes are identical every instantiation, so the compile cache still
	// hits (unlike a per-instance name, which would bypass it), and nothing is
	// registered under it globally (the module is anonymous).
	emptyNameTarget := ""
	for k := range comp.CoreInstances {
		if names := refNames[uint32(k)]; len(names) == 1 && names[0] == "" {
			emptyNameTarget = graphEmptyImportKey
			break
		}
	}

	// Import-discovery + the empty-name rewrite are pure over the immutable
	// component, so cache them per component when a CompileCache is present;
	// otherwise compute once for this instantiation. Either way the main loop
	// below reuses rewrittenCore instead of re-slicing/re-rewriting.
	var neededTypes map[string]map[string]coreFuncSig
	var rewrittenCore [][]byte
	var err error
	if cfg.compileCache != nil {
		var plan *graphPlan
		plan, err = cfg.compileCache.graphPlanFor(comp, func() (*graphPlan, error) {
			nt, rw, derr := discoverNeededFuncTypes(r, comp, componentBytes, emptyNameTarget)
			if derr != nil {
				return nil, derr
			}
			return &graphPlan{neededTypes: nt, rewritten: rw}, nil
		})
		if err == nil {
			neededTypes, rewrittenCore = plan.neededTypes, plan.rewritten
		}
	} else {
		neededTypes, rewrittenCore, err = discoverNeededFuncTypes(r, comp, componentBytes, emptyNameTarget)
	}
	if err != nil {
		return nil, err
	}

	resources := newHandleTable()
	// in is allocated NOW (instead of built as a struct literal at the end
	// of this function, as the trivial paths still do) so the async
	// builtins (task.return, context.get/set, backpressure.inc/dec -- see
	// async_builtins.go) wired into the core-instance loop below can close
	// over a stable *Instance directly, exactly the way they close over
	// resources. Every other field is filled in below, in the same places
	// the old struct literal set them; the struct literal at the bottom of
	// this function became field assignments on this same pointer instead
	// of a fresh allocation. mayEnter/mayLeave start true -- see their doc
	// on Instance.
	sh := cfg.sharedSched
	if sh == nil { // this instantiation is its own composition-tree root
		sh = &sched{}
		cfg.sharedSched = sh // propagated to every subCfg by instantiateNestedInstances below
	}
	in := &Instance{resolve: resolve, resources: resources, mayEnter: true, mayLeave: true, sched: sh}
	// A composed sub-instance inherits the destructors for the resources it
	// imports from a sibling, keyed by the table tag its own resource.drop uses.
	for tag, resolve := range cfg.importedResDtors {
		resources.registerDtor(tag, resourceDtor(resolve))
	}
	// Host-provided resources: the embedder's Go destructor runs when the guest
	// drops an own<R> of a host resource.
	for tag, fn := range cfg.hostResDtors {
		resources.registerDtor(tag, fn)
	}
	runResourceHooks(cfg, resources)
	instMods := make(map[int]api.Module, len(comp.CoreInstances))
	var closers []api.Module
	closeAll := func() {
		for i := len(closers) - 1; i >= 0; i-- {
			closers[i].Close(ctx) //nolint:errcheck // best-effort cleanup on an error path
		}
	}
	fail := func(err error) (*Instance, error) {
		closeAll()
		return nil, err
	}

	var coreModuleCount int
	var wasiCalls []string
	privateHostModCount := 0
	nextPrivateName := func() string {
		privateHostModCount++
		return fmt.Sprintf("%spriv%d", synthPrefix, privateHostModCount)
	}

	// coreFuncTarget resolves an absolute core func index to the module that
	// exports it and the export name to call. Defined before the main loop
	// (rather than after, where bindImportExportsGraph also needs it) so
	// resolveInlineExportItem can use the very same closure -- via
	// canonMemoryAndRealloc -- to resolve a canon lower's own "realloc"
	// CanonOpt while still inside the loop that's filling in instMods. This
	// is safe because it captures instMods by reference (a Go map, not a
	// snapshot) and the Component Model's index spaces are strictly
	// forward-declared: anything a core instance k's canon opts reference
	// must already have been instantiated by earlier iterations.
	// canonMods caches the single-func host module built for a canon-produced
	// core func resolved directly (not via an inline-export group) -- e.g. an
	// exported [constructor]t that lifts a `canon resource.new` core func.
	canonMods := map[int]api.Module{}
	var coreMemTarget func(idx int) (api.Module, error)
	var coreFuncTarget func(cfi int) (api.Module, string, error)
	coreFuncTarget = func(cfi int) (api.Module, string, error) {
		if cfi < 0 || cfi >= len(comp.CoreFuncSpace) {
			return nil, "", fmt.Errorf("core func index %d out of range of the %d-entry core func index space", cfi, len(comp.CoreFuncSpace))
		}
		entry := comp.CoreFuncSpace[cfi]
		switch entry.Kind {
		case binary.CoreFuncFromAlias:
			al := comp.Aliases[entry.Alias]
			mod, ok := instMods[int(al.InstanceIdx)]
			if !ok {
				return nil, "", fmt.Errorf("core func index %d targets core instance %d, which was not instantiated", cfi, al.InstanceIdx)
			}
			return mod, al.Name, nil
		case binary.CoreFuncFromCanon:
			// A canon-produced core func (resource.new/drop/rep or lower)
			// resolved directly -- build its host module once. This is the same
			// machinery resolveInlineExportItem uses for a canon consumed by a
			// core module's import, applied when the canon func is lifted/aliased
			// directly instead (an exported resource constructor).
			if mod, ok := canonMods[int(entry.Canon)]; ok {
				return mod, "f", nil
			}
			if int(entry.Canon) >= len(comp.Canons) {
				return nil, "", fmt.Errorf("core func index %d: canon index %d out of range", cfi, entry.Canon)
			}
			privName := nextPrivateName()
			hostMod, name, params, results, _, err := buildCanonHostModule(ctx, r, comp, cfg, resources, in, comp.Canons[entry.Canon], neededTypes, "", privName, privName, coreMemTarget, coreFuncTarget)
			if err != nil {
				return nil, "", fmt.Errorf("core func index %d: %w", cfi, err)
			}
			// A host module forbids ExportedFunction (it is meant to be imported
			// by core wasm, not called directly), but a boundExport calls its
			// core func that way. Wrap it in a one-func passthrough shim -- a
			// real core module importing the host func and re-exporting it -- so
			// the lift can call it. The host module registers globally by name,
			// so the shim's import resolves via ctx.
			shimBytes, err := buildPassthroughShim([]shimItem{{Sort: shimSortFunc, FromModule: privName, FromName: name, ExportName: "f", Params: params, Results: results}})
			if err != nil {
				return nil, "", fmt.Errorf("core func index %d: shim: %w", cfi, err)
			}
			shim, err := r.InstantiateWithConfig(ctx, shimBytes, wazy.NewModuleConfig().WithName("").WithStartFunctions())
			if err != nil {
				return nil, "", fmt.Errorf("core func index %d: instantiate canon shim: %w", cfi, err)
			}
			canonMods[int(entry.Canon)] = shim
			closers = append(closers, hostMod, shim)
			return shim, "f", nil
		default:
			return nil, "", fmt.Errorf("core func index %d: unknown core func space entry kind %d", cfi, entry.Kind)
		}
	}

	// coreMemTarget is coreFuncTarget's counterpart for the core MEMORY index
	// space -- used only by canonMemoryAndRealloc (via buildCanonHostModule)
	// to resolve a canon lower's own "memory" CanonOpt. Safe to define here
	// for the same forward-declaration reason as coreFuncTarget above.
	coreMemTarget = func(idx int) (api.Module, error) {
		if idx < 0 || idx >= len(coreMemSpace) {
			return nil, fmt.Errorf("core memory index %d out of range of the %d-entry core memory index space", idx, len(coreMemSpace))
		}
		at := coreMemSpace[idx]
		mod, ok := instMods[int(at.instIdx)]
		if !ok {
			return nil, fmt.Errorf("core memory index %d targets core instance %d, which was not instantiated", idx, at.instIdx)
		}
		return mod, nil
	}

	// keyToInst is this component instance's private import environment: a
	// per-instantiation map from an internal wiring name (a raw "with" name
	// like wasi:io/streams@0.2.12, the empty-import key, or a synthesized
	// core%d) to the instance that provides it. Its embedded modules and shims
	// are instantiated ANONYMOUSLY (WithName("")), so they never enter the
	// Runtime's global registry and never collide across components; all of
	// their internal imports resolve through this map via the ImportResolver
	// instead. This is wasmtime's model: a component's internal wiring lives in
	// its own instance graph, not a shared global namespace. keys[k] holds each
	// instance's key so a later shim can name its sources.
	keys := make([]string, len(comp.CoreInstances))
	keyToInst := map[string]api.Module{}
	// Chain to a caller-supplied ImportResolver, if any: the graph owns only
	// its own internal names, so a name it doesn't provide falls through to the
	// caller's resolver (then the global registry), never shadowing it.
	parentResolver, _ := ctx.Value(expctxkeys.ImportResolverKey{}).(experimental.ImportResolver)
	instCtx := experimental.WithImportResolver(ctx, func(name string) api.Module {
		if m := keyToInst[name]; m != nil {
			return m
		}
		if parentResolver != nil {
			return parentResolver(name)
		}
		return nil
	})

	// Register each of this component's OWN resource destructors on its table
	// BEFORE instantiating core modules -- a core module's `start` section runs
	// during its instantiation and may drop a resource, which must run the dtor.
	// The dtor resolver is lazy (its own module may not be up yet here). The tag
	// matches the resource canon path: cfg.resCanon in a composition, else the
	// raw definition index. This is what makes a guest's own resource.drop run
	// its dtor (previously only host-initiated DropResource did).
	instanceDtors := resolveDefinedResourceDtors(comp, coreFuncTarget)
	for defIdx, resolve := range instanceDtors {
		tag := defIdx
		if cfg.resCanon != nil {
			tag = cfg.resCanon(defIdx)
		}
		resources.registerDtor(tag, resourceDtor(resolve))
	}

	// sched.instantiating brackets exactly the window in which THIS
	// component's core modules are instantiated -- including any core wasm
	// `start` section, which the underlying engine runs synchronously as
	// part of instantiateCoreModule below (docs/component-model-async-
	// stackful-design.md §4.3). A sync-lowered call from a start function to
	// an async lift must trap eagerly, before the callee's core code ever
	// runs; the flag lives on the shared *sched (one per composition tree)
	// so a sibling nested component's already-instantiated async export
	// sees it too. Recursing into a NESTED component's own instantiateGraph
	// call (instantiateNestedInstances, below) brackets ITS OWN core-
	// instance loop the same way, so this is correctly scoped per component
	// even though sched.instantiating is one shared bool: core-instance
	// loops across the tree run strictly sequentially, never concurrently.
	sh.instantiating = true
	defer func() { sh.instantiating = false }()
	for k, ci := range comp.CoreInstances {
		// key is BOTH this instance's resolver key and the name a consumer
		// declares to import it: the raw "with" name, the empty-import key, or
		// a synthesized core%d (see moduleKeyForGraph). It is also the groupName
		// for neededTypes lookups (the consumer-declared name).
		key, nerr := moduleKeyForGraph(k, refNames[uint32(k)], emptyNameTarget)
		if nerr != nil {
			return fail(nerr)
		}
		keys[k] = key

		switch ci.Kind {
		case 0x00: // instantiate a real embedded core module
			if int(ci.ModuleIdx) >= len(comp.CoreModules) {
				return fail(fmt.Errorf("component/instance: core instance %d references core module %d, out of range of %d modules", k, ci.ModuleIdx, len(comp.CoreModules)))
			}
			// rewrittenCore[ci.ModuleIdx] already holds this module's bytes after
			// coreModuleBytes + the (stable) empty-import-name rewrite, computed
			// once in discovery above (and cached per component). Reusing it drops
			// the per-instantiation re-slice + re-rewrite. Stable bytes => the
			// compile cache still hits.
			coreBytes := rewrittenCore[ci.ModuleIdx]
			// WithName("") makes the module anonymous (not registered in the
			// global name map -- see store_module_list.go's registerModule); its
			// imports resolve through instCtx's resolver, and other modules
			// import it by its key, not a global name. WithStartFunctions()
			// clears wazy's default "run _start on instantiate": an embedded
			// core module's own _start is invoked later by another module once
			// the graph is wired (see shim.go), not eagerly here.
			mod, err := instantiateCoreModule(instCtx, r, cfg, coreBytes, wazy.NewModuleConfig().WithName("").WithStartFunctions())
			if err != nil {
				return fail(fmt.Errorf("component/instance: instantiate core module %d (key %q): %w", ci.ModuleIdx, key, err))
			}
			instMods[k] = mod
			keyToInst[key] = mod
			closers = append(closers, mod)
			coreModuleCount++

		case 0x01: // inline exports: a passthrough shim regrouping earlier items
			items := make([]shimItem, 0, len(ci.Exports))
			// Every canon-produced func in THIS group is packed into ONE shared
			// host module (built once, below) rather than a module per canon.
			// canonItemIdx[j] is the items[] slot of the j-th collected canon def,
			// wired to the merged module's "f<j>" export after it is built.
			var canonDefs []hostFuncDef
			var canonItemIdx []int
			for _, e := range ci.Exports {
				// Intercept an in-range canon func entry: compute its host func
				// now, defer the module build so the whole group shares one module.
				if e.Sort == 0x00 && int(e.CoreSortIdx) < len(comp.CoreFuncSpace) &&
					comp.CoreFuncSpace[e.CoreSortIdx].Kind == binary.CoreFuncFromCanon {
					centry := comp.CoreFuncSpace[e.CoreSortIdx]
					if int(centry.Canon) >= len(comp.Canons) {
						return fail(fmt.Errorf("component/instance: core instance %d: inline export %q references canon %d, out of range", k, e.Name, centry.Canon))
					}
					def, wasiCall, cerr := computeCanonHostFunc(ctx, r, comp, cfg, resources, in, comp.Canons[centry.Canon], neededTypes, key, e.Name, coreMemTarget, coreFuncTarget)
					if cerr != nil {
						return fail(fmt.Errorf("component/instance: core instance %d: inline export %q: %w", k, e.Name, cerr))
					}
					if wasiCall != "" {
						wasiCalls = append(wasiCalls, wasiCall)
					}
					canonItemIdx = append(canonItemIdx, len(items))
					// FromModule/FromName are filled once the merged module exists.
					items = append(items, shimItem{Sort: shimSortFunc, ExportName: e.Name, Params: def.params, Results: def.results})
					canonDefs = append(canonDefs, def)
					continue
				}
				// key doubles as groupName (the consumer-declared name) for
				// neededTypes lookups; keys names the shim's alias sources.
				item, wasiCall, privMod, err := resolveInlineExportItem(ctx, r, comp, cfg, resources, in, e, coreMemSpace, coreTableSpace, instMods, keys, neededTypes, key, nextPrivateName, coreFuncTarget, coreMemTarget)
				if err != nil {
					return fail(fmt.Errorf("component/instance: core instance %d: %w", k, err))
				}
				items = append(items, item)
				if wasiCall != "" {
					wasiCalls = append(wasiCalls, wasiCall)
				}
				if privMod != nil {
					closers = append(closers, privMod)
				}
			}
			// Build the group's one shared host module for all its canon funcs,
			// then point each canon shim item at its "f<j>" export.
			cacheableShim := true
			if len(canonDefs) > 0 {
				hostMod, herr := buildMergedCanonHostModule(ctx, r, nextPrivateName(), canonDefs)
				if herr != nil {
					return fail(fmt.Errorf("component/instance: core instance %d: %w", k, herr))
				}
				closers = append(closers, hostMod)
				// Reference the merged module by a COMPONENT-CONSTANT resolver key
				// (so the shim bytes are stable and cacheable), registering its
				// UNWRAPPED *wasm.ModuleInstance in keyToInst -- resolveImports
				// type-asserts to that (a host module is a wrapper over it). Defensive
				// fallback: on a key collision or a non-wrapper module, use the
				// per-instantiation global name and mark the shim uncacheable.
				groupKey := canonGroupKey(k)
				u, isWrapper := hostMod.(interface {
					UnwrapModuleInstance() *wasm.ModuleInstance
				})
				fromModule := groupKey
				if _, collides := keyToInst[groupKey]; isWrapper && !collides {
					keyToInst[groupKey] = u.UnwrapModuleInstance()
				} else {
					fromModule = hostMod.Name()
					cacheableShim = false
				}
				for j, idx := range canonItemIdx {
					items[idx].FromModule = fromModule
					items[idx].FromName = canonExportName(j)
				}
			}
			shimBytes, err := buildPassthroughShim(items)
			if err != nil {
				return fail(fmt.Errorf("component/instance: core instance %d: %w", k, err))
			}
			// Anonymous like the embedded modules above; consumers import it by
			// its key via the resolver, not by a global name. Every FromModule is
			// now component-constant (embedded-module keys + the stable canon-group
			// key), so shimBytes are identical every instantiation -- route through
			// the CompileCache (a warm cache then skips re-encoding + recompiling
			// each shim). cacheableShim is false only in the defensive canon-key
			// fallback above, where the bytes are not stable.
			mod, err := instantiateCoreModuleCacheable(instCtx, r, cfg, shimBytes, wazy.NewModuleConfig().WithName("").WithStartFunctions(), cacheableShim)
			if err != nil {
				return fail(fmt.Errorf("component/instance: instantiate regrouping shim for core instance %d (key %q): %w", k, key, err))
			}
			instMods[k] = mod
			keyToInst[key] = mod
			closers = append(closers, mod)

		default:
			return fail(fmt.Errorf("component/instance: core instance %d has unknown kind %#x", k, ci.Kind))
		}
	}

	// Recursively instantiate any nested component instances (comp.Instances,
	// the fused-adapter / nested-composition shape) and link siblings, before
	// binding exports -- an outer export may alias a nested instance's export.
	compInstances, subInstances, err := instantiateNestedInstances(ctx, r, comp, cfg)
	if err != nil {
		return fail(err)
	}
	closeSubs := func() {
		for _, s := range subInstances {
			s.Close(ctx) //nolint:errcheck // best-effort cleanup on an error path
		}
	}

	exports, err := bindImportExportsGraph(comp, componentFunc, coreFuncTarget, resolve, cfg.compileCache, compInstances)
	if err != nil {
		closeSubs()
		return fail(err)
	}

	// The remaining fields are filled in on the SAME *Instance the async
	// builtins above already closed over (see in's allocation, near the top
	// of this function) -- not a fresh struct literal.
	in.exports = exports
	in.instanceExports = buildInstanceExportIndex(exports)
	in.closers = closers
	in.subInstances = subInstances
	in.resourceDtors = instanceDtors
	in.resCanon = cfg.resCanon
	in.resourceOrigin = cfg.resourceOrigin
	in.comp = comp
	in.coreModuleCount = coreModuleCount
	in.wasiCalls = wasiCalls
	in.httpHost = cfg.httpHost
	// A resource type index is guest-owned unless it aliases an imported
	// instance's export (a host-provided resource) -- see resolveArgHandles.
	in.isGuestResource = func(rt uint32) bool {
		_, _, imported := resolveImportedResourceName(comp, rt)
		return !imported
	}
	return in, nil
}

// instantiateNestedInstances recursively instantiates comp.Instances -- the
// nested-component-composition shape, where one component binary declares
// nested component *definitions*, instantiates them, and links a sibling's
// export into a later sibling's import (the fused-adapter test shape). It
// returns a map from component-instance index (imported instances first, then
// comp.Instances) to the sub-Instance, plus the sub-Instances in creation order
// for Close.
//
// The key reuse: a sibling's lifted export is exactly a host import for the
// importer, so it is wired in as a delegating hostImport and the existing
// canon-lower/buildHostWrapper path lowers calls to it unchanged. Kind 0x01
// (inline-export) instances are the pass-through-shim shape handled in
// bindInstanceExportGraph, not here.
func instantiateNestedInstances(ctx context.Context, r wazy.Runtime, comp *binary.Component, cfg *config) (map[int]*Instance, []*Instance, error) {
	if len(comp.Instances) == 0 {
		return nil, nil, nil
	}
	numImported := 0
	for _, im := range comp.Imports {
		if im.ExternType == 0x05 { // instance
			numImported++
		}
	}

	// A component instance re-exported directly as an INSTANCE (the WIT-
	// exports-an-interface shim shape, comp.Exports' ExternType==0x05) is
	// bindInstanceExportGraph's exclusive responsibility -- it does its own
	// instantiation/validation there (nested-component-index bounds,
	// pure-shim shape) and must keep being the ONLY consumer for that shape;
	// don't preempt it here. Per bindInstanceExportGraph's own arithmetic,
	// such an export's ExternIndex IS the component-instance index directly
	// (numImportedInstances + localIdx, matching this func's compInstIdx).
	exportedAsInstance := make(map[int]bool)
	for _, exp := range comp.Exports {
		if exp.ExternType == 0x05 {
			exportedAsInstance[int(exp.ExternIndex)] = true
		}
	}

	// Every OTHER local "instantiate a component" entry (Kind 0x00) must
	// actually be instantiated, per spec, for its observable instantiation-
	// time side effects (a core module's `start` section) -- regardless of
	// whether anything later aliases one of its exports.
	// dont-block-start.wast's second case is exactly this shape: $D is
	// instantiated purely so its start function runs (and traps); nothing
	// ever references $D's exports (it has none). needed therefore
	// defaults to every local Kind==0x00 instance not already claimed by
	// bindInstanceExportGraph above.
	needed := make(map[int]bool)
	for i, inst := range comp.Instances {
		compInstIdx := numImported + i
		if inst.Kind == 0x00 && !exportedAsInstance[compInstIdx] {
			needed[compInstIdx] = true
		}
	}
	if len(needed) == 0 {
		return nil, nil, nil
	}
	// Pull in transitive dependencies: a needed instance's instance-args name
	// earlier (forward-declared) siblings it links, which must exist first.
	for i := len(comp.Instances) - 1; i >= 0; i-- {
		if !needed[numImported+i] {
			continue
		}
		for _, arg := range comp.Instances[i].Args {
			if arg.Sort == 0x05 {
				needed[int(arg.SortIdx)] = true
			}
		}
	}

	byIdx := make(map[int]*Instance, len(comp.Instances))
	var order []*Instance
	failClose := func() {
		for _, s := range order {
			s.Close(ctx) //nolint:errcheck // best-effort cleanup on an error path
		}
	}
	for i, inst := range comp.Instances {
		compInstIdx := numImported + i
		if !needed[compInstIdx] || inst.Kind != 0x00 {
			continue
		}
		if int(inst.ComponentIdx) >= len(comp.NestedComponents) {
			failClose()
			return nil, nil, fmt.Errorf("component/instance: component instance %d references nested component %d, out of range of %d", compInstIdx, inst.ComponentIdx, len(comp.NestedComponents))
		}
		nested := comp.NestedComponents[inst.ComponentIdx]

		// A pure re-export shim (no core module/canon of its own) is seen
		// through by bindInstanceExportGraph -- it just re-exports funcs passed
		// as instantiate-args, which are this component's own canon lifts. Don't
		// recursively instantiate it here (its args are funcs, not siblings);
		// only a nested component with its own core module/canons (the fused-
		// adapter shape) needs real instantiation and sibling linking.
		if validateShimComponent(nested) == nil {
			continue
		}

		// Build the nested component's import environment from the instantiate
		// args. An instance arg names a sibling sub-Instance; each of its
		// plain-func exports satisfies the nested component's instance import of
		// the same (arg) name, matched by func name. A resource the sibling
		// EXPORTS is lined up with the nested component's import of the same name
		// so a handle crossing the boundary is transferred by rep into the
		// nested component's own table under its own resource index, and the
		// sibling's (definer's) destructor is registered under the tag the nested
		// component's resource.drop uses.
		subCfg := &config{
			imports:          map[importKey]*hostImport{},
			compileCache:     cfg.compileCache,
			resCanon:         nested.ResourceDefIndex, // reduce a resource's deftype/export-alias indices to one tag
			importedResDtors: map[uint32]func() api.Function{},
			resourceOrigin:   map[uint32]resourceIdentity{},
			hostResDtors:     map[uint32]func(context.Context, uint32) error{},
			sharedSched:      cfg.sharedSched, // see instantiateGraph's "sh" resolution below
		}
		// typeArgTags maps a resource type the nested component IMPORTS (by its
		// own type-import TypeSpace index) to the composition-global tag the outer
		// provides for it -- so the nested component's own<r>/borrow<r>/
		// resource.drop tag the shared handle table consistently with the
		// provider (a host resource passed in as a `(with "r" (type ...))` arg).
		typeArgTags := map[uint32]uint32{}
		for _, arg := range inst.Args {
			switch arg.Sort {
			case 0x05: // instance: a sibling sub-Instance
				sib, ok := byIdx[int(arg.SortIdx)]
				if !ok {
					failClose()
					return nil, nil, fmt.Errorf("component/instance: component instance %d arg %q references instance %d, which is not a prior nested instantiation", compInstIdx, arg.Name, arg.SortIdx)
				}
				// Line up the sibling's exported resources with the nested
				// component's imports of the same name, plus the definer's dtor
				// and its composition-wide resourceIdentity (so a LATER sibling
				// that imports the very same resource through sib -- without sib
				// itself re-exporting it by name, e.g. dont-drop's `(borrow $R)`
				// param in drop-cross-task-borrow.wast, where $D never exports
				// "R" at all -- can still recognize it as the same resource; see
				// provToImp below and resourceIdentity's doc).
				importerResIdx := importedResourceIndices(nested, arg.Name)
				provDefToName := map[uint32]string{}
				if sib.comp != nil {
					for rname, sibDef := range exportedResourceDefs(sib.comp) {
						provDefToName[sibDef] = rname
						if dIdx, ok := importerResIdx[rname]; ok {
							tag := nested.ResourceDefIndex(dIdx)
							if dtor := sib.resourceDtors[sibDef]; dtor != nil {
								subCfg.importedResDtors[tag] = dtor
							}
							subCfg.resourceOrigin[tag] = sib.originOf(sibDef)
						}
					}
				}
				// provToImp translates a resource type index as it appears in
				// sib's OWN func descriptors (e.g. sib's borrow<R> param) to
				// nested's local resource tag for the SAME underlying resource.
				// Try resourceIdentity first: it matches even when sib itself
				// never re-exports the resource under any name (sib merely
				// reuses a resource it imports from a further sibling) -- the
				// name-based provDefToName/importerResIdx pair (populated only
				// from sib's OWN exports) can't see that case at all. Fall back
				// to the name-based path for anything resourceIdentity doesn't
				// (yet) cover, preserving prior behavior exactly when it applied.
				provToImp := func(provIdx uint32) (uint32, bool) {
					origin := sib.originOf(sib.canonTag(provIdx))
					for dIdx, o := range subCfg.resourceOrigin {
						if o == origin {
							return dIdx, true
						}
					}
					name, ok := provDefToName[sib.canonTag(provIdx)]
					if !ok {
						return 0, false
					}
					dIdx, ok := importerResIdx[name]
					return dIdx, ok
				}
				for name, be := range sib.exports {
					if strings.ContainsRune(name, '#') { // interface-member export, not a plain func
						continue
					}
					hi := delegatingHostImport(sib, name, be, provToImp) // sync arm, unchanged
					// Register the async arm too (Phase 3, docs/component-
					// model-async-phase3-design.md §3.1): an async lower
					// through this import now routes to sib's export as a
					// guestTask instead of bind-failing with "register it
					// with WithAsyncImport instead" -- see
					// computeCanonHostFunc's lower case and
					// buildAsyncHostWrapper's callee arm.
					hi.asyncTarget = &guestAsyncTarget{sub: sib, be: be, exportName: name, provToImp: provToImp}
					subCfg.imports[mkImportKey(arg.Name, name)] = hi
				}

			case 0x01: // func: satisfy the nested component's func import of the
				// same name with whatever the outer's aliased func names --
				// either a host import (outerFuncArgImport falls back to
				// importInterfaceName) or, just as often in the async
				// suites' multi-nested-component .wast shape, a single named
				// export of an earlier sibling nested instance (byIdx).
				hi, err := outerFuncArgImport(comp, cfg, byIdx, numImported, arg.SortIdx)
				if err != nil {
					failClose()
					return nil, nil, fmt.Errorf("component/instance: component instance %d arg %q: %w", compInstIdx, arg.Name, err)
				}
				subCfg.imports[mkImportKey(arg.Name, "")] = hi

			case 0x03: // type: pass a resource type in. Tag the nested component's
				// import of it with the outer's tag for the same resource, and
				// carry the host destructor.
				tag := effectiveResourceTypeIdx(comp, cfg, arg.SortIdx)
				if idx, ok := importedTypeIndex(nested, arg.Name); ok {
					typeArgTags[idx] = tag
				}
				if dtor := cfg.hostResDtors[tag]; dtor != nil {
					subCfg.hostResDtors[tag] = dtor
				}

			default:
				failClose()
				return nil, nil, fmt.Errorf("component/instance: component instance %d arg %q: unsupported sort %#x", compInstIdx, arg.Name, arg.Sort)
			}
		}
		// If any type args mapped a nested type import to an outer tag, layer that
		// over the default deftype/export-alias canonicalizer.
		if len(typeArgTags) > 0 {
			base := nested.ResourceDefIndex
			subCfg.resCanon = func(idx uint32) uint32 {
				if t, ok := typeArgTags[idx]; ok {
					return t
				}
				return base(idx)
			}
		}

		sub, err := instantiateGraph(ctx, r, nested, nested.Bytes, subCfg)
		if err != nil {
			failClose()
			return nil, nil, fmt.Errorf("component/instance: nested component instance %d: %w", compInstIdx, err)
		}
		byIdx[compInstIdx] = sub
		order = append(order, sub)
	}
	return byIdx, order, nil
}

// delegatingHostImport wraps a provider sub-Instance's exported func as a host
// import for a sibling importer. The importer's host wrapper is given the
// signature re-pointed to the IMPORTER's own resource type indices
// (translateResourceFD + the importer's resolver), so a crossing resource handle
// is minted/looked-up in the importer's own table under its own index --
// consistent with the importer's resource.drop, and with per-instance handle
// numbering. Inside, own<R>/borrow<R> args arrive as reps (the importer wrapper
// already reduced its handle) and are re-minted in the PROVIDER's table for the
// provider call; own/borrow results (provider handles) are reduced back to reps
// for the importer wrapper. Non-resource params pass straight through -- the
// fused-adapter (char roundtrip) case leaves the signature unchanged.
//
// hasBorrowParam is computed once here (a paramDescs walk), not per call: a
// cross-instance borrow<R> param needs a call-scoped mint (Phase 3, docs/
// component-model-async-phase3-design.md §4.1 site 1/site 3) -- the
// PROVIDER's borrow handle is minted scoped to a scope-only *task built
// fresh for this one call (never entered, no be, no gt: it exists purely to
// carry numBorrows, mirroring the reference sync-lift's own trap_if
// (num_borrows > 0) at task exit, which wazy's sync invoke() has no task of
// its own to run). A func with no borrow param skips the scope entirely
// (scope stays nil; repToProviderHandle then takes its unscoped NewBorrow
// fallback, unreachable here since no BorrowDesc param exists to trigger it).
func delegatingHostImport(sub *Instance, exportName string, be *boundExport, provToImp func(uint32) (uint32, bool)) *hostImport {
	fd, importerResolve := translateResourceFD(be.fd, sub.resolve, provToImp)
	paramDescs := be.paramTypes
	resDescs := resultDescs(be)
	hasBorrowParam := false
	for _, d := range paramDescs {
		if _, ok := d.(binary.BorrowDesc); ok {
			hasBorrowParam = true
			break
		}
	}
	return &hostImport{
		fn: func(ctx context.Context, args []abi.Value) ([]abi.Value, error) {
			var scope *task
			if hasBorrowParam {
				scope = &task{inst: sub}
			}
			in := make([]abi.Value, len(args))
			for i, a := range args {
				var err error
				if i < len(paramDescs) {
					a, err = repToProviderHandle(sub, paramDescs[i], a, scope)
					if err != nil {
						return nil, err
					}
				}
				in[i] = a
			}
			out, err := sub.invoke(ctx, be, exportName, in)
			if err != nil {
				return nil, err
			}
			// Sync-callee exit trap (§4.1 site 3): the reference's sync
			// canon_lift calls task.return_ itself, so its trap_if
			// (num_borrows > 0) covers this; wazy's sync invoke() has no
			// task, so the delegate supplies the equivalent check here.
			if scope != nil && scope.numBorrows > 0 {
				return nil, fmt.Errorf("component/instance: %s: borrow handles still remain at the end of the call (%d still held)", exportName, scope.numBorrows)
			}
			for i := range out {
				if i < len(resDescs) {
					if out[i], err = providerHandleToRep(sub, resDescs[i], out[i]); err != nil {
						return nil, err
					}
				}
			}
			return out, nil
		},
		customFD:      &fd,
		customResolve: importerResolve,
	}
}

// startDelegatedFromStackful is delegatingHostImport's arg/result resource
// mapping, run for a SYNC-lowered call whose callee is an async lift
// (callback or stackful) AND whose caller is itself a stackful task
// (docs/component-model-async-stackful-design.md §4.4): instead of
// sub.invoke's nested sched.drive (delegatingHostImport's fn, which would
// frame-hold the caller's own continuation beneath the callee's suspension
// -- exactly async-calls-sync's confirmed livelock), it starts the callee as
// an async export task and PARKS the calling stackful goroutine
// (thread.wait_until(subtask.resolved), ~2273-2275) instead of driving,
// letting the shared scheduler make progress on whatever else is queued
// while this task waits. host_import.go's buildHostWrapper calls this in
// place of hi.fn when it detects this exact shape; everything else
// (host-entry sync calls, a callback-caller's own nested drive) keeps using
// hi.fn unchanged.
func startDelegatedFromStackful(ctx context.Context, st *stackfulTask, tgt *guestAsyncTarget, args []abi.Value) ([]abi.Value, error) {
	sub, be, exportName := tgt.sub, tgt.be, tgt.exportName
	paramDescs := be.paramTypes
	resDescs := resultDescs(be)
	hasBorrowParam := false
	for _, d := range paramDescs {
		if _, ok := d.(binary.BorrowDesc); ok {
			hasBorrowParam = true
			break
		}
	}
	var scope *task
	if hasBorrowParam {
		scope = &task{inst: sub}
	}
	in := make([]abi.Value, len(args))
	for i, a := range args {
		var err error
		if i < len(paramDescs) {
			a, err = repToProviderHandle(sub, paramDescs[i], a, scope)
			if err != nil {
				return nil, err
			}
		}
		in[i] = a
	}

	var resolved bool
	var res []abi.Value
	onStart := func(*task) ([]abi.Value, error) { return in, nil }
	onResolve := func(vals []abi.Value, cancelled bool) error {
		// cancelled can't be true here: nothing requests cancellation of an
		// anonymous sync-lower subtask (there is no handle to cancel it by).
		res, resolved = vals, true
		return nil
	}
	if _, err := sub.startAsyncExportTask(ctx, be, exportName, onStart, onResolve); err != nil {
		return nil, err
	}
	if !resolved {
		st.block(func() bool { return resolved }, false) // non-cancellable, per ~2275
	}

	if scope != nil && scope.numBorrows > 0 {
		return nil, fmt.Errorf("component/instance: %s: borrow handles still remain at the end of the call (%d still held)", exportName, scope.numBorrows)
	}
	out := make([]abi.Value, len(res))
	for i, v := range res {
		if i < len(resDescs) {
			var err error
			if v, err = providerHandleToRep(sub, resDescs[i], v); err != nil {
				return nil, err
			}
		}
		out[i] = v
	}
	return out, nil
}

// moduleKeyForGraph is moduleNameFor's graph-engine counterpart: identical
// except that a sole "" ref name (see importrewrite.go) maps to
// emptyNameTarget instead of the literal empty string, since wazy's module
// registry cannot resolve an empty-named module by import.
func moduleKeyForGraph(coreInstanceIdx int, refNames []string, emptyNameTarget string) (string, error) {
	switch len(refNames) {
	case 0:
		// Unreferenced (e.g. the root): nothing imports it, so any distinct key
		// works; core%d is unique within this component.
		return fmt.Sprintf("core%d", coreInstanceIdx), nil
	case 1:
		if refNames[0] == "" {
			return emptyNameTarget, nil
		}
		// The raw "with" name is exactly what consumers import; the resolver
		// maps it to this (anonymous) instance. No prefix: the key lives only
		// in this component's private resolver map, never the global registry.
		return refNames[0], nil
	default:
		return "", fmt.Errorf("component/instance: core instance %d is referenced under %d names (%v); a core module can only be registered under one name", coreInstanceIdx, len(refNames), refNames)
	}
}

// discoverNeededFuncTypes decodes every embedded core module (applying the
// empty-import-name rewrite first, since wazy's own decoder rejects an empty
// import module name outright) and records the core-level signature of every
// func it imports, keyed by (module name, field name). See this file's package
// doc (step 5) for why this is the only available source of truth for a lowered
// import's core-level type.
//
// It decodes ONLY -- no native codegen. An import's module/field/type index is
// present in the decoded structure directly, so the full compile this used to
// do here was pure waste: every embedded core module got compiled twice per
// instantiation (once as a throwaway just to read imports, once for real in the
// loop below). Reading TypeSection by the import's own type index, with an
// explicit bounds check, also sidesteps buildFunctionDefinitions (only safe
// after validation, which a decode-only pass skips). The real, single compile
// -- cache-aware -- still happens in instantiateCoreModuleCacheable.
func discoverNeededFuncTypes(r wazy.Runtime, comp *binary.Component, componentBytes []byte, emptyNameTarget string) (map[string]map[string]coreFuncSig, [][]byte, error) {
	dec, ok := r.(interface {
		DecodeModuleNoCompile(bin []byte) (*wasm.Module, error)
	})
	if !ok {
		return nil, nil, fmt.Errorf("component/instance: runtime %T cannot decode core modules for import discovery", r)
	}
	out := make(map[string]map[string]coreFuncSig)
	// rewritten[i] is core module i's bytes after coreModuleBytes + the empty-
	// import-name rewrite -- the exact bytes the main instantiation loop needs,
	// captured here (the rewrite already happens for discovery) so the loop
	// reuses them instead of re-slicing and re-rewriting every instantiation.
	rewritten := make([][]byte, len(comp.CoreModules))
	for i, cm := range comp.CoreModules {
		coreBytes, err := coreModuleBytes(cm, componentBytes)
		if err != nil {
			return nil, nil, err
		}
		if emptyNameTarget != "" {
			coreBytes, _, err = rewriteEmptyImportModuleName(coreBytes, emptyNameTarget)
			if err != nil {
				return nil, nil, fmt.Errorf("component/instance: core module %d: %w", i, err)
			}
		}
		rewritten[i] = coreBytes
		mod, err := dec.DecodeModuleNoCompile(coreBytes)
		if err != nil {
			return nil, nil, fmt.Errorf("component/instance: core module %d: discover import types: %w", i, err)
		}
		for j := range mod.ImportSection {
			imp := &mod.ImportSection[j]
			if imp.Type != wasm.ExternTypeFunc {
				continue
			}
			if int(imp.DescFunc) >= len(mod.TypeSection) {
				return nil, nil, fmt.Errorf("component/instance: core module %d: import %q.%q references type index %d, out of range of the %d-entry type section", i, imp.Module, imp.Name, imp.DescFunc, len(mod.TypeSection))
			}
			ft := &mod.TypeSection[imp.DescFunc]
			if out[imp.Module] == nil {
				out[imp.Module] = make(map[string]coreFuncSig)
			}
			out[imp.Module][imp.Name] = coreFuncSig{
				params:  wasm.ToApiValueType(ft.Params),
				results: wasm.ToApiValueType(ft.Results),
			}
		}
	}
	return out, rewritten, nil
}

// resolveInlineExportItem resolves one CoreInlineExport entry (func, memory,
// or table sort) to the shimItem that re-exports it. groupName is the wazy
// module name the enclosing shim will be registered under -- used to look up
// this entry's required core-level type in neededTypes when it's a canon-
// produced func with no caller-supplied implementation. It returns the
// "iface.func" WASI call name when this entry is a canon lower, for
// Instance.WASICalls -- empty otherwise -- and, when this entry is a
// canon-produced func, the private host module buildCanonHostModule
// instantiated for it (nil otherwise): the caller must append this to
// Instance.closers itself (see instantiateGraph), since it is a real,
// separately-registered module the shim only imports from, and Close would
// otherwise leak it (and, on Runtime reuse, permanently occupy its private
// name -- see bench_test.go's BenchmarkInstantiateHello doc for the bug this
// closes).
// keys names each already-instantiated core instance under its resolver key
// (see instantiateGraph): a shim imports an anonymous alias source (embedded
// module or earlier shim) by that key, not by mod.Name() -- which is "" for an
// anonymous module.
func resolveInlineExportItem(
	ctx context.Context, r wazy.Runtime, comp *binary.Component, cfg *config, resources *handleTable, in *Instance,
	e binary.CoreInlineExport, coreMemSpace, coreTableSpace []aliasTarget, instMods map[int]api.Module, keys []string,
	neededTypes map[string]map[string]coreFuncSig, groupName string, nextPrivateName func() string,
	coreFuncTarget func(int) (api.Module, string, error), coreMemTarget func(int) (api.Module, error),
) (shimItem, string, api.Module, error) {
	switch e.Sort {
	case 0x00: // func
		if int(e.CoreSortIdx) >= len(comp.CoreFuncSpace) {
			return shimItem{}, "", nil, fmt.Errorf("inline export %q references core func %d, out of range of the %d-entry core func index space", e.Name, e.CoreSortIdx, len(comp.CoreFuncSpace))
		}
		entry := comp.CoreFuncSpace[e.CoreSortIdx]
		switch entry.Kind {
		case binary.CoreFuncFromAlias:
			al := comp.Aliases[entry.Alias]
			mod, ok := instMods[int(al.InstanceIdx)]
			if !ok {
				return shimItem{}, "", nil, fmt.Errorf("inline export %q: core func %d targets core instance %d, which was not instantiated", e.Name, e.CoreSortIdx, al.InstanceIdx)
			}
			fn := mod.ExportedFunction(al.Name)
			if fn == nil {
				return shimItem{}, "", nil, fmt.Errorf("inline export %q: core instance %d (%q) has no exported function %q", e.Name, al.InstanceIdx, mod.Name(), al.Name)
			}
			def := fn.Definition()
			return shimItem{Sort: shimSortFunc, FromModule: keys[int(al.InstanceIdx)], FromName: al.Name, ExportName: e.Name, Params: def.ParamTypes(), Results: def.ResultTypes()}, "", nil, nil

		case binary.CoreFuncFromCanon:
			canon := comp.Canons[entry.Canon]
			mod, exportName, params, results, wasiCall, err := buildCanonHostModule(ctx, r, comp, cfg, resources, in, canon, neededTypes, groupName, e.Name, nextPrivateName(), coreMemTarget, coreFuncTarget)
			if err != nil {
				return shimItem{}, "", nil, fmt.Errorf("inline export %q: %w", e.Name, err)
			}
			// mod is a host module here (see buildCanonHostModule): its
			// ExportedFunction is deliberately forbidden to call directly
			// (see hostModuleInstance in builder.go), so params/results come
			// straight from what buildCanonHostModule itself declared the Go
			// func with, not by re-querying mod.
			return shimItem{Sort: shimSortFunc, FromModule: mod.Name(), FromName: exportName, ExportName: e.Name, Params: params, Results: results}, wasiCall, mod, nil

		default:
			return shimItem{}, "", nil, fmt.Errorf("inline export %q: unknown core func space entry kind %d", e.Name, entry.Kind)
		}

	case 0x02: // memory
		if int(e.CoreSortIdx) >= len(coreMemSpace) {
			return shimItem{}, "", nil, fmt.Errorf("inline export %q references core memory %d, out of range of the %d-entry core memory index space", e.Name, e.CoreSortIdx, len(coreMemSpace))
		}
		at := coreMemSpace[e.CoreSortIdx]
		_, ok := instMods[int(at.instIdx)]
		if !ok {
			return shimItem{}, "", nil, fmt.Errorf("inline export %q: core memory %d targets core instance %d, which was not instantiated", e.Name, e.CoreSortIdx, at.instIdx)
		}
		return shimItem{Sort: shimSortMemory, FromModule: keys[int(at.instIdx)], FromName: at.name, ExportName: e.Name}, "", nil, nil

	case 0x01: // table
		if int(e.CoreSortIdx) >= len(coreTableSpace) {
			return shimItem{}, "", nil, fmt.Errorf("inline export %q references core table %d, out of range of the %d-entry core table index space", e.Name, e.CoreSortIdx, len(coreTableSpace))
		}
		at := coreTableSpace[e.CoreSortIdx]
		_, ok := instMods[int(at.instIdx)]
		if !ok {
			return shimItem{}, "", nil, fmt.Errorf("inline export %q: core table %d targets core instance %d, which was not instantiated", e.Name, e.CoreSortIdx, at.instIdx)
		}
		return shimItem{Sort: shimSortTable, FromModule: keys[int(at.instIdx)], FromName: at.name, ExportName: e.Name}, "", nil, nil

	default:
		return shimItem{}, "", nil, fmt.Errorf("inline export %q has unsupported core:sort %#x; only func (0x00), table (0x01), and memory (0x02) are supported by the graph engine", e.Name, e.Sort)
	}
}

// buildCanonHostModule builds a fresh, uniquely-named single-func Go host
// module backing one core-func-producing canon (lower, or one of the three
// resource canons), returning the module and the name ("f") it exports that
// func under. A lower canon with a caller-supplied WithImport uses it
// (reusing buildHostWrapper's full WIT-level lift/lower machinery,
// unchanged); otherwise it becomes a trap stub whose core-level signature
// comes from neededTypes[groupName][entryName] -- the type the real core
// module that will eventually consume this group's re-export already
// commits to -- and which panics naming the WASI iface+func, returned as
// wasiCall for Instance.WASICalls (empty for a resource canon or a
// caller-overridden lower).
// computeCanonHostFunc resolves one core-func-producing canon (a lower, or one
// of the three resource canons) to the Go host func that backs it, WITHOUT
// instantiating a module for it -- the caller batches every canon func in an
// inline-export group into a single host module (see buildMergedCanonHostModule),
// instead of paying a separate builder + compile + instantiate + sys.Context per
// canon. See buildCanonHostModule's (removed) doc, preserved below, for the
// lower/trap-stub/resource behaviors.
func computeCanonHostFunc(
	ctx context.Context, r wazy.Runtime, comp *binary.Component, cfg *config, resources *handleTable, in *Instance,
	canon binary.Canon, neededTypes map[string]map[string]coreFuncSig, groupName, entryName string,
	coreMemTarget func(int) (api.Module, error), coreFuncTarget func(int) (api.Module, string, error),
) (def hostFuncDef, wasiCall string, err error) {
	switch canon.Kind {
	case 0x01: // lower
		// Resolve the lowered func through the authoritative component func
		// index space, which includes func IMPORTS (a top-level func import a
		// nested component is parameterized by) as well as instance-export
		// aliases -- the older aliases+lifts reconstruction miscounts when an
		// import precedes them.
		fi := canon.FuncIdx
		if int(fi) >= len(comp.ComponentFuncSpace) {
			return hostFuncDef{}, "", fmt.Errorf("lower func index %d out of range of the component func index space", fi)
		}
		fe := comp.ComponentFuncSpace[fi]

		// isAsyncLower is CanonOpt kind 0x06 (async), the lower side of the
		// same bit bindFuncExportGraph checks for a lift (isAsyncLift):
		// docs/component-model-async-runtime-design.md §2. A lower with this
		// option creates a Subtask instead of calling straight through --
		// see buildAsyncHostWrapper. Computed before the fe.Kind switch below
		// so the ComponentFuncFromImport case can validate it against the
		// import's own declared type (validate-no-async-abi-for-sync-type.wast).
		isAsyncLower := false
		for _, opt := range canon.Opts {
			if opt.Kind == 0x06 {
				isAsyncLower = true
				break
			}
		}

		var iface, fname string
		switch fe.Kind {
		case binary.ComponentFuncFromImport:
			if int(fe.Import) >= len(comp.Imports) {
				return hostFuncDef{}, "", fmt.Errorf("lower func index %d: import out of range", fi)
			}
			im := comp.Imports[fe.Import]
			if im.ExternType != 0x01 { // func
				return hostFuncDef{}, "", fmt.Errorf("lower func index %d: import %q is not a func", fi, im.Name)
			}
			// A top-level func import is keyed by its own name (no interface).
			iface, fname = im.Name, ""
		case binary.ComponentFuncFromAlias:
			al := comp.Aliases[fe.Alias]
			if al.Sort != 0x01 || al.TargetKind != 0x00 {
				return hostFuncDef{}, "", fmt.Errorf("lower func index %d: unsupported alias %#x/%#x", fi, al.Sort, al.TargetKind)
			}
			var ierr error
			if iface, ierr = importInterfaceName(comp, al.InstanceIdx); ierr != nil {
				return hostFuncDef{}, "", ierr
			}
			fname = al.Name
		default:
			return hostFuncDef{}, "", fmt.Errorf("lowers a lifted (exported) func rather than an import; unsupported")
		}
		wasiCall = iface + "." + fname

		if hi, ok := cfg.imports[mkImportKey(iface, fname)]; ok {
			// A sync-registered (WithImport) or async-registered
			// (WithAsyncImport) import may only back a lower that agrees --
			// see WithAsyncImport's doc. hi.fn/hi.asyncFn are mutually
			// exclusive (set by exactly one of the two registration Options).
			// A composition delegate (hi.asyncTarget != nil, Phase 3 §3.1)
			// satisfies an async lower too -- it routes to the sibling's
			// export as a guestTask instead of a Go AsyncHostFunc.
			if isAsyncLower && hi.asyncFn == nil && hi.asyncTarget == nil {
				return hostFuncDef{}, "", fmt.Errorf("import %q func %q is lowered with the async option, but was registered via WithImport; register it with WithAsyncImport instead", iface, fname)
			}
			if !isAsyncLower && hi.fn == nil {
				return hostFuncDef{}, "", fmt.Errorf("import %q func %q is lowered synchronously, but was registered via WithAsyncImport; register it with WithImport instead", iface, fname)
			}

			memMod, reallocFn, merr := canonMemoryAndRealloc(canon, coreMemTarget, coreFuncTarget)
			if merr != nil {
				return hostFuncDef{}, "", fmt.Errorf("import %q func %q: %w", iface, fname, merr)
			}
			var fn api.GoModuleFunction
			var hiParams, hiResults []api.ValueType
			var herr error
			if isAsyncLower {
				fn, hiParams, hiResults, herr = buildAsyncHostWrapper(in, iface, fname, hi, resources, memMod, reallocFn)
			} else {
				fn, hiParams, hiResults, herr = buildHostWrapper(in, iface, fname, hi, resources, memMod, reallocFn)
			}
			if herr != nil {
				return hostFuncDef{}, "", herr
			}
			def = hostFuncDef{fn: fn, params: hiParams, results: hiResults}
			wasiCall = "" // caller-provided, not a trap stub
		} else {
			sig, ok := neededTypes[groupName][entryName]
			if !ok {
				return hostFuncDef{}, "", fmt.Errorf("cannot determine the core-level signature for lowered import %q %q: no consumer declares module %q field %q", iface, fname, groupName, entryName)
			}
			trapIface, trapName := iface, fname
			fn := api.GoModuleFunc(func(context.Context, api.Module, []uint64) {
				panic(fmt.Errorf("component/instance: WASI %s.%s not implemented (trap stub)", trapIface, trapName))
			})
			def = hostFuncDef{fn: fn, params: sig.params, results: sig.results}
		}

	case 0x02, 0x03, 0x04, binary.CanonKindResourceDropAsync: // resource.new, resource.drop (sync/async), resource.rep
		var rerr error
		def, rerr = resourceCanonHostFuncGraph(comp, cfg, resources, entryName, canon)
		if rerr != nil {
			return hostFuncDef{}, "", rerr
		}

	// --- MVP async builtins (docs/component-model-async-runtime-design.md
	// §1.5) -- wired the same way as a resource canon above: a fixed-i32
	// core func closing over the *Instance, no reflection/WithFunc. See
	// async_builtins.go. Every other async canon kind (waitable-set.*,
	// subtask.*, stream.*, future.*, error-context.*, task.cancel) is Phase
	// 1c/2/3 and still falls through to the default "does not produce a
	// core func" error below.
	case binary.CanonKindTaskCancel:
		def = taskCancelHostFuncGraph(in)

	case binary.CanonKindSubtaskCancel:
		def = subtaskCancelHostFuncGraph(in, canon)

	case binary.CanonKindTaskReturn:
		var terr error
		def, terr = taskReturnHostFuncGraph(in, canon)
		if terr != nil {
			return hostFuncDef{}, "", terr
		}

	case binary.CanonKindContextGet:
		var cerr error
		def, cerr = contextGetHostFuncGraph(in, canon)
		if cerr != nil {
			return hostFuncDef{}, "", cerr
		}

	case binary.CanonKindContextSet:
		var cerr error
		def, cerr = contextSetHostFuncGraph(in, canon)
		if cerr != nil {
			return hostFuncDef{}, "", cerr
		}

	case binary.CanonKindBackpressureInc:
		def = backpressureIncHostFuncGraph(in)

	case binary.CanonKindBackpressureDec:
		def = backpressureDecHostFuncGraph(in)

	case binary.CanonKindWaitableSetNew:
		def = waitableSetNewHostFunc(in)

	case binary.CanonKindWaitableSetWait:
		def = waitableSetWaitHostFunc(in, canon)

	case binary.CanonKindWaitableSetPoll:
		def = waitableSetPollHostFunc(in, canon)

	case binary.CanonKindWaitableSetDrop:
		def = waitableSetDropHostFunc(in)

	case binary.CanonKindWaitableJoin:
		def = waitableJoinHostFunc(in)

	case binary.CanonKindSubtaskDrop:
		def = subtaskDropHostFunc(in)

	// --- Phase 2: streams/futures/error-context
	// (docs/component-model-async-phase2-design.md) -- wired the same way as
	// the MVP async builtins above: a fixed-i32 core func closing over the
	// *Instance, no reflection/WithFunc. See stream_builtins.go/errorcontext.go.
	case binary.CanonKindStreamNew, binary.CanonKindStreamRead, binary.CanonKindStreamWrite,
		binary.CanonKindStreamCancelRead, binary.CanonKindStreamCancelWrite,
		binary.CanonKindStreamDropReadable, binary.CanonKindStreamDropWritable,
		binary.CanonKindFutureNew, binary.CanonKindFutureRead, binary.CanonKindFutureWrite,
		binary.CanonKindFutureCancelRead, binary.CanonKindFutureCancelWrite,
		binary.CanonKindFutureDropReadable, binary.CanonKindFutureDropWritable:
		var serr error
		def, serr = streamFutureCanonHostFunc(comp, in, canon, coreMemTarget, coreFuncTarget)
		if serr != nil {
			return hostFuncDef{}, "", serr
		}

	case binary.CanonKindErrorContextNew:
		memMod, _, merr := canonMemoryAndRealloc(canon, coreMemTarget, coreFuncTarget)
		if merr != nil {
			return hostFuncDef{}, "", merr
		}
		def = errorContextNewHostFunc(in, memMod)

	case binary.CanonKindErrorContextDebugMessage:
		memMod, reallocFn, merr := canonMemoryAndRealloc(canon, coreMemTarget, coreFuncTarget)
		if merr != nil {
			return hostFuncDef{}, "", merr
		}
		def = errorContextDebugMessageHostFunc(in, memMod, reallocFn)

	case binary.CanonKindErrorContextDrop:
		def = errorContextDropHostFunc(in)

	default:
		return hostFuncDef{}, "", fmt.Errorf("references a canon of kind %#x, which does not produce a core func", canon.Kind)
	}

	return def, wasiCall, nil
}

// canonExportName is the export name buildMergedCanonHostModule gives the i-th
// canon func packed into a group's shared host module. Stable within a build so
// the passthrough shim can import each func by name.
func canonExportName(i int) string { return fmt.Sprintf("f%d", i) }

// canonGroupKey is the COMPONENT-CONSTANT resolver key under which core instance
// k's merged canon host module is registered in keyToInst. Using a stable key
// for the shim's FromModule (rather than the host module's per-instantiation
// global name) keeps the group's passthrough-shim bytes identical every
// instantiation, so the compile cache hits instead of recompiling the shim.
func canonGroupKey(k int) string { return fmt.Sprintf("wazy:canon-group/%d", k) }

// buildMergedCanonHostModule instantiates ONE Go host module named privateName
// exporting every def in defs (func i under canonExportName(i) = "f<i>"),
// instead of a separate module per canon func. All canon funcs of one inline-
// export group share a module this way, cutting the per-canon builder + compile
// + instantiate + sys.Context down to one per group.
func buildMergedCanonHostModule(ctx context.Context, r wazy.Runtime, privateName string, defs []hostFuncDef) (api.Module, error) {
	b := r.NewHostModuleBuilder(privateName)
	for i, def := range defs {
		b = b.NewFunctionBuilder().WithGoModuleFunction(def.fn, def.params, def.results).Export(canonExportName(i))
	}
	hostMod, err := b.Instantiate(ctx)
	if err != nil {
		return nil, fmt.Errorf("instantiate private host func module %q (%d canon func(s)): %w", privateName, len(defs), err)
	}
	return hostMod, nil
}

// buildCanonHostModule builds a single-func private host module for one canon,
// exporting it as "f" (the isolated-canon shape the canonMods path and tests
// use). The graph's inline-export loop instead uses computeCanonHostFunc +
// buildMergedCanonHostModule to pack a whole group's canon funcs into one module.
func buildCanonHostModule(
	ctx context.Context, r wazy.Runtime, comp *binary.Component, cfg *config, resources *handleTable, in *Instance,
	canon binary.Canon, neededTypes map[string]map[string]coreFuncSig, groupName, entryName, privateName string,
	coreMemTarget func(int) (api.Module, error), coreFuncTarget func(int) (api.Module, string, error),
) (mod api.Module, exportName string, params, results []api.ValueType, wasiCall string, err error) {
	def, wasiCall, err := computeCanonHostFunc(ctx, r, comp, cfg, resources, in, canon, neededTypes, groupName, entryName, coreMemTarget, coreFuncTarget)
	if err != nil {
		return nil, "", nil, nil, "", err
	}
	b := r.NewHostModuleBuilder(privateName)
	b = b.NewFunctionBuilder().WithGoModuleFunction(def.fn, def.params, def.results).Export("f")
	hostMod, err := b.Instantiate(ctx)
	if err != nil {
		return nil, "", nil, nil, "", fmt.Errorf("instantiate private host func module %q: %w", privateName, err)
	}
	return hostMod, "f", def.params, def.results, wasiCall, nil
}

// canonMemoryAndRealloc resolves a canon lower's own "memory" (CanonOpt kind
// 0x03, a core memory index) and "realloc" (kind 0x04, a core func index)
// options -- when present -- to the real module that provides each, via
// coreMemTarget and coreFuncTarget respectively. These become
// buildHostWrapper's memOverride/reallocOverride (see its doc): per the
// Canonical ABI, they are static choices fixed by the component binary, not
// whichever module happens to directly execute the `call` reaching the host
// func at runtime -- which, in a real wasip2 CLI adapter graph, is often an
// indirect call-table trampoline module with no memory of its own (see this
// file's package doc). A canon with neither option (most resource-only or
// memory-free lowers) returns (nil, nil, nil): buildHostWrapper then falls
// back to the runtime caller, exactly as before this existed.
func canonMemoryAndRealloc(canon binary.Canon, coreMemTarget func(int) (api.Module, error), coreFuncTarget func(int) (api.Module, string, error)) (memMod api.Module, reallocFn api.Function, err error) {
	for _, opt := range canon.Opts {
		switch opt.Kind {
		case 0x03: // memory
			mod, merr := coreMemTarget(int(opt.Idx))
			if merr != nil {
				return nil, nil, fmt.Errorf("canon lower memory option: %w", merr)
			}
			memMod = mod

		case 0x04: // realloc
			mod, name, rerr := coreFuncTarget(int(opt.Idx))
			if rerr != nil {
				return nil, nil, fmt.Errorf("canon lower realloc option: %w", rerr)
			}
			fn := mod.ExportedFunction(name)
			if fn == nil {
				return nil, nil, fmt.Errorf("canon lower realloc option: core instance %q has no exported function %q", mod.Name(), name)
			}
			reallocFn = fn
		}
	}
	return memMod, reallocFn, nil
}

// resourceCanonHostFuncGraph is resourceCanonHostFunc's graph-engine
// counterpart. It builds the exact same fixed-signature host func (see
// resourceCanonHostFunc's doc), keyed by canon.TypeIdx as an opaque tag, but
// tolerates comp.ResolveType failing on canon.TypeIdx with the
// binary.ResolveType-documented "alias exports ... from [an imported]
// instance" gap: unlike simpler fixtures (which only declare
// resource types locally), a real wasip2 CLI adapter's resource canons
// almost always tag a resource type owned by an *imported* WASI interface
// (e.g. wasi:filesystem/types' "descriptor") whose nested type declarations
// this decoder does not retain (see typespace.go's doc) -- a hard
// requirement here would make every such canon unresolvable. The tag itself
// (canon.TypeIdx, or its cfg-translated form -- see effectiveResourceTypeIdx's
// doc) does not depend on resolving the type at all, so this still validates
// when it *can* (a resolvable type that is definitely not a resource fails
// loud, same as resourceCanonHostFunc), and only widens the "can't resolve
// structurally" case from a hard failure to a warning-free best effort.
func resourceCanonHostFuncGraph(comp *binary.Component, cfg *config, resources *handleTable, name string, canon binary.Canon) (hostFuncDef, error) {
	if td, err := comp.ResolveType(canon.TypeIdx); err == nil {
		if _, ok := td.(binary.ResourceDesc); !ok {
			return hostFuncDef{}, fmt.Errorf("component/instance: inline export %q: canon type %d is not a resource type (got %T)", name, canon.TypeIdx, td)
		}
	}
	typeIdx := effectiveResourceTypeIdx(comp, cfg, canon.TypeIdx)
	if cfg.resCanon != nil { // composition: tag by the global id siblings agree on
		typeIdx = cfg.resCanon(canon.TypeIdx)
	}

	switch canon.Kind {
	case 0x02: // resource.new: rep:i32 -> handle:i32
		fn := api.GoModuleFunc(func(_ context.Context, _ api.Module, stack []uint64) {
			rep := api.DecodeU32(stack[0])
			stack[0] = api.EncodeU32(resources.NewOwn(typeIdx, rep))
		})
		return hostFuncDef{fn: fn, params: []api.ValueType{api.ValueTypeI32}, results: []api.ValueType{api.ValueTypeI32}}, nil

	case 0x03, binary.CanonKindResourceDropAsync: // resource.drop (sync 0x03 / async 0x07): handle:i32 -> ()
		fn := api.GoModuleFunc(func(ctx context.Context, _ api.Module, stack []uint64) {
			h := api.DecodeU32(stack[0])
			// Own/borrow dispatch BEFORE any destructor lookup (Phase 3): a
			// borrow drop (handleTable.Drop's borrow arm) must never run the
			// resource's dtor -- only the own arm does. Read the rep before
			// dropping so the destructor (if any) can run against it -- an
			// importer dropping an own<R> it received runs the DEFINER's
			// dtor, registered on the shared table by global tag.
			//
			// resource.drop async (kind 0x07) decodes identically and is
			// executed synchronously here: the pinned reference's
			// canon_resource_drop has no async branch at all (it lifts the
			// dtor with async_=False unconditionally) -- see
			// docs/component-model-async-phase3-design.md §4.4.
			own, ownErr := resources.IsOwn(typeIdx, h)
			var dtor func(context.Context, uint32) error
			var rep uint32
			if ownErr == nil && own {
				dtor = resources.dtorFor(typeIdx)
				if dtor != nil {
					if r, err := resources.Rep(typeIdx, h); err == nil {
						rep = r
					}
				}
			}
			if err := resources.Drop(typeIdx, h); err != nil {
				panic(fmt.Errorf("component/instance: resource.drop (type %d): %w", typeIdx, err))
			}
			if dtor != nil {
				if err := dtor(ctx, rep); err != nil {
					panic(fmt.Errorf("component/instance: resource.drop (type %d): destructor: %w", typeIdx, err))
				}
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

// bindImportExportsGraph is bindImportExports's graph-engine counterpart: it
// binds every component-level export exactly the same way (a func export
// directly, an instance export -- the WIT-exports-an-interface shape --
// through its re-export shim), but resolves a core func index via
// coreFuncTarget instead of the flat (coreFuncAliases, numProducedCoreFuncs)
// partition simpler components rely on, since the graph engine's core func
// index space can genuinely interleave alias- and canon-produced entries.
func bindImportExportsGraph(comp *binary.Component, componentFunc func(uint32) (bool, int, aliasTarget, error), coreFuncTarget func(int) (api.Module, string, error), resolve abi.Resolver, abiCache *CompileCache, compInstances map[int]*Instance) (map[string]*boundExport, error) {
	exports := make(map[string]*boundExport, len(comp.Exports))
	for _, exp := range comp.Exports {
		switch exp.ExternType {
		case 0x01: // func
			be, err := bindFuncExportGraph(comp, exp.ExternIndex, componentFunc, coreFuncTarget, resolve, exp.Name, abiCache, compInstances)
			if err != nil {
				return nil, err
			}
			exports[exp.Name] = be

		case 0x05: // instance
			if err := bindInstanceExportGraph(comp, exp, componentFunc, coreFuncTarget, resolve, exports, abiCache); err != nil {
				return nil, err
			}

		case 0x03: // type: a component re-exporting its named types (rec-t,
			// var-t, ...) for the WIT interface. No runtime binding -- skip,
			// mirroring the type-import skip. Only func/instance are callable.
			continue

		default:
			return nil, fmt.Errorf("component/instance: export %q has extern kind %s (%#x); only func and instance exports are supported", exp.Name, api.ExternTypeName(exp.ExternType), exp.ExternType)
		}
	}
	return exports, nil
}

// bindFuncExportGraph is bindFuncExport's graph-engine counterpart -- see
// bindImportExportsGraph.
func bindFuncExportGraph(comp *binary.Component, funcIdx uint32, componentFunc func(uint32) (bool, int, aliasTarget, error), coreFuncTarget func(int) (api.Module, string, error), resolve abi.Resolver, diagName string, abiCache *CompileCache, compInstances map[int]*Instance) (*boundExport, error) {
	isLift, liftCanonIdx, at, err := componentFunc(funcIdx)
	if err != nil {
		return nil, fmt.Errorf("component/instance: export %q: %w", diagName, err)
	}
	if !isLift {
		// A func alias to a nested component instance we recursively
		// instantiated (the fused-adapter shape): re-expose that sub-Instance's
		// already-bound export directly. It stays valid because the sub-Instance
		// is held in subInstances and closed with this one.
		if sub, ok := compInstances[int(at.instIdx)]; ok {
			if be, ok := sub.exports[at.name]; ok {
				// An async export's runtime state (activeTask, exclusiveHeld,
				// ...) lives on sub, not on whatever Instance eventually
				// calls invoke() on this re-exported boundExport -- see
				// boundExport.home's doc. Copy (never mutate the shared be)
				// so sub's own exports map -- and any OTHER alias chain
				// reusing the same be -- is unaffected. Only set home when
				// it isn't already set: a MULTI-level re-export (sub's own
				// export was itself bound by re-exporting one of ITS
				// children) must keep pointing at the deepest true owner.
				if (be.asyncCallback || be.stackful) && be.home == nil {
					homeBE := *be
					homeBE.home = sub
					return &homeBE, nil
				}
				return be, nil
			}
			return nil, fmt.Errorf("component/instance: export %q: nested component instance %d has no export %q", diagName, at.instIdx, at.name)
		}
		return nil, fmt.Errorf("component/instance: export %q resolves to an imported func rather than a lift; only lifted funcs may be exported", diagName)
	}
	canon := comp.Canons[liftCanonIdx]
	// isAsyncLift is CanonOpt kind 0x06 (async): decoded fully since Phase 0,
	// but only the async+callback combination has a runtime this phase --
	// see the isAsyncLift branch below, after the core func/postReturn/
	// realloc options (identical for both shapes) are resolved.
	isAsyncLift := false
	for _, opt := range canon.Opts {
		if opt.Kind == 0x06 {
			isAsyncLift = true
			break
		}
	}
	td, err := comp.ResolveType(canon.TypeIdx)
	if err != nil {
		return nil, fmt.Errorf("component/instance: export %q lift references type %d: %w", diagName, canon.TypeIdx, err)
	}
	fd, ok := td.(binary.FuncDesc)
	if !ok {
		return nil, fmt.Errorf("component/instance: export %q lift type %d is not a func type (got %T)", diagName, canon.TypeIdx, td)
	}

	mod, name, err := coreFuncTarget(int(canon.CoreFuncIdx))
	if err != nil {
		return nil, fmt.Errorf("component/instance: export %q lifts %w", diagName, err)
	}

	postReturnName, err := resolvePostReturnFuncGraph(canon, coreFuncTarget, mod)
	if err != nil {
		return nil, fmt.Errorf("component/instance: export %q: %w", diagName, err)
	}

	reallocName, err := resolveReallocFuncGraph(canon, coreFuncTarget, mod)
	if err != nil {
		return nil, fmt.Errorf("component/instance: export %q: %w", diagName, err)
	}

	callbackName, err := resolveCallbackFuncGraph(canon, coreFuncTarget, mod)
	if err != nil {
		return nil, fmt.Errorf("component/instance: export %q: %w", diagName, err)
	}

	be := &boundExport{mod: mod, funcName: name, fd: fd, postReturnFuncName: postReturnName, reallocFuncName: reallocName}
	switch {
	case isAsyncLift && callbackName != "":
		be.asyncCallback = true
		be.callbackFuncName = callbackName
	case isAsyncLift && callbackName == "":
		// Async without a callback is the STACKFUL lift's async-no-callback
		// sub-shape (docs/component-model-async-stackful-design.md §0/§9):
		// the core func returns nothing, and results flow through the
		// task.return builtin -- routed through invokeStackful/
		// startStackfulExportTask instead of the plain sync path.
		be.stackful, be.stackfulAsyncOpts = true, true
	case !isAsyncLift && fd.Async:
		// A sync (no `async` canon opt) lift of an async-TYPED func is the
		// STACKFUL lift's sync-opts sub-shape: results are lifted straight
		// from the flat core results and the runtime calls task.return_
		// itself (~2158-2159) -- every stackful conformance suite exercises
		// this shape.
		be.stackful = true
	}
	finalizeBoundExport(be, resolve, abiCache, comp, funcIdx)
	return be, nil
}

// resolvePostReturnFuncGraph is resolvePostReturnFunc's graph-engine
// counterpart, preserving the same cross-instance safety check (a
// post-return func must target the same core instance/module as the lift's
// own core func) via api.Module identity rather than an instance index
// comparison.
func resolvePostReturnFuncGraph(canon binary.Canon, coreFuncTarget func(int) (api.Module, string, error), liftMod api.Module) (string, error) {
	for _, opt := range canon.Opts {
		if opt.Kind != 0x05 { // post-return
			continue
		}
		mod, name, err := coreFuncTarget(int(opt.Idx))
		if err != nil {
			return "", fmt.Errorf("post-return %w", err)
		}
		if mod != liftMod {
			return "", fmt.Errorf("post-return core func targets a different core instance than the lift's own core func; cross-instance post-return is not supported")
		}
		return name, nil
	}
	return "", nil
}

// resolveReallocFuncGraph resolves the canon lift's realloc option (CanonOpt
// kind 0x04) to its core export name, mirroring resolvePostReturnFuncGraph. It
// requires the realloc func to live on the same core instance as the lift's
// own core func (the boundExport lowers params against that one module's
// memory). Returns "" when the lift declares no realloc option.
func resolveReallocFuncGraph(canon binary.Canon, coreFuncTarget func(int) (api.Module, string, error), liftMod api.Module) (string, error) {
	for _, opt := range canon.Opts {
		if opt.Kind != 0x04 { // realloc
			continue
		}
		mod, name, err := coreFuncTarget(int(opt.Idx))
		if err != nil {
			return "", fmt.Errorf("realloc %w", err)
		}
		if mod != liftMod {
			return "", fmt.Errorf("realloc core func targets a different core instance than the lift's own core func; cross-instance realloc is not supported")
		}
		return name, nil
	}
	return "", nil
}

// resolveCallbackFuncGraph resolves an async lift's callback option
// (CanonOpt kind 0x07) to its core export name, mirroring
// resolvePostReturnFuncGraph/resolveReallocFuncGraph exactly (same
// same-core-instance requirement -- invokeAsyncCallback calls be.callbackFn
// on be.mod, so a callback targeting a different core instance would need
// cross-instance calling this package doesn't support). Returns "" when the
// lift declares no callback option (a stackful async lift, or a sync lift).
func resolveCallbackFuncGraph(canon binary.Canon, coreFuncTarget func(int) (api.Module, string, error), liftMod api.Module) (string, error) {
	for _, opt := range canon.Opts {
		if opt.Kind != 0x07 { // callback
			continue
		}
		mod, name, err := coreFuncTarget(int(opt.Idx))
		if err != nil {
			return "", fmt.Errorf("callback %w", err)
		}
		if mod != liftMod {
			return "", fmt.Errorf("callback core func targets a different core instance than the lift's own core func; cross-instance callback is not supported")
		}
		return name, nil
	}
	return "", nil
}

// bindInstanceExportGraph is bindInstanceExport's graph-engine counterpart --
// identical resolution of the re-export-shim shape (see host_import.go's
// doc), but calling bindFuncExportGraph for each member.
func bindInstanceExportGraph(comp *binary.Component, exp binary.Export, componentFunc func(uint32) (bool, int, aliasTarget, error), coreFuncTarget func(int) (api.Module, string, error), resolve abi.Resolver, exports map[string]*boundExport, abiCache *CompileCache) error {
	// The component-level "instance" sort index space is every imported
	// instance (comp.Imports with ExternType == 0x05), in import order,
	// followed by every locally-instantiated one (comp.Instances) -- unlike
	// bindInstanceExport's fixtures (which never mix the two), a component
	// like real_hello that both imports instances and instantiates its own
	// nested re-export shim needs this offset to land on the right entry.
	numImportedInstances := 0
	for _, im := range comp.Imports {
		if im.ExternType == 0x05 {
			numImportedInstances++
		}
	}
	localIdx := int(exp.ExternIndex) - numImportedInstances
	if localIdx < 0 {
		return fmt.Errorf("component/instance: export %q references instance %d, which is an imported instance re-exported directly; unsupported", exp.Name, exp.ExternIndex)
	}
	if localIdx >= len(comp.Instances) {
		return fmt.Errorf("component/instance: export %q references instance %d, out of range of %d imported + %d locally-instantiated instance(s)", exp.Name, exp.ExternIndex, numImportedInstances, len(comp.Instances))
	}
	inst := comp.Instances[localIdx]
	if inst.Kind == 0x01 { // inline exports: no nested component/shim at all
		// The WIT-tooling pattern this exercises (big-interleaving-test.wast's
		// `(instance $types (export "event-kind" (type $driver "event-kind"))
		// ...)`) is a synthetic instance built purely by re-listing existing
		// sort entries -- almost always types, re-exported for external
		// binding-generator consumption, never invoked by any wasm code. Bind
		// only the func members (mirroring bindImportExportsGraph's own
		// ExternType==0x01/0x03 split just above); every other sort (type,
		// value, nested instance, component) is non-callable metadata with
		// nothing to bind, exactly like a plain top-level type re-export.
		for _, member := range inst.Exports {
			if member.Sort != 0x01 { // func
				continue
			}
			diagName := instanceExportKey(exp.Name, member.Name)
			// compInstances nil: same best-effort scope as the Kind==0x00 shim
			// path below (its own doc: "never nested-instance aliases") -- an
			// inline-export instance's func member aliasing a NESTED
			// instance's export (rather than this component's own canon
			// lift) is unexercised by any suite and out of scope here too.
			be, err := bindFuncExportGraph(comp, member.SortIdx, componentFunc, coreFuncTarget, resolve, diagName, abiCache, nil)
			if err != nil {
				return err
			}
			exports[diagName] = be
		}
		return nil
	}
	if inst.Kind != 0x00 {
		return fmt.Errorf("component/instance: export %q instance %d is not a component instantiation (kind %#x); inline-export instances are not supported", exp.Name, exp.ExternIndex, inst.Kind)
	}
	if int(inst.ComponentIdx) >= len(comp.NestedComponents) {
		return fmt.Errorf("component/instance: export %q instance %d references nested component %d, out of range of %d decoded nested component(s)", exp.Name, exp.ExternIndex, inst.ComponentIdx, len(comp.NestedComponents))
	}
	nested := comp.NestedComponents[inst.ComponentIdx]
	if err := validateShimComponent(nested); err != nil {
		return fmt.Errorf("component/instance: export %q: nested component %d: %w; a more complex nested component is out of scope for this milestone", exp.Name, inst.ComponentIdx, err)
	}

	argByName := make(map[string]binary.InstantiateArg, len(inst.Args))
	for _, a := range inst.Args {
		argByName[a.Name] = a
	}
	shimFuncImports := shimFuncImportNames(nested)

	for _, member := range nested.Exports {
		diagName := instanceExportKey(exp.Name, member.Name)
		if member.ExternType != 0x01 { // func
			// A non-func member of an exported interface is a type/value/
			// instance re-export -- e.g. wasi:http/incoming-handler re-exports
			// the incoming-request and response-outparam resource types it
			// `use`s from wasi:http/types. These are non-callable metadata, so
			// skip them; only func members become boundExports.
			continue
		}
		if int(member.ExternIndex) >= len(shimFuncImports) {
			return fmt.Errorf("component/instance: %s: func index %d out of range of the shim's %d func import(s)", diagName, member.ExternIndex, len(shimFuncImports))
		}
		importName := shimFuncImports[member.ExternIndex]
		arg, ok := argByName[importName]
		if !ok {
			return fmt.Errorf("component/instance: %s: shim import %q has no matching instantiate-arg", diagName, importName)
		}
		if arg.Sort != 0x01 { // func
			return fmt.Errorf("component/instance: %s: instantiate-arg %q has non-func sort %#x", diagName, importName, arg.Sort)
		}

		// A shim's instantiate-arg funcs are this component's own canon lifts,
		// never nested-instance aliases, so no compInstances are needed here.
		be, err := bindFuncExportGraph(comp, arg.SortIdx, componentFunc, coreFuncTarget, resolve, diagName, abiCache, nil)
		if err != nil {
			return err
		}
		exports[diagName] = be
	}
	return nil
}
