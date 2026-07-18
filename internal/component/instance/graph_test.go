package instance

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/component/abi"
	"github.com/samyfodil/wazy/internal/component/binary"
)

// decodeRealHello returns a fresh decode of the real_hello fixture, so a
// test can mutate the decoded structure to trip a specific validation branch
// while keeping the real embedded core-module byte ranges valid -- same
// pattern as decodeLogHello in host_import_test.go.
func decodeRealHello(t *testing.T) *binary.Component {
	t.Helper()
	comp, err := binary.Decode(bytes.NewReader(realHelloWasm))
	if err != nil {
		t.Fatalf("decode real_hello: %v", err)
	}
	return comp
}

func runGraph(t *testing.T, comp *binary.Component, opts ...Option) (*Instance, error) {
	t.Helper()
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	t.Cleanup(func() { r.Close(ctx) })
	return instantiateGraph(ctx, r, comp, realHelloWasm, newConfig(opts))
}

func TestGraph_NonInstanceImport(t *testing.T) {
	comp := decodeRealHello(t)
	comp.Imports[0].ExternType = 0x01 // func
	_, err := runGraph(t, comp)
	requireErrContains(t, err, "only instance imports")
}

func TestGraph_RequiresCoreFuncSpace(t *testing.T) {
	comp := &binary.Component{
		Aliases: []binary.AliasDef{{Sort: 0x00, CoreSort: 0x00, TargetKind: 0x01, InstanceIdx: 0, Name: "x"}},
	}
	_, err := runGraph(t, comp)
	requireErrContains(t, err, "requires a component decoded via binary.Decode")
}

func TestGraph_ModuleNameFor_MultipleNames(t *testing.T) {
	comp := decodeRealHello(t)
	// core instance 14 (module1) already references instance 1 (via a
	// different core instance's arg? no -- reference core instance 1 which
	// core instance 2 already names "wasi_snapshot_preview1") under a
	// second name too.
	comp.CoreInstances[14].Args = append(comp.CoreInstances[14].Args,
		binary.CoreInstantiateArg{Name: "second-name", InstanceIdx: 1})
	_, err := runGraph(t, comp)
	requireErrContains(t, err, "referenced under 2 names")
}

func TestGraph_CoreModuleIdxOutOfRange(t *testing.T) {
	comp := decodeRealHello(t)
	comp.CoreInstances[0].ModuleIdx = 99
	_, err := runGraph(t, comp)
	requireErrContains(t, err, "out of range of 4 modules")
}

func TestGraph_CoreModuleBytesOOB(t *testing.T) {
	comp := decodeRealHello(t)
	comp.CoreModules[2].Size = len(realHelloWasm) + 1000
	_, err := runGraph(t, comp)
	requireErrContains(t, err, "out of bounds")
}

func TestGraph_UnknownCoreInstanceKind(t *testing.T) {
	comp := decodeRealHello(t)
	comp.CoreInstances[0].Kind = 0x77
	_, err := runGraph(t, comp)
	requireErrContains(t, err, "unknown kind")
}

func TestGraph_InlineExportUnsupportedSort(t *testing.T) {
	comp := decodeRealHello(t)
	comp.CoreInstances[3].Exports[0].Sort = 0x03 // global: unsupported
	_, err := runGraph(t, comp)
	requireErrContains(t, err, "unsupported core:sort")
}

func TestGraph_InlineExportFuncIdxOutOfRange(t *testing.T) {
	comp := decodeRealHello(t)
	comp.CoreInstances[1].Exports[0].CoreSortIdx = 9999
	_, err := runGraph(t, comp)
	requireErrContains(t, err, "out of range of the 42-entry core func index space")
}

func TestGraph_InlineExportMemoryIdxOutOfRange(t *testing.T) {
	comp := decodeRealHello(t)
	comp.CoreInstances[3].Exports[0].CoreSortIdx = 9999
	_, err := runGraph(t, comp)
	requireErrContains(t, err, "out of range of the 1-entry core memory index space")
}

func TestGraph_InlineExportTableIdxOutOfRange(t *testing.T) {
	comp := decodeRealHello(t)
	// core instance 15's first export is the "$imports" table.
	comp.CoreInstances[15].Exports[0].CoreSortIdx = 9999
	_, err := runGraph(t, comp)
	requireErrContains(t, err, "out of range of the 1-entry core table index space")
}

