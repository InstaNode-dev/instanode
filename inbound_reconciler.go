package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/lib/pq"
)

// Inbound reconciliation.
//
// When Brevo's inbound webhook delivery fails (e.g. a transient outage on our
// side, or their 23s timeout expires), the row is permanently missing from
// inbound_messages — Brevo has no retry-delivery API. The reconciler closes
// that gap by periodically listing Brevo's own inbound event log and
// backfilling any MessageId we don't already have.
//
// Limitation: Brevo's /v3/inbound/events API only exposes metadata (from,
// to, subject, messageId, received_at). Body content (Text/HTML) is ONLY
// delivered via webhook payload. A reconciled row therefore carries empty
// body_text + body_html; the admin UI should indicate "body not available —
// check Brevo dashboard" for those. This is strictly better than silent drops.

type brevoInboundEvent struct {
	UUID      string          `json:"uuid"`
	MessageID string          `json:"messageId"`
	Sender    string          `json:"sender"`
	Recipient string          `json:"recipient"`
	Subject   string          `json:"subject"`
	Date      string          `json:"date"`
	Logs      []brevoEventLog `json:"logs"`
}

type brevoEventLog struct {
	Date string `json:"date"`
	Type string `json:"type"`
}

type brevoInboundListResponse struct {
	Events []brevoInboundEvent `json:"events"`
}

// parsedReconcileInterval returns the reconciler tick period. Defaults to
// 10m when the config string is empty or unparseable.
func parsedReconcileInterval(raw string) time.Duration {
	if raw == "" {
		return 10 * time.Minute
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < time.Minute {
		slog.Warn("reconciler: invalid interval, defaulting to 10m", "raw", raw, "error", err)
		return 10 * time.Minute
	}
	return d
}

// startInboundReconciler boots the goroutine. No-op when BrevoAPIKey is empty.
func startInboundReconciler(db *sql.DB, cfg *Config) {
	if cfg.Email.BrevoAPIKey == "" {
		slog.Info("reconciler: BREVO_API_KEY not set, skipping")
		return
	}
	interval := parsedReconcileInterval(cfg.Email.ReconcileInterval)
	slog.Info("reconciler: starting", "interval", interval)

	go func() {
		// Kick one cycle shortly after boot so we don't wait the full interval
		// on the first run.
		time.Sleep(30 * time.Second)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			n, err := reconcileInboundOnce(ctx, db, cfg)
			cancel()
			if err != nil {
				slog.Warn("reconciler: tick failed", "error", err)
			} else if n > 0 {
				slog.Info("reconciler: backfilled rows", "count", n)
			}
			<-ticker.C
		}
	}()
}

// reconcileInboundOnce runs one reconciliation pass. Returns the number of
// newly-inserted rows. Errors are returned only for transport-level failures;
// individual event insert failures are logged and counted-as-skipped.
func reconcileInboundOnce(ctx context.Context, db *sql.DB, cfg *Config) (int, error) {
	now := time.Now().UTC()
	startDate := now.AddDate(0, 0, -2).Format("2006-01-02") // last 48h, Brevo expects YYYY-MM-DD
	endDate := now.Format("2006-01-02")

	events, err := fetchBrevoInboundEvents(ctx, cfg.Email.BrevoAPIKey, startDate, endDate, 100)
	if err != nil {
		return 0, err
	}
	if len(events) == 0 {
		return 0, nil
	}

	// The LIST endpoint only returns uuid/sender/recipient/date — NO logs,
	// messageId, or subject. We can't filter for terminal-state here. Instead
	// we reconcile EVERY event; the messageId-based second dedup below keeps
	// us from double-inserting rows the webhook already delivered.
	//
	// Pre-dedup by uuid (dedupkey = 'brevo-uuid:' + uuid) so a stable re-run
	// doesn't hit the detail endpoint N times per tick.
	uuids := make([]string, 0, len(events))
	for _, e := range events {
		if e.UUID != "" {
			uuids = append(uuids, "brevo-uuid:"+e.UUID)
		}
	}
	existingUUIDs, err := existingProviderIDs(ctx, db, uuids)
	if err != nil {
		return 0, fmt.Errorf("query existing uuids: %w", err)
	}

	inserted := 0
	for _, e := range events {
		if e.UUID == "" {
			continue
		}
		uuidKey := "brevo-uuid:" + e.UUID
		if _, ok := existingUUIDs[uuidKey]; ok {
			continue
		}
		// Fetch detail for messageId + subject.
		detail, err := fetchBrevoInboundEventDetail(ctx, cfg.Email.BrevoAPIKey, e.UUID)
		if err != nil {
			slog.Warn("reconciler: detail fetch failed", "error", err, "uuid", e.UUID)
			continue
		}
		// Second dedup: if the webhook DID land (concurrent with our tick),
		// inbound_messages already has a row keyed on messageId. Don't
		// double-insert with a uuid key.
		if detail.MessageID != "" {
			existingMID, err := existingProviderIDs(ctx, db, []string{detail.MessageID})
			if err == nil {
				if _, ok := existingMID[detail.MessageID]; ok {
					// Webhook won the race; skip.
					continue
				}
			}
		}
		// Provider id: prefer Brevo's MessageId (matches webhook path).
		// Fall back to "brevo-uuid:<uuid>" so re-reconciles stay idempotent
		// even when MessageId is missing.
		providerID := detail.MessageID
		if providerID == "" {
			providerID = uuidKey
		}
		if err := insertReconciledDetail(ctx, db, providerID, e, detail); err != nil {
			slog.Warn("reconciler: insert failed", "error", err, "provider_id", providerID)
			continue
		}
		inserted++
	}
	return inserted, nil
}

