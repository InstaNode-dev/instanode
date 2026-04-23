package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Plan-switch feature (monthly ↔ annual).
//
// Razorpay has no native "change plan" primitive — the only way to move a
// subscription to a different plan is to cancel the current one and create
// a new one. We do that at the *boundary* of the current billing period so
// the user is never double-charged and never loses the period they already
// paid for. The request-time handler just records the intent; the reconciler
// performs the Razorpay side effects when the effective timestamp passes.
//
// Lifecycle:
//
//   request-time (handleChangePlan):
//     users.pending_plan_change         ← "monthly" | "annual"
//     users.pending_plan_effective_at   ← users.current_period_end
//     users.plan_switch_scheduled_email_sent_at  ← NULL (re-armed)
//     users.plan_switch_activated_email_sent_at  ← NULL (re-armed)
//     → SendAsync(planSwitchScheduledEmail) via claim-lock
//
//   reconciler tick (promotePendingPlanSwitches):
//     when pending_plan_effective_at <= NOW() AND pending_plan_sub_id IS NULL:
//       - cancel current Razorpay subscription
//       - create new Razorpay subscription with notes.purpose="plan_switch"
//       - stamp users.pending_plan_sub_id = new_sub_id
//     (the new sub charges on its own schedule; activation arrives as webhook)
//
//   webhook-time (handleSubscriptionCharged, notes.purpose="plan_switch"):
//     users.razorpay_subscription_id  ← new_sub_id
//     users.plan_period               ← notes.plan
//     users.pending_plan_change       ← NULL
//     users.pending_plan_effective_at ← NULL
//     users.pending_plan_sub_id       ← NULL
//     → SendAsync(planSwitchActivatedEmail) via claim-lock
//     + the normal receiptEmail fires through the existing subscription.charged path.
//
//   cancel-pending (handleCancelPlanChange):
//     users.pending_plan_* ← NULL
//     → optional SendAsync(planSwitchCancelledEmail) if scheduled email had fired
//
// The request-time path is 100% DB; no Razorpay I/O. That keeps the handler
// fast, side-effect-free on partial failures, and lets us ship tests without
// stubbing the Razorpay SDK.

// ── Pure decision helpers ───────────────────────────────────────────────────

// planSwitchDecision classifies the outcome of a plan-switch request so the
// handler can translate it into (HTTP status, error code, message) without
// branching on config internals. The pure function keeps that mapping
// testable in isolation.
type planSwitchDecision int

const (
	planSwitchOK planSwitchDecision = iota
	planSwitchFeatureOff                    // 404
	planSwitchInvalidTarget                 // 400
	planSwitchNotActive                     // 409 (no active subscription)
	planSwitchAlreadyOnPlan                 // 409 (target == current)
	planSwitchAlreadyPending                // 409 (another switch in flight)
)

// decidePlanSwitchRequest is the pure core of POST /billing/change-plan.
// All inputs come from the User row (or request body) — no DB, no HTTP.
func decidePlanSwitchRequest(
	featureEnabled bool,
	currentPeriod string,
	subscriptionStatus *string,
	pendingPlanChange *string,
	target string,
) planSwitchDecision {
	if !featureEnabled {
		return planSwitchFeatureOff
	}
	target = strings.ToLower(strings.TrimSpace(target))
	if target != "monthly" && target != "annual" {
		return planSwitchInvalidTarget
	}
	status := ""
	if subscriptionStatus != nil {
		status = strings.ToLower(strings.TrimSpace(*subscriptionStatus))
	}
	if status != "active" {
		return planSwitchNotActive
	}
	if pendingPlanChange != nil && strings.TrimSpace(*pendingPlanChange) != "" {
		return planSwitchAlreadyPending
	}
	if strings.EqualFold(currentPeriod, target) {
		return planSwitchAlreadyOnPlan
	}
	return planSwitchOK
}

// decideCancelPlanSwitch is the pure core of DELETE /billing/change-plan.
// Returns true when the DB should clear the pending columns + optionally
// send the cancelled-switch email.
type cancelPlanSwitchDecision int

