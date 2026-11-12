package instance

import "fmt"

// This file checks the external visibility of types (Explainer.md,
// "External visibility of types"), as wasmparser's
// validate_and_register_named_types does: what a component imports or
// exports may name a record, variant, enum, flags or resource only through
// a type it imports or exports, so the type is visible to whoever imports or
// instantiates it. Identity is by ctTypeID: exporting a record names it by a
// new index, and only that index counts as named.

// ctVisibility is one component's or component type's named types. A
// component names few, so a set is a slice searched in full (ctHas).
type ctVisibility struct {
	imported, exported []*ctTypeID
}

// emptied is v with nothing in it but room, for reuse.
func (v ctVisibility) emptied() ctVisibility {
	return ctVisibility{imported: ctEmpty(v.imported), exported: ctEmpty(v.exported)}
}

func ctHas(set []*ctTypeID, id *ctTypeID) bool {
	for _, x := range set {
		if x == id {
			return true
		}
	}
	return false
}

// add registers ent, imported (import) or exported, and fails if it names a
// type that is not visible.
func (v *ctVisibility) add(ent ctEntity, import_ bool) error {
	if v.register(ent, import_) {
		return nil
	}
	kind := "export"
	if import_ {
		kind = "import"
	}
	return fmt.Errorf("%s not valid to be used as %s", ctSortDesc(ent.sort), kind)
}

// set is the named set an import (import_) or export is checked against.
func (v *ctVisibility) set(import_ bool) []*ctTypeID {
	if import_ {
		return v.imported
	}
	return v.exported
}

func (v *ctVisibility) register(ent ctEntity, import_ bool) bool {
	switch ent.sort {
	case ctSortType:
		set := v.set(import_)
		t := ent.ty
		// An imported type cannot name what only an export made visible.
		if import_ && t.from != nil && ctHas(v.exported, t.from) && !ctHas(v.imported, t.from) {
			return false
		}
		if !ctTypeContentsNamed(t, set) {
			return false
		}
		if t.id != nil {
			v.exported = append(v.exported, t.id)
			if import_ {
				v.imported = append(v.imported, t.id)
			}
		}
		return true
	case ctSortInstance:
		for _, x := range ent.inst.exports {
			if !v.register(x.ent, import_) {
				return false
			}
		}
		return true
	case ctSortFunc:
		return ctFuncNamed(ent.fn, v.set(import_))
	default: // a component or core module is standalone
		return true
	}
}

// ctTypeContentsNamed is all_valtypes_named: what type t refers to is named,
// though t itself need not be.
func ctTypeContentsNamed(t ctType, set []*ctTypeID) bool {
	switch {
	case t.val != nil:
		return ctDefinedNamed(t.val, set)
	case t.fn != nil:
		return ctFuncNamed(t.fn, set)
	case t.inst != nil:
		return ctInstanceNamed(t.inst, set)
	default: // a resource is named by its import or export; a component is closed
		return true
	}
}

// ctInstanceNamed is all_valtypes_named_in_instance: the types an instance
// type exports count as named within it, and only there.
func ctInstanceNamed(in *ctInstance, set []*ctTypeID) bool {
	// What it adds goes past set's end, which the caller's set does not
	// reach, so leaving drops it.
	for _, x := range in.exports {
		switch x.ent.sort {
		case ctSortFunc:
			if !ctFuncNamed(x.ent.fn, set) {
				return false
			}
		case ctSortType:
			if !ctTypeContentsNamed(x.ent.ty, set) {
				return false
			}
			if id := x.ent.ty.id; id != nil && !ctHas(set, id) {
				set = append(set, id)
			}
		case ctSortInstance:
			if !ctInstanceNamed(x.ent.inst, set) {
				return false
			}
		}
	}
	return true
}

func ctFuncNamed(f *ctFunc, set []*ctTypeID) bool {
	for _, list := range [2][]ctField{f.params, f.results} {
		for _, p := range list {
			if !ctValNamed(p.t, set) {
				return false
			}
		}
	}
	return true
}

