package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// These tests guard the one non-negotiable trust boundary on the Razorpay
// webhook endpoint: signature verification. Anything that passes here is
// treated as authentically from Razorpay; anything that fails must be
// rejected with 401.
//
// They deliberately exercise only the pre-DB branches of handleRazorpayWebhook
// (signature check, JSON parse, missing-event, empty-dedup-id) so the tests
// require no database. Happy-path DB persistence is covered by the E2E suite
// against a real Postgres; the value here is that a unit run catches a
// regression in the signature logic before any deploy.

// whsrv builds a minimal *server with just enough config for the webhook
// handler to run — no DB, no email, no redis. A real razorpayPayment is
// injected so signature verification runs against the test secret.
func whsrv(secret string) *server {
	cfg := &Config{
		Razorpay: RazorpayConfig{WebhookSecret: secret},
	}
	return &server{
		cfg:     cfg,
		payment: newRazorpayPayment(cfg.Razorpay),
	}
}

func signedRequest(t *testing.T, secret string, body string, includeSig bool) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhooks/razorpay", bytes.NewBufferString(body))
	if includeSig {
		s := (&server{}).computeSignature(body, secret)
		req.Header.Set("X-Razorpay-Signature", s)
	}
	req.Header.Set("Content-Type", "application/json")
	return req
}

// TestWebhook_ValidSignatureButUnknownEvent_Returns200NoOp — the happiest path
// we can reach without a DB: a fully-signed payload with no dedup id inside.
// The handler should return 200 and ignore the event.
func TestWebhook_ValidSignatureButUnknownEvent_Returns200NoOp(t *testing.T) {
	secret := "whsec_test_valid"
	s := whsrv(secret)

	// No top-level id, no payment.id, no subscription.id → dedupID stays empty
	// and the handler returns 200 without touching the DB.
	body := `{"event":"some.other.event"}`
	req := signedRequest(t, secret, body, true)
	rec := httptest.NewRecorder()

	s.handleRazorpayWebhook(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for valid sig / no dedup id", rec.Code)
	}
}

// TestWebhook_WrongSignature_Returns401 — the critical negative case. A forged
// body with a made-up signature must be rejected.
func TestWebhook_WrongSignature_Returns401(t *testing.T) {
	s := whsrv("whsec_actual_secret")
	req := httptest.NewRequest(http.MethodPost, "/webhooks/razorpay",
		bytes.NewBufferString(`{"event":"subscription.charged"}`))
	req.Header.Set("X-Razorpay-Signature", "totally-wrong-signature")

	rec := httptest.NewRecorder()
	s.handleRazorpayWebhook(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for wrong signature", rec.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["error"] != "invalid_signature" {
		t.Errorf("error = %v, want invalid_signature", body["error"])
	}
}

// TestWebhook_SignatureComputedWithWrongSecret_Returns401 — ensures we're
// actually using our configured WebhookSecret, not accepting any HMAC the
// caller bothered to compute. A caller who knows the body but not our secret
// must not be able to forge an acceptable signature.
func TestWebhook_SignatureComputedWithWrongSecret_Returns401(t *testing.T) {
	s := whsrv("whsec_real_server_secret")

	body := `{"event":"subscription.charged","id":"evt_1"}`
	// Sign with the wrong secret.
	attacker := (&server{}).computeSignature(body, "whsec_attacker_guess")

	req := httptest.NewRequest(http.MethodPost, "/webhooks/razorpay", bytes.NewBufferString(body))
	req.Header.Set("X-Razorpay-Signature", attacker)

	rec := httptest.NewRecorder()
	s.handleRazorpayWebhook(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 when signature used wrong secret", rec.Code)
	}
}

// TestWebhook_NoSignatureHeader_Returns401 — a request with no signature must
// not fall through to "empty string matches empty computed" or any other
// accidental truthiness.
func TestWebhook_NoSignatureHeader_Returns401(t *testing.T) {
	s := whsrv("whsec_test")
	req := httptest.NewRequest(http.MethodPost, "/webhooks/razorpay",
		bytes.NewBufferString(`{"event":"subscription.charged"}`))
	// Intentionally no X-Razorpay-Signature.

	rec := httptest.NewRecorder()
	s.handleRazorpayWebhook(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for missing signature", rec.Code)
	}
}

