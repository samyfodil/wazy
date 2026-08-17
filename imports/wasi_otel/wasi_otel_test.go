package wasi_otel_test

import (
	"context"
	_ "embed"
	"errors"
	"testing"
	"time"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/component"
	"github.com/samyfodil/wazy/imports/wasi_otel"
	"github.com/samyfodil/wazy/imports/wasip2"
	"github.com/samyfodil/wazy/internal/testing/require"
)

// otelGuestWasm is a component built with real wit-bindgen against the
// proposal's own WIT, so what it sends is what a generated guest sends rather
// than what this package expects. Source and build instructions:
// testdata/otelguest/.
//
//go:embed testdata/otelguest.wasm
var otelGuestWasm []byte

// collector records everything the guest reports, and is the Tracer,
// LogEmitter and MetricExporter all at once so one instance serves all three
// interfaces.
type collector struct {
	started   []wasi_otel.SpanContext
	ended     []wasi_otel.SpanData
	logs      []wasi_otel.LogRecord
	exports   []wasi_otel.ResourceMetrics
	current   wasi_otel.SpanContext
	exportErr error
}

func (c *collector) OnStart(_ context.Context, sc wasi_otel.SpanContext) {
	c.started = append(c.started, sc)
}

func (c *collector) OnEnd(_ context.Context, s wasi_otel.SpanData) {
	c.ended = append(c.ended, s)
}

func (c *collector) CurrentSpanContext(context.Context) wasi_otel.SpanContext {
	return c.current
}

func (c *collector) OnEmit(_ context.Context, r wasi_otel.LogRecord) {
	c.logs = append(c.logs, r)
}

func (c *collector) Export(_ context.Context, m wasi_otel.ResourceMetrics) error {
	c.exports = append(c.exports, m)
	return c.exportErr
}

// run instantiates the guest with the collector serving wasi:otel, and returns
// what the guest's run() reported.
func run(t *testing.T, c *collector) string {
	t.Helper()
	ctx := context.Background()

	r := wazy.NewRuntime(ctx)
	t.Cleanup(func() { r.Close(ctx) })

	// The guest is a Rust component, so it needs standard WASI for its own
	// runtime alongside the otel interfaces under test.
	opts := wasip2.WithWASI(wasip2.WASIConfig{})
	opts = append(opts, wasi_otel.Options(c)...)

	inst, err := component.Instantiate(ctx, r, otelGuestWasm, opts...)
	require.NoError(t, err)
	t.Cleanup(func() { inst.Close(ctx) })

	results, err := inst.Call(ctx, "run")
	require.NoError(t, err)
	require.Equal(t, 1, len(results))

	out, ok := results[0].(string)
	require.True(t, ok, "run() should return a string, got %T", results[0])
	return out
}

