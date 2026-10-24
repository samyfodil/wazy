package instance

import (
	"strings"
	"testing"
)

// Hand-built component-level types: the subtype rules of ctypes.go one by
// one, including the ones no vendored suite reaches (map, stream and future
// element mismatches, async funcs, multiple named results).

func prim(p string) *ctVal { return &ctVal{kind: ctvPrim, prim: p} }

func requireSubErr(t *testing.T, err error, want string) {
	t.Helper()
	if want == "" {
		if err != nil {
			t.Fatalf("got %v, want a subtype", err)
		}
		return
	}
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("got %v, want an error containing %q", err, want)
	}
}

func TestCTValSub(t *testing.T) {
	r1, r2 := &ctResource{name: "r1"}, &ctResource{}
	list := func(k ctValKind, e *ctVal) *ctVal { return &ctVal{kind: k, elems: []*ctVal{e}} }
	for _, tc := range []struct {
		name string
		a, b *ctVal
		want string
	}{
		{"map key", &ctVal{kind: ctvMap, elems: []*ctVal{prim("u32"), prim("u8")}}, &ctVal{kind: ctvMap, elems: []*ctVal{prim("s32"), prim("u8")}}, "type mismatch in map key: expected primitive `s32` found primitive `u32`"},
		{"map value", &ctVal{kind: ctvMap, elems: []*ctVal{prim("u32"), prim("u8")}}, &ctVal{kind: ctvMap, elems: []*ctVal{prim("u32"), prim("s8")}}, "type mismatch in map value"},
		{"map equal", &ctVal{kind: ctvMap, elems: []*ctVal{prim("u32"), prim("u8")}}, &ctVal{kind: ctvMap, elems: []*ctVal{prim("u32"), prim("u8")}}, ""},
		{"stream element", list(ctvStream, prim("u8")), list(ctvStream, prim("u16")), "type mismatch in stream"},
		{"stream none vs some", list(ctvStream, nil), list(ctvStream, prim("u8")), "expected stream type, but found none"},
		{"future some vs none", list(ctvFuture, prim("u8")), list(ctvFuture, nil), "expected future type to not be present"},
		{"bare future", list(ctvFuture, nil), list(ctvFuture, nil), ""},
		{"fixed vs list", &ctVal{kind: ctvFixedList, length: 2, elems: []*ctVal{prim("u8")}}, list(ctvList, prim("u8")), "expected list, found list"},
		{"list vs fixed", list(ctvList, prim("u8")), &ctVal{kind: ctvFixedList, length: 2, elems: []*ctVal{prim("u8")}}, "expected fixed-length list, found list"},
		{"own resources", &ctVal{kind: ctvOwn, res: r1}, &ctVal{kind: ctvOwn, res: r2}, "resource types are not the same (`r1` vs. an unnamed resource)"},
		{"own vs borrow", &ctVal{kind: ctvBorrow, res: r1}, &ctVal{kind: ctvOwn, res: r1}, "expected own, found borrow"},
		{"inline primitive vs record", &ctVal{kind: ctvRecord}, prim("u32"), "expected u32, found record"},
		{"defined primitive vs record", &ctVal{kind: ctvRecord}, &ctVal{kind: ctvPrim, prim: "u32", defined: true}, "expected primitive, found record"},
	} {
		t.Run(tc.name, func(t *testing.T) { requireSubErr(t, ctValSub(tc.a, tc.b), tc.want) })
	}
}

