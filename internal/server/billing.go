package server

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ── Shared helpers used across orders / webhook / subscriptions ─────────────

func userIDFromNotes(notes map[string]interface{}) (uuid.UUID, bool) {
	v, _ := notes["user_id"].(string)
	if v == "" {
		return uuid.Nil, false
	}
	id, err := uuid.Parse(v)
	if err != nil {
		return uuid.Nil, false
	}
	return id, true
}

// unixToTime converts the Razorpay numeric field (JSON number → float64) into
// a time.Time. Returns zero time on missing/invalid input so callers can decide
// whether to persist NULL.
func unixToTime(v interface{}) time.Time {
	switch n := v.(type) {
	case float64:
		return time.Unix(int64(n), 0).UTC()
	case int64:
		return time.Unix(n, 0).UTC()
	}
	return time.Time{}
}

// periodFromSubscription reads notes.plan (we set it at creation) so we know
// "monthly" vs "annual" without another Razorpay API call.
func periodFromSubscription(subEntity map[string]interface{}) string {
	notes, _ := subEntity["notes"].(map[string]interface{})
	if v, _ := notes["plan"].(string); v == "annual" {
		return "annual"
	}
	return "monthly"
}

// Deprecated shim. Prefer POST /api/me/claim {token}. Kept so existing
// pricing-page deep links keep working. Respects the same tier rules as
// /api/me/claim — a FREE user calling this should NOT have their resource
// silently promoted to tier='paid' (the old behaviour).
func (s *server) handleMigrateResource(w http.ResponseWriter, r *http.Request) {
	// Bound platform-PG UPDATE to 5s.
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	user := s.authUser(r)
	if user == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Sign in required.")
		return
	}

	tokenStr := r.URL.Query().Get("token")
	if tokenStr == "" {
		writeError(w, http.StatusBadRequest, "missing_token", "Pass ?token=<uuid>.")
		return
	}

	token, err := uuid.Parse(tokenStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_token", "token must be a UUID.")
		return
	}

	if user.PlanTier == "paid" {
		_, err = s.db.ExecContext(ctx,
			`UPDATE resources SET migrated_to_user_id = $1, tier = 'paid', expires_at = NULL
			 WHERE token = $2 AND migrated_to_user_id IS NULL`,
			user.ID, token,
		)
	} else {
		// Free user: claim ownership only. Tier and expiry stay as-is.
		_, err = s.db.ExecContext(ctx,
			`UPDATE resources SET migrated_to_user_id = $1
			 WHERE token = $2 AND migrated_to_user_id IS NULL`,
			user.ID, token,
		)
	}
	if err != nil {
		slog.ErrorContext(r.Context(), "migrate: update failed", "error", err, "user_id", user.ID, "token", token)
		writeError(w, http.StatusInternalServerError, "internal_error", "Could not claim the token — please retry.")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "token": token.String()})
}

// ── Pure helpers (testable without DB / HTTP / Razorpay) ────────────────────

// planConfig maps the frontend-facing (plan, currency) pair to the Razorpay
// plan_id, display label, and total_count we send to subscription.create.
// total_count caps how many times Razorpay auto-renews before the subscription
// ends — 120 months = 10 years for monthly, 10 years for annual, both
// effectively "until the user cancels".
//
// Currency must be "USD" or "INR" (case-insensitive). An empty or unknown
// currency falls back to USD — this keeps the legacy single-currency flow
// working if a caller forgets to thread currency through. The returned label
// includes the currency so the dashboard and emails don't have to re-derive it.
//
// When a currency-specific plan id is not configured (e.g. the ops team hasn't
// set RAZORPAY_PLAN_ID_INR_MONTHLY yet) the USD fallback fields
// PlanIDMonthly / PlanIDAnnual are used — callers treat an empty planID as
// "not configured" and return 503, so a USD-only deployment still works.
func planConfig(plan, currency string, cfg RazorpayConfig) (planID, label string, totalCount int, ok bool) {
	cur := normalizeCurrency(currency)
	switch plan {
	case "monthly":
		return pickPlanID(cur, cfg.PlanIDUSDMonthly, cfg.PlanIDINRMonthly, cfg.PlanIDMonthly),
			"Developer · Monthly (" + cur + ")", 120, true
	case "annual":
		return pickPlanID(cur, cfg.PlanIDUSDYearly, cfg.PlanIDINRYearly, cfg.PlanIDAnnual),
			"Developer · Annual (" + cur + ")", 10, true
	}
	return "", "", 0, false
}

// normalizeCurrency canonicalises user input into the uppercase ISO code we
// persist and ship to Razorpay. Everything except INR collapses to USD so a
// stray "", "eur", or "INR " still lands on a valid plan id.
func normalizeCurrency(cur string) string {
	c := strings.ToUpper(strings.TrimSpace(cur))
	if c == "INR" {
		return "INR"
	}
	return "USD"
}

// isSupportedCurrency reports whether the request-time currency is one we can
// act on (USD or INR, case-insensitive). Callers should pre-check before
// normalizing so a bogus "EUR" is rejected with invalid_currency instead of
// silently coerced to USD.
func isSupportedCurrency(cur string) bool {
	c := strings.ToUpper(strings.TrimSpace(cur))
	return c == "USD" || c == "INR"
}

// pickPlanID returns the currency-specific plan id, or the USD fallback if
// the currency-specific id is empty. The fallback exists so a deploy that
// forgot to set RAZORPAY_PLAN_ID_INR_* still works (it'll just use the legacy
// USD plan id) — callers that really need INR will notice via the empty
// `notes.currency` path showing USD charges on their Razorpay dashboard.
func pickPlanID(currency, usdID, inrID, usdFallback string) string {
	if currency == "INR" && inrID != "" {
		return inrID
	}
	if currency == "USD" && usdID != "" {
		return usdID
	}
	return usdFallback
}

// subscriptionStatusBlocksNew reports whether an existing subscription in the
// given state should prevent a user from starting a new one. We only block
// when the subscription is materially live — `active` (currently charging)
// or `authenticated` (mandate completed, first charge imminent). `created`
// is Razorpay's "short_url reserved but nothing confirmed" state; `pending`
// means a retry is in flight. Either can be abandoned by starting fresh —
// forcing the user to cancel a never-authenticated subscription creates the
// dead-end UX we hit when a user's test card got declined mid-checkout.
// Match is case- and whitespace-insensitive.
func subscriptionStatusBlocksNew(status *string) bool {
	if status == nil {
		return false
	}
	s := strings.ToLower(strings.TrimSpace(*status))
	switch s {
	case "active", "authenticated":
		return true
	}
	return false
}
