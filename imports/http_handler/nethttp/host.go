package nethttp

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/textproto"
	"net/url"
	"sort"
	"strings"

	"github.com/samyfodil/wazy/imports/http_handler"
)

// host implements http_handler.Host over a net/http request and response.
type host struct{}

var _ http_handler.Host = host{}

// EnableFeatures implements the same method as documented on
// http_handler.Host.
func (host) EnableFeatures(ctx context.Context, features http_handler.Features) http_handler.Features {
	if s, ok := ctx.Value(requestStateKey{}).(*requestState); ok {
		s.enableFeatures(features)
	}
	// Otherwise, this was called during init, but there's nothing to do
	// because net/http supports all features.
	return features
}

// GetMethod implements the same method as documented on http_handler.Host.
func (host) GetMethod(ctx context.Context) string {
	r := requestStateFromContext(ctx).r
	return r.Method
}

// SetMethod implements the same method as documented on http_handler.Host.
func (host) SetMethod(ctx context.Context, method string) {
	r := requestStateFromContext(ctx).r
	r.Method = method
}

// GetURI implements the same method as documented on http_handler.Host.
func (host) GetURI(ctx context.Context) string {
	r := requestStateFromContext(ctx).r
	u := r.URL
	result := u.EscapedPath()
	if result == "" {
		result = "/"
	}
	if u.ForceQuery || u.RawQuery != "" {
		result += "?" + u.RawQuery
	}
	return result
}

// SetURI implements the same method as documented on http_handler.Host.
func (host) SetURI(ctx context.Context, uri string) {
	r := requestStateFromContext(ctx).r
	if uri == "" { // url.ParseRequestURI fails on empty
		r.RequestURI = "/"
		r.URL.RawPath = "/"
		r.URL.Path = "/"
		r.URL.ForceQuery = false
		r.URL.RawQuery = ""
		return
	}
	u, err := url.ParseRequestURI(uri)
	if err != nil {
		panic(err)
	}
	r.RequestURI = uri
	r.URL.RawPath = u.RawPath
	r.URL.Path = u.Path
	r.URL.ForceQuery = u.ForceQuery
	r.URL.RawQuery = u.RawQuery
}

// GetProtocolVersion implements the same method as documented on
// http_handler.Host.
func (host) GetProtocolVersion(ctx context.Context) string {
	r := requestStateFromContext(ctx).r
	return r.Proto
}

// GetRequestHeaderNames implements the same method as documented on
// http_handler.Host.
func (host) GetRequestHeaderNames(ctx context.Context) (names []string) {
	r := requestStateFromContext(ctx).r

	count := len(r.Header)
	i := 0
	if r.Host != "" { // special-case the host header.
		count++
		names = make([]string, count)
		names[i] = "Host"
		i++
	} else if count == 0 {
		return nil
	} else {
		names = make([]string, count)
	}

	for n := range r.Header {
		if strings.HasPrefix(n, http.TrailerPrefix) {
			continue
		}
		names[i] = n
		i++
	}

	// Trim to what was actually written. The capacity was sized before
	// filtering trailers out, so any that were skipped would otherwise leave
	// empty names in the list the guest reads. (upstream compares len(names),
	// which is the allocated length, so its own "only trailers" guard here
	// never fires.)
	names = names[:i]
	if len(names) == 0 { // E.g. only trailers
		return nil
	}

	// Keys in a Go map don't have consistent ordering.
	sort.Strings(names)
	return
}

// GetRequestHeaderValues implements the same method as documented on
// http_handler.Host.
func (host) GetRequestHeaderValues(ctx context.Context, name string) []string {
	r := requestStateFromContext(ctx).r
	if textproto.CanonicalMIMEHeaderKey(name) == "Host" { // special-case the host header.
		return []string{r.Host}
	}
	return r.Header.Values(name)
}

