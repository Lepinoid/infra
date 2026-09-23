package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/lepinoid/infra/updater/internal/fsutil"
	"github.com/lepinoid/infra/updater/internal/gate"
	"github.com/lepinoid/infra/updater/internal/journal"
	recovery "github.com/lepinoid/infra/updater/internal/recover"
)

func (t *tx) dataPath(parts ...string) string {
	return filepath.Join(append([]string{filepath.Dir(filepath.Dir(t.store.Root))}, parts...)...)
}

func (t *tx) restoreWhitelist(ctx context.Context) error {
	m := t.j.Maintenance
	if m.WhitelistExisted == nil || m.PersistedEnabled == nil || m.RuntimeEnabled == nil {
		return fmt.Errorf("%w: incomplete whitelist snapshot", journal.ErrSchema)
	}
	if err := t.engine.Fence.Check(ctx, t.j); err != nil {
		return err
	}
	path := t.dataPath("whitelist.json")
	if *m.WhitelistExisted {
		if m.WhitelistBackup == nil || m.WhitelistChecksum == nil {
			return journal.ErrSchema
		}
		data, err := base64.StdEncoding.DecodeString(*m.WhitelistBackup)
		if err != nil {
			return err
		}
		if err := fsutil.WriteJSON(path, data); err != nil {
			return err
		}
		sum, err := fsutil.SHA256(path)
		if err != nil {
			return err
		}
		if sum != *m.WhitelistChecksum {
			return errors.New("restored whitelist checksum mismatch")
		}
	} else {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := fsutil.SyncDir(filepath.Dir(path)); err != nil {
			return err
		}
	}
	if err := t.engine.Fence.Check(ctx, t.j); err != nil {
		return err
	}
	reply, err := t.rcon("whitelist", "reload")
	if err != nil {
		return err
	}
	if strings.TrimSpace(reply) != "Reloaded the whitelist" {
		return fmt.Errorf("unexpected whitelist reload response: %q", reply)
	}
	if err := t.engine.Fence.Check(ctx, t.j); err != nil {
		return err
	}
	persistedNow, err := recovery.Whitelist(t.dataPath("server.properties"))
	if err != nil {
		return err
	}
	if !persistedNow && *m.PersistedEnabled {
		return recovery.ErrCAS
	}
	if err := recovery.SetWhitelist(t.dataPath("server.properties"), persistedNow, *m.PersistedEnabled); err != nil {
		return err
	}
	action := "off"
	if *m.RuntimeEnabled {
		action = "on"
	}
	reply, err = t.rcon("whitelist", action)
	if err != nil {
		return err
	}
	if reply != "Whitelist is now turned "+action && reply != "Whitelist is already turned "+action {
		return fmt.Errorf("unexpected whitelist %s response: %q", action, reply)
	}
	// RCON changes the persisted setting as well. Restore the separate snapshot
	// after changing the runtime state and verify it before releasing the gate.
	if err := t.engine.Fence.Check(ctx, t.j); err != nil {
		return err
	}
	persistedNow, err = recovery.Whitelist(t.dataPath("server.properties"))
	if err != nil {
		return err
	}
	if err := recovery.SetWhitelist(t.dataPath("server.properties"), persistedNow, *m.PersistedEnabled); err != nil {
		return err
	}
	persisted, err := readPersistedWhitelist(t.dataPath("server.properties"))
	if err != nil {
		return err
	}
	if persisted != *m.PersistedEnabled {
		return errors.New("persisted whitelist restoration failed")
	}
	return nil
}

func (t *tx) openAccess() error {
	t.j.Phase = "ACCESS_RESTORE_STARTED"
	if err := t.saveJournal(); err != nil {
		return err
	}
	err := t.engine.Open(t.ctx, func(ctx context.Context) error {
		if err := verifyInstalledPair(t.j.Candidate()); err != nil {
			return err
		}
		return t.restoreWhitelist(ctx)
	})
	t.j = t.engine.Journal
	return err
}

