package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"

	"github.com/google/uuid"
	"github.com/razorpay/razorpay-go"
)

type CreateOrderRequest struct {
	PlanID   string `json:"plan_id"`        // e.g., "developer"
	Currency string `json:"currency"`       // "USD" | "EUR" | "GBP" | "INR"
	Token    string `json:"token,omitempty"` // optional anon resource token to upgrade atomically on payment
}

type CreateOrderResponse struct {
	OrderID  string `json:"order_id"`
	Amount   int    `json:"amount"`
	Currency string `json:"currency"`
	KeyID    string `json:"key_id"`
	Name     string `json:"name"`
	Email    string `json:"email"`
	Contact  string `json:"contact"`
}

// planPricing holds minor-unit amounts (cents / paise) per currency.
// Monthly Developer: $12. Annual Developer: $120 (two months free).
// INR prices mirror the USD ratio — ₹999/mo and ₹9,990/yr.
var planPricing = map[string]map[string]int{
	"developer": {
		"USD": 1200,
		"EUR": 1200,
		"GBP": 1200,
		"INR": 99900,
	},
	"developer-annual": {
		"USD": 12000,
		"EUR": 12000,
		"GBP": 12000,
		"INR": 999000,
	},
}

func (s *server) handleCreateOrder(w http.ResponseWriter, r *http.Request) {
	user, err := s.getUserFromRequest(r)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	var req CreateOrderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	if req.Currency == "" {
		req.Currency = "USD"
	}

	currencies, ok := planPricing[req.PlanID]
	if !ok {
		http.Error(w, "Invalid plan", http.StatusBadRequest)
		return
	}
	amount, ok := currencies[req.Currency]
	if !ok {
		http.Error(w, "Invalid currency", http.StatusBadRequest)
		return
	}

	client := razorpay.NewClient(s.cfg.Razorpay.KeyID, s.cfg.Razorpay.KeySecret)

	notes := map[string]interface{}{
		"user_id": user.ID.String(),
		"plan_id": req.PlanID,
	}
	if req.Token != "" {
		if _, err := uuid.Parse(req.Token); err == nil {
			notes["token"] = req.Token
		}
	}

	data := map[string]interface{}{
		"amount":          amount,
		"currency":        req.Currency,
		"receipt":         uuid.New().String(),
		"payment_capture": 1,
		"notes":           notes,
	}

	order, err := client.Order.Create(data, nil)
	if err != nil {
		slog.Error("razorpay order create failed", "error", err, "user_id", user.ID, "plan", req.PlanID, "currency", req.Currency)
		http.Error(w, "Failed to create order", http.StatusInternalServerError)
		return
	}

	response := CreateOrderResponse{
		OrderID:  order["id"].(string),
		Amount:   amount,
		Currency: req.Currency,
		KeyID:    s.cfg.Razorpay.KeyID,
		Name:     "InstaNode User",
		Email:    user.Email,
		Contact:  "", // Optional
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func (s *server) handleRazorpayWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read body", http.StatusBadRequest)
		return
	}

	signature := r.Header.Get("X-Razorpay-Signature")
	expectedSignature := s.computeSignature(string(body), s.cfg.Razorpay.WebhookSecret)
	if !hmac.Equal([]byte(signature), []byte(expectedSignature)) {
		http.Error(w, "Invalid signature", http.StatusUnauthorized)
		return
	}

	var event map[string]interface{}
	if err := json.Unmarshal(body, &event); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	eventType, ok := event["event"].(string)
	if !ok {
		http.Error(w, "No event type", http.StatusBadRequest)
		return
	}

	// Extract payment entity (present on both payment.captured and payment.failed).
	payload, _ := event["payload"].(map[string]interface{})
	paymentMap, _ := payload["payment"].(map[string]interface{})
	entity, _ := paymentMap["entity"].(map[string]interface{})

	paymentID, _ := entity["id"].(string)
	if paymentID == "" {
		// Nothing to dedupe on — accept and return 200 so Razorpay doesn't retry.
		slog.Warn("razorpay webhook missing payment id", "event", eventType)
		w.WriteHeader(http.StatusOK)
		return
	}

	// Idempotency: record the payment id; if it was already seen, no-op.
	res, err := s.db.Exec(
		"INSERT INTO processed_webhooks (event_id) VALUES ($1) ON CONFLICT (event_id) DO NOTHING",
		paymentID,
	)
	if err != nil {
		slog.Error("webhook dedup insert failed", "error", err, "payment_id", paymentID)
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		slog.Info("razorpay webhook already processed; skipping", "payment_id", paymentID, "event", eventType)
		w.WriteHeader(http.StatusOK)
		return
	}

	switch eventType {
	case "payment.captured":
		s.handlePaymentCaptured(entity, paymentID)
	case "payment.failed":
		orderID, _ := entity["order_id"].(string)
		reason, _ := entity["error_description"].(string)
		if reason == "" {
			reason, _ = entity["error_reason"].(string)
		}
		slog.Warn("razorpay payment failed", "payment_id", paymentID, "order_id", orderID, "reason", reason)
	default:
		slog.Info("razorpay webhook event ignored", "event", eventType, "payment_id", paymentID)
	}

	w.WriteHeader(http.StatusOK)
}