func TestGraph_AliasTargetsUninstantiatedCoreInstance(t *testing.T) {
	comp := decodeRealHello(t)
	// core instance 1's first export ("fd_write") is a CoreFuncFromAlias --
	// redirect its Aliases entry to a core instance built later (14).
	entry := comp.CoreFuncSpace[comp.CoreInstances[1].Exports[0].CoreSortIdx]
	comp.Aliases[entry.Alias].InstanceIdx = 14
	_, err := runGraph(t, comp)
	requireErrContains(t, err, "was not instantiated")
}

func TestGraph_AliasTargetsMissingExport(t *testing.T) {
	comp := decodeRealHello(t)
	entry := comp.CoreFuncSpace[comp.CoreInstances[1].Exports[0].CoreSortIdx]
	comp.Aliases[entry.Alias].Name = "does-not-exist"
	_, err := runGraph(t, comp)
	requireErrContains(t, err, "has no exported function")
}

func TestGraph_MemoryAliasTargetsUninstantiated(t *testing.T) {
	comp := decodeRealHello(t)
	// The memory alias (core instance 3) targets core instance 2 (module0).
	// Find the alias entry: it's the first CoreSort==0x02 alias.
	for i := range comp.Aliases {
		al := &comp.Aliases[i]
		if al.Sort == 0x00 && al.TargetKind == 0x01 && al.CoreSort == 0x02 {
			al.InstanceIdx = 14 // not yet instantiated at that point in the graph
			break
		}
	}
	_, err := runGraph(t, comp)
	requireErrContains(t, err, "was not instantiated")
}

func TestGraph_TableAliasTargetsUninstantiated(t *testing.T) {
	comp := decodeRealHello(t)
	for i := range comp.Aliases {
		al := &comp.Aliases[i]
		if al.Sort == 0x00 && al.TargetKind == 0x01 && al.CoreSort == 0x01 {
			al.InstanceIdx = 16 // core instance 16 is built after 15, which needs this table
			break
		}
	}
	_, err := runGraph(t, comp)
	requireErrContains(t, err, "was not instantiated")
}

func TestGraph_CanonMissingConsumerType(t *testing.T) {
	comp := decodeRealHello(t)
	// core instance 10 exports "get-stderr" backed by a canon lower; rename
	// the export so neededTypes[groupName][entryName] misses.
	for i := range comp.CoreInstances[10].Exports {
		comp.CoreInstances[10].Exports[i].Name = "totally-different-name"
	}
	_, err := runGraph(t, comp)
	requireErrContains(t, err, "cannot determine the core-level signature")
}

func TestGraph_DuplicateShimName(t *testing.T) {
	comp := decodeRealHello(t)
	// Give core instance 4's group the same target name as core instance 3's
	// group ("env"), so both shims try to register under the same wazy name.
	for k, ci := range comp.CoreInstances {
		if ci.Kind != 0x00 {
			continue
		}
		for j, a := range ci.Args {
			if a.InstanceIdx == 4 {
				comp.CoreInstances[k].Args[j].InstanceIdx = 4
				comp.CoreInstances[k].Args[j].Name = "env"
			}
		}
	}
	_, err := runGraph(t, comp)
	if err == nil {
		t.Fatal("expected an error from a duplicate shim registration name")
	}
}

// TestGraph_PlainFuncExport exercises bindImportExportsGraph's func-export
// branch (0x01), which real_hello itself never uses directly -- its only
// top-level export is instance-typed -- by adding a second export that
// names the same lift canon as a bare func.
func TestGraph_PlainFuncExport(t *testing.T) {
	comp := decodeRealHello(t)
	var compFuncAliasCount uint32
	for _, al := range comp.Aliases {
		if al.Sort == 0x01 && al.TargetKind == 0x00 {
			compFuncAliasCount++
		}
	}
	comp.Exports = append(comp.Exports, binary.Export{Name: "extra-run", ExternType: 0x01, ExternIndex: compFuncAliasCount})

	inst, err := runGraph(t, comp)
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	defer inst.Close(context.Background())
	if _, ok := inst.exports["extra-run"]; !ok {
		t.Fatal("expected a bound export named \"extra-run\"")
	}
}