// TestGuest_tracing is the acceptance test for the tracing interface: a real
// span, with the shapes a simple one would not have -- events, links, a status
// carrying a payload, and a trace-state with entries.
func TestGuest_tracing(t *testing.T) {
	c := &collector{
		current: wasi_otel.SpanContext{
			TraceID:    "ffffffffffffffffffffffffffffffff",
			SpanID:     "ffffffffffffffff",
			TraceFlags: wasi_otel.TraceFlagsSampled,
			TraceState: []wasi_otel.TraceStateEntry{{Key: "host", Value: "wazy"}},
		},
	}

	out := run(t, c)

	// The return path: what the host handed back arrived intact.
	require.Equal(t,
		"current=ffffffffffffffffffffffffffffffff sampled=true state=1 export=ok",
		out)

	// on-start carried only a context.
	require.Equal(t, 1, len(c.started))
	require.Equal(t, "0123456789abcdef0123456789abcdef", c.started[0].TraceID)
	require.Equal(t, "0123456789abcdef", c.started[0].SpanID)
	require.True(t, c.started[0].TraceFlags.Sampled())
	require.False(t, c.started[0].IsRemote)
	require.Equal(t, 2, len(c.started[0].TraceState))
	require.Equal(t, "vendor-a", c.started[0].TraceState[0].Key)
	require.Equal(t, "value-b", c.started[0].TraceState[1].Value)

	require.Equal(t, 1, len(c.ended))
	span := c.ended[0]

	require.Equal(t, "GET /things", span.Name)
	require.Equal(t, "fedcba9876543210", span.ParentSpanID)
	require.Equal(t, wasi_otel.SpanKindServer, span.SpanKind)
	require.Equal(t, "server", span.SpanKind.String())

	// datetime round-trips through time.Time, nanoseconds included.
	require.Equal(t, int64(1700000000), span.StartTime.Unix())
	require.Equal(t, 250, span.StartTime.Nanosecond())
	require.Equal(t, int64(1700000001), span.EndTime.Unix())
	require.Equal(t, 500, span.EndTime.Nanosecond())

	require.Equal(t, 2, len(span.Attributes))
	require.Equal(t, "http.method", span.Attributes[0].Key)
	require.Equal(t, `"GET"`, span.Attributes[0].Value)

	require.Equal(t, 1, len(span.Events))
	require.Equal(t, "cache.miss", span.Events[0].Name)
	require.Equal(t, int64(1700000000), span.Events[0].Time.Unix())
	require.Equal(t, 1, len(span.Events[0].Attributes))

	require.Equal(t, 1, len(span.Links))
	require.Equal(t, "0123456789abcdef0123456789abcdef", span.Links[0].SpanContext.TraceID)
	require.Equal(t, "link.kind", span.Links[0].Attributes[0].Key)

	// The status variant's payload arm.
	require.Equal(t, wasi_otel.StatusError, span.Status.Code)
	require.Equal(t, "boom", span.Status.Description)

	require.Equal(t, "otelguest", span.InstrumentationScope.Name)
	require.NotNil(t, span.InstrumentationScope.Version)
	require.Equal(t, "0.1.0", *span.InstrumentationScope.Version)
	// The one option the guest deliberately left none.
	require.Nil(t, span.InstrumentationScope.SchemaURL)

	require.Equal(t, uint32(1), span.DroppedAttributes)
	require.Equal(t, uint32(2), span.DroppedEvents)
	require.Equal(t, uint32(3), span.DroppedLinks)
}

// TestGuest_logs covers the log record both fully populated and fully empty,
// which is where an option is most easily mishandled.
func TestGuest_logs(t *testing.T) {
	c := &collector{}
	run(t, c)

	require.Equal(t, 2, len(c.logs))

	full := c.logs[0]
	require.NotNil(t, full.Timestamp)
	require.Equal(t, int64(1700000002), full.Timestamp.Unix())
	require.NotNil(t, full.ObservedTimestamp)
	require.Equal(t, int64(1700000003), full.ObservedTimestamp.Unix())
	require.NotNil(t, full.SeverityText)
	require.Equal(t, "WARN", *full.SeverityText)
	require.NotNil(t, full.SeverityNumber)
	require.Equal(t, uint8(13), *full.SeverityNumber)
	require.NotNil(t, full.Body)
	require.Equal(t, `"disk almost full"`, *full.Body)
	require.Equal(t, 1, len(full.Attributes))
	require.Equal(t, "disk.pct", full.Attributes[0].Key)
	require.NotNil(t, full.EventName)
	require.Equal(t, "disk.usage", *full.EventName)
	require.NotNil(t, full.Resource)
	require.Equal(t, 1, len(full.Resource.Attributes))
	require.NotNil(t, full.Resource.SchemaURL)
	require.Equal(t, "https://example.invalid/schema", *full.Resource.SchemaURL)
	require.NotNil(t, full.InstrumentationScope)
	require.Equal(t, "otelguest", full.InstrumentationScope.Name)
	require.NotNil(t, full.TraceID)
	require.NotNil(t, full.SpanID)
	require.NotNil(t, full.TraceFlags)
	require.True(t, full.TraceFlags.Sampled())

	// Every field none: each optional has to come back nil rather than a zero
	// value that reads as present.
	empty := c.logs[1]
	require.Nil(t, empty.Timestamp)
	require.Nil(t, empty.ObservedTimestamp)
	require.Nil(t, empty.SeverityText)
	require.Nil(t, empty.SeverityNumber)
	require.Nil(t, empty.Body)
	require.Nil(t, empty.Attributes)
	require.Nil(t, empty.EventName)
	require.Nil(t, empty.Resource)
	require.Nil(t, empty.InstrumentationScope)
	require.Nil(t, empty.TraceID)
	require.Nil(t, empty.SpanID)
	require.Nil(t, empty.TraceFlags)
}

