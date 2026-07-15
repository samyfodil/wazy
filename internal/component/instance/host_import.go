package instance

import (
	"context"
	"fmt"

	"github.com/samyfodil/wazy"
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
}

type importKey struct {
	iface string
	name  string
}

// hostImport is a single registered import: its Go implementation plus the
// WIT parameter and result types the caller declared for it. The types are
// supplied by the caller because the binary decoder does not retain the func
// signatures declared inside an imported instance type (see the package doc).
type hostImport struct {
	fn      HostFunc
	params  []binary.TypeDesc
	results []binary.TypeDesc
}

func newConfig(opts []Option) *config {
	c := &config{imports: make(map[importKey]*hostImport)}
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
		c.imports[importKey{iface: iface, name: name}] = &hostImport{fn: fn, params: params, results: results}
	}
}

// aliasTarget is a resolved (instance index, export name) pair naming what an
// alias brings into scope.
type aliasTarget struct {
	instIdx uint32
	name    string
}

// instantiateWithImports handles the host-import (M3 STEP 3) shape: a
// component that imports host functions, lowers them into core functions the
// guest core module imports and calls, and exports functions via canon lift.
// See the package doc for the supported topology and the decoder gaps this
// works around.
func instantiateWithImports(ctx context.Context, r wazy.Runtime, comp *binary.Component, componentBytes []byte, cfg *config) (*Instance, error) {
	if len(comp.Instances) > 0 {
		return nil, fmt.Errorf("component/instance: component declares %d nested component instance(s); not supported by this milestone", len(comp.Instances))
	}
	for _, im := range comp.Imports {
		if im.ExternType != 0x05 { // instance
			return nil, fmt.Errorf("component/instance: import %q has extern kind %s (%#x); only instance imports are supported", im.Name, api.ExternTypeName(im.ExternType), im.ExternType)
		}
	}

	resolve := typeResolver(comp)

	// Component func index space: component-func aliases (into imported
	// instances) occupy the low indices, followed by lifted funcs. See the
	// package doc for the ordering assumption.
	var compFuncAliases []aliasTarget
	for _, al := range comp.Aliases {
		if al.Sort == 0x01 && al.TargetKind == 0x00 { // func-sort alias of an instance export
			compFuncAliases = append(compFuncAliases, aliasTarget{instIdx: al.InstanceIdx, name: al.Name})
		}
	}
	// coreFuncCanonIdxs holds every canon that produces a NEW core func
	// (lower, plus the three resource canons); liftCanonIdxs holds every
	// canon that instead produces a component-level func for export. Only
	// these five kinds are supported.
	var liftCanonIdxs, coreFuncCanonIdxs []int
	for i, cn := range comp.Canons {
		switch cn.Kind {
		case 0x00: // lift
			liftCanonIdxs = append(liftCanonIdxs, i)
		case 0x01, 0x02, 0x03, 0x04: // lower, resource.new, resource.drop, resource.rep
			coreFuncCanonIdxs = append(coreFuncCanonIdxs, i)
		default:
			return nil, fmt.Errorf("component/instance: canon[%d] has kind %#x; only canon lift (0x00), lower (0x01), resource.new (0x02), resource.drop (0x03), and resource.rep (0x04) are supported", i, cn.Kind)
		}
	}

	// componentFunc maps a component func index to either a lift canon index
	// (isLift) or an imported-func alias target.
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

	// Names each core instance is referenced under, from instantiate args.
	refNames := make(map[uint32][]string)
	for _, ci := range comp.CoreInstances {
		if ci.Kind == 0x00 {
			for _, arg := range ci.Args {
				refNames[arg.InstanceIdx] = append(refNames[arg.InstanceIdx], arg.Name)
			}
		}
	}

	instMods := make(map[int]api.Module)
	instIsHost := make(map[int]bool)
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

	numProducedCoreFuncs := len(coreFuncCanonIdxs)
	resources := newHandleTable()

	// Instantiate core instances in order. FromExports instances (which group
	// lowered import funcs and/or resource canon funcs) become synthetic
	// host modules; instantiate instances become real guest modules whose
	// imports resolve, by name, against the earlier-registered modules.
	for k, ci := range comp.CoreInstances {
		switch ci.Kind {
		case 0x00: // instantiate a core module
			if int(ci.ModuleIdx) >= len(comp.CoreModules) {
				return fail(fmt.Errorf("component/instance: core instance %d references core module %d, out of range of %d modules", k, ci.ModuleIdx, len(comp.CoreModules)))
			}
			coreBytes, err := coreModuleBytes(comp.CoreModules[ci.ModuleIdx], componentBytes)
			if err != nil {
				return fail(err)
			}
			name, err := moduleNameFor(k, refNames[uint32(k)])
			if err != nil {
				return fail(err)
			}
			mod, err := r.InstantiateWithConfig(ctx, coreBytes, wazy.NewModuleConfig().WithName(name))
			if err != nil {
				return fail(fmt.Errorf("component/instance: instantiate core module %d as %q: %w", ci.ModuleIdx, name, err))
			}
			instMods[k] = mod
			closers = append(closers, mod)

		case 0x01: // inline exports: a host module grouping lowered import / resource canon funcs
			names := refNames[uint32(k)]
			if len(names) != 1 {
				return fail(fmt.Errorf("component/instance: inline-export core instance %d is referenced under %d name(s); exactly 1 is supported by this milestone", k, len(names)))
			}
			hostModName := names[0]
			b := r.NewHostModuleBuilder(hostModName)
			for _, e := range ci.Exports {
				fnDef, err := resolveCoreFuncCanon(comp, cfg, resources, e, coreFuncCanonIdxs, componentFunc)
				if err != nil {
					return fail(err)
				}
				b = b.NewFunctionBuilder().WithGoModuleFunction(fnDef.fn, fnDef.params, fnDef.results).Export(e.Name)
			}
			hostMod, err := b.Instantiate(ctx)
			if err != nil {
				return fail(fmt.Errorf("component/instance: instantiate host module %q: %w", hostModName, err))
			}
			instMods[k] = hostMod
			instIsHost[k] = true
			closers = append(closers, hostMod)

		default:
			return fail(fmt.Errorf("component/instance: core instance %d has unknown kind %#x", k, ci.Kind))
		}
	}

	// Core func index space: lowered funcs occupy [0, numLowers); core-func
	// aliases follow. Core-export aliases are classified as func vs
	// memory/table/global by probing the instantiated target module, since
	// the decoder does not retain the alias's core:sort discriminator.
	var coreFuncAliases []aliasTarget
	for i, al := range comp.Aliases {
		if al.Sort != 0x00 || al.TargetKind != 0x01 {
			continue // not a core-export alias
		}
		mod, ok := instMods[int(al.InstanceIdx)]
		if !ok {
			return fail(fmt.Errorf("component/instance: alias[%d] references core instance %d, which was not instantiated", i, al.InstanceIdx))
		}
		if instIsHost[int(al.InstanceIdx)] {
			// Aliasing a func out of an inline-export (host) instance is not
			// supported; such aliases are not needed for the supported shape.
			continue
		}
		if mod.ExportedFunction(al.Name) != nil {
			coreFuncAliases = append(coreFuncAliases, aliasTarget{instIdx: al.InstanceIdx, name: al.Name})
		}
	}

	exports, err := bindImportExports(comp, componentFunc, coreFuncAliases, instMods, numProducedCoreFuncs)
	if err != nil {
		return fail(err)
	}

	return &Instance{resolve: resolve, exports: exports, closers: closers, resources: resources}, nil
}

