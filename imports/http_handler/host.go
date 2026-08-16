package http_handler

import (
	"context"
	"io"
)

// Host supports the host side of the WebAssembly module named HostModule.
// These callbacks are used by the guest function export FuncHandleRequest.
//
// The nethttp subpackage implements this over net/http. Implement it yourself
// to drive guests from another server abstraction.
type Host interface {
	// EnableFeatures supports the WebAssembly function export
	// FuncEnableFeatures.
	EnableFeatures(ctx context.Context, features Features) Features

	// GetMethod supports the WebAssembly function export FuncGetMethod.
	GetMethod(ctx context.Context) string

	// SetMethod supports the WebAssembly function export FuncSetMethod.
	SetMethod(ctx context.Context, method string)

	// GetURI supports the WebAssembly function export FuncGetURI.
	GetURI(ctx context.Context) string

	// SetURI supports the WebAssembly function export FuncSetURI.
	SetURI(ctx context.Context, uri string)

	// GetProtocolVersion supports the WebAssembly function export
	// FuncGetProtocolVersion.
	GetProtocolVersion(ctx context.Context) string

	// GetRequestHeaderNames supports the WebAssembly function export
	// FuncGetHeaderNames with HeaderKindRequest. This returns nil if no
	// headers exist.
	GetRequestHeaderNames(ctx context.Context) []string

	// GetRequestHeaderValues supports the WebAssembly function export
	// FuncGetHeaderValues with HeaderKindRequest. This returns nil if no
	// values exist.
	GetRequestHeaderValues(ctx context.Context, name string) []string

	// SetRequestHeaderValue supports the WebAssembly function export
	// FuncSetHeaderValue with HeaderKindRequest.
	SetRequestHeaderValue(ctx context.Context, name, value string)

	// AddRequestHeaderValue supports the WebAssembly function export
	// FuncAddHeaderValue with HeaderKindRequest.
	AddRequestHeaderValue(ctx context.Context, name, value string)

	// RemoveRequestHeader supports the WebAssembly function export
	// FuncRemoveHeader with HeaderKindRequest.
	RemoveRequestHeader(ctx context.Context, name string)

	// RequestBodyReader supports the WebAssembly function export FuncReadBody
	// with BodyKindRequest.
	RequestBodyReader(ctx context.Context) io.ReadCloser

	// RequestBodyWriter supports the WebAssembly function export FuncWriteBody
	// with BodyKindRequest.
	RequestBodyWriter(ctx context.Context) io.Writer

	// GetRequestTrailerNames supports the WebAssembly function export
	// FuncGetHeaderNames with HeaderKindRequestTrailers. This returns nil if
	// no trailers exist or FeatureTrailers is not supported.
	GetRequestTrailerNames(ctx context.Context) []string

	// GetRequestTrailerValues supports the WebAssembly function export
	// FuncGetHeaderValues with HeaderKindRequestTrailers. This returns nil if
	// no values exist or FeatureTrailers is not supported.
	GetRequestTrailerValues(ctx context.Context, name string) []string

	// SetRequestTrailerValue supports the WebAssembly function export
	// FuncSetHeaderValue with HeaderKindRequestTrailers. This panics if
	// FeatureTrailers is not supported.
	SetRequestTrailerValue(ctx context.Context, name, value string)

	// AddRequestTrailerValue supports the WebAssembly function export
	// FuncAddHeaderValue with HeaderKindRequestTrailers. This panics if
	// FeatureTrailers is not supported.
	AddRequestTrailerValue(ctx context.Context, name, value string)

	// RemoveRequestTrailer supports the WebAssembly function export
	// FuncRemoveHeader with HeaderKindRequestTrailers. This panics if
	// FeatureTrailers is not supported.
	RemoveRequestTrailer(ctx context.Context, name string)

	// GetStatusCode supports the WebAssembly function export
	// FuncGetStatusCode.
	GetStatusCode(ctx context.Context) uint32

	// SetStatusCode supports the WebAssembly function export
	// FuncSetStatusCode.
	SetStatusCode(ctx context.Context, statusCode uint32)

	// GetResponseHeaderNames supports the WebAssembly function export
	// FuncGetHeaderNames with HeaderKindResponse. This returns nil if no
	// headers exist.
	GetResponseHeaderNames(ctx context.Context) []string

	// GetResponseHeaderValues supports the WebAssembly function export
	// FuncGetHeaderValues with HeaderKindResponse. This returns nil if no
	// values exist.
	GetResponseHeaderValues(ctx context.Context, name string) []string

	// SetResponseHeaderValue supports the WebAssembly function export
	// FuncSetHeaderValue with HeaderKindResponse.
	SetResponseHeaderValue(ctx context.Context, name, value string)

	// AddResponseHeaderValue supports the WebAssembly function export
	// FuncAddHeaderValue with HeaderKindResponse.
	AddResponseHeaderValue(ctx context.Context, name, value string)

	// RemoveResponseHeader supports the WebAssembly function export
	// FuncRemoveHeader with HeaderKindResponse.
	RemoveResponseHeader(ctx context.Context, name string)

	// ResponseBodyReader supports the WebAssembly function export FuncReadBody
	// with BodyKindResponse.
	ResponseBodyReader(ctx context.Context) io.ReadCloser

	// ResponseBodyWriter supports the WebAssembly function export
	// FuncWriteBody with BodyKindResponse.
	ResponseBodyWriter(ctx context.Context) io.Writer

	// GetResponseTrailerNames supports the WebAssembly function export
	// FuncGetHeaderNames with HeaderKindResponseTrailers. This returns nil if
	// no trailers exist or FeatureTrailers is not supported.
	GetResponseTrailerNames(ctx context.Context) []string

	// GetResponseTrailerValues supports the WebAssembly function export
	// FuncGetHeaderValues with HeaderKindResponseTrailers. This returns nil if
	// no values exist or FeatureTrailers is not supported.
	GetResponseTrailerValues(ctx context.Context, name string) []string

	// SetResponseTrailerValue supports the WebAssembly function export
	// FuncSetHeaderValue with HeaderKindResponseTrailers. This panics if
	// FeatureTrailers is not supported.
	SetResponseTrailerValue(ctx context.Context, name, value string)

	// AddResponseTrailerValue supports the WebAssembly function export
	// FuncAddHeaderValue with HeaderKindResponseTrailers. This panics if
	// FeatureTrailers is not supported.
	AddResponseTrailerValue(ctx context.Context, name, value string)

	// RemoveResponseTrailer supports the WebAssembly function export
	// FuncRemoveHeader with HeaderKindResponseTrailers. This panics if
	// FeatureTrailers is not supported.
	RemoveResponseTrailer(ctx context.Context, name string)

	// GetSourceAddr supports the WebAssembly function export
	// FuncGetSourceAddr.
	GetSourceAddr(ctx context.Context) string
}

