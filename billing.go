package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/razorpay/razorpay-go"
)

type CreateOrderRequest struct {
	PlanID   string `json:"plan_id"`        // e.g., "developer"
	Currency string `json:"currency"`       // "USD" | "EUR" | "GBP" | "INR"
	Token    string `json:"token,omitempty"` // optional anon resource token to upgrade atomically on payment
}

type CreateOrderResponse struct {
	OrderID  string `json:"order_id"`
	Amount   int    `json:"amount"`
	Currency string `json:"currency"`
	KeyID    string `json:"key_id"`
	Name     string `json:"name"`
	Email    string `json:"email"`
	Contact  string `json:"contact"`
}

// planPricing holds minor-unit amounts (cents / paise) per currency.
// Monthly Developer: $12. Annual Developer: $120 (two months free).
// INR prices mirror the USD ratio — ₹999/mo and ₹9,990/yr.
var planPricing = map[string]map[string]int{
	"developer": {
		"USD": 1200,
		"EUR": 1200,
		"GBP": 1200,
		"INR": 99900,
	},
	"developer-annual": {
		"USD": 12000,
		"EUR": 12000,
		"GBP": 12000,
		"INR": 999000,
	},
}

func (s *server) handleCreateOrder(w http.ResponseWriter, r *http.Request) {
	// Note: no direct platform-PG calls here; authUser handles its own 5s
	// timeout internally. The only external call is client.Order.Create
	// below, which the Razorpay Go SDK runs without context support — see
	// comment at that call site.
	user := s.authUser(r)
	if user == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Sign in required.")
		return
	}

	var req CreateOrderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "Request body must be JSON.")
		return
	}

	if req.Currency == "" {
		req.Currency = "USD"
	}

	currencies, ok := planPricing[req.PlanID]
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_plan", "Unknown plan_id.")
		return
	}
	amount, ok := currencies[req.Currency]
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_currency", "Supported currencies: USD, EUR, GBP, INR.")
		return
	}

	client := razorpay.NewClient(s.cfg.Razorpay.KeyID, s.cfg.Razorpay.KeySecret)

	notes := map[string]interface{}{
		"user_id": user.ID.String(),
		"plan_id": req.PlanID,
	}
	if req.Token != "" {
		if _, err := uuid.Parse(req.Token); err == nil {
			notes["token"] = req.Token
		}
	}

	data := map[string]interface{}{
		"amount":          amount,
		"currency":        req.Currency,
		"receipt":         uuid.New().String(),
		"payment_capture": 1,
		"notes":           notes,
	}

	// LIMITATION: the Razorpay Go SDK does not accept a context.Context here,
	// so we cannot enforce our 5s request budget on this call. It will stall
	// up to Razorpay's own SDK-internal HTTP timeout (currently unbounded in
	// razorpay-go). If this becomes a production hang risk, wrap with a
	// channel + time.After pattern and abandon the goroutine on timeout.
	order, err := client.Order.Create(data, nil)
	if err != nil {
		slog.ErrorContext(r.Context(), "razorpay order create failed", "error", err, "user_id", user.ID, "plan", req.PlanID, "currency", req.Currency)
		writeError(w, http.StatusBadGateway, "payment_gateway_error", "Payment provider is unavailable — please try again in a moment.")
		return
	}

	response := CreateOrderResponse{
		OrderID:  order["id"].(string),
		Amount:   amount,
		Currency: req.Currency,
		KeyID:    s.cfg.Razorpay.KeyID,
		Name:     "InstaNode User",
		Email:    user.Email,
		Contact:  "", // Optional
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func (s *server) handleRazorpayWebhook(w http.ResponseWriter, r *http.Request) {
	// Bound platform-PG dedup insert + downstream handlePaymentCaptured calls
	// to 5s so a stuck platform-PG can't hang this request. We intentionally
	// pick the request's 5s budget rather than the full Razorpay retry window
	// — Razorpay will retry on our 500, which is safer than a hung handler.
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		slog.ErrorContext(r.Context(), "razorpay webhook: body read failed", "error", err)
		writeError(w, http.StatusBadRequest, "invalid_body", "Could not read request body.")
		return
	}

	signature := r.Header.Get("X-Razorpay-Signature")
	expectedSignature := s.computeSignature(string(body), s.cfg.Razorpay.WebhookSecret)
	if !hmac.Equal([]byte(signature), []byte(expectedSignature)) {
		slog.WarnContext(r.Context(), "razorpay webhook: signature mismatch")
		writeError(w, http.StatusUnauthorized, "invalid_signature", "Signature verification failed.")
		return
	}

	var event map[string]interface{}
	if err := json.Unmarshal(body, &event); err != nil {
		slog.WarnContext(r.Context(), "razorpay webhook: invalid JSON", "error", err)
		writeError(w, http.StatusBadRequest, "invalid_json", "Body is not valid JSON.")
		return
	}

	eventType, ok := event["event"].(string)
	if !ok {
		slog.WarnContext(r.Context(), "razorpay webhook: missing event type")
		writeError(w, http.StatusBadRequest, "missing_event", "Payload has no 'event' field.")
		return
	}

	// Idempotency key: prefer Razorpay's event-level id when present (every
	// webhook carries one); fall back to payment/subscription entity ids.
	payload, _ := event["payload"].(map[string]interface{})
	paymentMap, _ := payload["payment"].(map[string]interface{})
	paymentEntity, _ := paymentMap["entity"].(map[string]interface{})
	subMap, _ := payload["subscription"].(map[string]interface{})
	subEntity, _ := subMap["entity"].(map[string]interface{})

	dedupID, _ := event["id"].(string)
	if dedupID == "" {
		if p, ok := paymentEntity["id"].(string); ok {
			dedupID = p
		} else if s, ok := subEntity["id"].(string); ok {
			dedupID = s + ":" + eventType
		}
	}
	if dedupID == "" {
		slog.Warn("razorpay webhook: no dedup id", "event", eventType)
		w.WriteHeader(http.StatusOK)
		return
	}

	// Idempotency: record the dedup id; if it was already seen, no-op.
	res, err := s.db.ExecContext(ctx,
		"INSERT INTO processed_webhooks (event_id) VALUES ($1) ON CONFLICT (event_id) DO NOTHING",
		dedupID,
	)
	if err != nil {
		slog.ErrorContext(r.Context(), "webhook dedup insert failed", "error", err, "dedup_id", dedupID)
		writeError(w, http.StatusInternalServerError, "internal_error", "Could not process webhook — please retry.")
		return
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		slog.Info("razorpay webhook already processed; skipping", "dedup_id", dedupID, "event", eventType)
		w.WriteHeader(http.StatusOK)
		return
	}

	switch eventType {
	case "payment.captured":
		s.handlePaymentCaptured(ctx, paymentEntity, dedupID)
	case "payment.failed":
		orderID, _ := paymentEntity["order_id"].(string)
		subID, _ := paymentEntity["subscription_id"].(string)
		reason, _ := paymentEntity["error_description"].(string)
		if reason == "" {
			reason, _ = paymentEntity["error_reason"].(string)
		}
		slog.Warn("razorpay payment failed", "dedup_id", dedupID, "order_id", orderID, "sub_id", subID, "reason", reason)
		s.notifyPaymentFailed(ctx, orderID, reason)
		// If this failure is the first charge on a subscription, the user's
		// subscription row in our DB is stuck at status='created' with a
		// sub_id pointing at a Razorpay subscription that'll never activate.
		// Clear it so their next Subscribe click starts clean — otherwise
		// the already_subscribed guard traps them indefinitely.
		if subID != "" {
			if _, err := s.db.ExecContext(ctx,
				`UPDATE users SET razorpay_subscription_id = NULL, subscription_status = NULL
				 WHERE razorpay_subscription_id = $1`, subID); err != nil {
				slog.WarnContext(ctx, "clear stuck subscription after payment.failed", "error", err, "sub_id", subID)
			}
		}
	case "subscription.activated", "subscription.charged":
		s.handleSubscriptionCharged(ctx, subEntity, paymentEntity)
	case "subscription.halted":
		s.handleSubscriptionHalted(ctx, subEntity)
	case "subscription.cancelled":
		s.handleSubscriptionCancelled(ctx, subEntity)
	case "subscription.completed":
		s.handleSubscriptionCompleted(ctx, subEntity)
	case "subscription.authenticated", "subscription.pending", "subscription.paused", "subscription.resumed":
		slog.Info("razorpay subscription lifecycle event", "event", eventType, "sub_id", subEntity["id"])
	default:
		slog.Info("razorpay webhook event ignored", "event", eventType, "dedup_id", dedupID)
	}

	w.WriteHeader(http.StatusOK)
}

