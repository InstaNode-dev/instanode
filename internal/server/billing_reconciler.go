package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
)

// Billing reconciliation.
//
// Webhook delivery from Razorpay can miss events (we've observed
// subscription.activated silently absent in test mode). This goroutine polls
// every user who has a razorpay_subscription_id and:
//
//  1. Fetches the subscription from /v1/subscriptions/{id}.
//  2. If Razorpay says status=active AND paid_count>0 AND our DB is still
//     'created' / 'free', promote the user and send the receipt.
//  3. If Razorpay says status=halted and we still show 'active', downgrade
//     and send the failure email.
//  4. If Razorpay says cancelled/completed, mirror that in our DB.
//
// The on-paid transition is idempotent via plan_paid_at >= latest charge date
// — we skip sending the receipt email if we've already recorded a paid_at
// newer than Razorpay's latest charge.

const (
	billingReconcileStartupDelay = 45 * time.Second
	billingReconcileInterval     = 15 * time.Minute
	billingReconcileTickTimeout  = 90 * time.Second
)

type brevoRazorpaySubDetail struct {
	ID           string  `json:"id"`
	Status       string  `json:"status"`
	PaidCount    int     `json:"paid_count"`
	CurrentEnd   float64 `json:"current_end"` // unix seconds
	ChargeAt     float64 `json:"charge_at"`
	EndedAt      float64 `json:"ended_at"`
	ShortURL     string  `json:"short_url"`
	PlanID       string  `json:"plan_id"`
	Notes        map[string]interface{} `json:"notes"`
}

// reconcileAction is what the reconciler decides to do per user.
type reconcileAction string

const (
	actionNone      reconcileAction = ""          // already in sync
	actionActivate  reconcileAction = "activate"  // promote to paid + send receipt
	actionHalt      reconcileAction = "halt"      // downgrade + send failure email
	actionCancel    reconcileAction = "cancel"    // mark cancelled (keep paid until period end)
	actionComplete  reconcileAction = "complete"  // total_count reached
	actionClear     reconcileAction = "clear"     // stuck created → forget; user can retry
)

// decideBillingReconcile compares the user's DB state against the subscription
// state returned by Razorpay and returns the action to take. Pure function —
// testable without DB or HTTP.
//
// Parameters:
//
//	dbStatus: the user's current subscription_status in our DB (nil or "" for unknown).
//	dbTier: user's current plan_tier ("free" | "paid").
//	rpStatus: Razorpay's subscription.status ("created", "authenticated",
//	          "active", "pending", "halted", "cancelled", "completed", "expired").
//	rpPaidCount: Razorpay's paid_count (int; >0 means at least one charge succeeded).
//	ageSinceCreated: how long this subscription has existed on Razorpay.
func decideBillingReconcile(dbStatus *string, dbTier, rpStatus string, rpPaidCount int, ageSinceCreated time.Duration) reconcileAction {
	cur := ""
	if dbStatus != nil {
		cur = *dbStatus
	}
	switch rpStatus {
	case "active":
		// Razorpay charged at least once. If we still say free or non-active,
		// promote.
		if dbTier != "paid" || cur != "active" {
			return actionActivate
		}
		return actionNone
	case "halted":
		if dbTier == "paid" && cur != "halted" {
			return actionHalt
		}
		if cur != "halted" {
			return actionHalt
		}
		return actionNone
	case "cancelled":
		if cur != "cancelled" {
			return actionCancel
		}
		return actionNone
	case "completed", "expired":
		if cur != "completed" {
			return actionComplete
		}
		return actionNone
	case "created", "authenticated", "pending":
		// Subscription never advanced. If the sub has been 'created' for more
		// than 24h without any charge, abandon it so the user can retry.
		if rpPaidCount == 0 && ageSinceCreated > 24*time.Hour {
			return actionClear
		}
		return actionNone
	}
	return actionNone
}

