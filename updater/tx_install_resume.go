package main

import (
	"fmt"

	"github.com/lepinoid/infra/updater/internal/journal"
	updaterengine "github.com/lepinoid/infra/updater/internal/updater"
)

// resumeStartedInstall is the INSTALL_STARTED step, including the first attempt
// immediately after BACKUP_COMPLETE persisted that phase. Engine.InstallPair
// moves staged jars, so a stopped partial attempt must replenish the pinned
// bundle before replaying the pair installation.
func (t *tx) resumeStartedInstall(plan *manifestPlan) error {
	if t.j.Phase != "INSTALL_STARTED" || t.j.Lifecycle != "ACTIVE" || t.j.AccessState != "CLOSED" || !t.j.MaintenanceRequired {
		return fmt.Errorf("%w: installation requires active, closed INSTALL_STARTED", updaterengine.ErrFenced)
	}
	if plan == nil || !journal.SameIdentity(plan.Desired, t.j.TargetManifest) || t.j.TargetDigest != t.j.TargetManifest.Digest || t.j.Target != sourcePairFromManifest(t.j.TargetManifest) {
		return fmt.Errorf("%w: installation plan differs from journal target", journal.ErrSchema)
	}
	t.engine.Journal = t.j
	if err := t.engine.Fence.Check(t.ctx, t.j); err != nil {
		return err
	}
	// This is a no-op while the complete, verified staging pair is present.
	// After a jar was moved, a4Stage fetches and verifies the same pinned digest;
	// it changes staging only and leaves the original backup and live jars alone.
	if err := t.a4Stage(plan); err != nil {
		return err
	}
	// Keep the existing per-jar fencing and fresh ACTIVE gate checks. Replacing
	// an already-installed target jar with the same verified bytes is safe.
	if err := t.b6Install(plan); err != nil {
		return err
	}
	t.j.CommitCandidate = ptr("TARGET")
	t.j.Phase = "INSTALL_COMPLETE"
	return t.saveJournal()
}