func TestCTFuncSub(t *testing.T) {
	f := func(async bool, results ...ctField) *ctFunc { return &ctFunc{async: async, results: results} }
	for _, tc := range []struct {
		name string
		a, b *ctFunc
		want string
	}{
		{"async vs sync", f(true), f(false), "expected sync function, found async function"},
		{"sync vs async", f(false), f(true), "expected async function, found sync function"},
		{"a result where none is expected", f(false, ctField{t: prim("u8")}), f(false), "expected a result, found none"},
		{"no result where one is expected", f(false), f(false, ctField{t: prim("u8")}), "expected no result, found one"},
		{"result counts", f(false, ctField{"a", prim("u8")}, ctField{"b", prim("u8")}), f(false, ctField{"a", prim("u8")}, ctField{"b", prim("u8")}, ctField{"c", prim("u8")}), "expected 3 results, found 2"},
		{"result names", f(false, ctField{"a", prim("u8")}, ctField{"b", prim("u8")}), f(false, ctField{"a", prim("u8")}, ctField{"c", prim("u8")}), "expected result named `c`, found `b`"},
		{"named results", f(false, ctField{"a", prim("u8")}, ctField{"b", prim("u8")}), f(false, ctField{"a", prim("u8")}, ctField{"b", prim("u8")}), ""},
	} {
		t.Run(tc.name, func(t *testing.T) { requireSubErr(t, ctFuncSub(tc.a, tc.b), tc.want) })
	}
}

func TestCTTypeAndEntitySub(t *testing.T) {
	fn := &ctFunc{}
	inst := &ctInstance{exports: []ctNamed{{name: "f", ent: ctEntity{sort: ctSortFunc, fn: fn}}}}
	comp := &ctComponent{}
	for _, tc := range []struct {
		name string
		a, b ctType
		want string
	}{
		{"func types", ctType{fn: fn}, ctType{fn: &ctFunc{async: true}}, "expected async function"},
		{"instance types", ctType{inst: &ctInstance{}}, ctType{inst: inst}, "missing expected export `f`"},
		{"component types", ctType{comp: comp}, ctType{comp: &ctComponent{exports: inst.exports}}, "missing export named `f`"},
		{"func vs component", ctType{fn: fn}, ctType{comp: comp}, "expected component, found func"},
		{"instance vs func", ctType{inst: inst}, ctType{fn: fn}, "expected func, found instance"},
		{"component vs instance", ctType{comp: comp}, ctType{inst: inst}, "expected instance, found component"},
	} {
		t.Run(tc.name, func(t *testing.T) { requireSubErr(t, ctTypeSub(tc.a, tc.b), tc.want) })
	}
	// Entities of every sort, and the sorts' names in a mismatch.
	requireSubErr(t, ctEntitySub(ctEntity{sort: ctSortType, ty: ctType{val: prim("u8")}}, ctEntity{sort: ctSortValue}), "expected value, found type")
	requireSubErr(t, ctEntitySub(ctEntity{sort: ctSortType}, ctEntity{sort: 0x42}), "expected sort 0x42, found type")
	requireSubErr(t, ctEntitySub(ctEntity{sort: ctSortCoreModule, mod: &ctModule{}}, ctEntity{sort: ctSortCoreModule, mod: &ctModule{exports: []ctCoreExport{{name: "f"}}}}), "missing expected export `f`")
	requireSubErr(t, ctEntitySub(ctEntity{sort: ctSortComponent, comp: comp}, ctEntity{sort: ctSortComponent, comp: &ctComponent{imports: inst.exports}}), "")
}

