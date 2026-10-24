package instance

import (
	"fmt"
	"slices"
	"sync"

	"github.com/samyfodil/wazy/internal/component/binary"
)

// This file computes the component-level types of ctypes.go from a decoded
// component: the type of each item in each of its index spaces, of each
// instantiation it performs -- which is where an instantiation's arguments
// are checked against what the instantiated component imports -- and of the
// component as a whole, its imports and exports, which is what a component
// supplied for a component import is checked against the import's declared
// type with.

// ctScope is where a type definition's indices resolve: a component (ctEnv)
// or the body of a component or instance type (ctDeclScope).
type ctScope interface {
	typeAt(idx uint32) (ctType, error)
	// enclosing is the scope an `alias outer 1` reaches; nil at the top.
	enclosing() ctScope
	// newResource is a `(resource ...)` definition, which only a component
	// can make.
	newResource() (*ctResource, error)
	// coreTypeAt is core type idx: the core type index space is a scope's
	// too, the one a core module type's outer alias reaches.
	coreTypeAt(idx uint32) (ctCoreType, error)
}

// outerScope walks count scopes out, for an `alias outer count idx`, and
// reports whether that left a component -- rather than only the body of a
// type declared inside one.
func outerScope(s ctScope, count uint32) (_ ctScope, leftComponent bool, _ error) {
	for i := uint32(0); i < count; i++ {
		if _, ok := s.(*ctEnv); ok {
			leftComponent = true
		}
		if s = s.enclosing(); s == nil {
			return nil, false, fmt.Errorf("invalid outer alias count of %d", count)
		}
	}
	return s, leftComponent, nil
}

// outerType is the type an `alias outer count idx (type)` names. A resource
// type is generative -- each instance of the component that defines or
// imports it has its own -- so a component cannot alias one in from an
// enclosing component, nor any type mentioning one.
func outerType(s ctScope, count, idx uint32) (ctType, error) {
	outer, left, err := outerScope(s, count)
	if err != nil {
		return ctType{}, err
	}
	t, err := outer.typeAt(idx)
	if err != nil {
		return ctType{}, err
	}
	if left {
		free := map[*ctResource]bool{}
		ctFreeResources(ctEntity{sort: ctSortType, ty: t}, map[*ctResource]bool{}, free)
		if len(free) > 0 {
			return ctType{}, fmt.Errorf("type index %d refers to resources not defined in the current component", idx)
		}
	}
	return t, nil
}

// ctValRef resolves a valtype.
func ctValRef(s ctScope, ref binary.TypeRef) (*ctVal, error) {
	if ref.Primitive != "" {
		return ctPrim(ref.Primitive, false), nil
	}
	if ref.TypeIndex == nil {
		return nil, fmt.Errorf("value type names neither a primitive nor a type index")
	}
	t, err := s.typeAt(*ref.TypeIndex)
	if err != nil {
		return nil, err
	}
	if t.val == nil {
		return nil, fmt.Errorf("type index %d is a %s, not a defined value type", *ref.TypeIndex, t.desc())
	}
	return t.val, nil
}

func ctResourceRef(s ctScope, idx uint32) (*ctResource, error) {
	t, err := s.typeAt(idx)
	if err != nil {
		return nil, err
	}
	if t.res == nil {
		return nil, fmt.Errorf("type index %d is not a resource type", idx)
	}
	return t.res, nil
}

// ctConvert computes the type a type definition denotes in scope s.
func ctConvert(s ctScope, d binary.TypeDesc) (ctType, error) {
	switch d := d.(type) {
	case binary.FuncDesc:
		f, err := ctConvertFunc(s, d)
		return ctType{fn: f}, err
	case binary.InstanceDesc:
		inst, _, err := ctBuildDeclType(s, d.Decls, false)
		return ctType{inst: inst}, err
	case binary.ComponentDesc:
		_, comp, err := ctBuildDeclType(s, d.Decls, true)
		return ctType{comp: comp}, err
	case binary.ResourceDesc:
		r, err := s.newResource()
		return ctType{res: r}, err
	case nil:
		return ctType{}, fmt.Errorf("type definition was not decoded")
	}
	v, err := ctConvertVal(s, d)
	return ctType{val: v}, err
}

func ctConvertFunc(s ctScope, d binary.FuncDesc) (*ctFunc, error) {
	nr := len(d.Results.Named)
	if d.Results.Unnamed != nil {
		nr++
	}
	f := ctNewFunc(d.Async, len(d.Params), nr)
	for i, p := range d.Params {
		t, err := ctValRef(s, p.Type)
		if err != nil {
			return nil, fmt.Errorf("parameter `%s`: %w", p.Name, err)
		}
		f.params[i] = ctField{name: p.Name, t: t}
	}
	if d.Results.Unnamed != nil {
		t, err := ctValRef(s, *d.Results.Unnamed)
		if err != nil {
			return nil, fmt.Errorf("result: %w", err)
		}
		f.results[0] = ctField{t: t}
	}
	named := f.results[nr-len(d.Results.Named):]
	for i, r := range d.Results.Named {
		t, err := ctValRef(s, r.Type)
		if err != nil {
			return nil, fmt.Errorf("result `%s`: %w", r.Name, err)
		}
		named[i] = ctField{name: r.Name, t: t}
	}
	for _, r := range f.results {
		if ctHasBorrow(r.t) {
			// A borrow lives only as long as the call that lends it, so a
			// result -- which outlives the call -- cannot hold one.
			return nil, fmt.Errorf("function result cannot contain a `borrow` type")
		}
	}
	return f, nil
}