// startBillingReconciler launches the background goroutine. No-op when
// Razorpay keys aren't configured.
func startBillingReconciler(db *sql.DB, cfg *Config, em *emailer) {
	if cfg.Razorpay.KeyID == "" || cfg.Razorpay.KeySecret == "" {
		slog.Info("billing reconciler: razorpay keys not set, skipping")
		return
	}
	slog.Info("billing reconciler: starting", "interval", billingReconcileInterval)
	go func() {
		time.Sleep(billingReconcileStartupDelay)
		ticker := time.NewTicker(billingReconcileInterval)
		defer ticker.Stop()
		for {
			ctx, cancel := context.WithTimeout(context.Background(), billingReconcileTickTimeout)
			n, err := reconcileBillingOnce(ctx, db, cfg, em)
			cancel()
			if err != nil {
				slog.Warn("billing reconciler: tick failed", "error", err)
			} else if n > 0 {
				slog.Info("billing reconciler: actioned", "count", n)
			}

			// Plan-switch promotions share the tick cadence. Separate bounded
			// context so a slow Razorpay round-trip on the promotion side
			// cannot starve the main reconcile pass above. No-op when the
			// feature flag is off.
			psCtx, psCancel := context.WithTimeout(context.Background(), billingReconcileTickTimeout)
			if promoted, perr := promotePendingPlanSwitches(psCtx, db, cfg, liveRazorpayCancelSub, liveRazorpayCreateSub, time.Now()); perr != nil {
				slog.Warn("plan switch reconciler: tick failed", "error", perr)
			} else if promoted > 0 {
				slog.Info("plan switch reconciler: promoted", "count", promoted)
			}
			psCancel()

			<-ticker.C
		}
	}()
}

