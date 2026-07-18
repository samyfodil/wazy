package binary

import (
	"fmt"
	"io"
)

// Component represents a parsed WebAssembly Component Model container.
type Component struct {
	// Types contains the type definitions from the type section (section 7)
	// only, in that section's declaration order. This is NOT the full
	// component type index space that canon TypeIdx and export/instance type
	// references index into -- that space also includes type-sort aliases
	// and imported types, interleaved with Types in overall declaration
	// order. See TypeSpace and ResolveType.
	Types []Type

	// TypeSpace is the component's full type index space: one entry per
	// type-index-producing definition (type-section deftype, type-sort
	// alias, or imported type), in overall declaration order across
	// sections. Use ResolveType to resolve a type index through it rather
	// than indexing Types or TypeSpace directly. See typespace.go.
	TypeSpace []TypeSpaceEntry

	// Imports contains the import bindings.
	Imports []Import

	// Exports contains the export bindings.
	Exports []Export

	// ComponentFuncSpace is the component's func index space, one entry per
	// func-producing definition (func import / func alias / canon lift / func
	// export) in overall declaration order across sections. See
	// componentfuncspace.go.
	ComponentFuncSpace []ComponentFuncSpaceEntry

	// CoreModules are embedded core wasm modules (section 1).
	CoreModules []CoreModule

	// CoreInstances instantiate core modules with arguments (section 2).
	CoreInstances []CoreInstance

	// Instances instantiate components with arguments (section 5).
	Instances []Instance

	// NestedComponents are fully embedded sub-components (section 4), decoded
	// recursively -- per the Binary.md grammar, section_4(<component>) is a
	// complete component binary (its own magic/version/layer preamble plus
	// its own sections), not a bare section list. wit-component emits these
	// for a world that exports an interface: it packages the lifted core
	// funcs into a nested-component "shim" that re-exports them, and the
	// top-level export names an Instance (section 5) that instantiates this
	// nested component -- see internal/component/instance, which resolves
	// that shim to call the funcs it re-exports.
	//
	// Some real-world nested components (e.g. the wasip2 CLI adapter emitted
	// alongside a `wasi:cli/command` world) are far more complex than the
	// re-export shim -- they embed their own core module and full
	// instantiation graph. Those still decode fine here (this is a purely
	// structural, recursive decode with no semantic assumptions), but
	// internal/component/instance fails loud rather than trying to
	// instantiate them.
	NestedComponents []*Component

	// Aliases bring names into scope (section 6).
	Aliases []AliasDef

	// Canons describe canonical lift/lower bindings (section 8).
	Canons []Canon

	// CoreFuncSpace is the component's full core func index space, in
	// declaration order across the (possibly interleaved) alias and canon
	// sections -- see corefuncspace.go. Empty for a Component not produced by
	// Decode (e.g. a hand-built binary.Component in a test), matching
	// TypeSpace's convention; callers needing the core func index space fall
	// back to treating Aliases/Canons as already correctly ordered in that
	// case.
	CoreFuncSpace []CoreFuncSpaceEntry

	// Start is the optional start section that specifies startup behavior (section 9).
	Start *Start

	// RawSections tracks sections we parse the header for but skip the body.
	// Used for sections we don't fully decode yet (e.g., core-type, component decls).
	RawSections []RawSection

	// Bytes is the complete binary this component was decoded from (the whole
	// buffer for a top-level component; the section-4 sub-slice for a nested
	// one). CoreModule.Offset/Size index into it, so recursively instantiating
	// a NestedComponents entry needs its own Bytes, not the parent's.
	Bytes []byte

	// Decoded is set true by Decode. It distinguishes a component that went
	// through the binary decoder (whose CoreFuncSpace/ComponentFuncSpace/
	// TypeSpace are authoritative -- an empty CoreFuncSpace genuinely means no
	// core-func-producing aliases/canons) from a hand-built Component value
	// (the common shape in tests, which never populates those index spaces).
	// The graph engine uses it to tell "legitimately no core funcs" from "index
	// spaces were never built"; see graph.go.
	Decoded bool
}

// Type represents a value type in the component type section.
// For now, we store the raw index and a textual kind for easier debugging.
type Type struct {
	// Index is the type index in the type section.
	Index uint32

	// Kind is a human-readable string representation ("func", "record", "variant", etc.).
	// For now, we stub most kinds.
	Kind string

	// Descriptor is the full semantic representation of the type (enum, record, func, etc).
	// It is set during parsing and contains the complete structure for ABI and other uses.
	Descriptor TypeDesc

	// Raw holds the raw bytes of this type definition (for round-trip verification).
	Raw []byte
}

