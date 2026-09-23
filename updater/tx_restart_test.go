package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lepinoid/infra/updater/internal/fsutil"
	"github.com/lepinoid/infra/updater/internal/gate"
	"github.com/lepinoid/infra/updater/internal/journal"
	recovery "github.com/lepinoid/infra/updater/internal/recover"
	"github.com/lepinoid/infra/updater/internal/status"
	updaterengine "github.com/lepinoid/infra/updater/internal/updater"
)

func restartFixture(t *testing.T) (*tx, string, *restartPod, *time.Time) {
	t.Helper()
	x := gateTransaction(t)
	data := t.TempDir()
	x.store = journal.Store{Root: filepath.Join(data, "plugins", ".lepinoid")}
	if err := x.store.Init(); err != nil {
		t.Fatal(err)
	}
	x.engine.Store = x.store
	now := x.now()
	x.now = func() time.Time { return now }
	x.engine.Now = x.now
	for _, name := range []string{"LepinoidTools.jar", "Multiverse-Core.jar", "lepinoid-tools-gate.jar"} {
		if err := os.WriteFile(filepath.Join(data, "plugins", name), []byte("jar"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	hash := testSHA256([]byte("jar"))
	x.plan.Desired.ToolsSHA, x.plan.Desired.MultiverseSHA = hash, hash
	x.j.TargetManifest = x.plan.Desired
	x.j.Target = sourcePairFromManifest(x.plan.Desired)
	x.j.AccessState = "CLOSED"
	x.j.Maintenance.PersistedEnabled = ptr(true)
	if err := os.WriteFile(filepath.Join(data, "server.properties"), []byte("white-list=true\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := journal.Save(x.store.Path("gate-manifest.json"), recovery.GateManifest{SchemaVersion: 1, File: "lepinoid-tools-gate.jar", SHA256: hash, Version: "1", SupportedMinecraft: []string{"1.21.8"}}); err != nil {
		t.Fatal(err)
	}
	stageB7DeploymentPatch(x, t)
	if err := x.b7AnnotateRestart(x.plan); err != nil {
		t.Fatal(err)
	}
	x.j.Phase = "RESTART_REQUESTED"
	if err := x.saveJournal(); err != nil {
		t.Fatal(err)
	}
	pod := &restartPod{}
	pod.Metadata.Name, pod.Metadata.UID = "replacement-name", "replacement-uid"
	pod.Metadata.Annotations = map[string]string{"lepinoid.dev/restart-transaction": x.j.TransactionID, "lepinoid.dev/restart-requested-at": x.j.RestartRequestedAt.Format(time.RFC3339Nano)}
	if err := json.Unmarshal([]byte(`{"spec":{"containers":[{"name":"minecraft"},{"name":"updater"}]},"status":{"phase":"Running","containerStatuses":[{"name":"minecraft","ready":true},{"name":"updater","ready":true}]}}`), pod); err != nil {
		t.Fatal(err)
	}
	oldVerify, oldWait := verifyInstalledPair, restartWait
	t.Cleanup(func() { verifyInstalledPair, restartWait = oldVerify, oldWait })
	verifyInstalledPair = func(pair journal.Pair) error {
		for _, jar := range []journal.Jar{pair.Tools, pair.Multiverse} {
			sum, err := fsutil.SHA256(filepath.Join(data, "plugins", jar.Name))
			if err != nil {
				return err
			}
			if sum != jar.SHA256 {
				return errors.New("candidate checksum mismatch")
			}
		}
		return nil
	}
	restartWait = func(ctx context.Context, d time.Duration) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		now = now.Add(d)
		writeRestartAck(t, x, pod.Metadata.UID)
		return nil
	}
	return x, data, pod, &now
}

func writeRestartAck(t *testing.T, x *tx, uid string) {
	t.Helper()
	flag, err := journal.Read[journal.Flag](x.store.Path("maintenance.flag"))
	if err != nil {
		t.Fatal(err)
	}
	ack := gate.Status{SchemaVersion: 1, Version: "1", ServerInstanceID: uid, Phase: "ACTIVE", ObservedTransaction: &flag.TransactionID, ObservedGeneration: &flag.FencingGeneration, UpdatedAt: x.now()}
	if err := journal.Save(x.store.Path("gate-status.json"), ack); err != nil {
		t.Fatal(err)
	}
}

func startupStatus(x *tx, pod *restartPod) status.Status {
	return status.Status{SchemaVersion: 1, ServerInstanceID: pod.Metadata.UID, ProcessStartedAt: x.j.RestartRequestedAt.Add(time.Second), UpdatedAt: x.j.RestartRequestedAt.Add(2 * time.Second), Phase: "HEALTHY", PluginVersion: x.plan.Desired.Version, PluginCommitSHA: x.plan.Desired.PluginCommitSHA, MinecraftVersion: "1.21.8", Multiverse: status.Multiverse{Detected: &x.plan.Desired.MultiverseVersion, Expected: x.plan.Desired.MultiverseVersion, Compatible: true}}
}

func restartCommands(t *testing.T, x *tx, pod *restartPod, s *status.Status, monitorError error) *[]runnerCall {
	t.Helper()
	calls := &[]runnerCall{}
	x.commands = fakeRunner{calls: calls, reply: func(name string, args []string) ([]byte, error) {
		if name != "kubectl" {
			return nil, fmt.Errorf("unexpected local command %s", name)
		}
		if slices.Equal(args, []string{"get", "pods", "-l", "app=build-server", "-o", "json"}) {
			return json.Marshal(map[string]any{"items": []any{pod}})
		}
		if slices.Equal(args, []string{"get", "pod", pod.Metadata.Name, "-o", "json"}) {
			return json.Marshal(pod)
		}
		if len(args) > 5 && args[0] == "exec" {
			if args[1] != pod.Metadata.Name {
				t.Errorf("exec targeted old/wrong Pod: %q", args)
				return nil, errors.New("wrong Pod")
			}
			if slices.Equal(args[5:], []string{"cat", "/data/plugins/LepinoidTools/updater-status.json"}) {
				return json.Marshal(s)
			}
			if slices.Equal(args[5:], []string{"mc-monitor", "status", "--host", "localhost", "--port", "25565", "-json"}) && args[3] == "minecraft" {
				if monitorError != nil {
					return nil, monitorError
				}
				return []byte(`{"server_info":{"version":{"name":"Paper 1.21.8"}}}`), nil
			}
		}
		return nil, fmt.Errorf("unexpected command %s %q", name, args)
	}}
	return calls
}

func TestRestartB7InitRecoveryB8B9(t *testing.T) {
	for _, phase := range []string{"same job", "RESTART_REQUESTED", "VERIFYING"} {
		t.Run(phase, func(t *testing.T) {
			x, data, pod, now := restartFixture(t)
			previous := *x.j.PreviousPodUID
			if err := (recovery.Runner{Store: x.store, Data: data, PodUID: pod.Metadata.UID, Now: x.now}).Run(); err != nil {
				t.Fatal(err)
			}
			flag, err := journal.Read[journal.Flag](x.store.Path("maintenance.flag"))
			if err != nil || flag.FencingGeneration != x.j.FencingGeneration+1 {
				t.Fatalf("recovery did not advance flag: %+v %v", flag, err)
			}
			*now = now.Add(time.Minute)
			writeRestartAck(t, x, pod.Metadata.UID)
			if phase != "same job" { // A new Job initially discovers the replacement.
				x.podName, x.podUID = pod.Metadata.Name, pod.Metadata.UID
				x.j.Phase = phase
				x.j.Lifecycle, x.j.SuspendReason = "SUSPENDED", ptr("sidecar-unavailable")
			}
			s := startupStatus(x, pod)
			calls := restartCommands(t, x, pod, &s, nil)
			if err := x.b8Rollout(); err != nil {
				t.Fatal(err)
			}
			if x.podName != pod.Metadata.Name || x.podUID != pod.Metadata.UID || x.j.ExpectedPodUID != pod.Metadata.UID || *x.j.PreviousPodUID != previous || x.j.ReplacementPodUID == nil || *x.j.ReplacementPodUID != pod.Metadata.UID {
				t.Fatalf("identity not adopted safely: %+v", x.j)
			}
			flag, err = journal.Read[journal.Flag](x.store.Path("maintenance.flag"))
			if err != nil || flag.FencingGeneration != x.j.FencingGeneration {
				t.Fatalf("flag not synchronized: %+v %v", flag, err)
			}
			ack, err := journal.Read[gate.Status](x.store.Path("gate-status.json"))
			if err != nil || !ack.Active(gate.Identity{PodUID: x.podUID, TransactionID: x.j.TransactionID, Generation: x.j.FencingGeneration}, x.now()) {
				t.Fatalf("replacement closure not acknowledged: %+v %v", ack, err)
			}
			if err := x.reactivateSuspended(); err != nil {
				t.Fatal(err)
			}
			x.j.Phase = "VERIFYING"
			if err := x.saveJournal(); err != nil {
				t.Fatal(err)
			}
			if err := x.b9Healthy(x.plan); err != nil {
				t.Fatal(err)
			}
			if x.j.Lifecycle != "ACTIVE" {
				t.Fatalf("unexpected suspended journal: %+v", x.j)
			}
			monitorCalls := 0
			for _, call := range *calls {
				if slices.Contains(call.args, "mc-monitor") {
					monitorCalls++
					if !slices.Contains(call.args, "-json") {
						t.Fatal("B9 used text monitor")
					}
				}
			}
			if monitorCalls != 1 {
				t.Fatalf("live monitor calls=%d", monitorCalls)
			}
		})
	}
}

func TestB8WaitsForCompletePodAndClosure(t *testing.T) {
	x, _, pod, now := restartFixture(t)
	s := startupStatus(x, pod)
	restartCommands(t, x, pod, &s, nil)
	pod.Status.Containers = pod.Status.Containers[:1]
	polls := 0
	restartWait = func(context.Context, time.Duration) error {
		polls++
		*now = now.Add(time.Second)
		if x.j.ReplacementPodUID != nil {
			t.Fatal("adopted replacement before complete readiness and fresh closure")
		}
		switch polls {
		case 1:
			if err := json.Unmarshal([]byte(`{"status":{"phase":"Running","containerStatuses":[{"name":"minecraft","ready":true},{"name":"updater","ready":true}]}}`), pod); err != nil {
				t.Fatal(err)
			}
		case 2:
			writeRestartAck(t, x, pod.Metadata.UID)
		default:
			t.Fatal("failed to advance after closure became ready")
		}
		return nil
	}
	if err := x.b8Rollout(); err != nil {
		t.Fatal(err)
	}
	if polls != 2 {
		t.Fatalf("polls=%d, want readiness wait then closure wait", polls)
	}
}

func TestB8RejectsUnrelatedRestartAndCorruptPair(t *testing.T) {
	for _, which := range []string{"transaction", "request time", "jar", "stale ack", "lost lease"} {
		t.Run(which, func(t *testing.T) {
			x, data, pod, now := restartFixture(t)
			writeRestartAck(t, x, pod.Metadata.UID)
			s := startupStatus(x, pod)
			restartCommands(t, x, pod, &s, nil)
			before, err := os.ReadFile(x.store.Path("maintenance.flag"))
			if err != nil {
				t.Fatal(err)
			}
			switch which {
			case "transaction":
				pod.Metadata.Annotations["lepinoid.dev/restart-transaction"] = "other"
			case "request time":
				pod.Metadata.Annotations["lepinoid.dev/restart-requested-at"] = "old"
			case "jar":
				if err := os.WriteFile(filepath.Join(data, "plugins", "LepinoidTools.jar"), []byte("corrupt"), 0644); err != nil {
					t.Fatal(err)
				}
			case "stale ack":
				*now = now.Add(time.Minute)
			case "lost lease":
				x.engine.Fence.Lease = func(context.Context) error { return updaterengine.ErrFenced }
			}
			restartWait = func(context.Context, time.Duration) error { *now = now.Add(11 * time.Minute); return nil }
			if err := x.b8Rollout(); err == nil {
				t.Fatal("invalid replacement accepted")
			}
			if x.j.ReplacementPodUID != nil {
				t.Fatal("invalid replacement persisted")
			}
			after, err := os.ReadFile(x.store.Path("maintenance.flag"))
			if err != nil || string(before) != string(after) {
				t.Fatalf("closure flag changed: %v", err)
			}
		})
	}
}

func TestB9RejectsStaleIdentityManifestAndFailedMonitor(t *testing.T) {
	for _, which := range []string{"old pod", "version", "commit", "unsupported minecraft", "monitor version mismatch", "monitor failure", "pod name reused"} {
		t.Run(which, func(t *testing.T) {
			x, _, pod, now := restartFixture(t)
			*now = now.Add(time.Minute)
			writeRestartAck(t, x, pod.Metadata.UID)
			s := startupStatus(x, pod)
			restartCommands(t, x, pod, &s, nil)
			if err := x.b8Rollout(); err != nil {
				t.Fatal(err)
			}
			var monitorErr error
			switch which {
			case "old pod":
				s.ServerInstanceID = *x.j.PreviousPodUID
			case "version":
				s.PluginVersion = "other"
			case "commit":
				s.PluginCommitSHA = strings.Repeat("e", 40)
			case "unsupported minecraft":
				s.MinecraftVersion = "1.21.1"
			case "monitor version mismatch":
				s.MinecraftVersion = "1.21.9"
				x.plan.Desired.SupportedMinecraft = []string{"1.21.8", "1.21.9"}
			case "monitor failure":
				monitorErr = errors.New("connection refused")
			case "pod name reused":
				pod.Metadata.UID = "unrelated-uid"
			}
			calls := restartCommands(t, x, pod, &s, monitorErr)
			restartWait = func(context.Context, time.Duration) error { *now = now.Add(11 * time.Minute); return nil }
			if err := x.b9Healthy(x.plan); err == nil {
				t.Fatal("unhealthy startup accepted")
			}
			if x.j.Lifecycle != "SUSPENDED" {
				t.Fatalf("failure did not suspend: %+v", x.j)
			}
			if which == "pod name reused" {
				for _, call := range *calls {
					if slices.Contains(call.args, "exec") {
						t.Fatalf("exec reached unrelated pod: %q", call.args)
					}
				}
			}
		})
	}
}

// Resume enters through the production dispatcher, including restart closure,
// health, whitelist restoration, current commit, recovery archive, and GC.
func TestRecoverEntryCompletesRestartTransaction(t *testing.T) {
	for _, phase := range []string{"RESTART_REQUESTED", "VERIFYING"} {
		for _, runtimeEnabled := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/runtime=%t", phase, runtimeEnabled), func(t *testing.T) {
				x, data, pod, now := restartFixture(t)
				ctx, cancel := context.WithTimeout(x.ctx, 5*time.Second)
				defer cancel()
				x.ctx = ctx
				previousPodUID := *x.j.PreviousPodUID
				persistedEnabled := !runtimeEnabled
				whitelist := []byte(`[{"uuid":"550e8400-e29b-41d4-a716-446655440001","name":"first"},{"uuid":"550e8400-e29b-41d4-a716-446655440002","name":"second"}]`)
				x.j.Maintenance.WhitelistExisted = ptr(true)
				x.j.Maintenance.WhitelistBackup = ptr(base64.StdEncoding.EncodeToString(whitelist))
				x.j.Maintenance.WhitelistChecksum = ptr(testSHA256(whitelist))
				x.j.Maintenance.PersistedEnabled = ptr(persistedEnabled)
				x.j.Maintenance.RuntimeEnabled = ptr(runtimeEnabled)
				x.j.Phase = phase
				x.j.Lifecycle, x.j.SuspendReason = "SUSPENDED", ptr("sidecar-unavailable")
				if err := x.saveJournal(); err != nil {
					t.Fatal(err)
				}
				if x.dataPath("server.properties") != filepath.Join(data, "server.properties") {
					t.Fatal("fixture escaped temporary data root")
				}
				if err := writePersistedWhitelist(x.dataPath("server.properties"), persistedEnabled); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(x.dataPath("whitelist.json"), []byte("[]"), 0644); err != nil {
					t.Fatal(err)
				}
				sourceCurrent := journal.Current{Manifest: x.plan.Desired, InstalledAt: now.Add(-time.Hour)}
				sourceCurrent.Digest, sourceCurrent.Version = x.j.SourceDigest, "source-version"
				x.plan.Current = sourceCurrent
				if err := journal.Save(x.store.Path("current"), sourceCurrent); err != nil {
					t.Fatal(err)
				}

				if err := (recovery.Runner{Store: x.store, Data: data, PodUID: pod.Metadata.UID, Now: x.now}).Run(); err != nil {
					t.Fatal(err)
				}
				flag, err := journal.Read[journal.Flag](x.store.Path("maintenance.flag"))
				if err != nil || flag.FencingGeneration != x.j.FencingGeneration+1 {
					t.Fatalf("init-recover did not advance closure: %+v %v", flag, err)
				}
				*now = now.Add(time.Minute)
				// A resumed Job discovers the replacement before gate-status arrives.
				// B8 must wait and synchronize both the recovery and journal generations.
				x.podName, x.podUID = pod.Metadata.Name, pod.Metadata.UID
				resume, err := journal.Read[journal.Journal](x.store.Path("journal", x.j.TransactionID))
				if err != nil {
					t.Fatal(err)
				}
				s := startupStatus(x, pod)
				calls := restartCommands(t, x, pod, &s, nil)
				base := x.commands.(fakeRunner)
				var rconCommands []string
				runtimeNow := true // init-recover starts Minecraft with whitelist on.
				gateReleased := make(chan error, 1)
				gateStarted := false
				t.Cleanup(func() {
					cancel()
					if gateStarted {
						select {
						case <-gateReleased:
						case <-time.After(time.Second):
							t.Error("fake gate watcher did not stop")
						}
					}
				})
				x.commands = fakeRunner{calls: calls, reply: func(name string, args []string) ([]byte, error) {
					if name == "kubectl" && len(args) > 5 && args[0] == "exec" && (args[5] == "sh" || args[5] == "rcon-cli") {
						if args[1] != pod.Metadata.Name || args[3] != "minecraft" {
							return nil, fmt.Errorf("RCON targeted wrong replacement: %q", args)
						}
						if args[5] == "sh" {
							return []byte("test-password\n"), nil
						}
						if len(args) != 10 || !slices.Equal(args[5:9], []string{"rcon-cli", "--password", "test-password", "whitelist"}) {
							return nil, fmt.Errorf("unexpected RCON: %q", args)
						}
						action := args[9]
						rconCommands = append(rconCommands, action)
						// Gate release must follow both disk and runtime restoration.
						activeFlag, err := journal.Read[journal.Flag](x.store.Path("maintenance.flag"))
						if err != nil {
							return nil, err
						}
						if activeFlag.FencingGeneration != x.j.FencingGeneration {
							return nil, errors.New("RCON ran before replacement closure synchronization")
						}
						restored, err := os.ReadFile(x.dataPath("whitelist.json"))
						if err != nil || string(restored) != string(whitelist) {
							return nil, errors.New("RCON ran before whitelist file restoration")
						}
						if action == "reload" {
							return []byte("Reloaded the whitelist"), nil
						}
						if action != "on" && action != "off" {
							return nil, fmt.Errorf("unexpected whitelist action %q", action)
						}
						runtimeNow = action == "on"
						if err := writePersistedWhitelist(x.dataPath("server.properties"), runtimeNow); err != nil {
							return nil, err
						}
						if gateStarted {
							return nil, errors.New("runtime whitelist restored twice")
						}
						gateStarted = true
						// Only the filesystem is shared with this watcher. The captured
						// gate identity/time cannot race with mutable transaction state.
						ack := gate.Status{SchemaVersion: 1, Version: "1", ServerInstanceID: pod.Metadata.UID, Phase: "INACTIVE", ReleasedTransaction: ptr(activeFlag.TransactionID), ReleasedGeneration: ptr(activeFlag.FencingGeneration), UpdatedAt: *now}
						flagPath, ackPath := x.store.Path("maintenance.flag"), x.store.Path("gate-status.json")
						go func() {
							ticker := time.NewTicker(time.Millisecond)
							defer ticker.Stop()
							for {
								if _, err := os.Lstat(flagPath); errors.Is(err, os.ErrNotExist) {
									gateReleased <- journal.Save(ackPath, ack)
									return
								} else if err != nil {
									gateReleased <- err
									return
								}
								select {
								case <-ctx.Done():
									gateReleased <- ctx.Err()
									return
								case <-ticker.C:
								}
							}
						}()
						return []byte("Whitelist is now turned " + action), nil
					}
					return base.reply(name, args)
				}}

				if err := x.recoverEntry(resume); err != nil {
					t.Fatal(err)
				}
				if !gateStarted {
					t.Fatal("resume did not reach runtime restoration")
				}
				select {
				case err := <-gateReleased:
					gateStarted = false
					if err != nil {
						t.Fatal(err)
					}
				case <-ctx.Done():
					t.Fatal("gate release did not finish")
				}
				wantAction := "off"
				if runtimeEnabled {
					wantAction = "on"
				}
				if !slices.Equal(rconCommands, []string{"reload", wantAction}) || runtimeNow != runtimeEnabled {
					t.Fatalf("runtime restoration: calls=%v enabled=%t", rconCommands, runtimeNow)
				}
				persisted, err := readPersistedWhitelist(x.dataPath("server.properties"))
				if err != nil || persisted != persistedEnabled {
					t.Fatalf("persisted whitelist not restored independently: %t %v", persisted, err)
				}
				restored, err := os.ReadFile(x.dataPath("whitelist.json"))
				if err != nil || string(restored) != string(whitelist) {
					t.Fatalf("whitelist not restored: %s %v", restored, err)
				}
				current, err := journal.Read[journal.Current](x.store.Path("current"))
				if err != nil || !journal.SameIdentity(current.Manifest, x.j.TargetManifest) {
					t.Fatalf("target current not committed: %+v %v", current, err)
				}
				archived, err := journal.Read[journal.Journal](x.store.Path("archive", x.j.TransactionID))
				if err != nil {
					t.Fatal(err)
				}
				if archived.Lifecycle != "TERMINAL" || archived.Outcome == nil || *archived.Outcome != "SUCCEEDED" || archived.AccessState != "OPEN" || archived.Phase != "ACCESS_RESTORE_COMPLETE" || archived.MaintenanceRequired || archived.ExpectedPodUID != pod.Metadata.UID || archived.PreviousPodUID == nil || *archived.PreviousPodUID != previousPodUID {
					t.Fatalf("incorrect terminal archive: %+v", archived)
				}
				inv, err := x.store.Inspect()
				if err != nil || inv.Blocked || len(inv.Journals) != 0 || len(inv.Recovery) != 0 || inv.Flag != nil {
					t.Fatalf("unfinished inventory after success: %+v %v", inv, err)
				}
				entries, err := os.ReadDir(x.store.Path("archive"))
				if err != nil {
					t.Fatal(err)
				}
				recoveryArchived := false
				for _, entry := range entries {
					if strings.HasPrefix(entry.Name(), x.j.TransactionID+".recovery-") {
						recoveryArchived = true
					}
				}
				if !recoveryArchived {
					t.Fatal("init-recovery record was not archived")
				}
				ack, err := journal.Read[gate.Status](x.store.Path("gate-status.json"))
				if err != nil || !ack.Inactive(gate.Identity{PodUID: pod.Metadata.UID, TransactionID: x.j.TransactionID, Generation: x.j.FencingGeneration}, x.now()) {
					t.Fatalf("success without matching INACTIVE acknowledgement: %+v %v", ack, err)
				}
			})
		}
	}
}
