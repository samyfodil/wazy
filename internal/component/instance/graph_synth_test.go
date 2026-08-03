package instance

import (
	"bytes"
	"context"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/component/binary"
	"github.com/samyfodil/wazy/internal/leb128"
	"github.com/samyfodil/wazy/internal/testing/binaryencoding"
	"github.com/samyfodil/wazy/internal/wasm"
)

// This file builds SYNTHETIC component binaries in Go, in place of vendoring
// giant real-world fixtures, to pin the four instantiation-graph behaviors a
// real componentize-py CPython component depends on and which no decoded
// fixture in this package exercises:
//
//  1. one source core instance regrouped under several consumer-declared
//     "with" names,
//  2. several DIFFERENT source core instances sharing one such name,
//  3. passthrough of an immutable AND a mutable core global, with object
//     identity preserved (the shared-everything dynamic-linking shape:
//     `__stack_pointer` and the GOT tables),
//  4. several exported component instances, where each export interleaved
//     between two instance definitions shifts the later definition's index.
//
// Cases 1-3 are SELF-CHECKING without any component export: each consuming
// core module carries a wasm `start` function that reads what it was wired to
// and executes `unreachable` when the value is wrong. The graph runs a core
// module's start section during instantiation, so a mis-wired graph fails
// instantiateGraph outright rather than needing a lifted export to observe.
//
// Everything is generated in code -- no testdata file, hence nothing for the
// scratch/BSD CI jobs (which run prebuilt test binaries from the repo root) to
// fail to find.

// ---------------------------------------------------------------------------
// component binary assembly
// ---------------------------------------------------------------------------

// compBuilder assembles a component binary section by section. Sections are
// emitted in append order, which is exactly what the decoder's index spaces
// (TypeSpace / CoreFuncSpace / ComponentFuncSpace / ComponentInstanceSpace)
// are built from -- so a test can reproduce a real binary's cross-section
// interleaving by calling the section methods in the order it wants.
type compBuilder struct {
	buf bytes.Buffer
}

func newCompBuilder() *compBuilder {
	b := &compBuilder{}
	b.buf.Write([]byte{0x00, 0x61, 0x73, 0x6d}) // magic
	b.buf.Write([]byte{0x0d, 0x00})             // version 13
	b.buf.Write([]byte{0x01, 0x00})             // layer 1 (component)
	return b
}

func (b *compBuilder) section(id byte, body []byte) {
	b.buf.WriteByte(id)
	b.buf.Write(leb128.EncodeUint32(uint32(len(body))))
	b.buf.Write(body)
}

func (b *compBuilder) bytes() []byte { return b.buf.Bytes() }

// coreModule appends section 1, whose body is the core module binary itself.
func (b *compBuilder) coreModule(mod []byte) { b.section(1, mod) }

// label encodes a core:name / label: a length-prefixed UTF-8 string.
func label(s string) []byte {
	return append(leb128.EncodeUint32(uint32(len(s))), s...)
}

// externName encodes an import/export name: a 0x00 kind byte then a label.
func externName(s string) []byte { return append([]byte{0x00}, label(s)...) }

// vec prefixes an already-encoded element list with its count.
func vec(count int, body []byte) []byte {
	return append(leb128.EncodeUint32(uint32(count)), body...)
}

// synthArg is one core:instantiatearg: a "with" name bound to a core instance.
type synthArg struct {
	name string
	inst uint32
}

// synthInline is one core:inlineexport: a name bound to a core:sortidx.
type synthInline struct {
	name string
	sort byte // 0x00 func, 0x01 table, 0x02 memory, 0x03 global
	idx  uint32
}

// synthCoreInstance is one core:instance: either an instantiate (args != nil,
// or kind 0) or an inline-export group.
type synthCoreInstance struct {
	inline    []synthInline // non-nil => the inline-export form
	moduleIdx uint32
	args      []synthArg
}

