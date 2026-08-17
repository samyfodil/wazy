package wasi_otel

import (
	"context"

	"github.com/samyfodil/wazy/component"
)

// LogEmitter receives the logs a guest emits.
//
// As with [Tracer], the WIT gives `on-emit` no result, so there is nowhere to
// report a failure: a sink that cannot record a log must absorb the failure
// rather than trap the guest.
type LogEmitter interface {
	OnEmit(ctx context.Context, record LogRecord)
}

// logRecordType interns the `log-record` record, whose fields are optional
// almost throughout.
func logRecordType(c *commonTypes) component.TypeRef {
	tbl := c.tbl
	optStr := tbl.Option(str())

	return tbl.Record(
		"timestamp", tbl.Option(c.datetime),
		"observed-timestamp", tbl.Option(c.datetime),
		"severity-text", optStr,
		"severity-number", tbl.Option(component.Prim("u8")),
		"body", optStr,
		"attributes", tbl.Option(c.keyValues),
		"event-name", optStr,
		"resource", tbl.Option(c.resource),
		"instrumentation-scope", tbl.Option(c.scope),
		"trace-id", optStr,
		"span-id", optStr,
		// trace-flags is the same one-member flags set tracing declares. It is
		// interned again here rather than shared, because a flags type is
		// structural: two identical declarations are the same type.
		"trace-flags", tbl.Option(tbl.Flags("sampled")),
	)
}

// LogsOptions returns the component options implementing wasi:otel/logs on
// top of e.
func LogsOptions(e LogEmitter) []component.Option {
	c := newCommonTypes()
	recordRef := logRecordType(c)
	resolve := c.tbl.Resolver()

	onEmitFD := c.tbl.Func([]component.TypeRef{recordRef}, component.TypeRef{})
	onEmit := func(ctx context.Context, args []component.Value) ([]component.Value, error) {
		if len(args) != 1 {
			return nil, errArgs("on-emit", 1, len(args))
		}
		l := &lifter{}
		rec := liftLogRecord(l, args[0])
		if l.err != nil {
			return nil, l.err
		}
		e.OnEmit(ctx, rec)
		return nil, nil
	}

	return []component.Option{
		component.WithImportCustom(LogsInterface, "on-emit", onEmit, onEmitFD, resolve),
	}
}

// liftLogRecord lifts a `log-record`.
func liftLogRecord(l *lifter, v component.Value) LogRecord {
	const what = "log-record"
	f := l.fields(v, 12, what)

	rec := LogRecord{
		Timestamp:         l.optDatetime(f[0], what+".timestamp"),
		ObservedTimestamp: l.optDatetime(f[1], what+".observed-timestamp"),
		SeverityText:      l.optStr(f[2], what+".severity-text"),
		Body:              l.optStr(f[4], what+".body"),
		EventName:         l.optStr(f[6], what+".event-name"),
		TraceID:           l.optStr(f[9], what+".trace-id"),
		SpanID:            l.optStr(f[10], what+".span-id"),
	}

	// severity-number is an option<u8>, which lifts through uint32.
	if inner, ok := l.some(f[3]); ok {
		n := uint8(l.u32(inner, what+".severity-number"))
		rec.SeverityNumber = &n
	}

	// attributes is an option<list<key-value>>. A nil slice is `none`; an
	// empty list stays non-nil and so remains distinguishable.
	if inner, ok := l.some(f[5]); ok {
		attrs := l.keyValues(inner, what+".attributes")
		if attrs == nil {
			attrs = []KeyValue{}
		}
		rec.Attributes = attrs
	}

	if inner, ok := l.some(f[7]); ok {
		r := l.resource(inner, what+".resource")
		rec.Resource = &r
	}

	if inner, ok := l.some(f[8]); ok {
		s := l.scope(inner, what+".instrumentation-scope")
		rec.InstrumentationScope = &s
	}

	if inner, ok := l.some(f[11]); ok {
		tf := TraceFlags(l.u32(inner, what+".trace-flags"))
		rec.TraceFlags = &tf
	}

	return rec
}
