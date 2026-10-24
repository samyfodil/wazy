package wit

import (
	"embed"
	"testing"
)

// The sample files are embedded: some CI jobs run a test binary from the
// module root, where a relative path would not find them.
//
//go:embed testdata/*.wit
var samples embed.FS

// TestParse_ExternalIDPlacement covers what wasm-tools 1.262.0 -- the
// version testdata/probes.txt records -- still accepted: @external-id on an
// interface, a world or an include, which wit-parser refuses from
// wasm-tools@2e6b2d6d on (its parse-fail/external-id-on-*.wit, vendored in
// component/wit, with these messages).
func TestParse_ExternalIDPlacement(t *testing.T) {
	for src, want := range map[string]string{
		`package a:b; @external-id("x") interface i {}`:                    "x.wit:1:15: @external-id is not allowed on interface declarations",
		`package a:b; @external-id("x") world w {}`:                        "x.wit:1:15: @external-id is not allowed on world declarations",
		`package a:b; world v {} world w { @external-id("x") include v; }`: "x.wit:1:36: @external-id is not allowed on include statements",
	} {
		if _, err := Parse("x.wit", src); err == nil || err.Error() != want {
			t.Errorf("%s: %v\nwant %s", src, err, want)
		}
	}
}

// TestParse_Shapes checks what the parser builds from a source that uses
// every kind of item.
func TestParse_Shapes(t *testing.T) {
	pkg, err := Parse("x.wit", `/// docs
package a:b@1.2.3;
use c:d/e@1.0.0 as e2;
interface i {
  use e2.{t, u as v};
  record r { a: u8, b: list<u8, 4> }
  variant var { none, some(r) }
  enum e { x, y }
  flags f { p, q }
  resource res {
    constructor(x: u8) -> result<res, string>;
    m: func();
    s: static func() -> res;
    prop: get() -> u8;
    prop: set(v: u8);
  }
  type a = result<_, u8>;
  type h = borrow<res>;
  g: async func(x: tuple<u8, string>) -> map<string, option<u8>>;
}
world w {
  @unstable(feature = x) @external-id("id")
  import label: i;
  import c:d/e@1.0.0;
  import bare;
  export f: func();
  export x: interface {}
  include v with { a as b }
  include c:d/w;
}
package c:d { interface e { type t = u8; } }`)
	if err != nil {
		t.Fatal(err)
	}
	if pkg.Name != "a:b@1.2.3" || !pkg.Docs || pkg.NamePos != (Pos{"x.wit", 2, 9}) || len(pkg.Nested) != 1 || pkg.Nested[0].Name != "c:d" {
		t.Fatalf("package %+v", pkg)
	}
	use := pkg.Items[0].(*ToplevelUse)
	if name, _ := use.Local(); name != "e2" || use.Path != "c:d/e@1.0.0" {
		t.Errorf("use %+v", use)
	}
	i := pkg.Items[1].(*Interface)
	if u := i.Items[0].(*Use); u.Path != "e2" || len(u.Names) != 2 || u.Names[1].As != "v" {
		t.Errorf("use %+v", u)
	}
	if n, p := i.Items[0].(*Use).Names[1].Local(); n != "v" || p.Col != 19 {
		t.Errorf("local %s at %v", n, p)
	}
	res := i.Items[5].(*TypeDef).Type.(*Resource)
	if len(res.Methods) != 5 || !res.Methods[0].IsConstructor || !res.Methods[2].IsStatic || res.Methods[3].Func.Accessor != Getter || res.Methods[4].Func.Accessor != Setter {
		t.Errorf("resource %+v", res)
	}
	w := pkg.Items[2].(*World)
	label := w.Items[0].(*Import)
	if np, ok := label.Kind.(*ExternNamedPath); !ok || np.Path != "i" || label.Name != "label" || len(label.Gate) != 1 || len(label.ExternalID) != 1 {
		t.Errorf("labeled import %+v", label)
	}
	if p := w.Items[1].(*Import); p.Name != "c:d/e@1.0.0" {
		t.Errorf("path import %+v", p)
	}
	if inc := w.Items[5].(*Include); len(inc.Renames) != 1 || inc.Renames[0].As != "b" {
		t.Errorf("include %+v", inc)
	}
	for _, tc := range []struct {
		ty   TypeRef
		want string
	}{
		{i.Items[1].(*TypeDef).Type.(*Record).Fields[1].Type, "list<u8, 4>"},
		{i.Items[6].(*TypeDef).Type.(*TypeAlias).Target, "result<_, u8>"},
		{i.Items[7].(*TypeDef).Type.(*TypeAlias).Target, "borrow<res>"},
		{i.Items[8].(*InterfaceFunc).Func.Params[0].Type, "tuple<u8, string>"},
		{*i.Items[8].(*InterfaceFunc).Func.Result, "map<string, option<u8>>"},
		{TypeRef{Kind: "result", Inner: &TypeRef{Kind: "u8"}}, "result<u8>"},
		{TypeRef{Kind: "result", Inner: &TypeRef{Kind: "u8"}, Inner2: &TypeRef{Kind: "u8"}}, "result<u8, u8>"},
		{TypeRef{Kind: "result"}, "result"},
		{TypeRef{Kind: "list", Inner: &TypeRef{Kind: "named", Name: "x"}}, "list<x>"},
		{TypeRef{Kind: "stream"}, "stream"},
	} {
		if got := tc.ty.String(); got != tc.want {
			t.Errorf("%s, want %s", got, tc.want)
		}
	}
	if LastSegment("a:b/c@1.0.0") != "c" || LastSegment("c") != "c" {
		t.Error("LastSegment")
	}
}

func TestParse_TestdataFiles(t *testing.T) {
	for _, name := range []string{"random.wit", "simple.wit", "streams.wit"} {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse(name, string(sample(t, name))); err != nil {
				t.Error(err)
			}
		})
	}
}

// The AST's marker methods only tag which items may appear where.
func TestAST_Markers(t *testing.T) {
	for _, it := range []PackageItem{&Interface{}, &World{}, &ToplevelUse{}} {
		it.packageItem()
	}
	for _, it := range []InterfaceItem{&InterfaceFunc{}, &TypeDef{}, &Use{}} {
		it.interfaceItem()
	}
	for _, it := range []WorldItem{&TypeDef{}, &Use{}, &Include{}, &Import{}, &Export{}} {
		it.worldItem()
	}
	for _, it := range []TypeDefBody{&TypeAlias{}, &Record{}, &Variant{}, &Enum{}, &Flags{}, &Resource{}} {
		it.typeDefBody()
	}
	for _, it := range []ExternKind{&ExternFunc{}, &ExternInterface{}, &ExternPath{}, &ExternNamedPath{}} {
		it.externKind()
	}
	if (&syntaxError{msg: "m"}).Error() != "m" {
		t.Error("syntaxError")
	}
	if name, pos := (&ToplevelUse{Path: "a:b/c@1.0.0", PathPos: Pos{Line: 3}}).Local(); name != "c" || pos.Line != 3 {
		t.Errorf("use without as: %s %v", name, pos)
	}
}
