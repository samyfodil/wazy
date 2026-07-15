// Package instance instantiates a decoded WebAssembly component and exposes
// its exported functions for calling. This is the M3 STEP 2 "spine": a real
// component actually runs end to end, from raw component bytes through the
// embedded core module through the canon lift wiring to a lifted result.
//
// # Scope
//
// Instantiate and Call only support the shape needed to prove the spine
// works: a component with no imports, exactly one embedded core module,
// instantiated as the component's single core instance with no arguments,
// exporting functions defined purely via "canon lift" of a core function
// reached through a single core-export alias. Everything beyond that
// (component imports, multiple core modules/instances, canon lower, resource
// canonical built-ins, non-func exports, multi-result functions, and
// params/results that need linear memory when the core module exports none)
// fails loudly with a specific, named error instead of silently truncating
// or returning a zero value.
//
// # Known decoder limitation
//
// The component binary decoder (internal/component/binary, decodeAliasSection)
// parses past the one-byte core:sort discriminator on a core-sort alias
// (func/table/mem/global/type/module/instance) but does not retain it on
// binary.AliasDef. Because "canon lift" can only ever reference a core
// *function*, every core-sort/core-export alias this package resolves is
// therefore assumed to be a func alias. That assumption holds for every
// component this milestone can otherwise instantiate (no core memory/table/
// global aliases are reachable without imports or multiple core instances,
// both of which are already rejected above), but a future milestone that
// mixes core func aliases with core memory/table/global aliases in the same
// component will need AliasDef to retain the discriminator to disambiguate.
package instance

import (
	"bytes"
	"context"
	"fmt"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/component/abi"
	"github.com/samyfodil/wazy/internal/component/binary"
)

// Instance is an instantiated, no-import WebAssembly component: a single
// core module instance plus the wiring needed to call its component-level
// exported functions through their canon lift signatures.
type Instance struct {
	comp *binary.Component
	core api.Module

	// coreFuncIdx is the core func index space: coreFuncIdx[i] is the name
	// of the core-module export that core func index i refers to.
	coreFuncIdx []string

	// canonForExport maps a component export name to the index into
	// comp.Canons that defines it (always a canon lift; see package docs).
	canonForExport map[string]int

	resolve abi.Resolver
}

// Instantiate decodes componentBytes as a WebAssembly component, instantiates
// its single embedded core module into r, and wires up the export -> canon
// lift -> core func mapping needed to call exported functions via Call.
//
// See the package doc for exactly what shape of component is supported;
// anything outside that shape is rejected with a descriptive error rather
// than silently mishandled.
func Instantiate(ctx context.Context, r wazy.Runtime, componentBytes []byte) (*Instance, error) {
	comp, err := binary.Decode(bytes.NewReader(componentBytes))
	if err != nil {
		return nil, fmt.Errorf("component/instance: decode component: %w", err)
	}
	return instantiateComponent(ctx, r, comp, componentBytes)
}

