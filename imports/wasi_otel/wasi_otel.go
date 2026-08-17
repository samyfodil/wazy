// Package wasi_otel implements the host side of the wasi:otel proposal, so a
// component instrumented with OpenTelemetry reports its spans, logs and
// metrics to the embedder rather than needing a collector of its own.
//
// The proposal (https://github.com/WebAssembly/wasi-otel) inverts the usual
// arrangement: rather than the guest exporting telemetry over the network, it
// imports the three interfaces below and hands the host what it recorded.
// The host decides what that means -- forward it to a collector, fold it into
// the embedder's own traces, or drop it.
//
//	wasi:otel/tracing   on-start, on-end, current-span-context
//	wasi:otel/logs      on-emit
//	wasi:otel/metrics   export
//
// Implement whichever of [Tracer], [LogEmitter] and [MetricExporter] you want
// to serve, and pass the result of [Options] to component.Instantiate:
//
//	opts := wasi_otel.Options(myCollector)
//	inst, err := component.Instantiate(ctx, r, guestWasm, opts...)
//
// A guest importing an interface nobody serves fails to link, which is the
// same as any other unimplemented import.
//
// # Attributes are JSON
//
// WIT cannot express OpenTelemetry's recursive AnyValue, so the proposal
// carries attribute values as JSON text, with byte arrays base64-encoded
// behind a `data:application/octet-stream;base64,` prefix. This package passes
// that text through untouched; see [KeyValue].
//
// # Version
//
// Written against wasi:otel@0.2.0-rc.2. Interface matching ignores the version
// suffix, so this serves any release whose ABI is unchanged. The proposal is
// at Phase 0 and its types can still move; when they do, the signatures here
// move with them.
package wasi_otel

import (
	"fmt"

	"github.com/samyfodil/wazy/component"
)

// Options returns the component options for whichever wasi:otel interfaces h
// implements.
//
// A handler implementing all three of [Tracer], [LogEmitter] and
// [MetricExporter] serves all three; one implementing none returns no options,
// which leaves a guest's otel imports unresolved. To be explicit about which
// interfaces you serve -- or to serve them from different objects -- use
// [TracingOptions], [LogsOptions] and [MetricsOptions] directly.
func Options(h any) []component.Option {
	var opts []component.Option
	if t, ok := h.(Tracer); ok {
		opts = append(opts, TracingOptions(t)...)
	}
	if e, ok := h.(LogEmitter); ok {
		opts = append(opts, LogsOptions(e)...)
	}
	if m, ok := h.(MetricExporter); ok {
		opts = append(opts, MetricsOptions(m)...)
	}
	return opts
}

// errArgs reports a call arriving with an argument count the signature does
// not allow. It means the engine and the declared FuncDesc disagree, so it is
// a bug here rather than anything a guest chose.
func errArgs(name string, want, got int) error {
	return fmt.Errorf("wasi:otel %s: expected %d args, got %d", name, want, got)
}