// Import represents a component import binding.
type Import struct {
	// Name is the import name (e.g., "wasi:io/streams#input-stream").
	Name string

	// ExternType distinguishes the import kind (e.g., component, instance, func, value, type, module).
	ExternType byte

	// ExternIndex is the index into the appropriate namespace (e.g., component index, function index).
	ExternIndex uint32

	// TypeEqIndex, valid when ExternType == 0x03 (type) and TypeEqBound is
	// true, is the component type index this type import is declared equal to
	// (an `import "x" (type (eq N))` bound, what wit-component/cargo-component
	// emit for a world's exported types). Such an import resolves through to
	// type N -- see typespace.go's resolveTypeDepth -- rather than being
	// opaque. A `sub` (resource) type bound leaves TypeEqBound false.
	TypeEqIndex uint32
	TypeEqBound bool
}

// Export represents a component export binding.
type Export struct {
	// Name is the export name.
	Name string

	// ExternType distinguishes the export kind.
	ExternType byte

	// ExternIndex is the index into the appropriate namespace.
	ExternIndex uint32
}

// CoreModule represents an embedded core wasm module (section 1).
// The binary is stored as an offset and size; it is not parsed here,
// as wazy's core decoder handles that separately.
type CoreModule struct {
	Offset int // byte offset in the component binary
	Size   int // byte length of the core module
}

// CoreInstantiateArg represents an argument to instantiate a core module.
type CoreInstantiateArg struct {
	Name        string
	InstanceIdx uint32
}

// CoreInlineExport represents an inlined export in a core instance.
type CoreInlineExport struct {
	Name        string
	Sort        byte // core:sort: 0x00 func, 0x01 table, 0x02 memory, 0x03 global, 0x04 tag, 0x10 type, 0x11 module, 0x12 instance
	CoreSortIdx uint32
}

// CoreInstance represents a core module instantiation (section 2).
type CoreInstance struct {
	Kind      byte   // 0x00 = instantiate, 0x01 = inline exports
	ModuleIdx uint32 // used if Kind == 0x00
	Args      []CoreInstantiateArg
	Exports   []CoreInlineExport
}

// InstantiateArg represents an argument to instantiate a component.
type InstantiateArg struct {
	Name    string
	Sort    byte
	SortIdx uint32
}

// InlineExport represents an inlined export in an instance.
type InlineExport struct {
	Name    string
	Sort    byte
	SortIdx uint32
}

// Instance represents a component instance (section 5).
type Instance struct {
	Kind         byte   // 0x00 = instantiate, 0x01 = inline exports
	ComponentIdx uint32 // used if Kind == 0x00
	Args         []InstantiateArg
	Exports      []InlineExport
}

// CanonOpt represents a canonical option.
type CanonOpt struct {
	Kind byte   // 0x00-0x07 (and potentially more)
	Idx  uint32 // for options that carry an index
}

