package wasi_otel

import (
	"context"

	"github.com/samyfodil/wazy/component"
)

// Tracer receives the spans a guest produces.
//
// None of the methods return an error, which follows the WIT: `on-start` and
// `on-end` return nothing, so there is no arm to report a failure through. A
// sink that cannot record a span must not take the guest down with it -- if
// yours can fail, absorb it (log it, count it) rather than trapping.
type Tracer interface {
	// OnStart is called as a span begins, with only its context: the span has
	// no data yet.
	OnStart(ctx context.Context, sc SpanContext)

	// OnEnd is called with the finished span.
	OnEnd(ctx context.Context, span SpanData)

	// CurrentSpanContext returns the context of the most recently entered
	// span. Return the zero SpanContext when no span is active; that is what
	// an all-zero trace id means to a caller.
	CurrentSpanContext(ctx context.Context) SpanContext
}

// tracingTypes is the tracing interface's type graph.
type tracingTypes struct {
	*commonTypes

	spanContext component.TypeRef
	spanData    component.TypeRef
}

func newTracingTypes(c *commonTypes) *tracingTypes {
	t := &tracingTypes{commonTypes: c}
	tbl := c.tbl

	// trace-state is list<tuple<string, string>>.
	traceState := tbl.List(tbl.Tuple(str(), str()))

	// The flags set has one member, so its value is a single bit.
	traceFlags := tbl.Flags("sampled")

	t.spanContext = tbl.Record(
		"trace-id", str(),
		"span-id", str(),
		"trace-flags", traceFlags,
		"is-remote", component.Prim("bool"),
		"trace-state", traceState,
	)

	spanKind := tbl.Enum("client", "server", "producer", "consumer", "internal")

	status := tbl.Variant(
		component.VariantCaseSpec{Name: "unset"},
		component.VariantCaseSpec{Name: "ok"},
		component.VariantCaseSpec{Name: "error", Type: str()},
	)

	event := tbl.Record(
		"name", str(),
		"time", c.datetime,
		"attributes", c.keyValues,
	)

	link := tbl.Record(
		"span-context", t.spanContext,
		"attributes", c.keyValues,
	)

	t.spanData = tbl.Record(
		"span-context", t.spanContext,
		"parent-span-id", str(),
		"span-kind", spanKind,
		"name", str(),
		"start-time", c.datetime,
		"end-time", c.datetime,
		"attributes", c.keyValues,
		"events", tbl.List(event),
		"links", tbl.List(link),
		"status", status,
		"instrumentation-scope", c.scope,
		"dropped-attributes", component.Prim("u32"),
		"dropped-events", component.Prim("u32"),
		"dropped-links", component.Prim("u32"),
	)

	return t
}

// TracingOptions returns the component options implementing
// wasi:otel/tracing on top of t.
func TracingOptions(t Tracer) []component.Option {
	types := newTracingTypes(newCommonTypes())
	tbl := types.tbl
	resolve := tbl.Resolver()

	onStartFD := tbl.Func([]component.TypeRef{types.spanContext}, component.TypeRef{})
	onStart := func(ctx context.Context, args []component.Value) ([]component.Value, error) {
		if len(args) != 1 {
			return nil, errArgs("on-start", 1, len(args))
		}
		l := &lifter{}
		sc := liftSpanContext(l, args[0], "span-context")
		if l.err != nil {
			return nil, l.err
		}
		t.OnStart(ctx, sc)
		return nil, nil
	}

	onEndFD := tbl.Func([]component.TypeRef{types.spanData}, component.TypeRef{})
	onEnd := func(ctx context.Context, args []component.Value) ([]component.Value, error) {
		if len(args) != 1 {
			return nil, errArgs("on-end", 1, len(args))
		}
		l := &lifter{}
		span := liftSpanData(l, args[0])
		if l.err != nil {
			return nil, l.err
		}
		t.OnEnd(ctx, span)
		return nil, nil
	}

	currentFD := tbl.Func(nil, types.spanContext)
	current := func(ctx context.Context, args []component.Value) ([]component.Value, error) {
		if len(args) != 0 {
			return nil, errArgs("current-span-context", 0, len(args))
		}
		return []component.Value{lowerSpanContext(t.CurrentSpanContext(ctx))}, nil
	}

	return []component.Option{
		component.WithImportCustom(TracingInterface, "on-start", onStart, onStartFD, resolve),
		component.WithImportCustom(TracingInterface, "on-end", onEnd, onEndFD, resolve),
		component.WithImportCustom(TracingInterface, "current-span-context", current, currentFD, resolve),

		// The same function under the name it had before the proposal renamed
		// it. wasi-otel took "current-span-context" in April 2026, but the
		// guest SDKs still generate a call to "outer-span-context", so a guest
		// built today imports the old name and one built against the current
		// WIT imports the new one. The signature is identical, so both are
		// served rather than making the choice of SDK version decide whether a
		// guest links. Registering an import a guest never uses costs nothing.
		component.WithImportCustom(TracingInterface, "outer-span-context", current, currentFD, resolve),
	}
}

