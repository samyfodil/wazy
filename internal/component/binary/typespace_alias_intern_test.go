package binary

import (
	"reflect"
	"sync"
	"testing"
)

// useIfaceComponent builds the `use iface.{T}` shape resolveAlias's imported-
// instance branch exists for: type index 1 is a type-sort alias exporting "t"
// from imported instance 0, whose declared instancetype holds a record whose
// field references ANOTHER type in that instancetype's own local index space --
// so resolving it must globalize, and intern, into extraTypes.
func useIfaceComponent() *Component {
	localInner := uint32(1)
	instType := InstanceDesc{
		Types: []TypeDesc{
			RecordDesc{Fields: []RecordField{{Name: "f", Type: TypeRef{TypeIndex: &localInner}}}},
			PrimitiveDesc{Prim: "u32"},
		},
		Exports: map[string]TypeRef{"t": {TypeIndex: new(uint32)}}, // local index 0
	}
	return &Component{
		Types: []Type{{Index: 0, Kind: "instance", Descriptor: instType}},
		TypeSpace: []TypeSpaceEntry{
			{Kind: TypeSpaceDef, Def: 0},
			{Kind: TypeSpaceAlias, Alias: 0},
		},
		Imports: []Import{{Name: "iface", ExternType: 0x05, ExternIndex: 0}},
		Aliases: []AliasDef{{Sort: 0x03, TargetKind: 0x00, InstanceIdx: 0, Name: "t"}},
		ComponentInstanceSpace: []ComponentInstanceSpaceEntry{
			{Kind: ComponentInstanceFromImport, Import: 0},
		},
	}
}

// Every resolution used to globalize afresh (a new memo per call), so the same
// type interned again and came back holding different escape indices. Two
// consequences, both bad: extraTypes grew for the life of the Component, and
// reflect.DeepEqual said a type did not equal itself -- which is exactly how
// task.return decides whether a result type matches its binding
// (instance/async_builtins.go).
func TestResolveTypeInternsAliasedImportOnce(t *testing.T) {
	c := useIfaceComponent()

	first, err := c.ResolveType(1)
	if err != nil {
		t.Fatalf("first ResolveType: %v", err)
	}
	afterFirst := len(c.extraTypes)
	if afterFirst == 0 {
		t.Fatal("expected the first resolution to intern at least one extra type")
	}

	second, err := c.ResolveType(1)
	if err != nil {
		t.Fatalf("second ResolveType: %v", err)
	}
	if got := len(c.extraTypes); got != afterFirst {
		t.Errorf("extraTypes grew on re-resolution: %d -> %d", afterFirst, got)
	}
	if !reflect.DeepEqual(first, second) {
		t.Errorf("two resolutions of one type are not DeepEqual:\n first=%#v\nsecond=%#v", first, second)
	}

	// Ten more must not move it either -- an exported func resolves its result
	// type once per call, not once per component.
	for i := 0; i < 10; i++ {
		if _, err := c.ResolveType(1); err != nil {
			t.Fatalf("ResolveType: %v", err)
		}
	}
	if got := len(c.extraTypes); got != afterFirst {
		t.Errorf("extraTypes grew over repeated resolutions: %d -> %d", afterFirst, got)
	}
}

// One decoded Component backs every instance made from it, and resolution
// mutates it. Run under -race.
func TestResolveTypeConcurrent(t *testing.T) {
	c := useIfaceComponent()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				if _, err := c.ResolveType(1); err != nil {
					t.Errorf("ResolveType: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
}
