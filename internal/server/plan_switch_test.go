package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// ─────────────────────────────────────────────────────────────────────────────
// Pure decision helpers: decidePlanSwitchRequest
// ─────────────────────────────────────────────────────────────────────────────

func strPtr(s string) *string     { return &s }
func timePtr(t time.Time) *time.Time { return &t }

func TestDecidePlanSwitchRequest_FeatureOff(t *testing.T) {
	// When the feature flag is off the decision short-circuits immediately —
	// no further validation runs and the handler will 404. This matters
	// because a 400/409 response would tip off the caller that the endpoint
	// exists; 404 keeps the behind-flag feature completely hidden.
	got := decidePlanSwitchRequest(false, "monthly", strPtr("active"), nil, "annual")
	if got != planSwitchFeatureOff {
		t.Fatalf("got %v, want planSwitchFeatureOff", got)
	}
}

func TestDecidePlanSwitchRequest_InvalidTarget(t *testing.T) {
	cases := []string{"", "weekly", "  ", "ANNUALLY", "free", "pro", "monthly2"}
	for _, target := range cases {
		t.Run(target, func(t *testing.T) {
			got := decidePlanSwitchRequest(true, "monthly", strPtr("active"), nil, target)
			if got != planSwitchInvalidTarget {
				t.Fatalf("target %q: got %v, want planSwitchInvalidTarget", target, got)
			}
		})
	}
}

func TestDecidePlanSwitchRequest_CaseAndWhitespaceTolerant(t *testing.T) {
	// MONTHLY / ANNUAL / mixed case / leading+trailing spaces must all be
	// normalised so the handler and the UI don't have to.
	cases := []string{"monthly", "MONTHLY", "  annual  ", "Annual", "aNnUaL"}
	for _, target := range cases {
		t.Run(target, func(t *testing.T) {
			got := decidePlanSwitchRequest(true, "monthly", strPtr("active"), nil, target)
			if got == planSwitchInvalidTarget {
				t.Errorf("target %q should normalise, got invalid", target)
			}
		})
	}
}

func TestDecidePlanSwitchRequest_NotActive(t *testing.T) {
	// Anything other than status='active' blocks the switch. "created",
	// "halted", "cancelled", "" are all disallowed — the user has nothing
	// valid running to switch from.
	cases := []struct {
		name   string
		status *string
	}{
		{"nil", nil},
		{"empty", strPtr("")},
		{"created", strPtr("created")},
		{"authenticated", strPtr("authenticated")},
		{"halted", strPtr("halted")},
		{"cancelled", strPtr("cancelled")},
		{"completed", strPtr("completed")},
		{"pending", strPtr("pending")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := decidePlanSwitchRequest(true, "monthly", tc.status, nil, "annual")
			if got != planSwitchNotActive {
				t.Errorf("status %q: got %v, want planSwitchNotActive", tc.name, got)
			}
		})
	}
}

func TestDecidePlanSwitchRequest_AlreadyOnPlan(t *testing.T) {
	// Target == current → 409. Compare is case-insensitive — "Monthly"
	// matches "monthly".
	cases := []struct {
		current, target string
	}{
		{"monthly", "monthly"},
		{"annual", "annual"},
		{"MONTHLY", "monthly"},
		{"monthly", "MONTHLY"},
	}
	for _, tc := range cases {
		t.Run(tc.current+"→"+tc.target, func(t *testing.T) {
			got := decidePlanSwitchRequest(true, tc.current, strPtr("active"), nil, tc.target)
			if got != planSwitchAlreadyOnPlan {
				t.Errorf("got %v, want planSwitchAlreadyOnPlan", got)
			}
		})
	}
}

func TestDecidePlanSwitchRequest_AlreadyPending(t *testing.T) {
	// Pending switch in flight takes priority over same-plan detection —
	// even if the user re-clicks their existing pending target, they get
	// "switch_pending" instead of "already_on_plan".
	got := decidePlanSwitchRequest(true, "monthly", strPtr("active"), strPtr("annual"), "annual")
	if got != planSwitchAlreadyPending {
		t.Fatalf("got %v, want planSwitchAlreadyPending", got)
	}
}

