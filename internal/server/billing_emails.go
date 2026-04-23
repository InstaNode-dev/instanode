package server

import (
	"context"
	"database/sql"
	"log/slog"
	"time"

	"github.com/google/uuid"
)

// Single-send email idempotency for billing events.
//
// Why this exists: three call-sites race to send the receipt email after a
// successful charge — the subscription.charged webhook, the one-time-order
// payment.captured webhook, and the billing reconciler's periodic sweep.
// Similarly two call-sites send the cancel email: subscription.cancelled
// webhook and the reconciler. Without a lock we saw duplicate emails when
// Razorpay retried a webhook while the reconciler was mid-tick.
//
// The lock is the users.receipt_email_sent_at / users.cancel_email_sent_at
// column. A caller "claims" the send with an atomic UPDATE whose WHERE clause
// requires the slot to still be unclaimed; only one caller's UPDATE affects a
// row. The caller who wins the claim performs SendAsync, which is fire-and-
// forget — we intentionally accept the worst case of "timestamp set + SMTP
// dropped" (user never receives) over "SMTP succeeded + we retry anyway"
// (duplicate receipt). Users complain about dupes; missing emails resolve
// themselves when the user emails support.

// receiptClaim is what a claimReceiptEmail caller needs to compose and send
// the email. Period end falls back to a best-guess (now + period length) when
// the DB has no current_period_end — the reconciler's actionActivate branch
// writes it, but historical rows may be null.
type receiptClaim struct {
	Email     string
	Period    string // "monthly" | "annual"
	PeriodEnd time.Time
}

// claimReceiptEmail atomically reserves the right to send one receipt email
// for the user's latest charge. Returns ok=false when:
//   - the user row doesn't exist
//   - plan_paid_at is null (no charge on record)
//   - receipt_email_sent_at is already >= plan_paid_at (already sent)
//
// The WHERE clause is the lock: two concurrent callers both running this
// query see exactly one UPDATE affect a row.
func claimReceiptEmail(ctx context.Context, db *sql.DB, userID uuid.UUID) (receiptClaim, bool) {
	var (
		email  string
		period string
		pe     *time.Time
	)
	err := db.QueryRowContext(ctx, `
		UPDATE users
		   SET receipt_email_sent_at = NOW()
		 WHERE id = $1
		   AND plan_paid_at IS NOT NULL
		   AND (receipt_email_sent_at IS NULL OR receipt_email_sent_at < plan_paid_at)
		RETURNING email, COALESCE(plan_period,'monthly'), current_period_end`,
		userID,
	).Scan(&email, &period, &pe)
	if err != nil {
		if err != sql.ErrNoRows {
			slog.Warn("claimReceiptEmail: query failed", "error", err, "user_id", userID)
		}
		return receiptClaim{}, false
	}
	end := time.Time{}
	if pe != nil {
		end = *pe
	} else if period == "annual" {
		end = time.Now().AddDate(1, 0, 0).UTC()
	} else {
		end = time.Now().AddDate(0, 1, 0).UTC()
	}
	return receiptClaim{Email: email, Period: period, PeriodEnd: end}, true
}

// cancelClaim carries the data a cancel email needs.
type cancelClaim struct {
	Email     string
	Period    string
	PeriodEnd time.Time
}

// claimCancelEmail atomically reserves the right to send one cancellation
// email. Only fires when subscription_status is 'cancelled' and the slot
// hasn't already been claimed. Re-subscribing clears cancel_email_sent_at
// back to NULL (see handleCreateSubscription) so a later cancel still sends.
func claimCancelEmail(ctx context.Context, db *sql.DB, userID uuid.UUID) (cancelClaim, bool) {
	var (
		email  string
		period string
		pe     *time.Time
	)
	err := db.QueryRowContext(ctx, `
		UPDATE users
		   SET cancel_email_sent_at = NOW()
		 WHERE id = $1
		   AND subscription_status = 'cancelled'
		   AND cancel_email_sent_at IS NULL
		RETURNING email, COALESCE(plan_period,'monthly'), current_period_end`,
		userID,
	).Scan(&email, &period, &pe)
	if err != nil {
		if err != sql.ErrNoRows {
			slog.Warn("claimCancelEmail: query failed", "error", err, "user_id", userID)
		}
		return cancelClaim{}, false
	}
	end := time.Time{}
	if pe != nil {
		end = *pe
	}
	return cancelClaim{Email: email, Period: period, PeriodEnd: end}, true
}