// coreInstances appends section 2.
func (b *compBuilder) coreInstances(insts []synthCoreInstance) {
	var body bytes.Buffer
	for _, ci := range insts {
		if ci.inline != nil {
			body.WriteByte(0x01)
			body.Write(leb128.EncodeUint32(uint32(len(ci.inline))))
			for _, e := range ci.inline {
				body.Write(label(e.name))
				body.WriteByte(e.sort)
				body.Write(leb128.EncodeUint32(e.idx))
			}
			continue
		}
		body.WriteByte(0x00)
		body.Write(leb128.EncodeUint32(ci.moduleIdx))
		body.Write(leb128.EncodeUint32(uint32(len(ci.args))))
		for _, a := range ci.args {
			body.Write(label(a.name))
			body.WriteByte(0x12) // instance sort prefix
			body.Write(leb128.EncodeUint32(a.inst))
		}
	}
	b.section(2, vec(len(insts), body.Bytes()))
}

// synthAlias is one core-export alias: `(alias core export <instance> "<name>")`
// for the given core:sort.
type synthAlias struct {
	coreSort byte
	inst     uint32
	name     string
}

// coreAliases appends section 6 holding only core-export aliases.
func (b *compBuilder) coreAliases(aliases []synthAlias) {
	var body bytes.Buffer
	for _, al := range aliases {
		body.WriteByte(0x00)        // sort: core
		body.WriteByte(al.coreSort) // core:sort
		body.WriteByte(0x01)        // target: core export
		body.Write(leb128.EncodeUint32(al.inst))
		body.Write(label(al.name))
	}
	b.section(6, vec(len(aliases), body.Bytes()))
}

// funcTypeU32Result appends section 7 with a single `(func (result u32))`.
func (b *compBuilder) funcTypeU32Result() {
	// 0x40 functype, 0 params, 0x00 unnamed result, 0x79 u32.
	b.section(7, vec(1, []byte{0x40, 0x00, 0x00, 0x79}))
}

// canonLift appends section 8 with a single `(canon lift <coreFuncIdx>
// (type <typeIdx>))`.
func (b *compBuilder) canonLift(coreFuncIdx, typeIdx uint32) {
	var body bytes.Buffer
	body.WriteByte(0x00) // canon kind: lift
	body.WriteByte(0x00) // lift prefix
	body.Write(leb128.EncodeUint32(coreFuncIdx))
	body.WriteByte(0x00) // no opts
	body.Write(leb128.EncodeUint32(typeIdx))
	b.section(8, vec(1, body.Bytes()))
}

// inlineExportInstance appends section 5 with a single inline-export component
// instance re-listing already-defined component funcs.
func (b *compBuilder) inlineExportInstance(members map[string]uint32) {
	var body bytes.Buffer
	body.WriteByte(0x01) // inline exports
	body.Write(leb128.EncodeUint32(uint32(len(members))))
	for name, idx := range members {
		body.Write(externName(name))
		body.WriteByte(0x01) // sort: func
		body.Write(leb128.EncodeUint32(idx))
	}
	b.section(5, vec(1, body.Bytes()))
}

// exportInstance appends section 11 exporting one component instance by index.
func (b *compBuilder) exportInstance(name string, instIdx uint32) {
	var body bytes.Buffer
	body.Write(externName(name))
	body.WriteByte(0x05) // sort: instance
	body.Write(leb128.EncodeUint32(instIdx))
	body.WriteByte(0x00) // no ascribed type
	b.section(11, vec(1, body.Bytes()))
}

func decodeSynth(t *testing.T, b *compBuilder) ([]byte, *binary.Component) {
	t.Helper()
	raw := b.bytes()
	comp, err := binary.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("decode synthetic component: %v", err)
	}
	return raw, comp
}

func runSynth(t *testing.T, raw []byte, comp *binary.Component) (*Instance, error) {
	t.Helper()
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	t.Cleanup(func() { r.Close(ctx) })
	return instantiateGraph(ctx, r, comp, raw, newConfig(nil))
}

// ---------------------------------------------------------------------------
// core module assembly
// ---------------------------------------------------------------------------

func i32Const(v int32) []byte {
	return append([]byte{wasm.OpcodeI32Const}, leb128.EncodeInt32(v)...)
}

