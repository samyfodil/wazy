package instance

import (
	"bytes"
	"context"
	"testing"

	_ "embed"

	"github.com/samyfodil/wazy"

	"github.com/samyfodil/wazy/internal/component/binary"
)

// The composition shape every component composer emits. See the .wat sources
// for the full annotated text; in one line each:
//
//	(instance $p (instantiate $Provider))
//	(alias export $p "X" (instance $g))
//	(instance $c (instantiate $Consumer (with "Y" (instance $g))))
//
//go:embed testdata/compose_alias.wasm
var composeAliasWasm []byte

//go:embed testdata/compose_alias_renamed.wasm
var composeAliasRenamedWasm []byte

// composedRun instantiates a composed fixture and calls one export, with no
// host imports at all -- the whole point of a composition is that the consumer's
// only import is satisfied by the provider.
func composedRun(t *testing.T, raw []byte, exportName string) uint32 {
	t.Helper()
	comp, err := binary.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	t.Cleanup(func() { r.Close(ctx) })
	in, err := instantiateGraph(ctx, r, comp, raw, newConfig(nil))
	if err != nil {
		t.Fatalf("instantiate: %v", err)
	}
	t.Cleanup(func() { in.Close(ctx) }) //nolint:errcheck // test cleanup
	out, err := in.Call(ctx, exportName)
	if err != nil {
		t.Fatalf("call %q: %v", exportName, err)
	}
	if len(out) != 1 {
		t.Fatalf("call %q returned %d values, want 1", exportName, len(out))
	}
	got, ok := out[0].(uint32)
	if !ok {
		t.Fatalf("call %q returned %T, want uint32", exportName, out[0])
	}
	return got
}

// One number decomposes into four independent assertions about the composition
// boundary:
//
//	  7000 -- the provider minted an own<counter> with rep 7 and the consumer
//	          received it, then handed it back as a borrow<counter> that
//	          arrived with the same rep.
//	    10 -- RESOURCE IDENTITY: the consumer's own `canon resource.drop`, on
//	          its OWN local resource type index, ran the PROVIDER's destructor.
//	          That only happens if the resource reachable through the ALIASED
//	          export was lined up with the consumer's import of the same name
//	          (the exportedInstanceResourceDefs half of the fix); with the plain
//	          top-level exportedResourceDefs the drop fails outright, because
//	          the two halves tag the shared handle table differently.
//	     3 -- an ordinary non-resource call still works through the projection.
//	100000 -- a SECOND interface, projected off the same provider instance
//	          through its own alias, is bound independently of the first.
//
// compose_alias_renamed.wat drops the second interface (it is testing a
// different axis) and so stops 100000 short.
const (
	composedRunExpected      = 100000 + 7*1000 + 1*10 + 3
	composedRunExpectedNoTwo = 7*1000 + 1*10 + 3
)

func TestGraph_ComposedInstanceAlias(t *testing.T) {
	if got := composedRun(t, composeAliasWasm, "run"); got != composedRunExpected {
		t.Fatalf("run = %d, want %d", got, composedRunExpected)
	}
}

// The same composition with the two names deliberately pulled apart: the
// provider exports the interface as "other:pkg/iface@9.9.99" and the consumer
// still imports "test:compose/adder@1.0.0". Nothing in the binary format ties
// them together -- subtyping is checked on the instance TYPE, never on the
// outer export name -- so the members must be looked up under the ALIAS's name
// and registered under the ARG's name. Getting that backwards (or "simplifying"
// it to one name) binds nothing at all and the consumer fails to find "make".
//
// It also carries the two other shapes no composer emits but the format allows:
// an inline-export provider interface (instance Kind 0x01) and an outer
// instance export that names an ALIAS index rather than an instance definition.
func TestGraph_ComposedInstanceAliasWithRenamedExport(t *testing.T) {
	// Guard the premise: if a future rebuild of the fixture accidentally makes
	// the two names equal, this test silently stops testing anything.
	comp, err := binary.Decode(bytes.NewReader(composeAliasRenamedWasm))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	aliasName, argName := composedAliasAndArgNames(t, comp)
	if aliasName == argName {
		t.Fatalf("fixture no longer exercises X != Y: the alias and the arg both name %q", aliasName)
	}

	if got := composedRun(t, composeAliasRenamedWasm, "test:compose/runner@1.0.0#run"); got != composedRunExpectedNoTwo {
		t.Fatalf("run = %d, want %d", got, composedRunExpectedNoTwo)
	}
}

