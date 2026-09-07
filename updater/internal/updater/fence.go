package updater

import (
	"context"
	"errors"

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
		return ErrBlocked
	}
	if observed.PodUID != j.ExpectedPodUID || observed.Generation != j.FencingGeneration || observed.RecoveryPending {
		return ErrFenced
	}
	return nil
}
