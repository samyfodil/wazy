package wasi_otel

import (
	"context"

	"github.com/samyfodil/wazy/component"
)

// MetricExporter receives a guest's metric exports.
//
// Unlike the tracing and logs callbacks, `export` does return
// `result<_, error>` in the WIT, so a failure here is expressible: the error's
// message is handed back to the guest as the err arm, and the guest decides
// what to do about it. Returning an error does not trap.
type MetricExporter interface {
	Export(ctx context.Context, metrics ResourceMetrics) error
}

// metricsTypes is the metrics interface's type graph.
//
// It is the largest of the three by some way, but it introduces nothing new:
// the same records, lists and options, nested further.
type metricsTypes struct {
	*commonTypes
	resourceMetrics component.TypeRef
}

func newMetricsTypes(c *commonTypes) *metricsTypes {
	t := &metricsTypes{commonTypes: c}
	tbl := c.tbl

	u64 := component.Prim("u64")
	f64 := component.Prim("f64")

	// metric-number is the variant every measured value goes through. Case
	// order is the wire order, and NumberKind indexes it.
	metricNumber := tbl.Variant(
		component.VariantCaseSpec{Name: "f64", Type: f64},
		component.VariantCaseSpec{Name: "s64", Type: component.Prim("s64")},
		component.VariantCaseSpec{Name: "u64", Type: u64},
	)
	optNumber := tbl.Option(metricNumber)

	temporality := tbl.Enum("cumulative", "delta", "low-memory")

	exemplar := tbl.Record(
		"filtered-attributes", c.keyValues,
		"time", c.datetime,
		"value", metricNumber,
		"span-id", str(),
		"trace-id", str(),
	)
	exemplars := tbl.List(exemplar)

	// gauge and sum carry structurally identical data points; they are
	// separate declarations in the WIT, so they are separate here too.
	gaugeDataPoint := tbl.Record(
		"attributes", c.keyValues,
		"value", metricNumber,
		"exemplars", exemplars,
	)
	gauge := tbl.Record(
		"data-points", tbl.List(gaugeDataPoint),
		"start-time", tbl.Option(c.datetime),
		"time", c.datetime,
	)

	sumDataPoint := tbl.Record(
		"attributes", c.keyValues,
		"value", metricNumber,
		"exemplars", exemplars,
	)
	sum := tbl.Record(
		"data-points", tbl.List(sumDataPoint),
		"start-time", c.datetime,
		"time", c.datetime,
		"temporality", temporality,
		"is-monotonic", component.Prim("bool"),
	)

	histogramDataPoint := tbl.Record(
		"attributes", c.keyValues,
		"count", u64,
		"bounds", tbl.List(f64),
		"bucket-counts", tbl.List(u64),
		"min", optNumber,
		"max", optNumber,
		"sum", metricNumber,
		"exemplars", exemplars,
	)
	histogram := tbl.Record(
		"data-points", tbl.List(histogramDataPoint),
		"start-time", c.datetime,
		"time", c.datetime,
		"temporality", temporality,
	)

	exponentialBucket := tbl.Record(
		"offset", component.Prim("s32"),
		"counts", tbl.List(u64),
	)
	expHistogramDataPoint := tbl.Record(
		"attributes", c.keyValues,
		"count", u64,
		"min", optNumber,
		"max", optNumber,
		"sum", metricNumber,
		"scale", component.Prim("s8"),
		"zero-count", u64,
		"positive-bucket", exponentialBucket,
		"negative-bucket", exponentialBucket,
		"zero-threshold", f64,
		"exemplars", exemplars,
	)
	expHistogram := tbl.Record(
		"data-points", tbl.List(expHistogramDataPoint),
		"start-time", c.datetime,
		"time", c.datetime,
		"temporality", temporality,
	)

	// metric-data pairs each numeric type with each aggregation: twelve cases
	// over four payload shapes. The case order is what liftMetricData decodes
	// back into a (number kind, aggregation) pair.
	metricData := tbl.Variant(
		component.VariantCaseSpec{Name: "f64-gauge", Type: gauge},
		component.VariantCaseSpec{Name: "f64-sum", Type: sum},
		component.VariantCaseSpec{Name: "f64-histogram", Type: histogram},
		component.VariantCaseSpec{Name: "f64-exponential-histogram", Type: expHistogram},
		component.VariantCaseSpec{Name: "u64-gauge", Type: gauge},
		component.VariantCaseSpec{Name: "u64-sum", Type: sum},
		component.VariantCaseSpec{Name: "u64-histogram", Type: histogram},
		component.VariantCaseSpec{Name: "u64-exponential-histogram", Type: expHistogram},
		component.VariantCaseSpec{Name: "s64-gauge", Type: gauge},
		component.VariantCaseSpec{Name: "s64-sum", Type: sum},
		component.VariantCaseSpec{Name: "s64-histogram", Type: histogram},
		component.VariantCaseSpec{Name: "s64-exponential-histogram", Type: expHistogram},
	)

	metric := tbl.Record(
		"name", str(),
		"description", str(),
		"unit", str(),
		"data", metricData,
	)

	scopeMetrics := tbl.Record(
		"scope", c.scope,
		"metrics", tbl.List(metric),
	)

	t.resourceMetrics = tbl.Record(
		"resource", c.resource,
		"scope-metrics", tbl.List(scopeMetrics),
	)

	return t
}

