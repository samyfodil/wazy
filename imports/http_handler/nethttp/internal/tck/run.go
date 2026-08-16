package tck

import (
	_ "embed"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	handler "github.com/samyfodil/wazy/imports/http_handler"
)

// GuestWASM is the guest wasm module used by the TCK. The host must load this
// module in the server being tested by Run.
//
//go:embed tck.wasm
var GuestWASM []byte

// Run executes the TCK. The client is http.DefaultClient, or a different value
// to test the HTTP/2.0 transport. The url must point to a server with the
// TCK's guest wasm module loaded on top of the TCK's backend handler.
//
// For example, here's how to run the tests against a httptest.Server.
//
//	server := httptest.NewServer(h)
//	tck.Run(t, server.Client(), server.URL)
func Run(t *testing.T, client *http.Client, url string) {
	t.Parallel()

	if url == "" {
		t.Fatal("url is empty")
	}

	if url[len(url)-1] == '/' {
		url = url[:len(url)-1]
	}

	r := &testRunner{
		t:      t,
		client: client,
		url:    url,
	}

	r.testGetProtocolVersion()
	r.testGetMethod()
	r.testSetMethod()
	r.testGetURI()
	r.testSetURI()
	r.testGetHeaderValuesRequest()
	r.testGetRequestHeaderNamesRequest()
	r.testSetHeaderValueRequest()
	r.testAddHeaderValueRequest()
	r.testRemoveHeaderRequest()
	r.testReadBodyRequest()
	r.testGetSourceAddr()
}

type testRunner struct {
	t      *testing.T
	client *http.Client
	url    string
}

func (r *testRunner) testGetProtocolVersion() {
	hostFn := handler.FuncGetProtocolVersion

	testID := hostFn
	r.t.Run(testID, func(t *testing.T) {
		req, err := http.NewRequest("GET", fmt.Sprintf("%s/%s", r.url, hostFn), nil)
		if err != nil {
			t.Fatal(err)
		}

		req.Header.Set("x-httpwasm-tck-testid", testID)
		resp, err := r.client.Do(req)
		if err != nil {
			t.Error(err)
		}

		body := checkResponse(t, resp)

		want := resp.Proto
		have := body
		if want != have {
			t.Errorf("expected protocol version to be %s, have %s", want, have)
		}
	})
}

func (r *testRunner) testGetMethod() {
	hostFn := handler.FuncGetMethod

	// TODO: CONNECT is often handled outside of middleware
	methods := []string{"GET", "HEAD", "POST", "PUT", "DELETE", "OPTIONS", "TRACE", "PATCH"}
	for _, method := range methods {
		testID := fmt.Sprintf("%s/%s", hostFn, method)
		r.t.Run(testID, func(t *testing.T) {
			req, err := http.NewRequest(method, r.url, nil)
			if err != nil {
				t.Fatal(err)
			}

			req.Header.Set("x-httpwasm-tck-testid", testID)
			resp, err := r.client.Do(req)
			if err != nil {
				t.Error(err)
			}

			checkResponse(t, resp)
		})
	}
}

