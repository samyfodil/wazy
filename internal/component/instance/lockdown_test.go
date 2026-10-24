package instance

import (
	"context"
	_ "embed"
	"testing"

	"github.com/samyfodil/wazy"
)

//go:embed testdata/lockdown_store.wasm
var lockdownStoreWasm []byte

// A trap locks down the whole store, not just the instance it escaped from:
// "ok" lives in a different instance from "boom", and is refused once "boom"
// has trapped.
func TestLockdown_TrapLocksDownTheStore(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)
	in, err := Instantiate(ctx, r, lockdownStoreWasm)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close(ctx) //nolint:errcheck // test cleanup
	if got, err := in.Call(ctx, "ok"); err != nil || len(got) != 1 || got[0] != uint32(7) {
		t.Fatalf("ok = %v, %v; want [7]", got, err)
	}
	if _, err := in.Call(ctx, "boom"); err == nil {
		t.Fatal("boom did not trap")
	}
	_, err = in.Call(ctx, "ok")
	requireErrContains(t, err, "cannot enter component instance")
}
