package updater

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
)

func TestGCProtectionAndInterruptedDelete(t *testing.T) {
	now := time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)
	p := GCPlan{Protected: []string{"current", "desired", "source"}, Generations: []Generation{{"current", now}, {"desired", now}, {"source", now}, {"recent1", now}, {"recent2", now.Add(-time.Hour)}, {"old1", now.Add(-2 * time.Hour)}, {"old2", now.Add(-3 * time.Hour)}}}
	removed := []string{}
	stop := errors.New("kill")
	err := p.Run(context.Background(), func(context.Context) error { return nil }, func(digest string) error { removed = append(removed, digest); return stop })
	if !errors.Is(err, stop) || len(removed) != 1 || !slices.Contains([]string{"old1", "old2"}, removed[0]) {
		t.Fatalf("%v %v", removed, err)
	}
}

func TestBlockingSuppressesGC(t *testing.T) {
	p := GCPlan{Blocked: true, Generations: []Generation{{Digest: "old"}}}
	if err := p.Run(context.Background(), func(context.Context) error { t.Fatal("fencing invoked"); return nil }, func(string) error { t.Fatal("delete invoked"); return nil }); !errors.Is(err, ErrBlocked) {
		t.Fatal(err)
	}
}
