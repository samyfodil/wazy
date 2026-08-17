// A wasi:otel guest built with real wit-bindgen against the proposal's own
// WIT (wit/, copied from WebAssembly/wasi-otel).
//
// It exists to prove the host implementation against generated bindings rather
// than against hand-written bytes: what it sends is what wit-bindgen's lowering
// produces, so the host's declared type table has to agree with the real ABI or
// nothing links.
//
// The data is deliberately awkward -- options that are none beside options that
// are some, every metric-number kind, four aggregations, a status carrying a
// payload -- because the uniform cases are the ones that would pass anyway.
//
// Build: cargo build --release --target wasm32-wasip2

wit_bindgen::generate!({
    path: "wit",
    world: "otelguest",
    // The clocks types are only reached through wasi:otel, so generate them
    // here rather than pulling in a wasi crate for two integer fields.
    generate_all,
});

use wasi::clocks::wall_clock::Datetime;
use wasi::otel::logs;
use wasi::otel::metrics;
use wasi::otel::tracing;
use wasi::otel::types::{InstrumentationScope, KeyValue, Resource};

struct Component;

fn dt(seconds: u64, nanoseconds: u32) -> Datetime {
    Datetime {
        seconds,
        nanoseconds,
    }
}

fn kv(key: &str, value: &str) -> KeyValue {
    KeyValue {
        key: key.to_string(),
        value: value.to_string(),
    }
}

fn scope() -> InstrumentationScope {
    InstrumentationScope {
        name: "otelguest".to_string(),
        version: Some("0.1.0".to_string()),
        // Left none on purpose: the host has to tell it apart from the
        // version above, which is some.
        schema_url: None,
        attributes: vec![kv("scope.attr", "\"yes\"")],
    }
}

fn resource() -> Resource {
    Resource {
        attributes: vec![kv("service.name", "\"otelguest\"")],
        schema_url: Some("https://example.invalid/schema".to_string()),
    }
}

fn span_context() -> tracing::SpanContext {
    tracing::SpanContext {
        trace_id: "0123456789abcdef0123456789abcdef".to_string(),
        span_id: "0123456789abcdef".to_string(),
        trace_flags: tracing::TraceFlags::SAMPLED,
        is_remote: false,
        // Two entries, so a list of tuples is exercised rather than an empty one.
        trace_state: vec![
            ("vendor-a".to_string(), "value-a".to_string()),
            ("vendor-b".to_string(), "value-b".to_string()),
        ],
    }
}

