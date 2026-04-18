package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

type Resource struct {
	ID         uuid.UUID `json:"id"`
	Token      uuid.UUID `json:"token"`
	Type       string    `json:"type"`
	Name       string    `json:"name"`
	Tier       string    `json:"tier"`
	Status     string    `json:"status"`
	CreatedAt  time.Time `json:"created_at"`
	ExpiresAt  *time.Time `json:"expires_at"`
}

func (s *server) handleGetResources(w http.ResponseWriter, r *http.Request) {
	user := s.authUser(r)
	if user == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Sign in required.")
		return
	}

	rows, err := s.db.Query(`
		SELECT id, token, resource_type, name, tier, status, created_at, expires_at
		FROM resources
		WHERE migrated_to_user_id = $1 OR (token IN (SELECT token FROM resources WHERE migrated_to_user_id = $1))
		ORDER BY created_at DESC`, user.ID)
	if err != nil {
		slog.ErrorContext(r.Context(), "resources: query failed", "error", err, "user_id", user.ID)
		writeError(w, http.StatusInternalServerError, "internal_error", "Could not load your resources — please retry.")
		return
	}
	defer rows.Close()

	var resources []Resource
	for rows.Next() {
		var r Resource
		err := rows.Scan(&r.ID, &r.Token, &r.Type, &r.Name, &r.Tier, &r.Status, &r.CreatedAt, &r.ExpiresAt)
		if err != nil {
			continue
		}
		resources = append(resources, r)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resources)
}

// ── GET /api/me/token ───────────────────────────────────────────────────────
//
// Returns a freshly-signed JWT the user can paste into `Authorization: Bearer …`
// for CLI / agent calls against /db/new, /webhook/new, and /api/me/claim.
// The token is the same shape as the session cookie (30-day TTL), so
// rotating JWT_SECRET revokes every outstanding key.
func (s *server) handleGetAPIToken(w http.ResponseWriter, r *http.Request) {
	user := s.authUser(r)
	if user == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Sign in required.")
		return
	}
	tok, err := s.generateJWT(user.ID)
	if err != nil {
		slog.ErrorContext(r.Context(), "token: generate failed", "error", err, "user_id", user.ID)
		writeError(w, http.StatusInternalServerError, "internal_error", "Could not mint a token — please retry.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"token":      tok,
		"expires_in": int(jwtTTL.Seconds()),
	})
}

// ── POST /api/me/claim ──────────────────────────────────────────────────────
//
// Attach an anonymous resource to the authenticated user's account. Accepts a
// session cookie OR an Authorization: Bearer <JWT> header so CLI/agents can
// claim via the API. Body: {"token":"<uuid>"}.
//
// Idempotent — if the resource is already migrated to this user, returns 200
// with ok:true and the resource payload. If it belongs to a different user or
// doesn't exist, returns 404. Paid users get tier='paid' and expires_at=NULL;
// free-tier users get their existing anon resource reassigned without changing
// the tier.

type claimRequest struct {
	Token string `json:"token"`
}

func (s *server) handleClaimToken(w http.ResponseWriter, r *http.Request) {
	user := s.authUser(r)
	if user == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Sign in required.")
		return
	}

	var req claimRequest
	if derr := json.NewDecoder(r.Body).Decode(&req); derr != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "Request body must be JSON with a 'token' field.")
		return
	}
	tokenUUID, perr := uuid.Parse(strings.TrimSpace(req.Token))
	if perr != nil {
		writeError(w, http.StatusBadRequest, "invalid_token", "token must be a UUID.")
		return
	}

	// Look up the resource. If owned by this user already, return 200.
	// If owned by someone else, return 404 (don't leak existence).
	var (
		id            uuid.UUID
		ownerID       *uuid.UUID
		resourceType  string
		name          string
		status        string
		tier          string
		connectionURL string
	)
	if err := s.db.QueryRow(
		`SELECT id, migrated_to_user_id, resource_type, name, status, tier, connection_url
		 FROM resources WHERE token = $1`, tokenUUID,
	).Scan(&id, &ownerID, &resourceType, &name, &status, &tier, &connectionURL); err != nil {
		writeError(w, http.StatusNotFound, "not_found", "No resource with that token.")
		return
	}
	if status != "active" {
		writeError(w, http.StatusGone, "resource_expired", "This resource has expired — provision a new one.")
		return
	}
	if ownerID != nil && *ownerID != user.ID {
		writeError(w, http.StatusNotFound, "not_found", "No resource with that token.")
		return
	}

	// Paid users get permanence; free users just attach the resource without
	// changing the tier (they can upgrade later and call /api/me/claim again
	// or hit /billing/migrate, both of which UPDATE the same row).
	newTier := tier
	clearExpiry := false
	if user.PlanTier == "paid" {
		newTier = "paid"
		clearExpiry = true
	}

	var qerr error
	if clearExpiry {
		_, qerr = s.db.Exec(
			`UPDATE resources SET migrated_to_user_id = $1, tier = $2, expires_at = NULL
			 WHERE token = $3 AND status = 'active'`,
			user.ID, newTier, tokenUUID,
		)
	} else {
		_, qerr = s.db.Exec(
			`UPDATE resources SET migrated_to_user_id = $1
			 WHERE token = $2 AND status = 'active'`,
			user.ID, tokenUUID,
		)
	}
	if qerr != nil {
		slog.ErrorContext(r.Context(), "claim: update failed", "error", qerr, "user_id", user.ID, "token", tokenUUID)
		writeError(w, http.StatusInternalServerError, "internal_error", "Could not claim the token — please retry.")
		return
	}

	slog.InfoContext(r.Context(), "claim.success", "user_id", user.ID, "token", tokenUUID.String(), "tier", newTier)

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":            true,
		"id":            id.String(),
		"token":         tokenUUID.String(),
		"resource_type": resourceType,
		"name":          name,
		"tier":          newTier,
		"status":        "active",
	})
}