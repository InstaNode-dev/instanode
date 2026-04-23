package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

type Resource struct {
	ID         uuid.UUID `json:"id"`
	Token      uuid.UUID `json:"token"`
	Type       string    `json:"type"`
	Name       string    `json:"name"`
	Tier       string    `json:"tier"`
	Status     string    `json:"status"`
	CreatedAt  time.Time `json:"created_at"`
	ExpiresAt  *time.Time `json:"expires_at"`
}

func (s *server) handleGetResources(w http.ResponseWriter, r *http.Request) {
	// Bound platform-PG query to 5s.
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	user := s.authUser(r)
	if user == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Sign in required.")
		return
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT id, token, resource_type, name, tier, status, created_at, expires_at
		FROM resources
		WHERE migrated_to_user_id = $1
		  AND status NOT IN ('deleted', 'reaped', 'expired')
		ORDER BY created_at DESC`, user.ID)
	if err != nil {
		slog.ErrorContext(r.Context(), "resources: query failed", "error", err, "user_id", user.ID)
		writeError(w, http.StatusInternalServerError, "internal_error", "Could not load your resources — please retry.")
		return
	}
	defer rows.Close()

	var resources []Resource
	for rows.Next() {
		var r Resource
		err := rows.Scan(&r.ID, &r.Token, &r.Type, &r.Name, &r.Tier, &r.Status, &r.CreatedAt, &r.ExpiresAt)
		if err != nil {
			continue
		}
		resources = append(resources, r)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resources)
}

// ── GET /api/me/plan ────────────────────────────────────────────────────────
//
// Dedicated plan endpoint the dashboard polls to render the big plan banner.
// Lives separately from /auth/me so the UI can refresh just the plan block
// after a subscribe/cancel without re-fetching the whole user/resources graph.
//
// human_label is a pre-formatted one-liner ready to paste into the banner —
// dashboards don't have to re-implement the status/period/renewal stitching.
func (s *server) handleGetPlan(w http.ResponseWriter, r *http.Request) {
	user := s.authUser(r)
	if user == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Sign in required.")
		return
	}

	humanLabel := buildHumanPlanLabel(user)
	upgrades := buildAvailableUpgrades(s.baseURL, user)

	payload := map[string]any{
		"plan_tier":                user.PlanTier,
		"plan_period":              user.PlanPeriod,
		"plan_paid_at":             user.PlanPaidAt,
		"subscription_status":      user.SubscriptionStatus,
		"current_period_end":       user.CurrentPeriodEnd,
		"razorpay_subscription_id": user.RazorpaySubscriptionID,
		"plan_currency":            user.PlanCurrency,
		"human_label":              humanLabel,
		"available_upgrades":       upgrades,
	}

	// Plan-switch fields only appear when the feature flag is on. Dashboards
	// running against an older server (flag off) see the stable legacy shape
	// — no UI surface to probe, so the feature stays fully hidden.
	if s.cfg.Features.EnablePlanSwitch {
		payload["pending_plan_change"] = user.PendingPlanChange
		payload["pending_plan_effective_at"] = user.PendingPlanEffectiveAt
	}

	writeJSON(w, http.StatusOK, payload)
}

// ── GET /api/me/token ───────────────────────────────────────────────────────
//
// Returns a freshly-signed JWT the user can paste into `Authorization: Bearer …`
// for CLI / agent calls against /db/new, /webhook/new, and /api/me/claim.
// The token is the same shape as the session cookie (30-day TTL), so
// rotating JWT_SECRET revokes every outstanding key.
func (s *server) handleGetAPIToken(w http.ResponseWriter, r *http.Request) {
	user := s.authUser(r)
	if user == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Sign in required.")
		return
	}
	tok, err := s.generateJWT(user.ID)
	if err != nil {
		slog.ErrorContext(r.Context(), "token: generate failed", "error", err, "user_id", user.ID)
		writeError(w, http.StatusInternalServerError, "internal_error", "Could not mint a token — please retry.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"token":      tok,
		"expires_in": int(jwtTTL.Seconds()),
	})
}

// ── DELETE /api/me/resources/{token} ───────────────────────────────────────
//
// Soft-deletes one of the caller's resources. Paid-tier only — free-tier
// resources auto-expire in 24h so they don't need this; and we don't want
// an open endpoint that could be used to churn the provisioner faster than
// the rate limiter can reason about.
//
// The actual drop of the underlying database (and purge of stored webhook
// payloads) happens in the background from the reaper loop, typically
// within the next 5-minute tick. The response returns as soon as
// status='deleted' + deleted_at=NOW() has been written; the resource
// immediately stops appearing on the user's dashboard.
func (s *server) handleDeleteResource(w http.ResponseWriter, r *http.Request) {
	// Bound platform-PG UPDATE ... RETURNING to 5s.
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	user := s.authUser(r)
	if user == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Sign in required.")
		return
	}
	tokenStr := r.PathValue("token")
	tokenUUID, perr := uuid.Parse(strings.TrimSpace(tokenStr))
	if perr != nil {
		writeError(w, http.StatusBadRequest, "invalid_token", "token must be a UUID.")
		return
	}

	if user.PlanTier != "paid" {
		// Include the resource's own token in the upgrade URL so that when
		// the user pays, the webhook (`notes.token` path) atomically claims
		// THIS resource into their account in addition to flipping them to
		// paid — so after upgrade the delete can actually succeed.
		writeJSON(w, http.StatusForbidden, map[string]any{
			"ok":          false,
			"error":       "paid_tier_only",
			"message":     "Delete is a Developer-tier feature. Free-tier resources auto-expire in 24 hours — upgrade to remove them on demand.",
			"upgrade_url": s.marketingURL + pathMarketingPricingPage + "?token=" + tokenUUID.String(),
		})
		return
	}

	// Single UPDATE handles ownership, status check, and soft-delete in one
	// round-trip. RETURNING tells us whether anything was actually updated.
	var id uuid.UUID
	err := s.db.QueryRowContext(ctx,
		`UPDATE resources
		 SET status = 'deleted', deleted_at = NOW()
		 WHERE token = $1
		   AND migrated_to_user_id = $2
		   AND status = 'active'
		 RETURNING id`,
		tokenUUID, user.ID,
	).Scan(&id)
	if err != nil {
		// Either the token doesn't exist, belongs to someone else, or is
		// already non-active. We don't distinguish — 404 in every case so
		// we never leak another user's resource state.
		writeError(w, http.StatusNotFound, "not_found", "No active resource with that token.")
		return
	}

	slog.InfoContext(r.Context(), "resource.soft_deleted",
		"user_id", user.ID, "token", tokenUUID.String())

	writeJSON(w, http.StatusAccepted, map[string]any{
		"ok":      true,
		"id":      id.String(),
		"token":   tokenUUID.String(),
		"status":  "deleted",
		"message": "Queued for deletion. The underlying database is removed within 5 minutes.",
	})
}

// ── POST /api/me/claim ──────────────────────────────────────────────────────
//
// Attach an anonymous resource to the authenticated user's account. Accepts a
// session cookie OR an Authorization: Bearer <JWT> header so CLI/agents can
// claim via the API. Body: {"token":"<uuid>"}.
//
// Idempotent — if the resource is already migrated to this user, returns 200
// with ok:true and the resource payload. If it belongs to a different user or
// doesn't exist, returns 404. Paid users get tier='paid' and expires_at=NULL;
// free-tier users get their existing anon resource reassigned without changing
// the tier.

type claimRequest struct {
	Token string `json:"token"`
}

// handleClaimPreview exposes an unauthenticated, read-only view of a resource
// so the /start?token=… landing page can render "this is a Postgres database
// named X, expires in 23h" before the visitor commits to signing in. The
// token itself is the capability — anyone holding it can already call POST
// /api/me/claim, so revealing these non-secret fields does not widen the
// attack surface. connection_url and owner identity are deliberately not
// returned; those still require the claim roundtrip or an authenticated
// session.
func (s *server) handleClaimPreview(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	raw := strings.TrimSpace(r.URL.Query().Get("token"))
	if raw == "" {
		writeError(w, http.StatusBadRequest, "missing_token", "token query parameter is required.")
		return
	}
	tokenUUID, perr := uuid.Parse(raw)
	if perr != nil {
		writeError(w, http.StatusBadRequest, "invalid_token", "token must be a UUID.")
		return
	}

	var (
		resourceType   string
		name           string
		tier           string
		status         string
		expiresAt      sql.NullTime
		createdAt      time.Time
		alreadyClaimed bool
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT resource_type, name, tier, status, expires_at, created_at,
		        migrated_to_user_id IS NOT NULL AS already_claimed
		 FROM resources WHERE token = $1`,
		tokenUUID,
	).Scan(&resourceType, &name, &tier, &status, &expiresAt, &createdAt, &alreadyClaimed)
	if err != nil {
		// sql.ErrNoRows and any other lookup error both collapse to 404 so
		// we don't leak whether the token exists-but-is-broken vs. never-was.
		writeError(w, http.StatusNotFound, "not_found", "No resource with that token.")
		return
	}
	if status != "active" {
		writeError(w, http.StatusGone, "resource_expired", "This resource has expired — provision a new one.")
		return
	}

	payload := map[string]any{
		"ok":              true,
		"resource_type":   resourceType,
		"name":            name,
		"tier":            tier,
		"status":          status,
		"created_at":      createdAt.UTC().Format(time.RFC3339),
		"already_claimed": alreadyClaimed,
	}
	if expiresAt.Valid {
		payload["expires_at"] = expiresAt.Time.UTC().Format(time.RFC3339)
		remaining := time.Until(expiresAt.Time)
		if remaining < 0 {
			remaining = 0
		}
		payload["expires_in_seconds"] = int64(remaining.Seconds())
	}
	writeJSON(w, http.StatusOK, payload)
}