impl Guest for Component {
    fn run() -> String {
        // --- tracing ----------------------------------------------------
        tracing::on_start(&span_context());

        let span = tracing::SpanData {
            span_context: span_context(),
            parent_span_id: "fedcba9876543210".to_string(),
            span_kind: tracing::SpanKind::Server,
            name: "GET /things".to_string(),
            start_time: dt(1_700_000_000, 250),
            end_time: dt(1_700_000_001, 500),
            attributes: vec![kv("http.method", "\"GET\""), kv("http.status", "200")],
            events: vec![tracing::Event {
                name: "cache.miss".to_string(),
                time: dt(1_700_000_000, 750),
                attributes: vec![kv("cache.key", "\"things\"")],
            }],
            links: vec![tracing::Link {
                span_context: span_context(),
                attributes: vec![kv("link.kind", "\"follows\"")],
            }],
            // The one status arm carrying a payload.
            status: tracing::Status::Error("boom".to_string()),
            instrumentation_scope: scope(),
            dropped_attributes: 1,
            dropped_events: 2,
            dropped_links: 3,
        };
        tracing::on_end(&span);

        let current = tracing::current_span_context();

        // --- logs -------------------------------------------------------
        //
        // Every optional field is set, so the host proves it can read them.
        logs::on_emit(&logs::LogRecord {
            timestamp: Some(dt(1_700_000_002, 0)),
            observed_timestamp: Some(dt(1_700_000_003, 0)),
            severity_text: Some("WARN".to_string()),
            severity_number: Some(13),
            body: Some("\"disk almost full\"".to_string()),
            attributes: Some(vec![kv("disk.pct", "91")]),
            event_name: Some("disk.usage".to_string()),
            resource: Some(resource()),
            instrumentation_scope: Some(scope()),
            trace_id: Some("0123456789abcdef0123456789abcdef".to_string()),
            span_id: Some("0123456789abcdef".to_string()),
            trace_flags: Some(tracing::TraceFlags::SAMPLED),
        });

        // The same record with every option none, which is the case a host
        // that only ever sees populated logs gets wrong.
        logs::on_emit(&logs::LogRecord {
            timestamp: None,
            observed_timestamp: None,
            severity_text: None,
            severity_number: None,
            body: None,
            attributes: None,
            event_name: None,
            resource: None,
            instrumentation_scope: None,
            trace_id: None,
            span_id: None,
            trace_flags: None,
        });

        // --- metrics ----------------------------------------------------
        let exemplar = metrics::Exemplar {
            filtered_attributes: vec![kv("sampled", "true")],
            time: dt(1_700_000_004, 0),
            value: metrics::MetricNumber::F64(1.5),
            span_id: "0123456789abcdef".to_string(),
            trace_id: "0123456789abcdef0123456789abcdef".to_string(),
        };

        let gauge = metrics::Gauge {
            data_points: vec![metrics::GaugeDataPoint {
                attributes: vec![kv("host", "\"a\"")],
                value: metrics::MetricNumber::F64(42.5),
                exemplars: vec![exemplar.clone()],
            }],
            start_time: None, // option<datetime>, the none arm
            time: dt(1_700_000_005, 0),
        };

        let sum = metrics::Sum {
            data_points: vec![metrics::SumDataPoint {
                attributes: vec![kv("host", "\"b\"")],
                // A different number kind from the gauge above.
                value: metrics::MetricNumber::U64(7),
                exemplars: vec![],
            }],
            start_time: dt(1_700_000_006, 0),
            time: dt(1_700_000_007, 0),
            temporality: metrics::Temporality::Delta,
            is_monotonic: true,
        };

        let histogram = metrics::Histogram {
            data_points: vec![metrics::HistogramDataPoint {
                attributes: vec![],
                count: 3,
                bounds: vec![1.0, 5.0, 10.0],
                bucket_counts: vec![1, 1, 1, 0],
                min: Some(metrics::MetricNumber::S64(-2)),
                max: None, // one option some, one none, in one record
                sum: metrics::MetricNumber::S64(9),
                exemplars: vec![],
            }],
            start_time: dt(1_700_000_008, 0),
            time: dt(1_700_000_009, 0),
            temporality: metrics::Temporality::Cumulative,
        };

        let exp_histogram = metrics::ExponentialHistogram {
            data_points: vec![metrics::ExponentialHistogramDataPoint {
                attributes: vec![],
                count: 4,
                min: None,
                max: None,
                sum: metrics::MetricNumber::F64(12.25),
                scale: -3, // negative, so a signed 8-bit field is proven
                zero_count: 1,
                positive_bucket: metrics::ExponentialBucket {
                    offset: 2,
                    counts: vec![1, 2],
                },
                negative_bucket: metrics::ExponentialBucket {
                    offset: -4, // negative offset, an s32
                    counts: vec![1],
                },
                zero_threshold: 0.5,
                exemplars: vec![],
            }],
            start_time: dt(1_700_000_010, 0),
            time: dt(1_700_000_011, 0),
            temporality: metrics::Temporality::LowMemory,
        };

        let metrics_list = vec![
            metrics::Metric {
                name: "temperature".to_string(),
                description: "current temperature".to_string(),
                unit: "Cel".to_string(),
                data: metrics::MetricData::F64Gauge(gauge),
            },
            metrics::Metric {
                name: "requests".to_string(),
                description: "total requests".to_string(),
                unit: "1".to_string(),
                data: metrics::MetricData::U64Sum(sum),
            },
            metrics::Metric {
                name: "latency".to_string(),
                description: "request latency".to_string(),
                unit: "ms".to_string(),
                data: metrics::MetricData::S64Histogram(histogram),
            },
            metrics::Metric {
                name: "payload".to_string(),
                description: "payload size".to_string(),
                unit: "By".to_string(),
                data: metrics::MetricData::F64ExponentialHistogram(exp_histogram),
            },
        ];

        let export_result = metrics::export(&metrics::ResourceMetrics {
            resource: resource(),
            scope_metrics: vec![metrics::ScopeMetrics {
                scope: scope(),
                metrics: metrics_list,
            }],
        });

        let exported = match export_result {
            Ok(()) => "ok".to_string(),
            Err(e) => format!("err({e})"),
        };

        // What the host sent back, so the test can see the return path worked
        // as well as the argument path.
        format!(
            "current={} sampled={} state={} export={}",
            current.trace_id,
            current.trace_flags.contains(tracing::TraceFlags::SAMPLED),
            current.trace_state.len(),
            exported,
        )
    }
}

export!(Component);