const (
	cancelSwitchOK            cancelPlanSwitchDecision = iota
	cancelSwitchFeatureOff                             // 404
	cancelSwitchNothingPending                         // 409 (no pending_plan_change)
	cancelSwitchAlreadyFired                           // 409 (reconciler already created new sub — too late)
)

func decideCancelPlanSwitch(
	featureEnabled bool,
	pendingPlanChange *string,
	pendingPlanSubID *string,
) cancelPlanSwitchDecision {
	if !featureEnabled {
		return cancelSwitchFeatureOff
	}
	if pendingPlanChange == nil || strings.TrimSpace(*pendingPlanChange) == "" {
		return cancelSwitchNothingPending
	}
	if pendingPlanSubID != nil && strings.TrimSpace(*pendingPlanSubID) != "" {
		return cancelSwitchAlreadyFired
	}
	return cancelSwitchOK
}

// shouldPromotePendingSwitch tells the reconciler whether to act on a user
// row right now. Only true when the effective timestamp has passed AND the
// reconciler has not yet created the new Razorpay subscription.
func shouldPromotePendingSwitch(now time.Time, effectiveAt *time.Time, pendingSubID *string) bool {
	if effectiveAt == nil {
		return false
	}
	if pendingSubID != nil && strings.TrimSpace(*pendingSubID) != "" {
		return false
	}
	return !now.Before(*effectiveAt)
}

// ── Handlers ────────────────────────────────────────────────────────────────

type changePlanRequest struct {
	To string `json:"to"` // "monthly" | "annual"
}

type changePlanResponse struct {
	OK                     bool       `json:"ok"`
	From                   string     `json:"from"`
	To                     string     `json:"to"`
	PendingPlanChange      string     `json:"pending_plan_change"`
	PendingPlanEffectiveAt *time.Time `json:"pending_plan_effective_at"`
	Message                string     `json:"message"`
}

func (s *server) handleChangePlan(w http.ResponseWriter, r *http.Request) {
	// DB updates only; no Razorpay I/O here. 5s is generous for the three
	// short UPDATE statements we fire plus the scheduled-email claim UPDATE.
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	if !s.cfg.Features.EnablePlanSwitch {
		// 404 (not 403) so agents that probe the endpoint with the feature
		// off can't tell it exists and wait for it to flip on.
		http.NotFound(w, r)
		return
	}

	user := s.authUser(r)
	if user == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Sign in required.")
		return
	}

	var req changePlanRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "Request body must be JSON with a 'to' field.")
		return
	}
	target := strings.ToLower(strings.TrimSpace(req.To))

	decision := decidePlanSwitchRequest(
		s.cfg.Features.EnablePlanSwitch,
		user.PlanPeriod,
		user.SubscriptionStatus,
		user.PendingPlanChange,
		target,
	)
	switch decision {
	case planSwitchFeatureOff:
		http.NotFound(w, r)
		return
	case planSwitchInvalidTarget:
		writeError(w, http.StatusBadRequest, "invalid_plan", "'to' must be 'monthly' or 'annual'.")
		return
	case planSwitchNotActive:
		writeError(w, http.StatusConflict, "not_active",
			"You need an active subscription before scheduling a plan switch.")
		return
	case planSwitchAlreadyOnPlan:
		writeError(w, http.StatusConflict, "already_on_plan",
			"You're already on that plan.")
		return
	case planSwitchAlreadyPending:
		writeError(w, http.StatusConflict, "switch_pending",
			"A plan switch is already scheduled. Cancel it first with DELETE /billing/change-plan.")
		return
	}

	// Atomic write: if current_period_end is NULL (edge case on historical
	// rows) fall back to now + <period length> so the switch doesn't fire
	// immediately with a NULL effective timestamp. pending_plan_sub_id stays
	// NULL; the reconciler fills it once the new Razorpay sub is created.
	var effectiveAt time.Time
	if user.CurrentPeriodEnd != nil {
		effectiveAt = *user.CurrentPeriodEnd
	} else if user.PlanPeriod == "annual" {
		effectiveAt = time.Now().AddDate(1, 0, 0).UTC()
	} else {
		effectiveAt = time.Now().AddDate(0, 1, 0).UTC()
	}

	_, err := s.db.ExecContext(ctx, `
		UPDATE users
		   SET pending_plan_change                   = $1,
		       pending_plan_effective_at             = $2,
		       pending_plan_sub_id                   = NULL,
		       plan_switch_scheduled_email_sent_at   = NULL,
		       plan_switch_activated_email_sent_at   = NULL
		 WHERE id                                    = $3`,
		target, effectiveAt, user.ID,
	)
	if err != nil {
		slog.ErrorContext(r.Context(), "change-plan: persist failed",
			"error", err, "user_id", user.ID, "target", target)
		writeError(w, http.StatusInternalServerError, "internal_error",
			"Could not schedule the switch — please retry in a moment.")
		return
	}

	sendPlanSwitchScheduledIfUnsent(ctx, s.db, s.email, user.ID)

	slog.InfoContext(r.Context(), "change-plan: scheduled",
		"user_id", user.ID, "from", user.PlanPeriod, "to", target,
		"effective_at", effectiveAt.Format(time.RFC3339))

	writeJSON(w, http.StatusOK, changePlanResponse{
		OK:                     true,
		From:                   user.PlanPeriod,
		To:                     target,
		PendingPlanChange:      target,
		PendingPlanEffectiveAt: &effectiveAt,
		Message:                "Plan switch scheduled. Your current plan keeps running until the effective date.",
	})
}

