package wit

import "fmt"

// GateItem is a single feature-gate attribute attached to a WIT item:
// "@since(version = X)", "@unstable(feature = X)", or "@deprecated(version = X)".
type GateItem struct {
	Kind    string // "since", "unstable", or "deprecated"
	Version string // semver value, set for "since" and "deprecated"
	Feature string // feature name, set for "unstable"
}

// Gate is zero or more feature-gate attributes stacked immediately before an
// item (e.g. "@since(version = 0.2.0) @deprecated(version = 0.2.2)").
type Gate []GateItem

// Package represents a WIT package definition.
type Package struct {
	// Name is the fully-qualified package name (e.g., "wasi:io@0.2.0").
	Name string

	// Items are the top-level declarations (interfaces, worlds, type definitions, use statements).
	Items []PackageItem
}

// PackageItem is a marker interface for top-level package items.
type PackageItem interface {
	packageItem()
}

// Interface represents a WIT interface definition.
type Interface struct {
	Name       string
	Items      []InterfaceItem
	Gate       Gate
	ExternalID string
}

func (*Interface) packageItem() {}

// InterfaceItem is a marker interface for items within an interface.
type InterfaceItem interface {
	interfaceItem()
}

// InterfaceFunc represents a named function item directly inside an
// interface, e.g. "read: func(len: u64) -> result<list<u8>, stream-error>;".
type InterfaceFunc struct {
	Name       string
	Func       Func
	Gate       Gate
	ExternalID string
}

func (*InterfaceFunc) interfaceItem() {}

// World represents a WIT world definition.
type World struct {
	Name       string
	Items      []WorldItem
	Gate       Gate
	ExternalID string
}

func (*World) packageItem() {}

// WorldItem is a marker interface for items within a world.
type WorldItem interface {
	worldItem()
}

// TypeDef represents a type definition.
type TypeDef struct {
	Name        string
	Type        TypeDefBody
	Docs        string
	Gate        Gate
	ExternalID  string
	Unsupported string // if unsupported construct is detected, name it here
}

func (*TypeDef) packageItem()   {}
func (*TypeDef) interfaceItem() {}
func (*TypeDef) worldItem()     {}

// TypeDefBody represents the body of a type definition.
type TypeDefBody interface {
	typeDefBody()
}

// TypeAlias represents a type alias (e.g., "type foo = u32;").
type TypeAlias struct {
	Target TypeRef
}

func (*TypeAlias) typeDefBody() {}

// Record represents a record type.
type Record struct {
	Fields []RecordField
}

func (*Record) typeDefBody() {}

// RecordField represents a field in a record.
type RecordField struct {
	Name string
	Type TypeRef
}

// Variant represents a variant type (discriminated union).
type Variant struct {
	Cases []VariantCase
}

func (*Variant) typeDefBody() {}

// VariantCase represents a case in a variant.
type VariantCase struct {
	Name string
	Type *TypeRef // nil if no associated data
}

// Enum represents an enum type.
type Enum struct {
	Cases []string
}

func (*Enum) typeDefBody() {}

// Flags represents a flags (bitset) type.
type Flags struct {
	Flags []string
}

func (*Flags) typeDefBody() {}

// Resource represents a resource type.
type Resource struct {
	Methods []ResourceMethod
}

func (*Resource) typeDefBody() {}

// ResourceMethod represents a method, static function, or constructor on a
// resource: "name: func(...)", "name: static func(...)", or
// "constructor(...)".
type ResourceMethod struct {
	Name          string
	IsStatic      bool
	IsConstructor bool
	Func          Func
	Gate          Gate
	ExternalID    string
}

// Func represents a function definition or type.
type Func struct {
	Params []FuncParam
	Result *TypeRef // nil if no return type, or points to the result type
	Async  bool
}

// FuncParam represents a function parameter.
type FuncParam struct {
	Name string
	Type TypeRef
}

// Use represents a use statement.
type Use struct {
	Path       string            // e.g., "wasi:io/streams.{InputStream, OutputStream}"
	Names      map[string]string // local alias -> imported name
	Gate       Gate
	ExternalID string
}

