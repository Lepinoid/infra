package updater

import (
	"context"
	"errors"
	"os"
	"strings"

	"github.com/lepinoid/infra/updater/internal/gate"
	"github.com/lepinoid/infra/updater/internal/journal"
)

type WhitelistCommand func(context.Context, string) (string, error)

func (e *Engine) ProbeRuntime(ctx context.Context, command WhitelistCommand) error {
	if e.Journal.AccessState != "CLOSED" || !e.Journal.MaintenanceRequired || e.Journal.Phase != "MAINTENANCE_ACTIVE" {
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
	file := e.Store.Path("journal", e.Journal.TransactionID+".probe")
	existing, err := journal.Read[journal.Probe](file)
	if err == nil {
		if err := existing.Validate(); err != nil {
			return err
		}
		if existing.TransactionID != e.Journal.TransactionID || existing.RuntimeBefore == nil {
			return ErrBlocked
		}
		e.Journal.Maintenance.RuntimeEnabled = existing.RuntimeBefore
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	probe := journal.Probe{SchemaVersion: 1, TransactionID: e.Journal.TransactionID, Probe: "runtime-whitelist", StartedAt: e.Now().UTC()}
	if err := journal.Save(file, probe); err != nil {
		return err
	}
	if err := e.Fence.Check(ctx, e.Journal); err != nil {
		return err
	}
	response, err := command(ctx, "on")
	if err != nil {
		return err
	}
	before := false
	switch strings.TrimSpace(response) {
	case "Whitelist is already turned on":
		before = true
	case "Whitelist is now turned on":
		if err := e.Fence.Check(ctx, e.Journal); err != nil {
			return err
		}
		restored, err := command(ctx, "off")
		if err != nil {
			return err
		}
		if strings.TrimSpace(restored) != "Whitelist is now turned off" {
			return journal.ErrSchema
		}
	default:
		return journal.ErrSchema
	}
	if err := e.Fence.Check(ctx, e.Journal); err != nil {
		return err
	}
	probe.RuntimeBefore = &before
	if err := journal.Save(file, probe); err != nil {
		return err
	}
	e.Journal.Maintenance.RuntimeEnabled = &before
	return nil
}
