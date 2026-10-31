package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"time"

	"github.com/lepinoid/infra/updater/internal/cluster"
	"github.com/lepinoid/infra/updater/internal/journal"
	"github.com/lepinoid/infra/updater/internal/status"
	updaterengine "github.com/lepinoid/infra/updater/internal/updater"
)

const (
	checkpointWaitLimit    = 5 * time.Minute
	checkpointPollInterval = 5 * time.Second
	checkpointStatusPath   = "/data/plugins/LepinoidTools/updater-status.json"
)

var checkpointCommitSHA = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)

// b4Checkpoint keeps the request and its outcome across Jobs. An accepted UUID
// only acknowledges acceptance; success still requires verified remote storage.
func (t *tx) b4Checkpoint() error {
	if err := t.ctx.Err(); err != nil {
		return err
	}
	if t.j.Phase != "MAINTENANCE_ACTIVE" || t.j.AccessState != "CLOSED" || !t.j.MaintenanceRequired {
		return updaterengine.ErrFenced
	}
	ctx, cancel := context.WithTimeout(t.ctx, checkpointWaitLimit)
	defer cancel()
	deadline := t.now().Add(checkpointWaitLimit)
	request := *t
	request.ctx = ctx
	record, err := t.loadCheckpoint()
	if err != nil {
		return err
	}
	if record != nil && record.PodUID != t.j.ExpectedPodUID {
		// A replacement Pod needs its own save, after recovery has re-established
		// closure. A generation-only fence refresh retains the existing UUID.
		if err := t.engine.Fence.Check(t.ctx, t.j); err != nil {
			return err
		}
		observed := updaterengine.Observation{PodUID: t.j.ExpectedPodUID, Generation: t.j.FencingGeneration}
		if err := t.verifyClosureIntact(observed); err != nil {
			return fmt.Errorf("checkpoint Pod changed without verified closure: %w", err)
		}
		record = nil
	}
	if record != nil {
		switch record.State {
		case "succeeded":
			return t.engine.Fence.Check(t.ctx, t.j)
		case "failed", "rejected":
			// Only a durable, definitive failure/rejection permits a new request.
			// A newly observed failure below always ends the current Job first.
			record = nil
		}
	}
	lastObservation := "checkpoint has not been accepted"
	for {
		if err := t.ctx.Err(); err != nil {
			return err
		}
		if ctx.Err() != nil || !t.now().Before(deadline) {
			if t.engine.Log != nil {
				operationID := ""
				if record != nil && record.OperationID != nil {
					operationID = *record.OperationID
				}
				fmt.Fprintf(t.engine.Log, "updater: checkpoint timed out: operationId=%q; %s\n", operationID, lastObservation)
			}
			return t.suspend("checkpoint-timeout")
		}
		if record == nil {
			record = &journal.Checkpoint{SchemaVersion: 1, TransactionID: t.j.TransactionID, PodUID: t.j.ExpectedPodUID, RequestedAt: t.now().UTC(), State: "requesting"}
			// Persist intent before RCON: a lost response must never authorize a
			// duplicate save, including after the process itself is interrupted.
			if err := t.saveCheckpoint(*record); err != nil {
				return err
			}
			out, err := request.rcon("lepinoid", "updater-checkpoint")
			if err != nil {
				lastObservation = fmt.Sprintf("checkpoint acceptance unknown: %v", err)
				if errors.Is(err, errRCONPassword) {
					// Password lookup failed before the command could be sent.
					record.State = "rejected"
				}
			} else {
				id, reason := status.Checkpoint(out)
				if id != "" {
					record.OperationID, record.State = &id, "accepted"
				} else {
					lastObservation = reason
					if reason != "checkpoint-busy" && reason != "checkpoint-unavailable" {
						if err := t.ctx.Err(); err != nil {
							return err
						}
						return t.suspend(reason)
					}
					record.State = "rejected"
				}
			}
			if err := t.ctx.Err(); err != nil {
				return err
			}
			if record.State != "requesting" {
				if err := t.saveCheckpoint(*record); err != nil {
					return err
				}
			}
			if record.State == "rejected" {
				record = nil
			}
		}
		if record != nil && ctx.Err() == nil {
			s, err := t.readCheckpointStatus(ctx)
			if err := t.ctx.Err(); err != nil {
				return err
			}
			if ctx.Err() != nil || !t.now().Before(deadline) {
				continue
			}
			if err != nil {
				lastObservation = fmt.Sprintf("checkpoint status unavailable: %v", err)
			} else {
				lastObservation = fmt.Sprintf("status podUID=%q phase=%q operationId=%v trigger=%v state=%q result=%v", s.ServerInstanceID, s.Phase, checkpointString(s.AutoCommit.OperationID), checkpointString(s.AutoCommit.Trigger), s.AutoCommit.State, checkpointString(s.AutoCommit.Result))
				if record.OperationID == nil && checkpointMatchesIntent(s, *record, t.now()) {
					record.OperationID, record.State = s.AutoCommit.OperationID, "accepted"
					if err := t.saveCheckpoint(*record); err != nil {
						return err
					}
				}
				if record.OperationID != nil {
					switch checkpointCompletion(s, *record.OperationID, record.PodUID, t.now()) {
					case "succeeded":
						record.State = "succeeded"
						record.StartedAt, record.CompletedAt, record.PushedCommitSHA = s.AutoCommit.StartedAt, s.AutoCommit.CompletedAt, s.AutoCommit.PushedCommitSHA
						return t.saveCheckpoint(*record)
					case "failed":
						record.State = "failed"
						if err := t.saveCheckpoint(*record); err != nil {
							return err
						}
						return t.suspend("checkpoint-failed")
					}
				}
			}
		}
		select {
		case <-ctx.Done():
		case <-time.After(checkpointPollInterval):
		}
	}
}

