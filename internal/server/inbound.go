package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Inbound email receiving via Brevo's Inbound Parsing webhook.
//
// Wire protocol (as of April 2026): Brevo receives mail at the MX it publishes
// (in-smtp.brevo.com), parses the message, then POSTs a JSON envelope
// { "items": [ { ...one message... }, ... ] } to whatever webhook URL is
// configured in the Brevo dashboard. Each `item` typically contains:
//
//	Uuid, MessageId, InReplyTo, From {Address,Name},
//	To [{Address,Name}], Cc, ReplyTo, Subject,
//	RawTextBody / Text, RawHtmlBody / Html,
//	SpamScore, Headers (object), Attachments [...]
//
// We insert one row per item into the inbound_messages table, keyed by
// provider_id (MessageId → Uuid → content hash) so Brevo retries don't
// duplicate. Partial failures are logged; we still return 200 so Brevo
// doesn't back off aggressively.

// maxInboundBodyBytes caps the POST body. Brevo inbound payloads can be
// surprisingly large (HTML bodies + inlined attachments in base64), but 10 MB
// is well above any realistic inbound email we'd expect.
const maxInboundBodyBytes = 10 * 1024 * 1024

// inboundMessage is the flattened, DB-ready representation of one parsed
// item. All fields are already trimmed/normalised; headers marshal directly
// into the JSONB column.
type inboundMessage struct {
	ProviderID string          // Brevo MessageId (or fallback)
	FromEmail  string
	FromName   string
	ToEmail    string
	Subject    string
	BodyText   string
	BodyHTML   string
	SpamScore  *float64        // nil when Brevo didn't supply one
	RawHeaders json.RawMessage // preserved verbatim for debugging
}

// brevoItem mirrors the one-item shape. Field names are capitalised because
// Brevo sends them that way (Pascal-case), not standard Go JSON lower-case.
// We tolerate both spellings where they differ in the wild (Text vs RawTextBody).
type brevoItem struct {
	Uuid        string         `json:"Uuid"`
	MessageId   string         `json:"MessageId"`
	From        brevoAddress   `json:"From"`
	To          []brevoAddress `json:"To"`
	Subject     string         `json:"Subject"`
	RawTextBody string         `json:"RawTextBody"`
	Text        string         `json:"Text"`
	RawHtmlBody string         `json:"RawHtmlBody"`
	Html        string         `json:"Html"`
	SpamScore   *float64       `json:"SpamScore"`
	// Headers may arrive as an object {"X-Foo":"bar"} or an array
	// [{"Name":"X-Foo","Value":"bar"}]. We don't try to normalise — we store
	// whatever JSON we got.
	Headers json.RawMessage `json:"Headers"`
}

type brevoAddress struct {
	Address string `json:"Address"`
	Name    string `json:"Name"`
}

type brevoEnvelope struct {
	Items []brevoItem `json:"items"`
}

// parseBrevoPayload turns a raw Brevo POST body into a slice of DB-ready rows.
// Extracted as a pure function so tests don't need a DB. Returns an error only
// when the envelope itself is malformed; individual items with missing fields
// still yield a row (we log downstream instead of failing the batch).
func parseBrevoPayload(body []byte) ([]inboundMessage, error) {
	if len(body) == 0 {
		return nil, errors.New("empty body")
	}
	var env brevoEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("invalid json: %w", err)
	}
	out := make([]inboundMessage, 0, len(env.Items))
	for _, it := range env.Items {
		out = append(out, flattenItem(it))
	}
	return out, nil
}

func flattenItem(it brevoItem) inboundMessage {
	msg := inboundMessage{
		FromEmail: strings.TrimSpace(it.From.Address),
		FromName:  strings.TrimSpace(it.From.Name),
		Subject:   it.Subject,
		SpamScore: it.SpamScore,
		RawHeaders: it.Headers,
	}

	// Pick first recipient; fall back to empty — DB NOT NULL constraint is
	// defaulted to '' so this won't blow up, and the admin UI surfaces the
	// weirdness on read.
	if len(it.To) > 0 {
		msg.ToEmail = strings.TrimSpace(it.To[0].Address)
	}

	// Prefer the "Raw" variants (which include the unparsed MIME text) over
	// Text/Html — Brevo doesn't always populate both.
	if it.RawTextBody != "" {
		msg.BodyText = it.RawTextBody
	} else {
		msg.BodyText = it.Text
	}
	if it.RawHtmlBody != "" {
		msg.BodyHTML = it.RawHtmlBody
	} else {
		msg.BodyHTML = it.Html
	}

	// provider_id fallback chain: MessageId → Uuid → sha256(from|to|subject|body).
	// Brevo has historically provided MessageId for every item, but the docs
	// don't guarantee it, so we fall all the way back to a content hash to keep
	// ON CONFLICT (provider_id) DO NOTHING honest.
	switch {
	case it.MessageId != "":
		msg.ProviderID = it.MessageId
	case it.Uuid != "":
		msg.ProviderID = it.Uuid
	default:
		h := sha256.Sum256([]byte(msg.FromEmail + "|" + msg.ToEmail + "|" + msg.Subject + "|" + msg.BodyText))
		msg.ProviderID = "sha256:" + hex.EncodeToString(h[:])
	}

	return msg
}