func ctHasBorrow(v *ctVal) bool {
	if v == nil {
		return false
	}
	if v.kind == ctvBorrow {
		return true
	}
	for _, el := range v.elems {
		if ctHasBorrow(el) {
			return true
		}
	}
	return false
}

// ctConvertVal computes the value type a defvaltype denotes in scope s.
func ctConvertVal(s ctScope, d binary.TypeDesc) (*ctVal, error) {
	var err error
	ref := func(r binary.TypeRef) *ctVal {
		if err != nil {
			return nil
		}
		var v *ctVal
		v, err = ctValRef(s, r)
		return v
	}
	optRef := func(r *binary.TypeRef) *ctVal {
		if r == nil {
			return nil
		}
		return ref(*r)
	}
	var v *ctVal
	switch d := d.(type) {
	case binary.PrimitiveDesc:
		return ctPrim(d.Prim, true), nil
	case binary.RecordDesc:
		v = ctValOf(ctvRecord, len(d.Fields))
		v.labels = make([]string, len(d.Fields))
		for i, f := range d.Fields {
			v.labels[i], v.elems[i] = f.Name, ref(f.Type)
		}
	case binary.VariantDesc:
		v = ctValOf(ctvVariant, len(d.Cases))
		v.labels = make([]string, len(d.Cases))
		for i, c := range d.Cases {
			v.labels[i], v.elems[i] = c.Name, optRef(c.Type)
		}
	case binary.ListDesc:
		v = ctValOf(ctvList, 1)
		v.elems[0] = ref(d.Element)
	case binary.FixedListDesc:
		v = ctValOf(ctvFixedList, 1)
		v.elems[0], v.length = ref(d.Element), d.Length
	case binary.TupleDesc:
		v = ctValOf(ctvTuple, len(d.Elements))
		for i, e := range d.Elements {
			v.elems[i] = ref(e)
		}
	case binary.FlagsDesc:
		v = &ctVal{kind: ctvFlags, labels: d.Names}
	case binary.EnumDesc:
		v = &ctVal{kind: ctvEnum, labels: d.Cases}
	case binary.OptionDesc:
		v = ctValOf(ctvOption, 1)
		v.elems[0] = ref(d.Element)
	case binary.ResultDesc:
		v = ctValOf(ctvResult, 2)
		v.elems[0], v.elems[1] = optRef(d.Ok), optRef(d.Err)
	case binary.MapDesc:
		v = ctValOf(ctvMap, 2)
		v.elems[0], v.elems[1] = ref(d.Key), ref(d.Value)
	case binary.StreamDesc:
		v = ctValOf(ctvStream, 1)
		v.elems[0] = optRef(d.Element)
	case binary.FutureDesc:
		v = ctValOf(ctvFuture, 1)
		v.elems[0] = optRef(d.Element)
	case binary.OwnDesc:
		v = &ctVal{kind: ctvOwn}
		v.res, err = ctResourceRef(s, d.ResourceType)
	case binary.BorrowDesc:
		v = &ctVal{kind: ctvBorrow}
		v.res, err = ctResourceRef(s, d.ResourceType)
	default:
		return nil, fmt.Errorf("unsupported type definition %s", d.Kind())
	}
	if err != nil {
		return nil, err
	}
	return v, nil
}

// ctDeclScope is the body of a component or instance type: it opens type and
// instance index spaces of its own, which its declarations fill.
type ctDeclScope struct {
	up        ctScope
	types     []ctType
	insts     []*ctInstance
	coreTypes []ctCoreType
}

// ctDeclScopes keeps scopes between the types built in them: the type a
// body declares is built of what its declarations name, never of the scope.
var ctDeclScopes = sync.Pool{New: func() any { return new(ctDeclScope) }}

func (s *ctDeclScope) release() {
	*s = ctDeclScope{types: ctEmpty(s.types), insts: ctEmpty(s.insts), coreTypes: ctEmpty(s.coreTypes)}
	ctDeclScopes.Put(s)
}

func (s *ctDeclScope) coreTypeAt(idx uint32) (ctCoreType, error) {
	if int(idx) >= len(s.coreTypes) {
		return ctCoreType{}, fmt.Errorf("unknown core type %d: type index out of bounds", idx)
	}
	return s.coreTypes[idx], nil
}

func (s *ctDeclScope) typeAt(idx uint32) (ctType, error) {
	if int(idx) >= len(s.types) {
		return ctType{}, fmt.Errorf("unknown type %d: type index out of bounds", idx)
	}
	return s.types[idx], nil
}

func (s *ctDeclScope) enclosing() ctScope { return s.up }

func (s *ctDeclScope) newResource() (*ctResource, error) {
	return nil, fmt.Errorf("resources can only be defined within a concrete component")
}

// ctEntityOfDesc is the type an externdesc names in scope s; one of a `(sub
// resource)` is a new resource each time, which ctDeclared makes.
func ctEntityOfDesc(s ctScope, d binary.ExternDesc) (ctEntity, error) {
	e := ctEntity{sort: d.Sort}
	switch d.Sort {
	case ctSortCoreModule:
		t, err := s.coreTypeAt(d.Index)
		if err == nil && t.mod == nil {
			err = fmt.Errorf("core type index %d is not a module type", d.Index)
		}
		e.mod = t.mod
		return e, err
	case ctSortType: // an `eq` bound: ctDeclared makes a `sub resource`
		t, err := s.typeAt(d.Index)
		e.ty = t
		return e, err
	}
	t, err := s.typeAt(d.Index)
	if err != nil {
		return e, err
	}
	switch d.Sort {
	case ctSortFunc:
		e.fn = t.fn
	case ctSortComponent:
		e.comp = t.comp
	case ctSortInstance:
		e.inst = t.inst
	default:
		return e, fmt.Errorf("unsupported %s import or export", ctSortDesc(d.Sort))
	}
	if e.fn == nil && e.comp == nil && e.inst == nil {
		return e, fmt.Errorf("type index %d is not %s type", d.Index, map[byte]string{ctSortFunc: "a function", ctSortComponent: "a component", ctSortInstance: "an instance"}[d.Sort])
	}
	return e, nil
}