// brevoInboundEventDetail is what /v3/inbound/events/{uuid} returns. The shape
// is richer than the list: messageId, subject, and full log trail.
type brevoInboundEventDetail struct {
	MessageID string `json:"messageId"`
	Sender    string `json:"sender"`
	Recipient string `json:"recipient"`
	Subject   string `json:"subject"`
	Date      string `json:"receivedAt"`
}

func fetchBrevoInboundEventDetail(ctx context.Context, apiKey, uuid string) (*brevoInboundEventDetail, error) {
	url := "https://api.brevo.com/v3/inbound/events/" + uuid
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	req.Header.Set("api-key", apiKey)
	req.Header.Set("Accept", "application/json")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return nil, fmt.Errorf("brevo detail http %d: %s", resp.StatusCode, string(body))
	}
	var out brevoInboundEventDetail
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

// eventIsTerminal returns true when Brevo is done with this event — either
// it has been successfully delivered (and we should still backfill if the
// webhook landed before our deploy or the row got deleted), or it failed
// definitively. Events still in `received` / `processed` state without a
// terminal log are skipped so we don't race the webhook.
func eventIsTerminal(e brevoInboundEvent) bool {
	if len(e.Logs) == 0 {
		return false
	}
	switch e.Logs[len(e.Logs)-1].Type {
	case "delivered", "webhookFailed", "failed", "rejected":
		return true
	}
	return false
}

func fetchBrevoInboundEvents(ctx context.Context, apiKey, startDate, endDate string, limit int) ([]brevoInboundEvent, error) {
	url := fmt.Sprintf("https://api.brevo.com/v3/inbound/events?startDate=%s&endDate=%s&limit=%d",
		startDate, endDate, limit)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	req.Header.Set("api-key", apiKey)
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("brevo list: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("brevo list http %d: %s", resp.StatusCode, string(body))
	}
	var out brevoInboundListResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("brevo list decode: %w", err)
	}
	return out.Events, nil
}

// existingProviderIDs returns a set of provider_ids in the given slice that
// are already present in inbound_messages. Uses a single ANY($1) query so we
// don't run one SELECT per event.
func existingProviderIDs(ctx context.Context, db *sql.DB, ids []string) (map[string]struct{}, error) {
	out := make(map[string]struct{}, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := db.QueryContext(ctx, "SELECT provider_id FROM inbound_messages WHERE provider_id = ANY($1)", pq.Array(ids))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = struct{}{}
	}
	return out, rows.Err()
}

// insertReconciledDetail writes a metadata-only row using detail-endpoint
// fields. body_text / body_html / raw_headers stay empty because Brevo's
// detail API doesn't expose them either. Idempotent via ON CONFLICT DO NOTHING.
func insertReconciledDetail(ctx context.Context, db *sql.DB, providerID string, listEv brevoInboundEvent, detail *brevoInboundEventDetail) error {
	// Prefer the detail timestamp (receivedAt), fall back to list date.
	receivedAt := time.Now().UTC()
	for _, s := range []string{detail.Date, listEv.Date} {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			receivedAt = t.UTC()
			break
		}
	}
	sender := detail.Sender
	if sender == "" {
		sender = listEv.Sender
	}
	recipient := detail.Recipient
	if recipient == "" {
		recipient = listEv.Recipient
	}
	_, err := db.ExecContext(ctx, `
		INSERT INTO inbound_messages
		    (provider_id, from_email, from_name, to_email, subject, body_text, body_html, spam_score, raw_headers, received_at)
		VALUES ($1, $2, '', $3, $4, '', '', NULL, NULL, $5)
		ON CONFLICT (provider_id) DO NOTHING`,
		providerID, sender, recipient, detail.Subject, receivedAt,
	)
	return err
}