// A component type is a subtype of another when it imports less and exports
// more -- each under a substitution of the other side's resources.
func TestCTComponentSubResources(t *testing.T) {
	// b: (component (import "r" (type (sub resource))) (export "s" (type
	// (sub resource))) (export "f" (func (param "x" (own r)) (result (own s)))))
	br, bs := &ctResource{name: "r"}, &ctResource{name: "s"}
	b := &ctComponent{
		imports: []ctNamed{{name: "r", ent: ctEntity{sort: ctSortType, ty: ctType{res: br}}}},
		exports: []ctNamed{
			{name: "s", ent: ctEntity{sort: ctSortType, ty: ctType{res: bs}}},
			{name: "f", ent: ctEntity{sort: ctSortFunc, fn: &ctFunc{params: []ctField{{"x", &ctVal{kind: ctvOwn, res: br}}}, results: []ctField{{t: &ctVal{kind: ctvOwn, res: bs}}}}}},
		},
		importedRes: []ctResPath{{res: br, path: []string{"r"}}},
		definedRes:  []ctResPath{{res: bs, path: []string{"s"}}},
	}
	// a has the same shape over resources of its own.
	ar, as := &ctResource{}, &ctResource{}
	a := &ctComponent{
		imports: []ctNamed{{name: "r", ent: ctEntity{sort: ctSortType, ty: ctType{res: ar}}}},
		exports: []ctNamed{
			{name: "s", ent: ctEntity{sort: ctSortType, ty: ctType{res: as}}},
			{name: "f", ent: ctEntity{sort: ctSortFunc, fn: &ctFunc{params: []ctField{{"x", &ctVal{kind: ctvOwn, res: ar}}}, results: []ctField{{t: &ctVal{kind: ctvOwn, res: as}}}}}},
			{name: "extra", ent: ctEntity{sort: ctSortType, ty: ctType{val: prim("u8")}}},
		},
		importedRes: []ctResPath{{res: ar, path: []string{"r"}}},
	}
	requireSubErr(t, ctComponentSub(a, b), "")
	// f returning the IMPORTED resource where b's returns the defined one.
	a.exports[1].ent.fn = &ctFunc{params: []ctField{{"x", &ctVal{kind: ctvOwn, res: ar}}}, results: []ctField{{t: &ctVal{kind: ctvOwn, res: ar}}}}
	requireSubErr(t, ctComponentSub(a, b), "type mismatch for export `f`: type mismatch with result type: resource types are not the same")
}

// ctOpen lines a resource up through instance exports, and skips a path
// whose argument is of the wrong shape -- the check that follows names the
// actual mismatch.
func TestCTOpenResourcePaths(t *testing.T) {
	br := &ctResource{name: "r"}
	c := &ctComponent{
		imports:     []ctNamed{{name: "i", ent: ctEntity{sort: ctSortInstance, inst: &ctInstance{exports: []ctNamed{{name: "r", ent: ctEntity{sort: ctSortType, ty: ctType{res: br}}}}}}}},
		importedRes: []ctResPath{{res: br, path: []string{"i", "r"}}},
	}
	ar := &ctResource{}
	s, err := ctOpen([]ctNamed{{name: "i", ent: ctEntity{sort: ctSortInstance, inst: &ctInstance{exports: []ctNamed{{name: "r", ent: ctEntity{sort: ctSortType, ty: ctType{res: ar}}}}}}}}, c, true)
	if err != nil || s[br] != ar {
		t.Fatalf("ctOpen = %v, %v; want r mapped to the argument's", s, err)
	}
	// The argument is a func, not an instance: no mapping, and the check
	// says why.
	_, err = ctOpen([]ctNamed{{name: "i", ent: ctEntity{sort: ctSortFunc, fn: &ctFunc{}}}}, c, true)
	requireSubErr(t, err, "type mismatch for import `i`: expected instance, found func")
}

