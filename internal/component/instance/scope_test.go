package instance

import (
	"strings"
	"testing"

	"github.com/samyfodil/wazy/internal/component/binary"
)

// The scope's walk over hand-built index spaces: every producer of every
// sort it resolves, and every way that walk can fail. The vendored
// wasmtime-nested and wasm-tools-instantiate suites run the decoded shapes
// (TestWastValidation); these are the corners no valid binary reaches.
func TestScope_Resolve(t *testing.T) {
	inner := &binary.Component{}
	// A module defined, re-exported, and an instance's export aliased out;
	// a component defined and re-exported; an inline bundle carrying both,
	// another carrying the first as an instance, and that instance aliased
	// out of it.
	comp := &binary.Component{
		CoreModules: []binary.CoreModule{{}},
		CoreModuleSpace: []binary.CoreModuleSpaceEntry{
			{Kind: binary.CoreModuleFromDefinition, Module: 0},
			{Kind: binary.CoreModuleFromExport, Export: 0},
			{Kind: binary.CoreModuleFromAlias, Alias: 1}, // (core module $alias "m")
		},
		NestedComponents: []*binary.Component{inner},
		ComponentSpace: []binary.ComponentSpaceEntry{
			{Kind: binary.ComponentFromDefinition, Component: 0},
			{Kind: binary.ComponentFromExport, Export: 1},
		},
		Instances: []binary.Instance{
			{Kind: 0x01, Exports: []binary.InlineExport{{Name: "m", Sort: ctSortCoreModule, SortIdx: 1}, {Name: "c", Sort: ctSortComponent, SortIdx: 1}}},
			{Kind: 0x01, Exports: []binary.InlineExport{{Name: "i", Sort: ctSortInstance, SortIdx: 0}}},
		},
		ComponentInstanceSpace: []binary.ComponentInstanceSpaceEntry{
			{Kind: binary.ComponentInstanceFromDefinition, Instance: 0},
			{Kind: binary.ComponentInstanceFromDefinition, Instance: 1},
			{Kind: binary.ComponentInstanceFromAlias, Alias: 0}, // (instance $1 "i")
			{Kind: binary.ComponentInstanceFromExport, Export: 2},
		},
		Aliases: []binary.AliasDef{
			{Sort: ctSortInstance, TargetKind: 0x00, InstanceIdx: 1, Name: "i"},
			{Sort: 0x00, CoreSort: 0x11, TargetKind: 0x00, InstanceIdx: 3, Name: "m"},
		},
		Exports: []binary.Export{
			{Name: "m", ExternType: ctSortCoreModule, ExternIndex: 0},
			{Name: "c", ExternType: ctSortComponent, ExternIndex: 0},
			{Name: "i", ExternType: ctSortInstance, ExternIndex: 2},
		},
	}
	s := &scope{comp: comp}
	for idx := uint32(0); idx < 3; idx++ {
		if owner, m, err := s.module(idx); err != nil || owner != comp || m != 0 {
			t.Errorf("module(%d) = (%p, %d, %v), want (comp, 0)", idx, owner, m, err)
		}
	}
	if cv, err := s.component(1); err != nil || cv.def != inner || cv.env != s {
		t.Errorf("component(1) = (%+v, %v), want inner defined in s", cv, err)
	}
	ds, idx, err := s.instanceExport(3, "c", ctSortComponent)
	if err != nil || ds != s || idx != 1 {
		t.Errorf("instanceExport(3, c) = (%p, %d, %v), want (s, 1)", ds, idx, err)
	}
	if _, _, err := s.instanceExport(3, "m", ctSortComponent); err == nil || !strings.Contains(err.Error(), `instance 3 exports no component "m"`) {
		t.Errorf("a bundle export of the wrong sort = %v, want it refused", err)
	}

	// A hand-built component's modules and components are its spaces.
	flat := &scope{comp: &binary.Component{CoreModules: []binary.CoreModule{{}}, NestedComponents: []*binary.Component{inner}}}
	if owner, m, err := flat.module(0); err != nil || owner != flat.comp || m != 0 {
		t.Errorf("hand-built module(0) = (%p, %d, %v)", owner, m, err)
	}
	if cv, err := flat.component(0); err != nil || cv.def != inner {
		t.Errorf("hand-built component(0) = (%+v, %v)", cv, err)
	}
}

