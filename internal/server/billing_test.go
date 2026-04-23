package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"testing"
	"time"

	"github.com/google/uuid"
)

// ── computeSignature ────────────────────────────────────────────────────────
//
// The Razorpay webhook handler at billing.go:144-148 verifies signatures with
// hmac.Equal against the output of s.computeSignature. The tests below lock
// in both the exact output shape (64-char lowercase hex) and the canonical
// HMAC-SHA256 construction Razorpay mandates: hex(HMAC-SHA256(key=secret, msg=body)).

// TestComputeSignature_KnownVector pins the function against RFC 4231 test
// case 2 — key="Jefe", data="what do ya want for nothing?". This is the
// canonical HMAC-SHA256 vector; if this ever drifts the crypto is broken.
func TestComputeSignature_KnownVector(t *testing.T) {
	s := &server{}
	got := s.computeSignature("what do ya want for nothing?", "Jefe")
	want := "5bdcc146bf60754e6a042426089575c75a003f089d2739839dec58b964ec3843"
	if got != want {
		t.Fatalf("computeSignature RFC4231 vector mismatch:\n got  %q\n want %q", got, want)
	}
}

// TestComputeSignature_Shape asserts the output is always 64 lowercase hex
// characters. Razorpay's verification will fail on any deviation (uppercase,
// base64, etc.) so this is load-bearing.
func TestComputeSignature_Shape(t *testing.T) {
	s := &server{}
	hexRe := regexp.MustCompile(`^[0-9a-f]{64}$`)

	cases := []struct{ payload, secret string }{
		{"", ""},
		{"hello", "world"},
		{`{"event":"payment.captured","payload":{"payment":{"entity":{"id":"pay_abc"}}}}`, "whsec_test"},
		{"unicode-☃-and-nulls-\x00\x01", "s3cret"},
	}
	for _, tc := range cases {
		got := s.computeSignature(tc.payload, tc.secret)
		if !hexRe.MatchString(got) {
			t.Errorf("computeSignature(%q,%q) = %q — not 64 lowercase hex", tc.payload, tc.secret, got)
		}
	}
}

// TestComputeSignature_DifferentPayloads confirms the signature is payload-
// sensitive with a fixed secret. A webhook replayed with a tampered body
// must produce a different signature.
func TestComputeSignature_DifferentPayloads(t *testing.T) {
	s := &server{}
	secret := "shared-secret"
	a := s.computeSignature(`{"event":"payment.captured"}`, secret)
	b := s.computeSignature(`{"event":"payment.failed"}`, secret)
	if a == b {
		t.Fatalf("different payloads produced same signature: %q", a)
	}
}

// TestComputeSignature_DifferentSecrets confirms the signature is secret-
// sensitive with a fixed payload. Rotating the webhook secret must change
// every signature.
func TestComputeSignature_DifferentSecrets(t *testing.T) {
	s := &server{}
	payload := `{"event":"payment.captured"}`
	a := s.computeSignature(payload, "secret-one")
	b := s.computeSignature(payload, "secret-two")
	if a == b {
		t.Fatalf("different secrets produced same signature: %q", a)
	}
}

// TestComputeSignature_MatchesRazorpayVerifyPath re-derives the HMAC via the
// same stdlib primitives the handler uses for hmac.Equal comparison. If the
// two ever diverge, live webhooks will 401 even with a correct signature.
func TestComputeSignature_MatchesRazorpayVerifyPath(t *testing.T) {
	s := &server{}
	payload := `{"event":"subscription.charged","id":"evt_123"}`
	secret := "whsec_example"

	got := s.computeSignature(payload, secret)

	h := hmac.New(sha256.New, []byte(secret))
	h.Write([]byte(payload))
	want := hex.EncodeToString(h.Sum(nil))

	if got != want {
		t.Fatalf("computeSignature diverges from hex(HMAC-SHA256(...)):\n got  %q\n want %q", got, want)
	}
	// And the actual constant-time compare the handler performs:
	if !hmac.Equal([]byte(got), []byte(want)) {
		t.Fatalf("hmac.Equal returned false for matching signatures")
	}
}

