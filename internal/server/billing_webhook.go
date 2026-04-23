package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
)

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
	if !s.payment.VerifyWebhookSignature(body, signature) {
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
		order, err := s.payment.FetchOrder(ctx, orderID)
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
		order, err := s.payment.FetchOrder(ctx, orderID)
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

// computeSignature is retained for tests that sign fixture webhook bodies
// to exercise the verification path. Production code calls
// s.payment.VerifyWebhookSignature — don't reach for this in handlers.
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
	order, err := s.payment.FetchOrder(ctx, orderID)
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
