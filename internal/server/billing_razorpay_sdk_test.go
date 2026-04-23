package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// These tests stand in front of every outbound call the service makes to
// Razorpay. They pin:
//
//   - The HTTP method + path (if the SDK's routing ever changes silently or
//     our code paths call the wrong resource, tests fail).
//   - The Basic-auth principal (KeyID:KeySecret — a credential-swap bug caught
//     here rather than against live mode).
//   - The JSON body shape (plan_id / total_count / customer_notify / notes.*).
//     Any drift from the dual-currency spec would flip at least one assertion
//     before it hits Razorpay.
//   - Error propagation and context cancellation semantics the reconciler
//     relies on to not hang a tick.
//
// A fresh razorpayMock per test cleans up via t.Cleanup so the package-level
// base URL override never leaks across tests.

// rzCfg builds a RazorpayConfig with plan IDs populated for both plans, so
// planConfig returns valid values (we test the invalid-config paths separately
// with an empty cfg).
//
// The legacy PlanIDMonthly / PlanIDAnnual fields stay populated because
// pickPlanID falls back to them when no currency-specific plan id is
// configured — covering the "deploy that hasn't set the INR env vars yet"
// case. INR-specific tests populate PlanIDINRMonthly/PlanIDINRYearly directly.
func rzCfg() RazorpayConfig {
	return RazorpayConfig{
		KeyID:         "rzp_test_abc",
		KeySecret:     "rzp_secret_xyz",
		PlanIDMonthly: "plan_monthly_stub",
		PlanIDAnnual:  "plan_annual_stub",
	}
}

func decodeJSON(t *testing.T, b []byte) map[string]interface{} {
	t.Helper()
	var out map[string]interface{}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("response body is not valid JSON: %v\nbody: %s", err, string(b))
	}
	return out
}

// ─── liveRazorpayCreateSub ────────────────────────────────────────────────────

