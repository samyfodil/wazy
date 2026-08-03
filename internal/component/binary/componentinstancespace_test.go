package binary

import (
	"bytes"
	"testing"
)

// synthComponent assembles a component binary from already-encoded sections,
// in the order given -- which is exactly what the index spaces are built from.
func synthComponent(sections ...[]byte) []byte {
	var b bytes.Buffer
	b.Write([]byte{0x00, 0x61, 0x73, 0x6d}) // magic
	b.Write([]byte{0x0d, 0x00})             // version 13
	b.Write([]byte{0x01, 0x00})             // layer 1 (component)
	for _, s := range sections {
		b.Write(s)
	}
	return b.Bytes()
}

func synthSection(id byte, body []byte) []byte {
	out := []byte{id}
	out = append(out, byte(len(body))) // every body here is < 128 bytes
	return append(out, body...)
}

func synthLabel(s string) []byte {
	return append([]byte{byte(len(s))}, s...)
}

// synthInstanceImport encodes one `(import "n" (instance <typeidx>))`.
func synthInstanceImport(name string) []byte {
	body := []byte{0x00} // externname kind
	body = append(body, synthLabel(name)...)
	body = append(body, 0x05, 0x00) // externdesc: instance, type index 0
	return body
}

// synthInlineInstance encodes one inline-export instance definition with no
// members -- enough to occupy an index-space slot.
func synthInlineInstance() []byte { return []byte{0x01, 0x00} }

// synthInstanceExport encodes one `(export "n" (instance <idx>))`.
func synthInstanceExport(name string, idx byte) []byte {
	body := []byte{0x00}
	body = append(body, synthLabel(name)...)
	body = append(body, 0x05, idx, 0x00) // sortidx: instance <idx>; no ascribed type
	return body
}

// The whole point of ComponentInstanceSpace: an instance export introduces an
// aliasing index, so a definition declared AFTER it lands one slot later than
// the flat [imports]++[definitions] model predicts. This is the exact shape
// wit-component emits for a component with two exported interfaces.
func TestComponentInstanceSpace_ExportsShiftLaterDefinitions(t *testing.T) {
	// An instance type must exist for the import's externdesc to name.
	typeSec := synthSection(7, []byte{0x01, 0x42, 0x00}) // one instancetype with no decls
	raw := synthComponent(
		typeSec,
		synthSection(10, append([]byte{0x01}, synthInstanceImport("imported")...)),
		synthSection(5, append([]byte{0x01}, synthInlineInstance()...)),
		synthSection(11, append([]byte{0x01}, synthInstanceExport("first", 1)...)),
		synthSection(5, append([]byte{0x01}, synthInlineInstance()...)),
		synthSection(11, append([]byte{0x01}, synthInstanceExport("second", 3)...)),
	)
	c, err := Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := []ComponentInstanceSpaceEntry{
		{Kind: ComponentInstanceFromImport, Import: 0},
		{Kind: ComponentInstanceFromDefinition, Instance: 0},
		{Kind: ComponentInstanceFromExport, Export: 0},
		{Kind: ComponentInstanceFromDefinition, Instance: 1},
		{Kind: ComponentInstanceFromExport, Export: 1},
	}
	if len(c.ComponentInstanceSpace) != len(want) {
		t.Fatalf("instance space: got %+v, want %+v", c.ComponentInstanceSpace, want)
	}
	for i := range want {
		if c.ComponentInstanceSpace[i] != want[i] {
			t.Errorf("instance space[%d]: got %+v, want %+v", i, c.ComponentInstanceSpace[i], want[i])
		}
	}

	// The second export's sortidx (3) must resolve to the SECOND definition.
	// The flat model would compute 3-1 == 2, past the end of Instances.
	if def, ok := c.ResolveComponentInstance(3); !ok || def != 1 {
		t.Errorf("ResolveComponentInstance(3): got (%d, %v), want (1, true)", def, ok)
	}
	if def, ok := c.ResolveComponentInstance(1); !ok || def != 0 {
		t.Errorf("ResolveComponentInstance(1): got (%d, %v), want (0, true)", def, ok)
	}
	// An export index resolves THROUGH to the definition it aliases.
	if def, ok := c.ResolveComponentInstance(2); !ok || def != 0 {
		t.Errorf("ResolveComponentInstance(2): got (%d, %v), want (0, true)", def, ok)
	}
	// An imported instance is not a local definition.
	if _, ok := c.ResolveComponentInstance(0); ok {
		t.Error("ResolveComponentInstance(0) names an import; want ok=false")
	}
	// Out of range.
	if _, ok := c.ResolveComponentInstance(99); ok {
		t.Error("ResolveComponentInstance(99) is out of range; want ok=false")
	}
}

func TestResolveComponentInstance_UnpopulatedAndDegenerate(t *testing.T) {
	// A hand-built Component has no index space at all.
	var c Component
	if _, ok := c.ResolveComponentInstance(0); ok {
		t.Error("empty index space: want ok=false")
	}
	// An entry pointing past Instances / Exports must fail loud rather than
	// panic.
	c.ComponentInstanceSpace = []ComponentInstanceSpaceEntry{
		{Kind: ComponentInstanceFromDefinition, Instance: 5},
		{Kind: ComponentInstanceFromExport, Export: 5},
		{Kind: ComponentInstanceFromAlias, Alias: 0},
	}
	for i := range c.ComponentInstanceSpace {
		if _, ok := c.ResolveComponentInstance(uint32(i)); ok {
			t.Errorf("entry %d resolves; want ok=false", i)
		}
	}
	// A self-referential export chain terminates instead of looping forever.
	c.ComponentInstanceSpace = []ComponentInstanceSpaceEntry{{Kind: ComponentInstanceFromExport, Export: 0}}
	c.Exports = []Export{{Name: "loop", ExternType: 0x05, ExternIndex: 0}}
	if _, ok := c.ResolveComponentInstance(0); ok {
		t.Error("self-referential export chain resolves; want ok=false")
	}
}