// providerModule exports a func returning `ret`, an immutable i32 global
// `const-g`, a mutable i32 global `mut-g` and a memory -- the four sorts a
// shared-everything dynamic library regroups through inline-export instances.
func providerModule(ret int32, constVal, mutVal int32) []byte {
	body := append(i32Const(ret), wasm.OpcodeEnd)
	return binaryencoding.EncodeModule(&wasm.Module{
		TypeSection:     []wasm.FunctionType{{Results: []wasm.ValueType{wasm.ValueTypeI32}, ResultNumInUint64: 1}},
		FunctionSection: []wasm.Index{0},
		MemorySection:   &wasm.Memory{Min: 1, Cap: 1, Max: 1, IsMaxEncoded: true},
		GlobalSection: []wasm.Global{
			{Type: wasm.GlobalType{ValType: wasm.ValueTypeI32}, Init: wasm.NewConstantExpressionFromOpcode(wasm.OpcodeI32Const, leb128.EncodeInt32(constVal))},
			{Type: wasm.GlobalType{ValType: wasm.ValueTypeI32, Mutable: true}, Init: wasm.NewConstantExpressionFromOpcode(wasm.OpcodeI32Const, leb128.EncodeInt32(mutVal))},
		},
		CodeSection: []wasm.Code{{Body: body}},
		ExportSection: []wasm.Export{
			{Name: "id", Type: wasm.ExternTypeFunc, Index: 0},
			{Name: "const-g", Type: wasm.ExternTypeGlobal, Index: 0},
			{Name: "mut-g", Type: wasm.ExternTypeGlobal, Index: 1},
			{Name: "memory", Type: wasm.ExternTypeMemory, Index: 0},
		},
	})
}

// checkFuncModule imports (fromModule, "id") -> i32 and traps in its start
// function unless it returns want. Any mis-resolution of `fromModule` is
// therefore a hard instantiation failure.
func checkFuncModule(fromModule string, want int32) []byte {
	// start: if (call $id) != want { unreachable }
	body := []byte{wasm.OpcodeCall, 0x00}
	body = append(body, i32Const(want)...)
	body = append(body, wasm.OpcodeI32Ne, wasm.OpcodeIf, 0x40, wasm.OpcodeUnreachable, wasm.OpcodeEnd, wasm.OpcodeEnd)
	start := wasm.Index(1)
	return binaryencoding.EncodeModule(&wasm.Module{
		TypeSection: []wasm.FunctionType{
			{Results: []wasm.ValueType{wasm.ValueTypeI32}, ResultNumInUint64: 1},
			{},
		},
		ImportSection:       []wasm.Import{{Module: fromModule, Name: "id", Type: wasm.ExternTypeFunc, DescFunc: 0}},
		ImportFunctionCount: 1,
		FunctionSection:     []wasm.Index{1},
		CodeSection:         []wasm.Code{{Body: body}},
		StartSection:        &start,
	})
}

// checkGlobalModule imports an immutable and a mutable i32 global plus a
// memory, traps unless they hold wantConst/wantMut, then STORES setMut into
// the mutable one -- so a later module reading it proves the shim shared the
// one GlobalInstance rather than copying a value.
func checkGlobalModule(fromModule string, wantConst, wantMut, setMut int32) []byte {
	var body []byte
	body = append(body, wasm.OpcodeGlobalGet, 0x00)
	body = append(body, i32Const(wantConst)...)
	body = append(body, wasm.OpcodeI32Ne, wasm.OpcodeIf, 0x40, wasm.OpcodeUnreachable, wasm.OpcodeEnd)
	body = append(body, wasm.OpcodeGlobalGet, 0x01)
	body = append(body, i32Const(wantMut)...)
	body = append(body, wasm.OpcodeI32Ne, wasm.OpcodeIf, 0x40, wasm.OpcodeUnreachable, wasm.OpcodeEnd)
	body = append(body, i32Const(setMut)...)
	body = append(body, wasm.OpcodeGlobalSet, 0x01, wasm.OpcodeEnd)
	start := wasm.Index(0)
	return binaryencoding.EncodeModule(&wasm.Module{
		TypeSection: []wasm.FunctionType{{}},
		ImportSection: []wasm.Import{
			{Module: fromModule, Name: "const-g", Type: wasm.ExternTypeGlobal, DescGlobal: wasm.GlobalType{ValType: wasm.ValueTypeI32}},
			{Module: fromModule, Name: "mut-g", Type: wasm.ExternTypeGlobal, DescGlobal: wasm.GlobalType{ValType: wasm.ValueTypeI32, Mutable: true}},
			{Module: fromModule, Name: "memory", Type: wasm.ExternTypeMemory, DescMem: &wasm.Memory{Min: 1, Cap: 1, Max: 1, IsMaxEncoded: true}},
		},
		FunctionSection: []wasm.Index{0},
		CodeSection:     []wasm.Code{{Body: body}},
		StartSection:    &start,
	})
}

