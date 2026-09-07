package status

import (
	"testing"
	"time"
)

func TestCompatibilityUnknown(t *testing.T) {
	now := time.Date(2026, 9, 6, 3, 12, 0, 0, time.UTC)
	s := Status{SchemaVersion: 1, ServerInstanceID: "pod", MinecraftVersion: "1.21.1", UpdatedAt: now}
	for _, tc := range []struct {
		version string
		ok      bool
		age     time.Duration
		want    string
	}{{"1.21.1", true, 0, "1.21.1"}, {"1.21.2", true, 0, ""}, {"1.21.1", false, 0, ""}, {"1.21.1", true, 7 * time.Second, ""}} {
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