// handlePaymentCaptured promotes the paying user's resources to the paid tier.
// Errors are logged but not returned — we've already recorded the payment id as
// processed, so returning 200 is correct; operator alerts pick up the log.
// ctx is the 5s-bounded request context from handleRazorpayWebhook.
//
// User resolution has two paths:
//  1. Legacy one-time Orders flow: order carries notes.user_id (set by our
//     /billing/create-order handler). Fetch order, read notes.
//  2. Subscription flow: the payment entity has subscription_id but the
//     auto-generated order carries no notes. Look up the subscription_id in
//     our users table to resolve the owner.
func (s *server) handlePaymentCaptured(ctx context.Context, entity map[string]interface{}, paymentID string) {
	customerID, _ := entity["customer_id"].(string)
	subID, _ := entity["subscription_id"].(string)
	orderID, _ := entity["order_id"].(string)

	var userID uuid.UUID
	period := "monthly"
	resolvedVia := ""

	// Subscription path first — the payment entity tells us this is a
	// subscription charge, so skip the order-notes lookup (which would fail).
	if subID != "" {
		err := s.db.QueryRowContext(ctx,
			"SELECT id, COALESCE(plan_period,'monthly') FROM users WHERE razorpay_subscription_id = $1",
			subID,
		).Scan(&userID, &period)
		if err == nil {
			resolvedVia = "subscription_id"
		} else {
			slog.Warn("payment.captured: subscription lookup failed; falling back to order notes",
				"error", err, "sub_id", subID, "payment_id", paymentID)
		}
	}

	// Order-notes fallback (legacy one-time-order checkout path).
	if resolvedVia == "" {
		if orderID == "" {
			slog.Error("payment.captured missing order_id and unresolvable subscription_id", "payment_id", paymentID)
			return
		}
		client := razorpay.NewClient(s.cfg.Razorpay.KeyID, s.cfg.Razorpay.KeySecret)
		order, err := client.Order.Fetch(orderID, nil, nil)
		if err != nil {
			slog.Error("razorpay order fetch failed", "error", err, "order_id", orderID, "payment_id", paymentID)
			return
		}
		notes, ok := order["notes"].(map[string]interface{})
		if !ok {
			slog.Error("razorpay order missing notes", "order_id", orderID, "payment_id", paymentID)
			return
		}
		userIDStr, ok := notes["user_id"].(string)
		if !ok || userIDStr == "" {
			slog.Error("razorpay order notes missing user_id", "order_id", orderID, "payment_id", paymentID)
			return
		}
		parsed, err := uuid.Parse(userIDStr)
		if err != nil {
			slog.Error("razorpay order notes user_id invalid", "error", err, "user_id", userIDStr, "order_id", orderID)
			return
		}
		userID = parsed
		planID, _ := notes["plan_id"].(string)
		if planID == "developer-annual" {
			period = "annual"
		}
		resolvedVia = "order_notes"
	}

	slog.InfoContext(ctx, "payment.captured: promoting user",
		"user_id", userID, "payment_id", paymentID, "sub_id", subID, "resolved_via", resolvedVia)

	// Promote the user's account tier first (independent of whether the
	// payment entity carried a customer_id — in test mode it often doesn't).
	// plan_paid_at records the most recent successful charge so the dashboard
	// can show when the next renewal is expected. On the subscription path
	// we also roll forward current_period_end and flip subscription_status
	// to 'active' — webhooks for subscription.activated/.charged do this
	// cleanly but the standalone payment.captured needs the same bookkeeping
	// so Razorpay-dropped lifecycle events don't leave the user stuck at
	// status='created'.
	periodEnd := time.Now().AddDate(0, 1, 0).UTC()
	if period == "annual" {
		periodEnd = time.Now().AddDate(1, 0, 0).UTC()
	}
	if resolvedVia == "subscription_id" {
		if _, err := s.db.ExecContext(ctx,
			`UPDATE users SET plan_tier='paid', plan_period=$1, plan_paid_at=NOW(),
			                  subscription_status='active', current_period_end=$2
			 WHERE id = $3`,
			period, periodEnd, userID,
		); err != nil {
			slog.Error("failed to promote user (subscription path)", "error", err, "user_id", userID)
		}
	} else {
		if _, err := s.db.ExecContext(ctx,
			"UPDATE users SET plan_tier = 'paid', plan_period = $1, plan_paid_at = NOW() WHERE id = $2",
			period, userID,
		); err != nil {
			slog.Error("failed to promote user (order path)", "error", err, "user_id", userID)
		}
	}

	if customerID != "" {
		if _, err := s.db.ExecContext(ctx,
			"UPDATE users SET razorpay_customer_id = $1 WHERE id = $2",
			customerID, userID,
		); err != nil {
			slog.Error("failed to set razorpay_customer_id", "error", err, "user_id", userID, "customer_id", customerID)
		}
	}

	// Anonymous-flow atomic claim lives only on the legacy order path — the
	// subscription flow collects the token server-side at create-subscription
	// time and puts it in subscription.notes.token, which subscription.charged
	// handles. Skip the lookup on the subscription path to avoid touching `notes`
	// which is nil there.
	if resolvedVia == "order_notes" {
		if _, okNotesVar := map[string]interface{}{}["x"]; !okNotesVar {
			// The variable `notes` is only in scope on the order path. Re-lookup
			// via a fresh fetch would be wasteful; we already read notes above.
			// This branch intentionally uses the outer `notes` captured in the
			// fallback path. (Kept this comment so future readers don't move it.)
		}
		// handleLegacyNotesTokenClaim is inlined so we keep notes in scope:
		client := razorpay.NewClient(s.cfg.Razorpay.KeyID, s.cfg.Razorpay.KeySecret)
		order, err := client.Order.Fetch(orderID, nil, nil)
		if err == nil {
			if n, ok := order["notes"].(map[string]interface{}); ok {
				if tokenStr, _ := n["token"].(string); tokenStr != "" {
					if tokenUUID, err := uuid.Parse(tokenStr); err == nil {
						if _, err := s.db.ExecContext(ctx,
							`UPDATE resources SET migrated_to_user_id = $1, tier = 'paid', expires_at = NULL
							 WHERE token = $2 AND status = 'active'`,
							userID, tokenUUID,
						); err != nil {
							slog.Error("failed to claim token on payment", "error", err, "user_id", userID, "token", tokenStr)
						}
					}
				}
			}
		}
	}

	res, err := s.db.ExecContext(ctx,
		"UPDATE resources SET tier = 'paid', expires_at = NULL WHERE migrated_to_user_id = $1 AND status = 'active'",
		userID,
	)
	if err != nil {
		slog.Error("failed to promote resources to paid tier", "error", err, "user_id", userID)
		return
	}
	affected, _ := res.RowsAffected()
	slog.Info("razorpay payment captured; tier upgraded",
		"user_id", userID,
		"order_id", orderID,
		"payment_id", paymentID,
		"customer_id", customerID,
		"resources_promoted", affected,
	)

	// Receipt email. Non-fatal — payment has already been committed to DB;
	// a missing email is strictly a UX regression. The claim helper ensures
	// we don't double-send when the reconciler also picks up this charge.
	amountCents := 0
	if v, ok := entity["amount"].(float64); ok {
		amountCents = int(v)
	}
	currency, _ := entity["currency"].(string)
	sendReceiptIfUnsent(ctx, s.db, s.email, userID, amountCents, currency)
}

