package instance

import (
	"embed"
	"errors"
	"strings"
	"testing"

	"github.com/samyfodil/wazy/internal/component/binary"
	"github.com/samyfodil/wazy/internal/wasm"
)

// The core half of the type check (ctcore.go), case by case: what the
// vendored suites do not reach of the module types, the core subtyping and
// the names wasmparser gives types in its messages.

func TestCtModuleOfDefinition(t *testing.T) {
	m, err := ctModuleOfDefinition(ctcoreFixture(t, "kinds"))
	if err != nil {
		t.Fatal(err)
	}
	if len(m.imports) != 5 {
		t.Fatalf("imports %+v", m.imports)
	}
	want := []struct {
		module, name string
		kind         byte
	}{{"a", "f", 0x00}, {"a", "t", 0x01}, {"a", "m", 0x02}, {"a", "g", 0x03}, {"a", "e", 0x04}}
	for i, w := range want {
		if im := m.imports[i]; im.module != w.module || im.name != w.name || im.ext.kind != w.kind || !im.ext.known {
			t.Errorf("import %d = %+v, want %+v", i, im, w)
		}
	}
	if x := m.imports[1].ext; x.elem != wasm.ValueTypeFuncref || x.min != 1 || !x.hasMax || x.max != 2 {
		t.Errorf("table import %+v", x)
	}
	if x := m.imports[2].ext; !x.shared || x.min != 1 || x.max != 2 || x.pageLog2 != 16 {
		t.Errorf("memory import %+v", x)
	}
	if x := m.imports[3].ext; x.val != wasm.ValueTypeI64 || !x.mutable {
		t.Errorf("global import %+v", x)
	}
	if x := m.imports[4].ext; x.fn == nil || len(x.fn.params) != 1 {
		t.Errorf("tag import %+v", x)
	}
	exports := map[string]ctCoreExtern{}
	for _, x := range m.exports {
		exports[x.name] = x.ext
	}
	for name, kind := range map[string]byte{"t2": 0x01, "m2": 0x02, "g2": 0x03, "e2": 0x04, "f2": 0x00, "f": 0x00, "t": 0x01, "m": 0x02, "g": 0x03, "e": 0x04} {
		if x, ok := exports[name]; !ok || x.kind != kind || !x.known {
			t.Errorf("export %q = %+v, want a known sort %#x", name, x, kind)
		}
	}
	if x := exports["f2"]; x.fn == nil || len(x.fn.results) != 1 {
		t.Errorf("func export %+v", x)
	}
	for name, kind := range map[string]byte{"missing-func": 0x00, "missing-table": 0x01, "missing-memory": 0x02, "missing-global": 0x03, "missing-tag": 0x04} {
		if x := exports[name]; x.kind != kind || x.known {
			t.Errorf("export %q = %+v, want only its sort %#x known", name, x, kind)
		}
	}

	// A function whose type index is past the type section: decoding does
	// not validate, and its type is simply not known.
	bad := []byte{
		0, 'a', 's', 'm', 1, 0, 0, 0,
		1, 4, 1, 0x60, 0, 0, // type section: (func)
		3, 2, 1, 5, // function section: type 5
		7, 5, 1, 1, 'f', 0, 0, // export "f" (func 0)
		10, 4, 1, 2, 0, 0x0b, // code section
	}
	if m, err = ctModuleOfDefinition(bad); err != nil || len(m.exports) != 1 || m.exports[0].ext.fn != nil {
		t.Fatalf("a function of an unknown type: %+v, %v", m, err)
	}
	if _, err := ctModuleOfDefinition([]byte("not wasm")); err == nil {
		t.Fatal("a module that does not decode was read")
	}
}

// ctTypesScope is a ctCoreScope over a fixed list.
type ctTypesScope []ctCoreType

func (s ctTypesScope) coreTypeAt(idx uint32) (ctCoreType, error) {
	if int(idx) >= len(s) {
		return ctCoreType{}, errOutOfBounds
	}
	return s[idx], nil
}

var errOutOfBounds = errors.New("out of bounds")

