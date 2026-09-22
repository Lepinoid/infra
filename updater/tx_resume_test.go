package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/lepinoid/infra/updater/internal/gate"
	"github.com/lepinoid/infra/updater/internal/journal"
	updaterengine "github.com/lepinoid/infra/updater/internal/updater"
)

func staleResumeFixture(t *testing.T, maintenance bool) *tx {
	t.Helper()
	x := gateTransaction(t)
	if maintenance {
		x.j.Phase = "MAINTENANCE_ACTIVE"
		x.j.AccessState = "CLOSED"
		x.j.MaintenanceRequired = true
	}
	x.j.ExpectedPodUID = "pod-gone"
	x.j.FencingGeneration = 0
	x.engine.Journal = x.j
	x.engine.Fence = updaterengine.Fence{
		Lease: func(context.Context) error { return nil },
		Observe: func(context.Context) (updaterengine.Observation, error) {
			return updaterengine.Observation{PodUID: "pod-gone", Generation: 0}, nil
		},
	}
	if err := x.saveJournal(); err != nil {
		t.Fatal(err)
	}
	x.engine.Fence = updaterengine.Fence{
		Lease: func(context.Context) error { return nil },
		Observe: func(context.Context) (updaterengine.Observation, error) {
			return updaterengine.Observation{PodUID: "pod-live", Generation: 7}, nil
		},
	}
	return x
}

// Given: a pre-mutation journal (maintenanceRequired=false) whose fence fields went stale
// When: the transaction is resumed
// Then: the fence baseline follows the live deployment and saves keep working
func TestResumeRefreshesPreMutationFenceFromLiveValues(t *testing.T) {
	x := staleResumeFixture(t, false)

	if err := x.refreshFenceOnResume(); err != nil {
		t.Fatal(err)
	}
	if x.j.ExpectedPodUID != "pod-live" || x.j.FencingGeneration != 7 {
		t.Fatalf("not refreshed: %+v", x.j)
	}
	persisted, err := journal.Read[journal.Journal](x.store.Path("journal", x.j.TransactionID))
	if err != nil {
		t.Fatal(err)
	}
	if persisted.ExpectedPodUID != "pod-live" || persisted.FencingGeneration != 7 || persisted.Lifecycle != "ACTIVE" {
		t.Fatalf("persisted journal not refreshed: %+v", persisted)
	}
	if err := x.saveJournal(); err != nil {
		t.Fatalf("post-refresh save must not be fenced: %v", err)
	}
}

// Given: a maintenance journal (maintenanceRequired=true) with stale fence fields,
// an intact flag for the same txn, a fresh ACTIVE gate ack, and intact candidate jars
// When: the transaction is resumed
// Then: the fence baseline (journal and flag alike) advances to the live values
func TestResumeRefreshesMaintenanceFenceAfterClosureVerified(t *testing.T) {
	x := staleResumeFixture(t, true)
	flag := journal.Flag{SchemaVersion: 1, TransactionID: x.j.TransactionID, CreatedAt: x.now(), JournalPath: "journal/" + x.j.TransactionID, FencingGeneration: 4}
	if err := journal.Save(x.store.Path("maintenance.flag"), flag); err != nil {
		t.Fatal(err)
	}
	ack := gate.Status{SchemaVersion: 1, Version: "1", ServerInstanceID: "pod-live", Phase: "ACTIVE", ObservedTransaction: &x.j.TransactionID, ObservedGeneration: &flag.FencingGeneration, UpdatedAt: x.now()}
	if err := journal.Save(x.store.Path("gate-status.json"), ack); err != nil {
		t.Fatal(err)
	}
	original := verifyInstalledPair
	defer func() { verifyInstalledPair = original }()
	verifyInstalledPair = func(journal.Pair) error { return nil }

	if err := x.refreshFenceOnResume(); err != nil {
		t.Fatal(err)
	}
	if x.j.FencingGeneration != 7 || x.j.ExpectedPodUID != "pod-live" || x.j.Lifecycle != "ACTIVE" {
		t.Fatalf("not refreshed: %+v", x.j)
	}
	after, err := journal.Read[journal.Flag](x.store.Path("maintenance.flag"))
	if err != nil {
		t.Fatal(err)
	}
	if after.FencingGeneration != 7 {
		t.Fatalf("flag not advanced: %+v", after)
	}
}

