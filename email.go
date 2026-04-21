package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/smtp"
	"time"
)

type emailer struct {
	cfg EmailConfig
}

func newEmailer(cfg EmailConfig) *emailer {
	if cfg.SMTPHost == "" {
		slog.Info("email: transport not configured, Send() will be a no-op")
	} else {
		slog.Info("email: configured", "host", cfg.SMTPHost, "port", cfg.SMTPPort, "from", cfg.FromAddress)
	}
	return &emailer{cfg: cfg}
}

// SendAsync fires off a send in a goroutine with a bounded deadline.
// Call-site is never blocked — failures are logged, never propagated.
// Call-sites: welcome-on-first-login, Razorpay payment receipt.
func (e *emailer) SendAsync(to, subject, htmlBody string) {
	if e.cfg.SMTPHost == "" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := e.send(ctx, to, subject, htmlBody); err != nil {
			slog.Warn("email: send failed", "to", to, "subject", subject, "error", err)
			return
		}
		slog.Info("email: sent", "to", to, "subject", subject)
	}()
}

func (e *emailer) send(ctx context.Context, to, subject, htmlBody string) error {
	addr := fmt.Sprintf("%s:%d", e.cfg.SMTPHost, e.cfg.SMTPPort)
	from := fmt.Sprintf("%s <%s>", e.cfg.FromName, e.cfg.FromAddress)
	msg := []byte(fmt.Sprintf(
		"From: %s\r\nTo: %s\r\nSubject: %s\r\nMIME-Version: 1.0\r\nContent-Type: text/html; charset=UTF-8\r\n\r\n%s",
		from, to, subject, htmlBody,
	))

	type result struct{ err error }
	done := make(chan result, 1)
	go func() {
		auth := smtp.PlainAuth("", e.cfg.SMTPUser, e.cfg.SMTPPass, e.cfg.SMTPHost)
		done <- result{smtp.SendMail(addr, auth, e.cfg.FromAddress, []string{to}, msg)}
	}()
	select {
	case r := <-done:
		return r.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ── Templates (inline HTML — deliberately minimal; swap for Brevo template IDs if volume grows). ──

func welcomeEmail() (subject, html string) {
	subject = "Welcome to instanode"
	html = `<!doctype html>
<html><body style="font-family:system-ui,sans-serif;max-width:560px;margin:0 auto;padding:24px;color:#222;">
<p>You're in.</p>
<p>instanode provisions real Postgres databases in one HTTP call — no setup, no Docker, no separate dashboard to babysit.</p>
<p><strong>Get your API token</strong></p>
<ol style="padding-left:20px;">
  <li>Open your <a href="https://instanode.dev/dashboard.html">dashboard</a>.</li>
  <li>Click <strong>Reveal API token</strong>.</li>
  <li>Copy the bearer JWT and export it as <code style="background:#f4f4f4;padding:1px 4px;border-radius:3px;">INSTANODE_TOKEN</code>.</li>
</ol>
<p>Minimal usage:</p>
<pre style="background:#f4f4f4;padding:12px;border-radius:6px;font-size:12px;overflow-x:auto;">curl -s -X POST https://api.instanode.dev/db/new \
  -H "Authorization: Bearer $INSTANODE_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"name":"my-project"}'</pre>
<p>More: <a href="https://instanode.dev/agent.html">instanode.dev/agent.html</a> · <a href="https://api.instanode.dev/llms.txt">llms.txt</a></p>
<p style="margin-top:32px;color:#666;font-size:12px;">You're on the free tier. $12/mo or $120/yr ($10/mo equivalent) makes resources permanent and removes the 24h TTL. Upgrade: <a href="https://instanode.dev/pricing.html">instanode.dev/pricing.html</a></p>
<p style="color:#888;font-size:11px;">For any issues or queries, contact <a href="mailto:contact@instanode.dev" style="color:#888;">contact@instanode.dev</a>.</p>
</body></html>`
	return
}

func paymentFailedEmail(reason string) (subject, html string) {
	subject = "Payment failed — no charge was made"
	if reason == "" {
		reason = "The payment was declined by your bank or the card network."
	}
	html = fmt.Sprintf(`<!doctype html>
<html><body style="font-family:system-ui,sans-serif;max-width:560px;margin:0 auto;padding:24px;color:#222;">
<p>Your recent payment attempt did not go through.</p>
<p style="background:#fff5f5;border-left:3px solid #e33;padding:10px 14px;margin:16px 0;color:#933;font-size:13px;">%s</p>
<p>Nothing has been charged. Your account tier is unchanged and any free-tier resources are still active.</p>
<p><strong>Common fixes:</strong></p>
<ul style="padding-left:20px;">
  <li>Try a different card — some issuers block first-time international charges by default.</li>
  <li>Confirm your card allows online / CNP (card-not-present) transactions.</li>
  <li>Check that OTP/3-D Secure was completed on the issuer's page.</li>
</ul>
<p>Retry from the <a href="https://instanode.dev/pricing.html">pricing page</a> whenever you're ready.</p>
<p style="color:#888;font-size:11px;margin-top:32px;">For any issues or queries, contact <a href="mailto:contact@instanode.dev" style="color:#888;">contact@instanode.dev</a>.</p>
</body></html>`, reason)
	return
}

func subscriptionCancelledEmail(plan string, periodEnd time.Time) (subject, html string) {
	subject = "Your instanode subscription has been cancelled"
	untilLine := ""
	if !periodEnd.IsZero() {
		untilLine = fmt.Sprintf("<p>Paid access continues until <strong>%s</strong>. After that, your account reverts to the free tier.</p>", periodEnd.Format("2006-01-02"))
	} else {
		untilLine = "<p>Your account will revert to the free tier at the end of the current billing period.</p>"
	}
	html = fmt.Sprintf(`<!doctype html>
<html><body style="font-family:system-ui,sans-serif;max-width:560px;margin:0 auto;padding:24px;color:#222;">
<p>We've cancelled your <strong>%s</strong> subscription — no further charges will be made.</p>
%s
<p>Resources provisioned while you were on the paid plan stay reachable, but they'll start to expire on the free-tier TTL once your access downgrades. Re-subscribe any time from the <a href="https://instanode.dev/pricing.html">pricing page</a> to keep them permanent.</p>
<p style="color:#888;font-size:11px;margin-top:32px;">For any issues or queries, contact <a href="mailto:contact@instanode.dev" style="color:#888;">contact@instanode.dev</a>.</p>
</body></html>`, plan, untilLine)
	return
}

// planSwitchScheduledEmail is sent immediately after POST /billing/change-plan
// succeeds. fromPlan / toPlan are the human labels (from planLabelFor).
// effectiveAt is the current_period_end at request time — when the switch will
// actually fire. The user stays on fromPlan until that boundary.
func planSwitchScheduledEmail(fromPlan, toPlan string, effectiveAt time.Time) (subject, html string) {
	subject = "Plan switch scheduled — instanode"
	when := "the end of your current billing period"
	if !effectiveAt.IsZero() {
		when = effectiveAt.Format("2006-01-02")
	}
	html = fmt.Sprintf(`<!doctype html>
<html><body style="font-family:system-ui,sans-serif;max-width:560px;margin:0 auto;padding:24px;color:#222;">
<p>You've scheduled a switch from <strong>%s</strong> to <strong>%s</strong>.</p>
<p>The switch becomes effective on <strong>%s</strong>. Your current plan keeps running until then — no charge today.</p>
<p>If you change your mind, open your <a href="https://instanode.dev/dashboard.html">dashboard</a> and click <strong>Keep current plan</strong> before that date.</p>
<p style="color:#888;font-size:11px;margin-top:32px;">For any issues or queries, contact <a href="mailto:contact@instanode.dev" style="color:#888;">contact@instanode.dev</a>.</p>
</body></html>`, fromPlan, toPlan, when)
	return
}

// planSwitchActivatedEmail fires once, after the reconciler has flipped the
// subscription to the new plan and the first charge on the new plan has
// landed. A separate receipt email for the charge amount follows through the
// normal subscription.charged path.
func planSwitchActivatedEmail(newPlan string, nextRenewal time.Time) (subject, html string) {
	subject = "You're now on " + newPlan
	renewal := ""
	if !nextRenewal.IsZero() {
		renewal = fmt.Sprintf("Next renewal: <strong>%s</strong>.", nextRenewal.Format("2006-01-02"))
	}
	html = fmt.Sprintf(`<!doctype html>
<html><body style="font-family:system-ui,sans-serif;max-width:560px;margin:0 auto;padding:24px;">
<p>Your plan switch is live. You're now on <strong>%s</strong>.</p>
<p>%s A separate payment receipt will follow for this charge.</p>
<p>Resources on your account stay permanent. Dashboard: <a href="https://instanode.dev/dashboard.html">instanode.dev/dashboard.html</a></p>
<p style="color:#888;font-size:11px;margin-top:32px;">For any issues or queries, contact <a href="mailto:contact@instanode.dev" style="color:#888;">contact@instanode.dev</a>.</p>
</body></html>`, newPlan, renewal)
	return
}

// planSwitchCancelledEmail is sent when DELETE /billing/change-plan is called
// *after* planSwitchScheduledEmail has already fired. If the scheduled email
// never made it out (same-second cancel, or SMTP dropped), the handler skips
// this one — the user was never told the switch was coming, so telling them
// it's "cancelled" would be confusing.
func planSwitchCancelledEmail(stayingOn, dropped string) (subject, html string) {
	subject = "Plan switch cancelled — staying on " + stayingOn
	html = fmt.Sprintf(`<!doctype html>
<html><body style="font-family:system-ui,sans-serif;max-width:560px;margin:0 auto;padding:24px;">
<p>Your scheduled switch to <strong>%s</strong> has been cancelled.</p>
<p>You'll stay on <strong>%s</strong> — your next renewal date is unchanged.</p>
<p>If this wasn't you, reply to this email or <a href="mailto:contact@instanode.dev">contact@instanode.dev</a>.</p>
<p style="color:#888;font-size:11px;margin-top:32px;">For any issues or queries, contact <a href="mailto:contact@instanode.dev" style="color:#888;">contact@instanode.dev</a>.</p>
</body></html>`, dropped, stayingOn)
	return
}

func receiptEmail(plan string, amountCents int, currency string, periodEnd time.Time) (subject, html string) {
	amount := fmt.Sprintf("%.2f %s", float64(amountCents)/100.0, currency)
	subject = "Payment received — instanode Developer plan"
	html = fmt.Sprintf(`<!doctype html>
<html><body style="font-family:system-ui,sans-serif;max-width:560px;margin:0 auto;padding:24px;color:#222;">
<p>Payment confirmed.</p>
<table style="border-collapse:collapse;margin:12px 0;">
<tr><td style="padding:4px 12px 4px 0;color:#666;">Plan</td><td style="padding:4px 0;"><strong>%s</strong></td></tr>
<tr><td style="padding:4px 12px 4px 0;color:#666;">Amount</td><td style="padding:4px 0;"><strong>%s</strong></td></tr>
<tr><td style="padding:4px 12px 4px 0;color:#666;">Next renewal</td><td style="padding:4px 0;"><strong>%s</strong></td></tr>
</table>
<p>All resources on your account are now permanent — the 24h TTL is lifted. Existing anonymous databases you've claimed keep their data.</p>
<p>Dashboard: <a href="https://instanode.dev/dashboard.html">instanode.dev/dashboard.html</a></p>
<p style="color:#888;font-size:11px;">For any issues or queries, contact <a href="mailto:contact@instanode.dev" style="color:#888;">contact@instanode.dev</a>. Cancel anytime from the same address.</p>
</body></html>`, plan, amount, periodEnd.Format("2006-01-02"))
	return
}
