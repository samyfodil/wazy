package instance

import (
	"context"
	_ "embed"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/internal/component/abi"
)

// real_complex.component.wasm is a genuine cargo-component (wit-bindgen) guest
// built from a rich WIT world (wit/world.wit): nested records, variants with
// composite payloads, enums, flags, lists-of-records, option<record>, and
// result<u32,string> appear as function PARAMS and RESULTS. It is the
// integration counterpart to the abi unit oracle -- it drives the full
// bind + lowerParams(LowerStep) + CallExport + liftResult path end to end with
// complex types, and (unlike the hand-built fixtures) its func exports
// interleave with its lifts in the component func index space, exercising
// binary.ComponentFuncSpace.
//
//go:embed testdata/real_complex.component.wasm
var realComplexWasm []byte

// point builds the record { x: s32, y: s32 } value.
func point(x, y int32) abi.Value { return []abi.Value{x, y} }

func TestRealComplex(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	inst, err := Instantiate(ctx, r, realComplexWasm)
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	defer inst.Close(ctx)

	call := func(name string, args ...abi.Value) abi.Value {
		t.Helper()
		res, err := inst.Call(ctx, name, args...)
		if err != nil {
			t.Fatalf("Call %s: %v", name, err)
		}
		if len(res) != 1 {
			t.Fatalf("Call %s returned %d results, want 1", name, len(res))
		}
		return res[0]
	}

	// --- area-of(shape) -> u32 : variant param with composite payloads ---
	// circle(5) -> 3*5*5 = 75
	if got := call("area-of", abi.VariantValue{Disc: 0, Payload: uint32(5)}); got != uint32(75) {
		t.Errorf("area-of(circle 5) = %v, want 75", got)
	}
	// rect(point{4,5}) -> |4|*|5| = 20
	if got := call("area-of", abi.VariantValue{Disc: 1, Payload: point(4, 5)}); got != uint32(20) {
		t.Errorf("area-of(rect 4x5) = %v, want 20", got)
	}
	// poly([p,p,p]) -> len = 3
	if got := call("area-of", abi.VariantValue{Disc: 2, Payload: []abi.Value{point(0, 0), point(1, 1), point(2, 2)}}); got != uint32(3) {
		t.Errorf("area-of(poly 3pts) = %v, want 3", got)
	}
	// empty -> 0
	if got := call("area-of", abi.VariantValue{Disc: 3, Payload: nil}); got != uint32(0) {
		t.Errorf("area-of(empty) = %v, want 0", got)
	}

	// --- divide(a,b) -> result<u32,string> : result-with-string-err lift ---
	if got := call("divide", uint32(20), uint32(4)); got != (abi.ResultValue{IsErr: false, Payload: uint32(5)}) {
		t.Errorf("divide(20,4) = %#v, want Ok(5)", got)
	}
	if rv, _ := call("divide", uint32(1), uint32(0)).(abi.ResultValue); !rv.IsErr || rv.Payload != "division by zero" {
		t.Errorf("divide(1,0) = %#v, want Err(\"division by zero\")", rv)
	}

	// --- toggle(perms) -> perms : flags param + result. all=read|write|exec=7 ---
	if got := call("toggle", uint32(0b001)); got != uint32(0b110) { // ~read = write|exec
		t.Errorf("toggle(read) = %v, want 6", got)
	}

	// --- pick(items, idx) -> option<item> : list<record> param, option<record> lift ---
	items := []abi.Value{
		itemVal("a", []string{"t1"}, point(1, 2), 0),
		itemVal("b", nil, point(3, 4), 1),
	}
	// pick index 1 -> some(item "b")
	got := call("pick", items, uint32(1))
	picked, ok := got.([]abi.Value) // option some -> the inner record value
	if !ok || len(picked) != 4 || picked[0] != "b" {
		t.Fatalf("pick(items,1) = %#v, want some(item b)", got)
	}
	// pick out-of-range -> none (nil)
	if got := call("pick", items, uint32(9)); got != nil {
		t.Errorf("pick(items,9) = %#v, want none (nil)", got)
	}

	// --- classify(list<item>) -> summary : list<record> in, nested record out ---
	sum, ok := call("classify", items).([]abi.Value)
	if !ok || len(sum) != 5 {
		t.Fatalf("classify -> %#v, want a 5-field summary record", call("classify", items))
	}
	// summary { count: u32, total-area: u64, labels: list<string>, first: option<point>, status: result<string,u32> }
	if sum[0] != uint32(2) {
		t.Errorf("summary.count = %v, want 2", sum[0])
	}
	// total-area = |1*2| + |3*4| = 2 + 12 = 14
	if sum[1] != uint64(14) {
		t.Errorf("summary.total-area = %v, want 14", sum[1])
	}
	labels, _ := sum[2].([]abi.Value)
	if len(labels) != 2 || labels[0] != "a" || labels[1] != "b" {
		t.Errorf("summary.labels = %#v, want [a b]", sum[2])
	}
	if rv, _ := sum[4].(abi.ResultValue); rv.IsErr || rv.Payload != "2 items" {
		t.Errorf("summary.status = %#v, want Ok(\"2 items\")", sum[4])
	}
}

// itemVal builds the record { name: string, tags: list<string>, pos: point,
// tint: color(enum) } value.
func itemVal(name string, tags []string, pos abi.Value, tint uint32) abi.Value {
	tagVals := make([]abi.Value, len(tags))
	for i, tg := range tags {
		tagVals[i] = tg
	}
	return []abi.Value{name, tagVals, pos, tint}
}
