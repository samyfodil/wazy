// Package wit declares the WASI 0.2 WIT types that more than one host
// implementation needs.
//
// A WIT type belongs to the interface that defines it, not to whichever host
// package happened to need it first. `datetime` is wasi:clocks/wall-clock's,
// and interfaces well outside wasi:clocks reuse it -- wasi:filesystem's
// descriptor-stat, and wasi:otel's spans, logs and metrics -- so each would
// otherwise spell out its own copy of one wire format.
//
// This package declares types and nothing else: it depends only on
// [github.com/samyfodil/wazy/component], never on a host implementation, so a
// package implementing an unrelated WIT interface can take a type from here
// without depending on WASI itself.
//
// A declaration is a [component.RecordDesc] (or another TypeDesc), plus a
// convenience that interns it into a [component.TypeTable]. The descriptor
// form is what lets a caller intern it into a table of its own instead.
package wit

import "github.com/samyfodil/wazy/component"

// DatetimeDesc returns wasi:clocks/wall-clock's `datetime`:
//
//	record datetime { seconds: u64, nanoseconds: u32 }
//
// A value of it is a record value of the two fields in order:
// []component.Value{uint64(seconds), uint32(nanoseconds)}.
func DatetimeDesc() component.RecordDesc {
	return component.RecordDesc{Fields: []component.RecordField{
		{Name: "seconds", Type: component.TypeRef{Primitive: "u64"}},
		{Name: "nanoseconds", Type: component.TypeRef{Primitive: "u32"}},
	}}
}

// DatetimeType interns [DatetimeDesc] into tbl and returns its TypeRef.
func DatetimeType(tbl *component.TypeTable) component.TypeRef {
	return tbl.Add(DatetimeDesc())
}
