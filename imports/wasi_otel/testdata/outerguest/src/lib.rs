// A guest importing the tracing interface under the name the guest SDKs still
// generate, `outer-span-context`, taken from bytecodealliance/opentelemetry-wasi's
// own wit/. It exists to prove the host serves that name as well as the
// proposal's current `current-span-context`.
//
// Build: cargo build --release --target wasm32-wasip2

wit_bindgen::generate!({
    path: "wit",
    world: "outerguest",
    generate_all,
});

use wasi::otel::tracing;

struct Component;

impl Guest for Component {
    fn run() -> String {
        let sc = tracing::outer_span_context();
        format!(
            "{} sampled={} state={}",
            sc.trace_id,
            sc.trace_flags.contains(tracing::TraceFlags::SAMPLED),
            sc.trace_state.len(),
        )
    }
}

export!(Component);
