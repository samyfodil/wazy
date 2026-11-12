package instance

import (
	"embed"
	"strings"
	"testing"
)

// testdata/wit: shapes.wasm and implements.wasm are shapes.wit and
// implements.wit made components by `wasm-tools component embed --dummy`
// and `wasm-tools component new`, and their .want.wit what ComponentWIT
// prints for them, which binds as what `wasm-tools component wit` prints
// does (see component/bindgen's TestFromComponentMatchesWasmTools); the .wat
// files are built with `wasm-tools parse`.
//
//go:embed testdata/wit
var witFS embed.FS

func readWIT(t *testing.T, name string) []byte {
	t.Helper()
	b, err := witFS.ReadFile("testdata/wit/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Every kind of value type, a resource with a fallible constructor, a
// static async func and an alias, keywords as names, an inline interface,
// and a world use.
func TestComponentWIT(t *testing.T) {
	got, err := ComponentWIT(readWIT(t, "shapes.wasm"))
	if err != nil {
		t.Fatal(err)
	}
	if want := string(readWIT(t, "shapes.want.wit")); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}

	// A world func using an interface's types the world does not use itself
	// gets a use of them; an exported component is left out.
	got, err = ComponentWIT(readWIT(t, "world_use.wasm"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"  use test:w/host.{r, rec};\n",
		"  export keep: func(x: borrow<r>) -> u32;\n",
		"  export take: func(x: rec) -> u32;\n",
		"    resource r;\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	got, err = ComponentWIT(readWIT(t, "component_export.wasm"))
	if err != nil || strings.Contains(got, "export c") || !strings.Contains(got, "export f: func();") {
		t.Errorf("component export: %v\n%s", err, got)
	}

	// Instances that implement an interface are of it, which is printed
	// once however many instances it has; an inline interface's types are
	// its own.
	got, err = ComponentWIT(readWIT(t, "implements.wasm"))
	if err != nil {
		t.Fatal(err)
	}
	if want := string(readWIT(t, "implements.want.wit")); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	// A version suffix finishes the version it splits.
	got, err = ComponentWIT(readWIT(t, "version_suffix.wasm"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"  import a:b/c@1.2.3;\n", "  import x: a:b/d@0.2.1;\n", "package a:b@1.2.3 {\n", "package a:b@0.2.1 {\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

// What WIT cannot describe fails, saying what.
func TestComponentWITRefuses(t *testing.T) {
	for name, want := range map[string]string{
		"nested_instance.wasm":  `wit: import "test:n/outer": inner: a instance is not something WIT can describe`,
		"func_type_export.wasm": `wit: import "test:n/types": type t: a func type is not something WIT can describe`,
		"unnamed_record.wasm":   `wit: export "f": a record that no interface names`,
	} {
		if _, err := ComponentWIT(readWIT(t, name)); err == nil || err.Error() != want {
			t.Errorf("%s: %v, want %s", name, err, want)
		}
	}
	if _, err := ComponentWIT([]byte("not a component")); err == nil || !strings.HasPrefix(err.Error(), "wit: decode component: ") {
		t.Errorf("garbage: %v", err)
	}
	b, err := ctenvFS.ReadFile("testdata/ctenv/import_oob.wasm")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ComponentWIT(b); err == nil {
		t.Error("a component that fails the type check prints")
	}
}

// The refusals no component the type check accepts can reach, on the
// printer directly.
func TestWITPrinterRefuses(t *testing.T) {
	p := &witPrinter{valOwner: map[*ctVal]witName{}, resOwner: map[*ctResource]witName{}, byName: map[string][]witName{}}
	s := p.newScope("a:b/c", "a:b")
	u32 := ctPrim("u32", false)
	named := ctNewFunc(false, 0, 2)
	named.results[0], named.results[1] = ctField{"a", u32}, ctField{"b", u32}
	for what, err := range map[string]error{
		"accessor":     p.addFunc(s, "[method][get]r.x", ctNewFunc(false, 0, 0)),
		"named result": second2(p.funcSig(s, named, 0)),
		"no self":      second2(p.params(s, ctNewFunc(false, 0, 0), 1)),
		"resource":     second2(p.resName(s, &ctResource{})),
	} {
		if err == nil {
			t.Errorf("%s: no error", what)
		}
	}
	for _, name := range []string{"[constructor]r", "[static]r.m", "[method]r.m"} {
		fn := ctNewFunc(false, 1, 1)
		fn.params[0], fn.results[0] = ctField{"x", u32}, ctField{"", u32}
		if err := p.addFunc(s, name, fn); err == nil {
			t.Errorf("%s: a func of a resource that is not there", name)
		}
	}
	if err := p.typeExport(s, "x", ctType{fn: ctNewFunc(false, 0, 0)}); err == nil {
		t.Error("a func type exports")
	}
	if pkg, ok := witPackageOf("a:b/c@1.2.3"); !ok || pkg != "a:b@1.2.3" {
		t.Errorf("package of a:b/c@1.2.3: %q %v", pkg, ok)
	}
	if _, ok := witPackageOf("label"); ok {
		t.Error("a label is qualified")
	}
}

func second2[A any](_ A, err error) error { return err }
