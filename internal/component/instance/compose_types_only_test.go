package instance

import (
	"context"
	_ "embed"
	"testing"

	"github.com/samyfodil/wazy"
)

//go:embed testdata/compose_types_only.wasm
var composeTypesOnlyWasm []byte

// TestComposeTypesOnlyInterface composes over an interface that exports a
// resource type and NO functions -- what wit-component emits for any `interface`
// declaring just a record or a resource.
//
// Only func exports become "iface#member" keys in buildInstanceExportIndex, so
// such an interface is absent from instanceExports even though the provider
// genuinely exports it. Reading that absence as "no such export" made the whole
// composition fail to instantiate:
//
//	component instance 2 arg "test:x/types@1.0.0": instance 1 projects the export
//	"test:x/types@1.0.0" of nested instance 0, which exports no such instance
//
// Every interface in the cross-language example matrix has functions, so none of
// those pairs cover this.
func TestComposeTypesOnlyInterface(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	inst, err := Instantiate(ctx, r, composeTypesOnlyWasm)
	if err != nil {
		t.Fatalf("instantiate: %v", err)
	}
	defer inst.Close(ctx)

	got, err := inst.Call(ctx, "run")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(got) != 1 || got[0] != uint32(42) {
		t.Fatalf("run() = %#v, want [42]", got)
	}
}
