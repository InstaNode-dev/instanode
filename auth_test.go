package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// newTestServer builds a minimal *server with just cfg.JWT.Secret populated
// so we can exercise the JWT helpers without touching the DB or any other
// subsystem.
func newTestServer(t *testing.T) *server {
	t.Helper()
	return &server{cfg: &Config{JWT: JWTConfig{Secret: "test-secret-must-be-long-enough-pad"}}}
}

func TestGenerateAndParseJWT_RoundTrip(t *testing.T) {
	s := newTestServer(t)
	userID := uuid.New()

	tok, err := s.generateJWT(userID)
	if err != nil {
		t.Fatalf("generateJWT returned error: %v", err)
	}
	if tok == "" {
		t.Fatal("generateJWT returned empty token")
	}

	claims, err := s.parseJWT(tok)
	if err != nil {
		t.Fatalf("parseJWT returned error: %v", err)
	}
	if claims.UserID != userID {
		t.Fatalf("claims.UserID = %v, want %v", claims.UserID, userID)
	}
}

func TestParseJWT_WrongSecretFails(t *testing.T) {
	signer := newTestServer(t)
	verifier := &server{cfg: &Config{JWT: JWTConfig{Secret: "a-completely-different-secret-value-32b"}}}

	tok, err := signer.generateJWT(uuid.New())
	if err != nil {
		t.Fatalf("generateJWT: %v", err)
	}

	if _, err := verifier.parseJWT(tok); err == nil {
		t.Fatal("parseJWT with wrong secret: expected error, got nil")
	}
}

func TestParseJWT_GarbageStringFails(t *testing.T) {
	s := newTestServer(t)

	cases := []string{
		"",
		"not-a-jwt",
		"aaaa.bbbb.cccc",
		"header.payload", // missing signature segment
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc, func(t *testing.T) {
			if _, err := s.parseJWT(tc); err == nil {
				t.Fatalf("parseJWT(%q): expected error, got nil", tc)
			}
		})
	}
}

func TestParseJWT_ExpiredTokenFails(t *testing.T) {
	s := newTestServer(t)

	// Build a token with a valid HMAC (same secret) but ExpiresAt 1h ago.
	claims := Claims{
		UserID: uuid.New(),
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(-1 * time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now().Add(-2 * time.Hour)),
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := tok.SignedString([]byte(s.cfg.JWT.Secret))
	if err != nil {
		t.Fatalf("failed to sign expired token: %v", err)
	}

	if _, err := s.parseJWT(signed); err == nil {
		t.Fatal("parseJWT(expired): expected error, got nil")
	}
}

func TestGenerateJWT_ExpiresIn30Days(t *testing.T) {
	s := newTestServer(t)
	userID := uuid.New()

	before := time.Now()
	tok, err := s.generateJWT(userID)
	if err != nil {
		t.Fatalf("generateJWT: %v", err)
	}
	after := time.Now()

	claims, err := s.parseJWT(tok)
	if err != nil {
		t.Fatalf("parseJWT: %v", err)
	}
	if claims.ExpiresAt == nil {
		t.Fatal("claims.ExpiresAt is nil")
	}

	// jwtTTL is 30 days. Allow ±60s slack: the earliest possible expiry is
	// before+TTL, the latest is after+TTL. We check the real expiry sits in
	// that window, expanded by 60s on each side.
	const slack = 60 * time.Second
	minExp := before.Add(jwtTTL).Add(-slack)
	maxExp := after.Add(jwtTTL).Add(slack)
	got := claims.ExpiresAt.Time

	if got.Before(minExp) || got.After(maxExp) {
		t.Fatalf("ExpiresAt = %v, want in [%v, %v]", got, minExp, maxExp)
	}

	// Also sanity-check jwtTTL is exactly 30 days so a future change to the
	// constant fails this test loudly.
	if jwtTTL != 30*24*time.Hour {
		t.Fatalf("jwtTTL = %v, want 30d", jwtTTL)
	}
}

func TestUser_JSONShape(t *testing.T) {
	subID := "sub_abc123"
	status := "active"
	periodEnd := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)

	u := User{
		ID:                     uuid.New(),
		GitHubID:               42,
		Email:                  "test@example.com",
		PlanTier:               "paid",
		PlanPeriod:             "annual",
		RazorpaySubscriptionID: &subID,
		SubscriptionStatus:     &status,
		CurrentPeriodEnd:       &periodEnd,
		CreatedAt:              time.Now(),
	}

	b, err := json.Marshal(u)
	if err != nil {
		t.Fatalf("json.Marshal(User): %v", err)
	}
	raw := string(b)

	for _, field := range []string{
		`"plan_tier":"paid"`,
		`"plan_period":"annual"`,
		`"razorpay_subscription_id":"sub_abc123"`,
		`"subscription_status":"active"`,
		`"current_period_end":`,
	} {
		if !strings.Contains(raw, field) {
			t.Errorf("marshaled User missing %q; got %s", field, raw)
		}
	}

	// Also decode back into a map so we confirm the keys exist with the
	// correct wire names regardless of value formatting.
	var asMap map[string]any
	if err := json.Unmarshal(b, &asMap); err != nil {
		t.Fatalf("unmarshal to map: %v", err)
	}
	for _, key := range []string{
		"plan_tier",
		"plan_period",
		"razorpay_subscription_id",
		"subscription_status",
		"current_period_end",
	} {
		if _, ok := asMap[key]; !ok {
			t.Errorf("marshaled User missing key %q", key)
		}
	}
}

func TestUser_SubscriptionFieldsOmitEmpty(t *testing.T) {
	// Nil pointer fields with `omitempty` should NOT appear in the JSON.
	u := User{
		ID:         uuid.New(),
		GitHubID:   7,
		Email:      "nil@example.com",
		PlanTier:   "free",
		PlanPeriod: "",
		CreatedAt:  time.Now(),
	}
	b, err := json.Marshal(u)
	if err != nil {
		t.Fatalf("json.Marshal(User): %v", err)
	}
	raw := string(b)

	for _, key := range []string{
		`"razorpay_subscription_id"`,
		`"subscription_status"`,
		`"current_period_end"`,
	} {
		if strings.Contains(raw, key) {
			t.Errorf("expected %s to be omitted when nil, but JSON contains it: %s", key, raw)
		}
	}
}
