package wasi_otel

import "time"

// The Go shapes of the WIT records the three interfaces carry. They mirror
// wit/types.wit, wit/tracing.wit, wit/logs.wit and wit/metrics.wit field for
// field, in declaration order, so a reader can hold the WIT beside them.
//
// Optional fields follow one rule: a WIT `option<T>` whose T has no natural
// empty value is a pointer, and `option<list<T>>` is a slice whose nil is
// `none`. An empty list arrives distinct from an absent one, so the slice form
// loses nothing.

// KeyValue is one attribute.
//
// Value is not a plain string: the WIT type is documented as a JSON encoding
// of OpenTelemetry's AnyValue, because WIT cannot express the recursive type.
// Byte arrays inside it are base64 with a `data:application/octet-stream;base64,`
// prefix. This package passes the encoded string through untouched -- decoding
// it is the embedder's business, and a sink that only forwards should not pay
// to parse it.
type KeyValue struct {
	Key   string
	Value string
}

// Resource identifies the entity producing telemetry.
//
// It is `%resource` in the WIT, where the `%` is only a keyword escape.
type Resource struct {
	Attributes []KeyValue
	SchemaURL  *string
}

// InstrumentationScope describes the library that produced telemetry.
type InstrumentationScope struct {
	Name       string
	Version    *string
	SchemaURL  *string
	Attributes []KeyValue
}

// TraceFlags is the W3C trace flags bitset.
type TraceFlags uint32

// TraceFlagsSampled reports whether the span should be sampled. It is the only
// flag the WIT defines.
const TraceFlagsSampled TraceFlags = 1 << 0

// Sampled reports whether the sampled flag is set.
func (f TraceFlags) Sampled() bool { return f&TraceFlagsSampled != 0 }

// TraceStateEntry is one entry of a `trace-state`, which lets several tracing
// systems take part in one trace.
type TraceStateEntry struct {
	Key   string
	Value string
}

// SpanContext is the identifying information that propagates with a span.
//
// TraceID and SpanID are hexadecimal strings -- 16 and 8 bytes respectively --
// carried as text by the WIT rather than as bytes.
type SpanContext struct {
	TraceID    string
	SpanID     string
	TraceFlags TraceFlags
	IsRemote   bool
	TraceState []TraceStateEntry
}

// SpanKind describes a span's relationship to its parents and children.
type SpanKind uint32

// The span kinds, in the WIT's declaration order, which is the wire order.
const (
	SpanKindClient SpanKind = iota
	SpanKindServer
	SpanKindProducer
	SpanKindConsumer
	SpanKindInternal
)

// String implements fmt.Stringer.
func (k SpanKind) String() string {
	switch k {
	case SpanKindClient:
		return "client"
	case SpanKindServer:
		return "server"
	case SpanKindProducer:
		return "producer"
	case SpanKindConsumer:
		return "consumer"
	case SpanKindInternal:
		return "internal"
	}
	return "unknown"
}

// StatusCode is which arm of the WIT `status` variant a Status carries.
type StatusCode uint32

// The status codes, in the WIT's declaration order.
const (
	StatusUnset StatusCode = iota
	StatusOK
	StatusError
)

// Status is a span's status. Description is set only for StatusError, which is
// the one arm carrying a payload.
type Status struct {
	Code        StatusCode
	Description string
}

// Event is a moment in time on a span.
type Event struct {
	Name       string
	Time       time.Time
	Attributes []KeyValue
}

// Link describes a relationship to another span.
type Link struct {
	SpanContext SpanContext
	Attributes  []KeyValue
}

// SpanData is a finished span, as handed to Tracer.OnEnd.
type SpanData struct {
	SpanContext          SpanContext
	ParentSpanID         string
	SpanKind             SpanKind
	Name                 string
	StartTime            time.Time
	EndTime              time.Time
	Attributes           []KeyValue
	Events               []Event
	Links                []Link
	Status               Status
	InstrumentationScope InstrumentationScope

	// Counts of what the guest's own limits discarded, which is how a
	// consumer learns the span is not complete.
	DroppedAttributes uint32
	DroppedEvents     uint32
	DroppedLinks      uint32
}

// LogRecord is one emitted log, as handed to LogEmitter.OnEmit.
//
// Nearly every field is optional in the WIT, so nearly every field here is a
// pointer or a nil-able slice.
type LogRecord struct {
	Timestamp         *time.Time
	ObservedTimestamp *time.Time
	SeverityText      *string
	SeverityNumber    *uint8
	// Body carries the same JSON-encoded AnyValue as KeyValue.Value.
	Body                 *string
	Attributes           []KeyValue
	EventName            *string
	Resource             *Resource
	InstrumentationScope *InstrumentationScope
	TraceID              *string
	SpanID               *string
	TraceFlags           *TraceFlags
}