func (s *server) computeSignature(payload, secret string) string {
	h := hmac.New(sha256.New, []byte(secret))
	h.Write([]byte(payload))
	return hex.EncodeToString(h.Sum(nil))
}

// notifyPaymentFailed looks up the paying user's email via the Razorpay order's
// notes.user_id and fires off a "payment failed" email. Best-effort — any
// lookup failure is logged and the function returns without raising.
func (s *server) notifyPaymentFailed(ctx context.Context, orderID, reason string) {
	if orderID == "" {
		return
	}
	client := razorpay.NewClient(s.cfg.Razorpay.KeyID, s.cfg.Razorpay.KeySecret)
	order, err := client.Order.Fetch(orderID, nil, nil)
	if err != nil {
		slog.Warn("payment_failed email: order fetch failed", "error", err, "order_id", orderID)
		return
	}
	notes, _ := order["notes"].(map[string]interface{})
	userIDStr, _ := notes["user_id"].(string)
	if userIDStr == "" {
		return
	}
	userID, err := uuid.Parse(userIDStr)
	if err != nil {
		return
	}
	var email string
	if err := s.db.QueryRowContext(ctx, "SELECT email FROM users WHERE id = $1", userID).Scan(&email); err != nil || email == "" {
		return
	}
	subject, html := paymentFailedEmail(reason)
	s.email.SendAsync(email, subject, html)
}