// ── userIDFromNotes ─────────────────────────────────────────────────────────

func TestUserIDFromNotes_Valid(t *testing.T) {
	want := uuid.New()
	notes := map[string]interface{}{"user_id": want.String()}
	got, ok := userIDFromNotes(notes)
	if !ok {
		t.Fatalf("expected ok=true for valid UUID string")
	}
	if got != want {
		t.Fatalf("parsed UUID mismatch: got %v want %v", got, want)
	}
}

func TestUserIDFromNotes_Missing(t *testing.T) {
	got, ok := userIDFromNotes(map[string]interface{}{})
	if ok {
		t.Fatalf("expected ok=false for missing user_id, got %v", got)
	}
	if got != uuid.Nil {
		t.Fatalf("expected uuid.Nil for missing user_id, got %v", got)
	}
}

func TestUserIDFromNotes_Empty(t *testing.T) {
	got, ok := userIDFromNotes(map[string]interface{}{"user_id": ""})
	if ok || got != uuid.Nil {
		t.Fatalf("expected (uuid.Nil,false) for empty user_id, got (%v,%v)", got, ok)
	}
}

func TestUserIDFromNotes_Malformed(t *testing.T) {
	cases := []map[string]interface{}{
		{"user_id": "not-a-uuid"},
		{"user_id": "12345"},
		{"user_id": "xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx"},
		{"user_id": 12345},      // wrong type — int, not string
		{"user_id": nil},        // wrong type — nil
		{"user_id": []string{}}, // wrong type — slice
	}
	for i, notes := range cases {
		got, ok := userIDFromNotes(notes)
		if ok || got != uuid.Nil {
			t.Errorf("case %d (%v): expected (uuid.Nil,false), got (%v,%v)", i, notes, got, ok)
		}
	}
}

// ── unixToTime ──────────────────────────────────────────────────────────────

// TestUnixToTime_Float64 covers the Razorpay JSON-decoded path: encoding/json
// always unmarshals numbers into float64 unless UseNumber is set, so the
// float64 arm is what production hits. 1713446400 = 2024-04-18 13:20:00 UTC.
func TestUnixToTime_Float64(t *testing.T) {
	got := unixToTime(float64(1713446400))
	want := time.Date(2024, time.April, 18, 13, 20, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("unixToTime(1713446400.0) = %v, want %v", got, want)
	}
	if got.Location() != time.UTC {
		t.Fatalf("unixToTime should return UTC, got location %v", got.Location())
	}
}

func TestUnixToTime_Int64(t *testing.T) {
	got := unixToTime(int64(1713446400))
	want := time.Date(2024, time.April, 18, 13, 20, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("unixToTime(int64 1713446400) = %v, want %v", got, want)
	}
}

func TestUnixToTime_Zero(t *testing.T) {
	// A zero epoch is still a valid time.Time — confirm it round-trips.
	got := unixToTime(float64(0))
	want := time.Unix(0, 0).UTC()
	if !got.Equal(want) {
		t.Fatalf("unixToTime(0.0) = %v, want %v", got, want)
	}
}

func TestUnixToTime_Nil(t *testing.T) {
	got := unixToTime(nil)
	if !got.IsZero() {
		t.Fatalf("unixToTime(nil) expected zero time, got %v", got)
	}
}

func TestUnixToTime_UnsupportedTypes(t *testing.T) {
	cases := []interface{}{
		"1713446400",       // string — not accepted
		int(1713446400),    // plain int — not accepted (only int64)
		int32(1713446400),  // int32 — not accepted
		map[string]int{},   // bogus type
		[]byte("whatever"), // bogus type
	}
	for i, v := range cases {
		got := unixToTime(v)
		if !got.IsZero() {
			t.Errorf("case %d (%T %v): expected zero time, got %v", i, v, v, got)
		}
	}
}

// ── periodFromSubscription ──────────────────────────────────────────────────

func TestPeriodFromSubscription_Annual(t *testing.T) {
	sub := map[string]interface{}{
		"notes": map[string]interface{}{"plan": "annual"},
	}
	if got := periodFromSubscription(sub); got != "annual" {
		t.Fatalf("expected 'annual', got %q", got)
	}
}