// ---------------------------------------------------------------------------
// 1. one source instance regrouped under several consumer names
// ---------------------------------------------------------------------------

// A single provider core instance is passed to two different consumers under
// two different "with" names. Before core-instance identity keys, the graph
// refused this outright ("referenced under 2 names"). Both consumers' start
// functions assert they reached the real provider.
func TestSynthGraph_OneSourceRegroupedUnderTwoNames(t *testing.T) {
	b := newCompBuilder()
	b.coreModule(providerModule(11, 0, 0))     // core module 0
	b.coreModule(checkFuncModule("alpha", 11)) // core module 1
	b.coreModule(checkFuncModule("beta", 11))  // core module 2
	b.coreInstances([]synthCoreInstance{
		{moduleIdx: 0}, // 0: the provider
		{inline: []synthInline{{name: "id", sort: 0x00, idx: 0}}}, // 1: regroup its func
		{moduleIdx: 1, args: []synthArg{{"alpha", 1}}},            // 2: consumer A
		{moduleIdx: 2, args: []synthArg{{"beta", 1}}},             // 3: consumer B
	})
	b.coreAliases([]synthAlias{{coreSort: 0x00, inst: 0, name: "id"}})

	raw, comp := decodeSynth(t, b)
	in, err := runSynth(t, raw, comp)
	if err != nil {
		t.Fatalf("instantiate: %v", err)
	}
	in.Close(context.Background())
}

// ---------------------------------------------------------------------------
// 2. two source instances colliding on one consumer name
// ---------------------------------------------------------------------------

// Two DIFFERENT regrouping shims are both named "env" -- componentize-py's
// shape, where every dynamic library gets its own "env". The graph-wide
// name->module map is last-writer-wins, so before instantiate-args were
// resolved locally the FIRST consumer silently reached the SECOND provider
// (or, when the later group didn't export the name at all, failed with
// `"cabi_realloc" is not exported in module ""`). Each consumer's start
// function asserts it reached its own provider.
func TestSynthGraph_CollidingConsumerNamesResolveLocally(t *testing.T) {
	b := newCompBuilder()
	b.coreModule(providerModule(21, 0, 0))   // core module 0
	b.coreModule(providerModule(22, 0, 0))   // core module 1
	b.coreModule(checkFuncModule("env", 21)) // core module 2: must reach provider A
	b.coreModule(checkFuncModule("env", 22)) // core module 3: must reach provider B
	b.coreInstances([]synthCoreInstance{
		{moduleIdx: 0}, // 0: provider A
		{moduleIdx: 1}, // 1: provider B
		{inline: []synthInline{{name: "id", sort: 0x00, idx: 0}}}, // 2: "env" -> A's id
		{inline: []synthInline{{name: "id", sort: 0x00, idx: 1}}}, // 3: "env" -> B's id
		{moduleIdx: 2, args: []synthArg{{"env", 2}}},              // 4: consumer of A
		{moduleIdx: 3, args: []synthArg{{"env", 3}}},              // 5: consumer of B
	})
	b.coreAliases([]synthAlias{
		{coreSort: 0x00, inst: 0, name: "id"},
		{coreSort: 0x00, inst: 1, name: "id"},
	})

	raw, comp := decodeSynth(t, b)
	in, err := runSynth(t, raw, comp)
	if err != nil {
		t.Fatalf("instantiate: %v", err)
	}
	in.Close(context.Background())
}

