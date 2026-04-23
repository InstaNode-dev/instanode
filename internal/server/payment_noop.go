package server

import "context"

// noopPayment is the default Payment when no billing provider is configured.
// Every method returns ErrPaymentNotConfigured so handlers can uniformly map
// that to 503. Webhook signature checks always fail — unsigned callers can't
// drive state changes on a deployment without credentials.
type noopPayment struct{}

func (noopPayment) Configured() bool { return false }

func (noopPayment) CreateOrder(ctx context.Context, data map[string]interface{}) (map[string]interface{}, error) {
	return nil, ErrPaymentNotConfigured
}

func (noopPayment) FetchOrder(ctx context.Context, orderID string) (map[string]interface{}, error) {
	return nil, ErrPaymentNotConfigured
}

func (noopPayment) CreateSubscription(ctx context.Context, data map[string]interface{}) (map[string]interface{}, error) {
	return nil, ErrPaymentNotConfigured
}

func (noopPayment) CancelSubscription(ctx context.Context, subID string, opts map[string]interface{}) (map[string]interface{}, error) {
	return nil, ErrPaymentNotConfigured
}

func (noopPayment) ListSubscriptions(ctx context.Context, opts map[string]interface{}) (map[string]interface{}, error) {
	return nil, ErrPaymentNotConfigured
}

func (noopPayment) VerifyWebhookSignature(body []byte, signature string) bool { return false }
