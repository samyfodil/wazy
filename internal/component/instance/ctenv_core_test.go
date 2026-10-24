package instance

import (
	"context"
	"strings"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/component/binary"
)

// The core index spaces of the type check (ctenv_core.go) on hand-built
// components: every way an index resolves, and every way it fails to, down
// to the shapes only a malformed binary has.

// emptyModule is the smallest core module: magic and version.
var emptyModule = []byte{0, 'a', 's', 'm', 1, 0, 0, 0}

// moduleTypeComp is a component whose core type 0 is (module (export "f"
// (func))) and whose core module 0 imports it as "m".
func moduleTypeComp() *binary.Component {
	return &binary.Component{
		CoreTypes: []binary.CoreTypeDef{{Module: &binary.CoreModuleType{Decls: []binary.CoreModuleDecl{
			{Kind: binary.CoreModuleDeclType, Rec: []byte{0x60, 0x00, 0x00}},
			{Kind: binary.CoreModuleDeclExport, Name: "f", Extern: binary.CoreExternType{Kind: 0x00}},
		}}, Count: 1}},
		CoreTypeSpace:   []binary.CoreTypeSpaceEntry{{Kind: binary.CoreTypeFromDefinition}},
		Imports:         []binary.Import{{Name: "m", ExternType: 0x00}},
		CoreModuleSpace: []binary.CoreModuleSpaceEntry{{Kind: binary.CoreModuleFromImport}},
	}
}

func requireCT(t *testing.T, err error, want string) {
	t.Helper()
	if want == "" {
		if err != nil {
			t.Fatal(err)
		}
		return
	}
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("err = %v, want it to contain %q", err, want)
	}
}

func TestCTEnvCoreTypeAt(t *testing.T) {
	up := newCTEnv(&binary.Component{
		CoreTypes:     []binary.CoreTypeDef{{Rec: []byte{0x60, 0x00, 0x00}, Count: 1}},
		CoreTypeSpace: []binary.CoreTypeSpaceEntry{{Kind: binary.CoreTypeFromDefinition}},
	}, nil)
	child := &binary.Component{
		CoreTypeSpace: []binary.CoreTypeSpaceEntry{{Kind: binary.CoreTypeFromAlias}},
		Aliases:       []binary.AliasDef{{Sort: 0x00, CoreSort: 0x10, TargetKind: 0x02, OuterCount: 1}},
	}
	got, err := newCTEnv(child, up).coreTypeAt(0)
	if err != nil || got.fn == nil {
		t.Fatalf("an outer alias: %+v, %v", got, err)
	}
	_, err = newCTEnv(child, nil).coreTypeAt(0)
	requireCT(t, err, "invalid outer alias count of 1")
	child.Aliases[0].TargetKind = 0x01
	_, err = newCTEnv(child, up).coreTypeAt(0)
	requireCT(t, err, "a core type alias must be an outer alias")
	_, err = newCTEnv(child, up).coreTypeAt(1)
	requireCT(t, err, "unknown core type 1: type index out of bounds")
	if got, err := newCTEnv(moduleTypeComp(), nil).coreTypeAt(0); err != nil || got.mod == nil || len(got.mod.exports) != 1 {
		t.Fatalf("a module type: %+v, %v", got, err)
	}
}

func TestCTEnvModuleAt(t *testing.T) {
	c := moduleTypeComp()
	// Module 1 is module 0 exported; module 2 is an outer alias with no
	// component around this one; module 3 is embedded, and does not decode.
	c.Exports = []binary.Export{{Name: "e", ExternType: 0x00}}
	c.Aliases = []binary.AliasDef{{Sort: 0x00, CoreSort: 0x11, TargetKind: 0x02, OuterCount: 1}}
	c.CoreModules = []binary.CoreModule{{Offset: 0, Size: 4}}
	c.Bytes = []byte("junk")
	c.CoreModuleSpace = append(c.CoreModuleSpace,
		binary.CoreModuleSpaceEntry{Kind: binary.CoreModuleFromExport},
		binary.CoreModuleSpaceEntry{Kind: binary.CoreModuleFromAlias},
		binary.CoreModuleSpaceEntry{Kind: binary.CoreModuleFromDefinition})
	e := newCTEnv(c, nil)
	m0, err := e.moduleAt(0)
	if err != nil || len(m0.exports) != 1 {
		t.Fatalf("an import: %+v, %v", m0, err)
	}
	if m1, err := e.moduleAt(1); err != nil || m1 != m0 {
		t.Fatalf("an export: %+v, %v", m1, err)
	}
	_, err = e.moduleAt(2)
	requireCT(t, err, "invalid outer alias count of 1")
	_, err = e.moduleAt(3)
	requireCT(t, err, "core module 3: ")
	_, err = e.moduleAt(4)
	requireCT(t, err, "unknown module 4: module index out of bounds")

	// A module export naming itself, which only a malformed binary has.
	loop := &binary.Component{
		Exports:         []binary.Export{{Name: "e", ExternType: 0x00}},
		CoreModuleSpace: []binary.CoreModuleSpaceEntry{{Kind: binary.CoreModuleFromExport}},
	}
	_, err = newCTEnv(loop, nil).moduleAt(0)
	requireCT(t, err, "module")

	// The same alias, inside a component that has one around it.
	inner := &binary.Component{
		CoreModuleSpace: []binary.CoreModuleSpaceEntry{{Kind: binary.CoreModuleFromAlias}},
		Aliases:         []binary.AliasDef{{Sort: 0x00, CoreSort: 0x11, TargetKind: 0x02, OuterCount: 1}},
	}
	if m, err := newCTEnv(inner, e).moduleAt(0); err != nil || m != m0 {
		t.Fatalf("an outer alias: %+v, %v", m, err)
	}
}