func (s *server) handleCancelPlanChange(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	if !s.cfg.Features.EnablePlanSwitch {
		http.NotFound(w, r)
		return
	}

	user := s.authUser(r)
	if user == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Sign in required.")
		return
	}

	decision := decideCancelPlanSwitch(
		s.cfg.Features.EnablePlanSwitch,
		user.PendingPlanChange,
		user.PendingPlanSubID,
	)
	switch decision {
	case cancelSwitchFeatureOff:
		http.NotFound(w, r)
		return
	case cancelSwitchNothingPending:
		writeError(w, http.StatusConflict, "no_pending_switch",
			"There's no pending plan switch to cancel.")
		return
	case cancelSwitchAlreadyFired:
		writeError(w, http.StatusConflict, "switch_already_fired",
			"The switch has already been initiated at the billing provider — email contact@instanode.dev if you need to reverse it.")
		return
	}

	// Read the scheduled-email timestamp inside the same UPDATE so we
	// only send the cancelled-switch email when the user was actually
	// told the switch was coming. RETURNING keeps this to a single round-trip.
	var scheduledAt *time.Time
	var droppedTarget *string
	err := s.db.QueryRowContext(ctx, `
		UPDATE users
		   SET pending_plan_change                   = NULL,
		       pending_plan_effective_at             = NULL,
		       pending_plan_sub_id                   = NULL
		 WHERE id                                    = $1
		   AND pending_plan_change IS NOT NULL
		   AND pending_plan_sub_id IS NULL
		RETURNING plan_switch_scheduled_email_sent_at, $2::text`,
		user.ID,
		derefOr(user.PendingPlanChange, ""),
	).Scan(&scheduledAt, &droppedTarget)
	if err == sql.ErrNoRows {
		// Race: someone else cleared it (e.g. reconciler just fired).
		writeError(w, http.StatusConflict, "switch_already_fired",
			"The switch has just been initiated — email contact@instanode.dev to reverse it.")
		return
	}
	if err != nil {
		slog.ErrorContext(r.Context(), "change-plan: cancel persist failed",
			"error", err, "user_id", user.ID)
		writeError(w, http.StatusInternalServerError, "internal_error",
			"Could not cancel the switch — please retry.")
		return
	}

	if s.email != nil && scheduledAt != nil && user.Email != "" {
		dropped := planLabelFor(safeDeref(droppedTarget))
		staying := planLabelFor(user.PlanPeriod)
		subject, html := planSwitchCancelledEmail(staying, dropped)
		s.email.SendAsync(user.Email, subject, html)
	}

	slog.InfoContext(r.Context(), "change-plan: cancelled",
		"user_id", user.ID, "was_dropping_to", safeDeref(droppedTarget))

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":                        true,
		"pending_plan_change":       nil,
		"pending_plan_effective_at": nil,
		"message":                   "Plan switch cancelled. Nothing changes on your current plan.",
	})
}

