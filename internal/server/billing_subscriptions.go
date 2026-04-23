package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
)

type CreateSubscriptionRequest struct {
	Plan     string `json:"plan"`               // "monthly" | "annual"
	Currency string `json:"currency,omitempty"` // "USD" | "INR" — empty ⇒ USD
	Token    string `json:"token,omitempty"`    // optional anon resource token to claim on first charge
}

type CreateSubscriptionResponse struct {
	SubscriptionID string `json:"subscription_id"`
	ShortURL       string `json:"short_url"`
	KeyID          string `json:"key_id"`
	PlanLabel      string `json:"plan_label"`
}

// handleCreateSubscription creates a Razorpay Subscription for the logged-in
// user and persists subscription_id + status='created' on the user row. The
// returned short_url can be used directly (hosted Razorpay page) or fed into
// Razorpay Checkout.js as options.subscription_id.
func (s *server) handleCreateSubscription(w http.ResponseWriter, r *http.Request) {
	user := s.authUser(r)
	if user == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Sign in required.")
		return
	}

	var req CreateSubscriptionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "Request body must be JSON.")
		return
	}

	// Reject unknown currencies up-front. Empty is allowed (falls back to USD).
	if req.Currency != "" && !isSupportedCurrency(req.Currency) {
		writeError(w, http.StatusBadRequest, "invalid_currency", "currency must be 'USD' or 'INR'.")
		return
	}
	currency := normalizeCurrency(req.Currency)

	// Block double-subscribe: if the user has an active / pending subscription
	// already, point them at cancel-then-resubscribe rather than silently
	// creating a second one. Only when the prior sub is cancelled/completed/
	// halted is a new subscribe allowed.
	if subscriptionStatusBlocksNew(user.SubscriptionStatus) {
		writeError(w, http.StatusConflict, "already_subscribed",
			"You already have a subscription. Cancel the current one before starting a new one.")
		return
	}

	// Currency lock-in: if the user already has a paid plan_currency (from a
	// prior subscription cycle, even one that's since been cancelled), a new
	// subscribe must stay in the same currency. Mixing is rejected — not to
	// punish the user but because Razorpay plan ids are bound to a single
	// currency and a USD subscriber's saved card likely can't charge INR.
	if user.PlanCurrency != nil && *user.PlanCurrency != "" && *user.PlanCurrency != currency {
		writeError(w, http.StatusBadRequest, "cannot_change_currency",
			"Your account is already on a "+*user.PlanCurrency+" plan. Changing currency is not supported — contact support if you need to.")
		return
	}

	planID, planLabel, totalCount, ok := planConfig(req.Plan, currency, s.cfg.Razorpay)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_plan", "plan must be 'monthly' or 'annual'.")
		return
	}
	if planID == "" {
		writeError(w, http.StatusServiceUnavailable, "plan_not_configured", "Billing is not fully configured — contact support.")
		return
	}

	notes := map[string]interface{}{
		"user_id":  user.ID.String(),
		"plan":     req.Plan,
		"currency": currency,
	}
	if req.Token != "" {
		if _, err := uuid.Parse(req.Token); err == nil {
			notes["token"] = req.Token
		}
	}

	// Payment provider call with timing checkpoints so we can distinguish
	// "SDK never returned" vs "provider responded slowly" vs "outbound
	// blocked at the container level" in production logs. The Payment
	// impl runs the SDK in a goroutine and selects on ctx.Done so the
	// 15s bound below actually lands on the wire.
	callStart := time.Now()
	slog.InfoContext(r.Context(), "payment subscription create: starting",
		"user_id", user.ID, "plan", req.Plan, "plan_id", planID)

	subCtx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	sub, err := s.payment.CreateSubscription(subCtx, map[string]interface{}{
		"plan_id":         planID,
		"total_count":     totalCount,
		"customer_notify": 1,
		"notes":           notes,
	})
	elapsed := time.Since(callStart)
	if err != nil {
		if subCtx.Err() == context.DeadlineExceeded {
			slog.ErrorContext(r.Context(), "payment subscription create timeout",
				"user_id", user.ID, "plan", req.Plan, "elapsed_ms", elapsed.Milliseconds())
			writeError(w, http.StatusGatewayTimeout, "payment_gateway_timeout",
				"Payment provider took too long to respond. Please retry in a few seconds.")
			return
		}
		slog.ErrorContext(r.Context(), "payment subscription create failed",
			"error", err, "user_id", user.ID, "plan", req.Plan, "elapsed_ms", elapsed.Milliseconds())
		writeError(w, http.StatusBadGateway, "payment_gateway_error",
			"Payment provider returned an error — please try again in a moment. If the problem persists, contact support.")
		return
	}
	slog.InfoContext(r.Context(), "payment subscription create: ok",
		"user_id", user.ID, "plan", req.Plan, "elapsed_ms", elapsed.Milliseconds())

	subID, _ := sub["id"].(string)
	shortURL, _ := sub["short_url"].(string)

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	// Clear cancel_email_sent_at when a new subscription attaches so a later
	// cancel on this fresh sub still triggers a cancellation email (the claim
	// lock is per-sub-lifecycle, not lifetime).
	// plan_currency uses COALESCE so the first subscription locks in the
	// currency, and later re-subscribes (after a cancel) can't flip it —
	// defence in depth behind the explicit check above.
	if _, err := s.db.ExecContext(ctx,
		`UPDATE users
		    SET razorpay_subscription_id = $1,
		        subscription_status      = 'created',
		        plan_period              = $2,
		        plan_currency            = COALESCE(plan_currency, $3),
		        cancel_email_sent_at     = NULL
		  WHERE id                        = $4`,
		subID, req.Plan, currency, user.ID,
	); err != nil {
		slog.ErrorContext(r.Context(), "persist subscription_id failed", "error", err, "user_id", user.ID, "sub_id", subID)
	}

	writeJSON(w, http.StatusOK, CreateSubscriptionResponse{
		SubscriptionID: subID,
		ShortURL:       shortURL,
		KeyID:          s.cfg.Razorpay.KeyID,
		PlanLabel:      planLabel,
	})
}

