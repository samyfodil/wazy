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

	// Start is the optional start section that specifies startup behavior (section 9).
	Start *Start

	// RawSections tracks sections we parse the header for but skip the body.
	// Used for sections we don't fully decode yet (e.g., core-type, component decls).
	RawSections []RawSection
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
	Name       string
	InstanceIdx uint32
}

// CoreInlineExport represents an inlined export in a core instance.
type CoreInlineExport struct {
	Name       string
	Sort       byte // core:sort: 0x00 func, 0x01 table, 0x02 memory, 0x03 global, 0x04 tag, 0x10 type, 0x11 module, 0x12 instance
	CoreSortIdx uint32
}

// CoreInstance represents a core module instantiation (section 2).
type CoreInstance struct {
	Kind       byte // 0x00 = instantiate, 0x01 = inline exports
	ModuleIdx  uint32 // used if Kind == 0x00
	Args       []CoreInstantiateArg
	Exports    []CoreInlineExport
}

// InstantiateArg represents an argument to instantiate a component.
type InstantiateArg struct {
	Name  string
	Sort  byte
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
	Kind       byte // 0x00 = instantiate, 0x01 = inline exports
	ComponentIdx uint32 // used if Kind == 0x00
	Args       []InstantiateArg
	Exports    []InlineExport
}

// CanonOpt represents a canonical option.
type CanonOpt struct {
	Kind byte // 0x00-0x07 (and potentially more)
	Idx  uint32 // for options that carry an index
}

// Canon represents a canonical lift/lower binding (section 8).
type Canon struct {
	Kind        byte // 0x00 = lift, 0x01 = lower, 0x02/0x03/0x04 = resource.*
	CoreFuncIdx uint32 // used for lift (0x00)
	FuncIdx     uint32 // used for lower (0x01) and for result indices in Start
	Opts        []CanonOpt
	TypeIdx     uint32 // used for lift
}

// AliasDef represents an alias binding (section 6).
type AliasDef struct {
	Sort          byte // sort of the aliased item: 0x00 = core, 0x01 = func, 0x02 = value, 0x03 = type, 0x04 = component, 0x05 = instance
	TargetKind    byte // 0x00 = export, 0x01 = core export, 0x02 = outer
	InstanceIdx   uint32 // for export/core export targets
	Name          string // for export/core export targets
	OuterCount    uint32 // for outer targets
	OuterIndex    uint32 // for outer targets

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
	FuncIdx    uint32
	Args       []uint32
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
			case 0x00:
				kindStr = fmt.Sprintf("lift core func %d type %d", cn.CoreFuncIdx, cn.TypeIdx)
			case 0x01:
				kindStr = fmt.Sprintf("lower func %d", cn.FuncIdx)
			case 0x02:
				kindStr = fmt.Sprintf("resource.new type %d", cn.TypeIdx)
			case 0x03:
				kindStr = fmt.Sprintf("resource.drop type %d", cn.TypeIdx)
			case 0x04:
				kindStr = fmt.Sprintf("resource.rep type %d", cn.TypeIdx)
			default:
				kindStr = fmt.Sprintf("kind %#x", cn.Kind)
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
