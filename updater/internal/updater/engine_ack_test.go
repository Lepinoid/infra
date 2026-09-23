package updater

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/lepinoid/infra/updater/internal/gate"
	"github.com/lepinoid/infra/updater/internal/journal"
)

func TestReleaseRequiresAcknowledgementAfterFlagRemoval(t *testing.T) {
	e := probeEngine(t)
	generation := e.Journal.FencingGeneration
	ack := gate.Status{SchemaVersion: 1, Version: "fixture", ServerInstanceID: e.Journal.ExpectedPodUID, Phase: "INACTIVE", ReleasedTransaction: &e.Journal.TransactionID, ReleasedGeneration: &generation, UpdatedAt: e.Now().Add(-time.Second)}
	if !ack.Inactive(e.identity(), e.Now()) {
		t.Fatal("fixture must otherwise be a fresh matching ack")
	}
	if err := journal.Save(e.Store.Path("gate-status.json"), ack); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := e.waitGateAfter(ctx, false, e.Now()); !errors.Is(err, context.Canceled) {
		t.Fatalf("old INACTIVE ack was accepted: %v", err)
	}
	ack.UpdatedAt = e.Now()
	if err := journal.Save(e.Store.Path("gate-status.json"), ack); err != nil {
		t.Fatal(err)
	}
	if err := e.waitGateAfter(context.Background(), false, e.Now()); err != nil {
		t.Fatal(err)
	}
}