// TestGuest_metrics covers the largest type graph: four aggregations, each
// paired with a different number type, and the signed and optional fields
// inside them.
func TestGuest_metrics(t *testing.T) {
	c := &collector{}
	run(t, c)

	require.Equal(t, 1, len(c.exports))
	rm := c.exports[0]

	require.Equal(t, 1, len(rm.Resource.Attributes))
	require.Equal(t, "service.name", rm.Resource.Attributes[0].Key)
	require.Equal(t, 1, len(rm.ScopeMetrics))
	require.Equal(t, "otelguest", rm.ScopeMetrics[0].Scope.Name)

	ms := rm.ScopeMetrics[0].Metrics
	require.Equal(t, 4, len(ms))

	// f64-gauge.
	require.Equal(t, "temperature", ms[0].Name)
	require.Equal(t, "Cel", ms[0].Unit)
	require.Equal(t, wasi_otel.NumberF64, ms[0].Data.Number)
	require.NotNil(t, ms[0].Data.Gauge)
	g := ms[0].Data.Gauge
	require.Nil(t, g.StartTime) // option<datetime>, none
	require.Equal(t, int64(1700000005), g.Time.Unix())
	require.Equal(t, 1, len(g.DataPoints))
	require.Equal(t, wasi_otel.NumberF64, g.DataPoints[0].Value.Kind)
	require.Equal(t, 42.5, g.DataPoints[0].Value.F64)
	require.Equal(t, 1, len(g.DataPoints[0].Exemplars))
	require.Equal(t, 1.5, g.DataPoints[0].Exemplars[0].Value.F64)
	require.Equal(t, "0123456789abcdef", g.DataPoints[0].Exemplars[0].SpanID)

	// u64-sum.
	require.Equal(t, "requests", ms[1].Name)
	require.Equal(t, wasi_otel.NumberU64, ms[1].Data.Number)
	require.NotNil(t, ms[1].Data.Sum)
	s := ms[1].Data.Sum
	require.True(t, s.IsMonotonic)
	require.Equal(t, wasi_otel.TemporalityDelta, s.Temporality)
	require.Equal(t, "delta", s.Temporality.String())
	require.Equal(t, wasi_otel.NumberU64, s.DataPoints[0].Value.Kind)
	require.Equal(t, uint64(7), s.DataPoints[0].Value.U64)

	// s64-histogram, including a negative value and one option of each arm.
	require.Equal(t, "latency", ms[2].Name)
	require.Equal(t, wasi_otel.NumberS64, ms[2].Data.Number)
	require.NotNil(t, ms[2].Data.Histogram)
	h := ms[2].Data.Histogram
	require.Equal(t, wasi_otel.TemporalityCumulative, h.Temporality)
	dp := h.DataPoints[0]
	require.Equal(t, uint64(3), dp.Count)
	require.Equal(t, []float64{1, 5, 10}, dp.Bounds)
	require.Equal(t, []uint64{1, 1, 1, 0}, dp.BucketCounts)
	require.NotNil(t, dp.Min)
	require.Equal(t, int64(-2), dp.Min.S64)
	require.Nil(t, dp.Max)
	require.Equal(t, int64(9), dp.Sum.S64)

	// f64-exponential-histogram, with the signed scale and offset.
	require.Equal(t, "payload", ms[3].Name)
	require.Equal(t, wasi_otel.NumberF64, ms[3].Data.Number)
	require.NotNil(t, ms[3].Data.ExponentialHistogram)
	eh := ms[3].Data.ExponentialHistogram
	require.Equal(t, wasi_otel.TemporalityLowMemory, eh.Temporality)
	edp := eh.DataPoints[0]
	require.Equal(t, uint64(4), edp.Count)
	require.Equal(t, int8(-3), edp.Scale)
	require.Equal(t, uint64(1), edp.ZeroCount)
	require.Equal(t, 0.5, edp.ZeroThreshold)
	require.Equal(t, int32(2), edp.PositiveBucket.Offset)
	require.Equal(t, []uint64{1, 2}, edp.PositiveBucket.Counts)
	require.Equal(t, int32(-4), edp.NegativeBucket.Offset)
	require.Equal(t, 12.25, edp.Sum.F64)
}

// TestGuest_exportError covers the one interface with a result: a failing
// exporter reaches the guest as the err arm rather than trapping it.
func TestGuest_exportError(t *testing.T) {
	c := &collector{exportErr: errors.New("collector unreachable")}
	out := run(t, c)
	require.Equal(t,
		"current= sampled=false state=0 export=err(collector unreachable)",
		out)
}

