package component_test

import (
	"context"
	_ "embed"
	"fmt"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/component"
)

// This file is the acceptance test for "bring your own WIT": it implements a
// custom interface host-side using ONLY the public component API -- this is
// package component_test, so nothing internal is reachable even by accident.
// If it compiles and passes, an embedder outside this module can do the same.
//
// The interface (testdata/custom_wit.wit) was chosen to be hostile to a
// partial implementation. It needs, at once:
//
//   - a record parameter (a composite whose children are primitives)
//   - result<list<string>, variant> as a return -- composites nested two
//     deep, which WithImport's flat signature cannot express at all
//   - a variant with one payload-carrying case and one without
//   - a resource: constructed by the host, returned as own<counter>, then
//     used by the guest both as a method receiver and as a borrow argument
//
// The guest (testdata/custom_wit.component.wasm) is a real rustc
// wasm32-wasip2 component built with wit-bindgen against that same .wit, so
// the host side is talking to genuine generated bindings, not a hand-rolled
// caller.

//go:embed testdata/custom_wit.component.wasm
var customWITWasm []byte

// The resource tag this host mints counter handles under. Any value works as
// long as WithResourceTag maps it to the WIT resource, which is what lets the
// guest's own resource.drop find it.
const counterResType uint32 = 1

// counterHost is the host-side state behind the `counter` resource: rep ->
// current value. A real implementation would guard this with a mutex; the
// guest here is single-threaded.
type counterHost struct {
	next   uint32
	values map[uint32]uint32
}

func newCounterHost() *counterHost {
	return &counterHost{next: 1, values: map[uint32]uint32{}}
}

func (c *counterHost) create(start uint32) uint32 {
	rep := c.next
	c.next++
	c.values[rep] = start
	return rep
}

func (c *counterHost) bump(rep, by uint32) (uint32, error) {
	v, ok := c.values[rep]
	if !ok {
		return 0, fmt.Errorf("counter rep %d does not exist", rep)
	}
	v += by
	c.values[rep] = v
	return v, nil
}

func TestCustomWIT_HostImplementsResourcesAndNestedTypes(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	counters := newCounterHost()
	const iface = "acme:api/host@1.0.0"

	// --- lookup(q: query) -> result<list<string>, failure> ------------------
	//
	// The signature WithImport cannot express: the result is a result<> whose
	// ok arm is a list<string> and whose err arm is a variant. Both need a
	// table slot, and a table slot can only be referenced from another type
	// via a TypeRef -- which is exactly what TypeTable hands out.
	lookupTbl := component.NewTypeTable()
	queryRef := lookupTbl.Record(
		"name", component.Prim("string"),
		"limit", component.Prim("u32"),
	)
	failureRef := lookupTbl.Variant(
		component.VariantCaseSpec{Name: "not-found"},
		component.VariantCaseSpec{Name: "denied", Type: component.Prim("string")},
	)
	lookupFD := lookupTbl.Func(
		[]component.TypeRef{queryRef},
		lookupTbl.Result(lookupTbl.List(component.Prim("string")), failureRef),
	)

	lookup := func(_ context.Context, args []component.Value) ([]component.Value, error) {
		rec, ok := args[0].([]component.Value)
		if !ok || len(rec) != 2 {
			return nil, fmt.Errorf("lookup: expected a 2-field record, got %#v", args[0])
		}
		name := rec[0].(string)
		limit := rec[1].(uint32)
		if name == "" {
			// The err arm: a variant case that carries a payload.
			return []component.Value{component.ResultValue{
				IsErr:   true,
				Payload: component.VariantValue{Disc: 1, Payload: "empty name"},
			}}, nil
		}
		out := make([]component.Value, 0, limit)
		for i := uint32(0); i < limit; i++ {
			out = append(out, fmt.Sprintf("%s-%d", name, i))
		}
		return []component.Value{component.ResultValue{Payload: out}}, nil
	}

	// --- make(start: u32) -> counter ---------------------------------------
	//
	// A top-level own<counter> result: the host returns the rep and the engine
	// mints the guest handle under counterResType.
	makeTbl := component.NewTypeTable()
	makeFD := makeTbl.Func([]component.TypeRef{component.Prim("u32")}, makeTbl.Own(counterResType))
	makeFn := func(_ context.Context, args []component.Value) ([]component.Value, error) {
		return []component.Value{counters.create(args[0].(uint32))}, nil
	}

	// --- [method]counter.bump(self: borrow<counter>, by: u32) -> u32 --------
	//
	// A borrow arrives already resolved to the host rep.
	bumpTbl := component.NewTypeTable()
	bumpFD := bumpTbl.Func(
		[]component.TypeRef{bumpTbl.Borrow(counterResType), component.Prim("u32")},
		component.Prim("u32"),
	)
	bumpFn := func(_ context.Context, args []component.Value) ([]component.Value, error) {
		v, err := counters.bump(args[0].(uint32), args[1].(uint32))
		if err != nil {
			return nil, err
		}
		return []component.Value{v}, nil
	}

	// --- use-it(c: borrow<counter>, by: u32) -> u32 -------------------------
	useTbl := component.NewTypeTable()
	useFD := useTbl.Func(
		[]component.TypeRef{useTbl.Borrow(counterResType), component.Prim("u32")},
		component.Prim("u32"),
	)

	opts := []component.Option{
		component.WithImportCustom(iface, "lookup", lookup, lookupFD, lookupTbl.Resolver()),
		component.WithImportCustom(iface, "make", makeFn, makeFD, makeTbl.Resolver()),
		component.WithImportCustom(iface, "[method]counter.bump", bumpFn, bumpFD, bumpTbl.Resolver()),
		component.WithImportCustom(iface, "use-it", bumpFn, useFD, useTbl.Resolver()),
		// Without this the guest's own resource.drop of the owned counter
		// tags the handle with the component binary's type index, not
		// counterResType, and the drop trips the handle table's
		// cross-type-confusion check.
		component.WithResourceTag(iface, "counter", counterResType),
	}

	inst, err := component.Instantiate(ctx, r, customWITWasm, opts...)
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	defer inst.Close(ctx)

	results, err := inst.Call(ctx, "run")
	if err != nil {
		t.Fatalf("Call run(): %v", err)
	}
	got, ok := results[0].(string)
	if !ok {
		t.Fatalf("run() returned %T, want string", results[0])
	}

	// wazy-0,wazy-1 : the list<string> inside the result's ok arm, joined by
	//                 the guest, proving the nested composite round-tripped
	// 15            : counter created at 10, bumped by 5 through the method
	// 22            : the same counter, borrowed by use-it and bumped by 7
	const want = "wazy-0,wazy-1|15|22"
	if got != want {
		t.Fatalf("run() = %q, want %q", got, want)
	}
}