// ── Reconciler hook ─────────────────────────────────────────────────────────

// razorpaySubCreator abstracts the Razorpay SDK call so tests can stub it.
// Returns the new subscription id on success. currency is the user's locked-in
// ISO code ("USD" or "INR") so a plan switch never jumps currencies — even
// if the user's plan_currency row was somehow NULL'd, the caller normalizes
// to USD before calling us.
type razorpaySubCreator func(ctx context.Context, cfg RazorpayConfig, period, currency string, userID uuid.UUID) (string, error)

// razorpaySubCanceller abstracts the Razorpay cancel call so tests can stub it.
type razorpaySubCanceller func(ctx context.Context, cfg RazorpayConfig, subID string) error

// promotePendingPlanSwitches runs one reconciler pass over all users whose
// pending_plan_effective_at has elapsed but whose pending_plan_sub_id is
// still NULL. For each, it:
//
//  1. Cancels the current Razorpay subscription (best-effort — errors logged).
//  2. Creates a new subscription for the target plan.
//  3. Stamps pending_plan_sub_id so the tick is idempotent on retry.
//
// The activation email is NOT sent here — it's sent when Razorpay's
// subscription.activated / .charged webhook fires for the new sub, via
// sendPlanSwitchActivatedIfUnsent. Keeping the email off the reconciler
// path avoids races between the reconciler tick and the webhook.
//
// Returns the number of users promoted.
func promotePendingPlanSwitches(
	ctx context.Context,
	db *sql.DB,
	cfg *Config,
	cancelSub razorpaySubCanceller,
	createSub razorpaySubCreator,
	now time.Time,
) (int, error) {
	if !cfg.Features.EnablePlanSwitch {
		return 0, nil
	}

	rows, err := db.QueryContext(ctx, `
		SELECT id, razorpay_subscription_id, pending_plan_change, pending_plan_effective_at, plan_currency
		  FROM users
		 WHERE pending_plan_change     IS NOT NULL
		   AND pending_plan_effective_at IS NOT NULL
		   AND pending_plan_effective_at <= $1
		   AND pending_plan_sub_id     IS NULL`, now)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	type pending struct {
		userID      uuid.UUID
		oldSubID    *string
		target      string
		currency    string
		effectiveAt time.Time
	}
	var todo []pending
	for rows.Next() {
		var p pending
		var targetPtr *string
		var effectivePtr *time.Time
		var currencyPtr *string
		if err := rows.Scan(&p.userID, &p.oldSubID, &targetPtr, &effectivePtr, &currencyPtr); err != nil {
			continue
		}
		if targetPtr == nil || effectivePtr == nil {
			continue
		}
		p.target = *targetPtr
		p.effectiveAt = *effectivePtr
		// Legacy rows pre-dual-currency have plan_currency = NULL; fall back
		// to USD so those switches continue using the USD plan id pool.
		if currencyPtr != nil {
			p.currency = *currencyPtr
		}
		todo = append(todo, p)
	}

	promoted := 0
	for _, p := range todo {
		// Cancel the old sub first. Best-effort: if this errors we still
		// create the new one — the user's next action matters more than the
		// zombie sub at Razorpay (which our reconciler will clean up next
		// tick when it sees the sub state).
		if p.oldSubID != nil && *p.oldSubID != "" {
			if err := cancelSub(ctx, cfg.Razorpay, *p.oldSubID); err != nil {
				slog.Warn("plan switch: old sub cancel failed (continuing)",
					"error", err, "user_id", p.userID, "old_sub_id", *p.oldSubID)
			}
		}

		newSubID, err := createSub(ctx, cfg.Razorpay, p.target, p.currency, p.userID)
		if err != nil {
			slog.Error("plan switch: new sub create failed",
				"error", err, "user_id", p.userID, "target", p.target)
			continue
		}

		// Atomic stamp. The WHERE clause ensures we don't overwrite a
		// concurrently-promoted row if two reconciler ticks overlap.
		res, err := db.ExecContext(ctx, `
			UPDATE users
			   SET pending_plan_sub_id = $1
			 WHERE id                  = $2
			   AND pending_plan_sub_id IS NULL
			   AND pending_plan_change IS NOT NULL`, newSubID, p.userID)
		if err != nil {
			slog.Error("plan switch: stamp sub id failed",
				"error", err, "user_id", p.userID, "new_sub_id", newSubID)
			continue
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			slog.Info("plan switch: stamp was a no-op (concurrent tick won)",
				"user_id", p.userID, "new_sub_id", newSubID)
			continue
		}
		promoted++
		slog.Info("plan switch: new sub created, awaiting activation webhook",
			"user_id", p.userID, "target", p.target, "new_sub_id", newSubID)
	}
	return promoted, nil
}

