package recover

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lepinoid/infra/updater/internal/journal"
)

func TestMaintenanceRecoveryClosesBeforeStartup(t *testing.T) {
	data := t.TempDir()
	s := journal.Store{Root: filepath.Join(data, "plugins/.lepinoid")}
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte("jar"))
	checksum := hex.EncodeToString(hash[:])
	for _, name := range []string{"LepinoidTools.jar", "Multiverse-Core.jar", "lepinoid-tools-gate.jar"} {
		if err := os.WriteFile(filepath.Join(data, "plugins", name), []byte("jar"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := journal.Save(s.Path("gate-manifest.json"), GateManifest{1, "lepinoid-tools-gate.jar", "1", checksum, []string{"1.21.1"}}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "server.properties"), []byte("white-list=false\n"), 0644); err != nil {
		t.Fatal(err)
	}
	enabled := false
	j := journal.Journal{SchemaVersion: 1, TransactionID: "550e8400-e29b-41d4-a716-446655440000", Phase: "MAINTENANCE_PREPARED", Lifecycle: "ACTIVE", AccessState: "CLOSING", MaintenanceRequired: true, ExpectedPodUID: "old", Maintenance: journal.Maintenance{PersistedEnabled: &enabled}, Source: journal.Pair{Tools: journal.Jar{Name: "LepinoidTools.jar", SHA256: checksum}, Multiverse: journal.Jar{Name: "Multiverse-Core.jar", SHA256: checksum}}}
	now := time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)
	r := Runner{Store: s, Data: data, PodUID: "new", Now: func() time.Time { return now }}
	if err := r.Repair(j); err != nil {
		t.Fatal(err)
	}
	flag, err := journal.Read[journal.Flag](s.Path("maintenance.flag"))
	if err != nil {
		t.Fatal(err)
	}
	if flag.TransactionID != j.TransactionID || flag.FencingGeneration != 1 {
		t.Fatal(flag)
	}
	value, err := Whitelist(filepath.Join(data, "server.properties"))
	if err != nil || !value {
		t.Fatalf("%v %v", value, err)
	}
}

// Given: a consistent current pair and a zero-byte journal leftover
// When: Runner.Run executes the init-recover path
// Then: the leftover is quarantined and startup verification proceeds normally
func TestRunQuarantinesEmptyJournalLeftover(t *testing.T) {
	data := t.TempDir()
	s := journal.Store{Root: filepath.Join(data, "plugins/.lepinoid")}
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte("jar"))
	checksum := hex.EncodeToString(hash[:])
	for _, name := range []string{"LepinoidTools.jar", "Multiverse-Core.jar"} {
		if err := os.WriteFile(filepath.Join(data, "plugins", name), []byte("jar"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	current := journal.Current{Manifest: journal.Manifest{SchemaVersion: 1, Version: "2026.09.22-1.21.8-aaaaa", OCIRepository: "ghcr.io/lepinoid/lepinoid-tools", Digest: "sha256:" + strings.Repeat("ab", 32), PluginCommitSHA: strings.Repeat("0123456789", 4), ToolsSHA: checksum, MultiverseSHA: checksum, MultiverseVersion: "5.8.1", SupportedMinecraft: []string{"1.21.8"}}, InstalledAt: time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC)}
	if err := journal.Save(s.Path("current"), current); err != nil {
		t.Fatal(err)
	}
	const orphan = "11111111-2222-3333-4444-555555555555"
	if err := os.WriteFile(s.Path("journal", orphan), nil, 0644); err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	r := Runner{Store: s, Data: data, PodUID: "pod-new", Now: func() time.Time { return now }, Log: &log}
	if err := r.Run(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.Path("journal", orphan)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("zero-byte journal not quarantined: %v", err)
	}
	quarantined, err := os.ReadDir(s.Path("quarantine"))
	if err != nil || len(quarantined) != 1 {
		t.Fatalf("quarantine entries: %v %v", quarantined, err)
	}
	data2, err := os.ReadFile(s.Path("quarantine", quarantined[0].Name()))
	if err != nil || len(data2) != 0 {
		t.Fatalf("quarantined content changed: %v %d", err, len(data2))
	}
	if !strings.Contains(log.String(), orphan) {
		t.Fatalf("quarantine not diagnosed: %q", log.String())
	}
}

// Given: a non-empty schema-invalid journal that cannot decode
// When: Runner.Run executes the init-recover path
// Then: init-unrecoverable carries the corrupted file name
func TestRunDiagnosesSchemaInvalidJournal(t *testing.T) {
	data := t.TempDir()
	s := journal.Store{Root: filepath.Join(data, "plugins/.lepinoid")}
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	const broken = "22222222-3333-4444-5555-666666666666"
	if err := os.WriteFile(s.Path("journal", broken), []byte(`{"schemaVersion":1,"transactionId":`), 0644); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	var log bytes.Buffer
	r := Runner{Store: s, Data: data, PodUID: "pod-new", Now: func() time.Time { return now }, Log: &log}
	err := r.Run()
	if !errors.Is(err, ErrUnrecoverable) {
		t.Fatalf("expected init-unrecoverable, got %v", err)
	}
	if !strings.Contains(err.Error(), broken) {
		t.Fatalf("diagnostic lacks file name: %v", err)
	}
	if _, statErr := os.Stat(s.Path("journal", broken)); statErr != nil {
		t.Fatalf("non-empty invalid journal must not be auto-quarantined: %v", statErr)
	}
}
