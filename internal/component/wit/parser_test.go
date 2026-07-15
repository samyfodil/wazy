package wit

import (
	"embed"
	"fmt"
	"strings"
	"testing"
)

//go:embed testdata
var testdata embed.FS

// TestSimpleWIT tests parsing a hand-written simple WIT file.
func TestSimpleWIT(t *testing.T) {
	src := `package example:test@0.1.0;

interface math {
  type big-int = list<u8>;

  record point {
    x: s32,
    y: s32,
  }

  variant color {
    red,
    green,
    blue,
    custom(string),
  }

  enum direction {
    north,
    south,
    east,
    west,
  }

  flags permissions {
    read,
    write,
    execute,
  }

  add: func(a: u32, b: u32) -> u32;
}

world command {
  export run: func() -> result<_, string>;
}
`

	pkg, err := Parse("simple.wit", src)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	if pkg == nil {
		t.Fatal("Package is nil")
	}

	// Basic validation
	if pkg.Name == "" {
		t.Error("Package name is empty")
	}

	if len(pkg.Items) < 2 {
		t.Errorf("Expected at least 2 items (interface + world), got %d", len(pkg.Items))
	}

	// Check interface
	iface, ok := pkg.Items[0].(*Interface)
	if !ok {
		t.Errorf("First item is not Interface, got %T", pkg.Items[0])
	} else {
		if iface.Name != "math" {
			t.Errorf("Interface name is %s, expected 'math'", iface.Name)
		}

		// Check that the interface has multiple items
		if len(iface.Items) == 0 {
			t.Error("Interface has no items")
		}
	}

	// Check world
	world, ok := pkg.Items[1].(*World)
	if !ok {
		t.Errorf("Second item is not World, got %T", pkg.Items[1])
	} else {
		if world.Name != "command" {
			t.Errorf("World name is %s, expected 'command'", world.Name)
		}
	}

	t.Logf("Successfully parsed simple WIT: %+v", pkg)
}

// TestTypeDefinitions tests parsing various type definitions.
func TestTypeDefinitions(t *testing.T) {
	tests := []struct {
		name    string
		src     string
		wantErr bool
	}{
		{
			name: "record",
			src: `package test:test;
interface i {
  type point = record { x: u32, y: u32 };
}`,
			wantErr: false,
		},
		{
			name: "variant",
			src: `package test:test;
interface i {
  type result-type = variant { ok(u32), err(string) };
}`,
			wantErr: false,
		},
		{
			name: "enum",
			src: `package test:test;
interface i {
  type status = enum { pending, complete, failed };
}`,
			wantErr: false,
		},
		{
			name: "flags",
			src: `package test:test;
interface i {
  type perms = flags { read, write, execute };
}`,
			wantErr: false,
		},
		{
			name: "type alias",
			src: `package test:test;
interface i {
  type my-int = u32;
  type my-list = list<u32>;
  type my-string = string;
}`,
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pkg, err := Parse("test.wit", tt.src)
			if (err != nil) != tt.wantErr {
				t.Errorf("Parse error: %v, wantErr=%v", err, tt.wantErr)
				return
			}
			if err == nil && pkg == nil {
				t.Error("Parse succeeded but package is nil")
			}
		})
	}
}

// TestCompoundTypes tests parsing compound type references.
func TestCompoundTypes(t *testing.T) {
	tests := []struct {
		name     string
		typeExpr string
		wantKind string
	}{
		{"list", "list<u32>", "list"},
		{"option", "option<string>", "option"},
		{"result", "result<u32, string>", "result"},
		{"result_ok_only", "result<u32>", "result"},
		{"tuple", "tuple<u32, string>", "tuple"},
		{"map", "map<string, u32>", "map"},
		{"future", "future<u32>", "future"},
		{"stream", "stream<u8>", "stream"},
		{"own", "own<my-resource>", "own"},
		{"borrow", "borrow<my-resource>", "borrow"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := fmt.Sprintf(`package test:test;
interface i {
  type t = %s;
}`, tt.typeExpr)

			pkg, err := Parse("test.wit", src)
			if err != nil {
				t.Errorf("Parse failed: %v", err)
				return
			}

			if pkg == nil || len(pkg.Items) == 0 {
				t.Error("Package or items empty")
				return
			}

			iface, ok := pkg.Items[0].(*Interface)
			if !ok {
				t.Fatalf("Expected Interface, got %T", pkg.Items[0])
			}

			if len(iface.Items) == 0 {
				t.Fatal("Interface has no items")
			}

			typeDef, ok := iface.Items[0].(*TypeDef)
			if !ok {
				t.Fatalf("Expected TypeDef, got %T", iface.Items[0])
			}

			alias, ok := typeDef.Type.(*TypeAlias)
			if !ok {
				t.Fatalf("Expected TypeAlias, got %T", typeDef.Type)
			}

			if alias.Target.Kind != tt.wantKind {
				t.Errorf("Type kind is %s, expected %s", alias.Target.Kind, tt.wantKind)
			}
		})
	}
}

