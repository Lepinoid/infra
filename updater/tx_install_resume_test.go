package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lepinoid/infra/updater/internal/artifact"
	"github.com/lepinoid/infra/updater/internal/fsutil"
	"github.com/lepinoid/infra/updater/internal/gate"
	"github.com/lepinoid/infra/updater/internal/journal"
	updaterengine "github.com/lepinoid/infra/updater/internal/updater"
)

func installResumeFixture(t *testing.T) (*tx, stageFixture) {
	t.Helper()
	fx := newStageFixture(t)
	x := stageTransaction(t, fx.manifest)
	j, err := journal.Read[journal.Journal]("../build-server/contract-fixtures/infra/journal-active.json")
	if err != nil {
		t.Fatal(err)
	}
	j.Phase, j.CommitCandidate, j.FencingGeneration = "INSTALL_STARTED", nil, 0
	j.TargetManifest, j.TargetDigest, j.Target = fx.manifest, fx.manifest.Digest, sourcePairFromManifest(fx.manifest)
	j.SourceDigest = "sha256:" + strings.Repeat("cd", 32)
	j.StagingPath, j.BackupPath = filepath.Join("staging", j.TargetDigest), filepath.Join("backup", j.SourceDigest)
	sourceTools, sourceMultiverse := []byte("original tools jar"), []byte("original multiverse jar")
	j.Source = journal.Pair{Tools: journal.Jar{Name: "LepinoidTools.jar", SHA256: testSHA256(sourceTools)}, Multiverse: journal.Jar{Name: "Multiverse-Core.jar", SHA256: testSHA256(sourceMultiverse)}}
	x.j, x.engine.Journal = j, j
	currentManifest := fx.manifest
	currentManifest.Digest, currentManifest.ToolsSHA, currentManifest.MultiverseSHA = j.SourceDigest, j.Source.Tools.SHA256, j.Source.Multiverse.SHA256
	x.plan.Current = journal.Current{Manifest: currentManifest, InstalledAt: x.now()}
	if err := journal.Save(x.store.Path("current"), x.plan.Current); err != nil {
		t.Fatal(err)
	}
	backup := x.store.Path("backup", j.SourceDigest)
	if err := os.MkdirAll(backup, 0755); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{"LepinoidTools.jar": sourceTools, "Multiverse-Core.jar": sourceMultiverse} {
		for _, dir := range []string{filepath.Dir(x.store.Root), backup} {
			if err := os.WriteFile(filepath.Join(dir, name), data, 0644); err != nil {
				t.Fatal(err)
			}
		}
	}
	stage := x.store.Path("staging", fx.manifest.Digest)
	if err := os.MkdirAll(stage, 0755); err != nil {
		t.Fatal(err)
	}
	if err := artifact.Extract(bytes.NewReader(fx.tarBytes), stage); err != nil {
		t.Fatal(err)
	}
	if err := x.saveJournal(); err != nil {
		t.Fatal(err)
	}
	if err := x.engine.Flag(x.ctx); err != nil {
		t.Fatal(err)
	}
	ack := gate.Status{SchemaVersion: 1, ServerInstanceID: x.j.ExpectedPodUID, Phase: "ACTIVE", ObservedTransaction: ptr(x.j.TransactionID), ObservedGeneration: ptr(x.j.FencingGeneration), UpdatedAt: x.now()}
	if err := journal.Save(x.store.Path("gate-status.json"), ack); err != nil {
		t.Fatal(err)
	}
	x.commands = fakeRunner{calls: &[]runnerCall{}, reply: func(name string, args []string) ([]byte, error) {
		return nil, fmt.Errorf("unexpected network/cluster operation: %s %q", name, args)
	}}
	return x, fx
}

func reloadInstallTransaction(t *testing.T, x *tx) *tx {
	t.Helper()
	resumed := *x
	engine := *x.engine
	resumed.engine = &engine
	j, err := journal.Read[journal.Journal](x.store.Path("journal", x.j.TransactionID))
	if err != nil {
		t.Fatal(err)
	}
	resumed.j, resumed.engine.Journal = j, j
	return &resumed
}

func assertInstallResumePair(t *testing.T, x *tx, dir string, pair journal.Pair) {
	t.Helper()
	for _, jar := range []journal.Jar{pair.Tools, pair.Multiverse} {
		if err := fsutil.VerifyJar(filepath.Join(dir, jar.Name), jar.SHA256); err != nil {
			t.Fatal(err)
		}
	}
}