// CanonKind names the byte values Canon.Kind takes -- the first byte of each
// entry in the canonical-function section (id 0x08). 0x00-0x04 were already
// implemented; 0x05 and up are the Phase 0 async additions. Verified against
// wasm-tools 1.253 (`wasm-tools dump`) on internal/component/binary/testdata/
// async/*.wasm -- see decodeCanonSection and canonKindName.
//
// 0x07 (resource.drop async) was added in Phase 3 -- see
// docs/component-model-async-phase3-design.md §4.4; its payload (a single
// typeidx) is byte-identical to 0x03's, verified against fresh wasm-tools
// output the same way as every other kind in this table.
//
// CanonKindThreadYield/Index/NewIndirect/Suspend/YieldThenResume are the five
// thread.* builtins actually exercised by the vendored async .wast suites
// (cancellable, sync-barges-in, trap-if-sync-and-waitable-set, trap-if-
// block-and-sync); their opcodes and payload grammars are taken from the
// component-model spec's design/mvp/Binary.md canon-definitions EBNF (the
// 🧵/🔀 threads-proposal entries), cross-checked against `wasm-tools dump`
// on those four fixtures. The REMAINING thread.* opcodes (0x28 resume-later,
// 0x2a/0x2c/0x2d suspend/yield-then-promote, 0x40-0x42 spawn-ref/spawn-
// indirect/available-parallelism) are deliberately NOT added here: no
// vendored suite decodes one, so their byte layouts are unverified against
// a real fixture -- add them (and their decode case) only once a suite that
// actually needs them shows up. Decoding these five is NOT the same as
// implementing thread.* execution (no runtime support exists for any of
// them -- see stream_builtins.go/async_builtins.go's absence of a
// thread.yield/thread.new-indirect host func); every suite that reaches one
// at CALL time (not just decode) still traps or hangs on its own, real gap.
const (
	CanonKindLift              byte = 0x00
	CanonKindLower             byte = 0x01
	CanonKindResourceNew       byte = 0x02
	CanonKindResourceDrop      byte = 0x03
	CanonKindResourceRep       byte = 0x04
	CanonKindResourceDropAsync byte = 0x07

	CanonKindTaskCancel               byte = 0x05
	CanonKindSubtaskCancel            byte = 0x06
	CanonKindTaskReturn               byte = 0x09
	CanonKindContextGet               byte = 0x0a
	CanonKindContextSet               byte = 0x0b
	CanonKindThreadYield              byte = 0x0c
	CanonKindSubtaskDrop              byte = 0x0d
	CanonKindStreamNew                byte = 0x0e
	CanonKindStreamRead               byte = 0x0f
	CanonKindStreamWrite              byte = 0x10
	CanonKindStreamCancelRead         byte = 0x11
	CanonKindStreamCancelWrite        byte = 0x12
	CanonKindStreamDropReadable       byte = 0x13
	CanonKindStreamDropWritable       byte = 0x14
	CanonKindFutureNew                byte = 0x15
	CanonKindFutureRead               byte = 0x16
	CanonKindFutureWrite              byte = 0x17
	CanonKindFutureCancelRead         byte = 0x18
	CanonKindFutureCancelWrite        byte = 0x19
	CanonKindFutureDropReadable       byte = 0x1a
	CanonKindFutureDropWritable       byte = 0x1b
	CanonKindErrorContextNew          byte = 0x1c
	CanonKindErrorContextDebugMessage byte = 0x1d
	CanonKindErrorContextDrop         byte = 0x1e
	CanonKindWaitableSetNew           byte = 0x1f
	CanonKindWaitableSetWait          byte = 0x20
	CanonKindWaitableSetPoll          byte = 0x21
	CanonKindWaitableSetDrop          byte = 0x22
	CanonKindWaitableJoin             byte = 0x23
	CanonKindBackpressureInc          byte = 0x24
	CanonKindBackpressureDec          byte = 0x25
	CanonKindThreadIndex              byte = 0x26
	CanonKindThreadNewIndirect        byte = 0x27
	CanonKindThreadSuspend            byte = 0x29
	CanonKindThreadYieldThenResume    byte = 0x2b
)

// Canon represents a canonical lift/lower binding (section 8).
type Canon struct {
	Kind        byte   // one of the CanonKind* constants
	CoreFuncIdx uint32 // used for lift (0x00)
	FuncIdx     uint32 // used for lower (0x01) and for result indices in Start
	Opts        []CanonOpt
	TypeIdx     uint32 // lift's type index; resource.*'s type index; the
	// stream/future element typeidx for stream.{new,read,write,cancel-read,
	// cancel-write,drop-readable,drop-writable} and future's seven twins.

	// Async payload fields (Phase 0: decode + typing only, no execution).
	// Each is meaningful only for the Kind(s) noted; zero value otherwise.

	// Async is the async_:bool payload of subtask.cancel, stream.cancel-read/
	// -write, and future.cancel-read/-write.
	Async bool

	// Cancellable is waitable-set.wait/poll's cancellable:bool payload, and
	// (decode-only -- see CanonKindThreadYield's doc) thread.yield/thread.
	// suspend/thread.yield-then-resume's identically-shaped cancel:bool.
	Cancellable bool

	// TableIdx is thread.new-indirect's tbl:<core:tableidx> payload
	// (decode-only). TypeIdx doubles as its ft:<typeidx> payload.
	TableIdx uint32

	// MemIdx is waitable-set.wait/poll's memory index. (error-context.new/
	// debug-message also take a memory, but via the existing Opts memory
	// option (0x03), same as lift/lower -- they don't need this field.)
	MemIdx uint32

	// CoreValType is context.get/context.set's `ty` payload: a single CORE
	// valtype byte (e.g. 0x7f = i32), not a component valtype -- same
	// encoding as ResourceDesc.Rep. See coreValtypeName.
	CoreValType byte

	// Slot is context.get/context.set's `slot` payload (a context-storage
	// index, not a type or func index).
	Slot uint32

	// TaskReturnResult is task.return's result list. Verified via
	// `wasm-tools dump`/round-trip probes (not just the fixtures) that this
	// is encoded with the EXACT SAME grammar as a functype's result list
	// (readResultListDesc): tag 0x00 + one valtype ("unnamed", e.g. `(result
	// u32)`), or tag 0x01 + vec(labelvaltype) ("named", empty for a
	// no-result task.return). This resolves the brief's flagged ambiguity:
	// task.return's `(result ...)` clause is NOT a bare option<valtype> --
	// bytes `09 00 79 00` decode as tag=0x00 (unnamed) valtype=0x79 (u32)
	// then an empty opts vec, while a no-result task.return encodes as
	// `09 01 00 00` (tag=0x01 named-list, count=0, then empty opts vec).
	TaskReturnResult FuncResults
}

