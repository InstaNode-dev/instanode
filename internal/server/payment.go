package server

import (
	"context"
	"errors"
)

// ErrPaymentNotConfigured is returned by every Payment method on a noopPayment.
// Handlers that require billing should map it to 503 Service Unavailable — the
// operator hasn't wired up a billing provider for this deployment.
var ErrPaymentNotConfigured = errors.New("payment: provider not configured")

// Payment abstracts the billing provider. Two implementations ship today:
//
//   - razorpayPayment (internal/server/razorpay_client.go) — real Razorpay
//     calls; selected by main.Run when RAZORPAY_KEY_ID + RAZORPAY_KEY_SECRET
//     are both set.
//   - noopPayment (internal/server/payment_noop.go) — returns
//     ErrPaymentNotConfigured from every call; selected as the default so a
//     self-hoster can run the rest of the API (provisioning, auth, webhooks)
//     without signing up for Razorpay.
//
// Method payloads are map[string]interface{} today because that's the shape
// the razorpay-go SDK uses. As more providers land, replace with typed
// structs — razorpayPayment can marshal between the two.
type Payment interface {
	// Configured reports whether the backend has enough config
	// (credentials, plan IDs) to service live requests. Handlers should
	// short-circuit to 503 when this is false instead of calling through.
	Configured() bool

	// Orders — legacy one-time-charge flow (POST /billing/create-order).
	CreateOrder(ctx context.Context, data map[string]interface{}) (map[string]interface{}, error)
	FetchOrder(ctx context.Context, orderID string) (map[string]interface{}, error)

	// Subscriptions — recurring billing (preferred flow).
	CreateSubscription(ctx context.Context, data map[string]interface{}) (map[string]interface{}, error)
	CancelSubscription(ctx context.Context, subID string, opts map[string]interface{}) (map[string]interface{}, error)
	ListSubscriptions(ctx context.Context, opts map[string]interface{}) (map[string]interface{}, error)

	// VerifyWebhookSignature returns true iff signature is a valid
	// provider-specific MAC of body. Boolean rather than error so
	// handlers don't branch on error types for the common auth path.
	VerifyWebhookSignature(body []byte, signature string) bool
}
