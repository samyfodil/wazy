package instance

import (
	"fmt"

	"github.com/samyfodil/wazy/internal/component/binary"
)

// This file resolves the sorts whose values are DEFINITIONS rather than
// running code -- core modules and components, and the instances that carry
// them -- against what an instantiation was given.
//
// A core module or component index names a definition statically only when
// it reaches a section-1 module or a section-4 component: directly, through
// an export, or through an `alias outer` to one. Every other producer names
// what an instantiation supplies: an import names the instantiate-arg of that
// name, and an export alias -- `(core module $i "m")`, `(component $i "c")`
// -- names what instance $i exports. An `alias outer` closes over its
// environment: a component defined inside $c that outer-aliases $c's module
// import sees the module that instantiation of $c was given, however far the
// component travels before it is instantiated (wasmtime-nested's "closing
// over an outer alias which is actually an argument"). So a component value
// is its definition plus the scope it was defined in -- wasmtime's inliner
// calls the pair a ComponentClosure.
//
// None of this needs a running instance. What an instance exports of these
// sorts is fixed by the component it instantiates and the args it was given,
// so the scope of an instantiation answers it without instantiating
// anything; the graph engine still instantiates every instance once, in its
// own place, and this only says which definition an index names.

// scope is one instantiation of comp, as far as its definition-valued
// indices are concerned.
type scope struct {
	comp *binary.Component
	// outer is the scope comp's definition was evaluated in -- the
	// instantiation of comp.Outer that produced the component value -- and so
	// what an `alias outer` walks. nil for the root and for a component the
	// embedder supplied, neither of which is nested in anything.
	outer *scope
	// inst is the instantiation of comp, whose args name indices of from, the
	// instantiating component's scope. from is nil for the root, whose only
	// args are the components in supplied.
	inst     *binary.Instance
	from     *scope
	supplied map[string]*binary.Component
}

// componentValue is a component definition and the scope it was defined in.
// env is nil for a definition with nothing enclosing it.
type componentValue struct {
	def *binary.Component
	env *scope
}

// scopeFor is the scope comp's indices resolve in: cfg's own once
// instantiateGraph has set it up for comp, otherwise a root scope over the
// components cfg was given (none for a nil cfg -- a static question).
func (cfg *config) scopeFor(comp *binary.Component) *scope {
	if cfg == nil {
		return &scope{comp: comp}
	}
	if cfg.scope.comp == comp {
		return &cfg.scope
	}
	return &scope{comp: comp, supplied: cfg.scope.supplied}
}

// scopeMaxDepth bounds a walk through exports and aliases. A well-formed
// binary only names forward-declared items, so this guards against a
// hand-built or corrupt structure, not a real limit.
const scopeMaxDepth = 64

// module resolves a core module index to the component that embeds the
// module and its index in that component's CoreModules.
func (s *scope) module(idx uint32) (*binary.Component, uint32, error) {
	ds, local, _, err := s.def(ctSortCoreModule, idx)
	if err != nil {
		return nil, 0, err
	}
	return ds.comp, local, nil
}

// component resolves a component index to the component value it names.
func (s *scope) component(idx uint32) (componentValue, error) {
	ds, local, supplied, err := s.def(ctSortComponent, idx)
	if err != nil || supplied != nil {
		return componentValue{def: supplied}, err
	}
	return componentValue{def: ds.comp.NestedComponents[local], env: ds}, nil
}

// def walks idx in s's index space of sort to the scope that DEFINES what it
// names and the definition's index there: into CoreModules, NestedComponents
// or Instances. A component the embedder supplied for a root import has no
// defining scope; it comes back as supplied instead.
func (s *scope) def(sort byte, idx uint32) (ds *scope, local uint32, supplied *binary.Component, err error) {
	for depth := 0; depth < scopeMaxDepth; depth++ {
		c := s.comp
		kind, ref, err := slotOf(c, sort, idx)
		if err != nil {
			return nil, 0, nil, err
		}
		switch kind {
		case slotDef:
			return s, ref, nil, nil
		case slotExport:
			idx = c.Exports[ref].ExternIndex
		case slotImport:
			name := c.Imports[ref].Name
			if d := s.supplied[name]; d != nil && sort == ctSortComponent {
				return nil, 0, d, nil
			}
			from, argIdx, ok := s.arg(name, sort)
			if !ok {
				return nil, 0, nil, fmt.Errorf("%s %d names the %s import %q, which nothing supplied", ctSortDesc(sort), idx, ctSortDesc(sort), name)
			}
			s, idx = from, argIdx
		default: // slotAlias
			al := c.Aliases[ref]
			if al.TargetKind != 0x02 { // an export alias: what an instance exports
				if s, idx, err = s.instanceExport(al.InstanceIdx, al.Name, sort); err != nil {
					return nil, 0, nil, err
				}
				continue
			}
			for i := uint32(0); i < al.OuterCount; i++ {
				if s = s.enclosing(); s == nil {
					return nil, 0, nil, fmt.Errorf("%s %d: outer alias (count=%d) reaches past the outermost component", ctSortDesc(sort), idx, al.OuterCount)
				}
			}
			idx = al.OuterIndex
		}
	}
	return nil, 0, nil, fmt.Errorf("%s %d: export/alias chain exceeds %d links (cycle?)", ctSortDesc(sort), idx, scopeMaxDepth)
}