// AliasDef represents an alias binding (section 6).
type AliasDef struct {
	Sort        byte   // sort of the aliased item: 0x00 = core, 0x01 = func, 0x02 = value, 0x03 = type, 0x04 = component, 0x05 = instance
	TargetKind  byte   // 0x00 = export, 0x01 = core export, 0x02 = outer
	InstanceIdx uint32 // for export/core export targets
	Name        string // for export/core export targets
	OuterCount  uint32 // for outer targets
	OuterIndex  uint32 // for outer targets

	// CoreSort is the core:sort discriminator byte that follows Sort when
	// Sort == 0x00 (i.e. this alias names a core-level item): 0x00 func,
	// 0x01 table, 0x02 memory, 0x03 global, 0x04 tag, 0x10 type, 0x11
	// module, 0x12 instance. It is meaningless (and left at its zero value,
	// 0x00) when Sort != 0x00.
	//
	// A core-export alias (Sort == 0x00, TargetKind == 0x01) needs this to
	// tell a core-func alias apart from a core-memory/table/global alias --
	// see internal/component/instance, which used to disambiguate by
	// probing the instantiated target module's exports (a name that
	// resolves to a Function is a func alias) because this field didn't
	// exist; that probe is now a fallback for AliasDefs built by hand
	// (e.g. in tests) rather than by decodeAliasSection.
	CoreSort byte
}

// Start represents the start section (section 9).
type Start struct {
	FuncIdx     uint32
	Args        []uint32
	ResultCount uint32
}

// RawSection represents a section header we parsed but did not fully decode.
type RawSection struct {
	ID   byte
	Size uint32
}