// TestOptions covers which interfaces a handler is given, since a handler
// implementing only some of them must not be registered for the rest.
func TestOptions(t *testing.T) {
	// The full collector serves all three: three tracing functions (including
	// the pre-rename alias), one log, one metrics.
	require.Equal(t, 6, len(wasi_otel.Options(&collector{})))

	require.Equal(t, 4, len(wasi_otel.Options(tracerOnly{})))
	require.Equal(t, 1, len(wasi_otel.Options(logsOnly{})))
	require.Equal(t, 1, len(wasi_otel.Options(metricsOnly{})))

	// Something that implements none of them is served nothing, rather than
	// silently registering a broken import.
	require.Equal(t, 0, len(wasi_otel.Options(struct{}{})))
}

type tracerOnly struct{}

func (tracerOnly) OnStart(context.Context, wasi_otel.SpanContext) {}
func (tracerOnly) OnEnd(context.Context, wasi_otel.SpanData)      {}
func (tracerOnly) CurrentSpanContext(context.Context) wasi_otel.SpanContext {
	return wasi_otel.SpanContext{}
}

type logsOnly struct{}

func (logsOnly) OnEmit(context.Context, wasi_otel.LogRecord) {}

type metricsOnly struct{}

func (metricsOnly) Export(context.Context, wasi_otel.ResourceMetrics) error { return nil }

// TestTypes covers the small helpers on the exported types, which a consumer
// reads telemetry through.
func TestTypes(t *testing.T) {
	require.False(t, wasi_otel.TraceFlags(0).Sampled())
	require.True(t, wasi_otel.TraceFlagsSampled.Sampled())

	require.Equal(t, "client", wasi_otel.SpanKindClient.String())
	require.Equal(t, "producer", wasi_otel.SpanKindProducer.String())
	require.Equal(t, "consumer", wasi_otel.SpanKindConsumer.String())
	require.Equal(t, "internal", wasi_otel.SpanKindInternal.String())
	require.Equal(t, "unknown", wasi_otel.SpanKind(99).String())

	require.Equal(t, "cumulative", wasi_otel.TemporalityCumulative.String())
	require.Equal(t, "unknown", wasi_otel.Temporality(99).String())

	// Float reports a magnitude whatever the instrument's numeric type.
	require.Equal(t, 1.5, wasi_otel.Number{Kind: wasi_otel.NumberF64, F64: 1.5}.Float())
	require.Equal(t, -2.0, wasi_otel.Number{Kind: wasi_otel.NumberS64, S64: -2}.Float())
	require.Equal(t, 7.0, wasi_otel.Number{Kind: wasi_otel.NumberU64, U64: 7}.Float())
}

// TestSpanTimesAreUTC covers the datetime conversion landing in UTC rather
// than the host's zone, which would make recorded spans depend on where the
// embedder runs.
func TestSpanTimesAreUTC(t *testing.T) {
	c := &collector{}
	run(t, c)
	require.Equal(t, time.UTC, c.ended[0].StartTime.Location())
}

// outerGuestWasm imports the tracing interface under the name today's guest
// SDKs still generate. Source: testdata/outerguest/.
//
//go:embed testdata/outerguest.wasm
var outerGuestWasm []byte

// TestGuest_outerSpanContext covers the pre-rename name.
//
// wasi-otel renamed this function to current-span-context in April 2026, but
// bytecodealliance/opentelemetry-wasi still generates a call to
// outer-span-context, so a guest built with it today imports the old name.
// Both are served, and this is the guest that proves the old one links --
// without it the alias would be a claim in a comment.
func TestGuest_outerSpanContext(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	c := &collector{
		current: wasi_otel.SpanContext{
			TraceID:    "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			TraceFlags: wasi_otel.TraceFlagsSampled,
			TraceState: []wasi_otel.TraceStateEntry{{Key: "host", Value: "wazy"}},
		},
	}

	opts := wasip2.WithWASI(wasip2.WASIConfig{})
	opts = append(opts, wasi_otel.Options(c)...)

	inst, err := component.Instantiate(ctx, r, outerGuestWasm, opts...)
	require.NoError(t, err)
	defer inst.Close(ctx)

	results, err := inst.Call(ctx, "run")
	require.NoError(t, err)
	require.Equal(t, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa sampled=true state=1", results[0].(string))
}
