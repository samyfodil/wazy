package instance

import (
	"fmt"

	"github.com/samyfodil/wazy/internal/component/binary"
)

// The core index spaces of a ctEnv: core types, core modules and core
// instances, computed on demand like the component-level ones, and the
// checks over them. See ctcore.go for the types.

// ctCoreSpaces are a ctEnv's memoized core index spaces. Core types are not
// among them: one names nothing but its own declarations and an enclosing
// component's types, has no identity to keep, and is cheap to read again.
type ctCoreSpaces struct {
	modules []*ctModule
	modSt   []byte
	insts   []*ctCoreInstance
	instSt  []byte
}

// spaces returns e's core index spaces, allocating them on first use.
func (e *ctEnv) spaces() *ctCoreSpaces {
	if e.core == nil {
		c := e.comp
		e.core = &ctCoreSpaces{
			modules: make([]*ctModule, len(c.CoreModuleSpace)), modSt: make([]byte, len(c.CoreModuleSpace)),
			insts: make([]*ctCoreInstance, len(c.CoreInstances)), instSt: make([]byte, len(c.CoreInstances)),
		}
	}
	return e.core
}

// coreTypeAt is core type idx: a section-3 definition, or an outer alias of
// an enclosing component's.
func (e *ctEnv) coreTypeAt(idx uint32) (ctCoreType, error) {
	c := e.comp
	if int(idx) >= len(c.CoreTypeSpace) {
		return ctCoreType{}, fmt.Errorf("unknown core type %d: type index out of bounds", idx)
	}
	en := c.CoreTypeSpace[idx]
	if en.Kind == binary.CoreTypeFromAlias {
		al := c.Aliases[en.Alias]
		if al.TargetKind != 0x02 {
			return ctCoreType{}, fmt.Errorf("a core type alias must be an outer alias")
		}
		outer, _, err := outerScope(e, al.OuterCount)
		if err != nil {
			return ctCoreType{}, err
		}
		return outer.coreTypeAt(al.OuterIndex)
	}
	def := c.CoreTypes[en.Def]
	if def.Module != nil {
		mod, err := ctModuleOfType(e, def.Module)
		return ctCoreType{mod: mod}, err
	}
	return ctCoreType{fn: ctCoreRec(def.Rec)[en.Member]}, nil
}

// moduleAt is the type of core module idx. A module the component embeds is
// read off its own sections, which only the checks that need its type ask
// for.
func (e *ctEnv) moduleAt(idx uint32) (m *ctModule, err error) {
	c := e.comp
	if int(idx) >= len(c.CoreModuleSpace) {
		return nil, fmt.Errorf("unknown module %d: module index out of bounds", idx)
	}
	sp := e.spaces()
	if done, err := ctEnter(sp.modSt, idx, "module"); err != nil {
		return nil, err
	} else if done {
		return sp.modules[idx], nil
	}
	defer func() { ctLeave(sp.modSt, idx, err); sp.modules[idx] = m }()
	switch en := c.CoreModuleSpace[idx]; en.Kind {
	case binary.CoreModuleFromDefinition:
		bin, err := coreModuleBytes(c.CoreModules[en.Module], c.Bytes)
		if err != nil {
			return nil, err
		}
		if m, err = ctModuleOfDefinition(bin); err != nil {
			return nil, fmt.Errorf("core module %d: %w", idx, err)
		}
		return m, nil
	case binary.CoreModuleFromImport:
		ent, err := e.importAt(en.Import)
		return ent.mod, err
	case binary.CoreModuleFromExport:
		ent, err := e.exportAt(en.Export)
		return ent.mod, err
	default: // binary.CoreModuleFromAlias
		al := c.Aliases[en.Alias]
		if al.TargetKind != 0x02 {
			ent, err := e.aliasedExport(al)
			return ent.mod, err
		}
		outer := e
		for range al.OuterCount {
			if outer = outer.up; outer == nil {
				return nil, fmt.Errorf("invalid outer alias count of %d", al.OuterCount)
			}
		}
		return outer.moduleAt(al.OuterIndex)
	}
}

// coreModuleIsDefinition reports whether core module idx is, statically, a
// module some component embeds -- one the core linker checks as it links
// it -- rather than one an instantiation supplies.
func (e *ctEnv) coreModuleIsDefinition(idx uint32) bool {
	_, _, err := e.comp.ResolveCoreModule(idx)
	return err == nil
}

