package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lepinoid/infra/updater/internal/journal"
	updaterengine "github.com/lepinoid/infra/updater/internal/updater"
)

func terminalPremaintenance(t *testing.T, x *tx, phase string) {
	t.Helper()
	x.j.Phase, x.j.AccessState, x.j.MaintenanceRequired = phase, "OPEN", false
	x.j.CommitCandidate = nil
	if err := x.saveJournal(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(x.store.Path("maintenance.flag")); err != nil {
		t.Fatal(err)
	}
}

func terminalAssertArchived(t *testing.T, x *tx, id, outcome string) {
	t.Helper()
	archived, err := journal.Read[journal.Journal](x.store.Path("archive", id))
	if err != nil {
		t.Fatal(err)
	}
	if archived.Lifecycle != "TERMINAL" || archived.Outcome == nil || *archived.Outcome != outcome || archived.SuspendReason != nil || archived.AccessState != "OPEN" || archived.MaintenanceRequired {
		t.Fatalf("wrong archived outcome: %+v", archived)
	}
	if _, err := os.Stat(x.store.Path("journal", id)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old journal remains live: %v", err)
	}
	for _, suffix := range []string{".probe", ".recovery-own-a", ".recovery-own-b"} {
		if _, err := os.Stat(x.store.Path("archive", id+suffix)); err != nil {
			t.Fatalf("missing archived evidence %s: %v", suffix, err)
		}
	}
	if _, err := os.Stat(x.store.Path("recovery", "unrelated")); err != nil {
		t.Fatalf("unrelated recovery removed: %v", err)
	}
}

func TestTerminalSupersedeArchivesBeforeStartingNewTarget(t *testing.T) {
	for _, phase := range []string{"PREPARING", "DRIFT_DETECTED", "STAGED"} {
		for _, suspended := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/suspended=%v", phase, suspended), func(t *testing.T) {
				x := completionTransaction(t)
				terminalPremaintenance(t, x, phase)
				if suspended {
					if err := x.j.Suspend("mc-version-unknown"); err != nil {
						t.Fatal(err)
					}
					if err := x.saveJournal(); err != nil {
						t.Fatal(err)
					}
				}
				completionRecords(t, x)
				old := x.j
				currentBefore := string(completionRead(t, x.store.Path("current")))
				verifyInstalledPair = func(journal.Pair) error {
					t.Fatal("pre-maintenance supersede must not verify uninstalled target")
					return nil
				}
				plan := *x.plan
				plan.Desired.Digest = "sha256:" + strings.Repeat("f", 64)
				plan.Desired.Version = "replacement-version"
				plan.ObservedGeneration = old.FencingGeneration
				var calls []runnerCall
				x.commands = fakeRunner{calls: &calls, reply: func(name string, args []string) ([]byte, error) {
					if name != "kubectl" || slices.Contains(args, "patch") {
						t.Fatalf("unexpected mutation: %s %q", name, args)
					}
					terminalAssertArchived(t, x, old.TransactionID, "ABORTED")
					return nil, errors.New("server observations unavailable: stop new transaction at version gate")
				}}
				if err := x.runSupersede(old, &plan); err != nil {
					t.Fatal(err)
				}
				if len(calls) == 0 {
					t.Fatal("new transaction was not started")
				}
				terminalAssertArchived(t, x, old.TransactionID, "ABORTED")
				if x.j.TransactionID == old.TransactionID || x.j.TargetDigest != plan.Desired.Digest || x.j.Phase != "PREPARING" || x.j.Lifecycle != "SUSPENDED" || x.j.CommitCandidate != nil {
					t.Fatalf("replacement transaction not isolated from old state: %+v", x.j)
				}
				inv, err := x.store.Inspect()
				if err != nil || len(inv.Journals) != 1 || inv.Journals[0].TransactionID != x.j.TransactionID {
					t.Fatalf("next-job inventory blocked by old terminal: %+v %v", inv, err)
				}
				if after := string(completionRead(t, x.store.Path("current"))); after != currentBefore {
					t.Fatal("pre-maintenance supersede changed current")
				}
				if _, err := os.Stat(x.store.Path("gate-status.json")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("test must exercise absence of initial gate ack: %v", err)
				}
			})
		}
	}
}

