package updater

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/lepinoid/infra/updater/internal/journal"
)

func TestProbeResponseFixtures(t *testing.T) {
	for _, tc := range []struct {
		file   string
		before bool
		calls  int
	}{{"rcon-whitelist-on-when-on.txt", true, 1}, {"rcon-whitelist-on-when-off.txt", false, 2}} {
		t.Run(tc.file, func(t *testing.T) {
			e := probeEngine(t)
			calls := 0
			err := e.ProbeRuntime(context.Background(), func(_ context.Context, command string) (string, error) {
				calls++
				name := tc.file
				if command == "off" {
					name = "rcon-whitelist-off-when-on.txt"
				}
				data, err := os.ReadFile("../../../build-server/contract-fixtures/infra/" + name)
				return string(data), err
			})
			if err != nil {
				t.Fatal(err)
			}
			if calls != tc.calls || e.Journal.Maintenance.RuntimeEnabled == nil || *e.Journal.Maintenance.RuntimeEnabled != tc.before {
				t.Fatal(calls, e.Journal.Maintenance)
			}
		})
	}
}

func TestProbeCanonicalSchema(t *testing.T) {
	probe, err := journal.Read[journal.Probe]("../../fixtures/infra/runtime-whitelist-probe.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := probe.Validate(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile("../../schemas/runtime-whitelist-probe.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		AdditionalProperties bool     `json:"additionalProperties"`
		Required             []string `json:"required"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	if schema.AdditionalProperties || len(schema.Required) != 5 {
		t.Fatal(schema)
	}
	encoded, err := json.Marshal(probe)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	for _, key := range schema.Required {
		if _, ok := fields[key]; !ok {
			t.Fatal(key)
		}
	}
}
