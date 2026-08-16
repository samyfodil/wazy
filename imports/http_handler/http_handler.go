// Package http_handler runs guests built against the http-wasm handler ABI,
// the middleware ABI used by Traefik's and dapr's WebAssembly plugin systems.
// A guest compiled against any http-wasm SDK - Go/TinyGo, Rust, C,
// AssemblyScript - runs unmodified.
//
// The ABI is a host module named "http_handler" holding the functions below,
// plus a guest contract: exports FuncHandleRequest, FuncHandleResponse and a
// memory named "memory".
//
// Use the nethttp subpackage to wrap a guest as a net/http.Handler. This
// package is the engine underneath: it registers the host functions, compiles
// and pools guest instances, and calls the guest around a Host implementation.
//
// # Attribution
//
// This is a port of github.com/http-wasm/http-wasm-host-go (Apache 2.0),
// which is hard-typed to wazero. Wire format, function semantics, the Host
// interface and the doc comments below come from there. What changed is the
// runtime plumbing: wazy.Runtime, wazy.ModuleConfig and wazy's own
// imports/wasi_snapshot_preview1.
//
// See https://github.com/http-wasm/http-wasm-abi/blob/main/http_handler/http_handler.wit.md
package http_handler

// Memory is the name of the memory a guest must export.
const Memory = "memory"

// CtxNext is the result of FuncHandleRequest. For compatability with
// WebAssembly Core Specification 1.0, two uint32 values are combined into a
// single uint64 in the following order:
//
//   - ctx: opaque 32-bits the guest defines and the host propagates to
//     FuncHandleResponse. A typical use is correlation of request state.
//   - next: one to proceed to the next handler on the host. zero to skip any
//     next handler. Guests skip when they wrote a response or decided not to.
//
// When the guest decides to proceed to the next handler, it can return
// `ctxNext=1` which is the same as `next=1` without any request context. If it
// wants the host to propagate request context, it shifts that into the upper
// 32-bits of ctxNext like so:
//
//	ctxNext = uint64(reqCtx) << 32
//	if next {
//		ctxNext |= 1
//	}
//
// # Examples
//
//   - 0<<32|0  (0): don't proceed to the next handler.
//   - 0<<32|1  (1): proceed to the next handler without context state.
//   - 16<<32|1 (68719476737): proceed to the next handler and call
//     FuncHandleResponse with 16.
//   - 16<<32|16 (68719476736): the value 16 is ignored because
//     FuncHandleResponse won't be called.
type CtxNext uint64

// BufLimit is the possibly zero maximum length of a result value to write in
// bytes. If the actual value is larger than this, nothing is written to
// memory.
type BufLimit = uint32

// CountLen describes a possible empty sequence of NUL-terminated strings. For
// compatability with WebAssembly Core Specification 1.0, two uint32 values are
// combined into a single uint64 in the following order:
//
//   - count: zero if the sequence is empty, or the count of strings.
//   - len: possibly zero length of the sequence, including NUL-terminators.
//
// If the uint64 result is zero, the sequence is empty. Otherwise, you need to
// split the results like so.
//
//   - count: `uint32(countLen >> 32)`
//   - len: `uint32(countLen)`
//
// # Examples
//
//   - "": 0<<32|0 or simply zero.
//   - "Accept\0": 1<<32|7
//   - "Content-Type\0Content-Length\0": 2<<32|28
type CountLen = uint64

// EOFLen is the result of FuncReadBody which allows callers to know if the
// bytes returned are the end of the stream. For compatability with WebAssembly
// Core Specification 1.0, two uint32 values are combined into a single uint64
// in the following order:
//
//   - eof: the body is exhausted.
//   - len: possibly zero length of bytes read from the body.
//
// Here's how to split the results:
//
//   - eof: `uint32(eofLen >> 32)`
//   - len: `uint32(eofLen)`
//
// # Examples
//
//   - 1<<32|0 (4294967296): EOF and no bytes were read
//   - 0<<32|16 (16): 16 bytes were read and there may be more available.
//
// Note: `EOF` is not an error, so process `len` bytes returned regardless.
type EOFLen = uint64

type BodyKind uint32