func TestPeriodFromSubscription_Monthly(t *testing.T) {
	sub := map[string]interface{}{
		"notes": map[string]interface{}{"plan": "monthly"},
	}
	if got := periodFromSubscription(sub); got != "monthly" {
		t.Fatalf("expected 'monthly', got %q", got)
	}
}

// TestPeriodFromSubscription_DefaultsToMonthly locks in the fallback: unless
// notes.plan is the literal string "annual", the function returns "monthly".
// This matters because the DB column plan_period is NOT NULL.
func TestPeriodFromSubscription_DefaultsToMonthly(t *testing.T) {
	cases := []struct {
		name string
		sub  map[string]interface{}
	}{
		{"nil notes", map[string]interface{}{}},
		{"notes missing plan", map[string]interface{}{"notes": map[string]interface{}{}}},
		{"plan empty", map[string]interface{}{"notes": map[string]interface{}{"plan": ""}}},
		{"plan wrong type", map[string]interface{}{"notes": map[string]interface{}{"plan": 42}}},
		{"plan unknown value", map[string]interface{}{"notes": map[string]interface{}{"plan": "quarterly"}}},
		{"plan capitalised", map[string]interface{}{"notes": map[string]interface{}{"plan": "Annual"}}},
		{"notes wrong type", map[string]interface{}{"notes": "not-a-map"}},
	}
	for _, tc := range cases {
		if got := periodFromSubscription(tc.sub); got != "monthly" {
			t.Errorf("%s: expected 'monthly', got %q", tc.name, got)
		}
	}
}

// ── planPricing ─────────────────────────────────────────────────────────────

// TestPlanPricing_USDPositiveInts is a sanity check against price typos.
// Every plan must have a USD entry with a positive minor-unit value.
func TestPlanPricing_USDPositiveInts(t *testing.T) {
	if len(planPricing) == 0 {
		t.Fatal("planPricing is empty — billing is effectively disabled")
	}
	for plan, currencies := range planPricing {
		usd, ok := currencies["USD"]
		if !ok {
			t.Errorf("plan %q missing USD price", plan)
			continue
		}
		if usd <= 0 {
			t.Errorf("plan %q USD price must be positive, got %d", plan, usd)
		}
	}
}

// TestPlanPricing_AllCurrenciesPositive guards against a zero slipping in
// for any currency on any plan — a zero-amount order would be rejected by
// Razorpay anyway, but better to catch it in tests.
func TestPlanPricing_AllCurrenciesPositive(t *testing.T) {
	for plan, currencies := range planPricing {
		if len(currencies) == 0 {
			t.Errorf("plan %q has no currencies defined", plan)
			continue
		}
		for cur, amount := range currencies {
			if amount <= 0 {
				t.Errorf("plan %q currency %q must be positive, got %d", plan, cur, amount)
			}
		}
	}
}

// TestPlanPricing_DeveloperAnnualIsTenXMonthly pins the "two months free"
// promise: annual must be exactly 10x monthly across every shared currency.
// A typo here would silently overcharge or undercharge annual subscribers.
func TestPlanPricing_DeveloperAnnualIsTenXMonthly(t *testing.T) {
	monthly, ok := planPricing["developer"]
	if !ok {
		t.Fatal("planPricing missing 'developer' plan")
	}
	annual, ok := planPricing["developer-annual"]
	if !ok {
		t.Fatal("planPricing missing 'developer-annual' plan")
	}
	for cur, mAmount := range monthly {
		aAmount, ok := annual[cur]
		if !ok {
			t.Errorf("developer-annual missing currency %q", cur)
			continue
		}
		if aAmount != mAmount*10 {
			t.Errorf("developer-annual[%s] = %d, want %d (10x monthly %d)", cur, aAmount, mAmount*10, mAmount)
		}
	}
	// And vice versa — no currency unique to annual.
	for cur := range annual {
		if _, ok := monthly[cur]; !ok {
			t.Errorf("developer-annual has currency %q not present in developer", cur)
		}
	}
}
