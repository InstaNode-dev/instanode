package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// razorpayMock is an httptest-backed stand-in for api.razorpay.com. It captures
// every request the Razorpay Go SDK issues so tests can assert on the exact
// wire shape — method, path, basic-auth principal, JSON body — without
// touching the real API.
//
// Hook it up with:
//
//	m := newRazorpayMock(t)
//	m.respond("POST", "/v1/subscriptions", 200, map[string]any{"id": "sub_stub"})
//	sub, err := liveRazorpayCreateSub(ctx, cfg, "monthly", uuid.New())
//
// newRazorpayMock swaps the package-level razorpayBaseURLOverride for the
// httptest server's URL and restores it on t.Cleanup, so parallel tests in
// other packages are unaffected (and sequential tests in this file can't leak
// into each other).
type razorpayMock struct {
	t       *testing.T
	server  *httptest.Server
	mu      sync.Mutex
	routes  map[string]razorpayMockRoute // key = METHOD + " " + path
	calls   []capturedRazorpayCall
	prevURL string
}

type razorpayMockRoute struct {
	status int
	body   interface{} // marshalled to JSON
	// If set, called instead of returning body. Lets a test assert on the
	// request inline and choose a dynamic response.
	handler func(w http.ResponseWriter, r *http.Request, body []byte)
}

// capturedRazorpayCall mirrors the parts of an inbound request we care about
// verifying against the Razorpay API contract: method + path pin the endpoint,
// authUser pins the basic-auth principal (Key ID), body pins the JSON payload.
type capturedRazorpayCall struct {
	Method   string
	Path     string
	AuthUser string // HTTP Basic username = Razorpay Key ID
	AuthPass string // HTTP Basic password = Razorpay Key Secret
	AuthOK   bool
	Body     []byte
	Header   http.Header
}

// newRazorpayMock spins up a fresh mock server and points the SDK at it for
// the duration of the test.
func newRazorpayMock(t *testing.T) *razorpayMock {
	t.Helper()
	m := &razorpayMock{
		t:      t,
		routes: make(map[string]razorpayMockRoute),
	}
	m.server = httptest.NewServer(http.HandlerFunc(m.serve))

	// Swap the package-level override so every call via newRazorpayClient
	// (which every production code path now goes through) hits the mock.
	m.prevURL = loadRazorpayBaseURLOverride()
	setRazorpayBaseURLOverride(m.server.URL)

	t.Cleanup(func() {
		setRazorpayBaseURLOverride(m.prevURL)
		m.server.Close()
	})
	return m
}

// respond registers a static JSON response for (method, path). The Razorpay SDK
// builds paths like "/v1/subscriptions" or "/v1/subscriptions/sub_abc/cancel".
func (m *razorpayMock) respond(method, path string, status int, body interface{}) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.routes[method+" "+path] = razorpayMockRoute{status: status, body: body}
}

// respondFunc registers a dynamic handler that can inspect the request body and
// emit a tailored response. Use when the static respond above isn't enough —
// e.g. asserting a specific field inline.
func (m *razorpayMock) respondFunc(method, path string, fn func(w http.ResponseWriter, r *http.Request, body []byte)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.routes[method+" "+path] = razorpayMockRoute{handler: fn}
}

// calls returns a snapshot of everything the SDK has hit so far.
func (m *razorpayMock) recordedCalls() []capturedRazorpayCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]capturedRazorpayCall, len(m.calls))
	copy(out, m.calls)
	return out
}

func (m *razorpayMock) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	user, pass, ok := r.BasicAuth()

	m.mu.Lock()
	m.calls = append(m.calls, capturedRazorpayCall{
		Method:   r.Method,
		Path:     r.URL.Path,
		AuthUser: user,
		AuthPass: pass,
		AuthOK:   ok,
		Body:     body,
		Header:   r.Header.Clone(),
	})
	route, found := m.routes[r.Method+" "+r.URL.Path]
	m.mu.Unlock()

	if !found {
		// Unregistered route → fail loudly. Silent 200 would hide test drift.
		http.Error(w, `{"error":{"code":"BAD_REQUEST_ERROR","description":"unmocked route"}}`, http.StatusNotFound)
		return
	}

	if route.handler != nil {
		route.handler(w, r, body)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(route.status)
	_ = json.NewEncoder(w).Encode(route.body)
}
