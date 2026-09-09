package journal

import (
	"fmt"
	"regexp"
	"slices"
)

var Phases = []string{"PREPARING", "DRIFT_DETECTED", "STAGED", "MAINTENANCE_PREPARED", "MAINTENANCE_ACTIVE", "BACKUP_STARTED", "BACKUP_COMPLETE", "INSTALL_STARTED", "INSTALL_COMPLETE", "RESTART_REQUESTED", "VERIFYING", "ACCESS_RESTORE_STARTED", "ACCESS_RESTORE_COMPLETE", "ROLLBACK_INSTALL_STARTED", "ROLLBACK_INSTALL_COMPLETE", "ROLLBACK_RESTART_REQUESTED"}
var failures = []string{"gate-ack-unrecoverable", "pod-or-pvc-unavailable", "init-unrecoverable", "invariant-violation", "whitelist-restore-failed", "persisted-whitelist-cas-conflict"}
var suspensions = []string{"player-count-unknown", "checkpoint-timeout", "checkpoint-failed", "checkpoint-busy", "checkpoint-unavailable", "checkpoint-invalid-response", "player-count-unknown-after-restore", "gate-ack-timeout", "sidecar-unavailable", "api-unavailable", "mc-version-mismatch", "mc-version-unknown"}
var hashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
var commitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func SameIdentity(a, b Manifest) bool {
	a.ConfigMapResourceVersion = ""
	b.ConfigMapResourceVersion = ""
	return a.SchemaVersion == b.SchemaVersion && a.Version == b.Version && a.OCIRepository == b.OCIRepository && a.Digest == b.Digest && a.PluginCommitSHA == b.PluginCommitSHA && a.ToolsSHA == b.ToolsSHA && a.MultiverseSHA == b.MultiverseSHA && a.MultiverseVersion == b.MultiverseVersion && slices.Equal(a.SupportedMinecraft, b.SupportedMinecraft)
}

func (m Manifest) Validate() error {
	if m.SchemaVersion != 1 || m.OCIRepository != "ghcr.io/lepinoid/lepinoid-tools" || len(m.Digest) != 71 || m.Digest[:7] != "sha256:" || !hashPattern.MatchString(m.Digest[7:]) || !hashPattern.MatchString(m.ToolsSHA) || !hashPattern.MatchString(m.MultiverseSHA) || !commitPattern.MatchString(m.PluginCommitSHA) || m.Version == "" || m.MultiverseVersion == "" || len(m.SupportedMinecraft) == 0 {
		return ErrSchema
	}
	return nil
}

func (j Journal) Validate() error {
	if j.SchemaVersion != 1 || !uuidPattern.MatchString(j.TransactionID) || !slices.Contains(Phases, j.Phase) || j.FencingGeneration < 0 || j.ExpectedPodUID == "" {
		return ErrSchema
	}
	if err := j.TargetManifest.Validate(); err != nil {
		return err
	}
	if !slices.Contains([]string{"OPEN", "CLOSING", "CLOSED", "OPENING"}, j.AccessState) {
		return ErrSchema
	}
	switch j.Lifecycle {
	case "ACTIVE":
		if j.Outcome != nil || j.SuspendReason != nil {
			return ErrSchema
		}
	case "SUSPENDED":
		if j.Outcome != nil || j.SuspendReason == nil || !slices.Contains(suspensions, *j.SuspendReason) {
			return ErrSchema
		}
	case "TERMINAL":
		if j.Outcome == nil || !slices.Contains([]string{"SUCCEEDED", "ROLLED_BACK", "ABORTED_NO_CHANGE", "ABORTED", "FAILED_MANUAL_INTERVENTION"}, *j.Outcome) || j.SuspendReason != nil {
			return ErrSchema
		}
	default:
		return ErrSchema
	}
	if j.Outcome != nil && *j.Outcome == "FAILED_MANUAL_INTERVENTION" {
		if j.FailureReason == nil || !slices.Contains(failures, *j.FailureReason) {
			return ErrSchema
		}
	} else if j.FailureReason != nil {
		return ErrSchema
	}
	if j.CommitCandidate != nil && *j.CommitCandidate != "SOURCE" && *j.CommitCandidate != "TARGET" {
		return ErrSchema
	}
	if (j.Phase == "ACCESS_RESTORE_STARTED" || j.Phase == "ACCESS_RESTORE_COMPLETE") && j.CommitCandidate == nil {
		return ErrSchema
	}
	return nil
}

func (j *Journal) Fail(reason string) error {
	if !slices.Contains(failures, reason) {
		return fmt.Errorf("%w: failureReason", ErrSchema)
	}
	outcome := "FAILED_MANUAL_INTERVENTION"
	j.Lifecycle = "TERMINAL"
	j.Outcome = &outcome
	j.FailureReason = &reason
	j.SuspendReason = nil
	return nil
}

func (j *Journal) Suspend(reason string) error {
	if !slices.Contains(suspensions, reason) {
		return fmt.Errorf("%w: suspendReason", ErrSchema)
	}
	j.Lifecycle = "SUSPENDED"
	j.SuspendReason = &reason
	j.Outcome = nil
	j.FailureReason = nil
	return nil
}

func (j Journal) Candidate() Pair {
	if j.CommitCandidate != nil && *j.CommitCandidate == "TARGET" {
		return j.Target
	}
	return j.Source
}
