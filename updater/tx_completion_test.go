package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/lepinoid/infra/updater/internal/gate"
	"github.com/lepinoid/infra/updater/internal/journal"
	updaterengine "github.com/lepinoid/infra/updater/internal/updater"
)

func completionTransaction(t *testing.T) *tx {
	t.Helper()
	j, err := journal.Read[journal.Journal]("../build-server/contract-fixtures/infra/journal-active.json")
	if err != nil {
		t.Fatal(err)
	}
	store := journal.Store{Root: filepath.Join(t.TempDir(), "data", "plugins", ".lepinoid")}
	if err := store.Init(); err != nil {
		t.Fatal(err)
	}
	j.Phase = "ACCESS_RESTORE_STARTED"
	now := func() time.Time { return time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC) }
	source := j.TargetManifest
	source.Digest, source.ToolsSHA, source.MultiverseSHA = j.SourceDigest, j.Source.Tools.SHA256, j.Source.Multiverse.SHA256
	x := &tx{ctx: context.Background(), store: store, now: now, podName: "build-server-test", podUID: j.ExpectedPodUID, j: j, plan: &manifestPlan{Desired: j.TargetManifest, Current: journal.Current{Manifest: source, InstalledAt: now().Add(-time.Hour)}}}
	x.engine = &updaterengine.Engine{Store: store, Journal: j, Now: now, Fence: updaterengine.Fence{
		Lease: func(context.Context) error { return nil },
		Observe: func(context.Context) (updaterengine.Observation, error) {
			return updaterengine.Observation{PodUID: j.ExpectedPodUID, Generation: j.FencingGeneration}, nil
		},
	}}
	if err := x.saveJournal(); err != nil {
		t.Fatal(err)
	}
	if err := x.engine.Flag(x.ctx); err != nil {
		t.Fatal(err)
	}
	if err := journal.Save(store.Path("current"), x.plan.Current); err != nil {
		t.Fatal(err)
	}
	completionWrite(t, x.dataPath("server.properties"), []byte("rcon.password=test-password\nwhite-list=true\nother.property=keep\n"))
	completionWrite(t, x.dataPath("whitelist.json"), []byte(`[{"uuid":"during-maintenance","name":"temporary"}]`))
	original := verifyInstalledPair
	t.Cleanup(func() { verifyInstalledPair = original })
	verifyInstalledPair = func(pair journal.Pair) error {
		if pair != x.j.Target {
			return fmt.Errorf("unexpected candidate: %+v", pair)
		}
		return nil
	}
	return x
}

