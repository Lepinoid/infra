package updater

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/lepinoid/infra/updater/internal/journal"
)

var ErrFenced = errors.New("mutation fenced")
var ErrBlocked = errors.New("unresolved blocking record or manual intervention")

type Observation struct {
	PodUID          string
	Generation      int64
	RecoveryPending bool
	Blocked         bool
}
type Fence struct {
	Lease   func(context.Context) error
	Observe func(context.Context) (Observation, error)
}

func (f Fence) Check(ctx context.Context, j journal.Journal) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := f.Lease(ctx); err != nil {
		return err
	}
	observed, err := f.Observe(ctx)
	if err != nil {
		return err
	}
	if observed.Blocked {
		return fmt.Errorf("%w: a FAILED_MANUAL_INTERVENTION journal is pending manual resolution", ErrBlocked)
	}
	var mismatches []string
	if observed.PodUID != j.ExpectedPodUID {
		mismatches = append(mismatches, fmt.Sprintf("expectedPodUid: journal has %q but deployment runs pod %q", j.ExpectedPodUID, observed.PodUID))
	}
	if observed.Generation != j.FencingGeneration {
		mismatches = append(mismatches, fmt.Sprintf("fencingGeneration: journal pins %d but deployment is %d", j.FencingGeneration, observed.Generation))
	}
	if observed.RecoveryPending {
		mismatches = append(mismatches, "recoveryPending: init-recover is establishing closure")
	}
	if len(mismatches) > 0 {
		return fmt.Errorf("%w: %s", ErrFenced, strings.Join(mismatches, "; "))
	}
	return nil
}