// handleSubscriptionCharged runs on both subscription.activated (first charge)
// and subscription.charged (recurring). Promotes the user to paid, rolls forward
// current_period_end, sends a receipt.
//
// Plan-switch branch: when notes.purpose == "plan_switch", this is the first
// charge on the *new* sub the reconciler created. We clear the pending_plan_*
// columns so the switch is marked complete, and fire the one-time
// planSwitchActivatedEmail via its claim helper. The normal receipt email
// still goes out below — the switch email is separate, content-wise.
func (s *server) handleSubscriptionCharged(ctx context.Context, subEntity, paymentEntity map[string]interface{}) {
	subID, _ := subEntity["id"].(string)
	if subID == "" {
		return
	}
	notes, _ := subEntity["notes"].(map[string]interface{})
	userID, ok := userIDFromNotes(notes)
	if !ok {
		slog.Warn("subscription webhook: no user_id in notes", "sub_id", subID)
		return
	}

	periodEnd := unixToTime(subEntity["current_end"])
	period := periodFromSubscription(subEntity)
	purpose, _ := notes["purpose"].(string)
	isSwitchCharge := purpose == "plan_switch"

	// Read notes.currency defensively: legacy subs created before the dual-
	// currency rollout have no such note; default to USD so we don't NULL out
	// a previously-set plan_currency. COALESCE below keeps the lock-in
	// invariant even if notes.currency is bogus.
	planCurrency, _ := notes["currency"].(string)
	planCurrency = normalizeCurrency(planCurrency)

	if _, err := s.db.ExecContext(ctx,
		`UPDATE users
		   SET plan_tier                = 'paid',
		       plan_period              = $1,
		       plan_paid_at             = NOW(),
		       razorpay_subscription_id = $2,
		       subscription_status      = 'active',
		       current_period_end       = $3,
		       plan_currency            = COALESCE(plan_currency, $4)
		 WHERE id                        = $5`,
		period, subID, periodEnd, planCurrency, userID,
	); err != nil {
		slog.Error("subscription.charged: user update failed", "error", err, "user_id", userID, "sub_id", subID)
		return
	}

	// Plan-switch activation: clear pending_* columns so the switch is marked
	// complete on our side. Done as a separate UPDATE so the main promotion
	// UPDATE above (which is idempotent across renewals) stays unchanged on
	// recurring charges. The claim helper below is what atomically sends the
	// "you're now on <plan>" email — safe to call even when another caller
	// (reconciler sweep) just did the same.
	if isSwitchCharge {
		if _, err := s.db.ExecContext(ctx,
			`UPDATE users
			   SET pending_plan_change     = NULL,
			       pending_plan_effective_at = NULL,
			       pending_plan_sub_id     = NULL
			 WHERE id                       = $1`,
			userID,
		); err != nil {
			slog.Error("subscription.charged (plan_switch): clear pending failed",
				"error", err, "user_id", userID, "sub_id", subID)
		}
		sendPlanSwitchActivatedIfUnsent(ctx, s.db, s.email, userID)
	}

	// Claim any pre-payment anon token captured in notes.token — same semantics
	// as the old payment.captured path.
	if tokenStr, _ := notes["token"].(string); tokenStr != "" {
		if tokenUUID, err := uuid.Parse(tokenStr); err == nil {
			s.db.ExecContext(ctx,
				`UPDATE resources SET migrated_to_user_id = $1, tier = 'paid', expires_at = NULL
				 WHERE token = $2 AND status = 'active'`,
				userID, tokenUUID,
			)
		}
	}
	// Promote every active resource belonging to the user.
	s.db.ExecContext(ctx,
		"UPDATE resources SET tier = 'paid', expires_at = NULL WHERE migrated_to_user_id = $1 AND status = 'active'",
		userID,
	)

	// Receipt email — claim-locked so a retried webhook or a simultaneous
	// reconciler tick can't double-send.
	amountCents := 0
	if v, ok := paymentEntity["amount"].(float64); ok {
		amountCents = int(v)
	}
	currency, _ := paymentEntity["currency"].(string)
	sendReceiptIfUnsent(ctx, s.db, s.email, userID, amountCents, currency)

	slog.Info("subscription charged", "user_id", userID, "sub_id", subID, "period_end", periodEnd.Format(time.RFC3339))
}