// moduleNameFor picks the wazy module name to register a real core instance
// under. An instance referenced by exactly one instantiate-arg name takes
// that name (so a dependent module's by-name import resolves); an unreferenced
// instance (the root) gets a synthesized unique name.
func moduleNameFor(coreInstanceIdx int, refNames []string) (string, error) {
	switch len(refNames) {
	case 0:
		return fmt.Sprintf("wazy:component/core%d", coreInstanceIdx), nil
	case 1:
		return refNames[0], nil
	default:
		return "", fmt.Errorf("component/instance: core instance %d is referenced under %d names (%v); a core module can only be registered under one name", coreInstanceIdx, len(refNames), refNames)
	}
}

// hostFuncDef is a built host function: its wazy adapter plus the core param
// and result value types it declares (which must match the guest's import).
type hostFuncDef struct {
	fn      api.GoModuleFunction
	params  []api.ValueType
	results []api.ValueType
}

// resolveCoreFuncCanon resolves one inline export -- which groups a canon
// that produces a core func (lower, or one of the three resource canons) --
// all the way to its wazy host-module adapter.
func resolveCoreFuncCanon(comp *binary.Component, cfg *config, resources *handleTable, e binary.CoreInlineExport, coreFuncCanonIdxs []int, componentFunc func(uint32) (bool, int, aliasTarget, error)) (hostFuncDef, error) {
	if e.Sort != 0x00 { // core func
		return hostFuncDef{}, fmt.Errorf("component/instance: inline export %q has core sort %#x; only core funcs (0x00) may be grouped this way", e.Name, e.Sort)
	}
	if int(e.CoreSortIdx) >= len(coreFuncCanonIdxs) {
		return hostFuncDef{}, fmt.Errorf("component/instance: inline export %q references core func %d, which is not one of the %d canon-produced core funcs (lower/resource.new/resource.drop/resource.rep); only those may be grouped this way", e.Name, e.CoreSortIdx, len(coreFuncCanonIdxs))
	}
	canon := comp.Canons[coreFuncCanonIdxs[e.CoreSortIdx]]

	switch canon.Kind {
	case 0x01: // lower
		return resolveLoweredImport(comp, cfg, resources, e.Name, canon, componentFunc)
	case 0x02, 0x03, 0x04: // resource.new, resource.drop, resource.rep
		return resourceCanonHostFunc(comp, resources, e.Name, canon)
	default:
		return hostFuncDef{}, fmt.Errorf("component/instance: inline export %q references canon[%d] of kind %#x, which does not produce a core func", e.Name, coreFuncCanonIdxs[e.CoreSortIdx], canon.Kind)
	}
}