func TestCtModuleOfType(t *testing.T) {
	fn := &ctCoreFunc{params: []wasm.ValueType{wasm.ValueTypeI32}}
	scope := ctTypesScope{{fn: fn}, {mod: &ctModule{}}}
	decls := []binary.CoreModuleDecl{
		// type 0: a rec group of (func (param i32) (result (ref null 0)
		// (ref func))), a struct (field i8 (mut externref)), and an array
		// that is a final sub of type 1.
		{Kind: binary.CoreModuleDeclType, Rec: []byte{
			0x4e, 0x03,
			0x60, 0x01, 0x7f, 0x02, 0x63, 0x00, 0x64, 0x70,
			0x5f, 0x02, 0x78, 0x00, 0x6f, 0x01,
			0x4f, 0x01, 0x01, 0x5e, 0x7e, 0x00,
		}},
		{Kind: binary.CoreModuleDeclAlias, AliasCount: 0, AliasIndex: 0}, // type 3: type 0 again
		{Kind: binary.CoreModuleDeclAlias, AliasCount: 1, AliasIndex: 0}, // type 4: the scope's func
		{Kind: binary.CoreModuleDeclImport, Module: "a", Name: "f", Extern: binary.CoreExternType{Kind: 0x00, TypeIndex: 4}},
		{Kind: binary.CoreModuleDeclImport, Module: "a", Name: "t", Extern: binary.CoreExternType{Kind: 0x01, Bytes: []byte{0x70, 0x05, 0x01, 0x02}}},
		{Kind: binary.CoreModuleDeclImport, Module: "a", Name: "m", Extern: binary.CoreExternType{Kind: 0x02, Bytes: []byte{0x0b, 0x01, 0x02, 0x0c}}},
		{Kind: binary.CoreModuleDeclExport, Name: "g", Extern: binary.CoreExternType{Kind: 0x03, Bytes: []byte{0x64, 0x6e, 0x01}}},
		{Kind: binary.CoreModuleDeclExport, Name: "e", Extern: binary.CoreExternType{Kind: 0x04, TypeIndex: 3}},
	}
	m, err := ctModuleOfType(scope, &binary.CoreModuleType{Decls: decls})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.imports) != 3 || len(m.exports) != 2 {
		t.Fatalf("got %+v", m)
	}
	if m.imports[0].ext.fn != fn {
		t.Errorf("the outer alias did not reach the scope's type: %+v", m.imports[0].ext)
	}
	if x := m.imports[1].ext; x.elem != wasm.ValueTypeFuncref || x.min != 1 || x.max != 2 || !x.hasMax || !x.is64 {
		t.Errorf("table %+v", x)
	}
	if x := m.imports[2].ext; !x.shared || x.max != 2 || x.pageLog2 != 12 {
		t.Errorf("memory %+v", x)
	}
	if x := m.exports[0].ext; x.val != wasm.ValueTypeAnyref.AsNonNullable() || !x.mutable {
		t.Errorf("global %+v", x)
	}
	f := m.exports[1].ext.fn
	if f == nil || f.String() != "(func (param i32) (result (ref null 0) (ref func)))" {
		t.Errorf("tag type %v", f)
	}

	for _, tc := range []struct {
		name string
		decl binary.CoreModuleDecl
		want string
	}{
		{"own alias out of bounds", binary.CoreModuleDecl{Kind: binary.CoreModuleDeclAlias, AliasIndex: 9}, "unknown type 9: type index out of bounds"},
		{"outer alias out of bounds", binary.CoreModuleDecl{Kind: binary.CoreModuleDeclAlias, AliasCount: 1, AliasIndex: 9}, "out of bounds"},
		{"outer alias of a module type", binary.CoreModuleDecl{Kind: binary.CoreModuleDeclAlias, AliasCount: 1, AliasIndex: 1}, "a module type cannot alias a module type"},
		{"outer alias too far", binary.CoreModuleDecl{Kind: binary.CoreModuleDeclAlias, AliasCount: 2}, "outer type aliases in module type declarations are limited to a maximum count of 1"},
		{"func of a struct type", binary.CoreModuleDecl{Kind: binary.CoreModuleDeclImport, Extern: binary.CoreExternType{Kind: 0x00, TypeIndex: 1}}, "type index 1 is not a function type"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ds := append([]binary.CoreModuleDecl{decls[0]}, tc.decl)
			_, err := ctModuleOfType(scope, &binary.CoreModuleType{Decls: ds})
			if err == nil || !strings.Contains(err.Error(), "module type declaration 1: "+tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestCtCoreExternSub(t *testing.T) {
	f0 := &ctCoreFunc{}
	f1 := &ctCoreFunc{params: []wasm.ValueType{wasm.ValueTypeI32}}
	table := ctCoreExtern{kind: 0x01, known: true, elem: wasm.ValueTypeFuncref, min: 1}
	memory := ctCoreExtern{kind: 0x02, known: true, min: 1, pageLog2: 16}
	global := ctCoreExtern{kind: 0x03, known: true, val: wasm.ValueTypeAnyref}
	with := func(x ctCoreExtern, f func(*ctCoreExtern)) ctCoreExtern { f(&x); return x }
	for _, tc := range []struct {
		name string
		a, b ctCoreExtern
		want string
	}{
		{"sort", ctCoreExtern{kind: 0x00}, ctCoreExtern{kind: 0x04}, "expected tag, found func"},
		{"unknown func", ctCoreExtern{kind: 0x00}, ctCoreExtern{kind: 0x00, known: true, fn: f1}, ""},
		{"func", ctCoreExtern{kind: 0x00, known: true, fn: f1}, ctCoreExtern{kind: 0x00, known: true, fn: f0}, "expected: (func)\nfound:    (func (param i32))"},
		{"func params", ctCoreExtern{kind: 0x00, known: true, fn: f1}, ctCoreExtern{kind: 0x00, known: true, fn: &ctCoreFunc{params: []wasm.ValueType{wasm.ValueTypeI64}}}, "expected: (func (param i64))\nfound:    (func (param i32))"},
		{"func results", ctCoreExtern{kind: 0x00, known: true, fn: &ctCoreFunc{results: []wasm.ValueType{wasm.ValueTypeI32}}}, ctCoreExtern{kind: 0x00, known: true, fn: f0}, "expected: (func)\nfound:    (func (result i32))"},
		{"table", table, table, ""},
		{"table element", with(table, func(x *ctCoreExtern) { x.elem = wasm.ValueTypeExternref }), table, "expected table element type funcref, found externref"},
		{"table shared", with(table, func(x *ctCoreExtern) { x.shared = true }), table, "mismatch in the shared flag for tables"},
		{"table64", with(table, func(x *ctCoreExtern) { x.is64 = true }), table, "mismatch in index type used for tables"},
		{"table limits", table, with(table, func(x *ctCoreExtern) { x.min = 2 }), "mismatch in table limits"},
		{"table max", table, with(table, func(x *ctCoreExtern) { x.hasMax, x.max = true, 3 }), "mismatch in table limits"},
		{"table max fits", with(table, func(x *ctCoreExtern) { x.hasMax, x.max = true, 2 }), with(table, func(x *ctCoreExtern) { x.hasMax, x.max = true, 3 }), ""},
		{"memory shared", with(memory, func(x *ctCoreExtern) { x.shared = true }), memory, "mismatch in the shared flag for memories"},
		{"memory64", with(memory, func(x *ctCoreExtern) { x.is64 = true }), memory, "mismatch in index type used for memories"},
		{"memory page size", with(memory, func(x *ctCoreExtern) { x.pageLog2 = 0 }), memory, "mismatch in page size for memories"},
		{"memory limits", memory, with(memory, func(x *ctCoreExtern) { x.min = 2 }), "mismatch in memory limits"},
		{"global covariant", with(global, func(x *ctCoreExtern) { x.val = wasm.ValueTypeEqref }), global, ""},
		{"global contravariant", global, with(global, func(x *ctCoreExtern) { x.val = wasm.ValueTypeEqref }), "expected global type eqref, found anyref"},
		{"global mutability", with(global, func(x *ctCoreExtern) { x.mutable = true }), global, "global types differ in mutability"},
		{"mutable global invariant", with(global, func(x *ctCoreExtern) { x.mutable, x.val = true, wasm.ValueTypeEqref }), with(global, func(x *ctCoreExtern) { x.mutable = true }), "expected global type anyref, found eqref"},
		// A tag matches both ways, the expected type checked against the
		// found one first, as wasmparser does.
		{"tag", ctCoreExtern{kind: 0x04, known: true, fn: f0}, ctCoreExtern{kind: 0x04, known: true, fn: f1}, "expected: (func)\nfound:    (func (param i32))"},
		{"tag same", ctCoreExtern{kind: 0x04, known: true, fn: f1}, ctCoreExtern{kind: 0x04, known: true, fn: f1}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ctCoreExternSub(tc.a, tc.b)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || err.Error() != tc.want {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestCtCoreValSub(t *testing.T) {
	concrete := wasm.ValueTypeConcreteRef(3, true)
	for _, tc := range []struct {
		a, b wasm.ValueType
		want bool
	}{
		{wasm.ValueTypeI32, wasm.ValueTypeI32, true},
		{wasm.ValueTypeI32, wasm.ValueTypeI64, false},
		{wasm.ValueTypeI32, wasm.ValueTypeAnyref, false},
		{wasm.ValueTypeAnyref, wasm.ValueTypeAnyref.AsNonNullable(), false},
		{wasm.ValueTypeAnyref.AsNonNullable(), wasm.ValueTypeAnyref, true},
		{concrete, wasm.ValueTypeFuncref, true},
		{concrete, wasm.ValueTypeAnyref, false},
		{wasm.ValueTypeNullref, wasm.ValueTypeStructref, true},
		{wasm.ValueTypeNullref, wasm.ValueTypeFuncref, false},
		{wasm.ValueTypeNullFuncref, concrete, true},
		{wasm.ValueTypeNullExternref, wasm.ValueTypeExternref, true},
		{wasm.ValueTypeNullExnref, wasm.ValueTypeExnref, true},
		{wasm.ValueTypeI31ref, wasm.ValueTypeEqref, true},
		{wasm.ValueTypeArrayref, wasm.ValueTypeAnyref, true},
		{wasm.ValueTypeEqref, wasm.ValueTypeAnyref, true},
		{wasm.ValueTypeAnyref, wasm.ValueTypeEqref, false},
		{wasm.ValueTypeExternref, wasm.ValueTypeFuncref, false},
	} {
		if got := ctCoreValSub(tc.a, tc.b); got != tc.want {
			t.Errorf("%s <: %s = %v, want %v", ctCoreValDesc(tc.a), ctCoreValDesc(tc.b), got, tc.want)
		}
	}
}

func TestCtCoreValDesc(t *testing.T) {
	for v, want := range map[wasm.ValueType]string{
		wasm.ValueTypeF32: "f32", wasm.ValueTypeF64: "f64", wasm.ValueTypeV128: "v128",
		wasm.ValueTypeExnref: "exnref", wasm.ValueTypeI31ref: "i31ref", wasm.ValueTypeStructref: "structref",
		wasm.ValueTypeArrayref: "arrayref", wasm.ValueTypeNullref: "nullref", wasm.ValueTypeNullFuncref: "nullfuncref",
		wasm.ValueTypeNullExternref: "nullexternref", wasm.ValueTypeNullExnref: "nullexnref",
		wasm.ValueTypeConcreteRef(7, false):       "(ref 7)",
		wasm.ValueTypeFuncref.AsNonNullable():     "(ref func)",
		wasm.ValueTypeNullref.AsNonNullable():     "(ref none)",
		wasm.ValueTypeNullFuncref.AsNonNullable(): "(ref nofunc)",
	} {
		if got := ctCoreValDesc(v); got != want {
			t.Errorf("ctCoreValDesc(%#x) = %q, want %q", uint64(v), got, want)
		}
	}
}

// ctcoreFS holds the fixtures of testdata/ctcore: see its README.
//
//go:embed testdata/ctcore
var ctcoreFS embed.FS

func ctcoreFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := ctcoreFS.ReadFile("testdata/ctcore/" + name + ".wasm")
	if err != nil {
		t.Fatal(err)
	}
	return b
}