// ctDeclared is the type an import or export declares, with the resources
// it introduces -- a new one for `(sub resource)`, its own copies of those
// of an instance type -- located by paths starting with its name.
func ctDeclared(s ctScope, name string, d binary.ExternDesc) (ctEntity, []ctResPath, error) {
	if d.Sort == ctSortType && !d.Eq {
		r := newSubResource(name)
		return ctEntity{sort: ctSortType, ty: ctType{res: r.path[0].res}}, r.path[:], nil
	}
	e, err := ctEntityOfDesc(s, d)
	if err != nil {
		return e, nil, err
	}
	e, paths := ctFreshen(e, name)
	return e, paths, nil
}

// subResource is the resource a `(sub resource)` import or export named name
// introduces, and the path to it, made in one allocation.
type subResource struct {
	res  ctResource
	path [1]ctResPath
	name [1]string
}

func newSubResource(name string) *subResource {
	r := &subResource{res: ctResource{name: name}, name: [1]string{name}}
	r.path[0] = ctResPath{res: &r.res, path: r.name[:]}
	return r
}

// ctBuildDeclType builds the instance type (component false) or component
// type a list of declarations declares, in scope up.
func ctBuildDeclType(up ctScope, decls []binary.TypeDecl, component bool) (*ctInstance, *ctComponent, error) {
	// Sized once from the declarations, rather than grown one append at a
	// time: every instance type a component imports goes through here.
	var nImports, nExports, nTypes int
	for i := range decls {
		switch decls[i].Kind {
		case binary.TypeDeclImport:
			nImports++
		case binary.TypeDeclExport:
			nExports++
		}
		if decls[i].Kind != binary.TypeDeclCoreType {
			nTypes++
		}
	}
	s := ctDeclScopes.Get().(*ctDeclScope)
	defer s.release()
	s.up, s.types = up, slices.Grow(s.types, nTypes)
	imports, exports := make([]ctNamed, 0, nImports), make([]ctNamed, 0, nExports)
	var importedRes, definedRes []ctResPath
	for i, d := range decls {
		var err error
		switch d.Kind {
		case binary.TypeDeclCoreType:
			// The next index of the body's own core type space, which its
			// core module imports and exports name.
			if mt := d.CoreType.Module; mt != nil {
				var mod *ctModule
				if mod, err = ctModuleOfType(s, mt); err == nil {
					s.coreTypes = append(s.coreTypes, ctCoreType{mod: mod})
				}
				break
			}
			for _, f := range ctCoreRec(d.CoreType.Rec) {
				s.coreTypes = append(s.coreTypes, ctCoreType{fn: f})
			}
		case binary.TypeDeclType:
			var t ctType
			if t, err = ctConvert(s, d.Type); err == nil {
				s.types = append(s.types, t)
			}
		case binary.TypeDeclAlias:
			err = s.alias(d.Alias)
		case binary.TypeDeclImport, binary.TypeDeclExport:
			list, kind := &exports, "export"
			if d.Kind == binary.TypeDeclImport {
				list, kind = &imports, "import"
			}
			if _, dup := ctLookup(*list, d.Name); dup {
				err = fmt.Errorf("%s name `%s` conflicts with previous name `%s`", kind, d.Name, d.Name)
				break
			}
			var e ctEntity
			var paths []ctResPath
			if e, paths, err = ctDeclared(s, d.Name, d.Desc); err != nil {
				break
			}
			*list = append(*list, ctNamed{name: d.Name, ent: e})
			if d.Kind == binary.TypeDeclImport {
				importedRes = append(importedRes, paths...)
			} else {
				definedRes = append(definedRes, paths...)
			}
			switch e.sort {
			case ctSortType:
				s.types = append(s.types, e.ty)
			case ctSortInstance:
				s.insts = append(s.insts, e.inst)
			}
		}
		if err != nil {
			return nil, nil, fmt.Errorf("declaration %d: %w", i, err)
		}
	}
	if component {
		return nil, &ctComponent{imports: imports, exports: exports, importedRes: importedRes, definedRes: definedRes}, nil
	}
	return &ctInstance{exports: exports, fresh: definedRes}, nil, nil
}

// alias adds what a type body's alias names to the body's index spaces. A
// body can alias a type or an instance export of one of its own instances,
// or a type of an enclosing scope -- nothing else has an index space here.
func (s *ctDeclScope) alias(al binary.AliasDef) error {
	switch {
	case al.TargetKind == 0x00 && (al.Sort == ctSortType || al.Sort == ctSortInstance):
		if int(al.InstanceIdx) >= len(s.insts) {
			return fmt.Errorf("unknown instance %d: instance index out of bounds", al.InstanceIdx)
		}
		e, ok := ctLookup(s.insts[al.InstanceIdx].exports, al.Name)
		if !ok {
			return fmt.Errorf("instance %d has no export named `%s`", al.InstanceIdx, al.Name)
		}
		if e.sort != al.Sort {
			return fmt.Errorf("export `%s` for instance %d is not a %s", al.Name, al.InstanceIdx, ctSortDesc(al.Sort))
		}
		if e.sort == ctSortType {
			s.types = append(s.types, e.ty)
		} else {
			s.insts = append(s.insts, e.inst)
		}
		return nil
	case al.TargetKind == 0x02 && al.Sort == ctSortType:
		t, err := outerType(s, al.OuterCount, al.OuterIndex)
		if err != nil {
			return err
		}
		s.types = append(s.types, t)
		return nil
	case al.TargetKind == 0x02 && al.Sort == 0x00 && al.CoreSort == 0x10: // core type
		outer, _, err := outerScope(s, al.OuterCount)
		if err != nil {
			return err
		}
		t, err := outer.coreTypeAt(al.OuterIndex)
		if err != nil {
			return err
		}
		s.coreTypes = append(s.coreTypes, t)
		return nil
	}
	return fmt.Errorf("aliases in a component or instance type may only refer to types or instances")
}