func TestTerminalSupersedeRefusesEveryPostMaintenancePhase(t *testing.T) {
	for _, phase := range journal.Phases {
		if slices.Contains([]string{"PREPARING", "DRIFT_DETECTED", "STAGED"}, phase) {
			continue
		}
		t.Run(phase, func(t *testing.T) {
			x := completionTransaction(t)
			x.j.Phase = phase
			// Even an OPEN journal with no remaining flag must not be replaced
			// after it has passed the pre-maintenance phases.
			x.j.AccessState, x.j.MaintenanceRequired = "OPEN", false
			if err := x.saveJournal(); err != nil {
				t.Fatal(err)
			}
			old := string(completionRead(t, x.store.Path("journal", x.j.TransactionID)))
			if canSupersede(x.j) {
				t.Fatal("post-maintenance journal was replaceable")
			}
			if err := x.runSupersede(x.j, x.plan); err == nil {
				t.Fatal("post-maintenance supersede accepted")
			}
			if after := string(completionRead(t, x.store.Path("journal", x.j.TransactionID))); old != after {
				t.Fatal("rejected supersede changed journal")
			}
		})
	}
	for _, state := range []string{"CLOSING", "CLOSED", "OPENING", "maintenance required"} {
		t.Run(state, func(t *testing.T) {
			x := completionTransaction(t)
			x.j.Phase = "STAGED"
			x.j.AccessState, x.j.MaintenanceRequired = state, false
			if state == "maintenance required" {
				x.j.AccessState, x.j.MaintenanceRequired = "OPEN", true
			}
			if canSupersede(x.j) {
				t.Fatal("access transition was replaceable")
			}
			if err := x.runSupersede(x.j, x.plan); err == nil {
				t.Fatal("unsafe supersede accepted")
			}
		})
	}
}

func TestTerminalNormalOutcomesArchiveWithoutReapplying(t *testing.T) {
	for _, outcome := range []string{"SUCCEEDED", "ROLLED_BACK", "ABORTED", "ABORTED_NO_CHANGE"} {
		t.Run(outcome, func(t *testing.T) {
			x := completionTransaction(t)
			if outcome == "ABORTED" || outcome == "ABORTED_NO_CHANGE" {
				terminalPremaintenance(t, x, "STAGED")
			} else {
				completionReady(t, x)
			}
			if outcome == "ROLLED_BACK" {
				x.j.CommitCandidate = ptr("SOURCE")
				verifyInstalledPair = func(pair journal.Pair) error {
					if pair != x.j.Source {
						t.Fatalf("rollback checked target pair: %+v", pair)
					}
					return nil
				}
			} else if outcome != "SUCCEEDED" {
				verifyInstalledPair = func(journal.Pair) error { t.Fatal("pre-maintenance terminal requires no installed target"); return nil }
			}
			x.j.Lifecycle, x.j.Outcome = "TERMINAL", ptr(outcome)
			if err := x.saveJournal(); err != nil {
				t.Fatal(err)
			}
			completionRecords(t, x)
			before := string(completionRead(t, x.store.Path("current")))
			var calls []runnerCall
			x.commands = fakeRunner{calls: &calls, reply: func(name string, args []string) ([]byte, error) {
				t.Fatalf("terminal re-ran server work: %s %q", name, args)
				return nil, nil
			}}
			x.plan = nil // Terminal dispatch runs before desired-manifest discovery.
			if err := x.finalizeTerminal(); err != nil {
				t.Fatal(err)
			}
			terminalAssertArchived(t, x, x.j.TransactionID, outcome)
			if outcome != "SUCCEEDED" && string(completionRead(t, x.store.Path("current"))) != before {
				t.Fatal("non-success terminal changed current")
			}
			inv, err := x.store.Inspect()
			if err != nil || inv.Blocked || len(inv.Journals) != 0 {
				t.Fatalf("normal terminal blocked future jobs: %+v %v", inv, err)
			}
		})
	}
}