func completionWrite(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
}
func completionRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
func completionSnapshot(x *tx, exists, persisted, runtime bool) []byte {
	data := []byte("[\n  {\"uuid\":\"original-player\",\"name\":\"player\"}\n]\n")
	x.j.Maintenance.WhitelistExisted, x.j.Maintenance.PersistedEnabled, x.j.Maintenance.RuntimeEnabled = ptr(exists), ptr(persisted), ptr(runtime)
	if exists {
		x.j.Maintenance.WhitelistBackup = ptr(base64.StdEncoding.EncodeToString(data))
		x.j.Maintenance.WhitelistChecksum = ptr(fmt.Sprintf("%x", sha256.Sum256(data)))
	}
	return data
}
func completionAck(t *testing.T, x *tx, active bool) gate.Status {
	t.Helper()
	ack := gate.Status{SchemaVersion: 1, Version: "1", ServerInstanceID: x.j.ExpectedPodUID, Phase: "INACTIVE", ReleasedTransaction: ptr(x.j.TransactionID), ReleasedGeneration: ptr(x.j.FencingGeneration), UpdatedAt: x.now()}
	if active {
		ack.Phase, ack.ObservedTransaction, ack.ObservedGeneration = "ACTIVE", ptr(x.j.TransactionID), ptr(x.j.FencingGeneration)
	}
	if err := journal.Save(x.store.Path("gate-status.json"), ack); err != nil {
		t.Fatal(err)
	}
	return ack
}
func completionReady(t *testing.T, x *tx) {
	t.Helper()
	x.j.Phase, x.j.AccessState, x.j.MaintenanceRequired = "ACCESS_RESTORE_COMPLETE", "OPEN", false
	if err := x.saveJournal(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(x.store.Path("maintenance.flag")); err != nil {
		t.Fatal(err)
	}
	completionAck(t, x, false)
}
func completionRCON(t *testing.T, x *tx, action func(string) ([]byte, error)) *[]runnerCall {
	t.Helper()
	calls := &[]runnerCall{}
	x.commands = fakeRunner{calls: calls, reply: func(name string, args []string) ([]byte, error) {
		if name != "kubectl" {
			t.Fatalf("unexpected command %s %q", name, args)
		}
		if slices.Contains(args, "sed -n 's/^rcon.password=//p' /data/server.properties") {
			return []byte("test-password\n"), nil
		}
		i := slices.Index(args, "rcon-cli")
		if i < 0 || len(args) < i+4 {
			t.Fatalf("unexpected command %s %q", name, args)
		}
		if args[i+3] != "whitelist" || len(args) != i+5 {
			t.Fatalf("unexpected RCON command %q", args)
		}
		return action(args[i+4])
	}}
	return calls
}

func TestCompletionRestoresIndependentWhitelistSnapshot(t *testing.T) {
	for _, exists := range []bool{false, true} {
		for _, persisted := range []bool{false, true} {
			for _, runtime := range []bool{false, true} {
				t.Run(fmt.Sprintf("exists=%v/persisted=%v/runtime=%v", exists, persisted, runtime), func(t *testing.T) {
					x := completionTransaction(t)
					want := completionSnapshot(x, exists, persisted, runtime)
					var commands []string
					completionRCON(t, x, func(sub string) ([]byte, error) {
						commands = append(commands, sub)
						if _, err := os.Stat(x.store.Path("maintenance.flag")); err != nil {
							t.Fatalf("gate released before whitelist restoration: %v", err)
						}
						if sub == "reload" {
							got, err := os.ReadFile(x.dataPath("whitelist.json"))
							if exists && (err != nil || string(got) != string(want)) {
								t.Fatalf("reload saw wrong whitelist: %q %v", got, err)
							}
							if !exists && !errors.Is(err, os.ErrNotExist) {
								t.Fatalf("absent whitelist snapshot was not restored: %v", err)
							}
							return []byte("Reloaded the whitelist\n"), nil
						}
						// Model Minecraft persisting its runtime command, including
						// when the two original settings intentionally disagree.
						if err := writePersistedWhitelist(x.dataPath("server.properties"), sub == "on"); err != nil {
							t.Fatal(err)
						}
						return []byte("Whitelist is now turned " + sub + "\n"), nil
					})
					if err := x.restoreWhitelist(x.ctx); err != nil {
						t.Fatal(err)
					}
					action := "off"
					if runtime {
						action = "on"
					}
					if !slices.Equal(commands, []string{"reload", action}) {
						t.Fatalf("runtime state not explicitly restored: %v", commands)
					}
					if got, err := readPersistedWhitelist(x.dataPath("server.properties")); err != nil || got != persisted {
						t.Fatalf("persisted=%v want=%v err=%v", got, persisted, err)
					}
					if !strings.Contains(string(completionRead(t, x.dataPath("server.properties"))), "other.property=keep") {
						t.Fatal("unrelated property lost")
					}
				})
			}
		}
	}
}

func TestCompletionOpenSynchronizesJournalOnlyAfterGateAck(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		x := completionTransaction(t)
		x.now = func() time.Time { return time.Now().UTC() }
		x.engine.Now = x.now
		completionSnapshot(x, true, true, false)
		completionAck(t, x, true)
		var commands []string
		completionRCON(t, x, func(sub string) ([]byte, error) {
			commands = append(commands, sub)
			if _, err := os.Stat(x.store.Path("maintenance.flag")); err != nil {
				t.Fatalf("flag absent during restore: %v", err)
			}
			if sub == "reload" {
				return []byte("Reloaded the whitelist"), nil
			}
			return []byte("Whitelist is now turned " + sub), writePersistedWhitelist(x.dataPath("server.properties"), sub == "on")
		})
		transactionID := x.j.TransactionID
		watchCtx, stopWatch := context.WithCancel(context.Background())
		defer stopWatch()
		ackDone := make(chan error, 1)
		go func() {
			for {
				if _, err := os.Stat(x.store.Path("maintenance.flag")); errors.Is(err, os.ErrNotExist) {
					break
				}
				select {
				case <-watchCtx.Done():
					ackDone <- watchCtx.Err()
					return
				case <-time.After(time.Second):
				}
			}
			// Observe the real removal boundary before supplying an INACTIVE ack.
			j, err := journal.Read[journal.Journal](x.store.Path("journal", transactionID))
			if err == nil && (j.AccessState != "OPENING" || j.MaintenanceRequired) {
				err = fmt.Errorf("incorrect journal at gate release: %+v", j)
			}
			if err == nil {
				persisted, readErr := readPersistedWhitelist(x.dataPath("server.properties"))
				if readErr != nil || !persisted {
					err = fmt.Errorf("gate released before persisted whitelist restoration: persisted=%v err=%v", persisted, readErr)
				}
			}
			if err != nil {
				ackDone <- err
				return
			}
			ack := gate.Status{SchemaVersion: 1, Version: "1", ServerInstanceID: j.ExpectedPodUID, Phase: "INACTIVE", ReleasedTransaction: ptr(j.TransactionID), ReleasedGeneration: ptr(j.FencingGeneration), UpdatedAt: time.Now().UTC()}
			ackDone <- journal.Save(x.store.Path("gate-status.json"), ack)
		}()
		if err := x.openAccess(); err != nil {
			t.Fatal(err)
		}
		if err := <-ackDone; err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(commands, []string{"reload", "off"}) {
			t.Fatalf("restore commands: %v", commands)
		}
		persisted, err := journal.Read[journal.Journal](x.store.Path("journal", x.j.TransactionID))
		if err != nil {
			t.Fatal(err)
		}
		for _, j := range []journal.Journal{x.j, x.engine.Journal, persisted} {
			if j.Phase != "ACCESS_RESTORE_COMPLETE" || j.AccessState != "OPEN" || j.MaintenanceRequired {
				t.Fatalf("open state not synchronized: %+v", j)
			}
		}
	})
}

