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

func welcomeEmail(apiToken string) (subject, html string) {
	subject = "Welcome to instanode"
	html = fmt.Sprintf(`<!doctype html>
<html><body style="font-family:system-ui,sans-serif;max-width:560px;margin:0 auto;padding:24px;color:#222;">
<p>You're in.</p>
<p>instanode provisions real Postgres databases in one HTTP call — no setup, no Docker, no separate dashboard to babysit.</p>
<p>Your API token (bearer JWT, lifts the free-tier rate cap):</p>
<pre style="background:#f4f4f4;padding:12px;border-radius:6px;font-size:12px;word-break:break-all;">%s</pre>
<p>Minimal usage:</p>
<pre style="background:#f4f4f4;padding:12px;border-radius:6px;font-size:12px;">curl -s -X POST https://api.instanode.dev/db/new \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"name":"my-project"}'</pre>
<p>More: <a href="https://instanode.dev/agent.html">instanode.dev/agent.html</a> · <a href="https://api.instanode.dev/llms.txt">llms.txt</a></p>
<p style="margin-top:32px;color:#666;font-size:12px;">You're on the free tier. $12/mo or $120/yr ($10/mo equivalent) makes resources permanent and removes the 24h TTL. Upgrade: <a href="https://instanode.dev/pricing.html">instanode.dev/pricing.html</a></p>
<p style="color:#888;font-size:11px;">For any issues or queries, contact <a href="mailto:contact@instanode.dev" style="color:#888;">contact@instanode.dev</a>.</p>
</body></html>`, apiToken)
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