// An instantiation's exports are the instantiated component's, read in a
// scope of their own: its imports are the instantiation's args, and its
// outer aliases close over the scope it was defined in.
func TestScope_InstantiationClosesOverItsDefinition(t *testing.T) {
	parent := &binary.Component{
		CoreModules: []binary.CoreModule{{}},
		CoreModuleSpace: []binary.CoreModuleSpaceEntry{
			{Kind: binary.CoreModuleFromImport, Import: 0},     // (import "p" (core module))
			{Kind: binary.CoreModuleFromDefinition, Module: 0}, // the module passed in
		},
		Imports: []binary.Import{{Name: "p", ExternType: ctSortCoreModule}},
	}
	// child: (import "a" (core module)), (alias outer 1 0 (core module)) --
	// the parent's IMPORT -- and exports both.
	child := &binary.Component{
		Outer: parent,
		CoreModuleSpace: []binary.CoreModuleSpaceEntry{
			{Kind: binary.CoreModuleFromImport, Import: 0},
			{Kind: binary.CoreModuleFromAlias, Alias: 0},
		},
		Imports: []binary.Import{{Name: "a", ExternType: ctSortCoreModule}},
		Aliases: []binary.AliasDef{{Sort: 0x00, CoreSort: 0x11, TargetKind: 0x02, OuterCount: 1, OuterIndex: 0}},
		Exports: []binary.Export{
			{Name: "a", ExternType: ctSortCoreModule, ExternIndex: 0},
			{Name: "o", ExternType: ctSortCoreModule, ExternIndex: 1},
		},
	}
	parent.NestedComponents = []*binary.Component{child}
	parent.ComponentSpace = []binary.ComponentSpaceEntry{{Kind: binary.ComponentFromDefinition}}
	parent.Instances = []binary.Instance{{Kind: 0x00, ComponentIdx: 0, Args: []binary.InstantiateArg{{Name: "a", Sort: ctSortCoreModule, SortIdx: 1}}}}
	parent.ComponentInstanceSpace = []binary.ComponentInstanceSpaceEntry{{Kind: binary.ComponentInstanceFromDefinition}}

	root := &binary.Component{CoreModules: []binary.CoreModule{{}}}
	rootScope := &scope{comp: root}
	// parent, instantiated by root with root's module 0 for "p".
	ps := &scope{comp: parent, inst: &binary.Instance{Args: []binary.InstantiateArg{{Name: "p", Sort: ctSortCoreModule, SortIdx: 0}}}, from: rootScope}

	for _, tc := range []struct {
		name  string
		owner *binary.Component
	}{{"a", parent}, {"o", root}} {
		ds, idx, err := ps.instanceExport(0, tc.name, ctSortCoreModule)
		if err != nil {
			t.Fatalf("instanceExport(0, %q): %v", tc.name, err)
		}
		owner, m, err := ds.module(idx)
		if err != nil || owner != tc.owner || m != 0 {
			t.Errorf("export %q = (%p, %d, %v), want module 0 of %p", tc.name, owner, m, err, tc.owner)
		}
	}

	// Resolved statically -- no instantiation of parent -- its import names
	// nothing, and the child's outer alias falls back to parent itself.
	if _, _, err := (&scope{comp: child}).module(1); err == nil || !strings.Contains(err.Error(), `module 0 names the module import "p", which nothing supplied`) {
		t.Errorf("a static outer alias of an import = %v, want it unsupplied", err)
	}
}

func TestScope_Errors(t *testing.T) {
	cyclic := &binary.Component{
		CoreModuleSpace: []binary.CoreModuleSpaceEntry{{Kind: binary.CoreModuleFromAlias}},
		Aliases:         []binary.AliasDef{{Sort: 0x00, CoreSort: 0x11, TargetKind: 0x02, OuterCount: 0, OuterIndex: 0}},
	}
	outerless := &binary.Component{
		CoreModuleSpace: []binary.CoreModuleSpaceEntry{{Kind: binary.CoreModuleFromAlias}},
		Aliases:         []binary.AliasDef{{Sort: 0x00, CoreSort: 0x11, TargetKind: 0x02, OuterCount: 1}},
	}
	// An export alias of an instance past the space, and of one whose
	// component is past the component space.
	badInst := &binary.Component{
		CoreModuleSpace: []binary.CoreModuleSpaceEntry{{Kind: binary.CoreModuleFromAlias}},
		Aliases:         []binary.AliasDef{{Sort: 0x00, CoreSort: 0x11, TargetKind: 0x00, InstanceIdx: 2, Name: "m"}},
	}
	badComp := &binary.Component{
		CoreModuleSpace:        []binary.CoreModuleSpaceEntry{{Kind: binary.CoreModuleFromAlias}},
		Aliases:                []binary.AliasDef{{Sort: 0x00, CoreSort: 0x11, TargetKind: 0x00, InstanceIdx: 0, Name: "m"}},
		Instances:              []binary.Instance{{Kind: 0x00, ComponentIdx: 4}},
		ComponentInstanceSpace: []binary.ComponentInstanceSpaceEntry{{Kind: binary.ComponentInstanceFromDefinition}},
	}
	for _, tc := range []struct {
		name string
		comp *binary.Component
		want string
	}{
		{"cycle", cyclic, "export/alias chain exceeds 64 links"},
		{"outer alias past the root", outerless, "outer alias (count=1) reaches past the outermost component"},
		{"alias of an instance past the space", badInst, "instance 2 out of range of the 0-entry instance index space"},
		{"instance of a component past the space", badComp, "component 4 out of range of the 0-entry component index space"},
		{"module past the space", &binary.Component{}, "module 0 out of range of the 0-entry module index space"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := (&scope{comp: tc.comp}).module(0); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("module(0) = %v, want an error containing %q", err, tc.want)
			}
		})
	}
	if _, err := (&scope{comp: &binary.Component{}}).component(0); err == nil || !strings.Contains(err.Error(), "component 0 out of range of the 0-entry component index space") {
		t.Errorf("component(0) = %v, want it out of range", err)
	}
}
