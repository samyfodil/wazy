// A wasi:http/proxy component: answers every request with 200 and a fixed body.
import {
  Fields,
  OutgoingBody,
  OutgoingResponse,
  ResponseOutparam,
} from "wasi:http/types@0.2.12";

export const incomingHandler = {
  handle(request, responseOut) {
    const response = new OutgoingResponse(new Fields());
    const body = response.body();
    ResponseOutparam.set(responseOut, { tag: "ok", val: response });
    const out = body.write();
    out.blockingWriteAndFlush(new TextEncoder().encode("hello from js\n"));
    out[Symbol.dispose]();
    OutgoingBody.finish(body, undefined);
  },
};
