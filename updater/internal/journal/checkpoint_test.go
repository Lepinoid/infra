package journal

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func testCheckpoint() Checkpoint {
	id := "650e8400-e29b-41d4-a716-446655440000"
	return Checkpoint{SchemaVersion: 1, TransactionID: testJournal().TransactionID, PodUID: "pod-uid", RequestedAt: time.Date(2026, 10, 31, 1, 0, 0, 0, time.UTC), State: "accepted", OperationID: &id}
}

func TestCheckpointRecordStateContracts(t *testing.T) {
	for _, state := range []string{"requesting", "rejected", "accepted", "failed", "succeeded"} {
		t.Run(state, func(t *testing.T) {
			c := testCheckpoint()
			c.State = state
			if state == "requesting" || state == "rejected" {
				c.OperationID = nil
			}
			if state == "succeeded" {
				start, end, sha := c.RequestedAt, c.RequestedAt.Add(time.Minute), strings.Repeat("a", 40)
				c.StartedAt, c.CompletedAt, c.PushedCommitSHA = &start, &end, &sha
			}
			if err := c.Validate(); err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(c)
			if err != nil {
				t.Fatal(err)
			}
			after, err := Decode[Checkpoint](bytes.NewReader(data))
			if err != nil || !reflect.DeepEqual(c, after) {
				t.Fatalf("checkpoint did not round trip: %+v %v", after, err)
			}
		})
	}
}

func TestCheckpointRecordRejectsInvalidEvidence(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*Checkpoint)
	}{
		{"schema", func(c *Checkpoint) { c.SchemaVersion = 2 }},
		{"transaction", func(c *Checkpoint) { c.TransactionID = "invalid" }},
		{"pod", func(c *Checkpoint) { c.PodUID = "" }},
		{"request time", func(c *Checkpoint) { c.RequestedAt = time.Time{} }},
		{"state", func(c *Checkpoint) { c.State = "unknown" }},
		{"missing id", func(c *Checkpoint) { c.OperationID = nil }},
		{"invalid id", func(c *Checkpoint) { id := "invalid"; c.OperationID = &id }},
		{"intent has id", func(c *Checkpoint) { c.State = "requesting" }},
		{"incomplete success", func(c *Checkpoint) { c.State = "succeeded" }},
		{"premature evidence", func(c *Checkpoint) { at := c.RequestedAt; c.CompletedAt = &at }},
		{"invalid SHA", func(c *Checkpoint) {
			at, sha := c.RequestedAt, "bad"
			c.State, c.StartedAt, c.CompletedAt, c.PushedCommitSHA = "succeeded", &at, &at, &sha
		}},
		{"completion before start", func(c *Checkpoint) {
			at, end, sha := c.RequestedAt, c.RequestedAt.Add(-time.Second), strings.Repeat("b", 40)
			c.State, c.StartedAt, c.CompletedAt, c.PushedCommitSHA = "succeeded", &at, &end, &sha
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testCheckpoint()
			tc.edit(&c)
			if err := c.Validate(); err == nil {
				t.Fatalf("accepted invalid checkpoint: %+v", c)
			}
		})
	}
}

func TestCheckpointRecordStrictDecoder(t *testing.T) {
	data, err := json.Marshal(testCheckpoint())
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"schemaVersion", "transactionId", "podUid", "requestedAt", "state", "operationId", "startedAt", "completedAt", "pushedCommitSha"} {
		t.Run("missing "+field, func(t *testing.T) {
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(data, &fields); err != nil {
				t.Fatal(err)
			}
			delete(fields, field)
			invalid, err := json.Marshal(fields)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Decode[Checkpoint](bytes.NewReader(invalid)); err == nil {
				t.Fatalf("accepted missing %s", field)
			}
		})
	}
	for _, addition := range []string{`,"unexpected":null}`, `,"state":"accepted"}`} {
		invalid := string(data[:len(data)-1]) + addition
		if _, err := Decode[Checkpoint](strings.NewReader(invalid)); err == nil {
			t.Fatalf("accepted invalid checkpoint JSON: %s", invalid)
		}
	}
}

func TestCheckpointStoragePreservesLegacyJournalInventory(t *testing.T) {
	store := Store{Root: t.TempDir()}
	if err := store.Init(); err != nil {
		t.Fatal(err)
	}
	fixture, err := os.ReadFile("../../../build-server/contract-fixtures/infra/journal-active.json")
	if err != nil {
		t.Fatal(err)
	}
	j, err := Decode[Journal](bytes.NewReader(fixture))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.Path("journal", j.TransactionID), fixture, 0644); err != nil {
		t.Fatal(err)
	}
	c := testCheckpoint()
	c.TransactionID = j.TransactionID
	if err := Save(store.Path("checkpoint", c.TransactionID), c); err != nil {
		t.Fatal(err)
	}
	inventory, err := store.Inspect()
	if err != nil || len(inventory.Journals) != 1 || !reflect.DeepEqual(j, inventory.Journals[0]) {
		t.Fatalf("checkpoint changed legacy inventory: %+v %v", inventory, err)
	}
	after, err := os.ReadFile(store.Path("journal", j.TransactionID))
	if err != nil || !bytes.Equal(fixture, after) {
		t.Fatalf("legacy journal format changed: %v", err)
	}
}