func TestCTEnvCoreInstances(t *testing.T) {
	c := moduleTypeComp()
	// Core instance 0 instantiates the imported module; 1 exports its "f"
	// inline, through the core func alias; 2 instantiates a module that is
	// not there; 3 exports a func that is not there; 4 exports a func a
	// canon makes; 5 exports what core instance 5 itself exports.
	c.Aliases = []binary.AliasDef{
		{Sort: 0x00, CoreSort: 0x00, TargetKind: 0x01, InstanceIdx: 0, Name: "f"},
		{Sort: 0x00, CoreSort: 0x01, TargetKind: 0x01, InstanceIdx: 9, Name: "t"},
		{Sort: 0x00, CoreSort: 0x01, TargetKind: 0x01, InstanceIdx: 0, Name: "nope"},
		{Sort: 0x00, CoreSort: 0x00, TargetKind: 0x01, InstanceIdx: 5, Name: "g"},
	}
	c.CoreFuncSpace = []binary.CoreFuncSpaceEntry{{Kind: binary.CoreFuncFromAlias}, {Kind: binary.CoreFuncFromCanon}, {Kind: binary.CoreFuncFromAlias, Alias: 3}}
	c.CoreInstances = []binary.CoreInstance{
		{Kind: 0x00},
		{Kind: 0x01, Exports: []binary.CoreInlineExport{{Name: "f", Sort: 0x00, CoreSortIdx: 0}}},
		{Kind: 0x00, ModuleIdx: 7},
		{Kind: 0x01, Exports: []binary.CoreInlineExport{{Name: "x", Sort: 0x00, CoreSortIdx: 9}}},
		{Kind: 0x01, Exports: []binary.CoreInlineExport{{Name: "c", Sort: 0x00, CoreSortIdx: 1}}},
		{Kind: 0x01, Exports: []binary.CoreInlineExport{{Name: "g", Sort: 0x00, CoreSortIdx: 2}}},
	}
	e := newCTEnv(c, nil)
	in, err := e.coreInstanceAt(1)
	if err != nil || len(in.exports) != 1 || in.exports[0].ext.fn == nil {
		t.Fatalf("inline exports: %+v, %v", in, err)
	}
	_, err = e.coreInstanceAt(2)
	requireCT(t, err, "unknown module 7")
	_, err = e.coreInstanceAt(3)
	requireCT(t, err, "unknown function 9: function index out of bounds")
	if in, err := e.coreInstanceAt(4); err != nil || in.exports[0].ext.known {
		t.Fatalf("a canon's func: %+v, %v", in, err)
	}
	_, err = e.coreInstanceAt(5)
	requireCT(t, err, "core instance 5")
	_, err = e.coreInstanceAt(6)
	requireCT(t, err, "unknown core instance 6: instance index out of bounds")

	// Tables -- every core sort but func is aliases alone.
	_, err = e.coreItemAt(0x01, 0)
	requireCT(t, err, "unknown core instance 9")
	_, err = e.coreItemAt(0x01, 1)
	requireCT(t, err, "core instance 0 has no export named `nope`")
	_, err = e.coreItemAt(0x01, 2)
	requireCT(t, err, "unknown table 2: table index out of bounds")
	_, err = e.coreItemAt(0x02, 0)
	requireCT(t, err, "unknown memory 0: memory index out of bounds")
}