func (t *tx) loadCheckpoint() (*journal.Checkpoint, error) {
	path := t.store.Path("checkpoint", t.j.TransactionID)
	record, err := journal.Read[journal.Checkpoint](path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err == nil {
		err = record.Validate()
	}
	if err == nil && record.TransactionID != t.j.TransactionID {
		err = journal.ErrSchema
	}
	if err != nil {
		return nil, fmt.Errorf("checkpoint record %s: %w", path, err)
	}
	return &record, nil
}

func (t *tx) saveCheckpoint(record journal.Checkpoint) error {
	record.RequestedAt = record.RequestedAt.UTC()
	if record.StartedAt != nil {
		record.StartedAt = ptr(record.StartedAt.UTC())
	}
	if record.CompletedAt != nil {
		record.CompletedAt = ptr(record.CompletedAt.UTC())
	}
	if err := t.engine.Fence.Check(t.ctx, t.j); err != nil {
		return err
	}
	if err := record.Validate(); err != nil {
		return err
	}
	if record.TransactionID != t.j.TransactionID || record.PodUID != t.j.ExpectedPodUID {
		return journal.ErrSchema
	}
	return journal.Save(t.store.Path("checkpoint", t.j.TransactionID), record)
}

// The server cannot accept a caller-provided UUID. Recover a lost response only
// from a checkpoint started after this intent on the same Pod; an older save or
// a periodic save cannot establish that this request was accepted.
func checkpointMatchesIntent(s status.Status, record journal.Checkpoint, now time.Time) bool {
	a := s.AutoCommit
	id, _ := status.Checkpoint("operationId=" + checkpointString(a.OperationID))
	if s.SchemaVersion != 1 || s.ServerInstanceID != record.PodUID || s.Phase != "HEALTHY" || id == "" ||
		checkpointString(a.Trigger) != "UPDATER_CHECKPOINT" || a.StartedAt == nil || a.StartedAt.IsZero() ||
		a.StartedAt.Before(record.RequestedAt) || a.StartedAt.After(now.Add(2*time.Second)) ||
		s.UpdatedAt.Before(*a.StartedAt) || s.UpdatedAt.After(now.Add(2*time.Second)) {
		return false
	}
	if a.State == "running" {
		return checkpointString(a.Result) == "pending" && a.CompletedAt == nil && a.PushedCommitSHA == nil
	}
	return checkpointCompletion(s, id, record.PodUID, now) != ""
}

func (t *tx) readCheckpointStatus(ctx context.Context) (status.Status, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	pod := cluster.Pod{Name: t.podName, UID: t.j.ExpectedPodUID}
	data, err := t.commands.Exec(ctx, pod, "updater", "cat", checkpointStatusPath)
	if err != nil && ctx.Err() == nil {
		data, err = t.commands.Exec(ctx, pod, "minecraft", "cat", checkpointStatusPath)
	}
	if err != nil {
		return status.Status{}, err
	}
	var s status.Status
	err = json.Unmarshal(bytes.TrimSpace(data), &s)
	return s, err
}

func checkpointString(value *string) string {
	if value == nil {
		return "<null>"
	}
	return *value
}

// Status is an event-driven snapshot, not a heartbeat. A completed operation may
// be old when first observed; bind it to this UUID and Pod instead of requiring
// updatedAt to remain within the gate heartbeat's freshness window.
func checkpointCompletion(s status.Status, operationID, podUID string, now time.Time) string {
	a := s.AutoCommit
	if s.SchemaVersion != 1 || podUID == "" || s.ServerInstanceID != podUID || operationID == "" ||
		checkpointString(a.OperationID) != operationID || checkpointString(a.Trigger) != "UPDATER_CHECKPOINT" || a.State != "idle" {
		return ""
	}
	if checkpointString(a.Result) == "failed" {
		return "failed"
	}
	if s.Phase != "HEALTHY" || checkpointString(a.Result) != "succeeded" ||
		a.StartedAt == nil || a.CompletedAt == nil || a.StartedAt.IsZero() || a.CompletedAt.IsZero() ||
		a.CompletedAt.Before(*a.StartedAt) || a.CompletedAt.After(now.Add(2*time.Second)) ||
		s.UpdatedAt.Before(*a.CompletedAt) || !checkpointCommitSHA.MatchString(checkpointString(a.PushedCommitSHA)) {
		return ""
	}
	return "succeeded"
}
