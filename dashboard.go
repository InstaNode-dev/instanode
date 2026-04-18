package main

import (
	"encoding/json"
	"net/http"
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
	user, err := s.getUserFromRequest(r)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	rows, err := s.db.Query(`
		SELECT id, token, resource_type, name, tier, status, created_at, expires_at
		FROM resources
		WHERE migrated_to_user_id = $1 OR (token IN (SELECT token FROM resources WHERE migrated_to_user_id = $1))
		ORDER BY created_at DESC`, user.ID)
	if err != nil {
		http.Error(w, "Failed to query resources", http.StatusInternalServerError)
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