func TestLiveRazorpayCreateSub_MonthlyHappyPath(t *testing.T) {
	m := newRazorpayMock(t)
	m.respond("POST", "/v1/subscriptions", http.StatusOK, map[string]any{
		"id":        "sub_created_001",
		"status":    "created",
		"plan_id":   "plan_monthly_stub",
		"short_url": "https://rzp.io/s/abc",
	})

	userID := uuid.New()
	got, err := liveRazorpayCreateSub(context.Background(), rzCfg(), "monthly", "USD", userID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "sub_created_001" {
		t.Errorf("returned sub id = %q, want sub_created_001", got)
	}

	calls := m.recordedCalls()
	if len(calls) != 1 {
		t.Fatalf("expected exactly 1 Razorpay call, got %d", len(calls))
	}
	c := calls[0]

	// Endpoint pinning — a silent SDK constant change would flip this.
	if c.Method != "POST" || c.Path != "/v1/subscriptions" {
		t.Errorf("hit wrong endpoint: %s %s, want POST /v1/subscriptions", c.Method, c.Path)
	}

	// Basic-auth: Razorpay SDK sends KeyID:KeySecret via Authorization: Basic.
	if !c.AuthOK || c.AuthUser != "rzp_test_abc" || c.AuthPass != "rzp_secret_xyz" {
		t.Errorf("basic auth = (%q,%q,ok=%v), want (rzp_test_abc,rzp_secret_xyz,true)",
			c.AuthUser, c.AuthPass, c.AuthOK)
	}

	// Redundant check on the raw header — guards against future SDK migrations
	// that could drop to query-string auth or change header name.
	expectedHeader := "Basic " + base64.StdEncoding.EncodeToString([]byte("rzp_test_abc:rzp_secret_xyz"))
	if got := c.Header.Get("Authorization"); got != expectedHeader {
		t.Errorf("Authorization header = %q, want %q", got, expectedHeader)
	}

	// Body shape must exactly match what Razorpay's Subscriptions API expects.
	body := decodeJSON(t, c.Body)
	if body["plan_id"] != "plan_monthly_stub" {
		t.Errorf("plan_id = %v, want plan_monthly_stub", body["plan_id"])
	}
	// total_count round-trips through JSON as float64; planConfig says 120 for monthly.
	if v, ok := body["total_count"].(float64); !ok || int(v) != 120 {
		t.Errorf("total_count = %v, want 120", body["total_count"])
	}
	if v, ok := body["customer_notify"].(float64); !ok || int(v) != 1 {
		t.Errorf("customer_notify = %v, want 1", body["customer_notify"])
	}

	notes, ok := body["notes"].(map[string]interface{})
	if !ok {
		t.Fatalf("notes missing or wrong type: %T %v", body["notes"], body["notes"])
	}
	if notes["user_id"] != userID.String() {
		t.Errorf("notes.user_id = %v, want %s", notes["user_id"], userID)
	}
	if notes["plan"] != "monthly" {
		t.Errorf("notes.plan = %v, want monthly", notes["plan"])
	}
	// purpose=plan_switch is the tag the webhook dispatcher uses to decide
	// whether to clear pending_plan_* columns. Dropping it silently would leave
	// switches "pending forever" in the reconciler.
	if notes["purpose"] != "plan_switch" {
		t.Errorf("notes.purpose = %v, want plan_switch", notes["purpose"])
	}
	// currency is the lock-in marker the webhook preserves across renewals.
	// A missing currency note would leave plan_currency NULL on legacy rows
	// forever — the COALESCE in UPDATE users only stamps when we provide it.
	if notes["currency"] != "USD" {
		t.Errorf("notes.currency = %v, want USD", notes["currency"])
	}
}

func TestLiveRazorpayCreateSub_AnnualUsesAnnualPlanAndCount(t *testing.T) {
	m := newRazorpayMock(t)
	m.respond("POST", "/v1/subscriptions", http.StatusOK, map[string]any{"id": "sub_annual_001"})

	_, err := liveRazorpayCreateSub(context.Background(), rzCfg(), "annual", "USD", uuid.New())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	calls := m.recordedCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	body := decodeJSON(t, calls[0].Body)
	if body["plan_id"] != "plan_annual_stub" {
		t.Errorf("plan_id = %v, want plan_annual_stub", body["plan_id"])
	}
	if v, _ := body["total_count"].(float64); int(v) != 10 {
		t.Errorf("total_count = %v, want 10 (10 years × annual)", body["total_count"])
	}
	notes, _ := body["notes"].(map[string]interface{})
	if notes["plan"] != "annual" {
		t.Errorf("notes.plan = %v, want annual", notes["plan"])
	}
}

func TestLiveRazorpayCreateSub_InvalidPeriod_NoHTTPCall(t *testing.T) {
	// Guard: an unknown period must be rejected *before* we burn a Razorpay
	// API call. A regression that lets "weekly" slip through would bill the
	// wrong plan or error-out after the round-trip.
	m := newRazorpayMock(t)
	_, err := liveRazorpayCreateSub(context.Background(), rzCfg(), "weekly", "USD", uuid.New())
	if err == nil {
		t.Fatal("expected error for unknown period, got nil")
	}
	if !strings.Contains(err.Error(), "invalid target period") {
		t.Errorf("error = %v, want contains 'invalid target period'", err)
	}
	if len(m.recordedCalls()) != 0 {
		t.Errorf("unexpected Razorpay calls for invalid period: %d", len(m.recordedCalls()))
	}
}

func TestLiveRazorpayCreateSub_EmptyPlanID_NoHTTPCall(t *testing.T) {
	// If the ops team rolls out code with no PLAN_ID env var, we must fail
	// fast without creating a zero-plan subscription at Razorpay.
	cfg := RazorpayConfig{KeyID: "k", KeySecret: "s"} // no PlanIDMonthly/Annual
	m := newRazorpayMock(t)
	_, err := liveRazorpayCreateSub(context.Background(), cfg, "monthly", "USD", uuid.New())
	if err == nil {
		t.Fatal("expected error for empty plan_id, got nil")
	}
	if !strings.Contains(err.Error(), "not configured") {
		t.Errorf("error = %v, want contains 'not configured'", err)
	}
	if len(m.recordedCalls()) != 0 {
		t.Errorf("unexpected Razorpay calls for unconfigured plan: %d", len(m.recordedCalls()))
	}
}

func TestLiveRazorpayCreateSub_RazorpayReturns4xx(t *testing.T) {
	// A 400 from Razorpay must surface as a Go error. The SDK wraps the JSON
	// error body into an errors.BadRequestError — we just need to see a
	// non-nil error and no sub id returned.
	m := newRazorpayMock(t)
	m.respondFunc("POST", "/v1/subscriptions", func(w http.ResponseWriter, r *http.Request, body []byte) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintln(w, `{"error":{"code":"BAD_REQUEST_ERROR","description":"plan_id is not a valid plan"}}`)
	})
	got, err := liveRazorpayCreateSub(context.Background(), rzCfg(), "monthly", "USD", uuid.New())
	if err == nil {
		t.Fatal("expected error from Razorpay 400, got nil")
	}
	if got != "" {
		t.Errorf("sub_id on error = %q, want empty", got)
	}
}

