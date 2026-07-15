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
	var liftCanonIdxs, lowerCanonIdxs []int
	for i, cn := range comp.Canons {
		switch cn.Kind {
		case 0x00: // lift
			liftCanonIdxs = append(liftCanonIdxs, i)
		case 0x01: // lower
			lowerCanonIdxs = append(lowerCanonIdxs, i)
		default:
			return nil, fmt.Errorf("component/instance: canon[%d] has kind %#x; only canon lift (0x00) and lower (0x01) are supported", i, cn.Kind)
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

	numLowers := len(lowerCanonIdxs)

	// Instantiate core instances in order. FromExports instances (which group
	// lowered import funcs) become synthetic host modules; instantiate
	// instances become real guest modules whose imports resolve, by name,
	// against the earlier-registered modules.
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

		case 0x01: // inline exports: a host module grouping lowered import funcs
			names := refNames[uint32(k)]
			if len(names) != 1 {
				return fail(fmt.Errorf("component/instance: inline-export core instance %d is referenced under %d name(s); exactly 1 is supported by this milestone", k, len(names)))
			}
			hostModName := names[0]
			b := r.NewHostModuleBuilder(hostModName)
			for _, e := range ci.Exports {
				fnDef, err := resolveLoweredImport(comp, cfg, e, lowerCanonIdxs, componentFunc)
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

	exports, err := bindImportExports(comp, componentFunc, coreFuncAliases, instMods, numLowers)
	if err != nil {
		return fail(err)
	}

	return &Instance{resolve: resolve, exports: exports, closers: closers}, nil
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

// resolveLoweredImport resolves one inline export (grouping a lowered import
// func) all the way to the caller's HostFunc, and builds the wazy adapter.
func resolveLoweredImport(comp *binary.Component, cfg *config, e binary.CoreInlineExport, lowerCanonIdxs []int, componentFunc func(uint32) (bool, int, aliasTarget, error)) (hostFuncDef, error) {
	if e.Sort != 0x00 { // core func
		return hostFuncDef{}, fmt.Errorf("component/instance: inline export %q has core sort %#x; only core funcs (0x00) may be grouped this way", e.Name, e.Sort)
	}
	if int(e.CoreSortIdx) >= len(lowerCanonIdxs) {
		return hostFuncDef{}, fmt.Errorf("component/instance: inline export %q references core func %d, which is not one of the %d lowered import funcs; only lowered funcs may be grouped this way", e.Name, e.CoreSortIdx, len(lowerCanonIdxs))
	}
	lowerCanon := comp.Canons[lowerCanonIdxs[e.CoreSortIdx]]

	isLift, _, at, err := componentFunc(lowerCanon.FuncIdx)
	if err != nil {
		return hostFuncDef{}, fmt.Errorf("component/instance: inline export %q: %w", e.Name, err)
	}
	if isLift {
		return hostFuncDef{}, fmt.Errorf("component/instance: inline export %q lowers a lifted (exported) func rather than an import; unsupported by this milestone", e.Name)
	}

	iface, err := importInterfaceName(comp, at.instIdx)
	if err != nil {
		return hostFuncDef{}, fmt.Errorf("component/instance: inline export %q: %w", e.Name, err)
	}
	hi, ok := cfg.imports[importKey{iface: iface, name: at.name}]
	if !ok {
		return hostFuncDef{}, fmt.Errorf("component/instance: no host implementation provided for import %q func %q (use WithImport)", iface, at.name)
	}

	fn, params, results, err := buildHostWrapper(iface, at.name, hi)
	if err != nil {
		return hostFuncDef{}, err
	}
	return hostFuncDef{fn: fn, params: params, results: results}, nil
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
func bindImportExports(comp *binary.Component, componentFunc func(uint32) (bool, int, aliasTarget, error), coreFuncAliases []aliasTarget, instMods map[int]api.Module, numLowers int) (map[string]*boundExport, error) {
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
		if cfi < numLowers {
			return nil, fmt.Errorf("component/instance: export %q lifts core func %d, which is a lowered import func rather than a real core export; unsupported", exp.Name, cfi)
		}
		ai := cfi - numLowers
		if ai >= len(coreFuncAliases) {
			return nil, fmt.Errorf("component/instance: export %q lifts core func %d, out of range of the core func index space (%d lowers + %d aliases)", exp.Name, cfi, numLowers, len(coreFuncAliases))
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
// (reading strings/lists from the calling module's memory), calls the
// HostFunc, and lowers the results back into the core return slots.
func buildHostWrapper(iface, funcName string, hi *hostImport) (api.GoModuleFunction, []api.ValueType, []api.ValueType, error) {
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
		args, err := liftHostArgs(fd, resolve, stack, mod)
		if err != nil {
			panic(fmt.Errorf("component/instance: host import %q %q: %w", iface, funcName, err))
		}
		results, err := hi.fn(ctx, args)
		if err != nil {
			panic(fmt.Errorf("component/instance: host import %q %q: %w", iface, funcName, err))
		}
		if err := lowerHostResults(ctx, fd, resolve, results, stack, mod); err != nil {
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
// from the calling module's memory.
func liftHostArgs(fd binary.FuncDesc, resolve abi.Resolver, stack []uint64, mod api.Module) ([]abi.Value, error) {
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
		args[i] = v
		pos += len(flat)
	}
	return args, nil
}

// lowerHostResults lowers the HostFunc's result values back into the core
// return slots at the front of the stack, per fd's result types.
func lowerHostResults(ctx context.Context, fd binary.FuncDesc, resolve abi.Resolver, results []abi.Value, stack []uint64, mod api.Module) error {
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
	realloc := reallocOf(ctx, mod)
	flat, err := abi.LowerFlat(results[0], rt, resolve, realloc, mem)
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