func (s *server) handleClaimToken(w http.ResponseWriter, r *http.Request) {
	// Bound platform-PG SELECT + UPDATE to 5s.
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	user := s.authUser(r)
	if user == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Sign in required.")
		return
	}

	var req claimRequest
	if derr := json.NewDecoder(r.Body).Decode(&req); derr != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "Request body must be JSON with a 'token' field.")
		return
	}
	tokenUUID, perr := uuid.Parse(strings.TrimSpace(req.Token))
	if perr != nil {
		writeError(w, http.StatusBadRequest, "invalid_token", "token must be a UUID.")
		return
	}

	// Look up the resource. If owned by this user already, return 200.
	// If owned by someone else, return 404 (don't leak existence).
	var (
		id            uuid.UUID
		ownerID       *uuid.UUID
		resourceType  string
		name          string
		status        string
		tier          string
		connectionURL string
	)
	if err := s.db.QueryRowContext(ctx,
		`SELECT id, migrated_to_user_id, resource_type, name, status, tier, connection_url
		 FROM resources WHERE token = $1`, tokenUUID,
	).Scan(&id, &ownerID, &resourceType, &name, &status, &tier, &connectionURL); err != nil {
		writeError(w, http.StatusNotFound, "not_found", "No resource with that token.")
		return
	}
	if status != "active" {
		writeError(w, http.StatusGone, "resource_expired", "This resource has expired — provision a new one.")
		return
	}
	if ownerID != nil && *ownerID != user.ID {
		writeError(w, http.StatusNotFound, "not_found", "No resource with that token.")
		return
	}

	// Paid users get permanence; free users just attach the resource without
	// changing the tier (they can upgrade later and call /api/me/claim again
	// or hit /billing/migrate, both of which UPDATE the same row).
	newTier := tier
	clearExpiry := false
	if user.PlanTier == "paid" {
		newTier = "paid"
		clearExpiry = true
	}

	var qerr error
	if clearExpiry {
		_, qerr = s.db.ExecContext(ctx,
			`UPDATE resources SET migrated_to_user_id = $1, tier = $2, expires_at = NULL
			 WHERE token = $3 AND status = 'active'`,
			user.ID, newTier, tokenUUID,
		)
	} else {
		_, qerr = s.db.ExecContext(ctx,
			`UPDATE resources SET migrated_to_user_id = $1
			 WHERE token = $2 AND status = 'active'`,
			user.ID, tokenUUID,
		)
	}
	if qerr != nil {
		slog.ErrorContext(r.Context(), "claim: update failed", "error", qerr, "user_id", user.ID, "token", tokenUUID)
		writeError(w, http.StatusInternalServerError, "internal_error", "Could not claim the token — please retry.")
		return
	}

	slog.InfoContext(r.Context(), "claim.success", "user_id", user.ID, "token", tokenUUID.String(), "tier", newTier)

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":            true,
		"id":            id.String(),
		"token":         tokenUUID.String(),
		"resource_type": resourceType,
		"name":          name,
		"tier":          newTier,
		"status":        "active",
	})
}
// ── Pure helpers (testable without DB / HTTP) ───────────────────────────────