// MetricsOptions returns the component options implementing
// wasi:otel/metrics on top of e.
func MetricsOptions(e MetricExporter) []component.Option {
	types := newMetricsTypes(newCommonTypes())
	tbl := types.tbl
	resolve := tbl.Resolver()

	// export: func(metrics: resource-metrics) -> result<_, error>, where
	// `error` is a string. The ok arm carries nothing.
	resultRef := tbl.Result(component.TypeRef{}, str())
	exportFD := tbl.Func([]component.TypeRef{types.resourceMetrics}, resultRef)

	export := func(ctx context.Context, args []component.Value) ([]component.Value, error) {
		if len(args) != 1 {
			return nil, errArgs("export", 1, len(args))
		}
		l := &lifter{}
		rm := liftResourceMetrics(l, args[0])
		if l.err != nil {
			return nil, l.err
		}
		if err := e.Export(ctx, rm); err != nil {
			// The guest asked for a result, so a failing exporter is an err
			// arm rather than a trap.
			return []component.Value{component.ResultValue{
				IsErr:   true,
				Payload: err.Error(),
			}}, nil
		}
		return []component.Value{component.ResultValue{}}, nil
	}

	return []component.Option{
		component.WithImportCustom(MetricsInterface, "export", export, exportFD, resolve),
	}
}

// liftResourceMetrics lifts a `resource-metrics`.
func liftResourceMetrics(l *lifter, v component.Value) ResourceMetrics {
	const what = "resource-metrics"
	f := l.fields(v, 2, what)

	scopes := l.list(f[1], what+".scope-metrics")
	out := make([]ScopeMetrics, len(scopes))
	for i, s := range scopes {
		sf := l.fields(s, 2, what+".scope-metrics")
		metrics := l.list(sf[1], what+".metrics")
		ms := make([]Metric, len(metrics))
		for j, m := range metrics {
			mf := l.fields(m, 4, "metric")
			ms[j] = Metric{
				Name:        l.str(mf[0], "metric.name"),
				Description: l.str(mf[1], "metric.description"),
				Unit:        l.str(mf[2], "metric.unit"),
				Data:        liftMetricData(l, mf[3]),
			}
		}
		out[i] = ScopeMetrics{
			Scope:   l.scope(sf[0], what+".scope"),
			Metrics: ms,
		}
	}

	return ResourceMetrics{
		Resource:     l.resource(f[0], what+".resource"),
		ScopeMetrics: out,
	}
}

