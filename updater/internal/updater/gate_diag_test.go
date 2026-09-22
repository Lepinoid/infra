package updater

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/lepinoid/infra/updater/internal/gate"
)

// Given: a gate ack wait that timed out
// When: the diagnostic line is rendered
// Then: it carries the unmet expectation and the last observed status (or read failure)
func TestGateTimeoutDiagnosticContent(t *testing.T) {
	id := gate.Identity{PodUID: "pod-a", TransactionID: "txn-1", Generation: 5}
	txn := "txn-1"
	gen := int64(5)
	fresh := time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC)

	unreadable := gateTimeoutDiagnostic(id, true, nil, errors.New("permission denied"))
	for _, want := range []string{"ACTIVE", "pod-a", "txn-1", "permission denied"} {
		if !strings.Contains(unreadable, want) {
			t.Fatalf("diagnostic %q lacks %q", unreadable, want)
		}
	}

	stale := gate.Status{SchemaVersion: 1, Version: "1", ServerInstanceID: "pod-a", Phase: "INACTIVE", ObservedTransaction: &txn, ObservedGeneration: &gen, UpdatedAt: fresh}
	mismatch := gateTimeoutDiagnostic(id, true, &stale, nil)
	for _, want := range []string{"ACTIVE", "INACTIVE", "pod-a", "2026-09-09"} {
		if !strings.Contains(mismatch, want) {
			t.Fatalf("diagnostic %q lacks %q", mismatch, want)
		}
	}
}
