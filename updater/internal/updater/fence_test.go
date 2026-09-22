package updater

import (
	"context"
	"strings"
	"testing"

	"github.com/lepinoid/infra/updater/internal/journal"
)

func TestFenceStopsRecoveryAndPodChange(t *testing.T) {
	j := journal.Journal{ExpectedPodUID: "pod", FencingGeneration: 2}
	for _, tc := range []struct {
		pod        string
		generation int64
		pending    bool
		want       bool
	}{{"pod", 2, false, true}, {"new", 2, false, false}, {"pod", 3, false, false}, {"pod", 2, true, false}} {
		f := Fence{Lease: func(context.Context) error { return nil }, Observe: func(context.Context) (Observation, error) {
			return Observation{PodUID: tc.pod, Generation: tc.generation, RecoveryPending: tc.pending}, nil
		}}
		if err := f.Check(context.Background(), j); (err == nil) != tc.want {
			t.Fatalf("%+v %v", tc, err)
		}
	}
}

// Given: a journal fenced by an external change (pod replacement, generation
// move, recovery in progress, blocking record)
// When: Fence.Check rejects a mutation
// Then: the error names the failed comparison with journal and observed values
func TestFenceExplainsMismatch(t *testing.T) {
	j := journal.Journal{ExpectedPodUID: "pod", FencingGeneration: 2}
	for _, tc := range []struct {
		name     string
		observe  Observation
		contains []string
	}{
		{name: "pod replaced", observe: Observation{PodUID: "replacement", Generation: 2}, contains: []string{"expectedPodUid", `"pod"`, `"replacement"`}},
		{name: "generation moved", observe: Observation{PodUID: "pod", Generation: 3}, contains: []string{"fencingGeneration", "2", "3"}},
		{name: "recovery pending", observe: Observation{PodUID: "pod", Generation: 2, RecoveryPending: true}, contains: []string{"recoveryPending"}},
		{name: "blocking record", observe: Observation{PodUID: "pod", Generation: 2, Blocked: true}, contains: []string{"blocking"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := Fence{Lease: func(context.Context) error { return nil }, Observe: func(context.Context) (Observation, error) { return tc.observe, nil }}
			err := f.Check(context.Background(), j)
			if err == nil {
				t.Fatalf("expected fence rejection for %+v", tc.observe)
			}
			for _, want := range tc.contains {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error %q lacks %q", err, want)
				}
			}
		})
	}
}