func TestLiveRazorpayCreateSub_RazorpayReturns200ButNoID(t *testing.T) {
	// Defensive: a 200 with missing "id" field must still fail. This shape
	// shouldn't happen in practice but has been seen in Razorpay outage
	// post-mortems (partial data returned).
	m := newRazorpayMock(t)
	m.respond("POST", "/v1/subscriptions", http.StatusOK, map[string]any{
		"status": "created",
		// no "id"
	})
	got, err := liveRazorpayCreateSub(context.Background(), rzCfg(), "monthly", "USD", uuid.New())
	if err == nil {
		t.Fatal("expected error when response has no id, got nil")
	}
	if got != "" {
		t.Errorf("returned id = %q, want empty", got)
	}
	if !strings.Contains(err.Error(), "no id") {
		t.Errorf("error = %v, want contains 'no id'", err)
	}
}

func TestLiveRazorpayCreateSub_ContextCancelAbandonsCall(t *testing.T) {
	// The reconciler bounds each tick; if a tick's context elapses while the
	// SDK is mid-request, the function must return ctx.Err() rather than
	// waiting for the SDK goroutine. A regression here would stack up
	// reconciler goroutines indefinitely.
	done := make(chan struct{})
	m := newRazorpayMock(t)
	m.respondFunc("POST", "/v1/subscriptions", func(w http.ResponseWriter, r *http.Request, body []byte) {
		<-done
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, `{"id":"too_late"}`)
	})
	defer close(done)

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel immediately so the select in liveRazorpayCreateSub picks ctx.Done first.
	cancel()

	got, err := liveRazorpayCreateSub(ctx, rzCfg(), "monthly", "USD", uuid.New())
	if err == nil {
		t.Fatal("expected ctx.Err() after cancel, got nil")
	}
	if got != "" {
		t.Errorf("returned id on cancel = %q, want empty", got)
	}
	if err != context.Canceled {
		t.Errorf("error = %v, want context.Canceled", err)
	}
}

func TestLiveRazorpayCreateSub_INRMonthlyPicksINRPlanAndNotes(t *testing.T) {
	// An INR caller must (a) hit the INR-specific plan_id, (b) stamp
	// notes.currency=INR so the webhook COALESCE locks the user in. A bug
	// where the currency arg gets silently dropped from notes would leave
	// legacy COALESCE(plan_currency, ...) unable to tell USD from INR on
	// the first charge.
	m := newRazorpayMock(t)
	m.respond("POST", "/v1/subscriptions", http.StatusOK, map[string]any{"id": "sub_inr_001"})

	cfg := RazorpayConfig{
		KeyID:            "rzp_test_abc",
		KeySecret:        "rzp_secret_xyz",
		PlanIDMonthly:    "plan_legacy_M",   // legacy USD fallback
		PlanIDINRMonthly: "plan_inr_monthly_stub",
	}
	_, err := liveRazorpayCreateSub(context.Background(), cfg, "monthly", "INR", uuid.New())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	calls := m.recordedCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	body := decodeJSON(t, calls[0].Body)
	if body["plan_id"] != "plan_inr_monthly_stub" {
		t.Errorf("plan_id = %v, want plan_inr_monthly_stub", body["plan_id"])
	}
	notes, _ := body["notes"].(map[string]interface{})
	if notes["currency"] != "INR" {
		t.Errorf("notes.currency = %v, want INR", notes["currency"])
	}
}

func TestLiveRazorpayCreateSub_LegacyCurrencyNormalizesToUSD(t *testing.T) {
	// A caller passing empty/junk currency must normalize to USD in notes —
	// never leak "" or "EUR" into the Razorpay payload. This matches the
	// handleSubscriptionCharged defensive default on the webhook side.
	m := newRazorpayMock(t)
	m.respond("POST", "/v1/subscriptions", http.StatusOK, map[string]any{"id": "sub_usd_fb"})

	_, err := liveRazorpayCreateSub(context.Background(), rzCfg(), "monthly", "", uuid.New())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	notes, _ := decodeJSON(t, m.recordedCalls()[0].Body)["notes"].(map[string]interface{})
	if notes["currency"] != "USD" {
		t.Errorf("empty currency → notes.currency = %v, want USD", notes["currency"])
	}
}

// ─── liveRazorpayCancelSub ────────────────────────────────────────────────────

