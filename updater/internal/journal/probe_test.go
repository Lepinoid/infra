package journal

import (
	"testing"
	"time"
)

func TestProbeSeparatedFromJournals(t *testing.T) {
	s := Store{Root: t.TempDir()}
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	p := Probe{SchemaVersion: 1, TransactionID: "550e8400-e29b-41d4-a716-446655440000", Probe: "runtime-whitelist", StartedAt: time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)}
	if err := Save(s.Path("journal", p.TransactionID+".probe"), p); err != nil {
		t.Fatal(err)
	}
	inventory, err := s.Inspect()
	if err != nil {
		t.Fatal(err)
	}
	if len(inventory.Journals) != 0 || len(inventory.Probes) != 1 {
		t.Fatalf("%+v", inventory)
	}
	if !inventory.ProbeInterrupted {
		t.Fatal("incomplete probe not detected")
	}
}