// SetRequestHeaderValue implements the same method as documented on
// http_handler.Host.
func (host) SetRequestHeaderValue(ctx context.Context, name, value string) {
	s := requestStateFromContext(ctx)
	s.r.Header.Set(name, value)
}

// AddRequestHeaderValue implements the same method as documented on
// http_handler.Host.
func (host) AddRequestHeaderValue(ctx context.Context, name, value string) {
	s := requestStateFromContext(ctx)
	s.r.Header.Add(name, value)
}

// RemoveRequestHeader implements the same method as documented on
// http_handler.Host.
func (host) RemoveRequestHeader(ctx context.Context, name string) {
	s := requestStateFromContext(ctx)
	s.r.Header.Del(name)
}

// RequestBodyReader implements the same method as documented on
// http_handler.Host.
func (host) RequestBodyReader(ctx context.Context) io.ReadCloser {
	s := requestStateFromContext(ctx)
	return s.r.Body
}

// RequestBodyWriter implements the same method as documented on
// http_handler.Host.
func (host) RequestBodyWriter(ctx context.Context) io.Writer {
	s := requestStateFromContext(ctx)
	var b bytes.Buffer // reset
	s.r.Body = io.NopCloser(&b)
	return &b
}

// GetRequestTrailerNames implements the same method as documented on
// http_handler.Host.
func (host) GetRequestTrailerNames(ctx context.Context) (names []string) {
	header := requestStateFromContext(ctx).w.Header()
	return trailerNames(header)
}

// GetRequestTrailerValues implements the same method as documented on
// http_handler.Host.
func (host) GetRequestTrailerValues(ctx context.Context, name string) []string {
	header := requestStateFromContext(ctx).w.Header()
	return getTrailers(header, name)
}

// SetRequestTrailerValue implements the same method as documented on
// http_handler.Host.
func (host) SetRequestTrailerValue(ctx context.Context, name, value string) {
	header := requestStateFromContext(ctx).w.Header()
	setTrailer(header, name, value)
}

// AddRequestTrailerValue implements the same method as documented on
// http_handler.Host.
func (host) AddRequestTrailerValue(ctx context.Context, name, value string) {
	header := requestStateFromContext(ctx).w.Header()
	addTrailer(header, name, value)
}

// RemoveRequestTrailer implements the same method as documented on
// http_handler.Host.
func (host) RemoveRequestTrailer(ctx context.Context, name string) {
	header := requestStateFromContext(ctx).w.Header()
	removeTrailer(header, name)
}

// GetStatusCode implements the same method as documented on
// http_handler.Host.
func (host) GetStatusCode(ctx context.Context) uint32 {
	s := requestStateFromContext(ctx)
	if statusCode := s.w.(*bufferingResponseWriter).statusCode; statusCode == 0 {
		return 200 // default
	} else {
		return statusCode
	}
}

// SetStatusCode implements the same method as documented on
// http_handler.Host.
func (host) SetStatusCode(ctx context.Context, statusCode uint32) {
	s := requestStateFromContext(ctx)
	if w, ok := s.w.(*bufferingResponseWriter); ok {
		w.statusCode = statusCode
	} else {
		s.w.WriteHeader(int(statusCode))
	}
}

// GetResponseHeaderNames implements the same method as documented on
// http_handler.Host.
func (host) GetResponseHeaderNames(ctx context.Context) (names []string) {
	w := requestStateFromContext(ctx).w

	// allocate capacity == count though it might be smaller due to trailers.
	count := len(w.Header())
	if count == 0 {
		return nil
	}

	names = make([]string, 0, count)

	for n := range w.Header() {
		if strings.HasPrefix(n, http.TrailerPrefix) {
			continue
		}
		names = append(names, n)
	}

	if len(names) == 0 { // E.g. only trailers
		return nil
	}
	// Keys in a Go map don't have consistent ordering.
	sort.Strings(names)
	return
}