// liftMetricData decodes the twelve-case `metric-data` variant back into a
// number kind and one aggregation.
//
// The cases run aggregation-fastest within number type -- f64's four, then
// u64's, then s64's -- so the discriminant divides into the pair.
func liftMetricData(l *lifter, v component.Value) MetricData {
	disc, payload := l.variant(v, "metric-data")
	if l.err != nil {
		return MetricData{}
	}
	if disc > 11 {
		l.fail("metric-data: case %d is out of range", disc)
		return MetricData{}
	}

	// The WIT declares f64, then u64, then s64 -- not the order metric-number
	// uses -- so the number kind is looked up rather than computed.
	var data MetricData
	switch disc / 4 {
	case 0:
		data.Number = NumberF64
	case 1:
		data.Number = NumberU64
	case 2:
		data.Number = NumberS64
	}

	switch disc % 4 {
	case 0:
		g := liftGauge(l, payload)
		data.Gauge = &g
	case 1:
		s := liftSum(l, payload)
		data.Sum = &s
	case 2:
		h := liftHistogram(l, payload)
		data.Histogram = &h
	case 3:
		h := liftExponentialHistogram(l, payload)
		data.ExponentialHistogram = &h
	}
	return data
}

// liftNumber lifts a `metric-number` variant.
func liftNumber(l *lifter, v component.Value, what string) Number {
	disc, payload := l.variant(v, what)
	switch NumberKind(disc) {
	case NumberF64:
		return Number{Kind: NumberF64, F64: l.f64(payload, what+".f64")}
	case NumberS64:
		return Number{Kind: NumberS64, S64: l.s64(payload, what+".s64")}
	case NumberU64:
		return Number{Kind: NumberU64, U64: l.u64(payload, what+".u64")}
	}
	l.fail("%s: case %d is out of range", what, disc)
	return Number{}
}

// liftOptNumber lifts an option<metric-number>.
func liftOptNumber(l *lifter, v component.Value, what string) *Number {
	inner, ok := l.some(v)
	if !ok {
		return nil
	}
	n := liftNumber(l, inner, what)
	return &n
}

// liftExemplars lifts a list<exemplar>.
func liftExemplars(l *lifter, v component.Value, what string) []Exemplar {
	items := l.list(v, what)
	out := make([]Exemplar, len(items))
	for i, e := range items {
		f := l.fields(e, 5, "exemplar")
		out[i] = Exemplar{
			FilteredAttributes: l.keyValues(f[0], "exemplar.filtered-attributes"),
			Time:               l.datetime(f[1], "exemplar.time"),
			Value:              liftNumber(l, f[2], "exemplar.value"),
			SpanID:             l.str(f[3], "exemplar.span-id"),
			TraceID:            l.str(f[4], "exemplar.trace-id"),
		}
	}
	return out
}

func liftGauge(l *lifter, v component.Value) Gauge {
	const what = "gauge"
	f := l.fields(v, 3, what)

	points := l.list(f[0], what+".data-points")
	out := make([]GaugeDataPoint, len(points))
	for i, p := range points {
		pf := l.fields(p, 3, what+".data-point")
		out[i] = GaugeDataPoint{
			Attributes: l.keyValues(pf[0], what+".data-point.attributes"),
			Value:      liftNumber(l, pf[1], what+".data-point.value"),
			Exemplars:  liftExemplars(l, pf[2], what+".data-point.exemplars"),
		}
	}

	return Gauge{
		DataPoints: out,
		StartTime:  l.optDatetime(f[1], what+".start-time"),
		Time:       l.datetime(f[2], what+".time"),
	}
}

