package wit

import (
	"fmt"
	"strings"
)

// Pos is where an item is written: a file, and a 1-based line and byte
// column, as wit-parser reports them.
type Pos struct {
	File      string
	Line, Col int
}

func (p Pos) String() string { return fmt.Sprintf("%s:%d:%d", p.File, p.Line, p.Col) }

// GateItem is a single feature-gate attribute attached to a WIT item:
// "@since(version = X)", "@unstable(feature = X)", or "@deprecated(version = X)".
type GateItem struct {
	Kind    string // "since", "unstable", or "deprecated"
	Version string // semver value, set for "since" and "deprecated"
	Feature string // feature name, set for "unstable"
	Pos     Pos
}

// Gate is zero or more feature-gate attributes stacked immediately before an
// item (e.g. "@since(version = 0.2.0) @deprecated(version = 0.2.2)").
type Gate []GateItem

// Attrs are the attributes written before an item: its gate, and the
// position of each "@external-id(...)", in order.
type Attrs struct {
	Gate       Gate
	ExternalID []Pos
}

// Package is one parsed WIT file: an optional "package ns:name@ver;" header,
// the items at its top level, and any nested "package ns:name { ... }"
// blocks, each a package of its own.
type Package struct {
	// Name is the package name ("wasi:io@0.2.0"), or "" without a header.
	Name    string
	NamePos Pos
	// Docs says whether comments precede a file's header, which
	// wit-parser takes as the package's docs.
	Docs bool
	// Items are *Interface, *World and *ToplevelUse.
	Items  []PackageItem
	Nested []*Package
}

// PackageItem is a marker interface for top-level package items.
type PackageItem interface {
	packageItem()
}

// Interface represents a WIT interface definition.
type Interface struct {
	Name  string
	Pos   Pos
	Items []InterfaceItem
	Attrs
}

func (*Interface) packageItem() {}

// InterfaceItem is a marker interface for items within an interface.
type InterfaceItem interface {
	interfaceItem()
}

// InterfaceFunc represents a named function item directly inside an
// interface, e.g. "read: func(len: u64) -> result<list<u8>, stream-error>;".
type InterfaceFunc struct {
	Name string
	Pos  Pos
	Func Func
	Attrs
}

func (*InterfaceFunc) interfaceItem() {}

// World represents a WIT world definition.
type World struct {
	Name  string
	Pos   Pos
	Items []WorldItem
	Attrs
}

func (*World) packageItem() {}

// WorldItem is a marker interface for items within a world.
type WorldItem interface {
	worldItem()
}

// ToplevelUse is "use path;" or "use path as name;" at a file's top level:
// it names an interface or world for the rest of that file.
type ToplevelUse struct {
	Path    string
	PathPos Pos
	As      string // "" for none
	AsPos   Pos
	Pos     Pos // the `use` keyword
	Attrs
}

func (*ToplevelUse) packageItem() {}

// Local is the name a ToplevelUse is known by in its file.
func (u *ToplevelUse) Local() (string, Pos) {
	if u.As != "" {
		return u.As, u.AsPos
	}
	return LastSegment(u.Path), u.PathPos
}

// TypeDef represents a type definition.
type TypeDef struct {
	Name string
	Pos  Pos
	Type TypeDefBody
	Attrs
}

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
	Pos  Pos
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
	Pos  Pos
	Type *TypeRef // nil if no associated data
}

// Enum represents an enum type.
type Enum struct {
	Cases []string
	Pos   []Pos // of each case
}

func (*Enum) typeDefBody() {}

// Flags represents a flags (bitset) type.
type Flags struct {
	Flags []string
	Pos   []Pos // of each flag
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
	Pos           Pos
	IsStatic      bool
	IsConstructor bool
	Func          Func
	Attrs
}

// Accessor says whether a func is a property getter ("name: get() -> T") or
// setter ("name: set(value: T)").
type Accessor int

const (
	NoAccessor Accessor = iota
	Getter
	Setter
)

// Func represents a function definition or type.
type Func struct {
	Params   []FuncParam
	Result   *TypeRef // nil if no return type, or points to the result type
	Async    bool
	Accessor Accessor
	Pos      Pos // the `func`, `get`, `set` or `constructor` keyword
}

