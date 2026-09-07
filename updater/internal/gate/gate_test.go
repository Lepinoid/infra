package gate

import (
	"testing"
	"time"
)

func TestFreshBoundary(t *testing.T) {
	now := time.Date(2026, 9, 6, 3, 12, 0, 0, time.UTC)
	for _, tc := range []struct {
		delta time.Duration
		want  bool
	}{{6 * time.Second, true}, {6*time.Second + 1, false}, {-2 * time.Second, true}, {-2*time.Second - 1, false}} {
		if got := Fresh(now, now.Add(-tc.delta)); got != tc.want {
			t.Errorf("delta=%s got=%v", tc.delta, got)
		}
	}
}

func TestAckIdentity(t *testing.T) {
	now := time.Date(2026, 9, 6, 3, 12, 0, 0, time.UTC)
	tx := "tx"
	gen := int64(3)
	s := Status{SchemaVersion: 1, ServerInstanceID: "pod", Phase: "ACTIVE", ObservedTransaction: &tx, ObservedGeneration: &gen, UpdatedAt: now}
	expected := Identity{PodUID: "pod", TransactionID: tx, Generation: gen}
	if !s.Active(expected, now) {
		t.Fatal("matching ACTIVE rejected")
	}
	expected.Generation++
	if s.Active(expected, now) {
		t.Fatal("stale generation accepted")
	}
	s.Phase = "INACTIVE"
	s.ReleasedTransaction = &tx
	s.ReleasedGeneration = &gen
	if s.Inactive(expected, now) {
		t.Fatal("wrong released generation accepted")
	}
}