// Memo states of one index-space slot of a ctEnv.
const (
	ctUnseen byte = iota
	ctPending
	ctDone
)

// ctEnv computes the types of one decoded component's items. Every slot is
// computed at most once, so the resources a slot introduces have one
// identity however many times later definitions refer to it.
type ctEnv struct {
	comp *binary.Component
	up   *ctEnv

	types    []ctType
	typeSt   []byte
	funcs    []*ctFunc
	funcSt   []byte
	insts    []*ctInstance
	instSt   []byte
	comps    []*ctComponent
	compSt   []byte
	imports  []ctEntity
	importSt []byte
	exports  []ctEntity
	exportSt []byte

	// importedRes are the resources the imports introduce, by import index;
	// empty until an import introduces one.
	importedRes [][]ctResPath

	// created are the resources this component makes: its resource
	// definitions, each instantiation's fresh copies of the resources the
	// instantiated component defines, and an export's abstraction of one.
	// Those its exports reach are what the component type defines. local
	// are its resource definitions alone, the only resources whose
	// representation it may see (canon resource.new and resource.rep).
	created map[*ctResource]bool
	local   map[*ctResource]bool

	children []*ctEnv // by nested component, made on first use
	self     *ctComponent

	// core holds the core index spaces (ctenv_core.go), allocated when one
	// is first asked for: most components never are.
	core *ctCoreSpaces

	// st and ents back the memo states and the import and export entities.
	st   []byte
	ents []ctEntity
}

// ctEnvs keeps envs between type checks. Nothing a check computes outlives
// it but its verdict, so once a check is over its envs -- and every slice
// they hold, sized for the components checked so far -- serve the next one
// (see release).
var ctEnvs = sync.Pool{New: func() any { return new(ctEnv) }}

func newCTEnv(comp *binary.Component, up *ctEnv) *ctEnv {
	nT, nF, nI, nC := len(comp.TypeSpace), len(comp.ComponentFuncSpace), len(comp.ComponentInstanceSpace), len(comp.ComponentSpace)
	nIm, nEx := len(comp.Imports), len(comp.Exports)
	e := ctEnvs.Get().(*ctEnv)
	e.comp, e.up = comp, up
	// One slice for every memo state, cut into one per space.
	e.st = ctResize(e.st, nT+nF+nI+nC+nIm+nEx)
	st := e.st
	cut := func(n int) []byte {
		b := st[:n:n]
		st = st[n:]
		return b
	}
	e.types, e.typeSt = ctResize(e.types, nT), cut(nT)
	e.funcs, e.funcSt = ctResize(e.funcs, nF), cut(nF)
	e.insts, e.instSt = ctResize(e.insts, nI), cut(nI)
	e.comps, e.compSt = ctResize(e.comps, nC), cut(nC)
	// And one for the entities of both the imports and the exports.
	e.ents = ctResize(e.ents, nIm+nEx)
	e.imports, e.importSt = e.ents[:nIm:nIm], cut(nIm)
	e.exports, e.exportSt = e.ents[nIm:], cut(nEx)
	return e
}

// release returns e, and the envs of the components nested in it, to
// ctEnvs, once the check they served is over. What they hold is emptied, so
// it keeps nothing of the check alive.
func (e *ctEnv) release() {
	for _, ch := range e.children {
		if ch != nil {
			ch.release()
		}
	}
	clear(e.created)
	clear(e.local)
	*e = ctEnv{
		types: ctEmpty(e.types), funcs: ctEmpty(e.funcs), insts: ctEmpty(e.insts), comps: ctEmpty(e.comps),
		importedRes: ctEmpty(e.importedRes), children: ctEmpty(e.children),
		created: e.created, local: e.local, st: ctEmpty(e.st), ents: ctEmpty(e.ents),
	}
	ctEnvs.Put(e)
}

// ctEmpty is s with no elements, and zero in all it has room for.
func ctEmpty[T any](s []T) []T {
	clear(s[:cap(s)])
	return s[:0]
}

// ctResize is s, which ctEmpty emptied, at length n.
func ctResize[T any](s []T, n int) []T {
	if cap(s) < n {
		return make([]T, n)
	}
	return s[:n]
}

// create records r as made by this component (and, when local, defined
// here); the sets are allocated by the first resource a component makes.
func (e *ctEnv) create(r *ctResource, local bool) {
	if e.created == nil {
		e.created = map[*ctResource]bool{}
	}
	e.created[r] = true
	if local {
		if e.local == nil {
			e.local = map[*ctResource]bool{}
		}
		e.local[r] = true
	}
}

func (e *ctEnv) enclosing() ctScope {
	if e.up == nil {
		return nil
	}
	return e.up
}

func (e *ctEnv) newResource() (*ctResource, error) {
	r := &ctResource{}
	e.create(r, true)
	return r, nil
}

