package binary

import (
	"fmt"
	"io"
)

// Component represents a parsed WebAssembly Component Model container.
type Component struct {
	// Types contains the type definitions from the type section.
	Types []Type

	// Imports contains the import bindings.
	Imports []Import

	// Exports contains the export bindings.
	Exports []Export

	// RawSections tracks sections we parse the header for but skip the body.
	// Used for sections we don't fully decode yet (e.g., canonical, instance, alias).
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

// RawSection represents a section header we parsed but did not fully decode.
type RawSection struct {
	ID   byte
	Size uint32
}

// Dump writes a human-readable summary of the component to w.
// It prints the type section and the import/export graph.
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