func TestDecidePlanSwitchRequest_EmptyPendingStringCountsAsNone(t *testing.T) {
	// A historical row with pending_plan_change="" (whitespace / empty)
	// must not be treated as an in-flight switch.
	for _, s := range []string{"", "   ", "\t"} {
		got := decidePlanSwitchRequest(true, "monthly", strPtr("active"), strPtr(s), "annual")
		if got != planSwitchOK {
			t.Errorf("pending %q should be treated as none, got %v", s, got)
		}
	}
}

func TestDecidePlanSwitchRequest_HappyPath(t *testing.T) {
	cases := []struct {
		current, target string
	}{
		{"monthly", "annual"},
		{"annual", "monthly"},
	}
	for _, tc := range cases {
		t.Run(tc.current+"→"+tc.target, func(t *testing.T) {
			got := decidePlanSwitchRequest(true, tc.current, strPtr("active"), nil, tc.target)
			if got != planSwitchOK {
				t.Errorf("got %v, want planSwitchOK", got)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Pure decision helpers: decideCancelPlanSwitch
// ─────────────────────────────────────────────────────────────────────────────

func TestDecideCancelPlanSwitch_FeatureOff(t *testing.T) {
	got := decideCancelPlanSwitch(false, strPtr("annual"), nil)
	if got != cancelSwitchFeatureOff {
		t.Fatalf("got %v, want cancelSwitchFeatureOff", got)
	}
}

func TestDecideCancelPlanSwitch_NothingPending(t *testing.T) {
	cases := []*string{nil, strPtr(""), strPtr("   ")}
	for _, p := range cases {
		got := decideCancelPlanSwitch(true, p, nil)
		if got != cancelSwitchNothingPending {
			label := "nil"
			if p != nil {
				label = *p
			}
			t.Errorf("pending=%q: got %v, want cancelSwitchNothingPending", label, got)
		}
	}
}

func TestDecideCancelPlanSwitch_AlreadyFired(t *testing.T) {
	// Once pending_plan_sub_id is populated the reconciler has already
	// created the new Razorpay sub — there's no safe self-serve undo.
	got := decideCancelPlanSwitch(true, strPtr("annual"), strPtr("sub_new_123"))
	if got != cancelSwitchAlreadyFired {
		t.Fatalf("got %v, want cancelSwitchAlreadyFired", got)
	}
}

func TestDecideCancelPlanSwitch_EmptySubIDTreatedAsUnfired(t *testing.T) {
	// An empty string in pending_plan_sub_id must not trigger
	// "already fired" — the reconciler only ever writes a non-empty value.
	for _, p := range []*string{strPtr(""), strPtr(" ")} {
		got := decideCancelPlanSwitch(true, strPtr("annual"), p)
		if got != cancelSwitchOK {
			t.Errorf("subID=%q should be cancelSwitchOK, got %v", *p, got)
		}
	}
}

func TestDecideCancelPlanSwitch_HappyPath(t *testing.T) {
	got := decideCancelPlanSwitch(true, strPtr("annual"), nil)
	if got != cancelSwitchOK {
		t.Fatalf("got %v, want cancelSwitchOK", got)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Pure decision helpers: shouldPromotePendingSwitch
// ─────────────────────────────────────────────────────────────────────────────

func TestShouldPromotePendingSwitch_NilEffectiveAt(t *testing.T) {
	if shouldPromotePendingSwitch(time.Now(), nil, nil) {
		t.Error("nil effective_at must not promote")
	}
}

func TestShouldPromotePendingSwitch_PendingSubIDSet(t *testing.T) {
	past := time.Now().Add(-24 * time.Hour)
	if shouldPromotePendingSwitch(time.Now(), &past, strPtr("sub_new_1")) {
		t.Error("a populated pending_plan_sub_id means we already fired — must not re-promote")
	}
}

func TestShouldPromotePendingSwitch_EmptySubIDCountsAsUnset(t *testing.T) {
	past := time.Now().Add(-24 * time.Hour)
	if !shouldPromotePendingSwitch(time.Now(), &past, strPtr("  ")) {
		t.Error("empty pending_plan_sub_id must be treated as unset")
	}
}

func TestShouldPromotePendingSwitch_EffectiveInFuture(t *testing.T) {
	future := time.Now().Add(24 * time.Hour)
	if shouldPromotePendingSwitch(time.Now(), &future, nil) {
		t.Error("effective_at in future must not promote")
	}
}

func TestShouldPromotePendingSwitch_EffectiveNow(t *testing.T) {
	// Boundary case: effective_at == now. "not now.Before(effective)"
	// evaluates to true, so the promotion should fire.
	now := time.Now()
	if !shouldPromotePendingSwitch(now, &now, nil) {
		t.Error("effective_at == now should promote")
	}
}

func TestShouldPromotePendingSwitch_EffectiveInPast(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	if !shouldPromotePendingSwitch(time.Now(), &past, nil) {
		t.Error("effective_at in past must promote")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Email templates: planSwitchScheduledEmail / planSwitchActivatedEmail
//                 / planSwitchCancelledEmail
// ─────────────────────────────────────────────────────────────────────────────

func TestPlanSwitchScheduledEmail_SubjectAndBody(t *testing.T) {
	eff := time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC)
	subject, html := planSwitchScheduledEmail("Developer · Monthly", "Developer · Annual", eff)

	if !strings.Contains(strings.ToLower(subject), "plan switch scheduled") {
		t.Errorf("subject should contain 'Plan switch scheduled': %q", subject)
	}
	for _, want := range []string{"Developer · Monthly", "Developer · Annual", "2026-05-20"} {
		if !strings.Contains(html, want) {
			t.Errorf("html missing %q:\n%s", want, html)
		}
	}
	if !strings.Contains(html, "contact@instanode.dev") {
		t.Errorf("html missing support footer")
	}
	if jwtLike.FindString(html) != "" {
		t.Errorf("html leaked a JWT-shaped token")
	}
}

func TestPlanSwitchScheduledEmail_FallbackWhenEffectiveAtZero(t *testing.T) {
	// Historical rows may not have current_period_end — the email should
	// render a generic fallback instead of a bare "0001-01-01" date.
	_, html := planSwitchScheduledEmail("Monthly", "Annual", time.Time{})
	if strings.Contains(html, "0001-01-01") {
		t.Errorf("zero time leaked into html: %s", html)
	}
	if !strings.Contains(html, "end of your current billing period") {
		t.Errorf("fallback wording missing: %s", html)
	}
}

func TestPlanSwitchActivatedEmail_SubjectNamesNewPlan(t *testing.T) {
	next := time.Date(2027, 1, 15, 0, 0, 0, 0, time.UTC)
	subject, html := planSwitchActivatedEmail("Developer · Annual", next)

	if !strings.Contains(subject, "Developer · Annual") {
		t.Errorf("subject should name the new plan: %q", subject)
	}
	if !strings.Contains(html, "2027-01-15") {
		t.Errorf("body missing renewal date: %s", html)
	}
	if !strings.Contains(html, "receipt") {
		t.Errorf("body should tell user a separate receipt follows")
	}
}

func TestPlanSwitchActivatedEmail_NoRenewalStringWhenZero(t *testing.T) {
	_, html := planSwitchActivatedEmail("Developer · Monthly", time.Time{})
	if strings.Contains(html, "0001-01-01") {
		t.Errorf("zero time leaked into html")
	}
}

func TestPlanSwitchCancelledEmail_ContentFlowsCorrectDirection(t *testing.T) {
	// User stays on MONTHLY; the cancelled switch had been aimed at ANNUAL.
	subject, html := planSwitchCancelledEmail("Developer · Monthly", "Developer · Annual")
	if !strings.Contains(subject, "Developer · Monthly") {
		t.Errorf("subject should include staying-on plan: %q", subject)
	}
	if !strings.Contains(html, "Developer · Monthly") || !strings.Contains(html, "Developer · Annual") {
		t.Errorf("body missing both plan labels: %s", html)
	}
	if !strings.Contains(html, "contact@instanode.dev") {
		t.Errorf("body missing support footer")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Feature flag env override
// ─────────────────────────────────────────────────────────────────────────────

func TestFeaturesConfig_EnvOverride(t *testing.T) {
	cases := []struct {
		val  string
		want bool
	}{
		{"", false},
		{"1", true},
		{"true", true},
		{"TRUE", true},
		{"0", false},
		{"false", false},
		{"no", false},
	}
	for _, tc := range cases {
		t.Run("v="+tc.val, func(t *testing.T) {
			c := &Config{}
			// Always Setenv (including to "") so a parent-shell
			// ENABLE_PLAN_SWITCH doesn't leak into the test.
			t.Setenv("ENABLE_PLAN_SWITCH", tc.val)
			c.overrideWithEnv()
			if c.Features.EnablePlanSwitch != tc.want {
				t.Errorf("ENABLE_PLAN_SWITCH=%q: got %v, want %v", tc.val, c.Features.EnablePlanSwitch, tc.want)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Integration: handleChangePlan / handleCancelPlanChange branches that
// short-circuit before any DB call. Uses a stub *server with db == nil.
// ─────────────────────────────────────────────────────────────────────────────

func newPlanSwitchTestServer(t *testing.T, enabled bool) *server {
	t.Helper()
	return &server{
		cfg: &Config{
			Features: FeaturesConfig{EnablePlanSwitch: enabled},
			JWT:      JWTConfig{Secret: "plan-switch-test-secret-must-be-32b"},
		},
	}
}

func TestHandleChangePlan_FeatureOff_Returns404(t *testing.T) {
	s := newPlanSwitchTestServer(t, false)
	req := httptest.NewRequest(http.MethodPost, "/billing/change-plan",
		bytes.NewBufferString(`{"to":"annual"}`))
	rec := httptest.NewRecorder()
	s.handleChangePlan(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("feature off: status = %d, want 404", rec.Code)
	}
}

func TestHandleCancelPlanChange_FeatureOff_Returns404(t *testing.T) {
	s := newPlanSwitchTestServer(t, false)
	req := httptest.NewRequest(http.MethodDelete, "/billing/change-plan", nil)
	rec := httptest.NewRecorder()
	s.handleCancelPlanChange(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("feature off: status = %d, want 404", rec.Code)
	}
}

func TestHandleChangePlan_Unauthenticated_Returns401(t *testing.T) {
	// Feature on but no session cookie / bearer token — must 401 before
	// touching the DB (s.db is nil and would panic otherwise).
	s := newPlanSwitchTestServer(t, true)
	req := httptest.NewRequest(http.MethodPost, "/billing/change-plan",
		bytes.NewBufferString(`{"to":"annual"}`))
	rec := httptest.NewRecorder()
	s.handleChangePlan(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("unauth: status = %d, want 401", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["error"] != "unauthorized" {
		t.Errorf("error code = %v, want unauthorized", body["error"])
	}
}

func TestHandleCancelPlanChange_Unauthenticated_Returns401(t *testing.T) {
	s := newPlanSwitchTestServer(t, true)
	req := httptest.NewRequest(http.MethodDelete, "/billing/change-plan", nil)
	rec := httptest.NewRecorder()
	s.handleCancelPlanChange(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("unauth: status = %d, want 401", rec.Code)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// promotePendingPlanSwitches: feature flag gate + stub adapters.
// ─────────────────────────────────────────────────────────────────────────────

func TestPromotePendingPlanSwitches_FeatureOffIsNoOp(t *testing.T) {
	// With the flag off, the reconciler hook must return (0, nil) without
	// touching the DB or the Razorpay stubs. Stubs that flip a flag on
	// invocation make the contract load-bearing.
	cancelCalled := false
	createCalled := false
	cancelStub := razorpaySubCanceller(func(_ context.Context, _ RazorpayConfig, _ string) error {
		cancelCalled = true
		return nil
	})
	createStub := razorpaySubCreator(func(_ context.Context, _ RazorpayConfig, _ string, _ string, _ uuid.UUID) (string, error) {
		createCalled = true
		return "sub_new_stub", nil
	})
	cfg := &Config{Features: FeaturesConfig{EnablePlanSwitch: false}}
	// db param is *sql.DB — nil is safe because the flag check short-circuits
	// before any DB query runs.
	n, err := promotePendingPlanSwitches(context.Background(), nil, cfg, cancelStub, createStub, time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 0 {
		t.Errorf("got promoted=%d, want 0", n)
	}
	if cancelCalled || createCalled {
		t.Error("stubs should not have been invoked with feature off")
	}
}
