package main

import (
	"strings"
	"testing"
	"time"
)

// ── planConfig (billing.go) ────────────────────────────────────────────────

func TestPlanConfig_Monthly(t *testing.T) {
	cfg := RazorpayConfig{PlanIDMonthly: "plan_M", PlanIDAnnual: "plan_A"}
	pid, label, count, ok := planConfig("monthly", cfg)
	if !ok || pid != "plan_M" || label != "Developer · Monthly" || count != 120 {
		t.Errorf("monthly: got (%q,%q,%d,%v)", pid, label, count, ok)
	}
}

func TestPlanConfig_Annual(t *testing.T) {
	cfg := RazorpayConfig{PlanIDMonthly: "plan_M", PlanIDAnnual: "plan_A"}
	pid, label, count, ok := planConfig("annual", cfg)
	if !ok || pid != "plan_A" || label != "Developer · Annual" || count != 10 {
		t.Errorf("annual: got (%q,%q,%d,%v)", pid, label, count, ok)
	}
}

func TestPlanConfig_UnknownPlan(t *testing.T) {
	for _, name := range []string{"", "weekly", "monthly ", "MONTHLY", "free", "pro"} {
		_, _, _, ok := planConfig(name, RazorpayConfig{PlanIDMonthly: "x", PlanIDAnnual: "y"})
		if ok {
			t.Errorf("planConfig(%q) should reject", name)
		}
	}
}

func TestPlanConfig_EmptyPlanIDsStillOK(t *testing.T) {
	// planConfig returns (planID="", ok=true) when the plan is known but the
	// ID isn't configured yet — handler checks planID=="" separately and
	// 503s. The helper itself must still report ok=true so the caller can
	// distinguish "invalid plan name" (400) from "config missing" (503).
	pid, _, _, ok := planConfig("monthly", RazorpayConfig{})
	if !ok {
		t.Error("ok must stay true for a known plan even when ID is empty")
	}
	if pid != "" {
		t.Errorf("empty PlanIDMonthly must pass through as empty, got %q", pid)
	}
}

// ── subscriptionStatusBlocksNew (billing.go) ───────────────────────────────

func TestSubscriptionStatusBlocksNew(t *testing.T) {
	s := func(v string) *string { return &v }
	tests := []struct {
		in   *string
		want bool
	}{
		{nil, false},
		{s(""), false},
		{s("cancelled"), false},
		{s("halted"), false},
		{s("completed"), false},
		{s("expired"), false},
		{s("created"), false},  // short_url reserved, not yet authenticated — safe to replace
		{s("pending"), false},  // retry in flight, safe to replace
		{s("active"), true},
		{s("authenticated"), true},
		{s("  ACTIVE  "), true}, // whitespace + case insensitive
		{s("Authenticated"), true},
	}
	for _, tc := range tests {
		name := "nil"
		if tc.in != nil {
			name = *tc.in
		}
		got := subscriptionStatusBlocksNew(tc.in)
		if got != tc.want {
			t.Errorf("subscriptionStatusBlocksNew(%q) = %v, want %v", name, got, tc.want)
		}
	}
}

// ── buildHumanPlanLabel (dashboard.go) ─────────────────────────────────────

func TestBuildHumanPlanLabel_Nil(t *testing.T) {
	if got := buildHumanPlanLabel(nil); got != "" {
		t.Errorf("nil user should yield empty, got %q", got)
	}
}

func TestBuildHumanPlanLabel_Free(t *testing.T) {
	u := &User{PlanTier: "free"}
	got := buildHumanPlanLabel(u)
	if !strings.Contains(got, "Free tier") || !strings.Contains(got, "24h") {
		t.Errorf("free tier label missing expected text: %q", got)
	}
}

func TestBuildHumanPlanLabel_PaidMonthly(t *testing.T) {
	t0, _ := time.Parse(time.RFC3339, "2026-05-20T00:00:00Z")
	u := &User{PlanTier: "paid", PlanPeriod: "monthly", CurrentPeriodEnd: &t0}
	got := buildHumanPlanLabel(u)
	wantBits := []string{"Developer", "Monthly", "$12/mo", "active"}
	for _, s := range wantBits {
		if !strings.Contains(got, s) {
			t.Errorf("label %q missing %q", got, s)
		}
	}
	// Renewal date must NOT leak in the label.
	if strings.Contains(got, "2026-05-20") || strings.Contains(got, "renews") {
		t.Errorf("label must not expose renewal date, got %q", got)
	}
}