// TestFunctionSignatures tests parsing function signatures.
func TestFunctionSignatures(t *testing.T) {
	tests := []struct {
		name    string
		src     string
		wantErr bool
	}{
		{
			name: "simple func",
			src: `package test:test;
interface i {
  add: func(a: u32, b: u32) -> u32;
}`,
			wantErr: false,
		},
		{
			name: "func no params",
			src: `package test:test;
interface i {
  get-time: func() -> u64;
}`,
			wantErr: false,
		},
		{
			name: "func no return",
			src: `package test:test;
interface i {
  do-something: func(x: u32);
}`,
			wantErr: false,
		},
		{
			name: "async func",
			src: `package test:test;
interface i {
  async-op: async func(x: u32) -> u32;
}`,
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pkg, err := Parse("test.wit", tt.src)
			if (err != nil) != tt.wantErr {
				t.Errorf("Parse error: %v, wantErr=%v", err, tt.wantErr)
				return
			}
			if err == nil && pkg == nil {
				t.Error("Package is nil")
			}
		})
	}
}

// TestRealWASIStreamsWIT tests parsing the real, vendored wasi:io streams.wit
// with zero errors, including gate attributes and resource methods.
func TestRealWASIStreamsWIT(t *testing.T) {
	// Load the real WASI streams.wit
	data, err := testdata.ReadFile("testdata/streams.wit")
	if err != nil {
		t.Fatalf("Failed to read streams.wit: %v", err)
	}

	src := string(data)
	pkg, err := Parse("streams.wit", src)
	if err != nil {
		t.Fatalf("Failed to parse WASI streams.wit: %v", err)
	}

	if pkg == nil {
		t.Fatal("Package is nil")
	}

	if pkg.Name != "wasi:io@0.2.8" {
		t.Errorf("Package name is %q, expected %q", pkg.Name, "wasi:io@0.2.8")
	}

	if len(pkg.Items) != 1 {
		t.Fatalf("Expected 1 top-level item (interface streams), got %d", len(pkg.Items))
	}

	iface, ok := pkg.Items[0].(*Interface)
	if !ok {
		t.Fatalf("Expected Interface, got %T", pkg.Items[0])
	}
	if iface.Name != "streams" {
		t.Errorf("Interface name is %q, expected %q", iface.Name, "streams")
	}
	if len(iface.Gate) != 1 || iface.Gate[0].Kind != "since" || iface.Gate[0].Version != "0.2.0" {
		t.Errorf("Interface gate = %+v, expected a single @since(version = 0.2.0)", iface.Gate)
	}

	var uses, variants, resources int
	var inputStream, outputStream *Resource
	for _, item := range iface.Items {
		switch v := item.(type) {
		case *Use:
			uses++
			if len(v.Gate) != 1 || v.Gate[0].Kind != "since" {
				t.Errorf("use %q missing @since gate: %+v", v.Path, v.Gate)
			}
		case *TypeDef:
			switch body := v.Type.(type) {
			case *Variant:
				variants++
				if v.Name != "stream-error" || len(body.Cases) != 2 {
					t.Errorf("unexpected variant %q with %d cases", v.Name, len(body.Cases))
				}
			case *Resource:
				resources++
				switch v.Name {
				case "input-stream":
					inputStream = body
				case "output-stream":
					outputStream = body
				default:
					t.Errorf("unexpected resource %q", v.Name)
				}
			default:
				t.Errorf("unexpected TypeDef body %T for %q", body, v.Name)
			}
		default:
			t.Errorf("unexpected interface item %T", item)
		}
	}

	if uses != 2 {
		t.Errorf("expected 2 use statements, got %d", uses)
	}
	if variants != 1 {
		t.Errorf("expected 1 variant, got %d", variants)
	}
	if resources != 2 {
		t.Errorf("expected 2 resources, got %d", resources)
	}

	if inputStream == nil {
		t.Fatal("input-stream resource not found")
	}
	if len(inputStream.Methods) != 5 {
		t.Errorf("input-stream has %d methods, expected 5", len(inputStream.Methods))
	}
	for _, m := range inputStream.Methods {
		if len(m.Gate) != 1 || m.Gate[0].Kind != "since" || m.Gate[0].Version != "0.2.0" {
			t.Errorf("input-stream method %q missing @since gate: %+v", m.Name, m.Gate)
		}
	}

	if outputStream == nil {
		t.Fatal("output-stream resource not found")
	}
	if len(outputStream.Methods) != 10 {
		t.Errorf("output-stream has %d methods, expected 10", len(outputStream.Methods))
	}

	// splice/blocking-splice take a borrow<input-stream> parameter.
	var found bool
	for _, m := range outputStream.Methods {
		if m.Name == "splice" {
			found = true
			if len(m.Func.Params) != 2 || m.Func.Params[0].Type.Kind != "borrow" || m.Func.Params[0].Type.Name != "input-stream" {
				t.Errorf("splice params = %+v, expected first param borrow<input-stream>", m.Func.Params)
			}
		}
	}
	if !found {
		t.Error("splice method not found on output-stream")
	}

	t.Logf("Successfully parsed WASI streams.wit: package=%s, items=%d", pkg.Name, len(pkg.Items))
}

