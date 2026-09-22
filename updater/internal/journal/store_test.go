package journal

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBlockingDirectoryIsSeparate(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "journal/.blocking"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "journal/.blocking/record"), []byte(`{}`), 0644); err != nil {
		t.Fatal(err)
	}
	got, err := (Store{Root: root}).Inspect()
	if err != nil {
		t.Fatal(err)
	}
	if !got.Blocked || len(got.Journals) != 0 {
		t.Fatal(got)
	}
}

func testJournal() Journal {
	target := Manifest{SchemaVersion: 1, Version: "v1", OCIRepository: "ghcr.io/lepinoid/lepinoid-tools", Digest: "sha256:" + strings.Repeat("a", 64), PluginCommitSHA: strings.Repeat("b", 40), ToolsSHA: strings.Repeat("c", 64), MultiverseSHA: strings.Repeat("d", 64), MultiverseVersion: "m1", SupportedMinecraft: []string{"1.21.8"}}
	candidate := "TARGET"
	return Journal{SchemaVersion: 1, TransactionID: "550e8400-e29b-41d4-a716-446655440000", Phase: "PREPARING", Lifecycle: "ACTIVE", AccessState: "OPEN", FencingGeneration: 0, ExpectedPodUID: "pod-uid", TargetManifest: target, SourceDigest: "sha256:" + strings.Repeat("e", 64), TargetDigest: target.Digest, Source: Pair{Tools: Jar{Name: "LepinoidTools.jar", SHA256: target.ToolsSHA}, Multiverse: Jar{Name: "Multiverse-Core.jar", SHA256: target.MultiverseSHA}}, Target: Pair{Tools: Jar{Name: "LepinoidTools.jar", SHA256: target.ToolsSHA}, Multiverse: Jar{Name: "Multiverse-Core.jar", SHA256: target.MultiverseSHA}}, StagingPath: "staging/" + target.Digest, BackupPath: "backup/" + "sha256:" + strings.Repeat("e", 64), CommitCandidate: &candidate}
}

// Given: a valid journal plus a crashed tmp write (fsutil ".atomic-" residue)
// When: Inspect scans the journal directory
// Then: the residue is ignored and the valid journal is returned
func TestInspectSkipsAtomicTempLeftovers(t *testing.T) {
	root := t.TempDir()
	store := Store{Root: root}
	if err := os.MkdirAll(filepath.Join(root, "journal"), 0755); err != nil {
		t.Fatal(err)
	}
	j := testJournal()
	if err := Save(store.Path("journal", j.TransactionID), j); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.Path("journal", ".atomic-123456"), nil, 0644); err != nil {
		t.Fatal(err)
	}
	got, err := store.Inspect()
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Journals) != 1 || got.Journals[0].TransactionID != j.TransactionID {
		t.Fatal(got.Journals)
	}
}

// Given: a schema-invalid journal file
// When: Inspect fails on it
// Then: the error identifies the offending file for diagnosis
func TestInspectErrorNamesFile(t *testing.T) {
	root := t.TempDir()
	store := Store{Root: root}
	if err := os.MkdirAll(filepath.Join(root, "journal"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.Path("journal", "broken"), []byte(`{"schemaVersion":1,`), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := store.Inspect()
	if err == nil || !strings.Contains(err.Error(), "broken") {
		t.Fatalf("error must name the offending entry: %v", err)
	}
}