// A substitution rewrites exactly what mentions a resource it replaces, and
// returns everything else as it was.
func TestCTSubst(t *testing.T) {
	r, n := &ctResource{}, &ctResource{}
	s := ctSubst{r: n}
	own := &ctVal{kind: ctvOwn, res: r}
	fn := &ctFunc{params: []ctField{{"x", own}}}
	plain := &ctFunc{params: []ctField{{"x", prim("u8")}}}
	inst := &ctInstance{exports: []ctNamed{{name: "f", ent: ctEntity{sort: ctSortFunc, fn: fn}}}}
	comp := &ctComponent{exports: inst.exports, importedRes: []ctResPath{{res: &ctResource{}}}}

	if got := s.fn(plain); got != plain {
		t.Error("a func mentioning nothing replaced was copied")
	}
	if got := s.fn(fn); got == fn || got.params[0].t.res != n {
		t.Error("a func mentioning r was not rewritten")
	}
	if got := (ctSubst{}).inst(inst); got != inst {
		t.Error("an empty substitution copied an instance")
	}
	if got := s.inst(&ctInstance{exports: []ctNamed{{name: "g", ent: ctEntity{sort: ctSortFunc, fn: plain}}}}); got.exports[0].ent.fn != plain {
		t.Error("an instance mentioning nothing replaced was rewritten")
	}
	got := s.comp(comp)
	if got == comp || got.exports[0].ent.fn.params[0].t.res != n || &got.importedRes[0] != &comp.importedRes[0] {
		t.Error("a component's exports were not rewritten, or its own resources were")
	}
	if got := s.comp(&ctComponent{exports: []ctNamed{{name: "g", ent: ctEntity{sort: ctSortFunc, fn: plain}}}}); got.exports[0].ent.fn != plain {
		t.Error("a component mentioning nothing replaced was rewritten")
	}
	if s.comp(nil) != nil || (ctSubst{}).comp(comp) != comp {
		t.Error("nil and empty substitutions must return their input")
	}
	for _, tc := range []struct {
		in   ctType
		want func(ctType) bool
	}{
		{ctType{res: r}, func(t ctType) bool { return t.res == n }},
		{ctType{res: n}, func(t ctType) bool { return t.res == n }},
		{ctType{fn: fn}, func(t ctType) bool { return t.fn.params[0].t.res == n }},
		{ctType{inst: inst}, func(t ctType) bool { return t.inst.exports[0].ent.fn.params[0].t.res == n }},
		{ctType{comp: comp}, func(t ctType) bool { return t.comp.exports[0].ent.fn.params[0].t.res == n }},
		{ctType{}, func(t ctType) bool { return t == ctType{} }},
	} {
		if got := s.typ(tc.in); !tc.want(got) {
			t.Errorf("typ(%+v) = %+v", tc.in, got)
		}
	}
	if e := s.entity(ctEntity{sort: ctSortComponent, comp: comp}); e.comp == comp {
		t.Error("a component entity was not rewritten")
	}
	if e := s.entity(ctEntity{sort: ctSortCoreModule}); e.sort != ctSortCoreModule {
		t.Error("a core module entity changed")
	}
}

func TestCTDescs(t *testing.T) {
	for _, tc := range []struct{ got, want string }{
		{ctType{fn: &ctFunc{}}.desc(), "func"},
		{ctType{inst: &ctInstance{}}.desc(), "instance"},
		{ctType{comp: &ctComponent{}}.desc(), "component"},
		{ctType{res: &ctResource{}}.desc(), "resource"},
		{ctType{val: prim("u8")}.desc(), "defined type"},
		{ctSortDesc(ctSortValue), "value"},
		{ctSortDesc(ctSortType), "type"},
		{ctSortDesc(ctSortCoreModule), "module"},
		{ctSortDesc(0x09), "sort 0x9"},
		{ctFoundDesc(ctvFixedList), "list"},
		{ctFoundDesc(ctvTuple), "tuple"},
	} {
		if tc.got != tc.want {
			t.Errorf("got %q, want %q", tc.got, tc.want)
		}
	}
}

// The primitives are shared; a name that is none of them -- which only a
// hand-built descriptor carries -- still makes a value type.
func TestCTPrim(t *testing.T) {
	if a, b := ctPrim("u32", false), ctPrim("u32", false); a != b || a.prim != "u32" || a.defined {
		t.Errorf("u32: %+v, %+v", a, b)
	}
	if v := ctPrim("u32", true); !v.defined || v == ctPrim("u32", false) {
		t.Errorf("defined u32: %+v", v)
	}
	if v := ctPrim("weird", false); v.kind != ctvPrim || v.prim != "weird" {
		t.Errorf("weird: %+v", v)
	}
}
