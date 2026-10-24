package instance

import (
	"fmt"
	"slices"
)

// This file is the component-level type system: the types of the things a
// component imports, exports and instantiates with -- funcs, instances,
// components, value types and resources -- and the subtyping relation the
// Component Model defines on them (Explainer.md, "Type checking"). ctenv.go
// computes these types from a decoded component.
//
// It follows the validator the Bytecode Alliance ships, wasm-tools'
// wasmparser (validator/component_types.rs, SubtypeCx), rule for rule and
// error text for error text, so a component a validator refuses is refused
// here for the same stated reason:
//
//   - An instance type is a subtype of another when it exports at least every
//     name the other does, each a subtype of the other's. A component type is
//     a subtype of another when it imports at most what the other does
//     (contravariantly) and exports at least what the other does
//     (covariantly). Names are matched by name, not position.
//   - Funcs and value types are compared for equality, which is what
//     wasmparser does too: the spec leaves value subtyping open, and both
//     wasmparser and Wasmtime commit to exact equality until it is settled.
//   - A resource type is abstract: two are the same only when they are the
//     same resource. A `(sub resource)` import or export introduces a fresh
//     one, and a check lines the fresh resources of one side up with the
//     other side's by the path of names that reaches them before comparing
//     -- the substitution that makes "a component exporting resource R" a
//     subtype of "a component exporting some resource".
//
// Value types are normalised into one tree shape independent of the type
// index space they were declared in, so two components' types compare
// structurally, and a resource is a *ctResource compared by identity.

// ctResource is a resource type. Its identity is its address: every
// resource definition, and every `(sub resource)` import or export, makes a
// new one, and a substitution swaps one for another.
type ctResource struct {
	// name is what it was declared or imported as, for messages only.
	name string
}

// ctValKind is the type constructor of a ctVal.
type ctValKind uint8

const (
	ctvPrim ctValKind = iota
	ctvRecord
	ctvVariant
	ctvList
	ctvFixedList
	ctvTuple
	ctvFlags
	ctvEnum
	ctvOption
	ctvResult
	ctvOwn
	ctvBorrow
	ctvStream
	ctvFuture
	ctvMap
)

// desc is wasmparser's name for the constructor in a mismatch message.
func (k ctValKind) desc() string {
	return [...]string{
		ctvPrim: "primitive", ctvRecord: "record", ctvVariant: "variant", ctvList: "list",
		ctvFixedList: "fixed-length list", ctvTuple: "tuple", ctvFlags: "flags", ctvEnum: "enum",
		ctvOption: "option", ctvResult: "result", ctvOwn: "own", ctvBorrow: "borrow",
		ctvStream: "stream", ctvFuture: "future", ctvMap: "map",
	}[k]
}

// ctVal is a value type.
type ctVal struct {
	kind ctValKind

	// prim is a primitive's name ("u32", "string", ...). defined is set when
	// it was named through a type definition rather than written inline,
	// which only changes how a mismatch is worded (wasmparser says
	// "expected primitive" of the former).
	prim    string
	defined bool

	// labels are a record's field names, a variant's case names, or the
	// names of a flags or enum.
	labels []string

	// elems are the component types: a record's field types, a variant's
	// case payloads (nil for none), a tuple's types; the single element of a
	// list, fixed-length list, option, stream or future (nil for a bare
	// stream or future); a result's ok and err (each nil when absent); a
	// map's key and value.
	elems []*ctVal

	// length is a fixed-length list's.
	length uint32

	// res is an own's or borrow's resource.
	res *ctResource
}

// ctField is a named func parameter or result.
type ctField struct {
	name string
	t    *ctVal
}

// ctFunc is a func type.
type ctFunc struct {
	async   bool
	params  []ctField
	results []ctField // one unnamed result has the name ""
}

// ctNamed is one import or export.
type ctNamed struct {
	name string
	ent  ctEntity
}

// ctResPath locates a resource inside a list of imports or exports: the
// import or export it is reached by, then an export of each instance along
// the way.
type ctResPath struct {
	res  *ctResource
	path []string
}

// ctInstance is an instance type.
type ctInstance struct {
	exports []ctNamed

	// fresh are the resources this instance TYPE introduces with `(export
	// "r" (type (sub resource)))`, at any depth. Each import or export of
	// the type gets its own copy of them (see ctFreshen); an instance
	// itself, once it exists, introduces none.
	fresh []ctResPath
}

