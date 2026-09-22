package main

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/lepinoid/infra/updater/internal/journal"
	updaterengine "github.com/lepinoid/infra/updater/internal/updater"
)

// Given: a transaction that reached INSTALL_COMPLETE
// When: b7 annotates the deployment restart
// Then: kubectl receives the patch via -p argument; --patch-file is never used
// (kubectl 1.34 does not interpret --patch-file=- as stdin)
func TestB7AnnotateRestartPassesPatchAsArgument(t *testing.T) {
	x := gateTransaction(t)
	calls := stageB7DeploymentPatch(x, t)

	if err := x.b7AnnotateRestart(x.plan); err != nil {
		t.Fatal(err)
	}

	for _, call := range *calls {
		for _, a := range call.args {
			if strings.HasPrefix(a, "--patch-file") {
				t.Fatalf("stdin-based patch is unsupported: %q", call.args)
			}
		}
	}
	for _, call := range *calls {
		i := slices.Index(call.args, "-p")
		if i < 0 || i+1 >= len(call.args) {
			continue
		}
		if !strings.Contains(call.args[i+1], `"lepinoid.dev/restart-transaction":"`+x.j.TransactionID+`"`) {
			t.Fatalf("patch missing restart annotation: %s", call.args[i+1])
		}
		want := []string{"patch", "deployment", "build-server", "--type=strategic", "-p"}
		if !slices.Equal(call.args[:i+1], want) {
			t.Fatalf("unexpected patch argv: %q", call.args)
		}
		return
	}
	t.Fatalf("no -p patch call recorded: %+v", *calls)
}

func stageB7DeploymentPatch(x *tx, t *testing.T) *[]runnerCall {
	t.Helper()
	x.j.Phase = "INSTALL_COMPLETE"
	x.j.MaintenanceRequired = true
	x.plan.ObservedGeneration = x.j.FencingGeneration
	x.engine.Journal = x.j
	flag := journal.Flag{SchemaVersion: 1, TransactionID: x.j.TransactionID, CreatedAt: x.now(), JournalPath: "journal/" + x.j.TransactionID, FencingGeneration: x.j.FencingGeneration}
	if err := journal.Save(x.store.Path("maintenance.flag"), flag); err != nil {
		t.Fatal(err)
	}
	patched := false
	calls := &[]runnerCall{}
	x.commands = fakeRunner{calls: calls, reply: func(name string, args []string) ([]byte, error) {
		if name == "kubectl" && slices.Contains(args, "patch") {
			patched = true
			return nil, nil
		}
		t.Errorf("unexpected command: %s %q", name, args)
		return nil, errors.New("unexpected command")
	}}
	x.engine.Fence = updaterengine.Fence{
		Lease: func(context.Context) error { return nil },
		Observe: func(context.Context) (updaterengine.Observation, error) {
			generation := x.plan.ObservedGeneration
			if patched {
				generation++
			}
			return updaterengine.Observation{PodUID: x.podUID, Generation: generation}, nil
		},
	}
	return calls
}

// Given: b7's deployment patch succeeded and bumped the deployment generation
// When: the following journal save records the RESTART_REQUESTED transition
// Then: it is not fenced; the journal baseline advanced to the new generation
func TestB7PatchAdvancesJournalFenceGeneration(t *testing.T) {
	x := gateTransaction(t)
	stageB7DeploymentPatch(x, t)

	if err := x.b7AnnotateRestart(x.plan); err != nil {
		t.Fatal(err)
	}
	if err := x.saveJournal(); err != nil {
		t.Fatalf("save after patch must not be fenced: %v", err)
	}
	want := x.plan.ObservedGeneration + 1
	if x.j.FencingGeneration != want {
		t.Fatalf("journal fencingGeneration=%d, want %d", x.j.FencingGeneration, want)
	}
	persisted, err := journal.Read[journal.Journal](x.store.Path("journal", x.j.TransactionID))
	if err != nil {
		t.Fatal(err)
	}
	if persisted.FencingGeneration != want || persisted.RestartRequestedAt == nil {
		t.Fatalf("persisted journal lagging: %+v", persisted)
	}
}

// Given: b7's patch succeeded while maintenance.flag exists for this txn
// When: the fence baseline advances
// Then: the flag's fencing generation moves with it, keeping the gate ack chain consistent
func TestB7PatchAdvancesFlagGeneration(t *testing.T) {
	x := gateTransaction(t)
	stageB7DeploymentPatch(x, t)

	if err := x.b7AnnotateRestart(x.plan); err != nil {
		t.Fatal(err)
	}

	after, err := journal.Read[journal.Flag](x.store.Path("maintenance.flag"))
	if err != nil {
		t.Fatal(err)
	}
	if want := x.plan.ObservedGeneration + 1; after.FencingGeneration != want {
		t.Fatalf("flag fencingGeneration=%d, want %d", after.FencingGeneration, want)
	}
}
