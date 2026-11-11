package component_test

import (
	"context"
	_ "embed"
	"strings"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/component"
)

//go:embed testdata/keep_borrow.wasm
var keepBorrowWasm []byte

// A host resource lent to an export must be dropped by it before it returns;
// one that keeps the borrow fails the call rather than leaving a loan behind.
func TestCallExport_KeptBorrowFails(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	const tag uint32 = 0x700
	var table *component.HandleTable
	inst, err := component.Instantiate(ctx, r, keepBorrowWasm,
		component.WithResourceTag("test:lend/host", "r", tag),
		component.WithResourcesHook(func(t *component.HandleTable) { table = t }))
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	defer inst.Close(ctx)

	h := table.NewOwn(tag, 5)
	_, err = inst.Call(ctx, "keep", h)
	if err == nil || !strings.Contains(err.Error(), "borrow handles still remain at the end of the call (1 still held)") {
		t.Fatalf("Call = %v, want the kept borrow refused", err)
	}
	if rep, err := table.Rep(tag, h); err != nil || rep != 5 {
		t.Fatalf("the host's handle after the call: %d, %v", rep, err)
	}
}
