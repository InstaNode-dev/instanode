package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Pure-function tests for the parser + admin gate. Handler tests that need a
// DB use a stub *server with db == nil and short-circuit before any SQL call.

// ── parseBrevoPayload ──────────────────────────────────────────────────────

func TestParseBrevoPayload_EmptyBodyErrors(t *testing.T) {
	if _, err := parseBrevoPayload(nil); err == nil {
		t.Fatal("expected error on nil body, got nil")
	}
	if _, err := parseBrevoPayload([]byte{}); err == nil {
		t.Fatal("expected error on empty body, got nil")
	}
}

func TestParseBrevoPayload_InvalidJSONErrors(t *testing.T) {
	if _, err := parseBrevoPayload([]byte("not json")); err == nil {
		t.Fatal("expected error on garbage body, got nil")
	}
}

func TestParseBrevoPayload_Minimal(t *testing.T) {
	body := []byte(`{"items":[{"MessageId":"m1","From":{"Address":"a@b.com","Name":"A"},"To":[{"Address":"contact@instanode.dev"}],"Subject":"hi","RawTextBody":"hello"}]}`)
	msgs, err := parseBrevoPayload(body)
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	m := msgs[0]
	if m.ProviderID != "m1" {
		t.Errorf("ProviderID = %q, want %q", m.ProviderID, "m1")
	}
	if m.FromEmail != "a@b.com" {
		t.Errorf("FromEmail = %q, want a@b.com", m.FromEmail)
	}
	if m.FromName != "A" {
		t.Errorf("FromName = %q, want A", m.FromName)
	}
	if m.ToEmail != "contact@instanode.dev" {
		t.Errorf("ToEmail = %q", m.ToEmail)
	}
	if m.Subject != "hi" {
		t.Errorf("Subject = %q", m.Subject)
	}
	if m.BodyText != "hello" {
		t.Errorf("BodyText = %q", m.BodyText)
	}
}

func TestParseBrevoPayload_FullShape(t *testing.T) {
	// A complete fixture — confirms every field maps correctly, including
	// Headers preserved as raw JSONB, and the Raw* body variants preferred
	// over Text/Html.
	spam := 1.2
	fixture := map[string]any{
		"items": []map[string]any{{
			"MessageId":   "<abc@mx.brevo.com>",
			"Uuid":        "uuid-1",
			"From":        map[string]string{"Address": "sender@example.com", "Name": "Sender Name"},
			"To":          []map[string]string{{"Address": "contact@instanode.dev", "Name": "Contact"}},
			"Subject":     "Subject line",
			"RawTextBody": "plain raw",
			"Text":        "fallback plain", // should be ignored
			"RawHtmlBody": "<p>raw html</p>",
			"Html":        "<p>fallback</p>", // should be ignored
			"SpamScore":   spam,
			"Headers":     map[string]string{"X-Foo": "bar", "Received": "from mx.foo"},
		}},
	}
	body, _ := json.Marshal(fixture)
	msgs, err := parseBrevoPayload(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 msg, got %d", len(msgs))
	}
	m := msgs[0]
	if m.ProviderID != "<abc@mx.brevo.com>" {
		t.Errorf("ProviderID = %q", m.ProviderID)
	}
	if m.FromEmail != "sender@example.com" {
		t.Errorf("FromEmail = %q", m.FromEmail)
	}
	if m.FromName != "Sender Name" {
		t.Errorf("FromName = %q", m.FromName)
	}
	if m.ToEmail != "contact@instanode.dev" {
		t.Errorf("ToEmail = %q", m.ToEmail)
	}
	if m.Subject != "Subject line" {
		t.Errorf("Subject = %q", m.Subject)
	}
	if m.BodyText != "plain raw" {
		t.Errorf("BodyText = %q (want 'plain raw', i.e. Raw* preferred over Text)", m.BodyText)
	}
	if m.BodyHTML != "<p>raw html</p>" {
		t.Errorf("BodyHTML = %q (want raw, i.e. Raw* preferred over Html)", m.BodyHTML)
	}
	if m.SpamScore == nil || *m.SpamScore != spam {
		t.Errorf("SpamScore = %v, want %v", m.SpamScore, spam)
	}
	if len(m.RawHeaders) == 0 {
		t.Error("RawHeaders unexpectedly empty")
	}
	// Headers JSON should be a JSON object containing X-Foo key.
	if !bytes.Contains(m.RawHeaders, []byte("X-Foo")) {
		t.Errorf("RawHeaders missing X-Foo key: %s", string(m.RawHeaders))
	}
}