// planLabelFor returns the human label used in subject lines + email bodies.
func planLabelFor(period string) string {
	if period == "annual" {
		return "Developer · Annual"
	}
	return "Developer · Monthly"
}

// sendReceiptIfUnsent claims + sends a receipt, or is a no-op when already
// sent. The amount/currency are per-charge values; reconciler callers that
// don't have the charge entity should pass the plan's canonical amount
// (1200 USD for monthly, 12000 USD for annual).
func sendReceiptIfUnsent(ctx context.Context, db *sql.DB, em *emailer, userID uuid.UUID, amountCents int, currency string) {
	if em == nil {
		return
	}
	claim, ok := claimReceiptEmail(ctx, db, userID)
	if !ok || claim.Email == "" {
		return
	}
	if currency == "" {
		currency = "USD"
	}
	subject, html := receiptEmail(planLabelFor(claim.Period), amountCents, currency, claim.PeriodEnd)
	em.SendAsync(claim.Email, subject, html)
	slog.Info("billing email: receipt claimed + sent", "user_id", userID, "period", claim.Period)
}

// sendCancelIfUnsent claims + sends a cancellation notice.
func sendCancelIfUnsent(ctx context.Context, db *sql.DB, em *emailer, userID uuid.UUID) {
	if em == nil {
		return
	}
	claim, ok := claimCancelEmail(ctx, db, userID)
	if !ok || claim.Email == "" {
		return
	}
	subject, html := subscriptionCancelledEmail(planLabelFor(claim.Period), claim.PeriodEnd)
	em.SendAsync(claim.Email, subject, html)
	slog.Info("billing email: cancel claimed + sent", "user_id", userID, "period", claim.Period)
}

// planSwitchScheduledClaim carries the data needed to compose a
// "switch scheduled" email.
type planSwitchScheduledClaim struct {
	Email       string
	FromPeriod  string // "monthly" | "annual"
	ToPeriod    string
	EffectiveAt time.Time
}

// claimPlanSwitchScheduledEmail reserves the right to send one
// planSwitchScheduledEmail per (user, pending_plan_change). The claim is
// released when the handler calls UPDATE users SET pending_plan_change=$1,
// plan_switch_scheduled_email_sent_at=NULL — so a second switch request
// (after the first has been cancelled + re-initiated) still fires the email.
//
// Slot-not-claimed = (plan_switch_scheduled_email_sent_at IS NULL
//                     AND pending_plan_change IS NOT NULL).
func claimPlanSwitchScheduledEmail(ctx context.Context, db *sql.DB, userID uuid.UUID) (planSwitchScheduledClaim, bool) {
	var (
		email       string
		fromPeriod  string
		toPeriod    string
		effectiveAt *time.Time
	)
	err := db.QueryRowContext(ctx, `
		UPDATE users
		   SET plan_switch_scheduled_email_sent_at = NOW()
		 WHERE id = $1
		   AND pending_plan_change IS NOT NULL
		   AND plan_switch_scheduled_email_sent_at IS NULL
		RETURNING email, COALESCE(plan_period,'monthly'),
		          pending_plan_change, pending_plan_effective_at`,
		userID,
	).Scan(&email, &fromPeriod, &toPeriod, &effectiveAt)
	if err != nil {
		if err != sql.ErrNoRows {
			slog.Warn("claimPlanSwitchScheduledEmail: query failed", "error", err, "user_id", userID)
		}
		return planSwitchScheduledClaim{}, false
	}
	end := time.Time{}
	if effectiveAt != nil {
		end = *effectiveAt
	}
	return planSwitchScheduledClaim{
		Email:       email,
		FromPeriod:  fromPeriod,
		ToPeriod:    toPeriod,
		EffectiveAt: end,
	}, true
}