// Given: a maintenance journal whose closure cannot be verified (flag unreadable, ack not fresh, jars off)
// When: the transaction is resumed with stale fence fields
// Then: it suspends with closure-verification-failed and never adopts the live fence values
func TestResumeSuspendsWhenClosureUnverifiable(t *testing.T) {
	for _, tc := range []struct {
		name       string
		prepare    func(x *tx)
		contains   []string
		wantErrMsg string
	}{
		{name: "flag unreadable", prepare: func(x *tx) { /* gateTransaction leaves non-JSON flag */ }, wantErrMsg: "maintenance.flag"},
		{name: "ack not fresh", prepare: func(x *tx) {
			flag := journal.Flag{SchemaVersion: 1, TransactionID: x.j.TransactionID, CreatedAt: x.now(), JournalPath: "journal/" + x.j.TransactionID, FencingGeneration: 4}
			if err := journal.Save(x.store.Path("maintenance.flag"), flag); err != nil {
				t.Fatal(err)
			}
			stale := x.now().Add(-gate.AckTimeout)
			ack := gate.Status{SchemaVersion: 1, Version: "1", ServerInstanceID: "pod-live", Phase: "ACTIVE", ObservedTransaction: &x.j.TransactionID, ObservedGeneration: &flag.FencingGeneration, UpdatedAt: stale}
			if err := journal.Save(x.store.Path("gate-status.json"), ack); err != nil {
				t.Fatal(err)
			}
		}, wantErrMsg: "ACTIVE ack"},
		{name: "candidate jars off", prepare: func(x *tx) {
			flag := journal.Flag{SchemaVersion: 1, TransactionID: x.j.TransactionID, CreatedAt: x.now(), JournalPath: "journal/" + x.j.TransactionID, FencingGeneration: 4}
			if err := journal.Save(x.store.Path("maintenance.flag"), flag); err != nil {
				t.Fatal(err)
			}
			ack := gate.Status{SchemaVersion: 1, Version: "1", ServerInstanceID: "pod-live", Phase: "ACTIVE", ObservedTransaction: &x.j.TransactionID, ObservedGeneration: &flag.FencingGeneration, UpdatedAt: x.now()}
			if err := journal.Save(x.store.Path("gate-status.json"), ack); err != nil {
				t.Fatal(err)
			}
			original := verifyInstalledPair
			t.Cleanup(func() { verifyInstalledPair = original })
			verifyInstalledPair = func(journal.Pair) error { return errors.New("sha256 mismatch") }
		}, wantErrMsg: "commitCandidate=TARGET"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := staleResumeFixture(t, true)
			tc.prepare(x)

			err := x.refreshFenceOnResume()
			if err == nil || !strings.Contains(err.Error(), tc.wantErrMsg) {
				t.Fatalf("expected verification error containing %q, got %v", tc.wantErrMsg, err)
			}
			persisted, rerr := journal.Read[journal.Journal](x.store.Path("journal", x.j.TransactionID))
			if rerr != nil {
				t.Fatal(rerr)
			}
			if persisted.Lifecycle != "SUSPENDED" || persisted.SuspendReason == nil || *persisted.SuspendReason != "closure-verification-failed" {
				t.Fatalf("not suspended with reason: %+v", persisted)
			}
			if persisted.ExpectedPodUID != "pod-gone" || persisted.FencingGeneration != 0 {
				t.Fatalf("unverified resume must not adopt live fence values: %+v", persisted)
			}
			if err := persisted.Validate(); err != nil {
				t.Fatalf("suspended journal must stay schema-valid: %v", err)
			}
		})
	}
}
