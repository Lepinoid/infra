package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"time"

	"github.com/lepinoid/infra/updater/internal/cluster"
	"github.com/lepinoid/infra/updater/internal/status"
)

const (
	checkpointWaitLimit    = 5 * time.Minute
	checkpointPollInterval = 5 * time.Second
	checkpointStatusPath   = "/data/plugins/LepinoidTools/updater-status.json"
)

var checkpointCommitSHA = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)

// b4Checkpoint waits for the accepted operation's world save and remote-tip
// verification. An operationId acknowledges acceptance, not successful storage.
func (t *tx) b4Checkpoint() error {
	ctx, cancel := context.WithTimeout(t.ctx, checkpointWaitLimit)
	defer cancel()
	deadline := t.now().Add(checkpointWaitLimit)
	// Give the existing RCON/password helpers the same total deadline without
	// changing the transaction context needed to persist a timeout suspension.
	request := *t
	request.ctx = ctx
	var operationID string
	lastObservation := "checkpoint has not been accepted"
	for {
		if err := t.ctx.Err(); err != nil {
			return err
		}
		if ctx.Err() != nil || !t.now().Before(deadline) {
			if t.engine.Log != nil {
				fmt.Fprintf(t.engine.Log, "updater: checkpoint timed out: operationId=%q; %s\n", operationID, lastObservation)
			}
			return t.suspend("checkpoint-timeout")
		}
		if operationID == "" {
			out, err := request.rcon("lepinoid", "updater-checkpoint")
			if err != nil {
				lastObservation = fmt.Sprintf("checkpoint request unavailable: %v", err)
			} else {
				id, reason := status.Checkpoint(out)
				if id != "" {
					operationID = id
				} else {
					lastObservation = reason
					if reason != "checkpoint-busy" && reason != "checkpoint-unavailable" {
						if err := t.ctx.Err(); err != nil {
							return err
						}
						return t.suspend(reason)
					}
				}
			}
		}
		if operationID != "" && ctx.Err() == nil {
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
				switch checkpointCompletion(s, operationID, t.j.ExpectedPodUID, t.now()) {
				case "succeeded":
					return nil
				case "failed":
					return t.suspend("checkpoint-failed")
				}
			}
		}
		select {
		case <-ctx.Done():
		case <-time.After(checkpointPollInterval):
		}
	}
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