func TestGraph_ExportNotFuncOrInstance(t *testing.T) {
	comp := decodeRealHello(t)
	comp.Exports[0].ExternType = 0x02 // value
	_, err := runGraph(t, comp)
	requireErrContains(t, err, "only func and instance exports")
}

func TestGraph_BindInstanceExport_ImportedInstanceDirectly(t *testing.T) {
	comp := decodeRealHello(t)
	comp.Exports[0].ExternIndex = 0 // an imported instance, not the locally-instantiated one
	_, err := runGraph(t, comp)
	requireErrContains(t, err, "re-exported directly")
}

func TestGraph_BindInstanceExport_OutOfRange(t *testing.T) {
	comp := decodeRealHello(t)
	comp.Exports[0].ExternIndex = 9999
	_, err := runGraph(t, comp)
	requireErrContains(t, err, "out of range of")
}

// TestGraph_BindInstanceExport_UnsupportedInstanceKindTraps pins the
// still-rejected case: an Instance.Kind that is neither 0x00 (instantiate)
// nor 0x01 (inline exports, supported since this session's big-interleaving-
// test fix below) -- unreachable through a real decode (the decoder only
// ever produces 0x00/0x01), but kept as a defensive fail-loud guard against a
// future/malformed extension.
func TestGraph_BindInstanceExport_UnsupportedInstanceKindTraps(t *testing.T) {
	comp := decodeRealHello(t)
	comp.Instances[0].Kind = 0x02
	_, err := runGraph(t, comp)
	requireErrContains(t, err, "not a component instantiation")
}

// TestGraph_BindInstanceExport_InlineKindTypeOnlyBinds pins Instance.Kind ==
// 0x01 (inline exports): a synthetic instance built purely by re-listing
// existing sort entries, no nested-component instantiation at all (the WIT-
// tooling pattern big-interleaving-test.wast's `(instance $types (export
// "event-kind" (type $driver "event-kind")) ...)` uses to re-export types for
// external binding-generator consumption). A type-sort member has nothing
// callable to bind, so it's skipped -- mirroring bindImportExportsGraph's own
// ExternType==0x03 skip for a plain top-level type re-export -- and
// instantiation succeeds.
func TestGraph_BindInstanceExport_InlineKindTypeOnlyBinds(t *testing.T) {
	comp := decodeRealHello(t)
	comp.Instances[0].Kind = 0x01
	comp.Instances[0].Exports = []binary.InlineExport{{Name: "some-type", Sort: 0x03, SortIdx: 0}}
	if _, err := runGraph(t, comp); err != nil {
		t.Fatalf("expected an inline-export instance with only a type member to bind (skip) cleanly, got error: %v", err)
	}
}

func TestGraph_BindInstanceExport_NestedComponentOutOfRange(t *testing.T) {
	comp := decodeRealHello(t)
	comp.Instances[0].ComponentIdx = 99
	_, err := runGraph(t, comp)
	requireErrContains(t, err, "decoded nested component")
}

func TestGraph_BindInstanceExport_NotPureShim(t *testing.T) {
	comp := decodeRealHello(t)
	comp.NestedComponents[0].Canons = append(comp.NestedComponents[0].Canons, binary.Canon{})
	_, err := runGraph(t, comp)
	requireErrContains(t, err, "not a pure re-export shim")
}

// TestGraph_BindInstanceExport_MemberNotFuncSkipped proves a non-func member
// of an exported interface is skipped (a type/value/instance re-export, e.g.
// the resource types wasi:http/incoming-handler `use`s), not rejected --
// instantiation still succeeds.
func TestGraph_BindInstanceExport_MemberNotFuncSkipped(t *testing.T) {
	comp := decodeRealHello(t)
	comp.NestedComponents[0].Exports[0].ExternType = 0x02
	if _, err := runGraph(t, comp); err != nil {
		t.Fatalf("expected non-func member to be skipped, got error: %v", err)
	}
}

// ------- direct unit tests for the lower-level graph.go helpers -------

