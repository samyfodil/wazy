package main

import (
	"context"
	_ "embed"
	"fmt"
	"log"
	"sort"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/component"
)

// This example implements a WIT interface of your own design, host-side.
//
// The guest was compiled against testdata/store.wit, which is deliberately
// not a toy: it has a resource with a constructor and methods, an option
// return, a result whose error arm is a variant, and lists on both sides.
//
//	package example:store@1.0.0;
//
//	interface kv {
//	  resource bucket {
//	    constructor(name: string);
//	    put: func(key: string, value: list<u8>);
//	    get: func(key: string) -> option<list<u8>>;
//	  }
//	  variant error { no-such-bucket, denied(string) }
//	  list-keys: func(b: borrow<bucket>) -> result<list<string>, error>;
//	}
//
// Nothing here is WASI. wazy's own WASI 0.2 implementation is written
// against this same API (see imports/wasip2, which imports nothing internal),
// so anything it can express, an embedder can.
//
//go:embed testdata/store.wasm
var storeWasm []byte

// bucketResType is the tag this host mints bucket handles under. Any value
// works; what matters is that WithResourceTag maps it to the WIT resource, so
// the guest's own resource.drop can find it.
const bucketResType uint32 = 1

// store is the host state behind the `bucket` resource. Handles the guest
// holds are indices into this; reps are what the host sees.
type store struct {
	next    uint32
	buckets map[uint32]map[string][]byte
}

func newStore() *store { return &store{next: 1, buckets: map[uint32]map[string][]byte{}} }

func (s *store) create() uint32 {
	rep := s.next
	s.next++
	s.buckets[rep] = map[string][]byte{}
	return rep
}

func main() {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	s := newStore()

	// The interface name as the guest imports it. Matching ignores the
	// "@x.y.z" suffix, so one registration serves every patch version.
	const iface = "example:store/kv@1.0.0"

	// --- [constructor]bucket(name: string) -> bucket ----------------------
	//
	// A top-level own<bucket> result: return the rep and the engine mints the
	// guest's handle under bucketResType.
	ctorTbl := component.NewTypeTable()
	ctorFD := ctorTbl.Func([]component.TypeRef{component.Prim("string")}, ctorTbl.Own(bucketResType))
	ctor := func(_ context.Context, args []component.Value) ([]component.Value, error) {
		name := args[0].(string)
		rep := s.create()
		fmt.Printf("host: created bucket %q (rep %d)\n", name, rep)
		return []component.Value{rep}, nil
	}

	// --- [method]bucket.put(self: borrow<bucket>, key: string, value: list<u8>)
	//
	// A borrow argument arrives already resolved to the host rep. list<u8>
	// arrives as a []byte.
	putTbl := component.NewTypeTable()
	putFD := putTbl.Func([]component.TypeRef{
		putTbl.Borrow(bucketResType),
		component.Prim("string"),
		putTbl.List(component.Prim("u8")),
	}, component.TypeRef{}) // no result
	put := func(_ context.Context, args []component.Value) ([]component.Value, error) {
		rep, key := args[0].(uint32), args[1].(string)
		b, ok := s.buckets[rep]
		if !ok {
			return nil, fmt.Errorf("put: bucket rep %d does not exist", rep)
		}
		value, err := bytesOf(args[2])
		if err != nil {
			return nil, fmt.Errorf("put %q: %w", key, err)
		}
		b[key] = value
		return nil, nil
	}

	// --- [method]bucket.get(self: borrow<bucket>, key: string) -> option<list<u8>>
	//
	// option is nil for none, or the inner value for some.
	getTbl := component.NewTypeTable()
	getFD := getTbl.Func([]component.TypeRef{
		getTbl.Borrow(bucketResType),
		component.Prim("string"),
	}, getTbl.Option(getTbl.List(component.Prim("u8"))))
	get := func(_ context.Context, args []component.Value) ([]component.Value, error) {
		rep, key := args[0].(uint32), args[1].(string)
		v, ok := s.buckets[rep][key]
		if !ok {
			return []component.Value{nil}, nil // option::none
		}
		return []component.Value{v}, nil // option::some
	}

	// --- list-keys(b: borrow<bucket>) -> result<list<string>, error> -------
	//
	// The signature WithImport cannot express: both arms of the result are
	// composites, so each needs a slot in the type table.
	keysTbl := component.NewTypeTable()
	errRef := keysTbl.Variant(
		component.VariantCaseSpec{Name: "no-such-bucket"},
		component.VariantCaseSpec{Name: "denied", Type: component.Prim("string")},
	)
	keysFD := keysTbl.Func(
		[]component.TypeRef{keysTbl.Borrow(bucketResType)},
		keysTbl.Result(keysTbl.List(component.Prim("string")), errRef),
	)
	listKeys := func(_ context.Context, args []component.Value) ([]component.Value, error) {
		b, ok := s.buckets[args[0].(uint32)]
		if !ok {
			// The err arm, carrying the payload-less first variant case.
			return []component.Value{component.ResultValue{
				IsErr:   true,
				Payload: component.VariantValue{Disc: 0},
			}}, nil
		}
		keys := make([]string, 0, len(b))
		for k := range b {
			keys = append(keys, k)
		}
		sort.Strings(keys) // map order is not a contract; the guest prints these
		out := make([]component.Value, len(keys))
		for i, k := range keys {
			out[i] = k
		}
		return []component.Value{component.ResultValue{Payload: out}}, nil
	}

	inst, err := component.Instantiate(ctx, r, storeWasm,
		component.WithImportCustom(iface, "[constructor]bucket", ctor, ctorFD, ctorTbl.Resolver()),
		component.WithImportCustom(iface, "[method]bucket.put", put, putFD, putTbl.Resolver()),
		component.WithImportCustom(iface, "[method]bucket.get", get, getFD, getTbl.Resolver()),
		component.WithImportCustom(iface, "list-keys", listKeys, keysFD, keysTbl.Resolver()),

		// Required for any interface with a resource. The guest drops its
		// owned bucket through a canon carrying the component binary's own
		// type index; this maps that to bucketResType. Without it the first
		// drop trips the handle table's cross-type check.
		component.WithResourceTag(iface, "bucket", bucketResType),
	)
	if err != nil {
		log.Panicln(err)
	}
	defer inst.Close(ctx)

	results, err := inst.Call(ctx, "run")
	if err != nil {
		log.Panicln(err)
	}
	fmt.Printf("guest: %s\n", results[0].(string))
}

// bytesOf reads a lifted list<u8>. The ABI hands one over as a []byte when it
// can (a single copy, rather than one interface per byte), but a value built
// elsewhere may still arrive in the general []Value shape, so accept both.
func bytesOf(v component.Value) ([]byte, error) {
	switch t := v.(type) {
	case []byte:
		return t, nil
	case []component.Value:
		out := make([]byte, len(t))
		for i, e := range t {
			b, ok := e.(uint32)
			if !ok {
				return nil, fmt.Errorf("list<u8>[%d]: expected u8, got %T", i, e)
			}
			out[i] = byte(b)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("expected list<u8>, got %T", v)
	}
}