// resolveLoweredImport resolves a canon lower to the caller's HostFunc, and
// builds the wazy adapter.
func resolveLoweredImport(comp *binary.Component, cfg *config, resources *handleTable, name string, lowerCanon binary.Canon, componentFunc func(uint32) (bool, int, aliasTarget, error)) (hostFuncDef, error) {
	isLift, _, at, err := componentFunc(lowerCanon.FuncIdx)
	if err != nil {
		return hostFuncDef{}, fmt.Errorf("component/instance: inline export %q: %w", name, err)
	}
	if isLift {
		return hostFuncDef{}, fmt.Errorf("component/instance: inline export %q lowers a lifted (exported) func rather than an import; unsupported by this milestone", name)
	}

	iface, err := importInterfaceName(comp, at.instIdx)
	if err != nil {
		return hostFuncDef{}, fmt.Errorf("component/instance: inline export %q: %w", name, err)
	}
	hi, ok := cfg.imports[importKey{iface: iface, name: at.name}]
	if !ok {
		return hostFuncDef{}, fmt.Errorf("component/instance: no host implementation provided for import %q func %q (use WithImport)", iface, at.name)
	}

	fn, params, results, err := buildHostWrapper(iface, at.name, hi, resources)
	if err != nil {
		return hostFuncDef{}, err
	}
	return hostFuncDef{fn: fn, params: params, results: results}, nil
}