// enter guards one slot's computation: a slot that is reached again while
// it is being computed names itself, which no well-formed component does.
func ctEnter(st []byte, idx uint32, what string) (done bool, err error) {
	if int(idx) >= len(st) {
		return false, fmt.Errorf("unknown %s %d: %s index out of bounds", what, idx, what)
	}
	switch st[idx] {
	case ctDone:
		return true, nil
	case ctPending:
		return false, fmt.Errorf("unknown %s %d: %s index out of bounds (it names the definition it is part of)", what, idx, what)
	}
	st[idx] = ctPending
	return false, nil
}

// leave ends a slot's computation; a failed one may be retried, and fails
// the same way.
func ctLeave(st []byte, idx uint32, err error) {
	if err == nil {
		st[idx] = ctDone
	} else {
		st[idx] = ctUnseen
	}
}

func (e *ctEnv) typeAt(idx uint32) (t ctType, err error) {
	if done, err := ctEnter(e.typeSt, idx, "type"); err != nil {
		return ctType{}, err
	} else if done {
		return e.types[idx], nil
	}
	defer func() { ctLeave(e.typeSt, idx, err); e.types[idx] = t }()
	c := e.comp
	switch en := c.TypeSpace[idx]; en.Kind {
	case binary.TypeSpaceDef:
		return ctConvert(e, c.Types[en.Def].Descriptor)
	case binary.TypeSpaceImport:
		ent, err := e.importAt(en.Import)
		return ent.ty, err
	case binary.TypeSpaceExport:
		ent, err := e.exportAt(en.Export)
		return ent.ty, err
	default: // binary.TypeSpaceAlias
		al := c.Aliases[en.Alias]
		if al.TargetKind == 0x02 {
			return outerType(e, al.OuterCount, al.OuterIndex)
		}
		ent, err := e.aliasedExport(al)
		return ent.ty, err
	}
}

// aliasedExport is the export an `alias export` names.
func (e *ctEnv) aliasedExport(al binary.AliasDef) (ctEntity, error) {
	if al.TargetKind != 0x00 {
		return ctEntity{}, fmt.Errorf("a %s alias must name an instance export", ctSortDesc(al.Sort))
	}
	inst, err := e.instanceAt(al.InstanceIdx)
	if err != nil {
		return ctEntity{}, err
	}
	ent, ok := ctLookup(inst.exports, al.Name)
	if !ok {
		return ctEntity{}, fmt.Errorf("instance %d has no export named `%s`", al.InstanceIdx, al.Name)
	}
	if ent.sort != al.Sort {
		return ctEntity{}, fmt.Errorf("export `%s` for instance %d is not a %s", al.Name, al.InstanceIdx, ctSortDesc(al.Sort))
	}
	return ent, nil
}

func (e *ctEnv) funcAt(idx uint32) (f *ctFunc, err error) {
	if done, err := ctEnter(e.funcSt, idx, "function"); err != nil {
		return nil, err
	} else if done {
		return e.funcs[idx], nil
	}
	defer func() { ctLeave(e.funcSt, idx, err); e.funcs[idx] = f }()
	c := e.comp
	var ent ctEntity
	switch en := c.ComponentFuncSpace[idx]; en.Kind {
	case binary.ComponentFuncFromCanonLift:
		ti := c.Canons[en.Canon].TypeIdx
		t, err := e.typeAt(ti)
		if err != nil {
			return nil, err
		}
		if t.fn == nil {
			return nil, fmt.Errorf("type index %d is not a function type", ti)
		}
		return t.fn, nil
	case binary.ComponentFuncFromImport:
		ent, err = e.importAt(en.Import)
	case binary.ComponentFuncFromExport:
		ent, err = e.exportAt(en.Export)
	default: // binary.ComponentFuncFromAlias
		ent, err = e.aliasedExport(c.Aliases[en.Alias])
	}
	return ent.fn, err
}

func (e *ctEnv) instanceAt(idx uint32) (in *ctInstance, err error) {
	if done, err := ctEnter(e.instSt, idx, "instance"); err != nil {
		return nil, err
	} else if done {
		return e.insts[idx], nil
	}
	defer func() { ctLeave(e.instSt, idx, err); e.insts[idx] = in }()
	c := e.comp
	var ent ctEntity
	switch en := c.ComponentInstanceSpace[idx]; en.Kind {
	case binary.ComponentInstanceFromDefinition:
		in, err := e.instantiate(c.Instances[en.Instance])
		if err != nil {
			return nil, fmt.Errorf("instance %d: %w", idx, err)
		}
		return in, nil
	case binary.ComponentInstanceFromImport:
		ent, err = e.importAt(en.Import)
	case binary.ComponentInstanceFromExport:
		ent, err = e.exportAt(en.Export)
	default: // binary.ComponentInstanceFromAlias
		ent, err = e.aliasedExport(c.Aliases[en.Alias])
	}
	return ent.inst, err
}

func (e *ctEnv) componentAt(idx uint32) (ct *ctComponent, err error) {
	if done, err := ctEnter(e.compSt, idx, "component"); err != nil {
		return nil, err
	} else if done {
		return e.comps[idx], nil
	}
	defer func() { ctLeave(e.compSt, idx, err); e.comps[idx] = ct }()
	c := e.comp
	var ent ctEntity
	switch en := c.ComponentSpace[idx]; en.Kind {
	case binary.ComponentFromDefinition:
		return e.child(en.Component).selfType()
	case binary.ComponentFromImport:
		ent, err = e.importAt(en.Import)
	case binary.ComponentFromExport:
		ent, err = e.exportAt(en.Export)
	default: // binary.ComponentFromAlias
		al := c.Aliases[en.Alias]
		if al.TargetKind != 0x02 {
			ent, err = e.aliasedExport(al)
			break
		}
		var outer ctScope
		if outer, _, err = outerScope(e, al.OuterCount); err != nil {
			return nil, err
		}
		return outer.(*ctEnv).componentAt(al.OuterIndex)
	}
	return ent.comp, err
}

