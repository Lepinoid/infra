package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/lepinoid/infra/updater/internal/artifact"
	"github.com/lepinoid/infra/updater/internal/gate"
	"github.com/lepinoid/infra/updater/internal/journal"
	"github.com/lepinoid/infra/updater/internal/players"
	"github.com/lepinoid/infra/updater/internal/status"
)

func roundTrip[T any](t *testing.T, name string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("../build-server/contract-fixtures/shared", name))
	if err != nil {
		t.Fatal(err)
	}
	value, err := journal.Decode[T](bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var original, actual map[string]json.RawMessage
	if err := json.Unmarshal(data, &original); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &actual); err != nil {
		t.Fatal(err)
	}
	for key, want := range original {
		var a, b interface{}
		if err := json.Unmarshal(want, &a); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(actual[key], &b); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(a, b) {
			t.Fatalf("field %s changed", key)
		}
	}
}

func TestSharedJSONContracts(t *testing.T) {
	t.Run("gate", func(t *testing.T) { roundTrip[gate.Status](t, "gate-status.json") })
	t.Run("updater", func(t *testing.T) { roundTrip[status.Status](t, "updater-status.json") })
	t.Run("compatibility", func(t *testing.T) { roundTrip[artifact.Compatibility](t, "compatibility.json") })
}

func TestCheckpointFixtures(t *testing.T) {
	for _, tc := range []struct{ name, reason string }{{"operation-accepted.txt", ""}, {"error-busy.txt", "checkpoint-busy"}, {"error-unavailable.txt", "checkpoint-unavailable"}} {
		data, err := os.ReadFile(filepath.Join("../build-server/contract-fixtures/shared/updater-checkpoint", tc.name))
		if err != nil {
			t.Fatal(err)
		}
		id, reason := status.Checkpoint(string(data))
		if reason != tc.reason || (tc.reason == "" && id != "550e8400-e29b-41d4-a716-446655440000") {
			t.Fatalf("%s: %s %s", tc.name, id, reason)
		}
	}
}

func TestPlayerFixtures(t *testing.T) {
	for _, tc := range []struct {
		name string
		want players.Observation
	}{{"rcon-list-empty.txt", players.Observation{Count: 0, Valid: true}}, {"rcon-list-occupied.txt", players.Observation{Count: 2, Valid: true}}, {"rcon-list-invalid.txt", players.Observation{}}} {
		data, err := os.ReadFile(filepath.Join("../build-server/contract-fixtures/infra", tc.name))
		if err != nil {
			t.Fatal(err)
		}
		if got := players.RCON(string(data)); got != tc.want {
			t.Fatalf("%s: %+v", tc.name, got)
		}
	}
}

func TestInfraCanonicalFixtures(t *testing.T) {
	cases := []struct {
		name     string
		filename string
		schema   string
		probe    any
	}{
		{"maintenance flag", "maintenance-flag.json", "maintenance-flag.json", &journal.Flag{}},
		{"journal active", "journal-active.json", "journal.json", &journal.Journal{}},
		{"current", "current.json", "current.json", &journal.Current{}},
		{"recovery record", "recovery-record.json", "recovery.json", &journal.Recovery{}},
		{"runtime whitelist probe", "runtime-whitelist-probe.json", "runtime-whitelist-probe.json", &journal.Probe{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("../build-server/contract-fixtures/infra", tc.filename))
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := decodeInto(tc.probe, data)
			if err != nil {
				t.Fatal(err)
			}
			var strict func() error
			switch v := decoded.(type) {
			case *journal.Journal:
				strict = v.Validate
			case *journal.Current:
				strict = v.Validate
			case *journal.Probe:
				strict = v.Validate
			case *journal.Flag:
				strict = func() error { return nil }
			case *journal.Recovery:
				strict = func() error { return nil }
			}
			if strict == nil {
				t.Fatalf("unhandled %T", decoded)
			}
			if err := strict(); err != nil {
				t.Fatal(err)
			}
			schemaPath := filepath.Join("schemas", tc.schema)
			schema, err := os.ReadFile(schemaPath)
			if err != nil {
				t.Fatal(err)
			}
			var document struct {
				Required []string `json:"required"`
			}
			if err := json.Unmarshal(schema, &document); err != nil {
				t.Fatal(err)
			}
			var fixture map[string]json.RawMessage
			if err := json.Unmarshal(data, &fixture); err != nil {
				t.Fatal(err)
			}
			for _, key := range document.Required {
				if _, ok := fixture[key]; !ok {
					t.Errorf("fixture %s に必須キー %s が無い (schema %s)", tc.filename, key, schemaPath)
				}
			}
		})
	}
}

func decodeInto(target any, data []byte) (any, error) {
	switch typed := target.(type) {
	case *journal.Flag:
		v, err := journal.Decode[journal.Flag](bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		*typed = v
		return typed, nil
	case *journal.Journal:
		v, err := journal.Decode[journal.Journal](bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		*typed = v
		return typed, nil
	case *journal.Current:
		v, err := journal.Decode[journal.Current](bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		*typed = v
		return typed, nil
	case *journal.Recovery:
		v, err := journal.Decode[journal.Recovery](bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		*typed = v
		return typed, nil
	case *journal.Probe:
		v, err := journal.Decode[journal.Probe](bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		*typed = v
		return typed, nil
	default:
		return nil, journal.ErrSchema
	}
}
