package wasi_otel

import "github.com/samyfodil/wazy/component"

// The WIT type graph, interned into a component.TypeTable.
//
// wazy does not read the signatures out of a component's imported instance
// type, so a host declares what it implements (see component.WithImportCustom).
// These builders are that declaration, and they are the reason the guest's
// lowered call and this package's lifting agree on a layout.
//
// One table serves every function of an interface: a TypeRef is an index into
// the table it came from, so interning the shared records once and pointing
// several signatures at them is both correct and cheaper than a table each.

// Interface names, as a guest imports them.
//
// Matching ignores the "@x.y.z" suffix, so these serve any version of the
// proposal whose ABI is unchanged -- including its release candidates.
const (
	TracingInterface = "wasi:otel/tracing@0.2.0-rc.2"
	LogsInterface    = "wasi:otel/logs@0.2.0-rc.2"
	MetricsInterface = "wasi:otel/metrics@0.2.0-rc.2"
)

// commonTypes are the records shared across the three interfaces, interned
// into one table.
type commonTypes struct {
	tbl *component.TypeTable

	datetime component.TypeRef
	keyValue component.TypeRef
	// keyValues is list<key-value>, which appears in almost every record.
	keyValues component.TypeRef
	resource  component.TypeRef
	scope     component.TypeRef
}

// newCommonTypes interns the shared records into a fresh table.
func newCommonTypes() *commonTypes {
	t := &commonTypes{tbl: component.NewTypeTable()}

	// wasi:clocks/wall-clock's datetime, which the otel WIT uses rather than
	// defining a time type of its own.
	t.datetime = t.tbl.Record(
		"seconds", component.Prim("u64"),
		"nanoseconds", component.Prim("u32"),
	)

	t.keyValue = t.tbl.Record(
		"key", component.Prim("string"),
		"value", component.Prim("string"),
	)
	t.keyValues = t.tbl.List(t.keyValue)

	t.resource = t.tbl.Record(
		"attributes", t.keyValues,
		"schema-url", t.tbl.Option(component.Prim("string")),
	)

	t.scope = t.tbl.Record(
		"name", component.Prim("string"),
		"version", t.tbl.Option(component.Prim("string")),
		"schema-url", t.tbl.Option(component.Prim("string")),
		"attributes", t.keyValues,
	)

	return t
}

// str is component.Prim("string"), spelled once.
func str() component.TypeRef { return component.Prim("string") }