const (
	// BodyKindRequest represents an operation on an HTTP request body.
	//
	// # Notes on FuncReadBody
	//
	// FeatureBufferResponse is required to read the request body without
	// consuming it. To enable it, call FuncEnableFeatures before FuncNext.
	// Otherwise, a downstream handler may panic attempting to read a request
	// body already read upstream.
	//
	// # Notes on FuncWriteBody
	//
	// The first call to FuncWriteBody in FuncHandleRequest overwrites any
	// request body.
	BodyKindRequest BodyKind = 0

	// BodyKindResponse represents an operation on an HTTP request body.
	//
	// # Notes on FuncReadBody
	//
	// FeatureBufferResponse is required to read the response body produced by
	// FuncNext. To enable it, call FuncEnableFeatures beforehand. Otherwise,
	// a handler may panic calling FuncReadBody with BodyKindResponse.
	//
	// # Notes on FuncWriteBody
	//
	// The first call to FuncWriteBody in FuncHandleRequest or after FuncNext
	// overwrites any response body.
	BodyKindResponse BodyKind = 1
)

type HeaderKind uint32

const (
	// HeaderKindRequest represents an operation on HTTP request headers.
	HeaderKindRequest HeaderKind = 0

	// HeaderKindResponse represents an operation on HTTP response headers.
	HeaderKindResponse HeaderKind = 1

	// HeaderKindRequestTrailers represents an operation on HTTP request
	// trailers (trailing headers). This requires FeatureTrailers.
	//
	// To enable FeatureTrailers, call FuncEnableFeatures prior to FuncNext.
	// Doing otherwise, may result in a panic.
	HeaderKindRequestTrailers HeaderKind = 2

	// HeaderKindResponseTrailers represents an operation on HTTP response
	// trailers (trailing headers). This requires FeatureTrailers.
	//
	// To enable FeatureTrailers, call FuncEnableFeatures prior to FuncNext.
	// Doing otherwise, may result in a panic.
	HeaderKindResponseTrailers HeaderKind = 3
)