// TestRealWASIRandomWIT tests parsing the real, vendored wasi:random random.wit
// with zero errors, as a second independent real-world WASI 0.2 fixture.
func TestRealWASIRandomWIT(t *testing.T) {
	data, err := testdata.ReadFile("testdata/random.wit")
	if err != nil {
		t.Fatalf("Failed to read random.wit: %v", err)
	}

	src := string(data)
	pkg, err := Parse("random.wit", src)
	if err != nil {
		t.Fatalf("Failed to parse WASI random.wit: %v", err)
	}

	if pkg == nil {
		t.Fatal("Package is nil")
	}
	if pkg.Name != "wasi:random@0.2.8" {
		t.Errorf("Package name is %q, expected %q", pkg.Name, "wasi:random@0.2.8")
	}

	if len(pkg.Items) != 1 {
		t.Fatalf("Expected 1 top-level item (interface random), got %d", len(pkg.Items))
	}

	iface, ok := pkg.Items[0].(*Interface)
	if !ok {
		t.Fatalf("Expected Interface, got %T", pkg.Items[0])
	}
	if iface.Name != "random" {
		t.Errorf("Interface name is %q, expected %q", iface.Name, "random")
	}
	if len(iface.Gate) != 1 || iface.Gate[0].Kind != "since" || iface.Gate[0].Version != "0.2.0" {
		t.Errorf("Interface gate = %+v, expected a single @since(version = 0.2.0)", iface.Gate)
	}
	if len(iface.Items) != 2 {
		t.Fatalf("Expected 2 interface items, got %d", len(iface.Items))
	}

	fn, ok := iface.Items[0].(*InterfaceFunc)
	if !ok {
		t.Fatalf("Expected InterfaceFunc, got %T", iface.Items[0])
	}
	if fn.Name != "get-random-bytes" {
		t.Errorf("first func name is %q, expected %q", fn.Name, "get-random-bytes")
	}
	if fn.Func.Result == nil || fn.Func.Result.Kind != "list" {
		t.Errorf("get-random-bytes result = %v, expected list<u8>", fn.Func.Result)
	}

	t.Logf("Successfully parsed WASI random.wit: package=%s, items=%d", pkg.Name, len(pkg.Items))
}

// TestPackageDeclaration tests package name parsing.
func TestPackageDeclaration(t *testing.T) {
	tests := []struct {
		name        string
		src         string
		wantPkgName string
		wantErr     bool
	}{
		{
			name:        "simple",
			src:         "package test:foo;",
			wantPkgName: "test:foo",
			wantErr:     false,
		},
		{
			name:        "with version",
			src:         "package wasi:io@0.2.0;",
			wantPkgName: "wasi:io@0.2.0",
			wantErr:     false,
		},
		{
			name:        "with path",
			src:         "package namespace:package/subpath;",
			wantPkgName: "namespace:package/subpath",
			wantErr:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pkg, err := Parse("test.wit", tt.src)
			if (err != nil) != tt.wantErr {
				t.Errorf("Parse error: %v, wantErr=%v", err, tt.wantErr)
				return
			}
			if err == nil {
				if pkg.Name != tt.wantPkgName {
					t.Errorf("Package name is %q, expected %q", pkg.Name, tt.wantPkgName)
				}
			}
		})
	}
}

