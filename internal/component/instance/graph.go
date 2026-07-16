package instance

import (
	"context"
	"fmt"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/experimental"
	"github.com/samyfodil/wazy/internal/component/abi"
	"github.com/samyfodil/wazy/internal/component/binary"
	"github.com/samyfodil/wazy/internal/expctxkeys"
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
//    exactly like instantiateWithImports -- this is unaffected by the
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
// The component-level (WIT interface) host-import path (instantiateWithImports
// in host_import.go) and the single-module path (instantiateComponent in
// instance.go) are both left completely unmodified: this file adds a new,
// independent routing branch (needsGraphPath) rather than folding into
// either, so every existing test and fixture is provably unaffected.

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
		if im.ExternType != 0x05 { // instance
			return nil, fmt.Errorf("component/instance: import %q has extern kind %s (%#x); only instance imports are supported", im.Name, api.ExternTypeName(im.ExternType), im.ExternType)
		}
	}
	if len(comp.CoreFuncSpace) == 0 && (len(comp.Aliases) > 0 || len(comp.Canons) > 0) {
		return nil, fmt.Errorf("component/instance: the graph engine requires a component decoded via binary.Decode (comp.CoreFuncSpace is empty, but aliases/canons are present); hand-built binary.Component values are not supported by this path")
	}

	resolve := typeResolver(comp)
	synthPrefix := nextSynthNamePrefix()

	// Component func index space: identical construction to
	// instantiateWithImports -- see that function's doc for why component
	// func aliases always precede lifts structurally, which this reuses
	// unchanged.
	var compFuncAliases []aliasTarget
	for _, al := range comp.Aliases {
		if al.Sort == 0x01 && al.TargetKind == 0x00 {
			compFuncAliases = append(compFuncAliases, aliasTarget{instIdx: al.InstanceIdx, name: al.Name})
		}
	}
	var liftCanonIdxs []int
	for i, cn := range comp.Canons {
		if cn.Kind == 0x00 {
			liftCanonIdxs = append(liftCanonIdxs, i)
		}
	}
	componentFunc := func(idx uint32) (isLift bool, liftCanonIdx int, at aliasTarget, err error) {
		if int(idx) < len(compFuncAliases) {
			return false, 0, compFuncAliases[idx], nil
		}
		li := int(idx) - len(compFuncAliases)
		if li < len(liftCanonIdxs) {
			return true, liftCanonIdxs[li], aliasTarget{}, nil
		}
		return false, 0, aliasTarget{}, fmt.Errorf("component func index %d out of range of the component func index space (%d aliases + %d lifts)", idx, len(compFuncAliases), len(liftCanonIdxs))
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

	neededTypes, err := discoverNeededFuncTypes(ctx, r, cfg, comp, componentBytes, emptyNameTarget)
	if err != nil {
		return nil, err
	}

	resources := newHandleTable()
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
	coreFuncTarget := func(cfi int) (api.Module, string, error) {
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
			return nil, "", fmt.Errorf("core func index %d is a canon-produced func (lower/resource.*) rather than a real core export; unsupported", cfi)
		default:
			return nil, "", fmt.Errorf("core func index %d: unknown core func space entry kind %d", cfi, entry.Kind)
		}
	}

	// coreMemTarget is coreFuncTarget's counterpart for the core MEMORY index
	// space -- used only by canonMemoryAndRealloc (via buildCanonHostModule)
	// to resolve a canon lower's own "memory" CanonOpt. Safe to define here
	// for the same forward-declaration reason as coreFuncTarget above.
	coreMemTarget := func(idx int) (api.Module, error) {
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
			coreBytes, err := coreModuleBytes(comp.CoreModules[ci.ModuleIdx], componentBytes)
			if err != nil {
				return fail(err)
			}
			// Rewrite the empty import module name (the decoder rejects it) to
			// the STABLE emptyNameTarget, which the resolver maps. Stable => the
			// rewritten bytes are identical every instantiation, so the compile
			// cache still hits.
			if emptyNameTarget != "" {
				coreBytes, _, err = rewriteEmptyImportModuleName(coreBytes, emptyNameTarget)
				if err != nil {
					return fail(fmt.Errorf("component/instance: core instance %d: %w", k, err))
				}
			}
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
			for _, e := range ci.Exports {
				// key doubles as groupName (the consumer-declared name) for
				// neededTypes lookups; keys names the shim's alias sources.
				item, wasiCall, privMod, err := resolveInlineExportItem(ctx, r, comp, cfg, resources, e, coreMemSpace, coreTableSpace, instMods, keys, neededTypes, key, nextPrivateName, coreFuncTarget, coreMemTarget)
				if err != nil {
					return fail(fmt.Errorf("component/instance: core instance %d: %w", k, err))
				}
				items = append(items, item)
				if wasiCall != "" {
					wasiCalls = append(wasiCalls, wasiCall)
				}
				if privMod != nil {
					// A canon-produced core func's own private host module
					// (see resolveInlineExportItem's doc): the shim below
					// only imports from it, so it must be tracked here too
					// or Instance.Close leaks it.
					closers = append(closers, privMod)
				}
			}
			shimBytes, err := buildPassthroughShim(items)
			if err != nil {
				return fail(fmt.Errorf("component/instance: core instance %d: %w", k, err))
			}
			// Anonymous like the embedded modules above; consumers import it by
			// its key via the resolver, not by a global name.
			mod, err := r.InstantiateWithConfig(instCtx, shimBytes, wazy.NewModuleConfig().WithName("").WithStartFunctions())
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

	exports, err := bindImportExportsGraph(comp, componentFunc, coreFuncTarget, resolve)
	if err != nil {
		return fail(err)
	}

	return &Instance{
		resolve:         resolve,
		exports:         exports,
		instanceExports: buildInstanceExportIndex(exports),
		closers:         closers,
		resources:       resources,
		coreModuleCount: coreModuleCount,
		wasiCalls:       wasiCalls,
		httpHost:        cfg.httpHost,
	}, nil
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

// discoverNeededFuncTypes pre-compiles every embedded core module (applying
// the empty-import-name rewrite first, since wazy's own decoder rejects an
// empty import module name outright) and records the core-level signature of
// every func it imports, keyed by (module name, field name). See this
// file's package doc (step 5) for why this is the only available source of
// truth for a lowered import's core-level type.
//
// When cfg carries a CompileCache, this probe compile goes through it too
// (same coreBytes as the real instantiation loop below uses, so it's either
// a genuine cache warm-up -- reused a few lines later -- or, on a repeat
// Instantiate of the same component, an outright cache hit) and the
// CompiledModule is left open, owned by the cache, instead of closed
// immediately. With no cache, behavior is unchanged: a throwaway compile,
// closed right after its ImportedFunctions() are read.
func discoverNeededFuncTypes(ctx context.Context, r wazy.Runtime, cfg *config, comp *binary.Component, componentBytes []byte, emptyNameTarget string) (map[string]map[string]coreFuncSig, error) {
	cached := cfg != nil && cfg.compileCache != nil
	out := make(map[string]map[string]coreFuncSig)
	for i, cm := range comp.CoreModules {
		coreBytes, err := coreModuleBytes(cm, componentBytes)
		if err != nil {
			return nil, err
		}
		rewritten := false
		if emptyNameTarget != "" {
			coreBytes, rewritten, err = rewriteEmptyImportModuleName(coreBytes, emptyNameTarget)
			if err != nil {
				return nil, fmt.Errorf("component/instance: core module %d: %w", i, err)
			}
		}
		// A rewritten module's bytes carry the per-instantiation emptyNameTarget,
		// so it must not go through the cache (it would never be reused -- see
		// instantiateCoreModuleCacheable); compile it as a throwaway.
		useCache := cached && !rewritten
		var compiled wazy.CompiledModule
		if useCache {
			compiled, err = cfg.compileCache.getOrCompile(ctx, r, coreBytes)
		} else {
			compiled, err = r.CompileModule(ctx, coreBytes)
		}
		if err != nil {
			return nil, fmt.Errorf("component/instance: core module %d: discover import types: %w", i, err)
		}
		for _, fd := range compiled.ImportedFunctions() {
			modName, fieldName, _ := fd.Import()
			if out[modName] == nil {
				out[modName] = make(map[string]coreFuncSig)
			}
			out[modName][fieldName] = coreFuncSig{params: fd.ParamTypes(), results: fd.ResultTypes()}
		}
		if !useCache {
			if err := compiled.Close(ctx); err != nil {
				return nil, fmt.Errorf("component/instance: core module %d: %w", i, err)
			}
		}
	}
	return out, nil
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
	ctx context.Context, r wazy.Runtime, comp *binary.Component, cfg *config, resources *handleTable,
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
			mod, exportName, params, results, wasiCall, err := buildCanonHostModule(ctx, r, comp, cfg, resources, canon, neededTypes, groupName, e.Name, nextPrivateName(), coreMemTarget, coreFuncTarget)
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
func buildCanonHostModule(
	ctx context.Context, r wazy.Runtime, comp *binary.Component, cfg *config, resources *handleTable,
	canon binary.Canon, neededTypes map[string]map[string]coreFuncSig, groupName, entryName, privateName string,
	coreMemTarget func(int) (api.Module, error), coreFuncTarget func(int) (api.Module, string, error),
) (mod api.Module, exportName string, params, results []api.ValueType, wasiCall string, err error) {
	var def hostFuncDef

	switch canon.Kind {
	case 0x01: // lower
		var compFuncAliases []aliasTarget
		for _, al := range comp.Aliases {
			if al.Sort == 0x01 && al.TargetKind == 0x00 {
				compFuncAliases = append(compFuncAliases, aliasTarget{instIdx: al.InstanceIdx, name: al.Name})
			}
		}
		var liftCanonIdxs []int
		for i, cn := range comp.Canons {
			if cn.Kind == 0x00 {
				liftCanonIdxs = append(liftCanonIdxs, i)
			}
		}
		componentFunc := func(idx uint32) (isLift bool, liftCanonIdx int, at aliasTarget, err error) {
			if int(idx) < len(compFuncAliases) {
				return false, 0, compFuncAliases[idx], nil
			}
			li := int(idx) - len(compFuncAliases)
			if li < len(liftCanonIdxs) {
				return true, liftCanonIdxs[li], aliasTarget{}, nil
			}
			return false, 0, aliasTarget{}, fmt.Errorf("component func index %d out of range of the component func index space (%d aliases + %d lifts)", idx, len(compFuncAliases), len(liftCanonIdxs))
		}

		isLift, _, at, ferr := componentFunc(canon.FuncIdx)
		if ferr != nil {
			return nil, "", nil, nil, "", ferr
		}
		if isLift {
			return nil, "", nil, nil, "", fmt.Errorf("lowers a lifted (exported) func rather than an import; unsupported")
		}
		iface, ierr := importInterfaceName(comp, at.instIdx)
		if ierr != nil {
			return nil, "", nil, nil, "", ierr
		}
		wasiCall = iface + "." + at.name

		if hi, ok := cfg.imports[mkImportKey(iface, at.name)]; ok {
			memMod, reallocFn, merr := canonMemoryAndRealloc(canon, coreMemTarget, coreFuncTarget)
			if merr != nil {
				return nil, "", nil, nil, "", fmt.Errorf("import %q func %q: %w", iface, at.name, merr)
			}
			fn, hiParams, hiResults, herr := buildHostWrapper(iface, at.name, hi, resources, memMod, reallocFn)
			if herr != nil {
				return nil, "", nil, nil, "", herr
			}
			def = hostFuncDef{fn: fn, params: hiParams, results: hiResults}
			wasiCall = "" // caller-provided, not a trap stub
		} else {
			sig, ok := neededTypes[groupName][entryName]
			if !ok {
				return nil, "", nil, nil, "", fmt.Errorf("cannot determine the core-level signature for lowered import %q %q: no consumer declares module %q field %q", iface, at.name, groupName, entryName)
			}
			trapIface, trapName := iface, at.name
			fn := api.GoModuleFunc(func(context.Context, api.Module, []uint64) {
				panic(fmt.Errorf("component/instance: WASI %s.%s not implemented (trap stub)", trapIface, trapName))
			})
			def = hostFuncDef{fn: fn, params: sig.params, results: sig.results}
		}

	case 0x02, 0x03, 0x04: // resource.new, resource.drop, resource.rep
		var rerr error
		def, rerr = resourceCanonHostFuncGraph(comp, cfg, resources, entryName, canon)
		if rerr != nil {
			return nil, "", nil, nil, "", rerr
		}

	default:
		return nil, "", nil, nil, "", fmt.Errorf("references a canon of kind %#x, which does not produce a core func", canon.Kind)
	}

	b := r.NewHostModuleBuilder(privateName)
	b = b.NewFunctionBuilder().WithGoModuleFunction(def.fn, def.params, def.results).Export("f")
	hostMod, berr := b.Instantiate(ctx)
	if berr != nil {
		return nil, "", nil, nil, "", fmt.Errorf("instantiate private host func module %q: %w", privateName, berr)
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
// instance" gap: unlike instantiateWithImports' fixtures (which only declare
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

// bindImportExportsGraph is bindImportExports's graph-engine counterpart: it
// binds every component-level export exactly the same way (a func export
// directly, an instance export -- the WIT-exports-an-interface shape --
// through its re-export shim), but resolves a core func index via
// coreFuncTarget instead of the flat (coreFuncAliases, numProducedCoreFuncs)
// partition instantiateWithImports uses, since the graph engine's core func
// index space can genuinely interleave alias- and canon-produced entries.
func bindImportExportsGraph(comp *binary.Component, componentFunc func(uint32) (bool, int, aliasTarget, error), coreFuncTarget func(int) (api.Module, string, error), resolve abi.Resolver) (map[string]*boundExport, error) {
	exports := make(map[string]*boundExport, len(comp.Exports))
	for _, exp := range comp.Exports {
		switch exp.ExternType {
		case 0x01: // func
			be, err := bindFuncExportGraph(comp, exp.ExternIndex, componentFunc, coreFuncTarget, resolve, exp.Name)
			if err != nil {
				return nil, err
			}
			exports[exp.Name] = be

		case 0x05: // instance
			if err := bindInstanceExportGraph(comp, exp, componentFunc, coreFuncTarget, resolve, exports); err != nil {
				return nil, err
			}

		default:
			return nil, fmt.Errorf("component/instance: export %q has extern kind %s (%#x); only func and instance exports are supported", exp.Name, api.ExternTypeName(exp.ExternType), exp.ExternType)
		}
	}
	return exports, nil
}

// bindFuncExportGraph is bindFuncExport's graph-engine counterpart -- see
// bindImportExportsGraph.
func bindFuncExportGraph(comp *binary.Component, funcIdx uint32, componentFunc func(uint32) (bool, int, aliasTarget, error), coreFuncTarget func(int) (api.Module, string, error), resolve abi.Resolver, diagName string) (*boundExport, error) {
	isLift, liftCanonIdx, _, err := componentFunc(funcIdx)
	if err != nil {
		return nil, fmt.Errorf("component/instance: export %q: %w", diagName, err)
	}
	if !isLift {
		return nil, fmt.Errorf("component/instance: export %q resolves to an imported func rather than a lift; only lifted funcs may be exported", diagName)
	}
	canon := comp.Canons[liftCanonIdx]
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

	be := &boundExport{mod: mod, funcName: name, fd: fd, postReturnFuncName: postReturnName}
	finalizeBoundExport(be, resolve)
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

// bindInstanceExportGraph is bindInstanceExport's graph-engine counterpart --
// identical resolution of the re-export-shim shape (see host_import.go's
// doc), but calling bindFuncExportGraph for each member.
func bindInstanceExportGraph(comp *binary.Component, exp binary.Export, componentFunc func(uint32) (bool, int, aliasTarget, error), coreFuncTarget func(int) (api.Module, string, error), resolve abi.Resolver, exports map[string]*boundExport) error {
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

		be, err := bindFuncExportGraph(comp, arg.SortIdx, componentFunc, coreFuncTarget, resolve, diagName)
		if err != nil {
			return err
		}
		exports[diagName] = be
	}
	return nil
}
