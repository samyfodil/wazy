package instance

import (
	"context"
	"slices"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/internal/component/abi"
	"github.com/samyfodil/wazy/internal/component/binary"
)

// The exported host surface (public_host.go) is the internal one under
// another name; these pin that each name reaches what it says.

// A Harness applies the options and runs the resource hooks exactly as
// Instantiate would, and hands back what they registered.
func TestHarness(t *testing.T) {
	var hooked *HandleTable
	called := false
	fn := HostFunc(func(context.Context, []abi.Value) ([]abi.Value, error) {
		called = true
		return nil, nil
	})
	dtorRan := false
	h := NewHarness([]Option{
		WithImportCustom("acme:api/host@1.0.0", "lookup", fn, binary.FuncDesc{}, func(uint32) binary.TypeDesc { return nil }),
		WithResourceTag("acme:api/host@1.0.0", "thing", 7),
		WithResourcesHook(func(tbl *HandleTable) { hooked = tbl }),
		WithHostResourceDtor(7, func(context.Context, uint32) error { dtorRan = true; return nil }),
	})
	if hooked == nil || hooked != h.Resources() {
		t.Fatal("the resources hook did not get the harness's table")
	}
	got := h.Func("acme:api/host@1.0.5", "lookup") // matched without its version
	if got == nil {
		t.Fatal("Func did not find the registered import")
	}
	if _, err := got(context.Background(), nil); err != nil || !called {
		t.Fatalf("calling it: %v, called=%v", err, called)
	}
	if h.Func("acme:api/host", "missing") != nil {
		t.Error("Func found an import that was never registered")
	}
	if reg := h.Registered(); len(reg) != 1 || !slices.Equal(reg["acme:api/host"], []string{"lookup"}) {
		t.Errorf("Registered() = %v", reg)
	}
	if name := h.Resources().typeName(7); name == "" {
		t.Error("the tagged resource has no name in the table")
	}
	// The dtor option lands in the config the harness built.
	fn7 := h.cfg.hostResDtors[7]
	if fn7 == nil {
		t.Fatal("WithHostResourceDtor registered nothing")
	}
	if err := fn7(context.Background(), 1); err != nil || !dtorRan {
		t.Errorf("the registered dtor is not the one given: %v, ran=%v", err, dtorRan)
	}
}

type hostStateKey struct{}

// WithHostState values reach every Instance built with them, and an
// Instance without one answers nil. InstanceExports and Resources expose
// what a host driving an export needs.
func TestHostStateAndInstanceAccessors(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)
	root := componentImportFixture(t, "calc_root")
	calc := WithComponentImport("calc", componentImportFixture(t, "calc_exact"))
	in, err := Instantiate(ctx, r, root, calc, WithHostState(hostStateKey{}, "state"), WithHostState("other", 2))
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close(ctx)
	if got := in.HostState(hostStateKey{}); got != "state" {
		t.Errorf("HostState = %v", got)
	}
	if got := in.HostState("missing"); got != nil {
		t.Errorf("HostState(missing) = %v", got)
	}
	if in.Resources() == nil {
		t.Error("Resources() is nil")
	}
	if got := in.InstanceExports(); len(got) != 0 {
		t.Errorf("InstanceExports() = %v; calc_root exports a func, not an interface", got)
	}
	bare, err := Instantiate(ctx, r, root, calc)
	if err != nil {
		t.Fatal(err)
	}
	defer bare.Close(ctx)
	if got := bare.HostState(hostStateKey{}); got != nil {
		t.Errorf("HostState without WithHostState = %v", got)
	}
}