func TestCompletionRestoreFailureKeepsGateClosed(t *testing.T) {
	for _, fault := range []string{"snapshot missing", "checksum", "invalid backup", "reload response", "runtime response", "installed pair"} {
		t.Run(fault, func(t *testing.T) {
			x := completionTransaction(t)
			completionSnapshot(x, true, false, false)
			switch fault {
			case "snapshot missing":
				x.j.Maintenance.RuntimeEnabled = nil
			case "checksum":
				x.j.Maintenance.WhitelistChecksum = ptr(strings.Repeat("0", 64))
			case "invalid backup":
				x.j.Maintenance.WhitelistBackup = ptr("not base64")
			case "installed pair":
				verifyInstalledPair = func(journal.Pair) error { return errors.New("installed sha mismatch") }
			}
			completionRCON(t, x, func(sub string) ([]byte, error) {
				if sub == "reload" {
					if fault == "reload response" {
						return []byte("unexpected reload reply"), nil
					}
					return []byte("Reloaded the whitelist"), nil
				}
				if fault == "runtime response" {
					return []byte("unexpected whitelist reply"), nil
				}
				return []byte("Whitelist is now turned " + sub), nil
			})
			before := string(completionRead(t, x.store.Path("current")))
			if err := x.openAccess(); err == nil {
				t.Fatal("restoration failure accepted")
			}
			if _, err := os.Stat(x.store.Path("maintenance.flag")); err != nil {
				t.Fatalf("gate opened after failed restoration: %v", err)
			}
			if after := string(completionRead(t, x.store.Path("current"))); after != before {
				t.Fatal("current advanced after failed restoration")
			}
			persisted, err := journal.Read[journal.Journal](x.store.Path("journal", x.j.TransactionID))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(x.j, x.engine.Journal) || !reflect.DeepEqual(x.j, persisted) {
				t.Fatal("failed engine journal not synchronized")
			}
			if persisted.AccessState != "CLOSED" || !persisted.MaintenanceRequired || persisted.Outcome == nil || *persisted.Outcome != "FAILED_MANUAL_INTERVENTION" {
				t.Fatalf("not fail-closed: %+v", persisted)
			}
		})
	}
}