// The consumer declared LAST is the one the old graph-wide map happened to
// resolve correctly; this variant puts BOTH consumers after both groups, so a
// name-keyed resolver sends the first consumer to the wrong provider.
func TestSynthGraph_CollidingNamesWithBothGroupsFirst(t *testing.T) {
	b := newCompBuilder()
	b.coreModule(providerModule(31, 0, 0))
	b.coreModule(providerModule(32, 0, 0))
	b.coreModule(checkFuncModule("env", 31))
	b.coreModule(checkFuncModule("env", 32))
	b.coreInstances([]synthCoreInstance{
		{moduleIdx: 0}, // 0
		{moduleIdx: 1}, // 1
		{inline: []synthInline{{name: "id", sort: 0x00, idx: 0}}}, // 2
		{inline: []synthInline{{name: "id", sort: 0x00, idx: 1}}}, // 3
		{moduleIdx: 2, args: []synthArg{{"env", 2}}},              // 4
		{moduleIdx: 3, args: []synthArg{{"env", 3}}},              // 5
	})
	b.coreAliases([]synthAlias{
		{coreSort: 0x00, inst: 0, name: "id"},
		{coreSort: 0x00, inst: 1, name: "id"},
	})
	raw, comp := decodeSynth(t, b)
	in, err := runSynth(t, raw, comp)
	if err != nil {
		t.Fatalf("instantiate: %v", err)
	}
	in.Close(context.Background())
}

// The exact shape that broke a componentize-py CPython component: a
// regrouping shim aliases core instance 0's "id", but core instance 0 and
// core instance 1 are BOTH bound under the consumer name "env". Naming the
// shim's source by that consumer name resolved it to core instance 1 (the
// later writer), which exports only a memory -- reproducing
// `"cabi_realloc" is not exported in module ""` with "id" in place of
// cabi_realloc. Naming it by core-instance identity cannot go wrong.
func TestSynthGraph_ShimSourceIsIdentityNotConsumerName(t *testing.T) {
	b := newCompBuilder()
	b.coreModule(providerModule(71, 3, 4))          // core module 0
	b.coreModule(checkFuncModule("grp", 71))        // core module 1
	b.coreModule(checkGlobalModule("env", 3, 4, 4)) // core module 2
	b.coreInstances([]synthCoreInstance{
		{moduleIdx: 0}, // 0: the provider, bound as "env" by core instance 3
		{inline: []synthInline{ // 1: ALSO bound as "env", by core instance 4
			{name: "memory", sort: 0x02, idx: 0},
			{name: "const-g", sort: 0x03, idx: 0},
			{name: "mut-g", sort: 0x03, idx: 1},
		}},
		{inline: []synthInline{{name: "id", sort: 0x00, idx: 0}}}, // 2: aliases instance 0's "id"
		{moduleIdx: 1, args: []synthArg{{"grp", 2}, {"env", 0}}},  // 3
		{moduleIdx: 2, args: []synthArg{{"env", 1}}},              // 4
	})
	b.coreAliases([]synthAlias{
		{coreSort: 0x00, inst: 0, name: "id"},
		{coreSort: 0x02, inst: 0, name: "memory"},
		{coreSort: 0x03, inst: 0, name: "const-g"},
		{coreSort: 0x03, inst: 0, name: "mut-g"},
	})

	raw, comp := decodeSynth(t, b)
	if got := comp.CoreInstances[3].Args[1].Name; got != "env" {
		t.Fatalf("fixture: core instance 3 arg[1] name = %q, want \"env\"", got)
	}
	in, err := runSynth(t, raw, comp)
	if err != nil {
		t.Fatalf("instantiate: %v", err)
	}
	in.Close(context.Background())
}