// coreInstanceAt is the type of core instance k: what the module it
// instantiates exports, or the items it exports inline.
func (e *ctEnv) coreInstanceAt(k uint32) (in *ctCoreInstance, err error) {
	c := e.comp
	if int(k) >= len(c.CoreInstances) {
		return nil, fmt.Errorf("unknown core instance %d: instance index out of bounds", k)
	}
	sp := e.spaces()
	if done, err := ctEnter(sp.instSt, k, "core instance"); err != nil {
		return nil, err
	} else if done {
		return sp.insts[k], nil
	}
	defer func() { ctLeave(sp.instSt, k, err); sp.insts[k] = in }()
	ci := c.CoreInstances[k]
	if ci.Kind == 0x00 {
		m, err := e.moduleAt(ci.ModuleIdx)
		if err != nil {
			return nil, err
		}
		return &ctCoreInstance{exports: m.exports}, nil
	}
	in = &ctCoreInstance{}
	for _, x := range ci.Exports {
		ext, err := e.coreItemAt(x.Sort, x.CoreSortIdx)
		if err != nil {
			return nil, err
		}
		in.exports = append(in.exports, ctCoreExport{name: x.Name, ext: ext})
	}
	return in, nil
}

// coreItemAt is the type of core item idx of a core sort. A table, memory,
// global or tag is only ever an alias of a core instance's export; a func is
// that or a canon, whose core type only lower and the resource built-ins
// would fix -- left unknown here, the check passes it.
func (e *ctEnv) coreItemAt(sort byte, idx uint32) (ctCoreExtern, error) {
	c := e.comp
	var al *binary.AliasDef
	if sort == 0x00 {
		if int(idx) >= len(c.CoreFuncSpace) {
			return ctCoreExtern{}, fmt.Errorf("unknown function %d: function index out of bounds", idx)
		}
		if en := c.CoreFuncSpace[idx]; en.Kind == binary.CoreFuncFromAlias {
			al = &c.Aliases[en.Alias]
		} else {
			return ctCoreExtern{kind: 0x00}, nil
		}
	} else {
		n := uint32(0)
		for i := range c.Aliases {
			if a := &c.Aliases[i]; a.Sort == 0x00 && a.CoreSort == sort && a.TargetKind == 0x01 {
				if n == idx {
					al = a
					break
				}
				n++
			}
		}
		if al == nil {
			return ctCoreExtern{}, fmt.Errorf("unknown %s %d: %s index out of bounds", ctCoreKindDesc(sort), idx, ctCoreKindDesc(sort))
		}
	}
	in, err := e.coreInstanceAt(al.InstanceIdx)
	if err != nil {
		return ctCoreExtern{}, err
	}
	ext, ok := in.export(al.Name)
	if !ok {
		return ctCoreExtern{}, fmt.Errorf("core instance %d has no export named `%s`", al.InstanceIdx, al.Name)
	}
	return ext, nil
}

// coreInstanceExports calls fn with each export of core instance k, by name
// and sort -- all an alias check needs, read without decoding a module the
// component embeds: its export section is read alone, and its names handed
// over as the bytes they are (raw), so reading them allocates nothing; a
// name of any other instance comes as name.
func (e *ctEnv) coreInstanceExports(k uint32, fn func(raw []byte, name string, kind byte)) error {
	c := e.comp
	ci := c.CoreInstances[k]
	switch {
	case ci.Kind == 0x01:
		for _, x := range ci.Exports {
			fn(nil, x.Name, x.Sort)
		}
	case e.coreModuleIsDefinition(ci.ModuleIdx):
		owner, mi, _ := c.ResolveCoreModule(ci.ModuleIdx)
		bin, err := coreModuleBytes(owner.CoreModules[mi], owner.Bytes)
		if err != nil {
			return err
		}
		if err := binary.CoreModuleExports(bin, func(name []byte, kind byte) { fn(name, "", kind) }); err != nil {
			return fmt.Errorf("core module %d: %w", ci.ModuleIdx, err)
		}
	default:
		m, err := e.moduleAt(ci.ModuleIdx)
		if err != nil {
			return err
		}
		for _, x := range m.exports {
			fn(nil, x.name, x.ext.kind)
		}
	}
	return nil
}