// GetResponseHeaderValues implements the same method as documented on
// http_handler.Host.
func (host) GetResponseHeaderValues(ctx context.Context, name string) []string {
	w := requestStateFromContext(ctx).w
	return w.Header().Values(name)
}

// SetResponseHeaderValue implements the same method as documented on
// http_handler.Host.
func (host) SetResponseHeaderValue(ctx context.Context, name, value string) {
	s := requestStateFromContext(ctx)
	s.w.Header().Set(name, value)
}

// AddResponseHeaderValue implements the same method as documented on
// http_handler.Host.
func (host) AddResponseHeaderValue(ctx context.Context, name, value string) {
	s := requestStateFromContext(ctx)
	s.w.Header().Add(name, value)
}

// RemoveResponseHeader implements the same method as documented on
// http_handler.Host.
func (host) RemoveResponseHeader(ctx context.Context, name string) {
	s := requestStateFromContext(ctx)
	s.w.Header().Del(name)
}

// ResponseBodyReader implements the same method as documented on
// http_handler.Host.
func (host) ResponseBodyReader(ctx context.Context) io.ReadCloser {
	s := requestStateFromContext(ctx)
	body := s.w.(*bufferingResponseWriter).body
	return io.NopCloser(bytes.NewReader(body))
}

// ResponseBodyWriter implements the same method as documented on
// http_handler.Host.
func (host) ResponseBodyWriter(ctx context.Context) io.Writer {
	s := requestStateFromContext(ctx)
	if w, ok := s.w.(*bufferingResponseWriter); ok {
		w.body = nil // reset
		return w
	} else {
		return s.w
	}
}

// GetResponseTrailerNames implements the same method as documented on
// http_handler.Host.
func (host) GetResponseTrailerNames(ctx context.Context) (names []string) {
	header := requestStateFromContext(ctx).w.Header()
	return trailerNames(header)
}

// GetResponseTrailerValues implements the same method as documented on
// http_handler.Host.
func (host) GetResponseTrailerValues(ctx context.Context, name string) []string {
	header := requestStateFromContext(ctx).w.Header()
	return getTrailers(header, name)
}

// SetResponseTrailerValue implements the same method as documented on
// http_handler.Host.
func (host) SetResponseTrailerValue(ctx context.Context, name, value string) {
	header := requestStateFromContext(ctx).w.Header()
	setTrailer(header, name, value)
}

// AddResponseTrailerValue implements the same method as documented on
// http_handler.Host.
func (host) AddResponseTrailerValue(ctx context.Context, name, value string) {
	header := requestStateFromContext(ctx).w.Header()
	addTrailer(header, name, value)
}

// RemoveResponseTrailer implements the same method as documented on
// http_handler.Host.
func (host) RemoveResponseTrailer(ctx context.Context, name string) {
	header := requestStateFromContext(ctx).w.Header()
	removeTrailer(header, name)
}

func trailerNames(header http.Header) (names []string) {
	// We don't pre-allocate as there may be no trailers.
	for n := range header {
		if strings.HasPrefix(n, http.TrailerPrefix) {
			n = n[len(http.TrailerPrefix):]
			names = append(names, n)
		}
	}
	// Keys in a Go map don't have consistent ordering.
	sort.Strings(names)
	return
}

func getTrailers(header http.Header, name string) []string {
	return header.Values(http.TrailerPrefix + name)
}

func setTrailer(header http.Header, name string, value string) {
	header.Set(http.TrailerPrefix+name, value)
}

func addTrailer(header http.Header, name string, value string) {
	header.Set(http.TrailerPrefix+name, value)
}

func removeTrailer(header http.Header, name string) {
	header.Del(http.TrailerPrefix + name)
}

// GetSourceAddr implements the same method as documented on
// http_handler.Host.
func (host) GetSourceAddr(ctx context.Context) string {
	r := requestStateFromContext(ctx).r
	return r.RemoteAddr
}
