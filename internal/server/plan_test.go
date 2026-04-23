package server

import (
	"strings"
	"testing"
	"time"
)

// ── planConfig (billing.go) ────────────────────────────────────────────────

func TestPlanConfig_Monthly(t *testing.T) {
	cfg := RazorpayConfig{PlanIDMonthly: "plan_M", PlanIDAnnual: "plan_A"}
	pid, label, count, ok := planConfig("monthly", "USD", cfg)
	if !ok || pid != "plan_M" || label != "Developer · Monthly (USD)" || count != 120 {
		t.Errorf("monthly: got (%q,%q,%d,%v)", pid, label, count, ok)
	}
}

func TestPlanConfig_Annual(t *testing.T) {
	cfg := RazorpayConfig{PlanIDMonthly: "plan_M", PlanIDAnnual: "plan_A"}
	pid, label, count, ok := planConfig("annual", "USD", cfg)
	if !ok || pid != "plan_A" || label != "Developer · Annual (USD)" || count != 10 {
		t.Errorf("annual: got (%q,%q,%d,%v)", pid, label, count, ok)
	}
}

func TestPlanConfig_UnknownPlan(t *testing.T) {
	for _, name := range []string{"", "weekly", "monthly ", "MONTHLY", "free", "pro"} {
		_, _, _, ok := planConfig(name, "USD", RazorpayConfig{PlanIDMonthly: "x", PlanIDAnnual: "y"})
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
	pid, _, _, ok := planConfig("monthly", "USD", RazorpayConfig{})
	if !ok {
		t.Error("ok must stay true for a known plan even when ID is empty")
	}
	if pid != "" {
		t.Errorf("empty PlanIDMonthly must pass through as empty, got %q", pid)
	}
}

// ── planConfig currency matrix ─────────────────────────────────────────────
//
// The currency-specific plan ids must be selected when they're configured;
// the legacy PlanIDMonthly / PlanIDAnnual remain USD fallbacks for deploys
// that haven't rolled out the USD-specific env vars yet.

func TestPlanConfig_USDMonthlyPicksUSDPlanID(t *testing.T) {
	cfg := RazorpayConfig{
		PlanIDMonthly:    "plan_legacy_M",
		PlanIDUSDMonthly: "plan_usd_M",
		PlanIDINRMonthly: "plan_inr_M",
	}
	pid, label, _, ok := planConfig("monthly", "USD", cfg)
	if !ok || pid != "plan_usd_M" {
		t.Errorf("USD monthly: got (%q,%v), want plan_usd_M", pid, ok)
	}
	if !strings.Contains(label, "(USD)") {
		t.Errorf("USD monthly label = %q, want to include (USD)", label)
	}
}

func TestPlanConfig_INRMonthlyPicksINRPlanID(t *testing.T) {
	cfg := RazorpayConfig{
		PlanIDMonthly:    "plan_legacy_M",
		PlanIDUSDMonthly: "plan_usd_M",
		PlanIDINRMonthly: "plan_inr_M",
	}
	pid, label, _, ok := planConfig("monthly", "INR", cfg)
	if !ok || pid != "plan_inr_M" {
		t.Errorf("INR monthly: got (%q,%v), want plan_inr_M", pid, ok)
	}
	if !strings.Contains(label, "(INR)") {
		t.Errorf("INR monthly label = %q, want to include (INR)", label)
	}
}

func TestPlanConfig_INRYearlyPicksINRPlanID(t *testing.T) {
	cfg := RazorpayConfig{
		PlanIDAnnual:    "plan_legacy_A",
		PlanIDUSDYearly: "plan_usd_A",
		PlanIDINRYearly: "plan_inr_A",
	}
	pid, _, count, ok := planConfig("annual", "INR", cfg)
	if !ok || pid != "plan_inr_A" || count != 10 {
		t.Errorf("INR annual: got (%q,%d,%v), want (plan_inr_A,10,true)", pid, count, ok)
	}
}

func TestPlanConfig_INRMissingFallsBackToLegacyUSD(t *testing.T) {
	// A deploy with no INR ids configured must not silently produce an empty
	// plan id — the fallback to the legacy USD field lets the server stay
	// online (callers see USD pricing at checkout, which is the correct
	// degraded behaviour given there's no INR plan yet).
	cfg := RazorpayConfig{PlanIDMonthly: "plan_legacy_M", PlanIDAnnual: "plan_legacy_A"}
	pid, _, _, _ := planConfig("monthly", "INR", cfg)
	if pid != "plan_legacy_M" {
		t.Errorf("INR monthly with no INR id = %q, want fallback plan_legacy_M", pid)
	}
	pid2, _, _, _ := planConfig("annual", "INR", cfg)
	if pid2 != "plan_legacy_A" {
		t.Errorf("INR annual with no INR id = %q, want fallback plan_legacy_A", pid2)
	}
}

func TestPlanConfig_USDMissingFallsBackToLegacy(t *testing.T) {
	cfg := RazorpayConfig{PlanIDMonthly: "plan_legacy_M", PlanIDAnnual: "plan_legacy_A"}
	pid, _, _, _ := planConfig("monthly", "USD", cfg)
	if pid != "plan_legacy_M" {
		t.Errorf("USD monthly with no USD id = %q, want fallback plan_legacy_M", pid)
	}
}

func TestPlanConfig_UnknownCurrencyCoercesToUSD(t *testing.T) {
	// A caller that forgets to normalize currency (e.g. "EUR") must land on
	// USD — never crash and never pick INR. Guard by only setting the USD
	// plan id; if "EUR" leaked through and hit INR lookup, pid would be
	// empty (not plan_usd_M).
	cfg := RazorpayConfig{PlanIDUSDMonthly: "plan_usd_M"}
	for _, cur := range []string{"", "EUR", "GBP", "xxx", "   "} {
		pid, label, _, ok := planConfig("monthly", cur, cfg)
		if !ok || pid != "plan_usd_M" {
			t.Errorf("currency %q: got (%q,%v), want plan_usd_M", cur, pid, ok)
		}
		if !strings.Contains(label, "(USD)") {
			t.Errorf("currency %q: label %q should collapse to (USD)", cur, label)
		}
	}
}

// ── normalizeCurrency / isSupportedCurrency ────────────────────────────────

func TestNormalizeCurrency(t *testing.T) {
	tests := map[string]string{
		"":         "USD",
		"USD":      "USD",
		"usd":      "USD",
		" INR ":    "INR",
		"inr":      "INR",
		"EUR":      "USD", // unknown → USD by design
		"gbp":      "USD",
		"nonsense": "USD",
	}
	for in, want := range tests {
		if got := normalizeCurrency(in); got != want {
			t.Errorf("normalizeCurrency(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsSupportedCurrency(t *testing.T) {
	for _, ok := range []string{"USD", "usd", "INR", "inr", " USD ", " INR "} {
		if !isSupportedCurrency(ok) {
			t.Errorf("%q should be supported", ok)
		}
	}
	for _, nope := range []string{"", "EUR", "GBP", "xxx", "   ", "USDT"} {
		if isSupportedCurrency(nope) {
			t.Errorf("%q should be rejected", nope)
		}
	}
}

// ── planPriceLabels ────────────────────────────────────────────────────────

func TestPlanPriceLabels_USD(t *testing.T) {
	m, a := planPriceLabels("USD")
	if !strings.Contains(m, "$12/mo") || !strings.Contains(a, "$120/yr") {
		t.Errorf("USD labels = (%q,%q), want $12/mo + $120/yr", m, a)
	}
}

func TestPlanPriceLabels_INR(t *testing.T) {
	m, a := planPriceLabels("INR")
	if !strings.Contains(m, "₹200/mo") || !strings.Contains(a, "₹2,199/yr") {
		t.Errorf("INR labels = (%q,%q), want ₹200/mo + ₹2,199/yr", m, a)
	}
}

func TestPlanPriceLabels_LegacyFallsBackToUSD(t *testing.T) {
	// Users whose plan_currency column is still NULL (pre-dual-currency
	// subscribers) must see the USD strings, not a blank or INR label.
	m, a := planPriceLabels("")
	if !strings.Contains(m, "$12") || !strings.Contains(a, "$120") {
		t.Errorf("empty currency = (%q,%q), want USD fallback", m, a)
	}
}

// ── buildHumanPlanLabel with INR plan_currency ────────────────────────────

func TestBuildHumanPlanLabel_INRMonthly(t *testing.T) {
	cur := "INR"
	u := &User{PlanTier: "paid", PlanPeriod: "monthly", PlanCurrency: &cur}
	got := buildHumanPlanLabel(u)
	if !strings.Contains(got, "₹200/mo") {
		t.Errorf("INR monthly label = %q, want to contain ₹200/mo", got)
	}
	if strings.Contains(got, "$12") {
		t.Errorf("INR label must not leak USD pricing: %q", got)
	}
}

func TestBuildHumanPlanLabel_INRAnnual(t *testing.T) {
	cur := "INR"
	u := &User{PlanTier: "paid", PlanPeriod: "annual", PlanCurrency: &cur}
	got := buildHumanPlanLabel(u)
	if !strings.Contains(got, "₹2,199/yr") {
		t.Errorf("INR annual label = %q, want to contain ₹2,199/yr", got)
	}
}

func TestBuildHumanPlanLabel_INRSwitching(t *testing.T) {
	cur := "INR"
	target := "annual"
	u := &User{
		PlanTier:          "paid",
		PlanPeriod:        "monthly",
		PlanCurrency:      &cur,
		PendingPlanChange: &target,
	}
	got := buildHumanPlanLabel(u)
	if !strings.Contains(got, "switching to") || !strings.Contains(got, "₹2,199/yr") {
		t.Errorf("INR switching label = %q, want INR target price", got)
	}
	if strings.Contains(got, "$") {
		t.Errorf("INR switching label must not leak USD: %q", got)
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
		{s("created"), false}, // short_url reserved, not yet authenticated — safe to replace
		{s("pending"), false}, // retry in flight, safe to replace
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
	out := buildAvailableUpgrades("https://api.example.test", u)
	if len(out) != 2 {
		t.Fatalf("free tier should see 2 upgrades, got %d", len(out))
	}
	plans := []string{out[0]["plan"].(string), out[1]["plan"].(string)}
	if !(contains(plans, "monthly") && contains(plans, "annual")) {
		t.Errorf("free tier upgrades missing monthly or annual: %v", plans)
	}
}

func TestBuildAvailableUpgrades_NilUserDefaultsToFree(t *testing.T) {
	out := buildAvailableUpgrades("https://api.example.test", nil)
	if len(out) != 2 {
		t.Errorf("nil user should still see 2 upgrade paths (free behaviour), got %d", len(out))
	}
}

func TestBuildAvailableUpgrades_PaidMonthlySeesAnnual(t *testing.T) {
	u := &User{PlanTier: "paid", PlanPeriod: "monthly"}
	out := buildAvailableUpgrades("https://api.example.test", u)
	if len(out) != 1 {
		t.Fatalf("paid monthly should see 1 upgrade (annual), got %d", len(out))
	}
	if out[0]["plan"].(string) != "annual" {
		t.Errorf("paid monthly should see annual, got %v", out[0]["plan"])
	}
}

func TestBuildAvailableUpgrades_PaidAnnualSeesNothing(t *testing.T) {
	u := &User{PlanTier: "paid", PlanPeriod: "annual"}
	out := buildAvailableUpgrades("https://api.example.test", u)
	if len(out) != 0 {
		t.Errorf("paid annual should see 0 upgrades, got %d: %v", len(out), out)
	}
}

func TestBuildAvailableUpgrades_InstructionShape(t *testing.T) {
	u := &User{PlanTier: "free"}
	out := buildAvailableUpgrades("https://api.example.test", u)
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
