package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"sync/atomic"

	"github.com/razorpay/razorpay-go"
)

// razorpayBaseURLOverride, when non-nil, replaces the Razorpay SDK's default
// https://api.razorpay.com base URL on every client built via newRazorpayClient.
// Production never sets it so live traffic hits the real API. Tests set it (and
// restore it via t.Cleanup) to point the SDK at an httptest.Server so we can
// assert on the HTTP shape we emit without making any real network calls.
//
// Stored as atomic.Pointer because the Razorpay SDK call in the reconciler
// path runs in a goroutine that may outlive the test's context cancellation.
// A plain var would race between that lingering goroutine reading and the
// test's Cleanup writing.
var razorpayBaseURLOverride atomic.Pointer[string]

func setRazorpayBaseURLOverride(url string) {
	if url == "" {
		razorpayBaseURLOverride.Store(nil)
		return
	}
	razorpayBaseURLOverride.Store(&url)
}

func loadRazorpayBaseURLOverride() string {
	if p := razorpayBaseURLOverride.Load(); p != nil {
		return *p
	}
	return ""
}

// newRazorpayClient builds a configured Razorpay SDK client. All code paths
// that talk to Razorpay must go through this helper so tests can intercept
// via setRazorpayBaseURLOverride.
func newRazorpayClient(cfg RazorpayConfig) *razorpay.Client {
	c := razorpay.NewClient(cfg.KeyID, cfg.KeySecret)
	if url := loadRazorpayBaseURLOverride(); url != "" {
		c.Request.BaseURL = url
	}
	return c
}

// ── Payment implementation ──────────────────────────────────────────────────

// razorpayPayment implements Payment by delegating to razorpay-go. Each
// method spawns a goroutine for the SDK call and selects on ctx.Done so a
// caller can bail out on timeout even though the SDK itself does not accept
// a context.
type razorpayPayment struct {
	cfg RazorpayConfig
}

// newRazorpayPayment returns a Payment-implementing Razorpay client. The
// caller is responsible for deciding *whether* to construct one — see
// main.Run, which falls back to noopPayment when credentials aren't set.
func newRazorpayPayment(cfg RazorpayConfig) razorpayPayment {
	return razorpayPayment{cfg: cfg}
}

func (p razorpayPayment) Configured() bool {
	return p.cfg.KeyID != "" && p.cfg.KeySecret != ""
}

type razorpayResult struct {
	data map[string]interface{}
	err  error
}

func (p razorpayPayment) runSDK(ctx context.Context, fn func(*razorpay.Client) (map[string]interface{}, error)) (map[string]interface{}, error) {
	resCh := make(chan razorpayResult, 1)
	go func() {
		c := newRazorpayClient(p.cfg)
		data, err := fn(c)
		resCh <- razorpayResult{data: data, err: err}
	}()
	select {
	case r := <-resCh:
		return r.data, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (p razorpayPayment) CreateOrder(ctx context.Context, data map[string]interface{}) (map[string]interface{}, error) {
	return p.runSDK(ctx, func(c *razorpay.Client) (map[string]interface{}, error) {
		return c.Order.Create(data, nil)
	})
}

func (p razorpayPayment) FetchOrder(ctx context.Context, orderID string) (map[string]interface{}, error) {
	return p.runSDK(ctx, func(c *razorpay.Client) (map[string]interface{}, error) {
		return c.Order.Fetch(orderID, nil, nil)
	})
}

func (p razorpayPayment) CreateSubscription(ctx context.Context, data map[string]interface{}) (map[string]interface{}, error) {
	return p.runSDK(ctx, func(c *razorpay.Client) (map[string]interface{}, error) {
		return c.Subscription.Create(data, nil)
	})
}

func (p razorpayPayment) CancelSubscription(ctx context.Context, subID string, opts map[string]interface{}) (map[string]interface{}, error) {
	return p.runSDK(ctx, func(c *razorpay.Client) (map[string]interface{}, error) {
		return c.Subscription.Cancel(subID, opts, nil)
	})
}

func (p razorpayPayment) ListSubscriptions(ctx context.Context, opts map[string]interface{}) (map[string]interface{}, error) {
	return p.runSDK(ctx, func(c *razorpay.Client) (map[string]interface{}, error) {
		return c.Subscription.All(opts, nil)
	})
}

// VerifyWebhookSignature confirms hex(HMAC-SHA256(body, WebhookSecret))
// matches the X-Razorpay-Signature header in constant time. Razorpay does
// not prefix a timestamp (unlike Stripe) so the body alone is the MAC input.
func (p razorpayPayment) VerifyWebhookSignature(body []byte, signature string) bool {
	if p.cfg.WebhookSecret == "" {
		return false
	}
	h := hmac.New(sha256.New, []byte(p.cfg.WebhookSecret))
	h.Write(body)
	expected := hex.EncodeToString(h.Sum(nil))
	return hmac.Equal([]byte(signature), []byte(expected))
}
