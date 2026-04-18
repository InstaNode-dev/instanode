package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"

	"github.com/google/uuid"
	"github.com/razorpay/razorpay-go"
)

type CreateOrderRequest struct {
	PlanID string `json:"plan_id"` // e.g., "monthly", "annual"
}

type CreateOrderResponse struct {
	OrderID   string `json:"order_id"`
	Amount    int    `json:"amount"`
	Currency  string `json:"currency"`
	KeyID     string `json:"key_id"`
	Name      string `json:"name"`
	Email     string `json:"email"`
	Contact   string `json:"contact"`
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

	// Define plans
	plans := map[string]struct {
		amount   int
		currency string
		name     string
	}{
		"monthly": {50000, "INR", "Monthly Plan"}, // 500 INR
		"annual":  {500000, "INR", "Annual Plan"}, // 5000 INR
	}

	plan, ok := plans[req.PlanID]
	if !ok {
		http.Error(w, "Invalid plan", http.StatusBadRequest)
		return
	}

	client := razorpay.NewClient(s.cfg.Razorpay.KeyID, s.cfg.Razorpay.KeySecret)

	data := map[string]interface{}{
		"amount":          plan.amount,
		"currency":        plan.currency,
		"receipt":         uuid.New().String(),
		"payment_capture": 1,
		"notes": map[string]interface{}{
			"user_id": user.ID.String(),
		},
	}

	order, err := client.Order.Create(data, nil)
	if err != nil {
		http.Error(w, "Failed to create order", http.StatusInternalServerError)
		return
	}

	response := CreateOrderResponse{
		OrderID:  order["id"].(string),
		Amount:   plan.amount,
		Currency: plan.currency,
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

	if eventType == "payment.captured" {
		payload, ok := event["payload"].(map[string]interface{})
		if !ok {
			return
		}
		payment, ok := payload["payment"].(map[string]interface{})
		if !ok {
			return
		}
		entity, ok := payment["entity"].(map[string]interface{})
		if !ok {
			return
		}
		orderID, ok := entity["order_id"].(string)
		if !ok {
			return
		}

		// Get order to find user_id
		client := razorpay.NewClient(s.cfg.Razorpay.KeyID, s.cfg.Razorpay.KeySecret)
		order, err := client.Order.Fetch(orderID, nil, nil)
		if err != nil {
			return
		}
		notes, ok := order["notes"].(map[string]interface{})
		if !ok {
			return
		}
		userIDStr, ok := notes["user_id"].(string)
		if !ok {
			return
		}
		_, err = uuid.Parse(userIDStr)
		if err != nil {
			return
		}

		// TODO: Update user with razorpay_customer_id if needed
		// TODO: Migrate resources if token in query or something

		// For webhook, perhaps store the token in order notes.

		// For simplicity, assume the user has the token in their session or something.

		// Actually, in the flow, after login, the frontend has the token from ?token=, and can call an endpoint to migrate.

		// So, perhaps add a migrate endpoint.

	}

	w.WriteHeader(http.StatusOK)
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