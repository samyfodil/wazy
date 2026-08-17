package wit

import (
	"testing"

	"github.com/samyfodil/wazy/component"
)

// TestDatetimeDesc pins the record's field order and types.
//
// A record's field names never reach the wire -- records lower positionally --
// so reordering these two would silently change the layout of every interface
// that reuses datetime (wasi:filesystem's descriptor-stat, wasi:otel's spans,
// logs and metrics), and nothing downstream would necessarily point at why.
func TestDatetimeDesc(t *testing.T) {
	d := DatetimeDesc()

	if len(d.Fields) != 2 {
		t.Fatalf("expected 2 fields, got %d", len(d.Fields))
	}
	for i, want := range []struct{ name, prim string }{
		{"seconds", "u64"},
		{"nanoseconds", "u32"},
	} {
		if got := d.Fields[i].Name; got != want.name {
			t.Errorf("field %d name = %q, want %q", i, got, want.name)
		}
		if got := d.Fields[i].Type.Primitive; got != want.prim {
			t.Errorf("field %d type = %q, want %q", i, got, want.prim)
		}
	}
}

// TestDatetimeType covers the convenience: it interns the same record and
// returns a ref that resolves back to it.
func TestDatetimeType(t *testing.T) {
	tbl := component.NewTypeTable()
	ref := DatetimeType(tbl)

	if ref.TypeIndex == nil {
		t.Fatal("expected a table ref, not an inline primitive")
	}
	got, ok := tbl.Resolver()(*ref.TypeIndex).(component.RecordDesc)
	if !ok {
		t.Fatalf("interned entry = %T, want component.RecordDesc", tbl.Resolver()(*ref.TypeIndex))
	}

	want := DatetimeDesc()
	if len(got.Fields) != len(want.Fields) {
		t.Fatalf("interned record has %d fields, want %d", len(got.Fields), len(want.Fields))
	}
	for i := range want.Fields {
		if got.Fields[i] != want.Fields[i] {
			t.Errorf("field %d = %+v, want %+v", i, got.Fields[i], want.Fields[i])
		}
	}
}