// buildHumanPlanLabel renders the one-line plan label surfaced on the
// dashboard banner. Branches on plan_tier / plan_period / subscription_status
// / current_period_end so the UI doesn't have to re-implement the logic.
//
// A pending plan switch is surfaced with its own suffix ("switching to <X>")
// so the dashboard shows the scheduled change at a glance without having to
// cross-reference pending_plan_change separately. It only takes effect when
// the subscription is still active — if the user cancelled, the cancellation
// wording takes priority.
func buildHumanPlanLabel(user *User) string {
	if user == nil {
		return ""
	}
	if user.PlanTier != "paid" {
		return "Free tier — resources expire in 24h"
	}
	// plan_currency drives the price shown in the label. NULL (legacy subs
	// that pre-date dual-currency) falls through to USD via the default
	// branch in planPriceLabels.
	currency := ""
	if user.PlanCurrency != nil {
		currency = *user.PlanCurrency
	}
	monthlyLabel, annualLabel := planPriceLabels(currency)
	periodLabel := monthlyLabel
	if user.PlanPeriod == "annual" {
		periodLabel = annualLabel
	}
	subStatus := ""
	if user.SubscriptionStatus != nil {
		subStatus = *user.SubscriptionStatus
	}
	label := "Developer · " + periodLabel + " · active"
	switch subStatus {
	case "cancelled":
		label = "Developer · " + periodLabel + " · cancellation scheduled"
		return label
	case "halted":
		label = "Developer · " + periodLabel + " · payment halted — update your card"
		return label
	}
	if user.PendingPlanChange != nil && *user.PendingPlanChange != "" {
		target := annualLabel
		if *user.PendingPlanChange == "monthly" {
			target = monthlyLabel
		}
		label = "Developer · " + periodLabel + " · switching to " + target
	}
	return label
}