// ── Live Razorpay SDK adapters ──────────────────────────────────────────────
//
// These are the production implementations of the razorpaySubCreator /
// razorpaySubCanceller func types. They wrap the razorpay-go SDK — which is
// why they live here and not in the pure-function test file — and carry the
// notes.purpose="plan_switch" tag that the webhook handler dispatches on.
//
// All Razorpay SDK calls are synchronous and ignore the passed context
// (razorpay-go doesn't accept a context.Context), so we run them in a
// bounded goroutine and abandon if the context deadline elapses. This matches
// the pattern in handleCreateSubscription.

// liveRazorpayCreateSub creates a brand-new subscription for the target plan.
// Returns the new subscription id. currency is the caller's locked-in currency
// (USD or INR); it selects the right Razorpay plan id pool and is stamped on
// notes.currency so the activation webhook preserves the lock-in on renewal.
func liveRazorpayCreateSub(ctx context.Context, cfg RazorpayConfig, period, currency string, userID uuid.UUID) (string, error) {
	planID, _, totalCount, ok := planConfig(period, currency, cfg)
	if !ok {
		return "", fmt.Errorf("plan switch: invalid target period %q", period)
	}
	if planID == "" {
		return "", fmt.Errorf("plan switch: plan_id not configured for %q", period)
	}
	notes := map[string]interface{}{
		"user_id":  userID.String(),
		"plan":     period,
		"currency": normalizeCurrency(currency),
		"purpose":  "plan_switch",
	}
	type subResult struct {
		data map[string]interface{}
		err  error
	}
	resCh := make(chan subResult, 1)
	go func() {
		client := newRazorpayClient(cfg)
		data, err := client.Subscription.Create(map[string]interface{}{
			"plan_id":         planID,
			"total_count":     totalCount,
			"customer_notify": 1,
			"notes":           notes,
		}, nil)
		resCh <- subResult{data: data, err: err}
	}()
	select {
	case r := <-resCh:
		if r.err != nil {
			return "", r.err
		}
		id, _ := r.data["id"].(string)
		if id == "" {
			return "", fmt.Errorf("razorpay: subscription.create returned no id")
		}
		return id, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// liveRazorpayCancelSub cancels the caller's *current* Razorpay subscription.
// cancel_at_cycle_end=0 cancels immediately; we pass 0 because the plan-switch
// flow only calls this once the period has already elapsed.
func liveRazorpayCancelSub(ctx context.Context, cfg RazorpayConfig, subID string) error {
	if subID == "" {
		return nil
	}
	type cancelResult struct{ err error }
	resCh := make(chan cancelResult, 1)
	go func() {
		client := newRazorpayClient(cfg)
		_, err := client.Subscription.Cancel(subID, map[string]interface{}{"cancel_at_cycle_end": 0}, nil)
		resCh <- cancelResult{err: err}
	}()
	select {
	case r := <-resCh:
		return r.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ── helpers ─────────────────────────────────────────────────────────────────

func derefOr(p *string, fallback string) string {
	if p == nil {
		return fallback
	}
	return *p
}

func safeDeref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