// ctDefinedNamed is all_valtypes_named_in_defined: v's elements are named.
func ctDefinedNamed(v *ctVal, set []*ctTypeID) bool {
	switch v.kind {
	case ctvPrim, ctvFlags, ctvEnum:
		return true
	case ctvOwn, ctvBorrow:
		return ctHas(set, v.id)
	}
	for _, el := range v.elems {
		if el != nil && !ctValNamed(el, set) {
			return false
		}
	}
	return true
}

// ctValNamed is type_named_valtype: a record, variant, enum or flags must
// itself be named, a handle's resource must be, and anything else only
// needs its elements named.
func ctValNamed(v *ctVal, set []*ctTypeID) bool {
	if ctNominal(v) {
		return ctHas(set, v.id)
	}
	return ctDefinedNamed(v, set)
}

// ctIDSubst replaces type identities by others: an instantiation's imported
// types' by the arguments', so what the instance exports names the types it
// was given, as wasmparser remaps a component type's imported type ids when
// it is instantiated. Like ctSubst, it rewrites only what it changes; and
// each type once, so every use of one type stays one *ctVal (wit.go names
// types by that).
type ctIDSubst struct {
	ids  map[*ctTypeID]*ctTypeID
	memo map[*ctVal]*ctVal
}

// pair records the identities child's type imports have in arg, at any
// depth of an instance.
func (s *ctIDSubst) pair(child, arg ctEntity) {
	switch {
	case child.sort == ctSortType && arg.sort == ctSortType:
		if child.ty.id != nil && arg.ty.id != nil && child.ty.id != arg.ty.id {
			if s.ids == nil {
				s.ids = map[*ctTypeID]*ctTypeID{}
			}
			s.ids[child.ty.id] = arg.ty.id
		}
	case child.sort == ctSortInstance && arg.sort == ctSortInstance:
		for _, x := range child.inst.exports {
			if a, ok := ctLookup(arg.inst.exports, x.name); ok {
				s.pair(x.ent, a)
			}
		}
	}
}

func (s *ctIDSubst) val(v *ctVal) *ctVal {
	if v == nil || len(s.ids) == 0 {
		return v
	}
	if n, ok := s.memo[v]; ok {
		return n
	}
	n := s.rebuild(v)
	if s.memo == nil {
		s.memo = map[*ctVal]*ctVal{}
	}
	s.memo[v] = n
	return n
}

func (s *ctIDSubst) rebuild(v *ctVal) *ctVal {
	var elems []*ctVal
	for i, e := range v.elems {
		if n := s.val(e); n != e {
			if elems == nil {
				elems = append([]*ctVal(nil), v.elems...)
			}
			elems[i] = n
		}
	}
	id, renamed := s.ids[v.id]
	if elems == nil && !renamed {
		return v
	}
	c := *v
	if elems != nil {
		c.elems = elems
	}
	if renamed {
		c.id = id
	}
	return &c
}

func (s *ctIDSubst) fn(f *ctFunc) *ctFunc {
	var g *ctFunc
	for k, list := range [2][]ctField{f.params, f.results} {
		for i := range list {
			n := s.val(list[i].t)
			if n == list[i].t {
				continue
			}
			if g == nil {
				g = ctNewFunc(f.async, len(f.params), len(f.results))
				copy(g.params, f.params)
				copy(g.results, f.results)
			}
			if k == 0 {
				g.params[i].t = n
			} else {
				g.results[i].t = n
			}
		}
	}
	if g == nil {
		return f
	}
	return g
}

func (s *ctIDSubst) named(list []ctNamed) []ctNamed {
	if len(s.ids) == 0 {
		return list
	}
	var out []ctNamed
	for i, n := range list {
		e := n.ent
		switch e.sort {
		case ctSortFunc:
			e.fn = s.fn(e.fn)
		case ctSortInstance:
			if exports := s.named(e.inst.exports); !sameNamed(exports, e.inst.exports) {
				e.inst = &ctInstance{exports: exports, fresh: e.inst.fresh}
			}
		case ctSortType:
			if e.ty.val != nil {
				e.ty.val = s.val(e.ty.val)
			}
			if id, ok := s.ids[e.ty.id]; ok {
				e.ty.id = id
			}
		}
		if e != n.ent {
			if out == nil {
				out = append([]ctNamed(nil), list...)
			}
			out[i].ent = e
		}
	}
	if out == nil {
		return list
	}
	return out
}