// FuncParam represents a function parameter.
type FuncParam struct {
	Name string
	Pos  Pos
	Type TypeRef
}

// Use represents "use path.{a, b as c};" in an interface or world.
type Use struct {
	Path    string // "iface" or "ns:pkg/iface@ver"
	PathPos Pos
	Names   []UseName
	Attrs
}

func (*Use) interfaceItem() {}
func (*Use) worldItem()     {}

// UseName is one name a Use brings in: Name, under As if that is set.
type UseName struct {
	Name  string
	Pos   Pos
	As    string
	AsPos Pos
}

// Local is the name a UseName is known by in the using scope.
func (n UseName) Local() (string, Pos) {
	if n.As != "" {
		return n.As, n.AsPos
	}
	return n.Name, n.Pos
}

// Include represents an "include world;" or
// "include world with { a as a1, b as b1 }" statement.
type Include struct {
	Path    string
	PathPos Pos
	Renames []IncludeName
	Attrs
}

func (*Include) worldItem() {}

// IncludeName is one "a as b" of an include's with-list.
type IncludeName struct {
	Name, As string
	Pos      Pos
}

// Import represents an import statement.
type Import struct {
	Extern
}

func (*Import) worldItem() {}

// Export represents an export statement.
type Export struct {
	Extern
}

func (*Export) worldItem() {}

// Extern is what an import or export names. Name is the func's, inline
// interface's or label's name -- or, for an interface named by path alone,
// the path.
type Extern struct {
	Name string
	Pos  Pos
	Kind ExternKind
	Attrs
}

// ExternKind is the shape of an import or export.
type ExternKind interface {
	externKind()
}

// ExternFunc is "name: func(...)".
type ExternFunc struct{ Func Func }

// ExternInterface is "name: interface { ... }".
type ExternInterface struct{ Items []InterfaceItem }

// ExternPath is an interface named by path alone: "iface;" or
// "ns:pkg/iface@ver;". Its Extern.Name is the path.
type ExternPath struct{}

// ExternNamedPath is "label: path;": an import or export under a name of
// its own that implements the interface at Path.
type ExternNamedPath struct {
	Path    string
	PathPos Pos
}

func (*ExternFunc) externKind()      {}
func (*ExternInterface) externKind() {}
func (*ExternPath) externKind()      {}
func (*ExternNamedPath) externKind() {}

// TypeRef represents a type reference (including primitive types and compound types).
type TypeRef struct {
	Kind   string     // "u32", "string", "list", "option", "result", "named", etc.
	Name   string     // for named types and own/borrow handles
	Inner  *TypeRef   // list<T>, option<T>, result<T, _>, map<K, _>, future<T>, stream<T>
	Inner2 *TypeRef   // result<_, E>, map<_, V>
	Tuple  []*TypeRef // tuple<T1, T2, ...>
	Fixed  bool       // list<T, N>: a fixed-length list
	Length uint32     // list<T, N>: N
	Pos    Pos
}

// String returns the type as WIT writes it.
func (t *TypeRef) String() string {
	switch t.Kind {
	case "named":
		return t.Name
	case "own", "borrow":
		return t.Kind + "<" + t.Name + ">"
	case "tuple":
		parts := make([]string, len(t.Tuple))
		for i, e := range t.Tuple {
			parts[i] = e.String()
		}
		return "tuple<" + strings.Join(parts, ", ") + ">"
	case "list":
		if t.Fixed {
			return fmt.Sprintf("list<%s, %d>", t.Inner, t.Length)
		}
	case "result":
		switch {
		case t.Inner == nil && t.Inner2 == nil:
			return "result"
		case t.Inner2 == nil:
			return "result<" + t.Inner.String() + ">"
		case t.Inner == nil:
			return "result<_, " + t.Inner2.String() + ">"
		}
		return "result<" + t.Inner.String() + ", " + t.Inner2.String() + ">"
	case "map":
		return "map<" + t.Inner.String() + ", " + t.Inner2.String() + ">"
	}
	if t.Inner != nil {
		return t.Kind + "<" + t.Inner.String() + ">"
	}
	return t.Kind
}

// LastSegment is the item name in a use path: "c" in "c" or "a:b/c@1.0.0".
func LastSegment(path string) string {
	name, _, _ := strings.Cut(path[strings.LastIndex(path, "/")+1:], "@")
	return name
}