// resourceCanonHostFunc builds the fixed-signature host func for a
// resource.new / resource.drop / resource.rep canon, operating on resources.
// Per the Canonical ABI these three core funcs always take/return plain i32
// handles/reps -- there is no FuncDesc/WIT type involved at this layer (own
// and borrow only appear at the *component* level, in the func types of
// exports/imports that use these canon-produced core funcs) -- so this
// bypasses the abi package entirely and talks to the handle table directly.
func resourceCanonHostFunc(comp *binary.Component, resources *handleTable, name string, canon binary.Canon) (hostFuncDef, error) {
	if int(canon.TypeIdx) >= len(comp.Types) {
		return hostFuncDef{}, fmt.Errorf("component/instance: inline export %q: canon references type %d, out of range of %d types", name, canon.TypeIdx, len(comp.Types))
	}
	if _, ok := comp.Types[canon.TypeIdx].Descriptor.(binary.ResourceDesc); !ok {
		return hostFuncDef{}, fmt.Errorf("component/instance: inline export %q: canon type %d is not a resource type (got %T)", name, canon.TypeIdx, comp.Types[canon.TypeIdx].Descriptor)
	}
	typeIdx := canon.TypeIdx

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

// bindImportExports binds each component export to the core function that
// implements it (via its lift and the core func index space).
func bindImportExports(comp *binary.Component, componentFunc func(uint32) (bool, int, aliasTarget, error), coreFuncAliases []aliasTarget, instMods map[int]api.Module, numProducedCoreFuncs int) (map[string]*boundExport, error) {
	exports := make(map[string]*boundExport, len(comp.Exports))
	for _, exp := range comp.Exports {
		if exp.ExternType != 0x01 { // func
			return nil, fmt.Errorf("component/instance: export %q has extern kind %s (%#x); only func exports are supported", exp.Name, api.ExternTypeName(exp.ExternType), exp.ExternType)
		}
		isLift, liftCanonIdx, _, err := componentFunc(exp.ExternIndex)
		if err != nil {
			return nil, fmt.Errorf("component/instance: export %q: %w", exp.Name, err)
		}
		if !isLift {
			return nil, fmt.Errorf("component/instance: export %q resolves to an imported func rather than a lift; only lifted funcs may be exported", exp.Name)
		}
		canon := comp.Canons[liftCanonIdx]
		if int(canon.TypeIdx) >= len(comp.Types) {
			return nil, fmt.Errorf("component/instance: export %q lift references type %d, out of range of %d types", exp.Name, canon.TypeIdx, len(comp.Types))
		}
		fd, ok := comp.Types[canon.TypeIdx].Descriptor.(binary.FuncDesc)
		if !ok {
			return nil, fmt.Errorf("component/instance: export %q lift type %d is not a func type (got %T)", exp.Name, canon.TypeIdx, comp.Types[canon.TypeIdx].Descriptor)
		}

		cfi := int(canon.CoreFuncIdx)
		if cfi < numProducedCoreFuncs {
			return nil, fmt.Errorf("component/instance: export %q lifts core func %d, which is a canon-produced func (lower/resource.*) rather than a real core export; unsupported", exp.Name, cfi)
		}
		ai := cfi - numProducedCoreFuncs
		if ai >= len(coreFuncAliases) {
			return nil, fmt.Errorf("component/instance: export %q lifts core func %d, out of range of the core func index space (%d canon-produced funcs + %d aliases)", exp.Name, cfi, numProducedCoreFuncs, len(coreFuncAliases))
		}
		tgt := coreFuncAliases[ai]
		mod, ok := instMods[int(tgt.instIdx)]
		if !ok {
			return nil, fmt.Errorf("component/instance: export %q targets core instance %d, which was not instantiated", exp.Name, tgt.instIdx)
		}
		exports[exp.Name] = &boundExport{mod: mod, funcName: tgt.name, fd: fd}
	}
	return exports, nil
}

// buildHostWrapper builds the wazy GoModuleFunction that adapts a HostFunc to
// the guest's lowered core calling convention: it lifts the flat core args
// (reading strings/lists from the calling module's memory, and resolving
// own<T>/borrow<T> handles to their host rep via resources), calls the
// HostFunc, and lowers the results back into the core return slots (again
// allocating a fresh handle for any own<T>/borrow<T> result).
func buildHostWrapper(iface, funcName string, hi *hostImport, resources *handleTable) (api.GoModuleFunction, []api.ValueType, []api.ValueType, error) {
	fd, resolve := synthFuncDesc(hi.params, hi.results)

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
	if len(rawResults) > abi.MaxFlatResults {
		return nil, nil, nil, fmt.Errorf("component/instance: import %q func %q has %d flat results, exceeding the flat limit; spilled results are not supported by this milestone", iface, funcName, len(rawResults))
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
		args, err := liftHostArgs(fd, resolve, stack, mod, resources)
		if err != nil {
			panic(fmt.Errorf("component/instance: host import %q %q: %w", iface, funcName, err))
		}
		results, err := hi.fn(ctx, args)
		if err != nil {
			panic(fmt.Errorf("component/instance: host import %q %q: %w", iface, funcName, err))
		}
		if err := lowerHostResults(ctx, fd, resolve, results, stack, mod, resources); err != nil {
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
		v, err = resolveHandleArg(resources, pt, v)
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
func resolveHandleArg(resources *handleTable, pt binary.TypeDesc, v abi.Value) (abi.Value, error) {
	switch d := pt.(type) {
	case binary.OwnDesc:
		h, ok := v.(uint32)
		if !ok {
			return nil, fmt.Errorf("own<%d> arg: expected a uint32 handle, got %T", d.ResourceType, v)
		}
		rep, err := resources.TakeOwn(d.ResourceType, h)
		if err != nil {
			return nil, fmt.Errorf("own<%d> arg: %w", d.ResourceType, err)
		}
		return rep, nil
	case binary.BorrowDesc:
		h, ok := v.(uint32)
		if !ok {
			return nil, fmt.Errorf("borrow<%d> arg: expected a uint32 handle, got %T", d.ResourceType, v)
		}
		rep, err := resources.Rep(d.ResourceType, h)
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
// allocHandleResult.
func lowerHostResults(ctx context.Context, fd binary.FuncDesc, resolve abi.Resolver, results []abi.Value, stack []uint64, mod api.Module, resources *handleTable) error {
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
	realloc := reallocOf(ctx, mod)
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
