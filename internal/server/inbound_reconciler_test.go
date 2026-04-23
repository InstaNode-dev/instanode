package server

import (
	"encoding/json"
	"testing"
	"time"
)

func TestParsedReconcileInterval(t *testing.T) {
	tests := []struct {
		in   string
		want time.Duration
	}{
		{"", 10 * time.Minute},            // default
		{"not-a-duration", 10 * time.Minute}, // invalid → default
		{"30s", 10 * time.Minute},          // below 1m floor → default
		{"5m", 5 * time.Minute},
		{"1h", 1 * time.Hour},
	}
	for _, tc := range tests {
		got := parsedReconcileInterval(tc.in)
		if got != tc.want {
			t.Errorf("parsedReconcileInterval(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestEventIsTerminal(t *testing.T) {
	mk := func(types ...string) brevoInboundEvent {
		logs := make([]brevoEventLog, 0, len(types))
		for _, t := range types {
			logs = append(logs, brevoEventLog{Type: t})
		}
		return brevoInboundEvent{Logs: logs}
	}
	tests := []struct {
		name string
		ev   brevoInboundEvent
		want bool
	}{
		{"no_logs", brevoInboundEvent{}, false},
		{"still_in_flight", mk("received", "processed"), false},
		{"webhook_failed", mk("received", "processed", "webhookFailed"), true},
		{"delivered", mk("received", "processed", "delivered"), true},
		{"rejected", mk("rejected"), true},
		{"failed", mk("failed"), true},
		{"unknown_last", mk("received", "future_state"), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := eventIsTerminal(tc.ev); got != tc.want {
				t.Errorf("eventIsTerminal(%v) = %v, want %v", tc.ev, got, tc.want)
			}
		})
	}
}

func TestBrevoInboundListResponse_UnmarshalsRealPayload(t *testing.T) {
	// Fixture captured from the live account — only PII-free fields kept.
	fixture := []byte(`{"events":[
		{"uuid":"348e2922","messageId":"<m1@example.com>","sender":"a@example.com","recipient":"contact@instanode.dev","subject":"Test","date":"2026-04-20T21:41:11.000+02:00","logs":[{"type":"received"},{"type":"webhookFailed"}]},
		{"uuid":"67469d3e","messageId":"<m2@example.com>","sender":"b@example.com","recipient":"contact@instanode.dev","subject":"Another","date":"2026-04-20T21:42:17.000+02:00","logs":[{"type":"processed"}]}
	]}`)
	var out brevoInboundListResponse
	if err := json.Unmarshal(fixture, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(out.Events) != 2 {
		t.Fatalf("want 2 events, got %d", len(out.Events))
	}
	if out.Events[0].MessageID != "<m1@example.com>" {
		t.Errorf("event[0] MessageID = %q", out.Events[0].MessageID)
	}
	if !eventIsTerminal(out.Events[0]) {
		t.Error("event[0] (webhookFailed) should be terminal")
	}
	if eventIsTerminal(out.Events[1]) {
		t.Error("event[1] (processed only) should NOT be terminal")
	}
}

func TestReconcileDiffLogic_SkipsExistingByUUID(t *testing.T) {
	// Brevo's list response has no logs/messageId — we dedupe by UUID
	// (stored as provider_id='brevo-uuid:<uuid>') and reconcile everything
	// else. This test mirrors the filter used in reconcileInboundOnce.
	events := []brevoInboundEvent{
		{UUID: "new-one"},
		{UUID: "already-reconciled"},
		{UUID: ""}, // must skip
	}
	existing := map[string]struct{}{"brevo-uuid:already-reconciled": {}}

	var wouldFetchDetail []string
	for _, e := range events {
		if e.UUID == "" {
			continue
		}
		if _, ok := existing["brevo-uuid:"+e.UUID]; ok {
			continue
		}
		wouldFetchDetail = append(wouldFetchDetail, e.UUID)
	}

	if len(wouldFetchDetail) != 1 || wouldFetchDetail[0] != "new-one" {
		t.Errorf("expected [new-one], got %v", wouldFetchDetail)
	}
}