// child is the env of nested component definition i, whose outer aliases
// reach this one.
func (e *ctEnv) child(i uint32) *ctEnv {
	if len(e.children) == 0 {
		e.children = ctResize(e.children, len(e.comp.NestedComponents))
	}
	if e.children[i] == nil {
		e.children[i] = newCTEnv(e.comp.NestedComponents[i], e)
	}
	return e.children[i]
}

// entityAt is the type of item idx of the given sort.
func (e *ctEnv) entityAt(sort byte, idx uint32) (ctEntity, error) {
	ent := ctEntity{sort: sort}
	var err error
	switch sort {
	case ctSortCoreModule:
		ent.mod, err = e.moduleAt(idx)
	case ctSortFunc:
		ent.fn, err = e.funcAt(idx)
	case ctSortType:
		ent.ty, err = e.typeAt(idx)
	case ctSortComponent:
		ent.comp, err = e.componentAt(idx)
	case ctSortInstance:
		ent.inst, err = e.instanceAt(idx)
	default:
		err = fmt.Errorf("unsupported %s", ctSortDesc(sort))
	}
	return ent, err
}

func (e *ctEnv) importAt(i uint32) (ent ctEntity, err error) {
	if done, err := ctEnter(e.importSt, i, "import"); err != nil {
		return ctEntity{}, err
	} else if done {
		return e.imports[i], nil
	}
	defer func() { ctLeave(e.importSt, i, err); e.imports[i] = ent }()
	im := e.comp.Imports[i]
	d := binary.ExternDesc{Sort: im.ExternType, Index: im.ExternIndex}
	if im.ExternType == ctSortType {
		d.Index, d.Eq = im.TypeEqIndex, im.TypeEqBound
	}
	ent, paths, err := ctDeclared(e, im.Name, d)
	if err != nil {
		return ent, fmt.Errorf("import `%s`: %w", im.Name, err)
	}
	if len(paths) > 0 {
		if len(e.importedRes) == 0 {
			e.importedRes = ctResize(e.importedRes, len(e.imports))
		}
		e.importedRes[i] = paths
	}
	return ent, nil
}

func (e *ctEnv) exportAt(i uint32) (ent ctEntity, err error) {
	if done, err := ctEnter(e.exportSt, i, "export"); err != nil {
		return ctEntity{}, err
	} else if done {
		return e.exports[i], nil
	}
	defer func() { ctLeave(e.exportSt, i, err); e.exports[i] = ent }()
	ex := e.comp.Exports[i]
	if ent, err = e.entityAt(ex.ExternType, ex.ExternIndex); err != nil {
		return ent, fmt.Errorf("export `%s`: %w", ex.Name, err)
	}
	if ex.Ascribed == nil {
		return ent, nil
	}
	if ent, err = e.ascribe(ent, *ex.Ascribed, ex.Name); err != nil {
		return ent, fmt.Errorf("export `%s`: ascribed type of export is not compatible with item's type: %w", ex.Name, err)
	}
	return ent, nil
}

// ascribe checks actual against the type an export ascribes to it and
// returns the ascribed type, which is what the export then has. A resource
// the ascription introduces with `(sub resource)` is abstract: the item may
// be any resource there, and the export -- like any `(sub resource)` export
// -- is a new resource type, distinct from the one it was made from.
func (e *ctEnv) ascribe(actual ctEntity, d binary.ExternDesc, name string) (ctEntity, error) {
	asc, paths, err := ctDeclared(e, name, d)
	if err != nil {
		return asc, err
	}
	// Check actual against asc with asc's new resources taken to be the ones
	// actual has at the same places -- the check a component's export gets
	// against its declared type, which is what an ascription is.
	if _, err := ctOpen([]ctNamed{{name: name, ent: actual}}, &ctComponent{
		exports:    []ctNamed{{name: name, ent: asc}},
		definedRes: paths,
	}, false); err != nil {
		return asc, err
	}
	for _, p := range paths {
		e.create(p.res, false)
	}
	return asc, nil
}

// instantiate is the type of one instance definition: for `(instantiate c
// (with ...))`, c's exports with the resources the arguments supplied in
// place of c's imported ones and fresh resources for c's defined ones --
// once the arguments have been checked against c's imports.
func (e *ctEnv) instantiate(def binary.Instance) (*ctInstance, error) {
	if def.Kind == 0x01 { // inline exports: an instance of what it lists
		in := &ctInstance{}
		for _, x := range def.Exports {
			if _, dup := ctLookup(in.exports, x.Name); dup {
				return nil, fmt.Errorf("export name `%s` conflicts with previous name `%s`", x.Name, x.Name)
			}
			ent, err := e.entityAt(x.Sort, x.SortIdx)
			if err != nil {
				return nil, fmt.Errorf("export `%s`: %w", x.Name, err)
			}
			in.exports = append(in.exports, ctNamed{name: x.Name, ent: ent})
		}
		return in, nil
	}
	c, err := e.componentAt(def.ComponentIdx)
	if err != nil {
		return nil, err
	}
	args := make([]ctNamed, 0, len(def.Args))
	for _, a := range def.Args {
		if _, dup := ctLookup(args, a.Name); dup {
			return nil, fmt.Errorf("instantiation argument `%s` conflicts with previous argument `%s`", a.Name, a.Name)
		}
		ent, err := e.entityAt(a.Sort, a.SortIdx)
		if err != nil {
			return nil, fmt.Errorf("instantiation argument `%s`: %w", a.Name, err)
		}
		args = append(args, ctNamed{name: a.Name, ent: ent})
	}
	s, err := ctOpen(args, c, true)
	if err != nil {
		return nil, err
	}
	for _, rp := range c.definedRes {
		n := &ctResource{name: rp.res.name}
		if s == nil {
			s = make(ctSubst, len(c.definedRes))
		}
		s[rp.res] = n
		e.create(n, false)
	}
	return &ctInstance{exports: s.named(c.exports)}, nil
}