func TestModuleKeyForGraph(t *testing.T) {
	// Unreferenced -> synthesized core%d key.
	if got, err := moduleKeyForGraph(3, nil, "anon"); err != nil || got != "core3" {
		t.Fatalf("got (%q, %v), want (\"core3\", nil)", got, err)
	}
	// A non-empty refName is the raw name consumers import -- no prefix (the key
	// lives only in the component's private resolver map, never the registry).
	if got, err := moduleKeyForGraph(3, []string{"foo"}, "anon"); err != nil || got != "foo" {
		t.Fatalf("got (%q, %v), want (\"foo\", nil)", got, err)
	}
	// The sole "" ref maps to the (stable) emptyNameTarget key.
	if got, err := moduleKeyForGraph(3, []string{""}, "anon-import"); err != nil || got != "anon-import" {
		t.Fatalf("got (%q, %v), want (\"anon-import\", nil)", got, err)
	}
	if _, err := moduleKeyForGraph(3, []string{"a", "b"}, "anon"); err == nil {
		t.Fatal("expected an error for 2 ref names")
	}
}

func TestCoreFuncSpacePartitioned(t *testing.T) {
	cases := []struct {
		name string
		in   []binary.CoreFuncSpaceEntry
		want bool
	}{
		{"empty", nil, true},
		{"canons only", []binary.CoreFuncSpaceEntry{{Kind: binary.CoreFuncFromCanon}}, true},
		{"aliases only", []binary.CoreFuncSpaceEntry{{Kind: binary.CoreFuncFromAlias}}, true},
		{"canons then aliases", []binary.CoreFuncSpaceEntry{{Kind: binary.CoreFuncFromCanon}, {Kind: binary.CoreFuncFromAlias}}, true},
		{"interleaved", []binary.CoreFuncSpaceEntry{{Kind: binary.CoreFuncFromAlias}, {Kind: binary.CoreFuncFromCanon}}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := coreFuncSpacePartitioned(c.in); got != c.want {
				t.Errorf("coreFuncSpacePartitioned(%v) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

func TestBuildCanonHostModule_LowersLiftedFunc(t *testing.T) {
	comp := decodeRealHello(t)
	// canon 21 (get-stderr's lower) normally lowers a component func index
	// that resolves to an import alias; point it past the alias space into
	// the lift space instead.
	var compFuncAliasCount int
	for _, al := range comp.Aliases {
		if al.Sort == 0x01 && al.TargetKind == 0x00 {
			compFuncAliasCount++
		}
	}
	canon := comp.Canons[4]
	if canon.Kind != 0x01 {
		t.Fatalf("expected canon[21] to be a lower, got kind %#x", canon.Kind)
	}
	canon.FuncIdx = uint32(compFuncAliasCount) // first lift index

	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)
	_, _, _, _, _, err := buildCanonHostModule(ctx, r, comp, newConfig(nil), newHandleTable(), nil, canon, nil, "g", "e", "p", nil, nil, nil)
	requireErrContains(t, err, "lowers a lifted")
}

func TestBuildCanonHostModule_ImportInterfaceNameError(t *testing.T) {
	comp := decodeRealHello(t)
	canon := comp.Canons[4]
	// Redirect the lower's target alias to reference an out-of-range
	// imported-instance index.
	aliasIdx := -1
	for i, al := range comp.Aliases {
		if al.Sort == 0x01 && al.TargetKind == 0x00 {
			aliasIdx = i
			break
		}
	}
	if aliasIdx < 0 {
		t.Fatal("no component func alias found")
	}
	comp.Aliases[aliasIdx].InstanceIdx = 9999
	canon.FuncIdx = 0 // the first component-func alias, now pointing out of range

	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)
	_, _, _, _, _, err := buildCanonHostModule(ctx, r, comp, newConfig(nil), newHandleTable(), nil, canon, nil, "g", "e", "p", nil, nil, nil)
	requireErrContains(t, err, "out of range")
}

func TestBuildCanonHostModule_WithImportOverride(t *testing.T) {
	comp := decodeRealHello(t)
	canon := comp.Canons[4] // get-stderr's lower

	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	hostFn := func(context.Context, []abi.Value) ([]abi.Value, error) { return nil, nil }
	cfg := newConfig([]Option{WithImport("wasi:cli/stderr@0.2.3", "get-stderr", hostFn, nil, nil)})

	mod, exportName, _, _, wasiCall, err := buildCanonHostModule(ctx, r, comp, cfg, newHandleTable(), nil, canon, nil, "g", "e", "wazy:component/testpriv1", nil, nil, nil)
	if err != nil {
		t.Fatalf("buildCanonHostModule: %v", err)
	}
	if mod == nil {
		t.Fatal("mod is nil")
	}
	if exportName != "f" {
		t.Fatalf("exportName = %q, want \"f\"", exportName)
	}
	if wasiCall != "" {
		t.Fatalf("wasiCall = %q, want empty (caller-provided import is not a trap stub)", wasiCall)
	}
}

func TestBuildCanonHostModule_UnsupportedCanonKind(t *testing.T) {
	comp := decodeRealHello(t)
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)
	_, _, _, _, _, err := buildCanonHostModule(ctx, r, comp, newConfig(nil), newHandleTable(), nil, binary.Canon{Kind: 0xff}, nil, "g", "e", "p", nil, nil, nil)
	requireErrContains(t, err, "does not produce a core func")
}

func TestResourceCanonHostFuncGraph_UnsupportedKind(t *testing.T) {
	comp := &binary.Component{} // ResolveType fails on TypeIdx 0 (no types at all), tolerated
	_, err := resourceCanonHostFuncGraph(comp, newConfig(nil), newHandleTable(), "x", binary.Canon{Kind: 0xff})
	requireErrContains(t, err, "unsupported resource canon kind")
}

func TestResourceCanonHostFuncGraph_ResolvableButWrongType(t *testing.T) {
	comp := &binary.Component{Types: []binary.Type{{Descriptor: binary.PrimitiveDesc{Prim: "u32"}}}}
	_, err := resourceCanonHostFuncGraph(comp, newConfig(nil), newHandleTable(), "x", binary.Canon{Kind: 0x02, TypeIdx: 0})
	requireErrContains(t, err, "is not a resource type")
}

// TestResourceCanonHostFuncGraph_NewAndRep exercises resource.new (0x02) and
// resource.rep (0x04) end to end -- unlike real_hello, which only ever
// declares resource.drop canons -- by actually invoking the returned Go
// funcs against a live handleTable, round-tripping a rep through a handle.
func TestResourceCanonHostFuncGraph_NewAndRep(t *testing.T) {
	comp := &binary.Component{}
	resources := newHandleTable()
	ctx := context.Background()

	newDef, err := resourceCanonHostFuncGraph(comp, newConfig(nil), resources, "new", binary.Canon{Kind: 0x02, TypeIdx: 7})
	if err != nil {
		t.Fatalf("resource.new: %v", err)
	}
	stack := []uint64{99} // rep
	newDef.fn.Call(ctx, nil, stack)
	handle := uint32(stack[0])

	repDef, err := resourceCanonHostFuncGraph(comp, newConfig(nil), resources, "rep", binary.Canon{Kind: 0x04, TypeIdx: 7})
	if err != nil {
		t.Fatalf("resource.rep: %v", err)
	}
	stack2 := []uint64{uint64(handle)}
	repDef.fn.Call(ctx, nil, stack2)
	if uint32(stack2[0]) != 99 {
		t.Fatalf("resource.rep(new(99)) = %d, want 99", uint32(stack2[0]))
	}

	dropDef, err := resourceCanonHostFuncGraph(comp, newConfig(nil), resources, "drop", binary.Canon{Kind: 0x03, TypeIdx: 7})
	if err != nil {
		t.Fatalf("resource.drop: %v", err)
	}
	stack3 := []uint64{uint64(handle)}
	dropDef.fn.Call(ctx, nil, stack3) // must not panic
}

// TestResourceCanonHostFuncGraph_RepPanicsOnBadHandle proves the built
// resource.rep func's panic-on-error path actually runs (not dead code): an
// unknown handle must panic, matching resourceCanonHostFunc's own contract
// for the same case.
func TestResourceCanonHostFuncGraph_RepPanicsOnBadHandle(t *testing.T) {
	comp := &binary.Component{}
	resources := newHandleTable()
	def, err := resourceCanonHostFuncGraph(comp, newConfig(nil), resources, "rep", binary.Canon{Kind: 0x04, TypeIdx: 7})
	if err != nil {
		t.Fatalf("resource.rep: %v", err)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("expected a panic for an unknown handle")
		}
	}()
	def.fn.Call(context.Background(), nil, []uint64{12345})
}

// TestResourceCanonHostFuncGraph_DropPanicsOnBadHandle mirrors
// TestResourceCanonHostFuncGraph_RepPanicsOnBadHandle for resource.drop.
func TestResourceCanonHostFuncGraph_DropPanicsOnBadHandle(t *testing.T) {
	comp := &binary.Component{}
	resources := newHandleTable()
	def, err := resourceCanonHostFuncGraph(comp, newConfig(nil), resources, "drop", binary.Canon{Kind: 0x03, TypeIdx: 7})
	if err != nil {
		t.Fatalf("resource.drop: %v", err)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("expected a panic for an unknown handle")
		}
	}()
	def.fn.Call(context.Background(), nil, []uint64{12345})
}

