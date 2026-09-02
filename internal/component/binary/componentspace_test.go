package binary

import (
	"bytes"
	"strings"
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
	if got, want := len(c.ComponentSpace), 2; got != want {
		t.Fatalf("component space = %d entries, want %d: %+v", got, want, c.ComponentSpace)
	}
	def, importName, err := c.ResolveComponent(0)
	if err != nil {
		t.Fatalf("ResolveComponent(0): %v", err)
	}
	if def != nil || importName != "x" {
		t.Errorf("ResolveComponent(0) = (%v, %q), want (nil, \"x\")", def, importName)
	}
	// The section-4 nested component lands at index 1, BEHIND the import --
	// indexing NestedComponents directly would have put it at 0.
	def, importName, err = c.ResolveComponent(1)
	if err != nil {
		t.Fatalf("ResolveComponent(1): %v", err)
	}
	if def != c.NestedComponents[0] || importName != "" {
		t.Errorf("ResolveComponent(1) = (%p, %q), want (%p, \"\")", def, importName, c.NestedComponents[0])
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
	if got, want := len(c.ComponentSpace), 3; got != want {
		t.Fatalf("component space = %d entries, want %d: %+v", got, want, c.ComponentSpace)
	}
	def, _, err := c.ResolveComponent(1) // the export alias resolves through to index 0
	if err != nil {
		t.Fatalf("ResolveComponent(1): %v", err)
	}
	if def != c.NestedComponents[0] {
		t.Errorf("ResolveComponent(1) = %p, want the first nested component %p", def, c.NestedComponents[0])
	}
	def, _, err = c.ResolveComponent(2)
	if err != nil {
		t.Fatalf("ResolveComponent(2): %v", err)
	}
	if def != c.NestedComponents[1] {
		t.Errorf("ResolveComponent(2) = %p, want the second nested component %p", def, c.NestedComponents[1])
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
	def, _, err := child.ResolveComponent(0)
	if err != nil {
		t.Fatalf("ResolveComponent(0): %v", err)
	}
	if def != parent.NestedComponents[0] {
		t.Errorf("ResolveComponent(0) = %p, want the parent's first nested component %p", def, parent.NestedComponents[0])
	}
}

// A hand-built Component has no space; NestedComponents IS its space.
func TestComponentSpace_HandBuiltFallback(t *testing.T) {
	inner := &Component{}
	c := &Component{NestedComponents: []*Component{inner}}
	def, importName, err := c.ResolveComponent(0)
	if err != nil {
		t.Fatalf("ResolveComponent(0): %v", err)
	}
	if def != inner || importName != "" {
		t.Errorf("ResolveComponent(0) = (%p, %q), want (%p, \"\")", def, importName, inner)
	}
	if _, _, err := c.ResolveComponent(1); err == nil || !strings.Contains(err.Error(), "out of range of 1 nested components") {
		t.Errorf("ResolveComponent(1) error = %v, want an out-of-range error", err)
	}
}

func TestComponentSpace_FailLoudBranches(t *testing.T) {
	// A cycle: an export entry naming its own space slot.
	cyclic := &Component{
		ComponentSpace: []ComponentSpaceEntry{{Kind: ComponentFromExport, Export: 0}},
		Exports:        []Export{{ExternType: 0x04, ExternIndex: 0}},
	}
	// An outer alias landing on an ENCLOSING component's own component import:
	// satisfying it would need that component's instantiate-args, not this one's.
	outerToImport := &Component{
		ComponentSpace: []ComponentSpaceEntry{{Kind: ComponentFromImport, Import: 0}},
		Imports:        []Import{{Name: "x", ExternType: 0x04}},
	}
	aliasToOuterImport := &Component{
		Outer:          outerToImport,
		ComponentSpace: []ComponentSpaceEntry{{Kind: ComponentFromAlias, Alias: 0}},
		Aliases:        []AliasDef{{Sort: 0x04, TargetKind: 0x02, OuterCount: 1, OuterIndex: 0}},
	}
	tests := []struct {
		name string
		comp *Component
		idx  uint32
		want string
	}{
		{
			name: "index past the space",
			comp: &Component{ComponentSpace: []ComponentSpaceEntry{{Kind: ComponentFromDefinition}}, NestedComponents: []*Component{{}}},
			idx:  1,
			want: "out of range of the 1-entry component index space",
		},
		{
			name: "definition entry naming a missing nested component",
			comp: &Component{ComponentSpace: []ComponentSpaceEntry{{Kind: ComponentFromDefinition, Component: 3}}},
			want: "nested component index 3 out of range of 0",
		},
		{
			name: "import entry naming a missing import",
			comp: &Component{ComponentSpace: []ComponentSpaceEntry{{Kind: ComponentFromImport, Import: 3}}},
			want: "import index 3 out of range of 0 imports",
		},
		{
			name: "export entry naming a missing export",
			comp: &Component{ComponentSpace: []ComponentSpaceEntry{{Kind: ComponentFromExport, Export: 3}}},
			want: "export index 3 out of range of 0 exports",
		},
		{
			name: "alias entry naming a missing alias",
			comp: &Component{ComponentSpace: []ComponentSpaceEntry{{Kind: ComponentFromAlias, Alias: 3}}},
			want: "alias index 3 out of range of 0 aliases",
		},
		{
			name: "an export alias cannot name a component",
			comp: &Component{
				ComponentSpace: []ComponentSpaceEntry{{Kind: ComponentFromAlias, Alias: 0}},
				Aliases:        []AliasDef{{Sort: 0x04, TargetKind: 0x00}},
			},
			want: "alias target kind 0x0 cannot name a component",
		},
		{
			name: "outer alias with no enclosing component",
			comp: &Component{
				ComponentSpace: []ComponentSpaceEntry{{Kind: ComponentFromAlias, Alias: 0}},
				Aliases:        []AliasDef{{Sort: 0x04, TargetKind: 0x02, OuterCount: 1}},
			},
			want: "but only 0 are known",
		},
		{
			name: "outer alias landing on an enclosing component's import",
			comp: aliasToOuterImport,
			want: `enclosing component's component import "x"`,
		},
		{
			name: "unknown entry kind",
			comp: &Component{ComponentSpace: []ComponentSpaceEntry{{Kind: ComponentSpaceEntryKind(9)}}},
			want: "unknown component-space entry kind 9",
		},
		{
			name: "cyclic export chain",
			comp: cyclic,
			want: "alias chain exceeds depth",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := tt.comp.ResolveComponent(tt.idx)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("ResolveComponent(%d) error = %v, want it to contain %q", tt.idx, err, tt.want)
			}
		})
	}
}