func assertInstallResumeOutcome(t *testing.T, x *tx) {
	t.Helper()
	disk, err := journal.Read[journal.Journal](x.store.Path("journal", x.j.TransactionID))
	if err != nil {
		t.Fatal(err)
	}
	for name, j := range map[string]journal.Journal{"tx": x.j, "engine": x.engine.Journal, "disk": disk} {
		if j.Phase != "INSTALL_COMPLETE" || j.CommitCandidate == nil || *j.CommitCandidate != "TARGET" || j.Lifecycle != "ACTIVE" || j.AccessState != "CLOSED" || !j.MaintenanceRequired {
			t.Fatalf("%s advanced without completed closed target install: %+v", name, j)
		}
	}
	assertInstallResumePair(t, x, filepath.Dir(x.store.Root), x.j.Target)
	assertInstallResumePair(t, x, x.store.Path("backup", x.j.SourceDigest), x.j.Source)
	current, err := journal.Read[journal.Current](x.store.Path("current"))
	if err != nil {
		t.Fatal(err)
	}
	if current.Digest != x.j.SourceDigest {
		t.Fatal("install advanced current before restart/health/access restoration")
	}
	if _, err := os.Stat(x.store.Path("maintenance.flag")); err != nil {
		t.Fatalf("install removed closure: %v", err)
	}
}

func TestInstallResumeAfterPhaseSavedBeforeFirstJar(t *testing.T) {
	x, _ := installResumeFixture(t)
	// A new Job reloads INSTALL_STARTED, after its phase write but before b6.
	resumed := reloadInstallTransaction(t, x)
	if err := resumed.resumeStartedInstall(resumed.plan); err != nil {
		t.Fatal(err)
	}
	assertInstallResumeOutcome(t, resumed)
}

func TestInstallResumeReplenishesStageAfterPartialMove(t *testing.T) {
	x, fx := installResumeFixture(t)
	interrupted := errors.New("job interrupted between jar moves")
	x.engine.Fence.Lease = func(context.Context) error {
		hash, err := fsutil.SHA256(filepath.Join(filepath.Dir(x.store.Root), "LepinoidTools.jar"))
		if err == nil && hash == x.j.Target.Tools.SHA256 {
			return interrupted
		}
		return nil
	}
	if err := x.resumeStartedInstall(x.plan); !errors.Is(err, interrupted) {
		t.Fatalf("first attempt did not stop between jars: %v", err)
	}
	persisted, err := journal.Read[journal.Journal](x.store.Path("journal", x.j.TransactionID))
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Phase != "INSTALL_STARTED" || persisted.CommitCandidate != nil {
		t.Fatalf("partial pair was committed: %+v", persisted)
	}
	if err := fsutil.VerifyJar(filepath.Join(filepath.Dir(x.store.Root), x.j.Target.Tools.Name), x.j.Target.Tools.SHA256); err != nil {
		t.Fatal(err)
	}
	if err := fsutil.VerifyJar(filepath.Join(filepath.Dir(x.store.Root), x.j.Source.Multiverse.Name), x.j.Source.Multiverse.SHA256); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(x.store.Path("staging", x.j.TargetDigest, x.j.Target.Tools.Name)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("first jar was not consumed from stage: %v", err)
	}
	// Same Pod/generation and fresh closure: retry may safely replay the same pair.
	x.engine.Fence.Lease = func(context.Context) error { return nil }
	descriptor := []byte(fmt.Sprintf(`{"digest":%q}`, fx.manifest.Digest))
	stub := orasStub(t, fx, descriptor, fx.manifestJSON, fx.tarBytes, nil)
	x.commands = stub
	resumed := reloadInstallTransaction(t, x)
	if err := resumed.resumeStartedInstall(resumed.plan); err != nil {
		t.Fatal(err)
	}
	if len(*stub.calls) != 3 {
		t.Fatalf("partial staging did not refetch pinned bundle: %+v", *stub.calls)
	}
	assertInstallResumeOutcome(t, resumed)
}

func TestInstallResumeAfterBothMovesBeforeJournalCommit(t *testing.T) {
	x, fx := installResumeFixture(t)
	if err := x.b6Install(x.plan); err != nil {
		t.Fatal(err)
	}
	descriptor := []byte(fmt.Sprintf(`{"digest":%q}`, fx.manifest.Digest))
	stub := orasStub(t, fx, descriptor, fx.manifestJSON, fx.tarBytes, nil)
	x.commands = stub
	resumed := reloadInstallTransaction(t, x)
	if err := resumed.resumeStartedInstall(resumed.plan); err != nil {
		t.Fatal(err)
	}
	if len(*stub.calls) != 3 {
		t.Fatal("fully consumed stage was not replenished")
	}
	assertInstallResumeOutcome(t, resumed)
}