func TestResolvePostReturnFuncGraph_NoOpt(t *testing.T) {
	name, err := resolvePostReturnFuncGraph(binary.Canon{}, nil, nil)
	if err != nil || name != "" {
		t.Fatalf("got (%q, %v), want (\"\", nil)", name, err)
	}
}

var errBoom = errors.New("boom")

func TestBindFuncExportGraph_ComponentFuncError(t *testing.T) {
	componentFunc := func(uint32) (bool, int, aliasTarget, error) { return false, 0, aliasTarget{}, errBoom }
	_, err := bindFuncExportGraph(&binary.Component{}, 0, componentFunc, nil, nil, "x", nil, nil)
	requireErrContains(t, err, "boom")
}

func TestBindFuncExportGraph_ResolvesToImport(t *testing.T) {
	componentFunc := func(uint32) (bool, int, aliasTarget, error) { return false, 0, aliasTarget{}, nil }
	_, err := bindFuncExportGraph(&binary.Component{}, 0, componentFunc, nil, nil, "x", nil, nil)
	requireErrContains(t, err, "resolves to an imported func")
}

func TestBindFuncExportGraph_ResolveTypeError(t *testing.T) {
	comp := &binary.Component{Canons: []binary.Canon{{Kind: 0x00, TypeIdx: 99}}}
	componentFunc := func(uint32) (bool, int, aliasTarget, error) { return true, 0, aliasTarget{}, nil }
	_, err := bindFuncExportGraph(comp, 0, componentFunc, nil, nil, "x", nil, nil)
	requireErrContains(t, err, "lift references type")
}

