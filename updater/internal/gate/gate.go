package gate

import "time"

const AckTimeout = 60 * time.Second

type Identity struct {
	PodUID        string
	TransactionID string
	Generation    int64
}

type Status struct {
	SchemaVersion       int       `json:"schemaVersion"`
	Version             string    `json:"version"`
	ServerInstanceID    string    `json:"serverInstanceId"`
	Phase               string    `json:"phase"`
	ObservedTransaction *string   `json:"observedFlagTransactionId"`
	ObservedGeneration  *int64    `json:"observedFlagGeneration"`
	ReleasedTransaction *string   `json:"lastReleasedTransactionId"`
	ReleasedGeneration  *int64    `json:"lastReleasedFlagGeneration"`
	UpdatedAt           time.Time `json:"updatedAt"`
}

func Fresh(now, updated time.Time) bool {
	delta := now.Sub(updated)
	return !updated.IsZero() && delta >= -2*time.Second && delta <= 6*time.Second
}

func (s Status) Active(id Identity, now time.Time) bool {
	return s.SchemaVersion == 1 && s.ServerInstanceID == id.PodUID && s.Phase == "ACTIVE" && Fresh(now, s.UpdatedAt) && s.ObservedTransaction != nil && *s.ObservedTransaction == id.TransactionID && s.ObservedGeneration != nil && *s.ObservedGeneration == id.Generation
}

func (s Status) Inactive(id Identity, now time.Time) bool {
	return s.SchemaVersion == 1 && s.ServerInstanceID == id.PodUID && s.Phase == "INACTIVE" && Fresh(now, s.UpdatedAt) && s.ReleasedTransaction != nil && *s.ReleasedTransaction == id.TransactionID && s.ReleasedGeneration != nil && *s.ReleasedGeneration == id.Generation
}
