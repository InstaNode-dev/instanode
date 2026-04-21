package main

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