func TestBuildHumanPlanLabel_PaidAnnual(t *testing.T) {
	u := &User{PlanTier: "paid", PlanPeriod: "annual"}
	got := buildHumanPlanLabel(u)
	if !strings.Contains(got, "Annual") || !strings.Contains(got, "$120/yr") || !strings.Contains(got, "active") {
		t.Errorf("annual label missing expected: %q", got)
	}
}

func TestBuildHumanPlanLabel_Cancelled(t *testing.T) {
	s := "cancelled"
	t0, _ := time.Parse(time.RFC3339, "2026-06-01T00:00:00Z")
	u := &User{PlanTier: "paid", PlanPeriod: "monthly", SubscriptionStatus: &s, CurrentPeriodEnd: &t0}
	got := buildHumanPlanLabel(u)
	if !strings.Contains(got, "cancellation scheduled") {
		t.Errorf("cancelled label should tag scheduling, got %q", got)
	}
	if strings.Contains(got, "2026-06-01") || strings.Contains(got, "ends ") || strings.Contains(got, "renews") {
		t.Errorf("cancelled label must not expose dates, got %q", got)
	}
}

func TestBuildHumanPlanLabel_Halted(t *testing.T) {
	s := "halted"
	u := &User{PlanTier: "paid", PlanPeriod: "monthly", SubscriptionStatus: &s}
	got := buildHumanPlanLabel(u)
	if !strings.Contains(got, "payment halted") {
		t.Errorf("halted label missing warning: %q", got)
	}
}

// ── buildAvailableUpgrades (dashboard.go) ──────────────────────────────────

func TestBuildAvailableUpgrades_FreeSeeesBothPaths(t *testing.T) {
	u := &User{PlanTier: "free"}
	out := buildAvailableUpgrades(u)
	if len(out) != 2 {
		t.Fatalf("free tier should see 2 upgrades, got %d", len(out))
	}
	plans := []string{out[0]["plan"].(string), out[1]["plan"].(string)}
	if !(contains(plans, "monthly") && contains(plans, "annual")) {
		t.Errorf("free tier upgrades missing monthly or annual: %v", plans)
	}
}

func TestBuildAvailableUpgrades_NilUserDefaultsToFree(t *testing.T) {
	out := buildAvailableUpgrades(nil)
	if len(out) != 2 {
		t.Errorf("nil user should still see 2 upgrade paths (free behaviour), got %d", len(out))
	}
}

func TestBuildAvailableUpgrades_PaidMonthlySeesAnnual(t *testing.T) {
	u := &User{PlanTier: "paid", PlanPeriod: "monthly"}
	out := buildAvailableUpgrades(u)
	if len(out) != 1 {
		t.Fatalf("paid monthly should see 1 upgrade (annual), got %d", len(out))
	}
	if out[0]["plan"].(string) != "annual" {
		t.Errorf("paid monthly should see annual, got %v", out[0]["plan"])
	}
}

func TestBuildAvailableUpgrades_PaidAnnualSeesNothing(t *testing.T) {
	u := &User{PlanTier: "paid", PlanPeriod: "annual"}
	out := buildAvailableUpgrades(u)
	if len(out) != 0 {
		t.Errorf("paid annual should see 0 upgrades, got %d: %v", len(out), out)
	}
}

func TestBuildAvailableUpgrades_InstructionShape(t *testing.T) {
	u := &User{PlanTier: "free"}
	out := buildAvailableUpgrades(u)
	if len(out) == 0 {
		t.Fatal("need at least one upgrade for this test")
	}
	entry := out[0]
	// Required fields an agent would rely on.
	for _, k := range []string{"plan", "label", "price_usd", "billing_interval", "how_to_subscribe"} {
		if _, ok := entry[k]; !ok {
			t.Errorf("upgrade entry missing key %q: %v", k, entry)
		}
	}
	howto, ok := entry["how_to_subscribe"].(map[string]any)
	if !ok {
		t.Fatalf("how_to_subscribe must be a map, got %T", entry["how_to_subscribe"])
	}
	for _, k := range []string{"method", "url", "headers", "body", "response_field", "notes"} {
		if _, ok := howto[k]; !ok {
			t.Errorf("how_to_subscribe missing key %q: %v", k, howto)
		}
	}
	if howto["method"] != "POST" {
		t.Errorf("how_to_subscribe.method must be POST, got %v", howto["method"])
	}
}

// contains is a tiny helper — avoids importing slices.Contains which needs Go 1.21.
func contains(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}
