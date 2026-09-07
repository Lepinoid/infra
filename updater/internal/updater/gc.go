package updater

import (
	"context"
	"slices"
	"time"
)

type Generation struct {
	Digest    string
	CreatedAt time.Time
}
type GCPlan struct {
	Blocked     bool
	Protected   []string
	Generations []Generation
}

func (p GCPlan) Run(ctx context.Context, fence func(context.Context) error, remove func(string) error) error {
	if p.Blocked {
		return ErrBlocked
	}
	generations := slices.Clone(p.Generations)
	slices.SortFunc(generations, func(a, b Generation) int { return b.CreatedAt.Compare(a.CreatedAt) })
	kept := 0
	for _, g := range generations {
		if slices.Contains(p.Protected, g.Digest) {
			continue
		}
		if kept < 2 {
			kept++
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := fence(ctx); err != nil {
			return err
		}
		if err := remove(g.Digest); err != nil {
			return err
		}
	}
	return nil
}