const (
	// HostModule is the WebAssembly module name of the ABI this middleware
	// implements.
	//
	// Note: This is lower-hyphen case while functions are lower_underscore to
	// follow conventions of wit-bindgen, which retains the case format of the
	// interface filename as the module name, but converts function and
	// parameter names to lower_underscore format.
	HostModule = "http_handler"

	// FuncEnableFeatures tries to enable the given features and returns the
	// Features bitflag supported by the host. To have any affect, this must be
	// called prior to returning from FuncHandleRequest.
	//
	// This may be called prior to FuncHandleRequest, for example inside a
	// start function. Doing so reduces overhead per-call and also allows the
	// guest to fail early on unsupported.
	//
	// If called during FuncHandleRequest, any new features are enabled for the
	// scope of the current request. This allows fine-grained access to
	// expensive features such as FeatureBufferResponse.
	FuncEnableFeatures = "enable_features"

	// FuncGetConfig writes configuration from the host to memory if it exists
	// and isn't larger than BufLimit. The result is its length in bytes.
	//
	// Note: Configuration is determined by the guest and is not necessarily
	// UTF-8 encoded.
	FuncGetConfig = "get_config"

	// FuncLogEnabled returns 1 if the LogLevel is enabled. This value may be
	// cached at request granularity.
	//
	// This function is used to avoid unnecessary overhead generating log
	// messages that the host would discard due to its level being below this.
	FuncLogEnabled = "log_enabled"

	// FuncLog logs a message to the host's logs at the given LogLevel.
	FuncLog = "log"

	// FuncHandleRequest is the entrypoint guest export called by the host when
	// processing a request.
	//
	// To proceed to the next handler, the guest returns CtxNext `next=1`. The
	// simplest result is uint64(1). Guests who need request correlation can
	// also set the CtxNext "ctx" field. This will be propagated by the host as
	// the `reqCtx` parameter of FuncHandleResponse.
	//
	// To skip any next handler, the guest returns CtxNext `next=0`, or simply
	// uint64(0). In this case, FuncHandleResponse will not be called.
	FuncHandleRequest = "handle_request"

	// FuncHandleResponse is called by the host after processing the next
	// handler when the guest returns CtxNext `next=1` from FuncHandleRequest.
	//
	// The `reqCtx` parameter is a possibly zero CtxNext "ctx" field the host
	// propagated from FuncHandleRequest. This allows request correlation for
	// guests who need it.
	//
	// The `isError` parameter is one if there was a host error producing a
	// response. This allows guests to clean up any resources.
	//
	// By default, whether the next handler flushes the response prior to
	// returning is implementation-specific. If your handler needs to inspect
	// or manipulate the downstream response, enable FeatureBufferResponse via
	// FuncEnableFeatures prior to returning from FuncHandleRequest.
	FuncHandleResponse = "handle_response"

	// FuncGetMethod writes the method to memory if it isn't larger than
	// BufLimit. The result is its length in bytes. Ex. "GET"
	FuncGetMethod = "get_method"

	// FuncSetMethod overwrites the method with one read from memory.
	FuncSetMethod = "set_method"

	// FuncGetURI writes the URI to memory if it isn't larger than BufLimit.
	// The result is its length in bytes. Ex. "/v1.0/hi?name=panda"
	//
	// Note: The URI may include query parameters.
	FuncGetURI = "get_uri"

	// FuncSetURI overwrites the URI with one read from memory.
	//
	// Note: The URI may include query parameters.
	FuncSetURI = "set_uri"

	// FuncGetProtocolVersion writes the HTTP protocol version to memory if it
	// isn't larger than BufLimit. The result is its length in bytes.
	// Ex. "HTTP/1.1"
	//
	// See https://www.rfc-editor.org/rfc/rfc9110#name-protocol-version
	FuncGetProtocolVersion = "get_protocol_version"

	// FuncGetHeaderNames writes all names for the given HeaderKind,
	// NUL-terminated, to memory if the encoded length isn't larger than
	// BufLimit. CountLen is returned regardless of whether memory was written.
	FuncGetHeaderNames = "get_header_names"

	// FuncGetHeaderValues writes all values of the given HeaderKind and name,
	// NUL-terminated, to memory if the encoded length isn't larger than
	// BufLimit. CountLen is returned regardless of whether memory was written.
	FuncGetHeaderValues = "get_header_values"

	// FuncSetHeaderValue overwrites all values of the given HeaderKind and
	// name with the input.
	FuncSetHeaderValue = "set_header_value"

	// FuncAddHeaderValue adds a single value for the given HeaderKind and
	// name.
	FuncAddHeaderValue = "add_header_value"

	// FuncRemoveHeader removes any values for the given HeaderKind and name.
	FuncRemoveHeader = "remove_header"

	// FuncReadBody reads up to BufLimit bytes remaining in the BodyKind body
	// into memory at offset `buf`. A zero BufLimit will panic.
	//
	// The result is EOFLen, indicating the count of bytes read and whether
	// there may be more available.
	//
	// Unlike `get_XXX` functions, this function is stateful, so repeated calls
	// reads what's remaining in the stream, as opposed to starting from zero.
	// Callers do not have to exhaust the stream until `EOF`.
	FuncReadBody = "read_body"

	// FuncWriteBody reads `buf_len` bytes at memory offset `buf` and writes
	// them to the pending BodyKind body.
	//
	// Unlike `set_XXX` functions, this function is stateful, so repeated calls
	// write to the current stream.
	FuncWriteBody = "write_body"

	// FuncGetStatusCode returns the status code produced by FuncNext. This
	// requires FeatureBufferResponse.
	//
	// To enable FeatureBufferResponse, FuncEnableFeatures prior to FuncNext.
	// Doing otherwise, or calling before FuncNext will panic.
	FuncGetStatusCode = "get_status_code"

	// FuncSetStatusCode overrides the status code. The default is 200.
	//
	// To use this function after FuncNext, set FeatureBufferResponse via
	// FuncEnableFeatures. Otherwise, this can be called when FuncNext wasn't.
	FuncSetStatusCode = "set_status_code"

	// FuncGetSourceAddr writes the source address to memory if it isn't larger
	// than BufLimit. The result is its length in bytes.
	// Ex. "1.1.1.1:12345" or "[fe80::101e:2bdf:8bfb:b97e]:12345"
	FuncGetSourceAddr = "get_source_addr"
)