func TestBindFuncExportGraph_NotFuncType(t *testing.T) {
	comp := &binary.Component{
		Types:  []binary.Type{{Descriptor: binary.PrimitiveDesc{Prim: "u32"}}},
		Canons: []binary.Canon{{Kind: 0x00, TypeIdx: 0}},
	}
	componentFunc := func(uint32) (bool, int, aliasTarget, error) { return true, 0, aliasTarget{}, nil }
	_, err := bindFuncExportGraph(comp, 0, componentFunc, nil, nil, "x", nil, nil)
	requireErrContains(t, err, "is not a func type")
}

func TestBindFuncExportGraph_CoreFuncTargetError(t *testing.T) {
	fd := binary.FuncDesc{}
	comp := &binary.Component{
		Types:  []binary.Type{{Descriptor: fd}},
		Canons: []binary.Canon{{Kind: 0x00, TypeIdx: 0, CoreFuncIdx: 3}},
	}
	componentFunc := func(uint32) (bool, int, aliasTarget, error) { return true, 0, aliasTarget{}, nil }
	coreFuncTarget := func(int) (api.Module, string, error) { return nil, "", errBoom }
	_, err := bindFuncExportGraph(comp, 0, componentFunc, coreFuncTarget, nil, "x", nil, nil)
	requireErrContains(t, err, "boom")
}