// ctComponent is a component type.
type ctComponent struct {
	imports []ctNamed
	exports []ctNamed

	// importedRes are the resources the component's imports introduce, and
	// definedRes the ones its exports do: the abstract resources it is
	// parameterized by, and the ones each instantiation of it makes anew.
	importedRes []ctResPath
	definedRes  []ctResPath
}

// ctType is what a type index names: exactly one field is set.
type ctType struct {
	res  *ctResource
	val  *ctVal
	fn   *ctFunc
	inst *ctInstance
	comp *ctComponent
}

func (t ctType) desc() string {
	switch {
	case t.res != nil:
		return "resource"
	case t.val != nil:
		return "defined type"
	case t.fn != nil:
		return "func"
	case t.inst != nil:
		return "instance"
	default:
		return "component"
	}
}

// The sorts of the binary format, as ctEntity.sort.
const (
	ctSortCoreModule byte = 0x00
	ctSortFunc       byte = 0x01
	ctSortValue      byte = 0x02
	ctSortType       byte = 0x03
	ctSortComponent  byte = 0x04
	ctSortInstance   byte = 0x05
)

func ctSortDesc(sort byte) string {
	switch sort {
	case ctSortCoreModule:
		return "module"
	case ctSortFunc:
		return "func"
	case ctSortValue:
		return "value"
	case ctSortType:
		return "type"
	case ctSortComponent:
		return "component"
	case ctSortInstance:
		return "instance"
	}
	return fmt.Sprintf("sort %#x", sort)
}

// ctEntity is the type of one importable or exportable thing: the field
// its sort selects is set (none for a core module, whose type wazy does not
// check -- see ctEntitySub).
type ctEntity struct {
	sort byte
	fn   *ctFunc
	inst *ctInstance
	comp *ctComponent
	ty   ctType
}

func ctLookup(list []ctNamed, name string) (ctEntity, bool) {
	for i := range list {
		if list[i].name == name {
			return list[i].ent, true
		}
	}
	return ctEntity{}, false
}

// ctSubst replaces resources by others: an instantiation's imported
// resources by what its arguments supplied, its defined resources by fresh
// ones. It rewrites only what it changes, so a type with nothing to replace
// is returned as is.
type ctSubst map[*ctResource]*ctResource

func (s ctSubst) val(v *ctVal) *ctVal {
	if v == nil || len(s) == 0 {
		return v
	}
	if v.kind == ctvOwn || v.kind == ctvBorrow {
		if n, ok := s[v.res]; ok {
			c := *v
			c.res = n
			return &c
		}
		return v
	}
	var elems []*ctVal
	for i, e := range v.elems {
		if n := s.val(e); n != e {
			if elems == nil {
				elems = append([]*ctVal(nil), v.elems...)
			}
			elems[i] = n
		}
	}
	if elems == nil {
		return v
	}
	c := *v
	c.elems = elems
	return &c
}

func (s ctSubst) fields(fs []ctField) []ctField {
	var out []ctField
	for i, f := range fs {
		if n := s.val(f.t); n != f.t {
			if out == nil {
				out = append([]ctField(nil), fs...)
			}
			out[i].t = n
		}
	}
	if out == nil {
		return fs
	}
	return out
}

func (s ctSubst) fn(f *ctFunc) *ctFunc {
	if f == nil || len(s) == 0 {
		return f
	}
	params, results := s.fields(f.params), s.fields(f.results)
	if sameFields(params, f.params) && sameFields(results, f.results) {
		return f
	}
	return &ctFunc{async: f.async, params: params, results: results}
}

// sameFields reports whether a and b are the same slice, which is how fields
// says it changed nothing.
func sameFields(a, b []ctField) bool {
	return len(a) == len(b) && (len(a) == 0 || &a[0] == &b[0])
}