// Dump writes a human-readable summary of the component to w.
// It prints the type section, import/export graph, and instantiation graph.
func (c *Component) Dump(w io.Writer) error {
	if _, err := io.WriteString(w, "Component Model Binary\n"); err != nil {
		return err
	}
	if _, err := io.WriteString(w, "======================\n\n"); err != nil {
		return err
	}

	// Dump types
	if len(c.Types) > 0 {
		if _, err := io.WriteString(w, "Types:\n"); err != nil {
			return err
		}
		for i, t := range c.Types {
			if _, err := fmt.Fprintf(w, "  [%d] %s\n", i, t.Kind); err != nil {
				return err
			}
		}
		if _, err := io.WriteString(w, "\n"); err != nil {
			return err
		}
	}

	// Dump imports
	if len(c.Imports) > 0 {
		if _, err := io.WriteString(w, "Imports:\n"); err != nil {
			return err
		}
		for _, imp := range c.Imports {
			if _, err := fmt.Fprintf(w, "  %s (%s %d)\n", imp.Name, externTypeName(imp.ExternType), imp.ExternIndex); err != nil {
				return err
			}
		}
		if _, err := io.WriteString(w, "\n"); err != nil {
			return err
		}
	}

	// Dump exports
	if len(c.Exports) > 0 {
		if _, err := io.WriteString(w, "Exports:\n"); err != nil {
			return err
		}
		for _, exp := range c.Exports {
			if _, err := fmt.Fprintf(w, "  %s (%s %d)\n", exp.Name, externTypeName(exp.ExternType), exp.ExternIndex); err != nil {
				return err
			}
		}
		if _, err := io.WriteString(w, "\n"); err != nil {
			return err
		}
	}

	// Dump core modules
	if len(c.CoreModules) > 0 {
		if _, err := io.WriteString(w, "Core Modules:\n"); err != nil {
			return err
		}
		for i, m := range c.CoreModules {
			if _, err := fmt.Fprintf(w, "  [%d] offset=%d size=%d\n", i, m.Offset, m.Size); err != nil {
				return err
			}
		}
		if _, err := io.WriteString(w, "\n"); err != nil {
			return err
		}
	}

	// Dump core instances
	if len(c.CoreInstances) > 0 {
		if _, err := io.WriteString(w, "Core Instances:\n"); err != nil {
			return err
		}
		for i, ci := range c.CoreInstances {
			if ci.Kind == 0x00 {
				if _, err := fmt.Fprintf(w, "  [%d] instantiate module %d\n", i, ci.ModuleIdx); err != nil {
					return err
				}
			} else {
				if _, err := fmt.Fprintf(w, "  [%d] inline exports\n", i); err != nil {
					return err
				}
			}
		}
		if _, err := io.WriteString(w, "\n"); err != nil {
			return err
		}
	}

	// Dump instances
	if len(c.Instances) > 0 {
		if _, err := io.WriteString(w, "Instances:\n"); err != nil {
			return err
		}
		for i, inst := range c.Instances {
			if inst.Kind == 0x00 {
				if _, err := fmt.Fprintf(w, "  [%d] instantiate component %d\n", i, inst.ComponentIdx); err != nil {
					return err
				}
			} else {
				if _, err := fmt.Fprintf(w, "  [%d] inline exports\n", i); err != nil {
					return err
				}
			}
		}
		if _, err := io.WriteString(w, "\n"); err != nil {
			return err
		}
	}

	// Dump aliases
	if len(c.Aliases) > 0 {
		if _, err := io.WriteString(w, "Aliases:\n"); err != nil {
			return err
		}
		for i, a := range c.Aliases {
			targetDesc := ""
			switch a.TargetKind {
			case 0x00:
				targetDesc = fmt.Sprintf("export instance %d name %q", a.InstanceIdx, a.Name)
			case 0x01:
				targetDesc = fmt.Sprintf("core export instance %d name %q", a.InstanceIdx, a.Name)
			case 0x02:
				targetDesc = fmt.Sprintf("outer count=%d index=%d", a.OuterCount, a.OuterIndex)
			}
			sortDesc := fmt.Sprintf("%#x", a.Sort)
			if a.Sort == 0x00 {
				sortDesc = fmt.Sprintf("%#x (core:%#x)", a.Sort, a.CoreSort)
			}
			if _, err := fmt.Fprintf(w, "  [%d] sort=%s %s\n", i, sortDesc, targetDesc); err != nil {
				return err
			}
		}
		if _, err := io.WriteString(w, "\n"); err != nil {
			return err
		}
	}

	// Dump canons
	if len(c.Canons) > 0 {
		if _, err := io.WriteString(w, "Canons:\n"); err != nil {
			return err
		}
		for i, cn := range c.Canons {
			kindStr := ""
			switch cn.Kind {
			case CanonKindLift:
				kindStr = fmt.Sprintf("lift core func %d type %d", cn.CoreFuncIdx, cn.TypeIdx)
			case CanonKindLower:
				kindStr = fmt.Sprintf("lower func %d", cn.FuncIdx)
			case CanonKindResourceNew:
				kindStr = fmt.Sprintf("resource.new type %d", cn.TypeIdx)
			case CanonKindResourceDrop:
				kindStr = fmt.Sprintf("resource.drop type %d", cn.TypeIdx)
			case CanonKindResourceRep:
				kindStr = fmt.Sprintf("resource.rep type %d", cn.TypeIdx)
			default:
				// Async builtins (0x05+): named but with no per-field dump
				// yet -- Phase 0 is decode + typing only, so a name plus the
				// raw kind byte is enough for now.
				kindStr = fmt.Sprintf("%s (kind %#x)", canonKindName(cn.Kind), cn.Kind)
			}
			if _, err := fmt.Fprintf(w, "  [%d] %s\n", i, kindStr); err != nil {
				return err
			}
		}
		if _, err := io.WriteString(w, "\n"); err != nil {
			return err
		}
	}

	// Dump start
	if c.Start != nil {
		if _, err := fmt.Fprintf(w, "Start:\n  func %d args=%v results=%d\n\n", c.Start.FuncIdx, c.Start.Args, c.Start.ResultCount); err != nil {
			return err
		}
	}

	// Dump raw sections (for transparency)
	if len(c.RawSections) > 0 {
		if _, err := io.WriteString(w, "Skipped Sections:\n"); err != nil {
			return err
		}
		for _, rs := range c.RawSections {
			if _, err := fmt.Fprintf(w, "  %s (id=%d, size=%d)\n", sectionIDName(rs.ID), rs.ID, rs.Size); err != nil {
				return err
			}
		}
	}

	return nil
}