func (r *testRunner) testSetMethod() {
	hostFn := handler.FuncSetMethod

	testID := hostFn
	r.t.Run(testID, func(t *testing.T) {
		req, err := http.NewRequest("GET", r.url, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("x-httpwasm-tck-testid", testID)
		resp, err := r.client.Do(req)
		if err != nil {
			t.Error(err)
		}

		checkResponse(t, resp)

		if want, have := "POST", resp.Header.Get("x-httpwasm-next-method"); want != have {
			t.Errorf("expected method to be %s, have %s", want, have)
		}
	})
}

func (r *testRunner) testGetURI() {
	hostFn := handler.FuncGetURI

	tests := []struct {
		testID string
		uri    string
	}{
		{
			testID: "simple",
			uri:    "/simple",
		},
		{
			testID: "simple/escaping",
			uri:    "/simple%26clean",
		},
		{
			testID: "query",
			uri:    "/animal?name=panda",
		},
		{
			testID: "query/escaping",
			uri:    "/disney?name=chip%26dale",
		},
	}

	for _, tc := range tests {
		tt := tc
		testID := fmt.Sprintf("%s/%s", hostFn, tt.testID)
		r.t.Run(testID, func(t *testing.T) {
			req, err := http.NewRequest("GET", fmt.Sprintf("%s%s", r.url, tt.uri), nil)
			if err != nil {
				t.Fatal(err)
			}

			req.Header.Set("x-httpwasm-tck-testid", testID)
			resp, err := r.client.Do(req)
			if err != nil {
				t.Error(err)
			}

			checkResponse(t, resp)
		})
	}
}

func (r *testRunner) testSetURI() {
	hostFn := handler.FuncSetURI

	tests := []struct {
		testID string
		uri    string
	}{
		{
			testID: "simple",
			uri:    "/simple",
		},
		{
			testID: "simple/escaping",
			uri:    "/simple%26clean",
		},
		{
			testID: "query",
			uri:    "/animal?name=panda",
		},
		{
			testID: "query/escaping",
			uri:    "/disney?name=chip%26dale",
		},
	}

	for _, tc := range tests {
		tt := tc
		testID := fmt.Sprintf("%s/%s", hostFn, tt.testID)
		r.t.Run(testID, func(t *testing.T) {
			req, err := http.NewRequest("GET", r.url, nil)
			if err != nil {
				t.Fatal(err)
			}

			req.Header.Set("x-httpwasm-tck-testid", testID)
			resp, err := r.client.Do(req)
			if err != nil {
				t.Error(err)
			}

			checkResponse(t, resp)

			if want, have := tt.uri, resp.Header.Get("x-httpwasm-next-uri"); want != have {
				t.Errorf("expected uri to be %s, have %s", want, have)
			}
		})
	}
}

func (r *testRunner) testGetHeaderValuesRequest() {
	hostFn := handler.FuncGetHeaderValues

	tests := []struct {
		testID string
		key    string
		value  []string
	}{
		{
			testID: "lowercase-key",
			key:    "single-header",
			value:  []string{"value"},
		},
		{
			testID: "mixedcase-key",
			key:    "Single-Header",
			value:  []string{"value"},
		},
		{
			testID: "not-exists",
			key:    "not-header",
			value:  []string{},
		},
		{
			testID: "multiple-values",
			key:    "multi-header",
			value:  []string{"value1", "value2"},
		},
	}

	for _, tc := range tests {
		tt := tc
		testID := fmt.Sprintf("%s/request/%s", hostFn, tt.testID)
		r.t.Run(testID, func(t *testing.T) {
			req, err := http.NewRequest("GET", r.url, nil)
			if err != nil {
				t.Fatal(err)
			}

			for _, v := range tt.value {
				req.Header.Add(tt.key, v)
			}

			req.Header.Set("x-httpwasm-tck-testid", testID)
			resp, err := r.client.Do(req)
			if err != nil {
				t.Error(err)
			}

			checkResponse(t, resp)
		})
	}
}

func (r *testRunner) testGetRequestHeaderNamesRequest() {
	hostFn := handler.FuncGetHeaderNames

	testID := hostFn + "/request"
	r.t.Run(testID, func(t *testing.T) {
		req, err := http.NewRequest("GET", r.url, nil)
		if err != nil {
			t.Fatal(err)
		}

		req.Header.Add("a-header", "value1")
		req.Header.Add("b-header", "2")

		req.Header.Set("x-httpwasm-tck-testid", testID)
		resp, err := r.client.Do(req)
		if err != nil {
			t.Error(err)
		}

		checkResponse(t, resp)
	})
}

func (r *testRunner) testReadBodyRequest() {
	hostFn := handler.FuncReadBody

	tests := []struct {
		testID   string
		bodySize int
	}{
		{
			testID:   "empty",
			bodySize: 0,
		},
		{
			testID:   "small",
			bodySize: 5,
		},
		{
			testID:   "medium",
			bodySize: 2048,
		},
		{
			testID:   "large",
			bodySize: 4096,
		},
		{
			testID:   "xlarge",
			bodySize: 5000,
		},
	}

	for _, tc := range tests {
		tt := tc
		testID := fmt.Sprintf("%s/request/%s", hostFn, tt.testID)
		r.t.Run(testID, func(t *testing.T) {
			payload := strings.Repeat("a", tt.bodySize)
			req, err := http.NewRequest("POST", r.url, strings.NewReader(payload))
			if err != nil {
				t.Fatal(err)
			}

			req.Header.Set("x-httpwasm-tck-testid", testID)
			resp, err := r.client.Do(req)
			if err != nil {
				t.Error(err)
			}

			checkResponse(t, resp)
		})
	}
}

func (r *testRunner) testSetHeaderValueRequest() {
	hostFn := handler.FuncSetHeaderValue

	tests := []struct {
		name   string
		header string
	}{
		{
			name:   "new",
			header: "new-header",
		},
		{
			name:   "existing",
			header: "existing-header",
		},
	}
	for _, tc := range tests {
		tt := tc
		testID := fmt.Sprintf("%s/request/%s", hostFn, tt.name)
		r.t.Run(testID, func(t *testing.T) {
			req, err := http.NewRequest("GET", r.url, nil)
			if err != nil {
				t.Fatal(err)
			}

			req.Header.Set("existing-header", "bear")

			req.Header.Set("x-httpwasm-tck-testid", testID)
			resp, err := r.client.Do(req)
			if err != nil {
				t.Error(err)
			}

			checkResponse(t, resp)

			if want, have := "value", resp.Header.Get(fmt.Sprintf("x-httpwasm-next-header-%s-0", tt.header)); want != have {
				t.Errorf("expected header to be %s, have %s", want, have)
			}
		})
	}
}

func (r *testRunner) testAddHeaderValueRequest() {
	hostFn := handler.FuncAddHeaderValue

	tests := []struct {
		name   string
		header string
		values []string
	}{
		{
			name:   "new",
			header: "new-header",
			values: []string{"value"},
		},
		{
			name:   "existing",
			header: "existing-header",
			values: []string{"bear", "value"},
		},
	}
	for _, tc := range tests {
		tt := tc
		testID := fmt.Sprintf("%s/request/%s", hostFn, tt.name)
		r.t.Run(testID, func(t *testing.T) {
			req, err := http.NewRequest("GET", r.url, nil)
			if err != nil {
				t.Fatal(err)
			}

			req.Header.Set("existing-header", "bear")

			req.Header.Set("x-httpwasm-tck-testid", testID)
			resp, err := r.client.Do(req)
			if err != nil {
				t.Error(err)
			}

			checkResponse(t, resp)

			for i, v := range tt.values {
				if want, have := v, resp.Header.Get(fmt.Sprintf("x-httpwasm-next-header-%s-%d", tt.header, i)); want != have {
					t.Errorf("expected header to be %s, have %s", want, have)
				}
			}
		})
	}
}

func (r *testRunner) testRemoveHeaderRequest() {
	hostFn := handler.FuncRemoveHeader

	tests := []struct {
		name    string
		header  string
		removed bool
	}{
		{
			name:    "new",
			header:  "new-header",
			removed: false,
		},
		{
			name:    "existing",
			header:  "existing-header",
			removed: true,
		},
	}
	for _, tc := range tests {
		tt := tc
		testID := fmt.Sprintf("%s/request/%s", hostFn, tt.name)
		r.t.Run(testID, func(t *testing.T) {
			req, err := http.NewRequest("GET", r.url, nil)
			if err != nil {
				t.Fatal(err)
			}

			req.Header.Set("existing-header", "bear")

			req.Header.Set("x-httpwasm-tck-testid", testID)
			resp, err := r.client.Do(req)
			if err != nil {
				t.Error(err)
			}

			checkResponse(t, resp)

			want := "bear"
			if tt.removed {
				want = ""
			}
			if have := resp.Header.Get("x-httpwasm-next-header-existing-header-0"); want != have {
				t.Errorf("expected header to be %s, have %s", want, have)
			}
		})
	}
}

func (r *testRunner) testGetSourceAddr() {
	hostFn := handler.FuncGetSourceAddr

	r.t.Run(hostFn, func(t *testing.T) {
		req, err := http.NewRequest("GET", r.url, nil)
		if err != nil {
			t.Fatal(err)
		}

		req.Header.Set("x-httpwasm-tck-testid", hostFn)
		resp, err := r.client.Do(req)
		if err != nil {
			t.Error(err)
		}
		checkResponse(t, resp)
	})
}

func checkResponse(t *testing.T, resp *http.Response) string {
	t.Helper()

	if resp.Header.Get("x-httpwasm-tck-handled") != "1" {
		t.Error("x-httpwasm-tck-handled header is missing")
	}

	if resp.StatusCode == http.StatusInternalServerError {
		msg := resp.Header.Get("x-httpwasm-tck-failed")
		if msg == "" {
			t.Error("error status without test failure message")
		}
		t.Errorf("assertion failed: %s", msg)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Error("error reading response body")
	}
	return string(body)
}
