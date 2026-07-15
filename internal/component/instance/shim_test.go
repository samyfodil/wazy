package instance

import (
	"context"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/api"
)

// srcModuleWat is a tiny real core module (hand-encoded, not embedded from a
// fixture, since it exists purely to give buildPassthroughShim something
// real to import from) that owns a memory and a func, so shim tests can prove
// true identity sharing (not a copy) across the shim boundary.
//
// (module
//
//	(memory (export "mem") 1)
//	(func (export "add") (param i32 i32) (result i32)
//	  local.get 0 local.get 1 i32.add)
//
// )
var srcModuleWasm = []byte{
	0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00,
	// type section: 1 type (i32,i32)->i32
	0x01, 0x07, 0x01, 0x60, 0x02, 0x7f, 0x7f, 0x01, 0x7f,
	// function section: 1 func, type 0
	0x03, 0x02, 0x01, 0x00,
	// memory section: 1 memory, min 1
	0x05, 0x03, 0x01, 0x00, 0x01,
	// export section: "mem" memory 0, "add" func 0
	0x07, 0x0d, 0x02,
	0x03, 'm', 'e', 'm', 0x02, 0x00,
	0x03, 'a', 'd', 'd', 0x00, 0x00,
	// code section: 1 func body: local.get 0, local.get 1, i32.add, end
	0x0a, 0x09, 0x01,
	0x07, 0x00, 0x20, 0x00, 0x20, 0x01, 0x6a, 0x0b,
}

func TestBuildPassthroughShim_FuncPassthroughCallsRealFunc(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	if _, err := r.InstantiateWithConfig(ctx, srcModuleWasm, wazy.NewModuleConfig().WithName("src").WithStartFunctions()); err != nil {
		t.Fatalf("instantiate src: %v", err)
	}

	shimBytes, err := buildPassthroughShim([]shimItem{
		{Sort: shimSortFunc, FromModule: "src", FromName: "add", ExportName: "renamed-add",
			Params: []api.ValueType{api.ValueTypeI32, api.ValueTypeI32}, Results: []api.ValueType{api.ValueTypeI32}},
	})
	if err != nil {
		t.Fatalf("buildPassthroughShim: %v", err)
	}

	shim, err := r.InstantiateWithConfig(ctx, shimBytes, wazy.NewModuleConfig().WithName("shim").WithStartFunctions())
	if err != nil {
		t.Fatalf("instantiate shim: %v", err)
	}

	fn := shim.ExportedFunction("renamed-add")
	if fn == nil {
		t.Fatal("shim has no exported function \"renamed-add\"")
	}
	results, err := fn.Call(ctx, 2, 40)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if len(results) != 1 || results[0] != 42 {
		t.Fatalf("renamed-add(2,40) = %v, want [42]", results)
	}
}

func TestBuildPassthroughShim_MemorySharesIdentity(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	src, err := r.InstantiateWithConfig(ctx, srcModuleWasm, wazy.NewModuleConfig().WithName("src2").WithStartFunctions())
	if err != nil {
		t.Fatalf("instantiate src: %v", err)
	}

	shimBytes, err := buildPassthroughShim([]shimItem{
		{Sort: shimSortMemory, FromModule: "src2", FromName: "mem", ExportName: "env-memory"},
	})
	if err != nil {
		t.Fatalf("buildPassthroughShim: %v", err)
	}

	shim, err := r.InstantiateWithConfig(ctx, shimBytes, wazy.NewModuleConfig().WithName("shim2").WithStartFunctions())
	if err != nil {
		t.Fatalf("instantiate shim: %v", err)
	}

	shimMem := shim.ExportedMemory("env-memory")
	if shimMem == nil {
		t.Fatal("shim has no exported memory \"env-memory\"")
	}

	// Write through the ORIGINAL module's memory and confirm it's visible
	// through the shim's re-export -- true identity sharing, not a copy.
	if ok := src.Memory().WriteUint32Le(100, 0xdeadbeef); !ok {
		t.Fatal("write to src memory failed")
	}
	got, ok := shimMem.ReadUint32Le(100)
	if !ok {
		t.Fatal("read from shim memory failed")
	}
	if got != 0xdeadbeef {
		t.Fatalf("shim memory at offset 100 = %#x, want 0xdeadbeef (shim must share src's memory identity)", got)
	}

	// And the reverse direction.
	if ok := shimMem.WriteUint32Le(200, 0xcafef00d); !ok {
		t.Fatal("write through shim memory failed")
	}
	got2, ok := src.Memory().ReadUint32Le(200)
	if !ok {
		t.Fatal("read from src memory failed")
	}
	if got2 != 0xcafef00d {
		t.Fatalf("src memory at offset 200 = %#x, want 0xcafef00d", got2)
	}
}

