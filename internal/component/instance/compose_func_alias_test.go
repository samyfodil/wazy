package instance

import (
	"bytes"
	"context"
	_ "embed"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/internal/component/binary"
)

// A FUNC alias whose instance operand is itself an INSTANCE ALIAS -- see the
// .wat sources for the annotated shape; in three lines:
//
//	(instance $p (instantiate $Provider))                        ;; a DEFINITION
//	(alias export $p "test:compose/greeter@1.0.0" (instance $g))  ;; an ALIAS
//	(alias export $g "greet" (func $greet))                       ;; off the ALIAS
//
//go:embed testdata/compose_func_alias.wasm
var composeFuncAliasWasm []byte

//go:embed testdata/compose_func_alias_arg.wasm
var composeFuncAliasArgWasm []byte

// The component-instance index space has four producers and only one of them is
// an instance DEFINITION, which is the only kind a sub-Instance is filed under.
// A func alias may name any of the four, so resolving its operand by definition
// slot alone misses the two an alias chain produces -- and the miss did not read
// as a miss: the export side concluded the func must be an IMPORT and refused it
// with "resolves to an imported func rather than a lift; only lifted funcs may
// be exported", which is false twice over about a `canon lift` sitting one level
// of aliasing away.
func TestComposeFuncAliasThroughInstanceAlias(t *testing.T) {
	if got := composedRun(t, composeFuncAliasWasm, "greet", uint32(41)); got != 42 {
		t.Fatalf("greet(41) = %d, want 42", got)
	}
}

// The same alias put to the other use a func index has -- a `(with "greet"
// (func $greet))` instantiate-arg -- alongside the export. Two call sites, one
// root cause, so they resolve the operand through one shared helper; this is
// what keeps them shared.
//
// The order is not incidental. An instantiate-arg is bound during
// instantiateNestedInstances, strictly before any export is bound, so on a
// component that uses the alias both ways the ARG site fails first, with its
// own quite different symptom ("component instance index 1 out of range of 0
// imported instances" -- having found the operand was not a definition, it
// concluded it must name an imported instance). Fixing only the export site
// leaves this red.
func TestComposeFuncAliasThroughInstanceAliasAsInstantiateArg(t *testing.T) {
	if got := composedRun(t, composeFuncAliasArgWasm, "run"); got != 41 {
		t.Fatalf("run = %d, want 41 (greet(40) through the func-arg import)", got)
	}
	if got := composedRun(t, composeFuncAliasArgWasm, "greet", uint32(41)); got != 42 {
		t.Fatalf("greet(41) = %d, want 42 (the same alias, exported directly)", got)
	}
}

// The one shape the shared resolution refuses rather than half-binds: a func
// alias reached through TWO instance aliases.
//
//	(alias export $p "X" (instance $g))
//	(alias export $g "deeper" (instance $h))
//	(alias export $h "greet" (func $greet))
//
// A sub-Instance's exports are flat "iface#member" keys, which materializes
// exactly one level of instance nesting, so there is no second level to reach
// into -- the same limit the instantiate-arg and instance-export sides already
// enforce, and stated in the same words so a user meeting it through any of the
// three gets one diagnosis. What it must NOT do is fall back to the
// "resolves to an imported func rather than a lift" message the alias-miss used
// to produce: the func is neither imported nor unreachable for that reason, and
// sending the reader after a non-existent import wastes the whole investigation.
//
// Mutating a decoded fixture, rather than hand-writing a .wat, keeps the shape
// under test the ONLY difference from a composition that does work -- and no
// WIT-based tool can emit a nested interface to build the .wat from anyway.
func TestComposeFuncAliasFailsLoudOnATwoLevelProjection(t *testing.T) {
	tests := []struct {
		name    string
		fixture []byte
		wantErr string
	}{{
		// bindFuncExportGraph: the alias is what the component exports.
		name:    "as a func export",
		fixture: composeFuncAliasWasm,
		wantErr: `export "greet" aliases instance 2, which projects a nested instance export ("test:compose/greeter@1.0.0 -> deeper"); only one level of instance-export projection is supported`,
	}, {
		// outerFuncArgImport: the same alias handed to a sibling as a
		// `(with "greet" (func $greet))` arg, which is bound first.
		name:    "as a func instantiate-arg",
		fixture: composeFuncAliasArgWasm,
		wantErr: `func arg index 0 aliases instance 3, which projects a nested instance export ("test:compose/greeter@1.0.0 -> deeper"); only one level of instance-export projection is supported`,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			comp, err := binary.Decode(bytes.NewReader(tt.fixture))
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			// Chain a second instance alias onto the fixture's existing one,
			// then aim the FUNC alias at it. Found by sort, not by name.
			funcAlias := -1
			for i := range comp.Aliases {
				if comp.Aliases[i].Sort == 0x01 && comp.Aliases[i].TargetKind == 0x00 {
					funcAlias = i
					break
				}
			}
			if funcAlias < 0 {
				t.Fatal("fixture carries no func-export alias")
			}
			comp.Aliases = append(comp.Aliases, binary.AliasDef{Sort: 0x05, TargetKind: 0x00, InstanceIdx: comp.Aliases[funcAlias].InstanceIdx, Name: "deeper"})
			comp.ComponentInstanceSpace = append(comp.ComponentInstanceSpace, binary.ComponentInstanceSpaceEntry{
				Kind: binary.ComponentInstanceFromAlias, Alias: uint32(len(comp.Aliases) - 1),
			})
			comp.Aliases[funcAlias].InstanceIdx = uint32(len(comp.ComponentInstanceSpace) - 1)

			ctx := context.Background()
			r := wazy.NewRuntime(ctx)
			t.Cleanup(func() { r.Close(ctx) })
			_, err = instantiateGraph(ctx, r, comp, tt.fixture, newConfig(nil))
			requireErrContains(t, err, tt.wantErr)
		})
	}
}
