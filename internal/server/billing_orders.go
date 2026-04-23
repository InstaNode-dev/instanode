package server

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/google/uuid"
)

type CreateOrderRequest struct {
	PlanID   string `json:"plan_id"`         // e.g., "developer"
	Currency string `json:"currency"`        // "USD" | "EUR" | "GBP" | "INR"
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
	// Note: no direct platform-PG calls here; authUser handles its own 5s
	// timeout internally. The only external call is client.Order.Create
	// below, which the Razorpay Go SDK runs without context support — see
	// comment at that call site.
	user := s.authUser(r)
	if user == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Sign in required.")
		return
	}

	var req CreateOrderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "Request body must be JSON.")
		return
	}

	if req.Currency == "" {
		req.Currency = "USD"
	}

	currencies, ok := planPricing[req.PlanID]
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_plan", "Unknown plan_id.")
		return
	}
	amount, ok := currencies[req.Currency]
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_currency", "Supported currencies: USD, EUR, GBP, INR.")
		return
	}

	client := newRazorpayClient(s.cfg.Razorpay)

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

	// LIMITATION: the Razorpay Go SDK does not accept a context.Context here,
	// so we cannot enforce our 5s request budget on this call. It will stall
	// up to Razorpay's own SDK-internal HTTP timeout (currently unbounded in
	// razorpay-go). If this becomes a production hang risk, wrap with a
	// channel + time.After pattern and abandon the goroutine on timeout.
	order, err := client.Order.Create(data, nil)
	if err != nil {
		slog.ErrorContext(r.Context(), "razorpay order create failed", "error", err, "user_id", user.ID, "plan", req.PlanID, "currency", req.Currency)
		writeError(w, http.StatusBadGateway, "payment_gateway_error", "Payment provider is unavailable — please try again in a moment.")
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