// spamThreshold is the Brevo SpamScore above which we still store the message
// but tag it in logs. Brevo's scale is roughly 0–10 with 5 being "probably spam".
const spamThreshold = 5.0

// isAdmin centralises the gate used by /admin/* endpoints. Extracted so tests
// can exercise the match logic without spinning up a DB.
func isAdmin(user *User, adminEmail string) bool {
	if user == nil || adminEmail == "" {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(user.Email), strings.TrimSpace(adminEmail))
}

// ── POST /webhooks/brevo-inbound ───────────────────────────────────────────

// extractInboundToken pulls the webhook secret from (in order) the URL path
// value, the `?token=` query parameter, or the `X-Brevo-Token` header. Brevo
// has been observed to drop query strings silently on some paid plans; the
// path-based route is what we register in prod, but the other two live here
// as fallbacks so a Brevo UI change doesn't silently break ingest again.
func extractInboundToken(r *http.Request) string {
	if v := r.PathValue("token"); v != "" {
		return v
	}
	if v := r.URL.Query().Get("token"); v != "" {
		return v
	}
	return r.Header.Get("X-Brevo-Token")
}

func (s *server) handleBrevoInbound(w http.ResponseWriter, r *http.Request) {
	// Log every hit BEFORE the token check so a webhookFailed event can be
	// traced back to an actual request. User-Agent, source IP (via forwarded
	// header), and body-size are enough to distinguish Brevo, a rogue scan,
	// or a misconfigured client.
	ua := r.Header.Get("User-Agent")
	srcIP := r.Header.Get("Cf-Connecting-Ip")
	if srcIP == "" {
		srcIP = r.Header.Get("X-Forwarded-For")
	}
	tokenProvided := extractInboundToken(r)
	slog.InfoContext(r.Context(), "inbound: request received",
		"method", r.Method, "path", r.URL.Path, "has_query_token", r.URL.Query().Get("token") != "",
		"has_header_token", r.Header.Get("X-Brevo-Token") != "", "has_path_token", r.PathValue("token") != "",
		"user_agent", ua, "src_ip", srcIP, "content_length", r.ContentLength)

	expected := s.cfg.Email.BrevoInboundSecret
	if expected == "" || subtle.ConstantTimeCompare([]byte(expected), []byte(tokenProvided)) != 1 {
		_ = subtle.ConstantTimeCompare([]byte("placeholder"), []byte(tokenProvided))
		slog.WarnContext(r.Context(), "inbound: token mismatch", "provided_len", len(tokenProvided), "expected_empty", expected == "")
		writeError(w, http.StatusUnauthorized, "unauthorized", "Invalid inbound webhook token.")
		return
	}

	// Cap body — Brevo inbound can be large (HTML + attachments).
	r.Body = http.MaxBytesReader(w, r.Body, maxInboundBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		// MaxBytesReader returns a *MaxBytesError when the cap trips; any
		// other read error (e.g. client aborted) we also treat as 413 for
		// simplicity — it keeps Brevo retrying for transient network blips
		// but surfaces oversize uploads clearly.
		writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large",
			fmt.Sprintf("inbound body exceeds %d bytes", maxInboundBodyBytes))
		return
	}

	msgs, perr := parseBrevoPayload(body)
	if perr != nil {
		slog.WarnContext(r.Context(), "inbound: invalid payload", "error", perr, "bytes", len(body))
		writeError(w, http.StatusBadRequest, "invalid_payload", "Request body must be Brevo inbound JSON with an 'items' array.")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	received := 0
	for _, m := range msgs {
		if m.SpamScore != nil && *m.SpamScore > spamThreshold {
			slog.WarnContext(ctx, "inbound: high spam score", "from", m.FromEmail, "subject", m.Subject, "score", *m.SpamScore)
		}
		if err := s.insertInboundMessage(ctx, m); err != nil {
			// Never return non-200 on partial failure — Brevo would retry
			// the whole batch and we'd double-insert the good ones. Log and
			// continue.
			slog.ErrorContext(ctx, "inbound: insert failed", "error", err, "provider_id", m.ProviderID, "from", m.FromEmail)
			continue
		}
		received++
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "received": received})
}

