package instance

import (
	"bytes"
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/samyfodil/wazy"

	"github.com/samyfodil/wazy/internal/component/binary"
)

// resolveInstanceArgTarget is the single classifier for an instance-sort
// instantiate-arg index. It has to separate three things that are satisfied in
// completely different ways: a SIBLING sub-instance, an instance the component
// itself imports and passes straight through (resources.14.wast -- no runtime
// object, only re-keyed cfg entries), and an instance ALIAS, which instantiates
// nothing and instead projects an instance-typed export out of one of those
// two. Anything it cannot model must come back as an error, never as a
// plausible-looking target.
func TestResolveInstanceArgTarget(t *testing.T) {
	decoded := &binary.Component{
		Decoded:   true,
		Imports:   []binary.Import{{Name: "host", ExternType: 0x05}},
		Exports:   []binary.Export{{Name: "re", ExternType: 0x05, ExternIndex: 0}},
		Instances: []binary.Instance{{Kind: 0x00}},
		Aliases: []binary.AliasDef{
			{Sort: 0x05, TargetKind: 0x00, InstanceIdx: 2, Name: "iface"}, // 0: an export of the local definition
			{Sort: 0x05, TargetKind: 0x00, InstanceIdx: 3, Name: "inner"}, // 1: an export of alias 0
			{Sort: 0x05, TargetKind: 0x00, InstanceIdx: 0, Name: "x"},     // 2: an export of the IMPORT
			{Sort: 0x05, TargetKind: 0x02, OuterCount: 1},                 // 3: `alias outer`
			{Sort: 0x01, TargetKind: 0x00, InstanceIdx: 2, Name: "f"},     // 4: not an instance alias at all
		},
		ComponentInstanceSpace: []binary.ComponentInstanceSpaceEntry{
			{Kind: binary.ComponentInstanceFromImport, Import: 0},       // 0
			{Kind: binary.ComponentInstanceFromExport, Export: 0},       // 1 -> aliases 0
			{Kind: binary.ComponentInstanceFromDefinition, Instance: 0}, // 2
			{Kind: binary.ComponentInstanceFromAlias, Alias: 0},         // 3 -> 2 . "iface"
			{Kind: binary.ComponentInstanceFromAlias, Alias: 1},         // 4 -> 3 . "inner"
			{Kind: binary.ComponentInstanceFromAlias, Alias: 2},         // 5 -> 0 . "x"
			{Kind: binary.ComponentInstanceFromAlias, Alias: 3},         // 6
			{Kind: binary.ComponentInstanceFromAlias, Alias: 4},         // 7
			{Kind: binary.ComponentInstanceFromAlias, Alias: 99},        // 8
		},
	}
	// A cycle guard case: an export entry naming its own slot.
	cyclic := &binary.Component{
		Exports: []binary.Export{{ExternType: 0x05, ExternIndex: 0}},
		ComponentInstanceSpace: []binary.ComponentInstanceSpaceEntry{
			{Kind: binary.ComponentInstanceFromExport, Export: 0},
		},
	}
	tests := []struct {
		name    string
		comp    *binary.Component
		idx     uint32
		want    instanceArgTarget
		wantErr string
	}{
		{name: "an instance import", comp: decoded, idx: 0, want: instanceArgTarget{imported: true, importName: "host"}},
		{name: "an export aliasing that import", comp: decoded, idx: 1, want: instanceArgTarget{imported: true, importName: "host"}},
		{name: "a local definition", comp: decoded, idx: 2, want: instanceArgTarget{spaceIdx: 2}},
		// The composition shape: the arg names an alias, so the target is the
		// PROVIDER's definition plus the export name to project out of it.
		{name: "an instance-export alias of a local definition", comp: decoded, idx: 3, want: instanceArgTarget{spaceIdx: 2, projection: []string{"iface"}}},
		// Innermost-first accumulation must come back base-first.
		{name: "an alias of that alias", comp: decoded, idx: 4, want: instanceArgTarget{spaceIdx: 2, projection: []string{"iface", "inner"}}},
		{name: "an instance-export alias of an import", comp: decoded, idx: 5, want: instanceArgTarget{imported: true, importName: "host", projection: []string{"x"}}},
		{name: "an `alias outer` instance", comp: decoded, idx: 6, wantErr: "target kind 0x2"},
		{name: "an alias whose sort is not instance", comp: decoded, idx: 7, wantErr: "sort is 0x1"},
		{name: "an alias entry naming a missing alias", comp: decoded, idx: 8, wantErr: "past the end of the 5 alias(es)"},
		{name: "past the end of the space", comp: decoded, idx: 9, wantErr: "past the end of the 9-entry component instance index space"},
		{name: "an export entry naming a missing export", comp: &binary.Component{
			ComponentInstanceSpace: []binary.ComponentInstanceSpaceEntry{{Kind: binary.ComponentInstanceFromExport, Export: 4}},
		}, idx: 0, wantErr: "past the end of the 0 export(s)"},
		{name: "an import entry naming a missing import", comp: &binary.Component{
			ComponentInstanceSpace: []binary.ComponentInstanceSpaceEntry{{Kind: binary.ComponentInstanceFromImport, Import: 4}},
		}, idx: 0, wantErr: "past the end of the 0 import(s)"},
		{name: "a cyclic export chain", comp: cyclic, idx: 0, wantErr: "exceeds 64 links"},
		// A hand-built Component has no instance index space, so the flat
		// [imports] ++ [definitions] fallback classifies it. No alias is
		// representable there, so no projection can ever arise.
		{name: "hand-built: below the import count", comp: &binary.Component{
			Imports: []binary.Import{{Name: "wasi:io/streams", ExternType: 0x05}},
		}, idx: 0, want: instanceArgTarget{imported: true, importName: "wasi:io/streams"}},
		{name: "hand-built: at or above the import count", comp: &binary.Component{
			Imports: []binary.Import{{Name: "wasi:io/streams", ExternType: 0x05}},
		}, idx: 1, want: instanceArgTarget{spaceIdx: 1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveInstanceArgTarget(tt.comp, tt.idx)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("resolveInstanceArgTarget(%d) error = %v, want it to contain %q", tt.idx, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveInstanceArgTarget(%d): %v", tt.idx, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("resolveInstanceArgTarget(%d) = %+v, want %+v", tt.idx, got, tt.want)
			}
		})
	}
}