func externTypeName(t byte) string {
	switch t {
	case 0x00:
		return "module"
	case 0x01:
		return "func"
	case 0x02:
		return "value"
	case 0x03:
		return "type"
	case 0x04:
		return "component"
	case 0x05:
		return "instance"
	default:
		return "unknown"
	}
}

// canonKindName maps a Canon.Kind byte to its builtin name, for Dump and
// decode error messages. Kinds not in the CanonKind* table (0x08, 0x0c,
// 0x26+) never reach here -- decodeCanonSection fails loud on them
// before a Canon value exists.
func canonKindName(k byte) string {
	switch k {
	case CanonKindLift:
		return "lift"
	case CanonKindLower:
		return "lower"
	case CanonKindResourceNew:
		return "resource.new"
	case CanonKindResourceDrop:
		return "resource.drop"
	case CanonKindResourceRep:
		return "resource.rep"
	case CanonKindResourceDropAsync:
		return "resource.drop (async)"
	case CanonKindTaskCancel:
		return "task.cancel"
	case CanonKindSubtaskCancel:
		return "subtask.cancel"
	case CanonKindTaskReturn:
		return "task.return"
	case CanonKindContextGet:
		return "context.get"
	case CanonKindContextSet:
		return "context.set"
	case CanonKindSubtaskDrop:
		return "subtask.drop"
	case CanonKindStreamNew:
		return "stream.new"
	case CanonKindStreamRead:
		return "stream.read"
	case CanonKindStreamWrite:
		return "stream.write"
	case CanonKindStreamCancelRead:
		return "stream.cancel-read"
	case CanonKindStreamCancelWrite:
		return "stream.cancel-write"
	case CanonKindStreamDropReadable:
		return "stream.drop-readable"
	case CanonKindStreamDropWritable:
		return "stream.drop-writable"
	case CanonKindFutureNew:
		return "future.new"
	case CanonKindFutureRead:
		return "future.read"
	case CanonKindFutureWrite:
		return "future.write"
	case CanonKindFutureCancelRead:
		return "future.cancel-read"
	case CanonKindFutureCancelWrite:
		return "future.cancel-write"
	case CanonKindFutureDropReadable:
		return "future.drop-readable"
	case CanonKindFutureDropWritable:
		return "future.drop-writable"
	case CanonKindErrorContextNew:
		return "error-context.new"
	case CanonKindErrorContextDebugMessage:
		return "error-context.debug-message"
	case CanonKindErrorContextDrop:
		return "error-context.drop"
	case CanonKindWaitableSetNew:
		return "waitable-set.new"
	case CanonKindWaitableSetWait:
		return "waitable-set.wait"
	case CanonKindWaitableSetPoll:
		return "waitable-set.poll"
	case CanonKindWaitableSetDrop:
		return "waitable-set.drop"
	case CanonKindWaitableJoin:
		return "waitable.join"
	case CanonKindBackpressureInc:
		return "backpressure.inc"
	case CanonKindBackpressureDec:
		return "backpressure.dec"
	case CanonKindThreadYield:
		return "thread.yield"
	case CanonKindThreadIndex:
		return "thread.index"
	case CanonKindThreadNewIndirect:
		return "thread.new-indirect"
	case CanonKindThreadSuspend:
		return "thread.suspend"
	case CanonKindThreadYieldThenResume:
		return "thread.yield-then-resume"
	default:
		return fmt.Sprintf("canon kind %#x", k)
	}
}

func sectionIDName(id byte) string {
	switch id {
	case 0:
		return "Custom"
	case 1:
		return "CoreModule"
	case 2:
		return "CoreInstance"
	case 3:
		return "CoreType"
	case 4:
		return "Component"
	case 5:
		return "Instance"
	case 6:
		return "Alias"
	case 7:
		return "Type"
	case 8:
		return "Canonical"
	case 9:
		return "Start"
	case 10:
		return "Import"
	case 11:
		return "Export"
	case 12:
		return "Value"
	default:
		return "Unknown"
	}
}
