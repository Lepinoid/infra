package updater

import (
	"context"
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
