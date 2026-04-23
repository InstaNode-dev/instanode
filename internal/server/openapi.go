package server

import (
	_ "embed"
	"net/http"
)

// openapiJSON is the hand-written OpenAPI 3.1 spec served at GET /openapi.json.
// It describes every public endpoint on this API for AI agents, MCP registries,
// and doc generators. Edit openapi.json in the repo root — it's embedded at
// build time, no code regeneration needed.
//
//go:embed openapi.json
var openapiJSON []byte

// handleOpenAPI serves the embedded OpenAPI 3.1 document.
func (s *server) handleOpenAPI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// Cache for an hour at the edge — spec is embedded, changes only on deploy.
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.WriteHeader(http.StatusOK)
	w.Write(openapiJSON)
}