// planPriceLabels returns the monthly and annual price strings for a given
// currency. INR uses the $200/₹2,199 pricing tier; USD (and any unknown
// currency, for legacy-row safety) uses $12/$120.
func planPriceLabels(currency string) (monthly, annual string) {
	switch normalizeCurrency(currency) {
	case "INR":
		return "Monthly · ₹200/mo", "Annual · ₹2,199/yr"
	}
	return "Monthly · $12/mo", "Annual · $120/yr"
}

// buildAvailableUpgrades returns the list of upgrade paths relevant to the
// caller's current plan. Surface area for LLM agents: each item is a
// self-describing instruction (method/url/body/auth) so the agent can
// subscribe without scraping docs. Free → monthly + annual; paid monthly →
// annual; paid annual → none (cancellation-only).
func buildAvailableUpgrades(baseURL string, user *User) []map[string]any {
	upgrades := []map[string]any{}
	instruction := func(plan, label string, price int, interval string) map[string]any {
		return map[string]any{
			"plan":             plan,
			"label":            label,
			"price_usd":        price,
			"billing_interval": interval,
			"how_to_subscribe": map[string]any{
				"method":         "POST",
				"url":            baseURL + pathAPIBillingSubscription,
				"headers":        map[string]string{"Authorization": "Bearer <INSTANODE_TOKEN>", "Content-Type": "application/json"},
				"body":           map[string]string{"plan": plan},
				"response_field": "short_url",
				"notes":          "Open short_url in a browser to complete the subscription (mandate + first charge).",
			},
		}
	}
	if user == nil || user.PlanTier != "paid" {
		upgrades = append(upgrades, instruction("monthly", "Developer · Monthly", 12, "month"))
		upgrades = append(upgrades, instruction("annual", "Developer · Annual (2 months free)", 120, "year"))
	} else if user.PlanPeriod == "monthly" {
		// Suppress the "subscribe to annual" upgrade path when a switch is
		// already scheduled — the user should cancel the scheduled switch
		// before initiating a brand-new subscription.
		if user.PendingPlanChange == nil || *user.PendingPlanChange == "" {
			upgrades = append(upgrades, instruction("annual", "Developer · Annual (2 months free)", 120, "year"))
		}
	}
	return upgrades
}