// TestErrorReporting tests that errors include line and column information.
func TestErrorReporting(t *testing.T) {
	src := `package test:test;

interface bad {
  invalid syntax here
}
`

	_, err := Parse("test.wit", src)
	if err == nil {
		t.Fatal("Expected parse error")
	}

	// Check that error includes filename and line number
	errStr := err.Error()
	if !strings.Contains(errStr, "test.wit") {
		t.Errorf("Error doesn't include filename: %s", errStr)
	}
	if !strings.Contains(errStr, ":") {
		t.Errorf("Error doesn't include line:column info: %s", errStr)
	}
}

// TestUseStatement tests use statement parsing.
func TestUseStatement(t *testing.T) {
	src := `package test:test;

use wasi:io/streams.{input-stream, output-stream};

interface i {
  process: func(input: input-stream, output: output-stream) -> result<_, string>;
}
`

	pkg, err := Parse("test.wit", src)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	if pkg == nil {
		t.Fatal("Package is nil")
	}

	// Should have a Use statement and an Interface
	if len(pkg.Items) < 2 {
		t.Errorf("Expected at least 2 items (use + interface), got %d", len(pkg.Items))
	}

	_, ok := pkg.Items[0].(*Use)
	if !ok {
		t.Errorf("First item is not Use, got %T", pkg.Items[0])
	}
}

// TestPrimitiveTypes tests parsing all primitive types.
func TestPrimitiveTypes(t *testing.T) {
	primitives := []string{
		"u8", "u16", "u32", "u64",
		"s8", "s16", "s32", "s64",
		"f32", "f64",
		"bool", "char", "string",
	}

	for _, prim := range primitives {
		t.Run(prim, func(t *testing.T) {
			src := fmt.Sprintf(`package test:test;
interface i {
  type t = %s;
}`, prim)

			pkg, err := Parse("test.wit", src)
			if err != nil {
				t.Errorf("Parse failed: %v", err)
				return
			}

			if pkg == nil || len(pkg.Items) == 0 {
				t.Fatal("Package or items empty")
			}

			iface, ok := pkg.Items[0].(*Interface)
			if !ok {
				t.Fatalf("Expected Interface, got %T", pkg.Items[0])
			}

			if len(iface.Items) == 0 {
				t.Fatal("Interface has no items")
			}

			typeDef, ok := iface.Items[0].(*TypeDef)
			if !ok {
				t.Fatalf("Expected TypeDef, got %T", iface.Items[0])
			}

			alias, ok := typeDef.Type.(*TypeAlias)
			if !ok {
				t.Fatalf("Expected TypeAlias, got %T", typeDef.Type)
			}

			if alias.Target.Kind != prim {
				t.Errorf("Type kind is %s, expected %s", alias.Target.Kind, prim)
			}
		})
	}
}

// TestComments tests that comments are properly skipped.
func TestComments(t *testing.T) {
	src := `// Line comment
package test:test; // inline comment

/* Block comment */
interface i {
  /* Another block comment
     spanning multiple lines
  */
  type t = u32;
}
`

	pkg, err := Parse("test.wit", src)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	if pkg == nil {
		t.Fatal("Package is nil")
	}

	// Should have parsed successfully ignoring comments
	if pkg.Name == "" {
		t.Error("Package name is empty")
	}
}

// TestInvalidSyntax tests that invalid syntax produces errors.
func TestInvalidSyntax(t *testing.T) {
	tests := []struct {
		name string
		src  string
	}{
		{
			name: "missing semicolon",
			src:  "package test:test",
		},
		{
			name: "missing type name",
			src:  "package test:test; interface i { type = u32; }",
		},
		{
			name: "invalid characters",
			src:  "package test:test; interface $$$ { }",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse("test.wit", tt.src)
			if err == nil {
				t.Error("Expected parse error, got nil")
			}
		})
	}
}