func TestCTEnvCheckCoreInstances(t *testing.T) {
	// An alias of an inline instance's export of the wrong sort.
	c := &binary.Component{
		CoreInstances: []binary.CoreInstance{{Kind: 0x01, Exports: []binary.CoreInlineExport{{Name: "g", Sort: 0x03}}}},
		Aliases:       []binary.AliasDef{{Sort: 0x00, CoreSort: 0x00, TargetKind: 0x01, Name: "g"}},
	}
	requireCT(t, newCTEnv(c, nil).checkCoreInstances(), "export `g` for core instance 0 is not a func")

	// More aliases than the bookkeeping keeps on the stack, of an instance
	// of an imported module: every one of them found.
	m := moduleTypeComp()
	m.CoreInstances = []binary.CoreInstance{{Kind: 0x00}}
	for range 70 {
		m.Aliases = append(m.Aliases, binary.AliasDef{Sort: 0x00, CoreSort: 0x00, TargetKind: 0x01, Name: "f"})
	}
	requireCT(t, newCTEnv(m, nil).checkCoreInstances(), "")
	m.CoreInstances[0].ModuleIdx = 3
	requireCT(t, newCTEnv(m, nil).checkCoreInstances(), "unknown module 3")
	// Reading the exports of such an instance fails the same way.
	requireCT(t, newCTEnv(m, nil).coreInstanceExports(0, func([]byte, string, byte) {}), "unknown module 3")

	// An embedded module whose bytes are not there, and one whose sections
	// do not read.
	for _, tc := range []struct {
		bytes []byte
		cm    binary.CoreModule
		want  string
	}{
		{nil, binary.CoreModule{Size: 8}, "out of bounds"},
		{append(append([]byte{}, emptyModule...), 7, 9), binary.CoreModule{Size: 10}, "core module 0: section 7"},
	} {
		d := &binary.Component{
			Bytes:           tc.bytes,
			CoreModules:     []binary.CoreModule{tc.cm},
			CoreModuleSpace: []binary.CoreModuleSpaceEntry{{Kind: binary.CoreModuleFromDefinition}},
			CoreInstances:   []binary.CoreInstance{{Kind: 0x00}},
			Aliases:         []binary.AliasDef{{Sort: 0x00, TargetKind: 0x01, Name: "f"}},
		}
		requireCT(t, newCTEnv(d, nil).checkCoreInstances(), tc.want)
	}

	// The instantiation of an imported module, with an argument that is not
	// there.
	a := moduleTypeComp()
	a.CoreInstances = []binary.CoreInstance{{Kind: 0x00, Args: []binary.CoreInstantiateArg{{Name: "", InstanceIdx: 4}}}}
	requireCT(t, newCTEnv(a, nil).checkCoreInstances(), "unknown core instance 4")
}

func TestCTDeclScopeCoreTypes(t *testing.T) {
	up := newCTEnv(moduleTypeComp(), nil)
	funcType := &binary.CoreTypeDef{Rec: []byte{0x60, 0x00, 0x00}, Count: 1}
	moduleDesc := binary.ExternDesc{Sort: ctSortCoreModule}
	for _, tc := range []struct {
		name  string
		decls []binary.TypeDecl
		want  string
	}{
		{"a module type aliased in", []binary.TypeDecl{
			{Kind: binary.TypeDeclAlias, Alias: binary.AliasDef{Sort: 0x00, CoreSort: 0x10, TargetKind: 0x02, OuterCount: 1}},
			{Kind: binary.TypeDeclExport, Name: "m", Desc: moduleDesc},
		}, ""},
		{"a func type is not a module type", []binary.TypeDecl{
			{Kind: binary.TypeDeclCoreType, CoreType: funcType},
			{Kind: binary.TypeDeclExport, Name: "m", Desc: moduleDesc},
		}, "core type index 0 is not a module type"},
		{"no such core type", []binary.TypeDecl{
			{Kind: binary.TypeDeclExport, Name: "m", Desc: moduleDesc},
		}, "unknown core type 0: type index out of bounds"},
		{"an alias too far out", []binary.TypeDecl{
			{Kind: binary.TypeDeclAlias, Alias: binary.AliasDef{Sort: 0x00, CoreSort: 0x10, TargetKind: 0x02, OuterCount: 2}},
		}, "invalid outer alias count of 2"},
		{"an alias of nothing", []binary.TypeDecl{
			{Kind: binary.TypeDeclAlias, Alias: binary.AliasDef{Sort: 0x00, CoreSort: 0x10, TargetKind: 0x02, OuterCount: 1, OuterIndex: 5}},
		}, "unknown core type 5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := ctBuildDeclType(up, tc.decls, false)
			requireCT(t, err, tc.want)
		})
	}
}

// A destructor's core function must have type [rep] -> []: one of an i64
// resource takes an i64, one that does not resolve is skipped, and one of
// any other type is refused.
func TestCheckResourceDtors(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)
	mod, err := r.Instantiate(ctx, ctcoreFixture(t, "dtors"))
	if err != nil {
		t.Fatal(err)
	}
	zero := uint32(0)
	comp := &binary.Component{
		Types:     []binary.Type{{Descriptor: binary.ResourceDesc{Rep: binary.TypeRef{Primitive: "i64"}, Dtor: &zero}}},
		TypeSpace: []binary.TypeSpaceEntry{{Kind: binary.TypeSpaceDef}},
	}
	byName := func(name string) func(uint32) api.Function {
		return func(uint32) api.Function { return mod.ExportedFunction(name) }
	}
	requireCT(t, checkResourceDtors(comp, byName("i64")), "")
	requireCT(t, checkResourceDtors(comp, func(uint32) api.Function { return nil }), "")
	requireCT(t, checkResourceDtors(comp, byName("bad")), "core function 0 has wrong signature for a destructor")
}