// forwardImportedInstance re-keys the embedder's flat, name-keyed entries from
// the OUTER component's import name to the arg name (which is the nested
// component's own import name). The rename is the load-bearing part: the two
// names are equal in resources.14.wast only by coincidence.
func TestForwardImportedInstance_RekeysUnderTheArgName(t *testing.T) {
	hi := &hostImport{}
	dtorRan := false
	dtor := func(context.Context, uint32) error { dtorRan = true; return nil }
	cfg := &config{
		imports: map[importKey]*hostImport{
			mkImportKey("outer-host@0.2.4", "make"): hi,
			// An entry for a DIFFERENT interface must not be forwarded.
			mkImportKey("wasi:io/streams", "read"): {},
		},
		resourceTags: map[importKey]uint32{
			mkImportKey("outer-host", "thing"): 77,
			// A resource of this interface the nested component does NOT
			// import: still re-keyed for a deeper level, but it contributes
			// no typeArgTags entry.
			mkImportKey("outer-host", "unused"):    78,
			mkImportKey("wasi:io/streams", "pipe"): 79,
		},
		hostResDtors: map[uint32]func(ctx context.Context, rep uint32) error{77: dtor},
	}
	subCfg := &config{
		imports:      map[importKey]*hostImport{},
		hostResDtors: map[uint32]func(ctx context.Context, rep uint32) error{},
	}
	// The nested component imports the same instance under a DIFFERENT name,
	// and names the resource through a type-sort alias exporting "thing" from
	// that import -- the shape importedResourceIndices recognizes.
	nested := &binary.Component{
		Decoded:   true,
		Imports:   []binary.Import{{Name: "inner-host", ExternType: 0x05}},
		Aliases:   []binary.AliasDef{{Sort: 0x03, TargetKind: 0x00, InstanceIdx: 0, Name: "thing"}},
		TypeSpace: []binary.TypeSpaceEntry{{Kind: binary.TypeSpaceAlias, Alias: 0}},
	}
	typeArgTags := map[uint32]uint32{}
	forwardImportedInstance(cfg, subCfg, nested, "outer-host@0.2.4", "inner-host", typeArgTags)

	if got := subCfg.imports[mkImportKey("inner-host", "make")]; got != hi {
		t.Errorf("host func not re-keyed under the arg name: %+v", subCfg.imports)
	}
	if got, ok := subCfg.resourceTags[mkImportKey("inner-host", "thing")]; !ok || got != 77 {
		t.Errorf("resource tag = (%d, %v), want (77, true)", got, ok)
	}
	if got, ok := typeArgTags[0]; !ok || got != 77 {
		t.Errorf("typeArgTags[0] = (%d, %v), want (77, true); the nested component's own resource index must carry the outer tag", got, ok)
	}
	if len(typeArgTags) != 1 {
		t.Errorf("typeArgTags = %v, want only the resource the nested component actually imports", typeArgTags)
	}
	// The definer's destructor travels with the tag, so the nested
	// component's own resource.drop still runs it.
	if d := subCfg.hostResDtors[77]; d == nil {
		t.Error("host resource destructor was not forwarded with its tag")
	} else if err := d(context.Background(), 0); err != nil || !dtorRan {
		t.Errorf("forwarded destructor did not run: err=%v ran=%v", err, dtorRan)
	}
	// Nothing from another interface leaks across.
	if _, ok := subCfg.imports[mkImportKey("inner-host", "read")]; ok {
		t.Error("an unrelated interface's host func was forwarded")
	}
	if _, ok := subCfg.resourceTags[mkImportKey("inner-host", "pipe")]; ok {
		t.Error("an unrelated interface's resource tag was forwarded")
	}
	if got, ok := subCfg.resourceTags[mkImportKey("inner-host", "unused")]; !ok || got != 78 {
		t.Errorf("resourceTags[unused] = (%d, %v), want (78, true) for a deeper level to resolve", got, ok)
	}
}