// eofReader is safer than reading from os.DevNull as it can never overrun
// operating system file descriptors.
type eofReader struct{}

func (eofReader) Close() (err error)       { return }
func (eofReader) Read([]byte) (int, error) { return 0, io.EOF }

// UnimplementedHost is a Host with defaults, to embed in an implementation
// that only needs some of the surface.
type UnimplementedHost struct{}

var _ Host = UnimplementedHost{}

func (UnimplementedHost) EnableFeatures(context.Context, Features) Features                  { return 0 }
func (UnimplementedHost) GetMethod(context.Context) string                                   { return "GET" }
func (UnimplementedHost) SetMethod(context.Context, string)                                  {}
func (UnimplementedHost) GetURI(context.Context) string                                      { return "" }
func (UnimplementedHost) SetURI(context.Context, string)                                     {}
func (UnimplementedHost) GetProtocolVersion(context.Context) string                          { return "HTTP/1.1" }
func (UnimplementedHost) GetRequestHeaderNames(context.Context) (names []string)             { return }
func (UnimplementedHost) GetRequestHeaderValues(context.Context, string) (values []string)   { return }
func (UnimplementedHost) SetRequestHeaderValue(context.Context, string, string)              {}
func (UnimplementedHost) AddRequestHeaderValue(context.Context, string, string)              {}
func (UnimplementedHost) RemoveRequestHeader(context.Context, string)                        {}
func (UnimplementedHost) RequestBodyReader(context.Context) io.ReadCloser                    { return eofReader{} }
func (UnimplementedHost) RequestBodyWriter(context.Context) io.Writer                        { return io.Discard }
func (UnimplementedHost) GetRequestTrailerNames(context.Context) (names []string)            { return }
func (UnimplementedHost) GetRequestTrailerValues(context.Context, string) (values []string)  { return }
func (UnimplementedHost) SetRequestTrailerValue(context.Context, string, string)             {}
func (UnimplementedHost) AddRequestTrailerValue(context.Context, string, string)             {}
func (UnimplementedHost) RemoveRequestTrailer(context.Context, string)                       {}
func (UnimplementedHost) GetStatusCode(context.Context) uint32                               { return 200 }
func (UnimplementedHost) SetStatusCode(context.Context, uint32)                              {}
func (UnimplementedHost) GetResponseHeaderNames(context.Context) (names []string)            { return }
func (UnimplementedHost) GetResponseHeaderValues(context.Context, string) (values []string)  { return }
func (UnimplementedHost) SetResponseHeaderValue(context.Context, string, string)             {}
func (UnimplementedHost) AddResponseHeaderValue(context.Context, string, string)             {}
func (UnimplementedHost) RemoveResponseHeader(context.Context, string)                       {}
func (UnimplementedHost) ResponseBodyReader(context.Context) io.ReadCloser                   { return eofReader{} }
func (UnimplementedHost) ResponseBodyWriter(context.Context) io.Writer                       { return io.Discard }
func (UnimplementedHost) GetResponseTrailerNames(context.Context) (names []string)           { return }
func (UnimplementedHost) GetResponseTrailerValues(context.Context, string) (values []string) { return }
func (UnimplementedHost) SetResponseTrailerValue(context.Context, string, string)            {}
func (UnimplementedHost) AddResponseTrailerValue(context.Context, string, string)            {}
func (UnimplementedHost) RemoveResponseTrailer(context.Context, string)                      {}
func (UnimplementedHost) GetSourceAddr(context.Context) string                               { return "1.1.1.1:12345" }