func TestCompletionFinalizationRejectsUnprovenOpenTarget(t *testing.T) {
	for _, fault := range []string{"phase", "access", "maintenance", "nil candidate", "source candidate", "flag", "missing ack", "stale ack", "wrong pod", "wrong transaction", "wrong generation", "active ack", "installed pair", "lost lease"} {
		t.Run(fault, func(t *testing.T) {
			x := completionTransaction(t)
			completionReady(t, x)
			ack := completionAck(t, x, false)
			switch fault {
			case "phase":
				x.j.Phase = "VERIFYING"
			case "access":
				x.j.AccessState = "OPENING"
			case "maintenance":
				x.j.MaintenanceRequired = true
			case "nil candidate":
				x.j.CommitCandidate = nil
			case "source candidate":
				x.j.CommitCandidate = ptr("SOURCE")
			case "flag":
				if err := x.engine.Flag(x.ctx); err != nil {
					t.Fatal(err)
				}
			case "missing ack":
				if err := os.Remove(x.store.Path("gate-status.json")); err != nil {
					t.Fatal(err)
				}
			case "stale ack":
				ack.UpdatedAt = x.now().Add(-7 * time.Second)
			case "wrong pod":
				ack.ServerInstanceID = "different-pod"
			case "wrong transaction":
				ack.ReleasedTransaction = ptr("different-transaction")
			case "wrong generation":
				ack.ReleasedGeneration = ptr(x.j.FencingGeneration + 1)
			case "active ack":
				ack.Phase = "ACTIVE"
			case "installed pair":
				verifyInstalledPair = func(journal.Pair) error { return errors.New("installed sha mismatch") }
			case "lost lease":
				x.engine.Fence.Lease = func(context.Context) error { return errors.New("lease lost") }
			}
			if fault != "missing ack" {
				if err := journal.Save(x.store.Path("gate-status.json"), ack); err != nil {
					t.Fatal(err)
				}
			}
			before := string(completionRead(t, x.store.Path("current")))
			if err := x.finalizeSuccess(); err == nil {
				t.Fatal("unproven successful transaction accepted")
			}
			if after := string(completionRead(t, x.store.Path("current"))); before != after {
				t.Fatal("current advanced despite failed completion prerequisite")
			}
			if _, err := os.Stat(x.store.Path("journal", x.j.TransactionID)); err != nil {
				t.Fatalf("journal disappeared: %v", err)
			}
			if _, err := os.Stat(x.store.Path("archive", x.j.TransactionID)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid completion archived: %v", err)
			}
		})
	}
}