// checkCoreInstances checks the core instantiations of a module the
// component does not embed -- an import, or one an alias or argument names --
// against its type, as wasmparser's instantiate_core_module does, and every
// alias of a core instance's export by name and sort, as its
// alias_core_instance_export does. An embedded module's instantiations are
// linked by the core linker, which checks the same imports as it links them
// -- a second check here would read every embedded module on every
// instantiation that is not cached -- and coreLinkError says why in these
// words when it refuses one.
func (e *ctEnv) checkCoreInstances() error {
	c := e.comp
	for k, ci := range c.CoreInstances {
		if ci.Kind == 0x00 && !e.coreModuleIsDefinition(ci.ModuleIdx) {
			if err := e.checkCoreInstantiation(uint32(k)); err != nil {
				return err
			}
		}
	}
	// Each aliased instance's exports are read once, and every alias of it
	// looked for among them. kinds holds, per alias, the sort found plus
	// one, or -1 while it is looked for.
	var buf [64]int16
	kinds := buf[:0]
	if len(c.Aliases) > len(buf) {
		kinds = make([]int16, 0, len(c.Aliases))
	}
	kinds = kinds[:len(c.Aliases)]
	for i, al := range c.Aliases {
		if al.Sort != 0x00 || al.TargetKind != 0x01 || kinds[i] != 0 {
			continue
		}
		for j := i; j < len(c.Aliases); j++ {
			if b := c.Aliases[j]; b.Sort == 0x00 && b.TargetKind == 0x01 && b.InstanceIdx == al.InstanceIdx {
				kinds[j] = -1
			}
		}
		err := e.coreInstanceExports(al.InstanceIdx, func(raw []byte, name string, kind byte) {
			for j := i; j < len(c.Aliases); j++ {
				b := &c.Aliases[j]
				if kinds[j] != -1 || b.InstanceIdx != al.InstanceIdx {
					continue
				}
				if (raw != nil && string(raw) == b.Name) || (raw == nil && name == b.Name) {
					kinds[j] = int16(kind) + 1
				}
			}
		})
		if err != nil {
			return err
		}
	}
	for i, al := range c.Aliases {
		if kinds[i] == 0 {
			continue
		}
		if kinds[i] == -1 {
			return fmt.Errorf("core instance %d has no export named `%s`", al.InstanceIdx, al.Name)
		}
		if byte(kinds[i]-1) != al.CoreSort {
			return fmt.Errorf("export `%s` for core instance %d is not a %s", al.Name, al.InstanceIdx, ctCoreKindDesc(al.CoreSort))
		}
	}
	return nil
}

// checkCoreInstantiation checks core instance k's arguments against the type
// of the module it instantiates: each of the module's imports is looked up in
// the instance supplied for its module name and checked by core subtyping
// (wasmparser's instantiate_core_module).
func (e *ctEnv) checkCoreInstantiation(k uint32) error {
	ci := e.comp.CoreInstances[k]
	m, err := e.moduleAt(ci.ModuleIdx)
	if err != nil {
		return err
	}
	args := map[string]*ctCoreInstance{}
	for _, a := range ci.Args {
		in, err := e.coreInstanceAt(a.InstanceIdx)
		if err != nil {
			return err
		}
		args[a.Name] = in
	}
	for _, im := range m.imports {
		in, ok := args[im.module]
		if !ok {
			return fmt.Errorf("core instance %d: missing module instantiation argument named `%s`", k, im.module)
		}
		ext, ok := in.export(im.name)
		if !ok {
			return fmt.Errorf("core instance %d: module instantiation argument `%s` does not export an item named `%s`", k, im.module, im.name)
		}
		if err := ctCoreExternSub(ext, im.ext); err != nil {
			return fmt.Errorf("core instance %d: type mismatch for export `%s` of module instantiation argument `%s`: %w", k, im.name, im.module, err)
		}
	}
	return nil
}

// coreLinkError is what to report for err, the core linker's or the core
// decoder's refusal of comp's core instance k (every core instantiation of
// comp, when k is -1): the component model validator's account of the first
// core instantiation whose arguments do not have the types its module
// imports, which is what the refusal comes down to -- or err itself when the
// types find nothing. It runs only when instantiation has already failed: the
// core linker checks the same types as it links, so a component that links
// pays for no second check.
func coreLinkError(comp *binary.Component, k int, err error) error {
	var chain []*binary.Component
	for c := comp; c != nil; c = c.Outer {
		chain = append(chain, c)
	}
	var e *ctEnv
	for i := len(chain) - 1; i >= 0; i-- {
		e = newCTEnv(chain[i], e)
	}
	defer func() {
		for e != nil {
			up := e.up
			e.release()
			e = up
		}
	}()
	for j, ci := range comp.CoreInstances {
		if ci.Kind != 0x00 || (k >= 0 && j != k) {
			continue
		}
		if terr := e.checkCoreInstantiation(uint32(j)); terr != nil {
			return terr
		}
	}
	return err
}
