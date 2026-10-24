// A wasi:http/proxy component: answers every request with 200 and a fixed body.
// ../gen is produced by `wit-bindgen-go generate --world proxy -o gen ./wit`
// (see build.sh).
package main

import (
	incominghandler "example.com/sample/gen/wasi/http/incoming-handler"
	"example.com/sample/gen/wasi/http/types"
	"go.bytecodealliance.org/cm"
)

func init() {
	incominghandler.Exports.Handle = handle
}

func handle(request types.IncomingRequest, responseOut types.ResponseOutparam) {
	request.ResourceDrop()

	response := types.NewOutgoingResponse(types.NewFields())
	bodyResult := response.Body()
	body := bodyResult.OK()
	types.ResponseOutparamSet(responseOut, cm.OK[cm.Result[types.ErrorCodeShape, types.OutgoingResponse, types.ErrorCode]](response))

	outResult := body.Write()
	out := outResult.OK()
	out.BlockingWriteAndFlush(cm.ToList([]byte("hello from tinygo\n")))
	out.ResourceDrop()
	types.OutgoingBodyFinish(*body, cm.None[types.Trailers]())
}

func main() {}