// arg is what the instantiation that made s passed for its import name of
// sort: an index in the instantiating component's scope.
func (s *scope) arg(name string, sort byte) (*scope, uint32, bool) {
	if s.from != nil {
		for _, a := range s.inst.Args {
			if a.Name == name && a.Sort == sort {
				return s.from, a.SortIdx, true
			}
		}
	}
	return nil, 0, false
}

// enclosing is the scope s's component was defined in. A scope built without
// one -- a component resolved statically, or one a test instantiates
// directly -- falls back to its definition-time parent, which names the same
// definitions and supplies nothing.
func (s *scope) enclosing() *scope {
	if s.outer != nil {
		return s.outer
	}
	if s.comp.Outer != nil {
		return &scope{comp: s.comp.Outer}
	}
	return nil
}

// instanceExport resolves the export name of sort of instance idx to the
// scope it is an index of and that index. An inline bundle's exports are
// items of the scope that defines it; an instantiation's are items of the
// instantiated component, in a scope of its own.
func (s *scope) instanceExport(idx uint32, name string, sort byte) (*scope, uint32, error) {
	ds, local, _, err := s.def(ctSortInstance, idx)
	if err != nil {
		return nil, 0, err
	}
	inst := &ds.comp.Instances[local]
	if inst.Kind == 0x01 { // inline exports
		for _, x := range inst.Exports {
			if x.Name == name && x.Sort == sort {
				return ds, x.SortIdx, nil
			}
		}
	} else {
		cv, err := ds.component(inst.ComponentIdx)
		if err != nil {
			return nil, 0, err
		}
		for _, x := range cv.def.Exports {
			if x.Name == name && x.ExternType == sort {
				return &scope{comp: cv.def, outer: cv.env, inst: inst, from: ds}, x.ExternIndex, nil
			}
		}
	}
	return nil, 0, fmt.Errorf("instance %d exports no %s %q", idx, ctSortDesc(sort), name)
}

// The producers of an index, whatever its sort.
const (
	slotDef = iota
	slotImport
	slotExport
	slotAlias
)

// slotOf reads entry idx of c's index space of sort (core module, component
// or instance): which producer made it and that producer's index. A
// hand-built Component has no module or component space, and its CoreModules
// and NestedComponents are those spaces, as for ResolveCoreModule.
func slotOf(c *binary.Component, sort byte, idx uint32) (kind int, ref uint32, err error) {
	n := 0
	switch sort {
	case ctSortCoreModule:
		if n = len(c.CoreModuleSpace); n == 0 {
			if n = len(c.CoreModules); int(idx) < n {
				return slotDef, idx, nil
			}
		} else if int(idx) < n {
			switch e := c.CoreModuleSpace[idx]; e.Kind {
			case binary.CoreModuleFromDefinition:
				return slotDef, e.Module, nil
			case binary.CoreModuleFromImport:
				return slotImport, e.Import, nil
			case binary.CoreModuleFromExport:
				return slotExport, e.Export, nil
			default:
				return slotAlias, e.Alias, nil
			}
		}
	case ctSortComponent:
		if n = len(c.ComponentSpace); n == 0 {
			if n = len(c.NestedComponents); int(idx) < n {
				return slotDef, idx, nil
			}
		} else if int(idx) < n {
			switch e := c.ComponentSpace[idx]; e.Kind {
			case binary.ComponentFromDefinition:
				return slotDef, e.Component, nil
			case binary.ComponentFromImport:
				return slotImport, e.Import, nil
			case binary.ComponentFromExport:
				return slotExport, e.Export, nil
			default:
				return slotAlias, e.Alias, nil
			}
		}
	default: // ctSortInstance
		if n = len(c.ComponentInstanceSpace); int(idx) < n {
			switch e := c.ComponentInstanceSpace[idx]; e.Kind {
			case binary.ComponentInstanceFromDefinition:
				return slotDef, e.Instance, nil
			case binary.ComponentInstanceFromImport:
				return slotImport, e.Import, nil
			case binary.ComponentInstanceFromExport:
				return slotExport, e.Export, nil
			default:
				return slotAlias, e.Alias, nil
			}
		}
	}
	return 0, 0, fmt.Errorf("%s %d out of range of the %d-entry %s index space", ctSortDesc(sort), idx, n, ctSortDesc(sort))
}