func completionRecords(t *testing.T, x *tx) {
	t.Helper()
	for _, name := range []string{"own-a", "own-b", "unrelated"} {
		id := x.j.TransactionID
		if name == "unrelated" {
			id = "another-transaction"
		}
		record := journal.Recovery{SchemaVersion: 1, TransactionID: ptr(id), Result: "completed", Timestamp: x.now()}
		if err := journal.Save(x.store.Path("recovery", name), record); err != nil {
			t.Fatal(err)
		}
	}
	probe := journal.Probe{SchemaVersion: 1, TransactionID: x.j.TransactionID, Probe: "runtime-whitelist", StartedAt: x.now(), RuntimeBefore: ptr(false)}
	if err := journal.Save(x.store.Path("journal", x.j.TransactionID+".probe"), probe); err != nil {
		t.Fatal(err)
	}
}
func completionAssertArchived(t *testing.T, x *tx) {
	t.Helper()
	for _, name := range []string{x.j.TransactionID, x.j.TransactionID + ".probe", x.j.TransactionID + ".recovery-own-a", x.j.TransactionID + ".recovery-own-b"} {
		if _, err := os.Stat(x.store.Path("archive", name)); err != nil {
			t.Fatalf("not archived %s: %v", name, err)
		}
	}
	for _, parts := range [][]string{{"journal", x.j.TransactionID}, {"journal", x.j.TransactionID + ".probe"}, {"recovery", "own-a"}, {"recovery", "own-b"}} {
		if _, err := os.Stat(x.store.Path(parts...)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("live record left after archive %v: %v", parts, err)
		}
	}
	if _, err := os.Stat(x.store.Path("recovery", "unrelated")); err != nil {
		t.Fatalf("unrelated recovery lost: %v", err)
	}
	archived, err := journal.Read[journal.Journal](x.store.Path("archive", x.j.TransactionID))
	if err != nil {
		t.Fatal(err)
	}
	if archived.Lifecycle != "TERMINAL" || archived.Outcome == nil || *archived.Outcome != "SUCCEEDED" || archived.AccessState != "OPEN" || archived.MaintenanceRequired {
		t.Fatalf("wrong archived state: %+v", archived)
	}
}

func TestCompletionCommitsCurrentThenArchivesAllTransactionRecords(t *testing.T) {
	x := completionTransaction(t)
	completionReady(t, x)
	completionRecords(t, x)
	terminalChecked := false
	x.engine.Fence.Lease = func(context.Context) error {
		if x.j.Lifecycle == "TERMINAL" {
			current, err := journal.Read[journal.Current](x.store.Path("current"))
			if err != nil || !journal.SameIdentity(current.Manifest, x.j.TargetManifest) {
				t.Fatalf("terminal preceded current commit: %+v %v", current, err)
			}
			terminalChecked = true
		}
		return nil
	}
	if err := x.finalizeSuccess(); err != nil {
		t.Fatal(err)
	}
	if !terminalChecked {
		t.Fatal("terminal save was not fenced")
	}
	current, err := journal.Read[journal.Current](x.store.Path("current"))
	if err != nil || !journal.SameIdentity(current.Manifest, x.j.TargetManifest) || !current.InstalledAt.Equal(x.now()) {
		t.Fatalf("current not committed: %+v %v", current, err)
	}
	completionAssertArchived(t, x)
}

func TestCompletionRetriesAfterCurrentAndArchiveCrashBoundaries(t *testing.T) {
	for _, boundary := range []string{"terminal save", "recovery archive", "main archive"} {
		t.Run(boundary, func(t *testing.T) {
			x := completionTransaction(t)
			completionReady(t, x)
			completionRecords(t, x)
			injected := errors.New("simulated crash before terminal save")
			obstruction := ""
			if boundary == "terminal save" {
				x.engine.Fence.Lease = func(context.Context) error {
					if x.j.Lifecycle == "TERMINAL" {
						return injected
					}
					return nil
				}
			} else {
				obstruction = x.store.Path("archive", x.j.TransactionID)
				if boundary == "recovery archive" {
					obstruction += ".recovery-own-a"
				}
				if err := os.Mkdir(obstruction, 0755); err != nil {
					t.Fatal(err)
				}
			}
			if err := x.finalizeSuccess(); err == nil {
				t.Fatal("injected interruption not reached")
			}
			committed, err := journal.Read[journal.Current](x.store.Path("current"))
			if err != nil || !journal.SameIdentity(committed.Manifest, x.j.TargetManifest) {
				t.Fatalf("current missing at crash boundary: %+v %v", committed, err)
			}
			persisted, err := journal.Read[journal.Journal](x.store.Path("journal", x.j.TransactionID))
			if err != nil {
				t.Fatal(err)
			}
			wantLifecycle := "TERMINAL"
			if boundary == "terminal save" {
				wantLifecycle = "ACTIVE"
			}
			if persisted.Lifecycle != wantLifecycle {
				t.Fatalf("wrong crash state: %s", persisted.Lifecycle)
			}
			if obstruction != "" {
				if err := os.Remove(obstruction); err != nil {
					t.Fatal(err)
				}
			}
			x.engine.Fence.Lease = func(context.Context) error { return nil }
			x.now = func() time.Time { return committed.InstalledAt.Add(time.Second) }
			x.engine.Now = x.now
			// A terminal retry enters before manifest discovery and has no plan.
			if persisted.Lifecycle == "TERMINAL" {
				x.plan = nil
			}
			// Resume from the disk journal, including TERMINAL but not archived.
			if err := x.recoverEntry(persisted); err != nil {
				t.Fatal(err)
			}
			after, err := journal.Read[journal.Current](x.store.Path("current"))
			if err != nil || !after.InstalledAt.Equal(committed.InstalledAt) {
				t.Fatalf("retry changed installation time: %+v %v", after, err)
			}
			completionAssertArchived(t, x)
		})
	}
}

