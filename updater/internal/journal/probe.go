package journal

import "time"

type Probe struct {
	SchemaVersion int       `json:"schemaVersion"`
	TransactionID string    `json:"transactionId"`
	Probe         string    `json:"probe"`
	StartedAt     time.Time `json:"startedAt"`
	RuntimeBefore *bool     `json:"runtimeBefore"`
}

func (p Probe) Validate() error {
	if p.SchemaVersion != 1 || !uuidPattern.MatchString(p.TransactionID) || p.Probe != "runtime-whitelist" || p.StartedAt.IsZero() {
		return ErrSchema
	}
	return nil
}
