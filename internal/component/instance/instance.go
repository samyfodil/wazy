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
// Still rejected in both: canon resource.* built-ins, non-func exports,
// multi-result functions, whole-parameter-list / result spilling to memory,
// and string/list values when no linear memory is available.
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

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/component/abi"
	"github.com/samyfodil/wazy/internal/component/binary"
)

// Instance is an instantiated WebAssembly component: the set of instantiated
// core/host modules plus, for each component-level exported function, the
// binding needed to call it through its canon lift signature.
type Instance struct {
	resolve abi.Resolver

	// exports maps a component export name to the binding that backs it.
	exports map[string]*boundExport

	// closers are every module instantiated for this component (core guest
	// modules and synthetic host modules), closed in reverse order by Close.
	closers []api.Module
}

// boundExport binds a component export to the core function that implements
// it and to the module that provides the linear memory / cabi_realloc used to
// lower parameters and lift results across the call.
type boundExport struct {
	mod      api.Module      // exports funcName; its Memory() backs lower/lift
	funcName string          // core export name to call
	fd       binary.FuncDesc // component-level func type (lift signature)
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

	if len(comp.Imports) > 0 {
		return instantiateWithImports(ctx, r, comp, componentBytes, cfg)
	}
	return instantiateComponent(ctx, r, comp, componentBytes)
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
		fd := comp.Types[canon.TypeIdx].Descriptor.(binary.FuncDesc) // validated by validateCanons
		exports[name] = &boundExport{
			mod:      core,
			funcName: coreFuncIdx[canon.CoreFuncIdx],
			fd:       fd,
		}
	}

	return &Instance{resolve: resolve, exports: exports, closers: []api.Module{core}}, nil
}

// coreModuleBytes returns the slice of componentBytes holding an embedded core
// module, bounds-checked.
func coreModuleBytes(cm binary.CoreModule, componentBytes []byte) ([]byte, error) {
	if cm.Offset < 0 || cm.Size < 0 || cm.Offset+cm.Size > len(componentBytes) {
		return nil, fmt.Errorf("component/instance: core module byte range [%d:%d) is out of bounds for a %d-byte component", cm.Offset, cm.Offset+cm.Size, len(componentBytes))
	}
	return componentBytes[cm.Offset : cm.Offset+cm.Size], nil
}

// typeResolver returns an abi.Resolver over the component's type table.
func typeResolver(comp *binary.Component) abi.Resolver {
	types := comp.Types
	return func(idx uint32) binary.TypeDesc {
		if int(idx) >= len(types) {
			return nil
		}
		return types[idx].Descriptor
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
		if int(cn.TypeIdx) >= len(comp.Types) {
			return fmt.Errorf("component/instance: canon[%d] references type index %d, out of range of %d types", i, cn.TypeIdx, len(comp.Types))
		}
		if _, ok := comp.Types[cn.TypeIdx].Descriptor.(binary.FuncDesc); !ok {
			return fmt.Errorf("component/instance: canon[%d] type index %d is not a func type (got %T)", i, cn.TypeIdx, comp.Types[cn.TypeIdx].Descriptor)
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

func (in *Instance) invoke(ctx context.Context, be *boundExport, exportName string, args []abi.Value) ([]abi.Value, error) {
	fd := be.fd
	if len(args) != len(fd.Params) {
		return nil, fmt.Errorf("component/instance: export %q takes %d parameter(s), got %d", exportName, len(fd.Params), len(args))
	}

	coreFn := be.mod.ExportedFunction(be.funcName)
	if coreFn == nil {
		return nil, fmt.Errorf("component/instance: core module has no exported function %q (referenced by canon lift for export %q)", be.funcName, exportName)
	}

	// FlattenFunc gives the real core function signature (lift context: this
	// is the actual wrapper the core module exports). We cross-check our own
	// per-value flattening against it below to catch whole parameter-list /
	// result spilling, which this milestone does not implement.
	coreParamsWant, coreResultsWant, err := abi.FlattenFunc(fd, in.resolve, "lift")
	if err != nil {
		return nil, fmt.Errorf("component/instance: export %q: flatten func type: %w", exportName, err)
	}

	mem, memAvailable := memoryBytesOf(be.mod)
	realloc := reallocOf(ctx, be.mod)

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
		return nil, fmt.Errorf("component/instance: export %q: call core func %q: %w", exportName, be.funcName, err)
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

// liftResult lifts the raw core call results back into a single abi.Value per
// fd's declared result type. Multi-result functions and results that require
// memory when none is available both fail loudly.
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