// TestResourceConstructorAndStatic tests resource constructors and static
// methods, which aren't exercised by streams.wit or random.wit.
func TestResourceConstructorAndStatic(t *testing.T) {
	src := `package test:test;
interface i {
  resource blob {
    constructor(init: list<u8>);
    write: func(bytes: list<u8>);
    read: func(n: u32) -> list<u8>;
    merge: static func(lhs: borrow<blob>, rhs: borrow<blob>) -> blob;
  }
  resource blob2 {
    constructor(init: list<u8>) -> result<blob2>;
  }
}`

	pkg, err := Parse("test.wit", src)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	iface := pkg.Items[0].(*Interface)
	blobDef, ok := iface.Items[0].(*TypeDef)
	if !ok || blobDef.Name != "blob" {
		t.Fatalf("expected TypeDef 'blob', got %T", iface.Items[0])
	}
	blob, ok := blobDef.Type.(*Resource)
	if !ok {
		t.Fatalf("expected Resource, got %T", blobDef.Type)
	}
	if len(blob.Methods) != 4 {
		t.Fatalf("expected 4 methods on blob, got %d", len(blob.Methods))
	}

	ctor := blob.Methods[0]
	if !ctor.IsConstructor || ctor.Name != "constructor" || len(ctor.Func.Params) != 1 {
		t.Errorf("unexpected constructor: %+v", ctor)
	}
	if ctor.Func.Result != nil {
		t.Errorf("infallible constructor should have nil result, got %v", ctor.Func.Result)
	}

	write := blob.Methods[1]
	if write.Name != "write" || write.IsStatic || write.IsConstructor {
		t.Errorf("unexpected write method: %+v", write)
	}

	merge := blob.Methods[3]
	if merge.Name != "merge" || !merge.IsStatic || merge.IsConstructor {
		t.Errorf("unexpected merge method: %+v", merge)
	}
	if len(merge.Func.Params) != 2 || merge.Func.Result == nil || merge.Func.Result.Name != "blob" {
		t.Errorf("unexpected merge signature: %+v", merge.Func)
	}

	blob2Def := iface.Items[1].(*TypeDef)
	blob2 := blob2Def.Type.(*Resource)
	fallibleCtor := blob2.Methods[0]
	if fallibleCtor.Func.Result == nil || fallibleCtor.Func.Result.Kind != "result" {
		t.Errorf("fallible constructor should return result<blob2>, got %v", fallibleCtor.Func.Result)
	}
}

// TestStackedGates tests multiple gate attributes stacked on one item, per
// the WIT.md example ("@since(...) @deprecated(...) e: func();").
func TestStackedGates(t *testing.T) {
	src := `package test:test;
interface foo {
  a: func();

  @since(version = 0.2.1)
  b: func();

  @unstable(feature = fancier-foo)
  d: func();

  @since(version = 0.2.0)
  @deprecated(version = 0.2.2)
  e: func();
}`

	pkg, err := Parse("test.wit", src)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	iface := pkg.Items[0].(*Interface)
	if len(iface.Items) != 4 {
		t.Fatalf("expected 4 funcs, got %d", len(iface.Items))
	}

	a := iface.Items[0].(*InterfaceFunc)
	if len(a.Gate) != 0 {
		t.Errorf("a should have no gate, got %+v", a.Gate)
	}

	b := iface.Items[1].(*InterfaceFunc)
	if len(b.Gate) != 1 || b.Gate[0].Kind != "since" || b.Gate[0].Version != "0.2.1" {
		t.Errorf("unexpected gate on b: %+v", b.Gate)
	}

	d := iface.Items[2].(*InterfaceFunc)
	if len(d.Gate) != 1 || d.Gate[0].Kind != "unstable" || d.Gate[0].Feature != "fancier-foo" {
		t.Errorf("unexpected gate on d: %+v", d.Gate)
	}

	e := iface.Items[3].(*InterfaceFunc)
	if len(e.Gate) != 2 {
		t.Fatalf("expected 2 stacked gates on e, got %d: %+v", len(e.Gate), e.Gate)
	}
	if e.Gate[0].Kind != "since" || e.Gate[0].Version != "0.2.0" {
		t.Errorf("unexpected first gate on e: %+v", e.Gate[0])
	}
	if e.Gate[1].Kind != "deprecated" || e.Gate[1].Version != "0.2.2" {
		t.Errorf("unexpected second gate on e: %+v", e.Gate[1])
	}
}

