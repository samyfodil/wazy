package http_handler

import "strings"

// Features is a bit flag of features a host may support.
//
// Note: Numeric values are not intended to be interpreted except as bit flags.
type Features uint32

const (
	// FeatureBufferRequest buffers the HTTP request body when reading, so that
	// the next handler can see the original.
	//
	// Note: Buffering a request is done on the host and can use resources
	// such as memory. It also may reduce the features of the underlying
	// request due to implications of buffering or wrapping.
	FeatureBufferRequest Features = 1 << iota

	// FeatureBufferResponse buffers the HTTP response produced by the next
	// handler instead of sending it immediately. This allows the caller to
	// inspect and overwrite the HTTP status code, response body or trailers.
	// As the response is deferred, expect timing differences when enabled.
	//
	// Note: Buffering a response is done on the host and can use resources
	// such as memory. It also may reduce the features of the underlying
	// response due to implications of buffering or wrapping.
	FeatureBufferResponse

	// FeatureTrailers allows guests to act differently depending on if the
	// host supports HTTP trailing headers (trailers) or not.
	//
	// # Handling unsupported
	//
	// This is a feature flag because trailers are not well-supported.
	//
	// A host that doesn't support trailers must do the following:
	//   - return 0 for this bit in the FuncEnableFeatures result.
	//   - return no trailer names or values.
	//   - panic/trap on any call to set a trailer value.
	//
	// For example, a logging handler may be fine without trailers, while a
	// gRPC handler should err as it needs to access the gRPC status trailer. A
	// guest that requires trailers can fail during initialization instead of
	// per-request via inspecting the result of FuncEnableFeatures.
	//
	// See https://peps.python.org/pep-0444/#request-trailers-and-chunked-transfer-encoding
	FeatureTrailers
)

// WithEnabled enables the feature or group of features.
func (f Features) WithEnabled(feature Features) Features {
	return f | feature
}

// IsEnabled returns true if the feature (or group of features) is enabled.
func (f Features) IsEnabled(feature Features) bool {
	return f&feature != 0
}

// String implements fmt.Stringer by returning each enabled feature.
func (f Features) String() string {
	var builder strings.Builder
	for i := 0; i <= 31; i++ { // cycle through all bits to reduce code and maintenance
		target := Features(1 << i)
		if f.IsEnabled(target) {
			if name := featureName(target); name != "" {
				if builder.Len() > 0 {
					builder.WriteByte('|')
				}
				builder.WriteString(name)
			}
		}
	}
	return builder.String()
}

func featureName(f Features) string {
	switch f {
	case FeatureBufferRequest:
		return "buffer_request"
	case FeatureBufferResponse:
		return "buffer_response"
	case FeatureTrailers:
		return "trailers"
	}
	return ""
}