// A provider core module has both a memory (core memory index 0) and globals,
// but the provider itself is the source of a memory REGROUPED under a name
// another instance also claims -- the memory/table sorts take the same
// identity path as funcs and globals.
func TestSynthGraph_MemoryPassthroughUsesIdentity(t *testing.T) {
	b := newCompBuilder()
	b.coreModule(providerModule(81, 5, 6))          // core module 0
	b.coreModule(providerModule(82, 7, 8))          // core module 1
	b.coreModule(checkGlobalModule("env", 5, 6, 6)) // core module 2: must see module 0's
	b.coreInstances([]synthCoreInstance{
		{moduleIdx: 0}, // 0
		{moduleIdx: 1}, // 1
		{inline: []synthInline{ // 2: regroups instance 0's items, named "env"
			{name: "memory", sort: 0x02, idx: 0},
			{name: "const-g", sort: 0x03, idx: 0},
			{name: "mut-g", sort: 0x03, idx: 1},
		}},
		{inline: []synthInline{ // 3: regroups instance 1's items, ALSO named "env"
			{name: "memory", sort: 0x02, idx: 1},
			{name: "const-g", sort: 0x03, idx: 2},
			{name: "mut-g", sort: 0x03, idx: 3},
		}},
		{moduleIdx: 2, args: []synthArg{{"env", 2}}}, // 4
		{moduleIdx: 2, args: []synthArg{{"env", 2}}}, // 5
	})
	b.coreAliases([]synthAlias{
		{coreSort: 0x02, inst: 0, name: "memory"},
		{coreSort: 0x03, inst: 0, name: "const-g"},
		{coreSort: 0x03, inst: 0, name: "mut-g"},
		{coreSort: 0x02, inst: 1, name: "memory"},
		{coreSort: 0x03, inst: 1, name: "const-g"},
		{coreSort: 0x03, inst: 1, name: "mut-g"},
	})
	raw, comp := decodeSynth(t, b)
	in, err := runSynth(t, raw, comp)
	if err != nil {
		t.Fatalf("instantiate: %v", err)
	}
	in.Close(context.Background())
}

func TestSynthGraph_InstantiateArgNamesUninstantiatedInstance(t *testing.T) {
	b := newCompBuilder()
	b.coreModule(providerModule(41, 0, 0))
	b.coreModule(checkFuncModule("env", 41))
	b.coreInstances([]synthCoreInstance{
		{moduleIdx: 0}, // 0
		{inline: []synthInline{{name: "id", sort: 0x00, idx: 0}}}, // 1
		{moduleIdx: 1, args: []synthArg{{"env", 9}}},              // 2: forward ref
	})
	b.coreAliases([]synthAlias{{coreSort: 0x00, inst: 0, name: "id"}})
	raw, comp := decodeSynth(t, b)
	_, err := runSynth(t, raw, comp)
	requireErrContains(t, err, `core module 1 instantiate arg "env" references core instance 9, which was not instantiated`)
}

// ---------------------------------------------------------------------------
// 3. immutable + mutable core global passthrough
// ---------------------------------------------------------------------------

// A regrouping shim carries an immutable and a mutable core global (plus a
// memory, so the shape matches a real `env` group). The first consumer checks
// both initial values and writes the mutable one; the second checks it sees
// the write, which only holds if the shim re-exported the SAME GlobalInstance
// rather than a copy.
func TestSynthGraph_CoreGlobalPassthroughImmutableAndMutable(t *testing.T) {
	b := newCompBuilder()
	b.coreModule(providerModule(0, 7, 42))             // core module 0
	b.coreModule(checkGlobalModule("env", 7, 42, 99))  // core module 1
	b.coreModule(checkGlobalModule("env", 7, 99, 100)) // core module 2: sees the write
	b.coreInstances([]synthCoreInstance{
		{moduleIdx: 0}, // 0: provider
		{inline: []synthInline{ // 1: regroup memory + both globals
			{name: "memory", sort: 0x02, idx: 0},
			{name: "const-g", sort: 0x03, idx: 0},
			{name: "mut-g", sort: 0x03, idx: 1},
		}},
		{moduleIdx: 1, args: []synthArg{{"env", 1}}}, // 2
		{moduleIdx: 2, args: []synthArg{{"env", 1}}}, // 3
	})
	b.coreAliases([]synthAlias{
		{coreSort: 0x02, inst: 0, name: "memory"},
		{coreSort: 0x03, inst: 0, name: "const-g"},
		{coreSort: 0x03, inst: 0, name: "mut-g"},
	})

	raw, comp := decodeSynth(t, b)
	in, err := runSynth(t, raw, comp)
	if err != nil {
		t.Fatalf("instantiate: %v", err)
	}
	in.Close(context.Background())
}