func TestInstallResumeRefetchFailureDoesNotCommitOrMutateLivePair(t *testing.T) {
	x, fx := installResumeFixture(t)
	if err := fsutil.Move(x.store.Path("staging", x.j.TargetDigest, x.j.Target.Tools.Name), filepath.Join(filepath.Dir(x.store.Root), x.j.Target.Tools.Name)); err != nil {
		t.Fatal(err)
	}
	unavailable := errors.New("registry unavailable")
	x.commands = orasStub(t, fx, nil, nil, nil, unavailable)
	resumed := reloadInstallTransaction(t, x)
	if err := resumed.resumeStartedInstall(resumed.plan); !errors.Is(err, unavailable) {
		t.Fatalf("refetch failure lost: %v", err)
	}
	persisted, err := journal.Read[journal.Journal](x.store.Path("journal", x.j.TransactionID))
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Phase != "INSTALL_STARTED" || persisted.CommitCandidate != nil {
		t.Fatal("failed replenishment advanced journal")
	}
	if err := fsutil.VerifyJar(filepath.Join(filepath.Dir(x.store.Root), x.j.Target.Tools.Name), x.j.Target.Tools.SHA256); err != nil {
		t.Fatal(err)
	}
	if err := fsutil.VerifyJar(filepath.Join(filepath.Dir(x.store.Root), x.j.Source.Multiverse.Name), x.j.Source.Multiverse.SHA256); err != nil {
		t.Fatal(err)
	}
	assertInstallResumePair(t, x, x.store.Path("backup", x.j.SourceDigest), x.j.Source)
}

func TestInstallResumeRequiresPhaseClosureFenceAndPinnedTarget(t *testing.T) {
	for _, mode := range []string{"wrong phase", "suspended", "access open", "maintenance false", "lease lost", "canceled", "gate inactive", "target changed"} {
		t.Run(mode, func(t *testing.T) {
			x, _ := installResumeFixture(t)
			switch mode {
			case "wrong phase":
				x.j.Phase = "BACKUP_COMPLETE"
			case "suspended":
				x.j.Lifecycle = "SUSPENDED"
				x.j.SuspendReason = ptr("checkpoint-timeout")
			case "access open":
				x.j.AccessState = "OPEN"
			case "maintenance false":
				x.j.MaintenanceRequired = false
			case "lease lost":
				x.engine.Fence.Lease = func(context.Context) error { return updaterengine.ErrFenced }
			case "canceled":
				ctx, cancel := context.WithCancel(x.ctx)
				cancel()
				x.ctx = ctx
			case "gate inactive":
				if err := journal.Save(x.store.Path("gate-status.json"), gate.Status{SchemaVersion: 1, ServerInstanceID: x.j.ExpectedPodUID, Phase: "INACTIVE", UpdatedAt: x.now()}); err != nil {
					t.Fatal(err)
				}
			case "target changed":
				x.plan.Desired.Digest = "sha256:" + strings.Repeat("ef", 32)
			}
			if err := x.resumeStartedInstall(x.plan); err == nil {
				t.Fatal("unsafe replay was accepted")
			}
			assertInstallResumePair(t, x, filepath.Dir(x.store.Root), x.j.Source)
			disk, err := journal.Read[journal.Journal](x.store.Path("journal", x.j.TransactionID))
			if err != nil {
				t.Fatal(err)
			}
			if disk.Phase != "INSTALL_STARTED" || disk.CommitCandidate != nil {
				t.Fatal("rejected replay changed persisted journal")
			}
		})
	}
}

func TestInstallResumeDispatcherInstallsBeforeRestart(t *testing.T) {
	x, _ := installResumeFixture(t)
	resumed := reloadInstallTransaction(t, x)
	stop := errors.New("stop at restart request")
	resumed.commands = fakeRunner{calls: &[]runnerCall{}, reply: func(name string, args []string) ([]byte, error) {
		if name != "kubectl" || len(args) < 3 || args[0] != "patch" || args[1] != "deployment" {
			t.Fatalf("unexpected command before restart: %s %q", name, args)
		}
		assertInstallResumePair(t, resumed, filepath.Dir(resumed.store.Root), resumed.j.Target)
		return nil, stop
	}}
	if err := resumed.recoverEntry(resumed.j); !errors.Is(err, stop) {
		t.Fatalf("resume did not install before requesting restart: %v", err)
	}
	assertInstallResumeOutcome(t, resumed)
}
