package binary

import (
	"bytes"
	"slices"
	"testing"
)

// synthComponentImport encodes one `(import "n" (component <typeidx>))`.
func synthComponentImport(name string) []byte {
	body := []byte{0x00} // externname kind
	body = append(body, synthLabel(name)...)
	body = append(body, 0x04, 0x00) // externdesc: component, type index 0
	return body
}

// synthComponentExport encodes one `(export "n" (component <idx>))`.
func synthComponentExport(name string, idx byte) []byte {
	body := []byte{0x00}
	body = append(body, synthLabel(name)...)
	body = append(body, 0x04, idx, 0x00) // sortidx: component <idx>; no ascribed type
	return body
}

// synthOuterComponentAlias encodes section 6 carrying one
// `(alias outer <count> <idx> (component))`.
func synthOuterComponentAlias(count, idx byte) []byte {
	return synthSection(6, []byte{0x01, 0x04, 0x02, count, idx})
}

// The whole point of ComponentSpace: a component IMPORT occupies a component
// index, so a nested component that defines none of its own can still say
// `(instantiate 0)`. That is types.11.wast's `$c` exactly.
func TestComponentSpace_ImportOccupiesAnIndex(t *testing.T) {
	// A component type for the import's externdesc to name.
	typeSec := synthSection(7, []byte{0x01, 0x41, 0x00}) // one componenttype with no decls
	raw := synthComponent(
		typeSec,
		synthSection(10, append([]byte{0x01}, synthComponentImport("x")...)),
		synthNestedComponentSection(synthComponent()),
	)
	c, err := Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	// The section-4 nested component lands at index 1, BEHIND the import --
	// indexing NestedComponents directly would have put it at 0.
	want := []ComponentSpaceEntry{{Kind: ComponentFromImport, Import: 0}, {Kind: ComponentFromDefinition, Component: 0}}
	if !slices.Equal(c.ComponentSpace, want) {
		t.Fatalf("component space = %+v, want %+v", c.ComponentSpace, want)
	}
}

// An export introduces an aliasing index of its own, shifting every later
// definition -- the same rule ComponentInstanceSpace models for instances.
func TestComponentSpace_ExportShiftsLaterDefinitions(t *testing.T) {
	raw := synthComponent(
		synthNestedComponentSection(synthComponent()),                            // component index 0
		synthSection(11, append([]byte{0x01}, synthComponentExport("re", 0)...)), // index 1, aliases 0
		synthNestedComponentSection(synthComponent()),                            // index 2, NOT 1
	)
	c, err := Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := []ComponentSpaceEntry{{Kind: ComponentFromDefinition, Component: 0}, {Kind: ComponentFromExport, Export: 0}, {Kind: ComponentFromDefinition, Component: 1}}
	if !slices.Equal(c.ComponentSpace, want) {
		t.Fatalf("component space = %+v, want %+v", c.ComponentSpace, want)
	}
}

// `component` is one of the four outeraliassort sorts, so a nested component
// can name a component its parent defines.
func TestComponentSpace_OuterAlias(t *testing.T) {
	nested := synthComponent(synthOuterComponentAlias(1, 0))
	raw := synthComponent(
		synthNestedComponentSection(synthComponent()), // parent component index 0: the target
		synthNestedComponentSection(nested),           // parent component index 1: the aliaser
	)
	parent, err := Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	child := parent.NestedComponents[1]
	if want := []ComponentSpaceEntry{{Kind: ComponentFromAlias, Alias: 0}}; !slices.Equal(child.ComponentSpace, want) {
		t.Fatalf("component space = %+v, want %+v", child.ComponentSpace, want)
	}
	if al := child.Aliases[0]; al.Sort != 0x04 || al.TargetKind != 0x02 || al.OuterCount != 1 || al.OuterIndex != 0 || child.Outer != parent {
		t.Errorf("alias %+v", al)
	}
}