// sendPlanSwitchScheduledIfUnsent claims + sends the scheduled email.
// Only one call-site today (handleChangePlan) but the claim-lock keeps us
// safe if a retried request races.
func sendPlanSwitchScheduledIfUnsent(ctx context.Context, db *sql.DB, em *emailer, userID uuid.UUID) {
	if em == nil {
		return
	}
	claim, ok := claimPlanSwitchScheduledEmail(ctx, db, userID)
	if !ok || claim.Email == "" {
		return
	}
	subject, html := planSwitchScheduledEmail(
		planLabelFor(claim.FromPeriod),
		planLabelFor(claim.ToPeriod),
		claim.EffectiveAt,
	)
	em.SendAsync(claim.Email, subject, html)
	slog.Info("billing email: plan_switch scheduled claimed + sent",
		"user_id", userID, "from", claim.FromPeriod, "to", claim.ToPeriod)
}

// planSwitchActivatedClaim carries the data for the "switch is live" email.
type planSwitchActivatedClaim struct {
	Email       string
	NewPeriod   string
	NextRenewal time.Time
}

// claimPlanSwitchActivatedEmail reserves the right to send one
// planSwitchActivatedEmail per activation. Slot-not-claimed =
// plan_switch_activated_email_sent_at IS NULL AND the user just flipped
// to the new plan (plan_paid_at >= the previous switch's completion).
//
// Two call-sites race here: the reconciler's post-activate sweep and the
// webhook handler for subscription.activated with notes.purpose="plan_switch".
// The atomic UPDATE guarantees one wins.
func claimPlanSwitchActivatedEmail(ctx context.Context, db *sql.DB, userID uuid.UUID) (planSwitchActivatedClaim, bool) {
	var (
		email     string
		newPeriod string
		periodEnd *time.Time
	)
	err := db.QueryRowContext(ctx, `
		UPDATE users
		   SET plan_switch_activated_email_sent_at = NOW()
		 WHERE id = $1
		   AND plan_switch_activated_email_sent_at IS NULL
		   AND pending_plan_change IS NULL
		   AND plan_tier = 'paid'
		RETURNING email, COALESCE(plan_period,'monthly'), current_period_end`,
		userID,
	).Scan(&email, &newPeriod, &periodEnd)
	if err != nil {
		if err != sql.ErrNoRows {
			slog.Warn("claimPlanSwitchActivatedEmail: query failed", "error", err, "user_id", userID)
		}
		return planSwitchActivatedClaim{}, false
	}
	renewal := time.Time{}
	if periodEnd != nil {
		renewal = *periodEnd
	}
	return planSwitchActivatedClaim{
		Email:       email,
		NewPeriod:   newPeriod,
		NextRenewal: renewal,
	}, true
}

// sendPlanSwitchActivatedIfUnsent claims + sends the "you're now on X" email.
func sendPlanSwitchActivatedIfUnsent(ctx context.Context, db *sql.DB, em *emailer, userID uuid.UUID) {
	if em == nil {
		return
	}
	claim, ok := claimPlanSwitchActivatedEmail(ctx, db, userID)
	if !ok || claim.Email == "" {
		return
	}
	subject, html := planSwitchActivatedEmail(planLabelFor(claim.NewPeriod), claim.NextRenewal)
	em.SendAsync(claim.Email, subject, html)
	slog.Info("billing email: plan_switch activated claimed + sent",
		"user_id", userID, "period", claim.NewPeriod)
}