// liftSpanContext lifts a `span-context` record.
func liftSpanContext(l *lifter, v component.Value, what string) SpanContext {
	f := l.fields(v, 5, what)

	var state []TraceStateEntry
	if entries := l.list(f[4], what+".trace-state"); entries != nil {
		state = make([]TraceStateEntry, len(entries))
		for i, e := range entries {
			pair := l.fields(e, 2, what+".trace-state entry")
			state[i] = TraceStateEntry{
				Key:   l.str(pair[0], what+".trace-state key"),
				Value: l.str(pair[1], what+".trace-state value"),
			}
		}
	}

	return SpanContext{
		TraceID:    l.str(f[0], what+".trace-id"),
		SpanID:     l.str(f[1], what+".span-id"),
		TraceFlags: TraceFlags(l.u32(f[2], what+".trace-flags")),
		IsRemote:   l.bool_(f[3], what+".is-remote"),
		TraceState: state,
	}
}

// lowerSpanContext builds the `span-context` record value returned to a guest.
func lowerSpanContext(sc SpanContext) component.Value {
	state := make([]component.Value, len(sc.TraceState))
	for i, e := range sc.TraceState {
		state[i] = []component.Value{e.Key, e.Value}
	}
	return []component.Value{
		sc.TraceID,
		sc.SpanID,
		uint32(sc.TraceFlags),
		sc.IsRemote,
		state,
	}
}

// liftSpanData lifts a `span-data` record.
func liftSpanData(l *lifter, v component.Value) SpanData {
	const what = "span-data"
	f := l.fields(v, 14, what)

	events := l.list(f[7], what+".events")
	outEvents := make([]Event, len(events))
	for i, e := range events {
		ef := l.fields(e, 3, what+".event")
		outEvents[i] = Event{
			Name:       l.str(ef[0], what+".event.name"),
			Time:       l.datetime(ef[1], what+".event.time"),
			Attributes: l.keyValues(ef[2], what+".event.attributes"),
		}
	}

	links := l.list(f[8], what+".links")
	outLinks := make([]Link, len(links))
	for i, lk := range links {
		lf := l.fields(lk, 2, what+".link")
		outLinks[i] = Link{
			SpanContext: liftSpanContext(l, lf[0], what+".link.span-context"),
			Attributes:  l.keyValues(lf[1], what+".link.attributes"),
		}
	}

	// status is a variant whose third case carries the description.
	var status Status
	disc, payload := l.variant(f[9], what+".status")
	status.Code = StatusCode(disc)
	if StatusCode(disc) == StatusError {
		status.Description = l.str(payload, what+".status.error")
	}

	return SpanData{
		SpanContext:          liftSpanContext(l, f[0], what+".span-context"),
		ParentSpanID:         l.str(f[1], what+".parent-span-id"),
		SpanKind:             SpanKind(l.u32(f[2], what+".span-kind")),
		Name:                 l.str(f[3], what+".name"),
		StartTime:            l.datetime(f[4], what+".start-time"),
		EndTime:              l.datetime(f[5], what+".end-time"),
		Attributes:           l.keyValues(f[6], what+".attributes"),
		Events:               outEvents,
		Links:                outLinks,
		Status:               status,
		InstrumentationScope: l.scope(f[10], what+".instrumentation-scope"),
		DroppedAttributes:    l.u32(f[11], what+".dropped-attributes"),
		DroppedEvents:        l.u32(f[12], what+".dropped-events"),
		DroppedLinks:         l.u32(f[13], what+".dropped-links"),
	}
}
