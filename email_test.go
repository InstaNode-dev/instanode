package main

import (
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
)

// jwtLike matches a three-segment base64url string prefixed with "eyJ" —
// a shape typical of JWTs. The previous welcome template leaked the token,
// so we assert it never appears in the current welcome HTML.
var jwtLike = regexp.MustCompile(`eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`)

func TestWelcomeEmail_SubjectMentionsWelcome(t *testing.T) {
	subject, _ := welcomeEmail()
	if subject == "" {
		t.Fatal("welcomeEmail subject is empty")
	}
	if !strings.Contains(subject, "Welcome") {
		t.Errorf("welcomeEmail subject should mention 'Welcome', got %q", subject)
	}
}

func TestWelcomeEmail_HTMLPointsToDashboard(t *testing.T) {
	_, html := welcomeEmail()
	if !strings.Contains(html, "instanode.dev/dashboard") {
		t.Errorf("welcomeEmail html should reference 'instanode.dev/dashboard', got:\n%s", html)
	}
}

func TestWelcomeEmail_NoLeakedJWT(t *testing.T) {
	_, html := welcomeEmail()
	if matches := jwtLike.FindAllString(html, -1); len(matches) != 0 {
		t.Errorf("welcomeEmail html contained JWT-shaped token(s): %v\nhtml:\n%s", matches, html)
	}
}

func TestWelcomeEmail_ContainsContactFooter(t *testing.T) {
	_, html := welcomeEmail()
	if !strings.Contains(html, "contact@instanode.dev") {
		t.Errorf("welcomeEmail html missing 'contact@instanode.dev' footer, got:\n%s", html)
	}
}

func TestReceiptEmail_SubjectMentionsPaymentReceived(t *testing.T) {
	periodEnd := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	subject, _ := receiptEmail("Developer", 1200, "USD", periodEnd)
	if !strings.Contains(subject, "Payment received") {
		t.Errorf("receiptEmail subject should contain 'Payment received', got %q", subject)
	}
}

func TestReceiptEmail_HTMLContainsPlanAmountAndDate(t *testing.T) {
	periodEnd := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	_, html := receiptEmail("Developer", 1200, "USD", periodEnd)

	if !strings.Contains(html, "Developer") {
		t.Errorf("receiptEmail html missing plan label 'Developer':\n%s", html)
	}
	if !strings.Contains(html, "12.00") {
		t.Errorf("receiptEmail html missing formatted amount '12.00':\n%s", html)
	}
	if !strings.Contains(html, "USD") {
		t.Errorf("receiptEmail html missing currency 'USD':\n%s", html)
	}
	if !strings.Contains(html, "2026-05-20") {
		t.Errorf("receiptEmail html missing period-end date '2026-05-20':\n%s", html)
	}
}

func TestReceiptEmail_AmountRounding(t *testing.T) {
	tests := []struct {
		name        string
		amountCents int
		want        string
	}{
		{"1200 cents formats as 12.00", 1200, "12.00"},
		{"12000 cents formats as 120.00", 12000, "120.00"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			periodEnd := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			_, html := receiptEmail("Developer", tc.amountCents, "USD", periodEnd)
			if !strings.Contains(html, tc.want) {
				t.Errorf("receiptEmail html missing expected amount %q for %d cents:\n%s", tc.want, tc.amountCents, html)
			}
		})
	}
}

func TestPaymentFailedEmail_SubjectIncludesFailed(t *testing.T) {
	subject, _ := paymentFailedEmail("")
	if !strings.Contains(strings.ToLower(subject), "failed") {
		t.Errorf("paymentFailedEmail subject should include 'failed' (case-insensitive), got %q", subject)
	}
}

func TestPaymentFailedEmail_EmptyReasonFallsBackToDefault(t *testing.T) {
	_, html := paymentFailedEmail("")
	if strings.TrimSpace(html) == "" {
		t.Fatal("paymentFailedEmail html is empty for empty reason")
	}
	// Must contain a non-empty default message (something human-readable about the
	// payment being declined / nothing charged) — not an empty placeholder.
	if !strings.Contains(html, "declined") && !strings.Contains(html, "Nothing has been charged") {
		t.Errorf("paymentFailedEmail empty-reason html missing default message:\n%s", html)
	}
}

func TestPaymentFailedEmail_NonEmptyReasonIsIncluded(t *testing.T) {
	reason := "Card issuer rejected the charge: insufficient funds"
	_, html := paymentFailedEmail(reason)
	if !strings.Contains(html, reason) {
		t.Errorf("paymentFailedEmail html missing supplied reason %q:\n%s", reason, html)
	}
}

func TestPaymentFailedEmail_ContainsContactFooter(t *testing.T) {
	_, html := paymentFailedEmail("anything")
	if !strings.Contains(html, "contact@instanode.dev") {
		t.Errorf("paymentFailedEmail html missing 'contact@instanode.dev':\n%s", html)
	}
}

func TestNewEmailer_ReturnsNonNil(t *testing.T) {
	e := newEmailer(EmailConfig{})
	if e == nil {
		t.Fatal("newEmailer returned nil")
	}
}

func TestEmailer_SendAsync_NoopWhenSMTPHostEmpty(t *testing.T) {
	// Snapshot the goroutine count, SendAsync with an empty SMTPHost, and assert
	// no goroutine was spawned and no panic occurred. The function should return
	// immediately without any background work.
	before := runtime.NumGoroutine()
	e := newEmailer(EmailConfig{}) // SMTPHost == ""

	// If SendAsync spawns a goroutine here, that's a regression — the no-op
	// branch must bail out before `go func()`.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("SendAsync panicked on no-op path: %v", r)
		}
	}()
	e.SendAsync("to@example.com", "subject", "<p>body</p>")

	// Give the scheduler a beat in case a stray goroutine was spawned.
	time.Sleep(20 * time.Millisecond)
	after := runtime.NumGoroutine()
	if after > before {
		t.Errorf("SendAsync no-op path spawned goroutine(s): before=%d after=%d", before, after)
	}
}