// TestWorldItems tests inline world items: import func, export interface,
// use, and include.
func TestWorldItems(t *testing.T) {
	src := `package test:test;

interface console {
  log: func(msg: string);
}

world my-world {
  use console.{log};

  import slugify: func(text: string) -> string;

  export run: func() -> result<_, string>;

  export handler: interface {
    handle: func(req: string) -> string;
  }

  include other-world with { a as a1, b as b1 };
}`

	pkg, err := Parse("test.wit", src)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	if len(pkg.Items) != 2 {
		t.Fatalf("expected 2 top-level items, got %d", len(pkg.Items))
	}

	world, ok := pkg.Items[1].(*World)
	if !ok {
		t.Fatalf("expected World, got %T", pkg.Items[1])
	}
	if len(world.Items) != 5 {
		t.Fatalf("expected 5 world items, got %d", len(world.Items))
	}

	if _, ok := world.Items[0].(*Use); !ok {
		t.Errorf("world.Items[0] is %T, expected *Use", world.Items[0])
	}

	imp, ok := world.Items[1].(*Import)
	if !ok {
		t.Fatalf("world.Items[1] is %T, expected *Import", world.Items[1])
	}
	if imp.Name != "slugify" {
		t.Errorf("import name = %q, expected %q", imp.Name, "slugify")
	}
	if _, ok := imp.Type.(*ImportFunc); !ok {
		t.Errorf("import type = %T, expected *ImportFunc", imp.Type)
	}

	exp, ok := world.Items[2].(*Export)
	if !ok {
		t.Fatalf("world.Items[2] is %T, expected *Export", world.Items[2])
	}
	if exp.Name != "run" {
		t.Errorf("export name = %q, expected %q", exp.Name, "run")
	}

	handlerExp, ok := world.Items[3].(*Export)
	if !ok {
		t.Fatalf("world.Items[3] is %T, expected *Export", world.Items[3])
	}
	handlerIface, ok := handlerExp.Type.(*ExportInterface)
	if !ok {
		t.Fatalf("handler export type = %T, expected *ExportInterface", handlerExp.Type)
	}
	if len(handlerIface.Items) != 1 {
		t.Errorf("inline handler interface has %d items, expected 1", len(handlerIface.Items))
	}

	inc, ok := world.Items[4].(*Include)
	if !ok {
		t.Fatalf("world.Items[4] is %T, expected *Include", world.Items[4])
	}
	if inc.Path != "other-world" {
		t.Errorf("include path = %q, expected %q", inc.Path, "other-world")
	}
	if inc.Renames["a"] != "a1" || inc.Renames["b"] != "b1" {
		t.Errorf("unexpected include renames: %+v", inc.Renames)
	}
}

// TestVersionedUsePath tests a use-path with an "@version" segment
// immediately followed by ".{names}", which previously tripped a lexer bug
// where the version scanner greedily consumed the following '.' (e.g.
// "wasi:io/poll@0.2.8.{pollable}" from real wasi-clocks WIT).
func TestVersionedUsePath(t *testing.T) {
	src := `package test:test;
interface i {
  use wasi:io/poll@0.2.8.{pollable};
}`

	pkg, err := Parse("test.wit", src)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	iface := pkg.Items[0].(*Interface)
	use, ok := iface.Items[0].(*Use)
	if !ok {
		t.Fatalf("expected Use, got %T", iface.Items[0])
	}
	if use.Path != "wasi:io/poll@0.2.8" {
		t.Errorf("use path = %q, expected %q", use.Path, "wasi:io/poll@0.2.8")
	}
	if use.Names["pollable"] != "pollable" {
		t.Errorf("unexpected use names: %+v", use.Names)
	}
}

// BenchmarkParser benchmarks parsing a typical WIT file.
func BenchmarkParser(b *testing.B) {
	src := `package example:test@0.1.0;

interface types {
  type point = record { x: s32, y: s32 };
  type color = variant { red, green, blue, custom(string) };
  type direction = enum { north, south, east, west };
  type permissions = flags { read, write, execute };
  type my-list = list<u8>;
  type my-option = option<string>;
  type my-result = result<u32, string>;
}

interface math {
  add: func(a: u32, b: u32) -> u32;
  multiply: func(a: u32, b: u32) -> u32;
  divide: func(a: u32, b: u32) -> result<u32, string>;
}

world calculator {
  export math: interface {
    add: func(a: u32, b: u32) -> u32;
  }
}
`

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = Parse("benchmark.wit", src)
	}
}