// TestWebhook_TamperedBodySameSignature_Returns401 — the classic replay: the
// attacker steals a captured signature and attaches it to a mutated body.
// Must fail.
func TestWebhook_TamperedBodySameSignature_Returns401(t *testing.T) {
	secret := "whsec_test"
	s := whsrv(secret)

	origBody := `{"event":"subscription.charged","id":"evt_1"}`
	tamperedBody := `{"event":"subscription.charged","id":"evt_attacker_pwned"}`

	origSig := (&server{}).computeSignature(origBody, secret)

	req := httptest.NewRequest(http.MethodPost, "/webhooks/razorpay",
		bytes.NewBufferString(tamperedBody))
	req.Header.Set("X-Razorpay-Signature", origSig)

	rec := httptest.NewRecorder()
	s.handleRazorpayWebhook(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for signature/body mismatch", rec.Code)
	}
}

// TestWebhook_EmptyServerSecret_StillRequiresMatchingSignature — if the server
// misconfigures WebhookSecret="" we must NOT accept every unsigned request.
// The handler computes HMAC with empty key (deterministic) and expects the
// client to present that same hash. In practice this still rejects almost
// every real request (only an attacker who knew we were misconfigured could
// craft a hit), but it's worth asserting the handler doesn't short-circuit
// when the secret is empty.
func TestWebhook_EmptyServerSecret_RejectsMissingSignature(t *testing.T) {
	s := whsrv("")
	req := httptest.NewRequest(http.MethodPost, "/webhooks/razorpay",
		bytes.NewBufferString(`{"event":"subscription.charged"}`))
	// No signature header — header.Get returns "".
	// computeSignature("{...}", "") will be some non-empty hex → "" != hex → 401.

	rec := httptest.NewRecorder()
	s.handleRazorpayWebhook(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 even with empty server secret", rec.Code)
	}
}

// TestWebhook_ValidSignatureButInvalidJSON_Returns400 — once the signature
// passes, malformed JSON must get a distinct 400 so Razorpay doesn't retry
// a payload it can never deliver.
func TestWebhook_ValidSignatureButInvalidJSON_Returns400(t *testing.T) {
	secret := "whsec_test"
	s := whsrv(secret)

	body := `{not-json`
	req := signedRequest(t, secret, body, true)

	rec := httptest.NewRecorder()
	s.handleRazorpayWebhook(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for invalid JSON after valid sig", rec.Code)
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["error"] != "invalid_json" {
		t.Errorf("error = %v, want invalid_json", resp["error"])
	}
}

// TestWebhook_ValidSignatureMissingEvent_Returns400 — a well-formed JSON
// object that doesn't carry the "event" field is explicitly invalid per
// Razorpay's webhook contract; we reject it with 400 (not silently 200).
func TestWebhook_ValidSignatureMissingEvent_Returns400(t *testing.T) {
	secret := "whsec_test"
	s := whsrv(secret)

	body := `{"payload":{}}`
	req := signedRequest(t, secret, body, true)

	rec := httptest.NewRecorder()
	s.handleRazorpayWebhook(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for missing event", rec.Code)
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["error"] != "missing_event" {
		t.Errorf("error = %v, want missing_event", resp["error"])
	}
}

// TestWebhook_SignatureMatchesEmptyBody_Returns400 — a corner case: an empty
// body's HMAC is still well-defined. The sig verifies, but JSON parse fails.
// We should get 400, not 500 or 200.
func TestWebhook_SignatureMatchesEmptyBody_Returns400(t *testing.T) {
	secret := "whsec_test"
	s := whsrv(secret)

	sig := (&server{}).computeSignature("", secret)
	req := httptest.NewRequest(http.MethodPost, "/webhooks/razorpay", bytes.NewBuffer(nil))
	req.Header.Set("X-Razorpay-Signature", sig)

	rec := httptest.NewRecorder()
	s.handleRazorpayWebhook(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for empty-body signed request", rec.Code)
	}
}