func liftSum(l *lifter, v component.Value) Sum {
	const what = "sum"
	f := l.fields(v, 5, what)

	points := l.list(f[0], what+".data-points")
	out := make([]SumDataPoint, len(points))
	for i, p := range points {
		pf := l.fields(p, 3, what+".data-point")
		out[i] = SumDataPoint{
			Attributes: l.keyValues(pf[0], what+".data-point.attributes"),
			Value:      liftNumber(l, pf[1], what+".data-point.value"),
			Exemplars:  liftExemplars(l, pf[2], what+".data-point.exemplars"),
		}
	}

	return Sum{
		DataPoints:  out,
		StartTime:   l.datetime(f[1], what+".start-time"),
		Time:        l.datetime(f[2], what+".time"),
		Temporality: Temporality(l.u32(f[3], what+".temporality")),
		IsMonotonic: l.bool_(f[4], what+".is-monotonic"),
	}
}

func liftHistogram(l *lifter, v component.Value) Histogram {
	const what = "histogram"
	f := l.fields(v, 4, what)

	points := l.list(f[0], what+".data-points")
	out := make([]HistogramDataPoint, len(points))
	for i, p := range points {
		pf := l.fields(p, 8, what+".data-point")

		outBounds := l.f64s(pf[2], what+".bounds")
		outCounts := l.u64s(pf[3], what+".bucket-counts")

		out[i] = HistogramDataPoint{
			Attributes:   l.keyValues(pf[0], what+".data-point.attributes"),
			Count:        l.u64(pf[1], what+".data-point.count"),
			Bounds:       outBounds,
			BucketCounts: outCounts,
			Min:          liftOptNumber(l, pf[4], what+".data-point.min"),
			Max:          liftOptNumber(l, pf[5], what+".data-point.max"),
			Sum:          liftNumber(l, pf[6], what+".data-point.sum"),
			Exemplars:    liftExemplars(l, pf[7], what+".data-point.exemplars"),
		}
	}

	return Histogram{
		DataPoints:  out,
		StartTime:   l.datetime(f[1], what+".start-time"),
		Time:        l.datetime(f[2], what+".time"),
		Temporality: Temporality(l.u32(f[3], what+".temporality")),
	}
}

func liftExponentialHistogram(l *lifter, v component.Value) ExponentialHistogram {
	const what = "exponential-histogram"
	f := l.fields(v, 4, what)

	points := l.list(f[0], what+".data-points")
	out := make([]ExponentialHistogramDataPoint, len(points))
	for i, p := range points {
		pf := l.fields(p, 11, what+".data-point")
		out[i] = ExponentialHistogramDataPoint{
			Attributes:     l.keyValues(pf[0], what+".data-point.attributes"),
			Count:          l.u64(pf[1], what+".data-point.count"),
			Min:            liftOptNumber(l, pf[2], what+".data-point.min"),
			Max:            liftOptNumber(l, pf[3], what+".data-point.max"),
			Sum:            liftNumber(l, pf[4], what+".data-point.sum"),
			Scale:          int8(l.s32(pf[5], what+".data-point.scale")),
			ZeroCount:      l.u64(pf[6], what+".data-point.zero-count"),
			PositiveBucket: liftExponentialBucket(l, pf[7], what+".positive-bucket"),
			NegativeBucket: liftExponentialBucket(l, pf[8], what+".negative-bucket"),
			ZeroThreshold:  l.f64(pf[9], what+".data-point.zero-threshold"),
			Exemplars:      liftExemplars(l, pf[10], what+".data-point.exemplars"),
		}
	}

	return ExponentialHistogram{
		DataPoints:  out,
		StartTime:   l.datetime(f[1], what+".start-time"),
		Time:        l.datetime(f[2], what+".time"),
		Temporality: Temporality(l.u32(f[3], what+".temporality")),
	}
}

func liftExponentialBucket(l *lifter, v component.Value, what string) ExponentialBucket {
	f := l.fields(v, 2, what)
	return ExponentialBucket{
		Offset: l.s32(f[0], what+".offset"),
		Counts: l.u64s(f[1], what+".counts"),
	}
}