// reconcileBillingOnce does one full pass. Returns number of users acted on.
func reconcileBillingOnce(ctx context.Context, db *sql.DB, cfg *Config, em *emailer) (int, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, email, plan_tier, plan_period, plan_paid_at,
		       subscription_status, razorpay_subscription_id
		FROM users
		WHERE razorpay_subscription_id IS NOT NULL`)
	if err != nil {
		return 0, fmt.Errorf("query users: %w", err)
	}
	defer rows.Close()

	type row struct {
		userID    uuid.UUID
		email     string
		tier      string
		period    string
		paidAt    *time.Time
		status    *string
		subID     string
	}
	var users []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.userID, &r.email, &r.tier, &r.period, &r.paidAt, &r.status, &r.subID); err != nil {
			continue
		}
		users = append(users, r)
	}

	actioned := 0
	for _, u := range users {
		sub, err := fetchRazorpaySubscription(ctx, cfg.Razorpay.KeyID, cfg.Razorpay.KeySecret, u.subID)
		if err != nil {
			slog.Warn("billing reconciler: fetch failed", "error", err, "sub_id", u.subID)
			continue
		}
		age := time.Duration(0)
		if sub.CurrentEnd > 0 {
			age = time.Since(time.Unix(int64(sub.CurrentEnd), 0))
			if age < 0 {
				age = 0
			}
		}
		action := decideBillingReconcile(u.status, u.tier, sub.Status, sub.PaidCount, age)
		if action != actionNone {
			slog.Info("billing reconciler: action", "user_id", u.userID, "sub_id", u.subID,
				"db_tier", u.tier, "db_status", stringOr(u.status), "rp_status", sub.Status, "action", string(action))
			if err := applyBillingAction(ctx, db, em, u.userID, u.email, u.period, sub, action); err != nil {
				slog.Warn("billing reconciler: apply failed", "error", err, "user_id", u.userID, "action", string(action))
				continue
			}
			actioned++
		}

		// Even when the main decide-action loop returns actionNone (DB already
		// in sync with Razorpay), the confirmation / cancellation email may
		// have been dropped by a failed webhook or SMTP hiccup. The claim
		// helpers are no-ops when the corresponding slot has already been
		// filled, so this sweep is safe to run unconditionally.
		sweepBillingEmails(ctx, db, em, u.userID, u.period)
	}
	return actioned, nil
}

// sweepBillingEmails is the "have we forgotten to send the receipt or cancel
// email for this user?" pass. Both claim helpers are atomic: if the email was
// already sent, they no-op. If not, we send the canonical-amount receipt
// (reconciler doesn't have the per-charge invoice amount; the webhook path
// sends the true amount when available).
func sweepBillingEmails(ctx context.Context, db *sql.DB, em *emailer, userID uuid.UUID, period string) {
	if em == nil {
		return
	}
	canonical := 1200 // $12.00
	if period == "annual" {
		canonical = 12000 // $120.00
	}
	sendReceiptIfUnsent(ctx, db, em, userID, canonical, "USD")
	sendCancelIfUnsent(ctx, db, em, userID)
}

// applyBillingAction writes the DB changes for a single reconciliation action.
// It deliberately does NOT send any email directly — that's the job of the
// sweepBillingEmails pass that runs after applyBillingAction on every tick.
// The claim helpers in billing_emails.go use an atomic UPDATE ... WHERE to
// make the send-once guarantee hold across webhook + reconciler races.
func applyBillingAction(ctx context.Context, db *sql.DB, em *emailer, userID uuid.UUID, email, period string, sub *brevoRazorpaySubDetail, action reconcileAction) error {
	periodEnd := time.Unix(int64(sub.CurrentEnd), 0).UTC()
	if sub.CurrentEnd <= 0 {
		if period == "annual" {
			periodEnd = time.Now().AddDate(1, 0, 0).UTC()
		} else {
			periodEnd = time.Now().AddDate(0, 1, 0).UTC()
		}
	}

	switch action {
	case actionActivate:
		// Promote + set current_period_end. Also flips every active resource
		// belonging to the user to the paid tier so permanence kicks in.
		if _, err := db.ExecContext(ctx, `
			UPDATE users
			   SET plan_tier='paid', plan_period=$1, plan_paid_at=NOW(),
			       subscription_status='active', current_period_end=$2
			 WHERE id=$3`, period, periodEnd, userID); err != nil {
			return err
		}
		db.ExecContext(ctx, `UPDATE resources SET tier='paid', expires_at=NULL WHERE migrated_to_user_id=$1 AND status='active'`, userID)
		// Receipt email is claimed in the sweepBillingEmails pass.
	case actionHalt:
		if _, err := db.ExecContext(ctx, `UPDATE users SET subscription_status='halted', plan_tier='free' WHERE id=$1`, userID); err != nil {
			return err
		}
		// Halt email isn't claim-locked (no column) — only sent once here on
		// the actionHalt transition. Webhook path does the same.
		if em != nil && email != "" {
			subject, html := paymentFailedEmail("Your subscription has been halted after multiple failed charge attempts.")
			em.SendAsync(email, subject, html)
		}
	case actionCancel:
		if _, err := db.ExecContext(ctx, `UPDATE users SET subscription_status='cancelled', current_period_end=$1 WHERE id=$2`, periodEnd, userID); err != nil {
			return err
		}
		// Cancel email is claimed in the sweepBillingEmails pass.
	case actionComplete:
		if _, err := db.ExecContext(ctx, `UPDATE users SET subscription_status='completed' WHERE id=$1`, userID); err != nil {
			return err
		}
	case actionClear:
		// Abandon a never-authenticated subscription so the user can retry.
		// Also clear cancel_email_sent_at so a subsequent cancel on a new sub
		// still triggers a cancel email.
		if _, err := db.ExecContext(ctx, `
			UPDATE users
			   SET razorpay_subscription_id=NULL,
			       subscription_status=NULL,
			       cancel_email_sent_at=NULL
			 WHERE id=$1`, userID); err != nil {
			return err
		}
	}
	return nil
}

func fetchRazorpaySubscription(ctx context.Context, keyID, keySecret, subID string) (*brevoRazorpaySubDetail, error) {
	url := "https://api.razorpay.com/v1/subscriptions/" + subID
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	req.SetBasicAuth(keyID, keySecret)
	req.Header.Set("Accept", "application/json")
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("razorpay GET sub %d: %s", resp.StatusCode, string(body))
	}
	var out brevoRazorpaySubDetail
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return &out, nil
}

func stringOr(p *string) string {
	if p == nil {
		return "nil"
	}
	return *p
}