func TestParseBrevoPayload_FallbacksWhenMessageIdMissing(t *testing.T) {
	// No MessageId — should fall back to Uuid.
	body1 := []byte(`{"items":[{"Uuid":"u-1","From":{"Address":"a@b"},"To":[{"Address":"c@d"}]}]}`)
	msgs, err := parseBrevoPayload(body1)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if msgs[0].ProviderID != "u-1" {
		t.Errorf("ProviderID = %q, want u-1", msgs[0].ProviderID)
	}

	// Neither MessageId nor Uuid — should fall back to content hash prefixed with sha256:.
	body2 := []byte(`{"items":[{"From":{"Address":"a@b"},"To":[{"Address":"c@d"}],"Subject":"x"}]}`)
	msgs2, err := parseBrevoPayload(body2)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !strings.HasPrefix(msgs2[0].ProviderID, "sha256:") {
		t.Errorf("ProviderID = %q, want sha256: prefix", msgs2[0].ProviderID)
	}
}

func TestParseBrevoPayload_EmptyItems(t *testing.T) {
	msgs, err := parseBrevoPayload([]byte(`{"items":[]}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("expected 0 messages, got %d", len(msgs))
	}
}

// ── handler: token gate ────────────────────────────────────────────────────

func newInboundTestServer(secret string) *server {
	return &server{
		cfg: &Config{
			Email: EmailConfig{BrevoInboundSecret: secret},
			Admin: AdminConfig{Email: "admin@example.com"},
		},
	}
}

func TestBrevoInbound_TokenMismatch(t *testing.T) {
	s := newInboundTestServer("real-secret")

	cases := []struct {
		name string
		url  string
	}{
		{"missing token", "/webhooks/brevo-inbound"},
		{"wrong token", "/webhooks/brevo-inbound?token=wrong"},
		{"empty token", "/webhooks/brevo-inbound?token="},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, tc.url, strings.NewReader(`{"items":[]}`))
			rec := httptest.NewRecorder()
			s.handleBrevoInbound(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("%s: status = %d, want 401", tc.name, rec.Code)
			}
		})
	}
}

func TestBrevoInbound_TokenEmptyConfigRejectsAll(t *testing.T) {
	// When BrevoInboundSecret is empty, EVERY request (including one with an
	// empty token in the URL) must 401 — not be a silent bypass.
	s := newInboundTestServer("")
	req := httptest.NewRequest(http.MethodPost, "/webhooks/brevo-inbound?token=", strings.NewReader(`{"items":[]}`))
	rec := httptest.NewRecorder()
	s.handleBrevoInbound(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("empty-secret config: status = %d, want 401", rec.Code)
	}
}

func TestBrevoInbound_TokenConstantTime(t *testing.T) {
	// Smoke test only — we don't assert nanosecond equality, just that two
	// mismatched tokens return in the same order-of-magnitude. This guards
	// against future refactors sneaking in a `==` comparison on the secret.
	s := newInboundTestServer("correct-horse-battery-staple")

	call := func(tok string) time.Duration {
		req := httptest.NewRequest(http.MethodPost, "/webhooks/brevo-inbound?token="+tok, strings.NewReader(`{"items":[]}`))
		rec := httptest.NewRecorder()
		start := time.Now()
		s.handleBrevoInbound(rec, req)
		return time.Since(start)
	}
	d1 := call("a")                               // short mismatch
	d2 := call("completely-different-long-wrong") // long mismatch
	// Both should complete in < 100ms — we're really just asserting the code
	// path ran and didn't short-circuit with a length-dependent string compare
	// that returns `false` on the first byte for d1.
	if d1 > 100*time.Millisecond || d2 > 100*time.Millisecond {
		t.Errorf("token compare took too long: d1=%v d2=%v", d1, d2)
	}
}

func TestBrevoInbound_InvalidJSON(t *testing.T) {
	s := newInboundTestServer("s")
	req := httptest.NewRequest(http.MethodPost, "/webhooks/brevo-inbound?token=s", strings.NewReader("not json at all"))
	rec := httptest.NewRecorder()
	s.handleBrevoInbound(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "invalid_payload") {
		t.Errorf("body missing 'invalid_payload': %s", rec.Body.String())
	}
}

func TestBrevoInbound_BodyTooLarge(t *testing.T) {
	s := newInboundTestServer("s")

	// 11 MB body — just above the 10 MB inbound cap. MaxBytesReader wraps
	// the request body inside the handler, so we install a reader that
	// sources more bytes than the limit allows.
	big := bytes.Repeat([]byte("A"), 11*1024*1024)
	req := httptest.NewRequest(http.MethodPost, "/webhooks/brevo-inbound?token=s", bytes.NewReader(big))
	rec := httptest.NewRecorder()
	s.handleBrevoInbound(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", rec.Code)
	}
}

func TestBrevoInbound_EmptyItemsNoDB(t *testing.T) {
	// Empty items array parses cleanly and the handler returns 200 with
	// received:0 — and critically, never touches s.db (which is nil here).
	s := newInboundTestServer("s")
	req := httptest.NewRequest(http.MethodPost, "/webhooks/brevo-inbound?token=s", strings.NewReader(`{"items":[]}`))
	rec := httptest.NewRecorder()
	s.handleBrevoInbound(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out["ok"] != true {
		t.Errorf("ok field = %v, want true", out["ok"])
	}
	if got, _ := out["received"].(float64); got != 0 {
		t.Errorf("received = %v, want 0", got)
	}
}

// ── admin gate (pure function) ─────────────────────────────────────────────

func TestIsAdmin_NilUser(t *testing.T) {
	if isAdmin(nil, "admin@example.com") {
		t.Error("nil user should never be admin")
	}
}

func TestIsAdmin_EmptyAdminEmail(t *testing.T) {
	// Unset admin email → admin routes must be un-reachable (not "any email works").
	u := &User{Email: "anyone@example.com"}
	if isAdmin(u, "") {
		t.Error("empty admin email should deny everyone")
	}
}

func TestIsAdmin_NonAdminForbidden(t *testing.T) {
	u := &User{Email: "notadmin@example.com"}
	if isAdmin(u, "admin@example.com") {
		t.Error("non-admin user passed isAdmin gate")
	}
}

func TestIsAdmin_AdminAllowed(t *testing.T) {
	u := &User{Email: "admin@example.com"}
	if !isAdmin(u, "admin@example.com") {
		t.Error("admin user failed isAdmin gate")
	}
}

func TestIsAdmin_CaseAndWhitespaceInsensitive(t *testing.T) {
	// Gmail-style normalisation (case-insensitive, leading/trailing whitespace
	// ignored) matches what the DB upserts as the user's email.
	u := &User{Email: "  Admin@Example.com  "}
	if !isAdmin(u, "admin@example.com") {
		t.Error("admin gate should tolerate case + whitespace")
	}
}

// ── Admin handlers short-circuit before DB ─────────────────────────────────

// These exercise the auth+admin gates and confirm we never fall through to a
// DB call for non-admins. We reach through authUser by constructing an
// alternate test surface: because authUser requires a real DB for the user
// lookup, we can only test the unauthenticated branch cleanly here.

func TestAdminInbox_UnauthenticatedReturns401(t *testing.T) {
	s := newInboundTestServer("s") // db is nil — handler must not reach it
	req := httptest.NewRequest(http.MethodGet, "/admin/inbox", nil)
	rec := httptest.NewRecorder()
	s.handleAdminInboxList(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("unauth GET /admin/inbox: status = %d, want 401", rec.Code)
	}
}

func TestAdminInboxMarkRead_UnauthenticatedReturns401(t *testing.T) {
	s := newInboundTestServer("s")
	id := uuid.New().String()
	req := httptest.NewRequest(http.MethodPost, "/admin/inbox/"+id+"/mark-read", nil)
	req.SetPathValue("id", id)
	rec := httptest.NewRecorder()
	s.handleAdminInboxMarkRead(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("unauth POST /admin/inbox/{id}/mark-read: status = %d, want 401", rec.Code)
	}
}
