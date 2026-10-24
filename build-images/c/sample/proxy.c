// A wasi:http/proxy component: answers every request with 200 and a fixed body.
// gen/ is produced by `wit-bindgen c --out-dir gen wit` (see build.sh).
#include <string.h>

#include "gen/proxy.h"

void exports_wasi_http_incoming_handler_handle(
	exports_wasi_http_incoming_handler_own_incoming_request_t request,
	exports_wasi_http_incoming_handler_own_response_outparam_t response_out) {
	wasi_http_types_incoming_request_drop_own(request);

	wasi_http_types_own_outgoing_response_t response =
		wasi_http_types_constructor_outgoing_response(wasi_http_types_constructor_fields());
	wasi_http_types_own_outgoing_body_t body;
	wasi_http_types_method_outgoing_response_body(
		wasi_http_types_borrow_outgoing_response(response), &body);

	wasi_http_types_result_own_outgoing_response_error_code_t result = {
		.is_err = false,
		.val = {.ok = response},
	};
	wasi_http_types_static_response_outparam_set(response_out, &result);

	wasi_http_types_own_output_stream_t out;
	wasi_http_types_method_outgoing_body_write(wasi_http_types_borrow_outgoing_body(body), &out);
	const char *msg = "hello from c\n";
	proxy_list_u8_t contents = {.ptr = (uint8_t *)msg, .len = strlen(msg)};
	wasi_io_streams_stream_error_t err;
	wasi_io_streams_method_output_stream_blocking_write_and_flush(
		wasi_io_streams_borrow_output_stream(out), &contents, &err);
	wasi_io_streams_output_stream_drop_own(out);

	wasi_http_types_error_code_t finish_err;
	wasi_http_types_static_outgoing_body_finish(body, NULL, &finish_err);
}