// selfType is the component type of the component itself.
func (e *ctEnv) selfType() (*ctComponent, error) {
	if e.self != nil {
		return e.self, nil
	}
	c := e.comp
	named := make([]ctNamed, len(c.Imports)+len(c.Exports))
	t := &ctComponent{imports: named[:len(c.Imports):len(c.Imports)], exports: named[len(c.Imports):]}
	for i, im := range c.Imports {
		ent, err := e.importAt(uint32(i))
		if err != nil {
			return nil, err
		}
		t.imports[i] = ctNamed{name: im.Name, ent: ent}
		if len(e.importedRes) > 0 {
			t.importedRes = append(t.importedRes, e.importedRes[i]...)
		}
	}
	for i, ex := range c.Exports {
		ent, err := e.exportAt(uint32(i))
		if err != nil {
			return nil, err
		}
		t.exports[i] = ctNamed{name: ex.Name, ent: ent}
	}
	// The resources the component defines are the ones it made that its
	// exports reach, each at the first place an export names it as a type.
	// One an export uses without exporting it could not be lined up with
	// anything, so -- as for wasmparser -- the component is malformed.
	if len(e.created) == 0 {
		e.self = t
		return t, nil
	}
	seen := map[*ctResource]bool{}
	var walk func(list []ctNamed, prefix []string)
	walk = func(list []ctNamed, prefix []string) {
		for _, n := range list {
			p := append(prefix[:len(prefix):len(prefix)], n.name)
			switch {
			case n.ent.sort == ctSortType && n.ent.ty.res != nil:
				if r := n.ent.ty.res; e.created[r] && !seen[r] {
					seen[r] = true
					t.definedRes = append(t.definedRes, ctResPath{res: r, path: p})
				}
			case n.ent.sort == ctSortInstance:
				walk(n.ent.inst.exports, p)
			}
		}
	}
	walk(t.exports, nil)
	used := map[*ctResource]bool{}
	for _, n := range t.exports {
		ctFreeResources(n.ent, map[*ctResource]bool{}, used)
	}
	for r := range used {
		if e.created[r] && !seen[r] {
			return nil, fmt.Errorf("local resource type found in export but not exported itself")
		}
	}
	e.self = t
	return t, nil
}

// checkCoreIndices checks what the core-level definitions name by index and
// by name that the type check does not otherwise reach: each core
// instantiation's module and argument names, each inline core instance's
// export names and items, and each resource destructor. The core linker
// checks the types when it instantiates them.
func (e *ctEnv) checkCoreIndices() error {
	c := e.comp
	// The core table, memory and global index spaces hold only aliases.
	var tables, mems, globals uint32
	for _, al := range c.Aliases {
		if al.Sort == 0x00 && al.TargetKind == 0x01 {
			switch al.CoreSort {
			case 0x01:
				tables++
			case 0x02:
				mems++
			case 0x03:
				globals++
			}
		}
	}
	bound := [...]uint32{0x00: uint32(len(c.CoreFuncSpace)), 0x01: tables, 0x02: mems, 0x03: globals}
	what := [...]string{0x00: "function", 0x01: "table", 0x02: "memory", 0x03: "global"}
	modules := e.coreModuleCount()
	// One set of names, sized for the longest list and emptied for each.
	most := 0
	for _, ci := range c.CoreInstances {
		most = max(most, len(ci.Args)+len(ci.Exports))
	}
	names := make(map[string]bool, most)
	for k, ci := range c.CoreInstances {
		clear(names)
		if ci.Kind == 0x00 {
			if int(ci.ModuleIdx) >= modules {
				return fmt.Errorf("core instance %d: unknown module %d: module index out of bounds", k, ci.ModuleIdx)
			}
			for _, a := range ci.Args {
				if names[a.Name] {
					return fmt.Errorf("core instance %d: duplicate module instantiation argument named `%s`", k, a.Name)
				}
				names[a.Name] = true
				if int(a.InstanceIdx) >= k {
					return fmt.Errorf("core instance %d: unknown core instance %d: instance index out of bounds", k, a.InstanceIdx)
				}
			}
			continue
		}
		for _, x := range ci.Exports {
			if names[x.Name] {
				return fmt.Errorf("core instance %d: export name `%s` already defined", k, x.Name)
			}
			names[x.Name] = true
			if int(x.Sort) < len(bound) && x.CoreSortIdx >= bound[x.Sort] {
				return fmt.Errorf("core instance %d: export `%s`: unknown %s %d: %s index out of bounds", k, x.Name, what[x.Sort], x.CoreSortIdx, what[x.Sort])
			}
		}
	}
	for i, t := range c.Types {
		if r, ok := t.Descriptor.(binary.ResourceDesc); ok && r.Dtor != nil && *r.Dtor >= bound[0x00] {
			return fmt.Errorf("type %d: destructor: unknown function %d: function index out of bounds", i, *r.Dtor)
		}
	}
	return nil
}

func ctImportNames(c *binary.Component) []string {
	names := make([]string, len(c.Imports))
	for i, im := range c.Imports {
		names[i] = im.Name
	}
	return names
}

func ctExportNames(c *binary.Component) []string {
	names := make([]string, len(c.Exports))
	for i, ex := range c.Exports {
		names[i] = ex.Name
	}
	return names
}

