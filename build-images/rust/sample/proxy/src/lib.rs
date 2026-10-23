use wasi::http::types::{Fields, IncomingRequest, OutgoingBody, OutgoingResponse, ResponseOutparam};

struct Proxy;

impl wasi::exports::http::incoming_handler::Guest for Proxy {
    fn handle(_request: IncomingRequest, response_out: ResponseOutparam) {
        let response = OutgoingResponse::new(Fields::new());
        let body = response.body().unwrap();
        ResponseOutparam::set(response_out, Ok(response));
        let out = body.write().unwrap();
        out.blocking_write_and_flush(b"hello from rust\n").unwrap();
        drop(out);
        OutgoingBody::finish(body, None).unwrap();
    }
}

wasi::http::proxy::export!(Proxy);
