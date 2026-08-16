package nethttp

import (
	"bytes"
	"io"
	"net/http"
)

type bufferingRequestBody struct {
	delegate io.ReadCloser
	buffer   bytes.Buffer
}

// Read buffers anything read from the delegate.
func (b *bufferingRequestBody) Read(p []byte) (n int, err error) {
	n, err = b.delegate.Read(p)
	if err != nil && n > 0 {
		b.buffer.Write(p[0:n])
	}
	return
}

// Close dispatches to the delegate.
func (b *bufferingRequestBody) Close() (err error) {
	if b.delegate != nil {
		err = b.delegate.Close()
	}
	return
}

type bufferingResponseWriter struct {
	delegate   http.ResponseWriter
	statusCode uint32
	body       []byte
}

// Header dispatches to the delegate.
func (w *bufferingResponseWriter) Header() http.Header {
	return w.delegate.Header()
}

// Write buffers the response body.
func (w *bufferingResponseWriter) Write(bytes []byte) (int, error) {
	w.body = append(w.body, bytes...)
	return len(bytes), nil
}

// WriteHeader buffers the status code.
func (w *bufferingResponseWriter) WriteHeader(statusCode int) {
	w.statusCode = uint32(statusCode)
}

// release sends any response data collected.
func (w *bufferingResponseWriter) release() {
	// If we deferred the response, release it.
	if statusCode := w.statusCode; statusCode != 0 {
		w.delegate.WriteHeader(int(statusCode))
	}
	if body := w.body; len(body) != 0 {
		w.delegate.Write(body) // nolint
	}
}