// current is committed only after the server's access has been restored. Each
// write precedes the archive rename, so a crash can safely resume this operation.
func (t *tx) finalizeSuccess() error {
	if err := t.j.TargetManifest.Validate(); err != nil {
		return err
	}
	if t.j.TargetDigest != t.j.TargetManifest.Digest || t.j.Target != sourcePairFromManifest(t.j.TargetManifest) {
		return errors.New("target journal pair disagrees with target manifest")
	}
	if t.j.Phase != "ACCESS_RESTORE_COMPLETE" || t.j.AccessState != "OPEN" || t.j.MaintenanceRequired || t.j.CommitCandidate == nil || *t.j.CommitCandidate != "TARGET" {
		return errors.New("refusing successful commit before target access restoration")
	}
	if _, err := os.Lstat(t.store.Path("maintenance.flag")); !errors.Is(err, os.ErrNotExist) {
		if err != nil {
			return err
		}
		return errors.New("refusing successful commit with maintenance flag present")
	}
	ack, err := journal.Read[gate.Status](t.store.Path("gate-status.json"))
	if err != nil {
		return err
	}
	id := gate.Identity{PodUID: t.j.ExpectedPodUID, TransactionID: t.j.TransactionID, Generation: t.j.FencingGeneration}
	if !ack.Inactive(id, t.now()) {
		return errors.New("refusing successful commit without fresh INACTIVE gate acknowledgement")
	}
	if err := verifyInstalledPair(t.j.Target); err != nil {
		return err
	}
	if err := t.engine.Fence.Check(t.ctx, t.j); err != nil {
		return err
	}
	current := journal.Current{Manifest: t.j.TargetManifest, InstalledAt: t.now().UTC()}
	previous, err := journal.Read[journal.Current](t.store.Path("current"))
	if err == nil && journal.SameIdentity(previous.Manifest, current.Manifest) {
		current.InstalledAt = previous.InstalledAt
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := journal.Save(t.store.Path("current"), current); err != nil {
		return err
	}
	outcome := "SUCCEEDED"
	t.j.Lifecycle, t.j.Outcome, t.j.SuspendReason, t.j.FailureReason = "TERMINAL", &outcome, nil, nil
	if err := t.saveJournal(); err != nil {
		return err
	}
	if err := t.archiveTransaction(); err != nil {
		return err
	}
	return t.b11GC()
}

// resumeTerminal validates the terminal outcome before any fence refresh. A
// manual or closed terminal must never be converted into a resumable suspension.
func (t *tx) resumeTerminal() error {
	if err := t.checkTerminalAccess(); err != nil {
		return err
	}
	if err := t.refreshFenceOnResume(); err != nil {
		return err
	}
	return t.finalizeTerminal()
}

func (t *tx) checkTerminalAccess() error {
	if t.j.Lifecycle != "TERMINAL" || t.j.Outcome == nil {
		return errors.New("terminal journal outcome missing")
	}
	switch *t.j.Outcome {
	case "SUCCEEDED", "ABORTED", "ABORTED_NO_CHANGE", "ROLLED_BACK":
	default:
		return errors.New("terminal journal requires manual recovery")
	}
	if t.j.MaintenanceRequired || t.j.AccessState != "OPEN" {
		return errors.New("terminal journal access has not been restored")
	}
	if _, err := os.Lstat(t.store.Path("maintenance.flag")); !errors.Is(err, os.ErrNotExist) {
		if err != nil {
			return err
		}
		return errors.New("terminal journal still has a maintenance flag")
	}
	return nil
}

// Normal terminal outcomes are archiveable after their access-restoration
// postconditions are rechecked. They must never reapply the target bundle.
func (t *tx) finalizeTerminal() error {
	if err := t.checkTerminalAccess(); err != nil {
		return err
	}
	if *t.j.Outcome == "SUCCEEDED" {
		return t.finalizeSuccess()
	}
	if !canSupersede(t.j) {
		if t.j.Phase != "ACCESS_RESTORE_COMPLETE" {
			return errors.New("terminal journal restoration is incomplete")
		}
		ack, err := journal.Read[gate.Status](t.store.Path("gate-status.json"))
		if err != nil {
			return err
		}
		id := gate.Identity{PodUID: t.j.ExpectedPodUID, TransactionID: t.j.TransactionID, Generation: t.j.FencingGeneration}
		if !ack.Inactive(id, t.now()) {
			return errors.New("terminal journal lacks a fresh INACTIVE acknowledgement")
		}
		if err := verifyInstalledPair(t.j.Candidate()); err != nil {
			return err
		}
	}
	return t.archiveTransaction()
}

func (t *tx) archiveTransaction() error {
	records, err := os.ReadDir(t.store.Path("recovery"))
	if err != nil {
		return err
	}
	for _, entry := range records {
		record, err := journal.Read[journal.Recovery](t.store.Path("recovery", entry.Name()))
		if err != nil {
			return err
		}
		if record.TransactionID == nil || *record.TransactionID != t.j.TransactionID {
			continue
		}
		if err := t.engine.Fence.Check(t.ctx, t.j); err != nil {
			return err
		}
		if err := fsutil.Move(t.store.Path("recovery", entry.Name()), t.store.Path("archive", t.j.TransactionID+".recovery-"+entry.Name())); err != nil {
			return err
		}
	}
	probe := t.store.Path("journal", t.j.TransactionID+".probe")
	if _, err := os.Lstat(probe); err == nil {
		if err := t.engine.Fence.Check(t.ctx, t.j); err != nil {
			return err
		}
		if err := fsutil.Move(probe, t.store.Path("archive", t.j.TransactionID+".probe")); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := t.engine.Fence.Check(t.ctx, t.j); err != nil {
		return err
	}
	if err := fsutil.Move(t.store.Path("journal", t.j.TransactionID), t.store.Path("archive", t.j.TransactionID)); err != nil {
		return err
	}
	return nil
}