func (*Use) packageItem()   {}
func (*Use) interfaceItem() {}
func (*Use) worldItem()     {}

// Include represents an "include world;" or
// "include world with { a as a1, b as b1 };" statement.
type Include struct {
	Path       string
	Renames    map[string]string // original name -> alias
	Gate       Gate
	ExternalID string
}

func (*Include) worldItem() {}

// Import represents an import statement.
type Import struct {
	Name       string
	Type       ImportType
	Gate       Gate
	ExternalID string
}

func (*Import) worldItem() {}

// ImportType is a marker for import kinds.
type ImportType interface {
	importType()
}

// ImportFunc represents an imported function.
type ImportFunc struct {
	Func Func
}

func (*ImportFunc) importType() {}

// ImportInterface represents an imported interface.
type ImportInterface struct {
	Items []InterfaceItem
}

func (*ImportInterface) importType() {}

// Export represents an export statement.
type Export struct {
	Name       string
	Type       ExportType
	Gate       Gate
	ExternalID string
}

func (*Export) worldItem() {}

// ExportType is a marker for export kinds.
type ExportType interface {
	exportType()
}

// ExportFunc represents an exported function.
type ExportFunc struct {
	Func Func
}

func (*ExportFunc) exportType() {}

// ExportInterface represents an exported interface.
type ExportInterface struct {
	Items []InterfaceItem
}

func (*ExportInterface) exportType() {}

// TypeRef represents a type reference (including primitive types and compound types).
type TypeRef struct {
	Kind        string     // "u32", "string", "list", "option", "result", "record", etc.
	Name        string     // for named types (user-defined or resource types)
	Inner       *TypeRef   // for list<T>, option<T>, own<T>, borrow<T>
	Inner2      *TypeRef   // for result<T,E>, map<K,V>, tuple element
	Tuple       []*TypeRef // for tuple<T1, T2, ...>
	Unsupported string     // if an unsupported construct is detected, name it here
}

// String returns a human-readable representation of the type.
func (t *TypeRef) String() string {
	if t == nil {
		return "nil"
	}
	if t.Unsupported != "" {
		return fmt.Sprintf("UNSUPPORTED(%s)", t.Unsupported)
	}
	switch t.Kind {
	case "u8", "u16", "u32", "u64", "s8", "s16", "s32", "s64", "f32", "f64", "bool", "char", "string":
		return t.Kind
	case "list":
		if t.Inner != nil {
			return fmt.Sprintf("list<%s>", t.Inner.String())
		}
		return "list"
	case "option":
		if t.Inner != nil {
			return fmt.Sprintf("option<%s>", t.Inner.String())
		}
		return "option"
	case "result":
		if t.Inner != nil && t.Inner2 != nil {
			return fmt.Sprintf("result<%s,%s>", t.Inner.String(), t.Inner2.String())
		} else if t.Inner != nil {
			return fmt.Sprintf("result<%s>", t.Inner.String())
		}
		return "result"
	case "tuple":
		var parts []string
		for _, elem := range t.Tuple {
			parts = append(parts, elem.String())
		}
		return fmt.Sprintf("tuple<%v>", parts)
	case "own":
		if t.Inner != nil {
			return fmt.Sprintf("own<%s>", t.Inner.String())
		}
		return fmt.Sprintf("own<%s>", t.Name)
	case "borrow":
		if t.Inner != nil {
			return fmt.Sprintf("borrow<%s>", t.Inner.String())
		}
		return fmt.Sprintf("borrow<%s>", t.Name)
	case "map":
		if t.Inner != nil && t.Inner2 != nil {
			return fmt.Sprintf("map<%s,%s>", t.Inner.String(), t.Inner2.String())
		}
		return "map"
	case "future":
		if t.Inner != nil {
			return fmt.Sprintf("future<%s>", t.Inner.String())
		}
		return "future"
	case "stream":
		if t.Inner != nil {
			return fmt.Sprintf("stream<%s>", t.Inner.String())
		}
		return "stream"
	default:
		// Named type
		return t.Name
	}
}
