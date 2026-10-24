package instance

import (
	"context"
	_ "embed"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/internal/component/abi"
	"github.com/samyfodil/wazy/internal/component/binary"
)

//go:embed testdata/instantiation_order.wasm
var instantiationOrderWasm []byte

// Core and component instances are instantiated in the order the binary
// defines them: a start function logs 1 in core instance $a, 2 in the nested
// component instance $c defined after it, and 3 in core instance $b defined
// after that. failOn makes the host fail that digit's call, which must stop
// instantiation right there.
func TestInstantiationFollowsDefinitionOrder(t *testing.T) {
	for _, tc := range []struct {
		name   string
		failOn uint32
		want   []uint32
	}{
		{"all", 0, []uint32{1, 2, 3}},
		{"core before the nested instance fails", 1, []uint32{1}},
		{"nested instance fails", 2, []uint32{1, 2}},
		{"core after the nested instance fails", 3, []uint32{1, 2, 3}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			r := wazy.NewRuntime(ctx)
			defer r.Close(ctx)
			var got []uint32
			logged := errors.New("refused")
			in, err := Instantiate(ctx, r, instantiationOrderWasm, WithImport("test:order/log", "log",
				func(_ context.Context, args []abi.Value) ([]abi.Value, error) {
					d := args[0].(uint32)
					got = append(got, d)
					if d == tc.failOn {
						return nil, logged
					}
					return nil, nil
				}, []binary.TypeDesc{binary.PrimitiveDesc{Prim: "u32"}}, nil))
			if tc.failOn == 0 {
				if err != nil {
					t.Fatalf("Instantiate: %v", err)
				}
				in.Close(ctx) //nolint:errcheck // test cleanup
			} else if err == nil || !strings.Contains(err.Error(), "refused") {
				t.Fatalf("Instantiate = %v, want the host's refusal", err)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("start functions ran %v, want %v", got, tc.want)
			}
		})
	}
}

// A canon lower of a nested instance's export binds to the instance itself,
// which was instantiated first; one that does not resolve is refused.
func TestLowerOfNestedInstanceExportRefusals(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)
	for _, tc := range []struct {
		name     string
		kind     byte
		siblings map[int]*Instance
		want     string
	}{
		{"inline instance", 0x01, nil, "is not a real nested component instantiation"},
		{"never instantiated", 0x00, nil, "was never instantiated"},
		{"no such export", 0x00, map[int]*Instance{0: {}}, `has no export "g"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			comp := &binary.Component{
				Instances:              []binary.Instance{{Kind: tc.kind}},
				ComponentInstanceSpace: []binary.ComponentInstanceSpaceEntry{{Kind: binary.ComponentInstanceFromDefinition}},
				Aliases:                []binary.AliasDef{{Sort: 0x01, Name: "g"}},
				ComponentFuncSpace:     []binary.ComponentFuncSpaceEntry{{Kind: binary.ComponentFuncFromAlias}},
			}
			cfg := newConfig(nil)
			cfg.siblings = tc.siblings
			_, _, err := computeCanonHostFunc(ctx, r, comp, cfg, newHandleTable(), nil, binary.Canon{Kind: 0x01}, nil, nil, "e", nil, nil, nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}
