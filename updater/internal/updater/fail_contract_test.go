package updater

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lepinoid/infra/updater/internal/gate"
	"github.com/lepinoid/infra/updater/internal/journal"
)

func TestFailureReasonLiteralCoverage(t *testing.T) {
	allowed := map[string]bool{}
	for _, reason := range []string{"gate-ack-unrecoverable", "pod-or-pvc-unavailable", "init-unrecoverable", "invariant-violation", "whitelist-restore-failed", "persisted-whitelist-cas-conflict"} {
		allowed[reason] = true
	}
	found := map[string]bool{}
	err := filepath.WalkDir("../..", func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return walkErr
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		text := string(data)
		for _, marker := range []string{`.Fail(ctx, "`, `.Fail("`} {
			for search := text; ; {
				i := strings.Index(search, marker)
				if i < 0 {
					break
				}
				start := i + len(marker)
				end := start
				for end < len(search) && search[end] != '"' {
					end++
				}
				if end < len(search) {
					literal := search[start:end]
					if !strings.Contains(literal, "%") && !strings.Contains(literal, "\\") {
						found[literal] = true
					}
				}
				search = search[end+1:]
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) == 0 {
		t.Fatal("no Fail literal calls found")
	}
	for literal := range found {
		if !allowed[literal] {
			t.Errorf("Fail 呼び出しの文字列 %q は failureReason enum に含まれない", literal)
		}
	}
}

func TestOpenFailsToWhitelistRestoreFailed(t *testing.T) {
	store := journal.Store{Root: t.TempDir()}
	if err := store.Init(); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)
	txID := "550e8400-e29b-41d4-a716-446655440000"
	gen := int64(0)
	if err := journal.Save(store.Path("gate-status.json"), gate.Status{SchemaVersion: 1, Version: "fixture", ServerInstanceID: "pod", Phase: "ACTIVE", ObservedTransaction: &txID, ObservedGeneration: &gen, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	engine := &Engine{
		Store: store,
		Now:   func() time.Time { return now },
		Journal: journal.Journal{
			SchemaVersion:   1,
			TransactionID:   txID,
			Phase:           "ACCESS_RESTORE_STARTED",
			Lifecycle:       "ACTIVE",
			AccessState:     "CLOSED",
			ExpectedPodUID:  "pod",
			CommitCandidate: strPtr("TARGET"),
			TargetManifest: journal.Manifest{
				SchemaVersion:      1,
				Version:            "v",
				OCIRepository:      "ghcr.io/lepinoid/lepinoid-tools",
				Digest:             "sha256:" + strings.Repeat("0", 64),
				PluginCommitSHA:    strings.Repeat("0", 40),
				ToolsSHA:           strings.Repeat("a", 64),
				MultiverseSHA:      strings.Repeat("b", 64),
				MultiverseVersion:  "5.7.1",
				SupportedMinecraft: []string{"1.21.1"},
			},
			SourceDigest: "sha256:" + strings.Repeat("c", 64),
			TargetDigest: "sha256:" + strings.Repeat("0", 64),
			Source: journal.Pair{
				Tools:      journal.Jar{Name: "LepinoidTools.jar", SHA256: strings.Repeat("a", 64)},
				Multiverse: journal.Jar{Name: "Multiverse-Core.jar", SHA256: strings.Repeat("b", 64)},
			},
			Target: journal.Pair{
				Tools:      journal.Jar{Name: "LepinoidTools.jar", SHA256: strings.Repeat("a", 64)},
				Multiverse: journal.Jar{Name: "Multiverse-Core.jar", SHA256: strings.Repeat("b", 64)},
			},
			Maintenance: journal.Maintenance{
				RuntimeEnabled:   boolPtr(true),
				PersistedEnabled: boolPtr(true),
				WhitelistExisted: boolPtr(false),
				StartedAt:        timePtr(now),
			},
		},
		Fence: Fence{
			Lease:   func(context.Context) error { return nil },
			Observe: func(context.Context) (Observation, error) { return Observation{PodUID: "pod"}, nil },
		},
	}
	injected := errors.New("restore failed")
	if err := engine.Open(context.Background(), func(context.Context) error { return injected }); err == nil {
		t.Fatal("Open がエラーを返さない")
	}
	if engine.Journal.Lifecycle != "TERMINAL" || engine.Journal.Outcome == nil || *engine.Journal.Outcome != "FAILED_MANUAL_INTERVENTION" {
		t.Fatalf("outcome: %+v", engine.Journal)
	}
	if engine.Journal.FailureReason == nil || *engine.Journal.FailureReason != "whitelist-restore-failed" {
		t.Fatalf("failureReason: %+v", engine.Journal.FailureReason)
	}
	persisted, err := journal.Read[journal.Journal](store.Path("journal", txID))
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Outcome == nil || *persisted.Outcome != "FAILED_MANUAL_INTERVENTION" || persisted.FailureReason == nil || *persisted.FailureReason != "whitelist-restore-failed" {
		t.Fatalf("persisted: %+v", persisted)
	}
}

func boolPtr(v bool) *bool           { return &v }
func timePtr(t time.Time) *time.Time { return &t }
func strPtr(v string) *string        { return &v }