// handleSubscriptionHalted — Razorpay gave up after retry policy exhausted.
// Downgrade the user to free tier; existing anon-claimed resources keep working
// until their TTL (resources stay tier='paid' on the row — no scary data loss
// on billing failure; operator can reach out before yanking access).
func (s *server) handleSubscriptionHalted(ctx context.Context, subEntity map[string]interface{}) {
	subID, _ := subEntity["id"].(string)
	notes, _ := subEntity["notes"].(map[string]interface{})
	userID, ok := userIDFromNotes(notes)
	if !ok {
		return
	}
	s.db.ExecContext(ctx,
		"UPDATE users SET subscription_status = 'halted', plan_tier = 'free' WHERE id = $1",
		userID,
	)
	var email string
	if err := s.db.QueryRowContext(ctx, "SELECT email FROM users WHERE id = $1", userID).Scan(&email); err == nil && email != "" {
		subject, html := paymentFailedEmail("Your subscription has been halted after multiple failed charge attempts.")
		s.email.SendAsync(email, subject, html)
	}
	slog.Warn("subscription halted", "user_id", userID, "sub_id", subID)
}

// handleSubscriptionCancelled fires when a subscription is cancelled — whether
// via our API, the Razorpay dashboard, or Razorpay's own lifecycle. User
// resolution falls back to sub_id lookup because dashboard-initiated cancels
// sometimes arrive without our notes attached. Sending the cancellation email
// is claim-locked so a reconciler sweep for the same sub can't double-send.
func (s *server) handleSubscriptionCancelled(ctx context.Context, subEntity map[string]interface{}) {
	subID, _ := subEntity["id"].(string)
	notes, _ := subEntity["notes"].(map[string]interface{})

	var userID uuid.UUID
	if id, ok := userIDFromNotes(notes); ok {
		userID = id
	} else if subID != "" {
		if err := s.db.QueryRowContext(ctx,
			"SELECT id FROM users WHERE razorpay_subscription_id = $1", subID,
		).Scan(&userID); err != nil {
			slog.Warn("subscription.cancelled: cannot resolve user", "sub_id", subID, "error", err)
			return
		}
	} else {
		return
	}

	periodEnd := unixToTime(subEntity["current_end"])
	// An outright cancel takes precedence over a pending plan switch — if the
	// user (or Razorpay) cancels the current sub, we don't want the reconciler
	// to then fire a *new* sub for the switch they're walking away from. Clear
	// the pending_plan_* columns in the same UPDATE so the abandonment is atomic.
	if periodEnd.IsZero() {
		if _, err := s.db.ExecContext(ctx,
			`UPDATE users
			    SET subscription_status       = 'cancelled',
			        pending_plan_change       = NULL,
			        pending_plan_effective_at = NULL,
			        pending_plan_sub_id       = NULL
			  WHERE id                        = $1`,
			userID,
		); err != nil {
			slog.Error("subscription.cancelled: persist failed", "error", err, "user_id", userID)
			return
		}
	} else {
		if _, err := s.db.ExecContext(ctx,
			`UPDATE users
			    SET subscription_status       = 'cancelled',
			        current_period_end        = $1,
			        pending_plan_change       = NULL,
			        pending_plan_effective_at = NULL,
			        pending_plan_sub_id       = NULL
			  WHERE id                        = $2`,
			periodEnd, userID,
		); err != nil {
			slog.Error("subscription.cancelled: persist failed", "error", err, "user_id", userID)
			return
		}
	}

	sendCancelIfUnsent(ctx, s.db, s.email, userID)
	slog.Info("subscription cancelled", "user_id", userID, "sub_id", subID)
}

func (s *server) handleSubscriptionCompleted(ctx context.Context, subEntity map[string]interface{}) {
	subID, _ := subEntity["id"].(string)
	notes, _ := subEntity["notes"].(map[string]interface{})
	userID, ok := userIDFromNotes(notes)
	if !ok {
		return
	}
	s.db.ExecContext(ctx,
		"UPDATE users SET subscription_status = 'completed' WHERE id = $1",
		userID,
	)
	slog.Info("subscription completed", "user_id", userID, "sub_id", subID)
}