// TestBindFuncExportGraph_AsyncNoCallback pins the async-no-callback
// STACKFUL sub-shape's binding (docs/component-model-async-stackful-
// design.md §9): a canon lift with the async opt but no callback opt is
// no longer rejected -- it binds be.stackful/be.stackfulAsyncOpts instead,
// routed through invokeStackful.
func TestBindFuncExportGraph_AsyncNoCallback(t *testing.T) {
	_, mod := memModule(t)
	fd := binary.FuncDesc{}
	comp := &binary.Component{
		Types:  []binary.Type{{Descriptor: fd}},
		Canons: []binary.Canon{{Kind: 0x00, TypeIdx: 0, CoreFuncIdx: 0, Opts: []binary.CanonOpt{{Kind: 0x06}}}}, // async, no callback opt
	}
	componentFunc := func(uint32) (bool, int, aliasTarget, error) { return true, 0, aliasTarget{}, nil }
	coreFuncTarget := func(int) (api.Module, string, error) { return mod, "main", nil }
	be, err := bindFuncExportGraph(comp, 0, componentFunc, coreFuncTarget, nil, "x", nil, nil)
	if err != nil {
		t.Fatalf("bindFuncExportGraph: %v", err)
	}
	if !be.stackful || !be.stackfulAsyncOpts {
		t.Fatalf("be.stackful=%v be.stackfulAsyncOpts=%v, want both true", be.stackful, be.stackfulAsyncOpts)
	}
	if be.asyncCallback {
		t.Fatal("be.asyncCallback should be false for the no-callback sub-shape")
	}
}

func TestBindFuncExportGraph_PostReturnError(t *testing.T) {
	fd := binary.FuncDesc{}
	comp := &binary.Component{
		Types:  []binary.Type{{Descriptor: fd}},
		Canons: []binary.Canon{{Kind: 0x00, TypeIdx: 0, CoreFuncIdx: 0, Opts: []binary.CanonOpt{{Kind: 0x05, Idx: 1}}}},
	}
	componentFunc := func(uint32) (bool, int, aliasTarget, error) { return true, 0, aliasTarget{}, nil }
	calls := 0
	coreFuncTarget := func(int) (api.Module, string, error) {
		calls++
		if calls == 1 {
			return nil, "main", nil // the lift's own core func
		}
		return nil, "", errBoom // the post-return lookup
	}
	_, err := bindFuncExportGraph(comp, 0, componentFunc, coreFuncTarget, nil, "x", nil, nil)
	requireErrContains(t, err, "boom")
}

func TestResolvePostReturnFuncGraph_TargetError(t *testing.T) {
	canon := binary.Canon{Opts: []binary.CanonOpt{{Kind: 0x05, Idx: 5}}}
	coreFuncTarget := func(int) (api.Module, string, error) { return nil, "", errBoom }
	_, err := resolvePostReturnFuncGraph(canon, coreFuncTarget, nil)
	requireErrContains(t, err, "post-return")
}

func TestResolvePostReturnFuncGraph_CrossInstanceMismatch(t *testing.T) {
	comp := decodeRealHello(t)
	inst, err := Instantiate(context.Background(), wazy.NewRuntime(context.Background()), realHelloWasm)
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	defer inst.Close(context.Background())
	_ = comp

	modA := inst.exports["wasi:cli/run@0.2.3#run"].mod
	canon := binary.Canon{Opts: []binary.CanonOpt{{Kind: 0x05, Idx: 0}}}
	coreFuncTarget := func(int) (api.Module, string, error) { return nil, "other", nil }
	_, err = resolvePostReturnFuncGraph(canon, coreFuncTarget, modA)
	requireErrContains(t, err, "cross-instance post-return")
}

// TestDiscoverNeededFuncTypes_BadImportTypeIndex covers the malformed-input
// guard on the decode-only discovery path. A core module whose func import
// declares a type index past its type section decodes fine (the decoder does
// not cross-check the index), so discoverNeededFuncTypes must catch it with a
// clean error rather than let the out-of-range index reach a slice index panic
// -- the old compile-based discovery got this validation from the compiler; the
// decode-only path adds an explicit bounds check.
func TestDiscoverNeededFuncTypes_BadImportTypeIndex(t *testing.T) {
	// Minimal core module: one empty func type, then a func import "m"."f"
	// declaring type index 5 -- out of range of the 1-entry type section.
	coreBytes := []byte{
		0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00, // magic + version
		0x01, 0x04, 0x01, 0x60, 0x00, 0x00, // type section: 1 func type () -> ()
		0x02, 0x07, 0x01, 0x01, 'm', 0x01, 'f', 0x00, 0x05, // import m.f func, typeidx 5
	}
	comp := &binary.Component{CoreModules: []binary.CoreModule{{Offset: 0, Size: len(coreBytes)}}}

	r := wazy.NewRuntime(context.Background())
	defer r.Close(context.Background())

	_, _, err := discoverNeededFuncTypes(r, comp, coreBytes, "")
	requireErrContains(t, err, "out of range of the")
}
