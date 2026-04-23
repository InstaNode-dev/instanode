package server

import (
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