// The shim must declare the source global's real mutability: a `const`
// declaration against a `var` source is rejected by resolveImports, and the
// two consumers above would not share state. Assert the encoder emits the
// mutability byte the item carries.
func TestBuildPassthroughShim_GlobalEncoding(t *testing.T) {
	got, err := buildPassthroughShim([]shimItem{
		{Sort: shimSortGlobal, FromModule: "m", FromName: "c", ExportName: "c2", GlobalType: api.ValueTypeI32},
		{Sort: shimSortGlobal, FromModule: "m", FromName: "v", ExportName: "v2", GlobalType: api.ValueTypeI64, GlobalMutable: true},
	})
	if err != nil {
		t.Fatalf("buildPassthroughShim: %v", err)
	}
	// importdesc global: 0x03 valtype mut
	if !bytes.Contains(got, []byte{0x03, api.ValueTypeI32, 0x00}) {
		t.Errorf("missing immutable i32 global import descriptor in %x", got)
	}
	if !bytes.Contains(got, []byte{0x03, api.ValueTypeI64, 0x01}) {
		t.Errorf("missing mutable i64 global import descriptor in %x", got)
	}
	// Both must decode and validate as a real core module, with the exports
	// at global indices 0 and 1.
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)
	dec, ok := r.(interface {
		DecodeModuleNoCompile(bin []byte) (*wasm.Module, error)
	})
	if !ok {
		t.Skip("runtime cannot decode core modules")
	}
	m, err := dec.DecodeModuleNoCompile(got)
	if err != nil {
		t.Fatalf("decode shim: %v", err)
	}
	if len(m.ImportSection) != 2 || m.ImportSection[0].DescGlobal.Mutable || !m.ImportSection[1].DescGlobal.Mutable {
		t.Fatalf("unexpected import section: %+v", m.ImportSection)
	}
	for i, exp := range m.ExportSection {
		if exp.Type != wasm.ExternTypeGlobal || exp.Index != uint32(i) {
			t.Errorf("export[%d] = %+v, want global index %d", i, exp, i)
		}
	}
}

func TestBuildPassthroughShim_GlobalWithoutValueType(t *testing.T) {
	_, err := buildPassthroughShim([]shimItem{{Sort: shimSortGlobal, FromModule: "m", FromName: "g", ExportName: "g"}})
	requireErrContains(t, err, "has no value type")
}

func TestSynthGraph_InlineExportGlobalMissingOnSource(t *testing.T) {
	b := newCompBuilder()
	b.coreModule(providerModule(0, 7, 42))
	b.coreInstances([]synthCoreInstance{
		{moduleIdx: 0},
		{inline: []synthInline{{name: "g", sort: 0x03, idx: 0}}},
	})
	b.coreAliases([]synthAlias{{coreSort: 0x03, inst: 0, name: "nope"}})
	raw, comp := decodeSynth(t, b)
	_, err := runSynth(t, raw, comp)
	requireErrContains(t, err, `has no exported global "nope"`)
}

func TestSynthGraph_InlineExportGlobalUninstantiatedSource(t *testing.T) {
	b := newCompBuilder()
	b.coreModule(providerModule(0, 7, 42))
	b.coreInstances([]synthCoreInstance{
		{inline: []synthInline{{name: "g", sort: 0x03, idx: 0}}}, // 0: before its source
		{moduleIdx: 0}, // 1
	})
	b.coreAliases([]synthAlias{{coreSort: 0x03, inst: 1, name: "const-g"}})
	raw, comp := decodeSynth(t, b)
	_, err := runSynth(t, raw, comp)
	requireErrContains(t, err, "core global 0 targets core instance 1, which was not instantiated")
}

// ---------------------------------------------------------------------------
// 4. several exported component instances, index space shifted by each export
// ---------------------------------------------------------------------------