// instantiateComponent does the validation and wiring described in the
// package doc, given an already-decoded Component. It is split out from
// Instantiate so tests can exercise every validation branch directly against
// a hand-built binary.Component, without needing binary.Decode to produce
// each specific (often hard-to-construct) shape from real wasm bytes.
func instantiateComponent(ctx context.Context, r wazy.Runtime, comp *binary.Component, componentBytes []byte) (*Instance, error) {
	if len(comp.Imports) > 0 {
		return nil, fmt.Errorf("component/instance: component declares %d import(s); components with imports are not supported by this milestone", len(comp.Imports))
	}
	if len(comp.Instances) > 0 {
		return nil, fmt.Errorf("component/instance: component declares %d nested component instance(s); not supported by this milestone", len(comp.Instances))
	}
	if len(comp.CoreModules) != 1 {
		return nil, fmt.Errorf("component/instance: expected exactly 1 embedded core module, found %d; multiple core modules are not supported by this milestone", len(comp.CoreModules))
	}
	if len(comp.CoreInstances) != 1 {
		return nil, fmt.Errorf("component/instance: expected exactly 1 core instance, found %d; multiple core instances are not supported by this milestone", len(comp.CoreInstances))
	}

	ci := comp.CoreInstances[0]
	if ci.Kind != 0x00 {
		return nil, fmt.Errorf("component/instance: core instance is not a module instantiation (kind %#x); inline-export core instances are not supported by this milestone", ci.Kind)
	}
	if int(ci.ModuleIdx) != 0 {
		return nil, fmt.Errorf("component/instance: core instance references core module index %d, expected the sole core module (index 0)", ci.ModuleIdx)
	}
	if len(ci.Args) > 0 {
		return nil, fmt.Errorf("component/instance: core module instantiation takes %d argument(s); core modules with their own imports are not supported by this milestone", len(ci.Args))
	}

	cm := comp.CoreModules[0]
	if cm.Offset < 0 || cm.Size < 0 || cm.Offset+cm.Size > len(componentBytes) {
		return nil, fmt.Errorf("component/instance: core module byte range [%d:%d) is out of bounds for a %d-byte component", cm.Offset, cm.Offset+cm.Size, len(componentBytes))
	}
	coreBytes := componentBytes[cm.Offset : cm.Offset+cm.Size]

	core, err := r.InstantiateWithConfig(ctx, coreBytes, wazy.NewModuleConfig())
	if err != nil {
		return nil, fmt.Errorf("component/instance: instantiate embedded core module: %w", err)
	}

	coreFuncIdx, err := buildCoreFuncIndexSpace(comp)
	if err != nil {
		core.Close(ctx) //nolint:errcheck // best-effort cleanup; the decode/wiring error below is what matters
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

	types := comp.Types
	resolve := func(idx uint32) binary.TypeDesc {
		if int(idx) >= len(types) {
			return nil
		}
		return types[idx].Descriptor
	}

	return &Instance{
		comp:           comp,
		core:           core,
		coreFuncIdx:    coreFuncIdx,
		canonForExport: canonForExport,
		resolve:        resolve,
	}, nil
}

// buildCoreFuncIndexSpace walks comp.Aliases to construct the core func
// index space (see package doc for the core:sort discriminator caveat).
func buildCoreFuncIndexSpace(comp *binary.Component) ([]string, error) {
	coreFuncIdx := make([]string, 0, len(comp.Aliases))
	for i, al := range comp.Aliases {
		if al.Sort != 0x00 {
			return nil, fmt.Errorf("component/instance: alias[%d] has non-core sort %#x; only core func aliases are supported by this milestone", i, al.Sort)
		}
		if al.TargetKind != 0x01 {
			return nil, fmt.Errorf("component/instance: alias[%d] targets kind %#x; only core-export aliases (off the sole core instance) are supported by this milestone", i, al.TargetKind)
		}
		if int(al.InstanceIdx) != 0 {
			return nil, fmt.Errorf("component/instance: alias[%d] references core instance %d, expected the sole core instance (index 0)", i, al.InstanceIdx)
		}
		coreFuncIdx = append(coreFuncIdx, al.Name)
	}
	return coreFuncIdx, nil
}

// validateCanons checks that every canon is a supported "canon lift" whose
// core func index and type index are in range, so later Call lookups never
// need to re-check them.
func validateCanons(comp *binary.Component, coreFuncIdx []string) error {
	for i, cn := range comp.Canons {
		if cn.Kind != 0x00 {
			return fmt.Errorf("component/instance: canon[%d] has kind %#x; only canon lift (0x00) is supported by this milestone", i, cn.Kind)
		}
		if int(cn.CoreFuncIdx) >= len(coreFuncIdx) {
			return fmt.Errorf("component/instance: canon[%d] references core func index %d, but the core func index space only has %d entries", i, cn.CoreFuncIdx, len(coreFuncIdx))
		}
		if int(cn.TypeIdx) >= len(comp.Types) {
			return fmt.Errorf("component/instance: canon[%d] references type index %d, out of range of %d types", i, cn.TypeIdx, len(comp.Types))
		}
		if _, ok := comp.Types[cn.TypeIdx].Descriptor.(binary.FuncDesc); !ok {
			return fmt.Errorf("component/instance: canon[%d] type index %d is not a func type (got %T)", i, cn.TypeIdx, comp.Types[cn.TypeIdx].Descriptor)
		}
	}
	return nil
}

// buildExportIndex maps each func-sort component export name to its index
// into comp.Canons. Because Instantiate already rejected any component
// import or component-level func alias (which would otherwise also occupy
// slots in the component func index space), that index space is exactly the
// canon declaration order: component func index i == comp.Canons[i].
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
// args. Each arg is lowered into core wasm values per its declared
// parameter type, the underlying core function (found via the export's
// canon lift) is called with the flattened arguments, and the raw core
// results are lifted back into an abi.Value per the canon's declared
// result type.
func (in *Instance) Call(ctx context.Context, exportName string, args ...abi.Value) ([]abi.Value, error) {
	canonIdx, ok := in.canonForExport[exportName]
	if !ok {
		return nil, fmt.Errorf("component/instance: export %q not found", exportName)
	}
	canon := in.comp.Canons[canonIdx]
	fd, ok := in.comp.Types[canon.TypeIdx].Descriptor.(binary.FuncDesc)
	if !ok {
		// Instantiate already validated this; kept as a defensive check.
		return nil, fmt.Errorf("component/instance: export %q: canon type is not a func type (got %T)", exportName, in.comp.Types[canon.TypeIdx].Descriptor)
	}

	if len(args) != len(fd.Params) {
		return nil, fmt.Errorf("component/instance: export %q takes %d parameter(s), got %d", exportName, len(fd.Params), len(args))
	}

	coreFuncName := in.coreFuncIdx[canon.CoreFuncIdx]
	coreFn := in.core.ExportedFunction(coreFuncName)
	if coreFn == nil {
		return nil, fmt.Errorf("component/instance: core module has no exported function %q (referenced by canon lift for export %q)", coreFuncName, exportName)
	}

	// FlattenFunc tells us the real core function signature (lift context:
	// this is the actual wrapper the core module exports). We cross-check
	// our own per-value flattening against it below to catch whole
	// parameter-list / result spilling, which this milestone doesn't
	// implement.
	coreParamsWant, coreResultsWant, err := abi.FlattenFunc(fd, in.resolve, "lift")
	if err != nil {
		return nil, fmt.Errorf("component/instance: export %q: flatten func type: %w", exportName, err)
	}

	mem, memAvailable := in.memoryBytes()
	realloc := in.reallocFn(ctx)

	coreArgs, err := in.lowerParams(fd, args, mem, memAvailable, realloc, exportName)
	if err != nil {
		return nil, err
	}
	if len(coreArgs) != len(coreParamsWant) {
		return nil, fmt.Errorf("component/instance: export %q: parameter list flattens to %d core value(s) but the core signature expects %d; whole-parameter-list spilling to memory is not supported by this milestone", exportName, len(coreArgs), len(coreParamsWant))
	}

	stack := make([]uint64, len(coreArgs))
	for i, cv := range coreArgs {
		stack[i] = cv.Bits
	}

	rawResults, err := coreFn.Call(ctx, stack...)
	if err != nil {
		return nil, fmt.Errorf("component/instance: export %q: call core func %q: %w", exportName, coreFuncName, err)
	}

	return in.liftResult(fd, rawResults, coreResultsWant, mem, memAvailable, exportName)
}

// lowerParams lowers each component-level argument into its flattened core
// values, in parameter order.
func (in *Instance) lowerParams(fd binary.FuncDesc, args []abi.Value, mem []byte, memAvailable bool, realloc abi.Realloc, exportName string) ([]abi.CoreValue, error) {
	var coreArgs []abi.CoreValue
	for i, p := range fd.Params {
		pt, err := resolveTypeRef(&p.Type, in.resolve)
		if err != nil {
			return nil, fmt.Errorf("component/instance: export %q param %d (%s): %w", exportName, i, p.Name, err)
		}
		if usesMemory(pt, in.resolve) && !memAvailable {
			return nil, fmt.Errorf("component/instance: export %q param %d (%s) requires linear memory (string/list), but the core module exports no memory", exportName, i, p.Name)
		}
		flat, err := abi.LowerFlat(args[i], pt, in.resolve, realloc, mem)
		if err != nil {
			return nil, fmt.Errorf("component/instance: export %q param %d (%s): lower: %w", exportName, i, p.Name, err)
		}
		coreArgs = append(coreArgs, flat...)
	}
	return coreArgs, nil
}

// liftResult lifts the raw core call results back into a single abi.Value
// per fd's declared result type. Multi-result functions (more than one
// named result) and results that require memory when the core module has
// none both fail loudly rather than being silently mishandled.
func (in *Instance) liftResult(fd binary.FuncDesc, rawResults []uint64, coreResultsWant []string, mem []byte, memAvailable bool, exportName string) ([]abi.Value, error) {
	resultRefs := funcResultTypeRefs(fd)
	if len(resultRefs) > 1 {
		return nil, fmt.Errorf("component/instance: export %q has %d named results; multiple component-level results are not supported by this milestone", exportName, len(resultRefs))
	}
	if len(resultRefs) == 0 {
		if len(rawResults) != 0 {
			return nil, fmt.Errorf("component/instance: export %q: core func returned %d value(s) for a 0-result signature", exportName, len(rawResults))
		}
		return nil, nil
	}

	rt, err := resolveTypeRef(&resultRefs[0], in.resolve)
	if err != nil {
		return nil, fmt.Errorf("component/instance: export %q result: %w", exportName, err)
	}
	if usesMemory(rt, in.resolve) && !memAvailable {
		return nil, fmt.Errorf("component/instance: export %q result requires linear memory (string/list), but the core module exports no memory", exportName)
	}

	flatKinds, err := abi.Flatten(rt, in.resolve)
	if err != nil {
		return nil, fmt.Errorf("component/instance: export %q result: %w", exportName, err)
	}
	if len(flatKinds) != len(coreResultsWant) {
		// FlattenFunc collapsed the result to a single pointer because it
		// exceeds MaxFlatResults: the real core func returns a memory
		// pointer we'd need to Load a synthetic record from, which this
		// milestone does not implement.
		return nil, fmt.Errorf("component/instance: export %q result flattens to %d core value(s), exceeding the flat-result limit (core signature returns %d value(s), a spilled memory pointer); spilled results are not supported by this milestone", exportName, len(flatKinds), len(coreResultsWant))
	}
	if len(rawResults) != len(flatKinds) {
		return nil, fmt.Errorf("component/instance: export %q: core func returned %d value(s), expected %d", exportName, len(rawResults), len(flatKinds))
	}

	coreResults := make([]abi.CoreValue, len(rawResults))
	for i, u := range rawResults {
		coreResults[i] = abi.CoreValue{Kind: flatKinds[i], Bits: u}
	}

	val, err := abi.LiftFlat(coreResults, rt, in.resolve, mem)
	if err != nil {
		return nil, fmt.Errorf("component/instance: export %q result: lift: %w", exportName, err)
	}
	return []abi.Value{val}, nil
}

// Close releases the underlying core module instance. It does not close the
// Runtime passed to Instantiate, which the caller owns.
func (in *Instance) Close(ctx context.Context) error {
	return in.core.Close(ctx)
}

// memoryBytes returns a read/write view of the core module's exported
// "memory", if it has one.
func (in *Instance) memoryBytes() ([]byte, bool) {
	m := in.core.ExportedMemory("memory")
	if m == nil {
		return nil, false
	}
	buf, ok := m.Read(0, m.Size())
	if !ok {
		return nil, false
	}
	return buf, true
}

// reallocFn returns the abi.Realloc backed by the core module's
// "cabi_realloc" export, or one that fails loudly if the core module
// doesn't export it.
func (in *Instance) reallocFn(ctx context.Context) abi.Realloc {
	fn := in.core.ExportedFunction("cabi_realloc")
	if fn == nil {
		return func(uint32, uint32, uint32, uint32) (uint32, error) {
			return 0, fmt.Errorf("component/instance: memory allocation requires a %q export on the core module, which is not present", "cabi_realloc")
		}
	}
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

// funcResultTypeRefs normalizes FuncResults (unnamed-or-named) into a slice
// of TypeRefs: 0 entries for no results, 1 for a single unnamed or named
// result, or more for multiple named results (which Call rejects).
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
// binary.TypeDesc, failing loud instead of returning a nil descriptor that
// would otherwise surface as an opaque "unknown type descriptor" error
// downstream.
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

// usesMemory reports whether a value of type t needs linear memory to lower
// or lift (directly, as a string/list, or transitively through a
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