// composedAliasAndArgNames returns the provider's exported interface name (the
// name the instance alias projects) and the consumer's import name (the name
// the instantiate-arg carries) for a composed fixture.
func composedAliasAndArgNames(t *testing.T, comp *binary.Component) (aliasName, argName string) {
	t.Helper()
	for _, inst := range comp.Instances {
		for _, arg := range inst.Args {
			if arg.Sort != 0x05 { // instance
				continue
			}
			tgt, err := resolveInstanceArgTarget(comp, arg.SortIdx)
			if err != nil || len(tgt.projection) != 1 {
				continue
			}
			return tgt.projection[0], arg.Name
		}
	}
	t.Fatal("fixture carries no projected instance-sort instantiate-arg")
	return "", ""
}

// Every shape the instance index space can express but this runtime cannot
// wire must fail loud and name what it found. Mis-wiring silently is the real
// hazard here: a handle tagged against the wrong resource type, or an import
// bound to the wrong interface, surfaces much later as a corrupt-looking
// runtime error with no connection to the composition that caused it.
//
// Each case mutates a DECODED real fixture rather than hand-building one, so
// the shape under test is the only thing that differs from a composition that
// does work.
func TestGraph_InstanceAliasFailsLoudOnShapesItCannotWire(t *testing.T) {
	// aliasSlot finds the component-instance index space slot of the
	// instance-sort alias a composition links its consumer through, plus the
	// index of the AliasDef behind it.
	aliasSlot := func(t *testing.T, comp *binary.Component) (slot uint32, aliasIdx uint32) {
		t.Helper()
		for i, e := range comp.ComponentInstanceSpace {
			if e.Kind == binary.ComponentInstanceFromAlias && comp.Aliases[e.Alias].Sort == 0x05 {
				return uint32(i), e.Alias
			}
		}
		t.Fatal("fixture carries no instance-sort alias")
		return 0, 0
	}
	// repointArg aims every instance-sort instantiate-arg that currently names
	// `from` at `to`.
	repointArg := func(comp *binary.Component, from, to uint32) {
		for i := range comp.Instances {
			for j := range comp.Instances[i].Args {
				if a := &comp.Instances[i].Args[j]; a.Sort == 0x05 && a.SortIdx == from {
					a.SortIdx = to
				}
			}
		}
	}

	tests := []struct {
		name    string
		fixture []byte
		mutate  func(t *testing.T, comp *binary.Component)
		wantErr string
	}{{
		// A chained alias: `(alias export $g "deeper" (instance $h))` on top of
		// the composition's own alias. A sub-Instance's exports are flat
		// "X#member" keys, which materializes exactly one level of nesting, so
		// there is no second level to look anything up in.
		name:    "an alias of the alias, two levels deep",
		fixture: composeAliasWasm,
		mutate: func(t *testing.T, comp *binary.Component) {
			slot, _ := aliasSlot(t, comp)
			comp.Aliases = append(comp.Aliases, binary.AliasDef{Sort: 0x05, TargetKind: 0x00, InstanceIdx: slot, Name: "deeper"})
			comp.ComponentInstanceSpace = append(comp.ComponentInstanceSpace, binary.ComponentInstanceSpaceEntry{
				Kind: binary.ComponentInstanceFromAlias, Alias: uint32(len(comp.Aliases) - 1),
			})
			repointArg(comp, slot, uint32(len(comp.ComponentInstanceSpace)-1))
		},
		wantErr: `projects a nested instance export ("test:compose/adder@1.0.0 -> deeper"); only one level of instance-export projection is supported`,
	}, {
		// An `alias outer` names an instance in an ENCLOSING component's index
		// space, which this engine does not model at all. It must never fall
		// through to the sibling lookup.
		name:    "an `alias outer` in the instance sort",
		fixture: composeAliasWasm,
		mutate: func(t *testing.T, comp *binary.Component) {
			_, aliasIdx := aliasSlot(t, comp)
			comp.Aliases[aliasIdx].TargetKind = 0x02
		},
		wantErr: "only an instance-export alias (0x00) names an instance this component can resolve",
	}, {
		// The alias names an export the provider does not have. Binding zero
		// members and letting the consumer fail later with "no such import"
		// would point at the wrong component entirely.
		name:    "an alias projecting an export the provider does not have",
		fixture: composeAliasWasm,
		mutate: func(t *testing.T, comp *binary.Component) {
			_, aliasIdx := aliasSlot(t, comp)
			comp.Aliases[aliasIdx].Name = "test:compose/nope@1.0.0"
		},
		wantErr: `projects the export "test:compose/nope@1.0.0" of nested instance 0, which exports no such instance`,
	}, {
		// Aliasing an instance-typed export OUT of an instance the OUTER
		// component imports. An imported instance has no *Instance in this
		// runtime -- only the flat, name-keyed cfg entries the embedder
		// registered -- so it has no instance-typed export to project.
		name:    "an alias projecting an export of an IMPORTED instance",
		fixture: nil, // resources.14: the fixture with a real instance import
		mutate: func(t *testing.T, comp *binary.Component) {
			var importSlot uint32
			found := false
			for i, e := range comp.ComponentInstanceSpace {
				if e.Kind == binary.ComponentInstanceFromImport {
					importSlot, found = uint32(i), true
					break
				}
			}
			if !found {
				t.Fatal("resources.14 was expected to carry an instance import")
			}
			comp.Aliases = append(comp.Aliases, binary.AliasDef{Sort: 0x05, TargetKind: 0x00, InstanceIdx: importSlot, Name: "inner"})
			comp.ComponentInstanceSpace = append(comp.ComponentInstanceSpace, binary.ComponentInstanceSpaceEntry{
				Kind: binary.ComponentInstanceFromAlias, Alias: uint32(len(comp.Aliases) - 1),
			})
			repointArg(comp, importSlot, uint32(len(comp.ComponentInstanceSpace)-1))
		},
		wantErr: `aliases the export "inner" of the IMPORTED instance "host"`,
	}, {
		// The outer component's own instance EXPORT naming an alias -- the
		// bindInstanceExportGraph counterpart -- with the projected name bent
		// the same way.
		name:    "an instance EXPORT projecting a name the sub-instance does not have",
		fixture: composeAliasRenamedWasm,
		mutate: func(t *testing.T, comp *binary.Component) {
			for i := range comp.Aliases {
				if comp.Aliases[i].Sort == 0x05 && comp.Aliases[i].Name == "test:compose/runner@1.0.0" {
					comp.Aliases[i].Name = "test:compose/gone@1.0.0"
					return
				}
			}
			t.Fatal("fixture carries no re-export alias to bend")
		},
		wantErr: `projects the export "test:compose/gone@1.0.0" of nested instance 2; that instance exports no such instance`,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := tt.fixture
			if raw == nil {
				var err error
				if raw, err = wastFS.ReadFile("testdata/wast/resources/resources.14.wasm"); err != nil {
					t.Fatalf("read fixture: %v", err)
				}
			}
			comp, err := binary.Decode(bytes.NewReader(raw))
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			tt.mutate(t, comp)
			ctx := context.Background()
			r := wazy.NewRuntime(ctx)
			t.Cleanup(func() { r.Close(ctx) })
			_, err = instantiateGraph(ctx, r, comp, raw, newConfig(nil))
			requireErrContains(t, err, tt.wantErr)
		})
	}
}
