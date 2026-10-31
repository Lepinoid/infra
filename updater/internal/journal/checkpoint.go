package journal

import (
	"regexp"
	"time"
)

// Checkpoint lives outside journal/ so older readiness and recovery readers can
// still decode the unchanged transaction journal during a management upgrade.
type Checkpoint struct {
	SchemaVersion   int        `json:"schemaVersion"`
	TransactionID   string     `json:"transactionId"`
	PodUID          string     `json:"podUid"`
	RequestedAt     time.Time  `json:"requestedAt"`
	State           string     `json:"state"`
	OperationID     *string    `json:"operationId"`
	StartedAt       *time.Time `json:"startedAt"`
	CompletedAt     *time.Time `json:"completedAt"`
	PushedCommitSHA *string    `json:"pushedCommitSha"`
}

var checkpointCommitPattern = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)

func (c Checkpoint) Validate() error {
	if c.SchemaVersion != 1 || !uuidPattern.MatchString(c.TransactionID) || c.PodUID == "" || c.RequestedAt.IsZero() {
		return ErrSchema
	}
	switch c.State {
	case "requesting", "rejected":
		if c.OperationID != nil {
			return ErrSchema
		}
	case "accepted", "failed", "succeeded":
		if c.OperationID == nil || !uuidPattern.MatchString(*c.OperationID) {
			return ErrSchema
		}
	default:
		return ErrSchema
	}
	if c.State == "succeeded" {
		if c.StartedAt == nil || c.CompletedAt == nil || c.StartedAt.IsZero() || c.CompletedAt.IsZero() ||
			c.CompletedAt.Before(*c.StartedAt) || c.PushedCommitSHA == nil || !checkpointCommitPattern.MatchString(*c.PushedCommitSHA) {
			return ErrSchema
		}
	} else if c.StartedAt != nil || c.CompletedAt != nil || c.PushedCommitSHA != nil {
		return ErrSchema
	}
	return nil
}