// handlePaymentCaptured promotes the paying user's resources to the paid tier.
// Errors are logged but not returned — we've already recorded the payment id as
// processed, so returning 200 is correct; operator alerts pick up the log.
func (s *server) handlePaymentCaptured(entity map[string]interface{}, paymentID string) {
	orderID, _ := entity["order_id"].(string)
	if orderID == "" {
		slog.Error("payment.captured missing order_id", "payment_id", paymentID)
		return
	}

	customerID, _ := entity["customer_id"].(string)

	// Fetch order to read notes.user_id.
	client := razorpay.NewClient(s.cfg.Razorpay.KeyID, s.cfg.Razorpay.KeySecret)
	order, err := client.Order.Fetch(orderID, nil, nil)
	if err != nil {
		slog.Error("razorpay order fetch failed", "error", err, "order_id", orderID, "payment_id", paymentID)
		return
	}
	notes, ok := order["notes"].(map[string]interface{})
	if !ok {
		slog.Error("razorpay order missing notes", "order_id", orderID, "payment_id", paymentID)
		return
	}
	userIDStr, ok := notes["user_id"].(string)
	if !ok || userIDStr == "" {
		slog.Error("razorpay order notes missing user_id", "order_id", orderID, "payment_id", paymentID)
		return
	}
	userID, err := uuid.Parse(userIDStr)
	if err != nil {
		slog.Error("razorpay order notes user_id invalid", "error", err, "user_id", userIDStr, "order_id", orderID)
		return
	}

	// Promote the user's account tier first (independent of whether the
	// payment entity carried a customer_id — in test mode it often doesn't).
	if _, err := s.db.Exec(
		"UPDATE users SET plan_tier = 'paid' WHERE id = $1",
		userID,
	); err != nil {
		slog.Error("failed to promote user plan_tier", "error", err, "user_id", userID)
	}

	if customerID != "" {
		if _, err := s.db.Exec(
			"UPDATE users SET razorpay_customer_id = $1 WHERE id = $2",
			customerID, userID,
		); err != nil {
			slog.Error("failed to set razorpay_customer_id", "error", err, "user_id", userID, "customer_id", customerID)
		}
	}

	// If the anonymous-flow token is in notes, claim + promote that specific
	// resource atomically. Keeps the "pay before login" path working.
	if tokenStr, _ := notes["token"].(string); tokenStr != "" {
		if tokenUUID, err := uuid.Parse(tokenStr); err == nil {
			if _, err := s.db.Exec(
				`UPDATE resources SET migrated_to_user_id = $1, tier = 'paid', expires_at = NULL
				 WHERE token = $2 AND status = 'active'`,
				userID, tokenUUID,
			); err != nil {
				slog.Error("failed to claim token on payment", "error", err, "user_id", userID, "token", tokenStr)
			}
		}
	}

	res, err := s.db.Exec(
		"UPDATE resources SET tier = 'paid', expires_at = NULL WHERE migrated_to_user_id = $1 AND status = 'active'",
		userID,
	)
	if err != nil {
		slog.Error("failed to promote resources to paid tier", "error", err, "user_id", userID)
		return
	}
	affected, _ := res.RowsAffected()
	slog.Info("razorpay payment captured; tier upgraded",
		"user_id", userID,
		"order_id", orderID,
		"payment_id", paymentID,
		"customer_id", customerID,
		"resources_promoted", affected,
	)
}

func (s *server) computeSignature(payload, secret string) string {
	h := hmac.New(sha256.New, []byte(secret))
	h.Write([]byte(payload))
	return hex.EncodeToString(h.Sum(nil))
}

func (s *server) handleMigrateResource(w http.ResponseWriter, r *http.Request) {
	user, err := s.getUserFromRequest(r)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	tokenStr := r.URL.Query().Get("token")
	if tokenStr == "" {
		http.Error(w, "No token", http.StatusBadRequest)
		return
	}

	token, err := uuid.Parse(tokenStr)
	if err != nil {
		http.Error(w, "Invalid token", http.StatusBadRequest)
		return
	}

	// Update resources with this token to migrated_to_user_id
	_, err = s.db.Exec("UPDATE resources SET migrated_to_user_id = $1, tier = 'paid', expires_at = NULL WHERE token = $2 AND migrated_to_user_id IS NULL", user.ID, token)
	if err != nil {
		http.Error(w, "Failed to migrate", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}