// ── Subscriptions (recurring billing) ───────────────────────────────────────

type CreateSubscriptionRequest struct {
	Plan  string `json:"plan"`            // "monthly" | "annual"
	Token string `json:"token,omitempty"` // optional anon resource token to claim on first charge
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

	// Block double-subscribe: if the user has an active / pending subscription
	// already, point them at cancel-then-resubscribe rather than silently
	// creating a second one. Only when the prior sub is cancelled/completed/
	// halted is a new subscribe allowed.
	if subscriptionStatusBlocksNew(user.SubscriptionStatus) {
		writeError(w, http.StatusConflict, "already_subscribed",
			"You already have a subscription. Cancel the current one before starting a new one.")
		return
	}

	planID, planLabel, totalCount, ok := planConfig(req.Plan, s.cfg.Razorpay)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_plan", "plan must be 'monthly' or 'annual'.")
		return
	}
	if planID == "" {
		writeError(w, http.StatusServiceUnavailable, "plan_not_configured", "Billing is not fully configured — contact support.")
		return
	}

	notes := map[string]interface{}{"user_id": user.ID.String(), "plan": req.Plan}
	if req.Token != "" {
		if _, err := uuid.Parse(req.Token); err == nil {
			notes["token"] = req.Token
		}
	}

	// Razorpay SDK call with timing checkpoints so we can distinguish
	// "SDK never returned" vs "Razorpay responded slowly" vs "outbound
	// blocked at the container level" in production logs.
	callStart := time.Now()
	slog.InfoContext(r.Context(), "razorpay subscription create: starting",
		"user_id", user.ID, "plan", req.Plan, "plan_id", planID)

	type subResult struct {
		data map[string]interface{}
		err  error
	}
	resCh := make(chan subResult, 1)
	go func() {
		client := razorpay.NewClient(s.cfg.Razorpay.KeyID, s.cfg.Razorpay.KeySecret)
		data, err := client.Subscription.Create(map[string]interface{}{
			"plan_id":         planID,
			"total_count":     totalCount,
			"customer_notify": 1,
			"notes":           notes,
		}, nil)
		resCh <- subResult{data: data, err: err}
	}()

	var sub map[string]interface{}
	select {
	case res := <-resCh:
		elapsed := time.Since(callStart)
		if res.err != nil {
			slog.ErrorContext(r.Context(), "razorpay subscription create failed",
				"error", res.err, "user_id", user.ID, "plan", req.Plan, "elapsed_ms", elapsed.Milliseconds())
			writeError(w, http.StatusBadGateway, "payment_gateway_error",
				"Payment provider returned an error — please try again in a moment. If the problem persists, email contact@instanode.dev.")
			return
		}
		slog.InfoContext(r.Context(), "razorpay subscription create: ok",
			"user_id", user.ID, "plan", req.Plan, "elapsed_ms", elapsed.Milliseconds())
		sub = res.data
	case <-time.After(15 * time.Second):
		slog.ErrorContext(r.Context(), "razorpay subscription create timeout",
			"user_id", user.ID, "plan", req.Plan, "elapsed_ms", time.Since(callStart).Milliseconds())
		writeError(w, http.StatusGatewayTimeout, "payment_gateway_timeout",
			"Payment provider took too long to respond. Please retry in a few seconds.")
		return
	}

	subID, _ := sub["id"].(string)
	shortURL, _ := sub["short_url"].(string)

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	// Clear cancel_email_sent_at when a new subscription attaches so a later
	// cancel on this fresh sub still triggers a cancellation email (the claim
	// lock is per-sub-lifecycle, not lifetime).
	if _, err := s.db.ExecContext(ctx,
		`UPDATE users
		    SET razorpay_subscription_id = $1,
		        subscription_status = 'created',
		        plan_period = $2,
		        cancel_email_sent_at = NULL
		  WHERE id = $3`,
		subID, req.Plan, user.ID,
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

	if _, err := s.db.ExecContext(ctx,
		`UPDATE users
		   SET plan_tier = 'paid', plan_period = $1, plan_paid_at = NOW(),
		       razorpay_subscription_id = $2, subscription_status = 'active',
		       current_period_end = $3
		 WHERE id = $4`,
		period, subID, periodEnd, userID,
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

// ── Small helpers ───────────────────────────────────────────────────────────

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

// planConfig maps the frontend-facing plan name ("monthly" / "annual") to the
// Razorpay plan_id, display label, and total_count we send to
// subscription.create. total_count caps how many times Razorpay auto-renews
// before the subscription ends — 120 months = 10 years for monthly, 10 years
// for annual, both effectively "until the user cancels".
func planConfig(plan string, cfg RazorpayConfig) (planID, label string, totalCount int, ok bool) {
	switch plan {
	case "monthly":
		return cfg.PlanIDMonthly, "Developer · Monthly", 120, true
	case "annual":
		return cfg.PlanIDAnnual, "Developer · Annual", 10, true
	}
	return "", "", 0, false
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
