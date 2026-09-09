package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/lepinoid/infra/updater/internal/cluster"
	"github.com/lepinoid/infra/updater/internal/fsutil"
	"github.com/lepinoid/infra/updater/internal/journal"
	"github.com/lepinoid/infra/updater/internal/status"
	updaterengine "github.com/lepinoid/infra/updater/internal/updater"
)

type runnerCall struct {
	name string
	args []string
}

type fakeRunner struct {
	calls *[]runnerCall
	reply func(string, []string) ([]byte, error)
}

var _ cluster.Runner = fakeRunner{}

func (f fakeRunner) Run(_ context.Context, _ []byte, name string, args ...string) ([]byte, error) {
	*f.calls = append(*f.calls, runnerCall{name, slices.Clone(args)})
	return f.reply(name, args)
}

func (f fakeRunner) Kubectl(ctx context.Context, input []byte, args ...string) ([]byte, error) {
	return f.Run(ctx, input, "kubectl", args...)
}

func (f fakeRunner) Exec(ctx context.Context, pod cluster.Pod, container string, args ...string) ([]byte, error) {
	return f.Kubectl(ctx, nil, append([]string{"exec", pod.Name, "-c", container, "--"}, args...)...)
}

func TestMCVersionGate(t *testing.T) {
	for _, tc := range []struct {
		name, version, monitor, reason, resume                                     string
		stale, wrongPod, badSchema, monitorError, statusError, badStatus, fallback bool
	}{
		{name: "supported", version: "1.21.8"},
		{name: "unsupported", version: "1.21.1", reason: "mc-version-mismatch"},
		{name: "disagreement", version: "1.21.1", monitor: "1.21.8", reason: "mc-version-unknown"},
		{name: "stale", version: "1.21.8", stale: true, reason: "mc-version-unknown"},
		{name: "instance mismatch", version: "1.21.8", wrongPod: true, reason: "mc-version-unknown"},
		{name: "monitor exit", version: "1.21.8", monitorError: true, reason: "mc-version-unknown"},
		{name: "monitor invalid JSON", version: "1.21.8", monitor: "invalid", reason: "mc-version-unknown"},
		{name: "status exit", version: "1.21.8", statusError: true, reason: "mc-version-unknown"},
		{name: "status invalid JSON", version: "1.21.8", badStatus: true, reason: "mc-version-unknown"},
		{name: "status schema", version: "1.21.8", badSchema: true, reason: "mc-version-unknown"},
		{name: "status fallback", version: "1.21.8", fallback: true},
		{name: "resume mismatch", version: "1.21.8", resume: "mc-version-mismatch"},
		{name: "resume unknown", version: "1.21.8", resume: "mc-version-unknown"},
		{name: "still mismatch", version: "1.21.1", resume: "mc-version-mismatch", reason: "mc-version-mismatch"},
		{name: "still unknown", version: "1.21.8", stale: true, resume: "mc-version-unknown", reason: "mc-version-unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Given: a real journal store and read-only server observations.
			x := gateTransaction(t)
			if tc.resume != "" {
				x.j.Lifecycle, x.j.SuspendReason = "SUSPENDED", ptr(tc.resume)
				x.engine.Journal = x.j
				if err := journal.Save(x.store.Path("journal", x.j.TransactionID), x.j); err != nil {
					t.Fatal(err)
				}
			}
			s := status.Status{SchemaVersion: 1, ServerInstanceID: x.podUID, MinecraftVersion: tc.version, UpdatedAt: x.now()}
			if tc.stale {
				s.UpdatedAt = s.UpdatedAt.Add(-time.Hour)
			}
			if tc.wrongPod {
				s.ServerInstanceID = "other-pod"
			}
			if tc.badSchema {
				s.SchemaVersion = 2
			}
			statusJSON, err := json.Marshal(s)
			if err != nil {
				t.Fatal(err)
			}
			if tc.badStatus {
				statusJSON = []byte("{")
			}
			version := tc.monitor
			if version == "" {
				version = tc.version
			}
			monitorJSON := []byte(fmt.Sprintf(`{"server_info":{"version":{"name":%q}}}`, version))
			if tc.monitor == "invalid" {
				monitorJSON = []byte("{")
			}
			var calls []runnerCall
			fetchReached := false
			x.commands = fakeRunner{calls: &calls, reply: func(name string, args []string) ([]byte, error) {
				if name == "oras" && slices.Equal(args, []string{"blob", "fetch", x.plan.Desired.OCIRepository + "@" + x.plan.Desired.Digest, "--output", "-"}) {
					fetchReached = true
					return nil, errors.New("stop at staging boundary")
				}
				monitorArgs := []string{"exec", x.podName, "-c", "minecraft", "--", "mc-monitor", "status", "--host", "localhost", "--port", "25565", "--format", "json"}
				if name == "kubectl" && slices.Equal(args, monitorArgs) {
					if tc.monitorError {
						return nil, errors.New("monitor exited 1")
					}
					return monitorJSON, nil
				}
				for _, container := range []string{"updater", "minecraft"} {
					if name == "kubectl" && slices.Equal(args, []string{"exec", x.podName, "-c", container, "--", "cat", "/data/plugins/LepinoidTools/updater-status.json"}) {
						if tc.statusError || (tc.fallback && container == "updater") {
							return nil, errors.New("status unavailable")
						}
						return statusJSON, nil
					}
				}
				t.Errorf("unexpected command: %s %q", name, args)
				return nil, errors.New("unexpected command")
			}}
			before := gateFileHashes(t, x.store)

			// When: dispatch the actual transaction, including the recovery entry.
			if tc.resume != "" {
				err = x.recoverEntry(x.j)
			} else {
				err = x.runActive()
			}

			// Then: rejection is persisted without staging or mutation; acceptance reaches a4Stage.
			if tc.reason == "" {
				if !fetchReached || !errors.Is(err, errAPIUnavailable) {
					t.Fatalf("staging not reached: calls=%+v err=%v", calls, err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				for _, call := range calls {
					if call.name == "oras" || slices.Contains(call.args, "patch") {
						t.Fatalf("mutation command: %+v", call)
					}
				}
				if _, err := os.Stat(x.store.Path("staging", x.plan.Desired.Digest)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("staging touched: %v", err)
				}
			}
			if after := gateFileHashes(t, x.store); !reflect.DeepEqual(before, after) {
				t.Fatalf("files changed: before=%v after=%v", before, after)
			}
			persisted, err := journal.Read[journal.Journal](x.store.Path("journal", x.j.TransactionID))
			if err != nil {
				t.Fatal(err)
			}
			for _, j := range []journal.Journal{x.j, x.engine.Journal, persisted} {
				if err := j.Validate(); err != nil {
					t.Fatal(err)
				}
				if tc.reason == "" {
					if j.Lifecycle != "ACTIVE" || j.SuspendReason != nil {
						t.Fatalf("not active: %+v", j)
					}
				} else if j.Lifecycle != "SUSPENDED" || j.SuspendReason == nil || *j.SuspendReason != tc.reason {
					t.Fatalf("wrong suspension: %+v", j)
				}
				if j.Phase != "PREPARING" || j.AccessState != "OPEN" || j.MaintenanceRequired {
					t.Fatalf("advanced past gate: %+v", j)
				}
			}
		})
	}
}

func gateTransaction(t *testing.T) *tx {
	t.Helper()
	j, err := journal.Read[journal.Journal]("../build-server/contract-fixtures/infra/journal-active.json")
	if err != nil {
		t.Fatal(err)
	}
	j.Phase, j.AccessState, j.MaintenanceRequired = "PREPARING", "OPEN", false
	j.TargetManifest.SupportedMinecraft = []string{"1.21.8"}
	store := journal.Store{Root: filepath.Join(t.TempDir(), ".lepinoid")}
	if err := store.Init(); err != nil {
		t.Fatal(err)
	}
	now := func() time.Time { return time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC) }
	x := &tx{ctx: context.Background(), store: store, now: now, podName: "build-server-test", podUID: j.ExpectedPodUID, j: j, plan: &manifestPlan{Desired: j.TargetManifest}}
	x.engine = &updaterengine.Engine{Store: store, Journal: j, Now: now, Fence: updaterengine.Fence{
		Lease: func(context.Context) error { return nil },
		Observe: func(context.Context) (updaterengine.Observation, error) {
			return updaterengine.Observation{PodUID: j.ExpectedPodUID, Generation: j.FencingGeneration}, nil
		},
	}}
	if err := x.saveJournal(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{store.Path("maintenance.flag"), filepath.Join(filepath.Dir(store.Root), "LepinoidTools.jar"), filepath.Join(filepath.Dir(store.Root), "Multiverse-Core.jar")} {
		if err := os.WriteFile(path, []byte("original "+filepath.Base(path)), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return x
}

func gateFileHashes(t *testing.T, store journal.Store) map[string]string {
	t.Helper()
	hashes := make(map[string]string)
	for _, path := range []string{store.Path("maintenance.flag"), filepath.Join(filepath.Dir(store.Root), "LepinoidTools.jar"), filepath.Join(filepath.Dir(store.Root), "Multiverse-Core.jar")} {
		sum, err := fsutil.SHA256(path)
		if err != nil {
			t.Fatal(err)
		}
		hashes[path] = sum
	}
	return hashes
}