// NumberKind is the numeric type an instrument records in.
type NumberKind uint32

// The number kinds, in the WIT's `metric-number` declaration order.
const (
	NumberF64 NumberKind = iota
	NumberS64
	NumberU64
)

// Number is one measured value. Kind says which field carries it.
type Number struct {
	Kind NumberKind
	F64  float64
	S64  int64
	U64  uint64
}

// Float returns the value as a float64 whatever its kind, for a consumer that
// only wants a magnitude.
func (n Number) Float() float64 {
	switch n.Kind {
	case NumberS64:
		return float64(n.S64)
	case NumberU64:
		return float64(n.U64)
	}
	return n.F64
}

// Temporality is the window an aggregation was calculated over.
type Temporality uint32

// The temporalities, in the WIT's declaration order.
const (
	TemporalityCumulative Temporality = iota
	TemporalityDelta
	TemporalityLowMemory
)

// String implements fmt.Stringer.
func (t Temporality) String() string {
	switch t {
	case TemporalityCumulative:
		return "cumulative"
	case TemporalityDelta:
		return "delta"
	case TemporalityLowMemory:
		return "low-memory"
	}
	return "unknown"
}

// Exemplar is a measurement sampled from a time series as a typical example.
//
// SpanID and TraceID are empty when no span was active, or when the active
// span was not sampled.
type Exemplar struct {
	FilteredAttributes []KeyValue
	Time               time.Time
	Value              Number
	SpanID             string
	TraceID            string
}

// GaugeDataPoint is one point of a Gauge.
type GaugeDataPoint struct {
	Attributes []KeyValue
	Value      Number
	Exemplars  []Exemplar
}

// Gauge is the current value of an instrument.
type Gauge struct {
	DataPoints []GaugeDataPoint
	StartTime  *time.Time
	Time       time.Time
}

// SumDataPoint is one point of a Sum.
type SumDataPoint struct {
	Attributes []KeyValue
	Value      Number
	Exemplars  []Exemplar
}

// Sum is the sum of an instrument's measurements.
type Sum struct {
	DataPoints  []SumDataPoint
	StartTime   time.Time
	Time        time.Time
	Temporality Temporality
	IsMonotonic bool
}

// HistogramDataPoint is one point of a Histogram.
type HistogramDataPoint struct {
	Attributes   []KeyValue
	Count        uint64
	Bounds       []float64
	BucketCounts []uint64
	Min          *Number
	Max          *Number
	Sum          Number
	Exemplars    []Exemplar
}

// Histogram is a bucketed distribution of an instrument's measurements.
type Histogram struct {
	DataPoints  []HistogramDataPoint
	StartTime   time.Time
	Time        time.Time
	Temporality Temporality
}

// ExponentialBucket is a run of bucket counts starting at Offset.
//
// Counts[i] holds the count for the bucket at index Offset+i.
type ExponentialBucket struct {
	Offset int32
	Counts []uint64
}

// ExponentialHistogramDataPoint is one point of an ExponentialHistogram.
type ExponentialHistogramDataPoint struct {
	Attributes []KeyValue
	Count      uint64
	Min        *Number
	Max        *Number
	Sum        Number
	// Scale sets the resolution: bucket boundaries sit at powers of
	// base = 2 ^ (2 ^ -Scale).
	Scale int8
	// ZeroCount is how many values fell within ZeroThreshold of zero.
	ZeroCount      uint64
	PositiveBucket ExponentialBucket
	NegativeBucket ExponentialBucket
	ZeroThreshold  float64
	Exemplars      []Exemplar
}

// ExponentialHistogram is a distribution with exponentially sized buckets.
type ExponentialHistogram struct {
	DataPoints  []ExponentialHistogramDataPoint
	StartTime   time.Time
	Time        time.Time
	Temporality Temporality
}

// MetricData is a metric's aggregated data.
//
// The WIT models this as a twelve-case variant, one per pairing of a number
// type with an aggregation. The pairing is carried here as two fields instead:
// Number is the instrument's numeric type, and exactly one of the four
// aggregation pointers is non-nil.
type MetricData struct {
	Number               NumberKind
	Gauge                *Gauge
	Sum                  *Sum
	Histogram            *Histogram
	ExponentialHistogram *ExponentialHistogram
}

// Metric is one named aggregation produced by a meter.
type Metric struct {
	Name        string
	Description string
	Unit        string
	Data        MetricData
}

// ScopeMetrics is the metrics one instrumentation scope produced.
type ScopeMetrics struct {
	Scope   InstrumentationScope
	Metrics []Metric
}

// ResourceMetrics is a full export: the resource that collected the metrics,
// and the metrics grouped by scope.
type ResourceMetrics struct {
	Resource     Resource
	ScopeMetrics []ScopeMetrics
}