func TestBuildPassthroughShim_TableSharesIdentity(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	// A module owning a funcref table (exported), and an indirect-call
	// trampoline that calls through it -- so we can observe whether filling
	// the table via one module name affects a call made through another
	// module name importing the same table.
	//
	// (module
	//   (type $t (func (result i32)))
	//   (table (export "tbl") 1 1 funcref)
	//   (func (export "callit") (type $t) i32.const 0 call_indirect (type $t))
	// )
	tableOwnerWasm := []byte{
		0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00,
		0x01, 0x05, 0x01, 0x60, 0x00, 0x01, 0x7f, // type 0: ()->i32
		0x03, 0x02, 0x01, 0x00, // func section: 1 func type 0
		0x04, 0x04, 0x01, 0x70, 0x00, 0x01, // table section: 1 table funcref min1 max1
		0x07, 0x10, 0x02,
		0x03, 't', 'b', 'l', 0x01, 0x00,
		0x06, 'c', 'a', 'l', 'l', 'i', 't', 0x00, 0x00,
		0x0a, 0x09, 0x01,
		0x07, 0x00, 0x41, 0x00, 0x11, 0x00, 0x00, 0x0b, // i32.const 0; call_indirect (type 0); end
	}

	owner, err := r.InstantiateWithConfig(ctx, tableOwnerWasm, wazy.NewModuleConfig().WithName("owner").WithStartFunctions())
	if err != nil {
		t.Fatalf("instantiate owner: %v", err)
	}

	// A second module that fills element 0 of an imported table with its own
	// func -- mirrors module3's role in real_hello (filling a shared table
	// that another module already holds a reference to via a shim).
	//
	// (module
	//   (type $t (func (result i32)))
	//   (import "owner" "tbl" (table 1 funcref))
	//   (func (result i32) i32.const 99)
	//   (elem (i32.const 0) func 0)
	// )
	fillerWasm := []byte{
		0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00,
		0x01, 0x05, 0x01, 0x60, 0x00, 0x01, 0x7f,
		0x02, 0x0f, 0x01, 0x05, 'o', 'w', 'n', 'e', 'r', 0x03, 't', 'b', 'l', 0x01, 0x70, 0x00, 0x01,
		0x03, 0x02, 0x01, 0x00,
		0x09, 0x07, 0x01, 0x00, 0x41, 0x00, 0x0b, 0x01, 0x00, // elem seg 0, kind active table 0, offset i32.const 0, funcidx 0
		0x0a, 0x07, 0x01, 0x05, 0x00, 0x41, 0xe3, 0x00, 0x0b, // func body: i32.const 99 (sleb128 0xe3 0x00); end
	}

	if _, err := r.InstantiateWithConfig(ctx, fillerWasm, wazy.NewModuleConfig().WithName("filler").WithStartFunctions()); err != nil {
		t.Fatalf("instantiate filler: %v", err)
	}

	// Before wiring a shim, owner's own call_indirect must already see the
	// filler's write (both modules import/own the SAME table object).
	fn := owner.ExportedFunction("callit")
	results, err := fn.Call(ctx)
	if err != nil {
		t.Fatalf("callit: %v", err)
	}
	if len(results) != 1 || results[0] != 99 {
		t.Fatalf("callit() = %v, want [99] (table must be shared between owner and filler)", results)
	}

	// Now prove the SHIM re-export also shares the same table: a THIRD
	// module imports the table only through the shim's re-exported name, and
	// filling an element via that path must be visible from owner's
	// call_indirect too.
	shimBytes, err := buildPassthroughShim([]shimItem{
		{Sort: shimSortTable, FromModule: "owner", FromName: "tbl", ExportName: "tbl-via-shim"},
	})
	if err != nil {
		t.Fatalf("buildPassthroughShim: %v", err)
	}
	if _, err := r.InstantiateWithConfig(ctx, shimBytes, wazy.NewModuleConfig().WithName("shim3").WithStartFunctions()); err != nil {
		t.Fatalf("instantiate shim: %v", err)
	}

	// A second filler that imports the table via the SHIM's name instead of
	// "owner" directly, and overwrites element 0 with a different value.
	filler2Wasm := []byte{
		0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00,
		0x01, 0x05, 0x01, 0x60, 0x00, 0x01, 0x7f,
		0x02, 0x18, 0x01, 0x05, 's', 'h', 'i', 'm', '3', 0x0c, 't', 'b', 'l', '-', 'v', 'i', 'a', '-', 's', 'h', 'i', 'm', 0x01, 0x70, 0x00, 0x01,
		0x03, 0x02, 0x01, 0x00,
		0x09, 0x07, 0x01, 0x00, 0x41, 0x00, 0x0b, 0x01, 0x00,
		0x0a, 0x07, 0x01, 0x05, 0x00, 0x41, 0xfb, 0x00, 0x0b, // i32.const 123 (sleb128 0xfb 0x00)
	}
	if _, err := r.InstantiateWithConfig(ctx, filler2Wasm, wazy.NewModuleConfig().WithName("filler2").WithStartFunctions()); err != nil {
		t.Fatalf("instantiate filler2: %v", err)
	}

	results2, err := fn.Call(ctx)
	if err != nil {
		t.Fatalf("callit (after shim-wired refill): %v", err)
	}
	if len(results2) != 1 || results2[0] != 123 {
		t.Fatalf("callit() after refilling via the shim = %v, want [123] (shim table export must share owner's table identity)", results2)
	}
}

func TestBuildPassthroughShim_NoItems(t *testing.T) {
	if _, err := buildPassthroughShim(nil); err == nil {
		t.Fatal("expected an error for zero items")
	}
}

func TestBuildPassthroughShim_EmptySourceName(t *testing.T) {
	if _, err := buildPassthroughShim([]shimItem{{Sort: shimSortFunc, FromModule: "", FromName: "x", ExportName: "y"}}); err == nil {
		t.Fatal("expected an error for empty FromModule")
	}
	if _, err := buildPassthroughShim([]shimItem{{Sort: shimSortFunc, FromModule: "x", FromName: "", ExportName: "y"}}); err == nil {
		t.Fatal("expected an error for empty FromName")
	}
}

func TestBuildPassthroughShim_EmptyExportName(t *testing.T) {
	if _, err := buildPassthroughShim([]shimItem{{Sort: shimSortFunc, FromModule: "x", FromName: "y", ExportName: ""}}); err == nil {
		t.Fatal("expected an error for empty ExportName")
	}
}

func TestBuildPassthroughShim_UnsupportedSort(t *testing.T) {
	if _, err := buildPassthroughShim([]shimItem{{Sort: 0x03, FromModule: "x", FromName: "y", ExportName: "z"}}); err == nil {
		t.Fatal("expected an error for an unsupported core:sort")
	}
}