// checkResourceCanon checks the type a resource built-in names: a resource
// for each, and one this component defines for resource.new and
// resource.rep, which see its representation.
func (e *ctEnv) checkResourceCanon(cn binary.Canon) error {
	switch cn.Kind {
	case binary.CanonKindResourceNew, binary.CanonKindResourceRep, binary.CanonKindResourceDrop, binary.CanonKindResourceDropAsync:
	default:
		return nil
	}
	t, err := e.typeAt(cn.TypeIdx)
	if err != nil {
		return err
	}
	if t.res == nil {
		return fmt.Errorf("type index %d is not a resource type", cn.TypeIdx)
	}
	if (cn.Kind == binary.CanonKindResourceNew || cn.Kind == binary.CanonKindResourceRep) && !e.local[t.res] {
		return fmt.Errorf("type index %d is not a local resource", cn.TypeIdx)
	}
	return nil
}

// ctFreeResources adds to set every resource entity e's type mentions but
// does not itself bind: the resources a component type's imports and exports
// introduce, and an instance type's fresh ones, are its own. bound collects
// those as the walk meets them; being unique to their type, they need never
// be removed from it.
func ctFreeResources(e ctEntity, bound, set map[*ctResource]bool) {
	switch e.sort {
	case ctSortFunc:
		ctFreeInFunc(e.fn, bound, set)
	case ctSortInstance:
		ctFreeInInstance(e.inst, bound, set)
	case ctSortComponent:
		ctFreeInComponent(e.comp, bound, set)
	case ctSortType:
		switch t := e.ty; {
		case t.res != nil:
			if !bound[t.res] {
				set[t.res] = true
			}
		case t.val != nil:
			ctFreeInVal(t.val, bound, set)
		case t.fn != nil:
			ctFreeInFunc(t.fn, bound, set)
		case t.inst != nil:
			ctFreeInInstance(t.inst, bound, set)
		case t.comp != nil:
			ctFreeInComponent(t.comp, bound, set)
		}
	}
}

func ctFreeInInstance(in *ctInstance, bound, set map[*ctResource]bool) {
	for _, p := range in.fresh {
		bound[p.res] = true
	}
	for _, n := range in.exports {
		ctFreeResources(n.ent, bound, set)
	}
}

func ctFreeInComponent(c *ctComponent, bound, set map[*ctResource]bool) {
	for _, p := range c.importedRes {
		bound[p.res] = true
	}
	for _, p := range c.definedRes {
		bound[p.res] = true
	}
	for _, n := range c.imports {
		ctFreeResources(n.ent, bound, set)
	}
	for _, n := range c.exports {
		ctFreeResources(n.ent, bound, set)
	}
}

func ctFreeInFunc(f *ctFunc, bound, set map[*ctResource]bool) {
	for _, p := range f.params {
		ctFreeInVal(p.t, bound, set)
	}
	for _, r := range f.results {
		ctFreeInVal(r.t, bound, set)
	}
}

func ctFreeInVal(v *ctVal, bound, set map[*ctResource]bool) {
	if v == nil {
		return
	}
	if v.res != nil && !bound[v.res] {
		set[v.res] = true
	}
	for _, el := range v.elems {
		ctFreeInVal(el, bound, set)
	}
}

// coreModuleCount bounds the core module index space: definitions, aliases,
// imports and exports alike (binary.CoreModuleSpace).
func (e *ctEnv) coreModuleCount() int { return len(e.comp.CoreModuleSpace) }

// check computes the type of every instance definition, alias and export of
// the component, and of every component nested in it, so every
// instantiation in the tree has had its arguments checked and every alias
// names an export that is there, of the sort it says.
func (e *ctEnv) check() error {
	c := e.comp
	for _, list := range [2][]string{ctImportNames(c), ctExportNames(c)} {
		for i, name := range list {
			for _, prev := range list[:i] {
				if prev == name {
					return fmt.Errorf("name `%s` conflicts with previous name `%s`", name, prev)
				}
			}
		}
	}
	for i := range c.Imports {
		if _, err := e.importAt(uint32(i)); err != nil {
			return err
		}
	}
	for i, en := range c.ComponentInstanceSpace {
		if en.Kind == binary.ComponentInstanceFromDefinition || en.Kind == binary.ComponentInstanceFromAlias {
			if _, err := e.instanceAt(uint32(i)); err != nil {
				return err
			}
		}
	}
	for i := range c.ComponentFuncSpace {
		if _, err := e.funcAt(uint32(i)); err != nil {
			return err
		}
	}
	for i, en := range c.ComponentSpace {
		if en.Kind == binary.ComponentFromAlias {
			if _, err := e.componentAt(uint32(i)); err != nil {
				return err
			}
		}
	}
	for i := range c.TypeSpace {
		if _, err := e.typeAt(uint32(i)); err != nil {
			return err
		}
	}
	for _, cn := range c.Canons {
		if err := e.checkResourceCanon(cn); err != nil {
			return err
		}
	}
	if err := e.checkCoreIndices(); err != nil {
		return err
	}
	if err := e.checkCoreInstances(); err != nil {
		return err
	}
	// A core module aliased out of a component instance's exports: wazy
	// runs no such component, but the alias still has to name a module.
	for _, al := range c.Aliases {
		if al.Sort == 0x00 && al.CoreSort == 0x11 && al.TargetKind == 0x00 {
			if _, err := e.aliasedExport(al); err != nil {
				return err
			}
		}
	}
	for i := range e.comp.Exports {
		if _, err := e.exportAt(uint32(i)); err != nil {
			return err
		}
	}
	for i := range e.comp.NestedComponents {
		if err := e.child(uint32(i)).check(); err != nil {
			return fmt.Errorf("nested component %d: %w", i, err)
		}
	}
	_, err := e.selfType()
	return err
}
