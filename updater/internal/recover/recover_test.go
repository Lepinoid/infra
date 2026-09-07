package recover

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
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