// insertInboundMessage inserts one parsed message. ON CONFLICT (provider_id)
// DO NOTHING absorbs Brevo retries without duplicating rows. RawHeaders is
// coerced to NULL when absent so the JSONB column doesn't end up holding
// the literal string "null".
func (s *server) insertInboundMessage(ctx context.Context, m inboundMessage) error {
	var headers any
	if len(m.RawHeaders) > 0 && string(m.RawHeaders) != "null" {
		headers = []byte(m.RawHeaders)
	}
	var spam any
	if m.SpamScore != nil {
		spam = *m.SpamScore
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO inbound_messages
		    (provider_id, from_email, from_name, to_email, subject, body_text, body_html, spam_score, raw_headers)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (provider_id) DO NOTHING`,
		m.ProviderID, m.FromEmail, m.FromName, m.ToEmail, m.Subject, m.BodyText, m.BodyHTML, spam, headers,
	)
	return err
}

// ── GET /admin/inbox ───────────────────────────────────────────────────────

// adminInboxRow is the trimmed row shape returned to the admin UI. We
// deliberately skip raw_headers here to keep the list response small; add a
// dedicated GET /admin/inbox/{id} endpoint later if a detail view needs them.
type adminInboxRow struct {
	ID         uuid.UUID  `json:"id"`
	ProviderID *string    `json:"provider_id,omitempty"`
	FromEmail  string     `json:"from_email"`
	FromName   *string    `json:"from_name,omitempty"`
	ToEmail    string     `json:"to_email"`
	Subject    string     `json:"subject"`
	BodyText   string     `json:"body_text"`
	BodyHTML   string     `json:"body_html"`
	SpamScore  *float64   `json:"spam_score,omitempty"`
	ReceivedAt time.Time  `json:"received_at"`
	ReadAt     *time.Time `json:"read_at,omitempty"`
}

func (s *server) handleAdminInboxList(w http.ResponseWriter, r *http.Request) {
	user := s.authUser(r)
	if user == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Sign in required.")
		return
	}
	if !isAdmin(user, s.cfg.Admin.Email) {
		writeError(w, http.StatusForbidden, "forbidden", "Admin only")
		return
	}

	limit := 50
	if v := strings.TrimSpace(r.URL.Query().Get("limit")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 200 {
		limit = 200
	}
	offset := 0
	if v := strings.TrimSpace(r.URL.Query().Get("offset")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			offset = n
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	rows, err := s.db.QueryContext(ctx, `
		SELECT id, provider_id, from_email, from_name, to_email,
		       subject, body_text, body_html, spam_score, received_at, read_at
		FROM inbound_messages
		ORDER BY received_at DESC
		LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		slog.ErrorContext(ctx, "inbox: query failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "Could not load inbox.")
		return
	}
	defer rows.Close()

	out := make([]adminInboxRow, 0)
	for rows.Next() {
		var row adminInboxRow
		if err := rows.Scan(&row.ID, &row.ProviderID, &row.FromEmail, &row.FromName,
			&row.ToEmail, &row.Subject, &row.BodyText, &row.BodyHTML,
			&row.SpamScore, &row.ReceivedAt, &row.ReadAt); err != nil {
			slog.WarnContext(ctx, "inbox: row scan failed", "error", err)
			continue
		}
		out = append(out, row)
	}

	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM inbound_messages`).Scan(&total); err != nil {
		// Fall back to len(out) — better than 500 when the UI only needs a
		// rough count.
		slog.WarnContext(ctx, "inbox: count failed", "error", err)
		total = len(out)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"messages": out,
		"total":    total,
	})
}

// ── POST /admin/inbox/{id}/mark-read ───────────────────────────────────────

func (s *server) handleAdminInboxMarkRead(w http.ResponseWriter, r *http.Request) {
	user := s.authUser(r)
	if user == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Sign in required.")
		return
	}
	if !isAdmin(user, s.cfg.Admin.Email) {
		writeError(w, http.StatusForbidden, "forbidden", "Admin only")
		return
	}

	idStr := r.PathValue("id")
	id, err := uuid.Parse(strings.TrimSpace(idStr))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_id", "id must be a UUID.")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	res, err := s.db.ExecContext(ctx, `
		UPDATE inbound_messages SET read_at = NOW()
		WHERE id = $1 AND read_at IS NULL`, id)
	if err != nil {
		slog.ErrorContext(ctx, "inbox: mark read failed", "error", err, "id", id)
		writeError(w, http.StatusInternalServerError, "internal_error", "Could not mark message read.")
		return
	}
	// res.RowsAffected is advisory only; the response is always ok:true so the
	// caller doesn't have to special-case "already read".
	_ = res

	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