func TestCompletionGCProtectsBundleIdentitiesAndGate(t *testing.T) {
	x := completionTransaction(t)
	current := x.plan.Current
	current.Digest = "sha256:" + strings.Repeat("e", 64)
	if err := journal.Save(x.store.Path("current"), current); err != nil {
		t.Fatal(err)
	}
	protected := []string{x.j.SourceDigest, x.j.TargetDigest, current.Digest}
	unprotected := []string{"sha256:" + strings.Repeat("1", 64), "sha256:" + strings.Repeat("2", 64), "sha256:" + strings.Repeat("3", 64)}
	for _, section := range []string{"staging", "backup"} {
		for i, digest := range append(slices.Clone(protected), unprotected...) {
			path := x.store.Path(section, digest)
			if err := os.Mkdir(path, 0755); err != nil {
				t.Fatal(err)
			}
			stamp := x.now().Add(time.Duration(i) * time.Hour)
			if err := os.Chtimes(path, stamp, stamp); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Mkdir(x.store.Path(section, "gate"), 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := x.b11GC(); err != nil {
		t.Fatal(err)
	}
	for _, section := range []string{"staging", "backup"} {
		for _, digest := range append(slices.Clone(protected), "gate", unprotected[1], unprotected[2]) {
			if _, err := os.Stat(x.store.Path(section, digest)); err != nil {
				t.Fatalf("protected/retained generation deleted %s/%s: %v", section, digest, err)
			}
		}
		if _, err := os.Stat(x.store.Path(section, unprotected[0])); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("old unprotected generation not deleted: %v", err)
		}
	}
}

func TestCompletionSaveSynchronizesEngineAndSuspensionStopsRun(t *testing.T) {
	x := completionTransaction(t)
	x.j.Phase = "MAINTENANCE_ACTIVE"
	if err := x.saveJournal(); err != nil {
		t.Fatal(err)
	}
	if x.engine.Journal.Phase != "MAINTENANCE_ACTIVE" {
		t.Fatal("save used stale engine journal")
	}
	var calls []runnerCall
	x.commands = fakeRunner{calls: &calls, reply: func(name string, args []string) ([]byte, error) {
		if name != "kubectl" || slices.Contains(args, "patch") {
			t.Fatalf("mutation after player observation failure: %s %q", name, args)
		}
		if slices.Contains(args, "sed -n 's/^rcon.password=//p' /data/server.properties") {
			return []byte("test-password"), nil
		}
		if slices.Equal(args, []string{"get", "pod", x.podName, "-o", "json"}) {
			return []byte(fmt.Sprintf(`{"metadata":{"name":%q,"uid":%q}}`, x.podName, x.podUID)), nil
		}
		if slices.Contains(args, "list") || slices.Contains(args, "mc-monitor") {
			return []byte("unparseable player count"), nil
		}
		t.Fatalf("advanced past player-count suspension: %s %q", name, args)
		return nil, nil
	}}
	if err := x.runActive(); err != nil {
		t.Fatalf("suspension should be normal stop: %v", err)
	}
	persisted, err := journal.Read[journal.Journal](x.store.Path("journal", x.j.TransactionID))
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range []journal.Journal{x.j, x.engine.Journal, persisted} {
		if j.Lifecycle != "SUSPENDED" || j.SuspendReason == nil || *j.SuspendReason != "player-count-unknown" || j.Phase != "MAINTENANCE_ACTIVE" || j.AccessState != "CLOSED" {
			t.Fatalf("suspension advanced or was not persisted: %+v", j)
		}
	}
	if err := x.suspend("checkpoint-timeout"); !errors.Is(err, errSuspended) {
		t.Fatalf("missing stop sentinel: %v", err)
	}
}

func TestCompletionOpenTimeoutReclosesAndSynchronizesSuspension(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		x := completionTransaction(t)
		x.now = func() time.Time { return time.Now().UTC() }
		x.engine.Now = x.now
		completionSnapshot(x, true, false, false)
		completionAck(t, x, true)
		completionRCON(t, x, func(sub string) ([]byte, error) {
			if sub == "reload" {
				return []byte("Reloaded the whitelist"), nil
			}
			return []byte("Whitelist is already turned " + sub), nil
		})
		before := string(completionRead(t, x.store.Path("current")))
		id, pod, generation := x.j.TransactionID, x.j.ExpectedPodUID, x.j.FencingGeneration
		watchCtx, stopWatch := context.WithCancel(context.Background())
		defer stopWatch()
		ackDone := make(chan error, 1)
		go func() {
			for {
				select {
				case <-watchCtx.Done():
					ackDone <- nil
					return
				case <-time.After(time.Second):
				}
				// Simulate a gate that never acknowledges release, but acknowledges
				// the fallback maintenance flag once the updater reinstates it.
				if _, err := os.Stat(x.store.Path("maintenance.flag")); err == nil {
					ack := gate.Status{SchemaVersion: 1, Version: "1", ServerInstanceID: pod, Phase: "ACTIVE", ObservedTransaction: ptr(id), ObservedGeneration: ptr(generation), UpdatedAt: time.Now().UTC()}
					if err := journal.Save(x.store.Path("gate-status.json"), ack); err != nil {
						ackDone <- err
						return
					}
				}
			}
		}()
		err := x.openAccess()
		stopWatch()
		if ackErr := <-ackDone; ackErr != nil {
			t.Fatal(ackErr)
		}
		if err == nil || !strings.Contains(err.Error(), "gate wait timed out") {
			t.Fatalf("missing gate release failure: %v", err)
		}
		persisted, err := journal.Read[journal.Journal](x.store.Path("journal", id))
		if err != nil {
			t.Fatal(err)
		}
		for _, j := range []journal.Journal{x.j, x.engine.Journal, persisted} {
			if j.Lifecycle != "SUSPENDED" || j.AccessState != "CLOSED" || !j.MaintenanceRequired || j.SuspendReason == nil || *j.SuspendReason != "gate-ack-timeout" {
				t.Fatalf("timeout state not synchronized: %+v", j)
			}
		}
		if _, err := os.Stat(x.store.Path("maintenance.flag")); err != nil {
			t.Fatalf("maintenance was not restored: %v", err)
		}
		if after := string(completionRead(t, x.store.Path("current"))); before != after {
			t.Fatal("current committed without gate release ack")
		}
	})
}

func TestCompletionGCStopsWithoutDeletingWhenFenced(t *testing.T) {
	x := completionTransaction(t)
	var paths []string
	for _, section := range []string{"staging", "backup"} {
		for i := 1; i <= 3; i++ {
			path := x.store.Path(section, fmt.Sprintf("sha256:%064d", i))
			if err := os.Mkdir(path, 0755); err != nil {
				t.Fatal(err)
			}
			paths = append(paths, path)
		}
	}
	lost := errors.New("lost lease before GC mutation")
	x.engine.Fence.Lease = func(context.Context) error { return lost }
	if err := x.b11GC(); !errors.Is(err, lost) {
		t.Fatalf("GC ignored fencing: %v", err)
	}
	for _, path := range paths {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("GC deleted after loss: %s: %v", path, err)
		}
	}
}
