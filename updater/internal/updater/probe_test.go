package updater

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/lepinoid/infra/updater/internal/gate"
	"github.com/lepinoid/infra/updater/internal/journal"
)

func probeEngine(t *testing.T) *Engine {
	t.Helper()
	store := journal.Store{Root: t.TempDir()}
	if err := store.Init(); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)
	tx := "550e8400-e29b-41d4-a716-446655440000"
	gen := int64(0)
	if err := journal.Save(store.Path("gate-status.json"), gate.Status{SchemaVersion: 1, Version: "fixture", ServerInstanceID: "pod", Phase: "ACTIVE", ObservedTransaction: &tx, ObservedGeneration: &gen, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	return &Engine{Store: store, Now: func() time.Time { return now }, Journal: journal.Journal{TransactionID: tx, Phase: "MAINTENANCE_ACTIVE", MaintenanceRequired: true, AccessState: "CLOSED", ExpectedPodUID: "pod"}, Fence: Fence{Lease: func(context.Context) error { return nil }, Observe: func(context.Context) (Observation, error) { return Observation{PodUID: "pod"}, nil }}}
}

func TestProbeRestoresOffBeforeCompletion(t *testing.T) {
	e := probeEngine(t)
	calls := []string{}
	err := e.ProbeRuntime(context.Background(), func(_ context.Context, command string) (string, error) {
		record, err := journal.Read[journal.Probe](e.Store.Path("journal", e.Journal.TransactionID+".probe"))
		if err != nil {
			t.Fatal(err)
		}
		if record.RuntimeBefore != nil {
			t.Fatal("completed before restoration")
		}
		calls = append(calls, command)
		switch command {
		case "on":
			return "Whitelist is now turned on\n", nil
		case "off":
			return "Whitelist is now turned off\n", nil
		default:
			t.Fatal(command)
			return "", nil
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || calls[0] != "on" || calls[1] != "off" {
		t.Fatal(calls)
	}
	record, err := journal.Read[journal.Probe](e.Store.Path("journal", e.Journal.TransactionID+".probe"))
	if err != nil {
		t.Fatal(err)
	}
	if record.RuntimeBefore == nil || *record.RuntimeBefore {
		t.Fatal(record)
	}
}

func TestProbeLostResponseRemainsIncomplete(t *testing.T) {
	e := probeEngine(t)
	lost := errors.New("response lost")
	err := e.ProbeRuntime(context.Background(), func(context.Context, string) (string, error) { return "", lost })
	if !errors.Is(err, lost) {
		t.Fatal(err)
	}
	record, err := journal.Read[journal.Probe](e.Store.Path("journal", e.Journal.TransactionID+".probe"))
	if err != nil {
		t.Fatal(err)
	}
	if record.RuntimeBefore != nil {
		t.Fatal("uncertain result completed")
	}
}

func TestProbeExistingIncompleteNeverRepeatsRCON(t *testing.T) {
	e := probeEngine(t)
	p := journal.Probe{SchemaVersion: 1, TransactionID: e.Journal.TransactionID, Probe: "runtime-whitelist", StartedAt: e.Now()}
	if err := journal.Save(e.Store.Path("journal", p.TransactionID+".probe"), p); err != nil {
		t.Fatal(err)
	}
	err := e.ProbeRuntime(context.Background(), func(context.Context, string) (string, error) { t.Fatal("repeated uncertain probe"); return "", nil })
	if !errors.Is(err, ErrBlocked) {
		t.Fatal(err)
	}
}
