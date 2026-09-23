package status

import (
	"testing"
	"time"

	"github.com/lepinoid/infra/updater/internal/journal"
)

func TestCompatibilityUnknown(t *testing.T) {
	now := time.Date(2026, 9, 6, 3, 12, 0, 0, time.UTC)
	s := Status{SchemaVersion: 1, ServerInstanceID: "pod", MinecraftVersion: "1.21.1", UpdatedAt: now}
	for _, tc := range []struct {
		version string
		ok      bool
		age     time.Duration
		want    string
	}{{"1.21.1", true, 0, "1.21.1"}, {"1.21.2", true, 0, ""}, {"1.21.1", false, 0, ""}, {"1.21.1", true, 7 * time.Second, "1.21.1"}} {
		s.UpdatedAt = now.Add(-tc.age)
		if got := s.Minecraft(Monitor{Version: tc.version, Valid: tc.ok}, "pod", now); got != tc.want {
			t.Fatal(got)
		}
	}
}

func TestCheckpoint(t *testing.T) {
	for _, tc := range []struct{ input, id, reason string }{{"operationId=550e8400-e29b-41d4-a716-446655440000", "550e8400-e29b-41d4-a716-446655440000", ""}, {"error=busy", "", "checkpoint-busy"}, {"error=unavailable", "", "checkpoint-unavailable"}, {"operationId=no", "", "checkpoint-invalid-response"}} {
		id, reason := Checkpoint(tc.input)
		if id != tc.id || reason != tc.reason {
			t.Fatalf("%s: %s %s", tc.input, id, reason)
		}
	}
}

func TestParseMonitor(t *testing.T) {
	paper := []byte(`{"server_info":{"version":{"name":"Paper 1.21.8","protocol":772}}}`)
	got := ParseMonitor(paper)
	if !got.Valid || got.Version != "1.21.8" {
		t.Fatalf("paper-prefixed: %+v", got)
	}
	legacy := []byte(`{"server_info":{"version":{"name":"1.21.8","protocol":772}}}`)
	if got := ParseMonitor(legacy); !got.Valid || got.Version != "1.21.8" {
		t.Fatalf("legacy: %+v", got)
	}
	invalid := []byte(`{"server_info":{"version":{"name":"unknown","protocol":772}}}`)
	if got := ParseMonitor(invalid); got.Valid {
		t.Fatalf("invalid should not parse: %+v", got)
	}
}

func TestStartupEventStatusRemainsHealthyWithValidIdentity(t *testing.T) {
	now := time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)
	version := "5.8.1"
	base := Status{SchemaVersion: 1, ServerInstanceID: "new", ProcessStartedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour + time.Second), Phase: "HEALTHY", PluginVersion: "bundle", PluginCommitSHA: "commit", MinecraftVersion: "1.21.8", Multiverse: Multiverse{Detected: &version, Expected: version, Compatible: true}}
	want := Startup{PodUID: "new", PreviousPodUID: "old", RequestedAt: now.Add(-time.Hour - time.Minute), Manifest: journal.Manifest{Version: "bundle", PluginCommitSHA: "commit", MultiverseVersion: version, SupportedMinecraft: []string{"1.21.8"}}}
	if !base.Healthy(want, now) {
		t.Fatal("event-driven health incorrectly expired")
	}
	for _, tc := range []struct {
		name   string
		change func(*Status, *Startup)
	}{
		{"zero updated", func(s *Status, _ *Startup) { s.UpdatedAt = time.Time{} }},
		{"zero started", func(s *Status, _ *Startup) { s.ProcessStartedAt = time.Time{} }},
		{"zero requested", func(_ *Status, w *Startup) { w.RequestedAt = time.Time{} }},
		{"updated before process", func(s *Status, _ *Startup) { s.UpdatedAt = s.ProcessStartedAt.Add(-time.Second) }},
		{"future updated", func(s *Status, _ *Startup) { s.UpdatedAt = now.Add(3 * time.Second) }},
		{"future process", func(s *Status, _ *Startup) {
			s.ProcessStartedAt = now.Add(3 * time.Second)
			s.UpdatedAt = s.ProcessStartedAt
		}},
		{"process before request", func(s *Status, w *Startup) { s.ProcessStartedAt = w.RequestedAt.Add(-time.Second) }},
		{"old pod", func(s *Status, w *Startup) { s.ServerInstanceID = w.PreviousPodUID }},
		{"same previous", func(_ *Status, w *Startup) { w.PreviousPodUID = w.PodUID }},
		{"missing previous", func(_ *Status, w *Startup) { w.PreviousPodUID = "" }},
		{"unsupported minecraft", func(s *Status, _ *Startup) { s.MinecraftVersion = "1.21.9" }},
		{"negative sequence", func(s *Status, _ *Startup) { s.StatusSequence = -1 }},
		{"version mismatch", func(s *Status, _ *Startup) { s.PluginVersion = "other" }},
		{"commit mismatch", func(s *Status, _ *Startup) { s.PluginCommitSHA = "other" }},
		{"multiverse mismatch", func(s *Status, _ *Startup) { s.Multiverse.Expected = "other" }},
		{"unhealthy", func(s *Status, _ *Startup) { s.Phase = "UNHEALTHY" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, w := base, want
			tc.change(&s, &w)
			if s.Healthy(w, now) {
				t.Fatal("invalid startup accepted")
			}
		})
	}
}

func TestCheckpointRCONTerminalReset(t *testing.T) {
	const id = "550e8400-e29b-41d4-a716-446655440000"
	for _, tc := range []struct{ name, input, id, reason string }{
		{"accepted", "operationId=" + id + "\n\x1b[0m\n", id, ""},
		{"busy", "error=busy\n\x1b[0m\n", "", "checkpoint-busy"},
		{"unavailable", "error=unavailable\n\x1b[0m\n", "", "checkpoint-unavailable"},
		{"embedded reset", "operationId=\x1b[0m" + id, "", "checkpoint-invalid-response"},
		{"reset without newline", "operationId=" + id + "\x1b[0m", "", "checkpoint-invalid-response"},
		{"unknown prefix", "prefix operationId=" + id + "\n\x1b[0m\n", "", "checkpoint-invalid-response"},
		{"other escape", "operationId=" + id + "\x1b[31m", "", "checkpoint-invalid-response"},
		{"extra text", "operationId=" + id + "\nextra\n\x1b[0m\n", "", "checkpoint-invalid-response"},
		{"multiple responses", "operationId=" + id + "\nerror=busy\n\x1b[0m\n", "", "checkpoint-invalid-response"},
		{"multiple resets", "operationId=" + id + "\x1b[0m\x1b[0m", "", "checkpoint-invalid-response"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gotID, gotReason := Checkpoint(tc.input)
			if gotID != tc.id || gotReason != tc.reason {
				t.Fatalf("Checkpoint(%q) = %q, %q; want %q, %q", tc.input, gotID, gotReason, tc.id, tc.reason)
			}
		})
	}
}