func TestTerminalFinalizationRejectsManualOrUnrestoredState(t *testing.T) {
	for _, fault := range []string{"not terminal", "no outcome", "manual recovery", "resolved manual recovery", "maintenance required", "closed", "flag", "incomplete restore phase", "missing ack", "stale ack", "wrong ack identity", "candidate mismatch", "lost lease"} {
		t.Run(fault, func(t *testing.T) {
			x := completionTransaction(t)
			completionReady(t, x)
			x.j.Lifecycle, x.j.Outcome, x.j.CommitCandidate = "TERMINAL", ptr("ROLLED_BACK"), ptr("SOURCE")
			verifyInstalledPair = func(pair journal.Pair) error {
				if pair != x.j.Source {
					return errors.New("wrong rollback candidate")
				}
				return nil
			}
			if err := x.saveJournal(); err != nil {
				t.Fatal(err)
			}
			completionRecords(t, x)
			switch fault {
			case "not terminal":
				x.j.Lifecycle = "ACTIVE"
			case "no outcome":
				x.j.Outcome = nil
			case "manual recovery", "resolved manual recovery":
				x.j.Outcome, x.j.FailureReason = ptr("FAILED_MANUAL_INTERVENTION"), ptr("whitelist-restore-failed")
				if fault == "resolved manual recovery" {
					x.j.Resolution = ptr("operator resolved")
				}
			case "maintenance required":
				x.j.MaintenanceRequired = true
			case "closed":
				x.j.AccessState = "CLOSED"
			case "flag":
				if err := x.engine.Flag(x.ctx); err != nil {
					t.Fatal(err)
				}
			case "incomplete restore phase":
				x.j.Phase = "BACKUP_COMPLETE"
			case "missing ack":
				if err := os.Remove(x.store.Path("gate-status.json")); err != nil {
					t.Fatal(err)
				}
			case "stale ack", "wrong ack identity":
				ack := completionAck(t, x, false)
				if fault == "stale ack" {
					ack.UpdatedAt = x.now().Add(-8 * time.Second)
				} else {
					ack.ReleasedTransaction = ptr("another-transaction")
				}
				if err := journal.Save(x.store.Path("gate-status.json"), ack); err != nil {
					t.Fatal(err)
				}
			case "candidate mismatch":
				verifyInstalledPair = func(journal.Pair) error { return errors.New("installed checksum mismatch") }
			case "lost lease":
				x.engine.Fence.Lease = func(context.Context) error { return errors.New("lease lost") }
			}
			before := string(completionRead(t, x.store.Path("current")))
			if err := x.finalizeTerminal(); err == nil {
				t.Fatal("unsafe terminal archived")
			}
			if _, err := os.Stat(x.store.Path("archive", x.j.TransactionID)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("unsafe terminal archive exists: %v", err)
			}
			for _, path := range []string{x.store.Path("journal", x.j.TransactionID), x.store.Path("journal", x.j.TransactionID+".probe"), x.store.Path("recovery", "own-a")} {
				if _, err := os.Stat(path); err != nil {
					t.Fatalf("evidence lost after rejected completion: %s %v", path, err)
				}
			}
			if after := string(completionRead(t, x.store.Path("current"))); before != after {
				t.Fatal("rejected terminal changed current")
			}
		})
	}
}

