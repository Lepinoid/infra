package updater

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/lepinoid/infra/updater/internal/fsutil"
	"github.com/lepinoid/infra/updater/internal/gate"
	"github.com/lepinoid/infra/updater/internal/journal"
)

type Engine struct {
	Store   journal.Store
	Journal journal.Journal
	Fence   Fence
	Now     func() time.Time
}

func (e *Engine) Save(ctx context.Context) error {
	if err := e.Fence.Check(ctx, e.Journal); err != nil {
		return err
	}
	if err := e.Journal.Validate(); err != nil {
		return err
	}
	return journal.Save(e.Store.Path("journal", e.Journal.TransactionID), e.Journal)
}

func (e *Engine) Phase(ctx context.Context, phase string) error {
	e.Journal.Phase = phase
	return e.Save(ctx)
}

func (e *Engine) JarMutation(ctx context.Context, mutation func() error) error {
	if e.Journal.AccessState != "CLOSED" || !e.Journal.MaintenanceRequired {
		return ErrFenced
	}
	if err := e.Fence.Check(ctx, e.Journal); err != nil {
		return err
	}
	ack, err := journal.Read[gate.Status](e.Store.Path("gate-status.json"))
	if err != nil {
		return err
	}
	if !ack.Active(e.identity(), e.Now()) {
		return ErrFenced
	}
	return mutation()
}

func (e *Engine) identity() gate.Identity {
	return gate.Identity{PodUID: e.Journal.ExpectedPodUID, TransactionID: e.Journal.TransactionID, Generation: e.Journal.FencingGeneration}
}

func (e *Engine) Flag(ctx context.Context) error {
	if err := e.Fence.Check(ctx, e.Journal); err != nil {
		return err
	}
	return journal.Save(e.Store.Path("maintenance.flag"), journal.Flag{SchemaVersion: 1, TransactionID: e.Journal.TransactionID, CreatedAt: e.Now().UTC(), JournalPath: "journal/" + e.Journal.TransactionID, FencingGeneration: e.Journal.FencingGeneration})
}

func (e *Engine) WaitGate(ctx context.Context, active bool) error {
	bounded, cancel := context.WithTimeout(ctx, gate.AckTimeout)
	defer cancel()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		s, err := journal.Read[gate.Status](e.Store.Path("gate-status.json"))
		if err == nil && ((active && s.Active(e.identity(), e.Now())) || (!active && s.Inactive(e.identity(), e.Now()))) {
			return nil
		}
		select {
		case <-bounded.Done():
			return bounded.Err()
		case <-ticker.C:
		}
	}
}

func (e *Engine) Suspend(ctx context.Context, reason string) error {
	if err := e.Journal.Suspend(reason); err != nil {
		return err
	}
	return e.Save(ctx)
}

func (e *Engine) Fail(ctx context.Context, reason string) error {
	if err := e.Journal.Fail(reason); err != nil {
		return err
	}
	return e.Save(ctx)
}

func (e *Engine) Open(ctx context.Context, restore func(context.Context) error) error {
	if err := restore(ctx); err != nil {
		return errors.Join(err, e.Fail(ctx, "whitelist-restore-failed"))
	}
	e.Journal.AccessState = "OPENING"
	e.Journal.MaintenanceRequired = false
	if err := e.Phase(ctx, "ACCESS_RESTORE_STARTED"); err != nil {
		return err
	}
	if err := e.Fence.Check(ctx, e.Journal); err != nil {
		return err
	}
	if err := os.Remove(e.Store.Path("maintenance.flag")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := fsutil.SyncDir(e.Store.Root); err != nil {
		return err
	}
	if err := e.WaitGate(ctx, false); err != nil {
		e.Journal.MaintenanceRequired = true
		if saveErr := e.Save(ctx); saveErr != nil {
			return saveErr
		}
		if flagErr := e.Flag(ctx); flagErr != nil {
			return errors.Join(flagErr, e.Fail(ctx, "gate-ack-unrecoverable"))
		}
		if ackErr := e.WaitGate(ctx, true); ackErr != nil {
			return errors.Join(ackErr, e.Fail(ctx, "gate-ack-unrecoverable"))
		}
		e.Journal.AccessState = "CLOSED"
		return errors.Join(err, e.Suspend(ctx, "gate-ack-timeout"))
	}
	e.Journal.AccessState = "OPEN"
	return e.Phase(ctx, "ACCESS_RESTORE_COMPLETE")
}

func (e *Engine) InstallPair(ctx context.Context, source string, pair journal.Pair) error {
	for _, jar := range []journal.Jar{pair.Tools, pair.Multiverse} {
		if jar.Name != "LepinoidTools.jar" && jar.Name != "Multiverse-Core.jar" {
			return journal.ErrSchema
		}
		src := filepath.Join(source, jar.Name)
		if err := fsutil.VerifyJar(src, jar.SHA256); err != nil {
			return err
		}
		dst := filepath.Join(filepath.Dir(e.Store.Root), jar.Name)
		if err := e.JarMutation(ctx, func() error { return fsutil.Move(src, dst) }); err != nil {
			return err
		}
	}
	return nil
}