// resolveComponentDef is the component index space plus the one thing the
// space cannot answer on its own: what the parent supplied for a
// component-sort IMPORT (types.11.wast).
func TestResolveComponentDef(t *testing.T) {
	supplied := &binary.Component{}
	comp := &binary.Component{
		Decoded: true,
		Imports: []binary.Import{{Name: "x", ExternType: 0x04}},
		ComponentSpace: []binary.ComponentSpaceEntry{
			{Kind: binary.ComponentFromImport, Import: 0},
		},
	}

	// Unsatisfied: nothing supplied a definition for the import.
	if _, err := resolveComponentDef(comp, &config{}, 0); err == nil ||
		!strings.Contains(err.Error(), `names the component import "x", which nothing supplied`) {
		t.Fatalf("unsatisfied component import error = %v, want it to name the import", err)
	}
	// cfg == nil is the bind-time shape: a component import is simply
	// unsatisfiable there, which is what it has always been.
	if _, err := resolveComponentDef(comp, nil, 0); err == nil {
		t.Fatal("expected a nil cfg to leave a component import unsatisfied")
	}
	// Satisfied by the parent's `(with "x" (component N))` arg.
	got, err := resolveComponentDef(comp, &config{componentArgs: map[string]*binary.Component{"x": supplied}}, 0)
	if err != nil {
		t.Fatalf("resolveComponentDef: %v", err)
	}
	if got != supplied {
		t.Errorf("resolveComponentDef = %p, want the supplied definition %p", got, supplied)
	}
	// An out-of-range index still fails loud, through the binary package.
	if _, err := resolveComponentDef(comp, &config{}, 5); err == nil ||
		!strings.Contains(err.Error(), "out of range of the 1-entry component index space") {
		t.Fatalf("out-of-range error = %v, want the index-space message", err)
	}
}

// A ROOT component that imports a COMPONENT is still rejected: only an
// enclosing component's `(with "x" (component N))` instantiate-arg can supply
// a component definition, and there is deliberately no Option that does. This
// is what the official suite's simple.4 ("root-level component imports are not
// supported") asserts.
func TestGraph_RootLevelComponentImportRejected(t *testing.T) {
	comp := decodeRealHello(t)
	comp.Imports[0].ExternType = 0x04 // component
	_, err := runGraph(t, comp)
	requireErrContains(t, err, "only instance imports are supported")
}

// The same import IS satisfiable once an enclosing component supplied a
// definition for it -- the path instantiateNestedInstances takes for a 0x04
// instantiate-arg.
func TestGraph_ComponentImportSatisfiedByAParentArg(t *testing.T) {
	comp := decodeRealHello(t)
	comp.Imports[0].ExternType = 0x04
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	t.Cleanup(func() { r.Close(ctx) })
	cfg := newConfig(nil)
	cfg.componentArgs = map[string]*binary.Component{comp.Imports[0].Name: {}}
	// It gets past the import gate; whatever happens after is this fixture's
	// own business (its remaining imports are unregistered), so only assert
	// that the gate itself no longer rejects it.
	_, err := instantiateGraph(ctx, r, comp, realHelloWasm, cfg)
	if err != nil && strings.Contains(err.Error(), "only instance imports are supported") {
		t.Fatalf("a supplied component import must pass the import gate, got: %v", err)
	}
}

// An instance-sort instantiate-arg that names nothing at all in the component
// instance index space still fails loud, with the resolver's own explanation
// wrapped into the sentence. Every index the space CAN produce -- a sibling
// definition, an import, an export alias, an instance alias -- is resolved
// (see resolveInstanceArgTarget); only a malformed one lands here.
func TestGraph_InstanceArgIsNeitherSiblingNorImport(t *testing.T) {
	// resources.14 is the fixture whose `(with "host" (instance $host))` names
	// an imported instance; pointing that arg past every space slot makes it
	// name nothing at all.
	raw, err := wastFS.ReadFile("testdata/wast/resources/resources.14.wasm")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	comp, err := binary.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	var found bool
	for i := range comp.Instances {
		for j := range comp.Instances[i].Args {
			if comp.Instances[i].Args[j].Sort == 0x05 {
				comp.Instances[i].Args[j].SortIdx = 9999
				found = true
			}
		}
	}
	if !found {
		t.Fatal("resources.14 was expected to carry an instance-sort instantiate-arg")
	}
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	t.Cleanup(func() { r.Close(ctx) })
	_, err = instantiateGraph(ctx, r, comp, raw, newConfig(nil))
	requireErrContains(t, err, "neither a prior nested instantiation nor an imported instance")
}