func TestTerminalAbortedArchiveResumesWithoutGateAck(t *testing.T) {
	x := completionTransaction(t)
	terminalPremaintenance(t, x, "PREPARING")
	x.j.Lifecycle, x.j.Outcome = "TERMINAL", ptr("ABORTED")
	if err := x.saveJournal(); err != nil {
		t.Fatal(err)
	}
	completionRecords(t, x)
	before := string(completionRead(t, x.store.Path("current")))
	obstruction := x.store.Path("archive", x.j.TransactionID)
	if err := os.Mkdir(obstruction, 0755); err != nil {
		t.Fatal(err)
	}
	if err := x.finalizeTerminal(); err == nil {
		t.Fatal("main archive failure not reached")
	}
	// Recovery and probe were already archived, while the ABORTED main record
	// remains live. A later Job must finish cleanup without recreating them.
	if _, err := os.Stat(x.store.Path("journal", x.j.TransactionID+".probe")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("probe archive boundary not reached: %v", err)
	}
	if err := os.Remove(obstruction); err != nil {
		t.Fatal(err)
	}
	persisted, err := journal.Read[journal.Journal](x.store.Path("journal", x.j.TransactionID))
	if err != nil {
		t.Fatal(err)
	}
	x.j, x.engine.Journal, x.plan = persisted, persisted, nil
	x.engine.Fence.Observe = func(context.Context) (updaterengine.Observation, error) {
		return updaterengine.Observation{PodUID: "replacement-pod", Generation: persisted.FencingGeneration + 1}, nil
	}
	if err := x.resumeTerminal(); err != nil {
		t.Fatal(err)
	}
	terminalAssertArchived(t, x, x.j.TransactionID, "ABORTED")
	if string(completionRead(t, x.store.Path("current"))) != before {
		t.Fatal("archive retry changed current")
	}
	if _, err := os.Stat(x.store.Path("gate-status.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("test must not manufacture a gate ack: %v", err)
	}
}

func TestTerminalResumeRejectsUnsafeOutcomeBeforeRefreshingFence(t *testing.T) {
	for _, fault := range []string{"resolved manual recovery", "closed normal terminal", "maintenance required", "flag present"} {
		t.Run(fault, func(t *testing.T) {
			x := completionTransaction(t)
			x.j.Lifecycle, x.j.Outcome = "TERMINAL", ptr("ABORTED")
			x.j.AccessState, x.j.MaintenanceRequired = "OPEN", false
			if fault != "flag present" {
				if err := os.Remove(x.store.Path("maintenance.flag")); err != nil {
					t.Fatal(err)
				}
			}
			switch fault {
			case "resolved manual recovery":
				x.j.Outcome = ptr("FAILED_MANUAL_INTERVENTION")
				x.j.FailureReason, x.j.Resolution = ptr("whitelist-restore-failed"), ptr("operator resolved")
				x.j.AccessState, x.j.MaintenanceRequired = "CLOSED", true
			case "closed normal terminal":
				x.j.AccessState = "CLOSED"
			case "maintenance required":
				x.j.MaintenanceRequired = true
			}
			if err := x.saveJournal(); err != nil {
				t.Fatal(err)
			}
			// A resolved FMI record passes inventory. Its missing flag would make
			// stale-fence closure verification fail and overwrite FMI as SUSPENDED
			// if the terminal entry ran refreshFenceOnResume before rejecting it.
			if inv, err := x.a1Inventory(); err != nil || inv.Blocked || len(inv.Journals) != 1 {
				t.Fatalf("fixture does not reach terminal dispatch: %+v %v", inv, err)
			}
			original := x.j
			journalBefore := string(completionRead(t, x.store.Path("journal", x.j.TransactionID)))
			currentBefore := string(completionRead(t, x.store.Path("current")))
			observations := 0
			x.engine.Fence.Observe = func(context.Context) (updaterengine.Observation, error) {
				observations++
				return updaterengine.Observation{PodUID: "replacement-pod", Generation: original.FencingGeneration + 1}, nil
			}
			if err := x.resumeTerminal(); err == nil {
				t.Fatal("unsafe terminal resumed")
			}
			if observations != 0 {
				t.Fatalf("outcome was checked after %d fence observations", observations)
			}
			if !reflect.DeepEqual(x.j, original) || !reflect.DeepEqual(x.engine.Journal, original) {
				t.Fatal("unsafe terminal converted into resumable state")
			}
			if got := string(completionRead(t, x.store.Path("journal", original.TransactionID))); got != journalBefore {
				t.Fatal("rejected terminal journal changed on disk")
			}
			if got := string(completionRead(t, x.store.Path("current"))); got != currentBefore {
				t.Fatal("rejected terminal changed current")
			}
			if _, err := os.Stat(x.store.Path("archive", original.TransactionID)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("rejected terminal was archived: %v", err)
			}
		})
	}
}
