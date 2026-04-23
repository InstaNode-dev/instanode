package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Unit tests exercise only the pre-DB validation branches of
// handleClaimPreview so they run without a Postgres dependency. Happy-path
// and row-not-found coverage lives in the E2E suite against a real DB.

func TestHandleClaimPreview_MissingTokenReturns400(t *testing.T) {
	s := &server{}
	req := httptest.NewRequest(http.MethodGet, "/claim/preview", nil)
	rec := httptest.NewRecorder()

	s.handleClaimPreview(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["error"] != "missing_token" {
		t.Errorf("error = %v, want missing_token", body["error"])
	}
}

func TestHandleClaimPreview_WhitespaceTokenReturns400(t *testing.T) {
	// A token like "   " should collapse to empty after TrimSpace and be
	// treated the same as a missing param — not as an invalid UUID.
	s := &server{}
	req := httptest.NewRequest(http.MethodGet, "/claim/preview?token=%20%20%20", nil)
	rec := httptest.NewRecorder()

	s.handleClaimPreview(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["error"] != "missing_token" {
		t.Errorf("error = %v, want missing_token", body["error"])
	}
}

func TestHandleClaimPreview_MalformedTokenReturns400(t *testing.T) {
	s := &server{}
	req := httptest.NewRequest(http.MethodGet, "/claim/preview?token=not-a-uuid", nil)
	rec := httptest.NewRecorder()

	s.handleClaimPreview(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["error"] != "invalid_token" {
		t.Errorf("error = %v, want invalid_token", body["error"])
	}
}