func (s ctSubst) named(list []ctNamed) []ctNamed {
	var out []ctNamed
	for i, n := range list {
		if e := s.entity(n.ent); e != n.ent {
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

func (s ctSubst) inst(in *ctInstance) *ctInstance {
	if in == nil || len(s) == 0 {
		return in
	}
	exports := s.named(in.exports)
	if sameNamed(exports, in.exports) {
		return in
	}
	return &ctInstance{exports: exports, fresh: in.fresh}
}

// comp substitutes into a component type's imports and exports. Its own
// imported and defined resources are bound by it and so never in s (each
// was made for this type alone), which is why they carry over unchanged.
func (s ctSubst) comp(c *ctComponent) *ctComponent {
	if c == nil || len(s) == 0 {
		return c
	}
	imports, exports := s.named(c.imports), s.named(c.exports)
	if sameNamed(imports, c.imports) && sameNamed(exports, c.exports) {
		return c
	}
	return &ctComponent{imports: imports, exports: exports, importedRes: c.importedRes, definedRes: c.definedRes}
}

func sameNamed(a, b []ctNamed) bool {
	return len(a) == len(b) && (len(a) == 0 || &a[0] == &b[0])
}

func (s ctSubst) typ(t ctType) ctType {
	switch {
	case t.res != nil:
		if n, ok := s[t.res]; ok {
			return ctType{res: n}
		}
		return t
	case t.val != nil:
		return ctType{val: s.val(t.val)}
	case t.fn != nil:
		return ctType{fn: s.fn(t.fn)}
	case t.inst != nil:
		return ctType{inst: s.inst(t.inst)}
	case t.comp != nil:
		return ctType{comp: s.comp(t.comp)}
	}
	return t
}

func (s ctSubst) entity(e ctEntity) ctEntity {
	if len(s) == 0 {
		return e
	}
	switch e.sort {
	case ctSortFunc:
		e.fn = s.fn(e.fn)
	case ctSortInstance:
		e.inst = s.inst(e.inst)
	case ctSortComponent:
		e.comp = s.comp(e.comp)
	case ctSortType:
		e.ty = s.typ(e.ty)
	}
	return e
}

// ctFreshen gives an import or export of entity e its own copy of the
// resources e's instance type introduces -- importing one instance type
// twice yields two instances whose resources differ -- and returns where
// each new resource sits, relative to e.
func ctFreshen(e ctEntity) (ctEntity, []ctResPath) {
	if e.sort != ctSortInstance || len(e.inst.fresh) == 0 {
		return e, nil
	}
	s := make(ctSubst, len(e.inst.fresh))
	paths := make([]ctResPath, len(e.inst.fresh))
	for i, p := range e.inst.fresh {
		n := &ctResource{name: p.res.name}
		s[p.res] = n
		paths[i] = ctResPath{res: n, path: p.path}
	}
	// A new instance either way: the copy introduces nothing of its own, so
	// the template's fresh list must not come along.
	e.inst = &ctInstance{exports: s.named(e.inst.exports)}
	return e, paths
}

// prefix puts name in front of every path.
func prefixPaths(name string, paths []ctResPath) []ctResPath {
	for i := range paths {
		paths[i].path = append([]string{name}, paths[i].path...)
	}
	return paths
}

// ctEntitySub reports whether a is a subtype of b, and why not.
func ctEntitySub(a, b ctEntity) error {
	if a.sort != b.sort {
		return fmt.Errorf("expected %s, found %s", ctSortDesc(b.sort), ctSortDesc(a.sort))
	}
	switch a.sort {
	case ctSortFunc:
		return ctFuncSub(a.fn, b.fn)
	case ctSortInstance:
		return ctInstanceSub(a.inst, b.inst)
	case ctSortComponent:
		return ctComponentSub(a.comp, b.comp)
	case ctSortType:
		return ctTypeSub(a.ty, b.ty)
	}
	// A core module. wazy does not model core module types, because it does
	// not run a component that hands one to another: a component importing
	// one, an instantiation passing one to a component that instantiates
	// it, and a core module export are each refused when instantiated. So
	// the type a core module is passed as is not checked here -- nothing
	// wazy runs relies on it -- and a component that merely passes one along
	// to something that never instantiates it stays valid, as before.
	return nil
}

// ctTypeSub reports whether type a is a subtype of type b.
func ctTypeSub(a, b ctType) error {
	switch {
	case a.res != nil && b.res != nil:
		if a.res != b.res {
			return fmt.Errorf("resource types are not the same (%s vs. %s)", ctResName(a.res), ctResName(b.res))
		}
		return nil
	case a.val != nil && b.val != nil:
		return ctValSub(a.val, b.val)
	case a.fn != nil && b.fn != nil:
		return ctFuncSub(a.fn, b.fn)
	case a.inst != nil && b.inst != nil:
		return ctInstanceSub(a.inst, b.inst)
	case a.comp != nil && b.comp != nil:
		return ctComponentSub(a.comp, b.comp)
	}
	return fmt.Errorf("expected %s, found %s", b.desc(), a.desc())
}

func ctResName(r *ctResource) string {
	if r.name == "" {
		return "an unnamed resource"
	}
	return "`" + r.name + "`"
}

// ctFuncSub reports whether func type a is a subtype of b: equality, as in
// wasmparser.
func ctFuncSub(a, b *ctFunc) error {
	if a.async != b.async {
		return fmt.Errorf("expected %s function, found %s function", syncDesc(b.async), syncDesc(a.async))
	}
	if len(a.params) != len(b.params) {
		return fmt.Errorf("expected %d parameters, found %d", len(b.params), len(a.params))
	}
	for i := range a.params {
		an, bn := a.params[i].name, b.params[i].name
		if an != bn {
			return fmt.Errorf("expected parameter named `%s`, found `%s`", bn, an)
		}
		if err := ctValSub(a.params[i].t, b.params[i].t); err != nil {
			return fmt.Errorf("type mismatch in function parameter `%s`: %w", an, err)
		}
	}
	switch la, lb := len(a.results), len(b.results); {
	// wasmparser's wording for these two reads from the other side; kept,
	// since it is what a test of either asserts.
	case la == 1 && lb == 0:
		return fmt.Errorf("expected a result, found none")
	case la == 0 && lb == 1:
		return fmt.Errorf("expected no result, found one")
	case la != lb:
		return fmt.Errorf("expected %d results, found %d", lb, la)
	}
	for i := range a.results {
		if an, bn := a.results[i].name, b.results[i].name; an != bn {
			return fmt.Errorf("expected result named `%s`, found `%s`", bn, an)
		}
		if err := ctValSub(a.results[i].t, b.results[i].t); err != nil {
			return fmt.Errorf("type mismatch with result type: %w", err)
		}
	}
	return nil
}

func syncDesc(async bool) string {
	if async {
		return "async"
	}
	return "sync"
}

// ctInstanceSub reports whether instance type a is a subtype of b: a
// exports every name b does, each a subtype of b's.
func ctInstanceSub(a, b *ctInstance) error {
	for _, be := range b.exports {
		ae, ok := ctLookup(a.exports, be.name)
		if !ok {
			return fmt.Errorf("missing expected export `%s`", be.name)
		}
		if err := ctEntitySub(ae, be.ent); err != nil {
			return fmt.Errorf("type mismatch in instance export `%s`: %w", be.name, err)
		}
	}
	return nil
}

// ctComponentSub reports whether component type a is a subtype of b.
//
// Someone holding what they believe is a b, but is really an a, supplies
// b's imports -- which must satisfy a's, the first ctOpen -- and receives
// a's exports, with whatever resources they supplied substituted in, which
// must satisfy b's: the second ctOpen. That one lines b's defined resources
// up with the ones a really exports. This is wasmparser's
// SubtypeCx::component_type.
func ctComponentSub(a, b *ctComponent) error {
	s, err := ctOpen(b.imports, a, true)
	if err != nil {
		return err
	}
	_, err = ctOpen(s.named(a.exports), b, false)
	return err
}

// ctOpen checks that the items in args satisfy the imports of component
// type c (imports true) or its exports, and returns the substitution from
// c's imported (or defined) resources to the resources args supplies for
// them. Instantiating c with args is exactly ctOpen(args, c, true).
func ctOpen(args []ctNamed, c *ctComponent, imports bool) (ctSubst, error) {
	entities, resources, kind := c.exports, c.definedRes, "export"
	if imports {
		entities, resources, kind = c.imports, c.importedRes, "import"
	}
	// First line each of c's abstract resources up with the one args puts
	// at the same path. An arg of the wrong shape maps nothing: the check
	// below then fails on it, with the better message.
	s := make(ctSubst, len(resources))
	for _, rp := range resources {
		arg, ok := ctLookup(args, rp.path[0])
		for _, name := range rp.path[1:] {
			if !ok || arg.sort != ctSortInstance {
				ok = false
				break
			}
			arg, ok = ctLookup(arg.inst.exports, name)
		}
		if ok && arg.sort == ctSortType && arg.ty.res != nil {
			s[rp.res] = arg.ty.res
		}
	}
	for _, want := range entities {
		got, ok := ctLookup(args, want.name)
		if !ok {
			return nil, fmt.Errorf("missing %s named `%s`", kind, want.name)
		}
		if err := ctEntitySub(got, s.entity(want.ent)); err != nil {
			return nil, fmt.Errorf("type mismatch for %s `%s`: %w", kind, want.name, err)
		}
	}
	return s, nil
}

// ctValSub reports whether value type a is a subtype of b: equality, as in
// wasmparser, with its messages.
func ctValSub(a, b *ctVal) error {
	if a.kind != b.kind {
		switch {
		case a.kind == ctvPrim:
			return fmt.Errorf("expected %s, found %s", b.kind.desc(), a.prim)
		case b.kind == ctvPrim && !b.defined:
			return fmt.Errorf("expected %s, found %s", b.prim, ctFoundDesc(a.kind))
		}
		return fmt.Errorf("expected %s, found %s", b.kind.desc(), ctFoundDesc(a.kind))
	}
	switch a.kind {
	case ctvPrim:
		if a.prim != b.prim {
			return fmt.Errorf("expected primitive `%s` found primitive `%s`", b.prim, a.prim)
		}
	case ctvRecord:
		if len(a.labels) != len(b.labels) {
			return fmt.Errorf("expected %d fields, found %d", len(b.labels), len(a.labels))
		}
		for i, an := range a.labels {
			if bn := b.labels[i]; an != bn {
				return fmt.Errorf("expected field name `%s`, found `%s`", bn, an)
			}
			if err := ctValSub(a.elems[i], b.elems[i]); err != nil {
				return fmt.Errorf("type mismatch in record field `%s`: %w", an, err)
			}
		}
	case ctvVariant:
		if len(a.labels) != len(b.labels) {
			return fmt.Errorf("expected %d cases, found %d", len(b.labels), len(a.labels))
		}
		for i, an := range a.labels {
			if bn := b.labels[i]; an != bn {
				return fmt.Errorf("expected case named `%s`, found `%s`", bn, an)
			}
			switch at, bt := a.elems[i], b.elems[i]; {
			case at != nil && bt != nil:
				if err := ctValSub(at, bt); err != nil {
					return fmt.Errorf("type mismatch in variant case `%s`: %w", an, err)
				}
			case at == nil && bt != nil:
				return fmt.Errorf("expected case `%s` to have a type, found none", an)
			case at != nil && bt == nil:
				return fmt.Errorf("expected case `%s` to have no type", an)
			}
		}
	case ctvList, ctvOption:
		return ctValSub(a.elems[0], b.elems[0])
	case ctvFixedList:
		if a.length != b.length {
			return fmt.Errorf("expected fixed-length %d, found size %d", b.length, a.length)
		}
		return ctValSub(a.elems[0], b.elems[0])
	case ctvMap:
		if err := ctValSub(a.elems[0], b.elems[0]); err != nil {
			return fmt.Errorf("type mismatch in map key: %w", err)
		}
		if err := ctValSub(a.elems[1], b.elems[1]); err != nil {
			return fmt.Errorf("type mismatch in map value: %w", err)
		}
	case ctvTuple:
		if len(a.elems) != len(b.elems) {
			return fmt.Errorf("expected %d types, found %d", len(b.elems), len(a.elems))
		}
		for i := range a.elems {
			if err := ctValSub(a.elems[i], b.elems[i]); err != nil {
				return fmt.Errorf("type mismatch in tuple field %d: %w", i, err)
			}
		}
	case ctvFlags, ctvEnum:
		if !slices.Equal(a.labels, b.labels) {
			return fmt.Errorf("mismatch in %s elements", a.kind.desc())
		}
	case ctvResult:
		for i, what := range [2]string{"ok", "err"} {
			switch at, bt := a.elems[i], b.elems[i]; {
			case at != nil && bt != nil:
				if err := ctValSub(at, bt); err != nil {
					return fmt.Errorf("type mismatch in %s variant: %w", what, err)
				}
			case at == nil && bt != nil:
				return fmt.Errorf("expected %s type, but found none", what)
			case at != nil && bt == nil:
				return fmt.Errorf("expected %s type to not be present", what)
			}
		}
	case ctvOwn, ctvBorrow:
		if a.res != b.res {
			return fmt.Errorf("resource types are not the same (%s vs. %s)", ctResName(a.res), ctResName(b.res))
		}
	case ctvStream, ctvFuture:
		what := a.kind.desc()
		switch at, bt := a.elems[0], b.elems[0]; {
		case at != nil && bt != nil:
			if err := ctValSub(at, bt); err != nil {
				return fmt.Errorf("type mismatch in %s: %w", what, err)
			}
		case at == nil && bt != nil:
			return fmt.Errorf("expected %s type, but found none", what)
		case at != nil && bt == nil:
			return fmt.Errorf("expected %s type to not be present", what)
		}
	}
	return nil
}

// ctFoundDesc names a's constructor on the "found" side of a mismatch the
// way wasmparser does, which calls a fixed-length list a list there.
func ctFoundDesc(k ctValKind) string {
	if k == ctvFixedList {
		return "list"
	}
	return k.desc()
}
