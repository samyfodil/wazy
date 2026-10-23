# A wasi:http/proxy component: answers every request with 200 and a fixed body.
from componentize_py_types import Ok
from wit_world import exports
from wit_world.imports.types import (
    Fields,
    IncomingRequest,
    OutgoingBody,
    OutgoingResponse,
    ResponseOutparam,
)


class IncomingHandler(exports.IncomingHandler):
    def handle(self, request: IncomingRequest, response_out: ResponseOutparam) -> None:
        response = OutgoingResponse(Fields())
        body = response.body()
        ResponseOutparam.set(response_out, Ok(response))
        with body.write() as out:
            out.blocking_write_and_flush(b"hello from python\n")
        OutgoingBody.finish(body, None)