func TestLiveRazorpayCancelSub_HappyPath(t *testing.T) {
	m := newRazorpayMock(t)
	m.respond("POST", "/v1/subscriptions/sub_to_cancel/cancel", http.StatusOK, map[string]any{
		"id":     "sub_to_cancel",
		"status": "cancelled",
	})

	err := liveRazorpayCancelSub(context.Background(), rzCfg(), "sub_to_cancel")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	calls := m.recordedCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	c := calls[0]

	if c.Method != "POST" || c.Path != "/v1/subscriptions/sub_to_cancel/cancel" {
		t.Errorf("hit wrong endpoint: %s %s, want POST /v1/subscriptions/sub_to_cancel/cancel", c.Method, c.Path)
	}

	// Basic auth — same as create path.
	if c.AuthUser != "rzp_test_abc" || c.AuthPass != "rzp_secret_xyz" {
		t.Errorf("basic auth = (%q,%q), want (rzp_test_abc,rzp_secret_xyz)", c.AuthUser, c.AuthPass)
	}

	// cancel_at_cycle_end: 0 means "cancel now". The plan-switch flow only
	// cancels the old sub after the period has elapsed, so 0 is correct.
	body := decodeJSON(t, c.Body)
	if v, ok := body["cancel_at_cycle_end"].(float64); !ok || int(v) != 0 {
		t.Errorf("cancel_at_cycle_end = %v, want 0", body["cancel_at_cycle_end"])
	}
}

func TestLiveRazorpayCancelSub_EmptySubIDIsNoop(t *testing.T) {
	// An empty sub id means "no old sub to cancel" — seen when a fresh user
	// promotes through plan_switch without any prior sub. Must not hit the API.
	m := newRazorpayMock(t)
	if err := liveRazorpayCancelSub(context.Background(), rzCfg(), ""); err != nil {
		t.Fatalf("empty sub id should be noop, got error: %v", err)
	}
	if n := len(m.recordedCalls()); n != 0 {
		t.Errorf("expected 0 Razorpay calls for empty sub id, got %d", n)
	}
}

func TestLiveRazorpayCancelSub_RazorpayError(t *testing.T) {
	m := newRazorpayMock(t)
	m.respondFunc("POST", "/v1/subscriptions/sub_bad/cancel", func(w http.ResponseWriter, r *http.Request, body []byte) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintln(w, `{"error":{"code":"BAD_REQUEST_ERROR","description":"subscription already cancelled"}}`)
	})
	err := liveRazorpayCancelSub(context.Background(), rzCfg(), "sub_bad")
	if err == nil {
		t.Fatal("expected error from Razorpay 400, got nil")
	}
}

func TestLiveRazorpayCancelSub_ContextCancel(t *testing.T) {
	done := make(chan struct{})
	m := newRazorpayMock(t)
	m.respondFunc("POST", "/v1/subscriptions/sub_slow/cancel", func(w http.ResponseWriter, r *http.Request, body []byte) {
		<-done
	})
	defer close(done)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := liveRazorpayCancelSub(ctx, rzCfg(), "sub_slow")
	if err == nil {
		t.Fatal("expected ctx.Err() after cancel, got nil")
	}
	if err != context.Canceled {
		t.Errorf("error = %v, want context.Canceled", err)
	}
}

// ─── Integration: razorpayBaseURLOverride acts globally ──────────────────────

func TestRazorpayBaseURLOverride_AppliesToFreshClients(t *testing.T) {
	// Smoke test that newRazorpayClient always picks up the override. If
	// someone re-introduces a razorpay.NewClient(...) call that bypasses our
	// helper, a test elsewhere would hit api.razorpay.com for real — this
	// test demonstrates the boundary by calling newRazorpayClient directly
	// and verifying the SDK honors our base URL.
	m := newRazorpayMock(t)
	m.respond("POST", "/v1/subscriptions", http.StatusOK, map[string]any{"id": "sub_smoke"})

	client := newRazorpayClient(rzCfg())
	if client.Request.BaseURL != m.server.URL {
		t.Fatalf("client.BaseURL = %q, want override %q", client.Request.BaseURL, m.server.URL)
	}

	_, err := client.Subscription.Create(map[string]interface{}{
		"plan_id":         "plan_monthly_stub",
		"total_count":     120,
		"customer_notify": 1,
	}, nil)
	if err != nil {
		t.Fatalf("unexpected error on stub call: %v", err)
	}
	if n := len(m.recordedCalls()); n != 1 {
		t.Errorf("expected exactly 1 captured call, got %d", n)
	}
}

// TestRazorpayBaseURLOverride_EmptyMeansProductionDefault confirms that when
// no test override is active, newRazorpayClient leaves the SDK pointed at the
// real API. This is the guarantee production relies on — a broken test hook
// that leaked into prod would be a credential-leak incident.
func TestRazorpayBaseURLOverride_EmptyMeansProductionDefault(t *testing.T) {
	// Save and restore in case another test (or the harness) set it.
	prev := loadRazorpayBaseURLOverride()
	setRazorpayBaseURLOverride("")
	t.Cleanup(func() { setRazorpayBaseURLOverride(prev) })

	client := newRazorpayClient(rzCfg())
	if client.Request.BaseURL != "https://api.razorpay.com" {
		t.Errorf("prod BaseURL = %q, want https://api.razorpay.com", client.Request.BaseURL)
	}
}