// Section order is (instance)(export)(instance)(export) -- what wit-component
// emits for a component with two exported interfaces, and what a
// componentize-py CPython component has. Per Binary.md, "all exports (of all
// sorts) introduce a new index that aliases the exported definition", so the
// second definition lands at instance index 2, not 1: the old
// `ExternIndex - numImportedInstances` arithmetic resolved the second export
// out of range.
func TestSynthGraph_TwoExportedInstancesWithInterleavedAliases(t *testing.T) {
	b := newCompBuilder()
	b.coreModule(providerModule(55, 0, 0))
	b.coreInstances([]synthCoreInstance{{moduleIdx: 0}})
	b.coreAliases([]synthAlias{{coreSort: 0x00, inst: 0, name: "id"}})
	b.funcTypeU32Result()
	b.canonLift(0, 0)                                      // component func 0
	b.inlineExportInstance(map[string]uint32{"first": 0})  // instance def 0 -> instance index 0
	b.exportInstance("iface-a", 0)                         // introduces instance index 1
	b.inlineExportInstance(map[string]uint32{"second": 0}) // instance def 1 -> instance index 2
	b.exportInstance("iface-b", 2)

	raw, comp := decodeSynth(t, b)

	// The decoded index space must show the export-introduced alias between
	// the two definitions.
	wantKinds := []binary.ComponentInstanceSpaceEntryKind{
		binary.ComponentInstanceFromDefinition,
		binary.ComponentInstanceFromExport,
		binary.ComponentInstanceFromDefinition,
		binary.ComponentInstanceFromExport,
	}
	if len(comp.ComponentInstanceSpace) != len(wantKinds) {
		t.Fatalf("instance space: got %+v, want %d entries", comp.ComponentInstanceSpace, len(wantKinds))
	}
	for i, want := range wantKinds {
		if got := comp.ComponentInstanceSpace[i].Kind; got != want {
			t.Errorf("instance space[%d] kind: got %d, want %d", i, got, want)
		}
	}
	if def, ok := comp.ResolveComponentInstance(2); !ok || def != 1 {
		t.Fatalf("instance index 2: got (%d, %v), want (1, true)", def, ok)
	}
	// An export index resolves THROUGH the alias to the definition it names.
	if def, ok := comp.ResolveComponentInstance(1); !ok || def != 0 {
		t.Fatalf("instance index 1 (the export alias): got (%d, %v), want (0, true)", def, ok)
	}

	in, err := runSynth(t, raw, comp)
	if err != nil {
		t.Fatalf("instantiate: %v", err)
	}
	defer in.Close(context.Background())

	ctx := context.Background()
	for _, tc := range []struct{ iface, member string }{
		{"iface-a", "first"},
		{"iface-b", "second"},
	} {
		res, err := in.CallExport(ctx, tc.iface, tc.member)
		if err != nil {
			t.Fatalf("call %s#%s: %v", tc.iface, tc.member, err)
		}
		if len(res) != 1 || res[0] != uint32(55) {
			t.Errorf("call %s#%s: got %+v, want 55", tc.iface, tc.member, res)
		}
	}
}

func TestSynthGraph_ExportedInstanceIndexOutOfRange(t *testing.T) {
	b := newCompBuilder()
	b.coreModule(providerModule(55, 0, 0))
	b.coreInstances([]synthCoreInstance{{moduleIdx: 0}})
	b.coreAliases([]synthAlias{{coreSort: 0x00, inst: 0, name: "id"}})
	b.funcTypeU32Result()
	b.canonLift(0, 0)
	b.inlineExportInstance(map[string]uint32{"first": 0})
	b.exportInstance("iface-a", 7) // nothing at instance index 7
	raw, comp := decodeSynth(t, b)
	_, err := runSynth(t, raw, comp)
	requireErrContains(t, err, "out of range of 0 imported + 1 locally-instantiated instance(s)")
}

// componentInstanceDef must keep working for a hand-built Component, which has
// no decoded index space at all -- the pre-existing flat model.
func TestComponentInstanceDef_HandBuiltFallback(t *testing.T) {
	comp := &binary.Component{
		Imports:   []binary.Import{{Name: "i", ExternType: 0x05}},
		Instances: []binary.Instance{{Kind: 0x01}, {Kind: 0x01}},
	}
	if got, ok := componentInstanceDef(comp, 1); !ok || got != 0 {
		t.Errorf("index 1: got (%d, %v), want (0, true)", got, ok)
	}
	if got, ok := componentInstanceDef(comp, 2); !ok || got != 1 {
		t.Errorf("index 2: got (%d, %v), want (1, true)", got, ok)
	}
	if _, ok := componentInstanceDef(comp, 0); ok {
		t.Error("index 0 names an imported instance; want ok=false")
	}
	if _, ok := componentInstanceDef(comp, 3); ok {
		t.Error("index 3 is out of range; want ok=false")
	}
	if got, want := componentInstanceSpaceIndices(comp), []int{1, 2}; got[0] != want[0] || got[1] != want[1] {
		t.Errorf("space indices: got %v, want %v", got, want)
	}
}
