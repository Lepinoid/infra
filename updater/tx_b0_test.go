package main

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/lepinoid/infra/updater/internal/journal"
)

// Given: a transaction that completed STAGED (target manifest staged)
// When: B0 enters MAINTENANCE_PREPARED (before a5GateClosed)
// Then: the persisted journal carries the full whitelist snapshot (whitelist.json
// state, runtime/persisted enforcement, startedAt) atomically with the phase,
// so init-recover can re-establish closure after a crash from this point on
func TestB0RecordsWhitelistSnapshot(t *testing.T) {
	for _, tc := range []struct {
		name    string
		onReply string
		want    bool
	}{
		{name: "runtime whitelist already on", onReply: "Whitelist is already turned on", want: true},
		{name: "runtime whitelist was off", onReply: "Whitelist is now turned on", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := gateTransaction(t)
			x.j.Maintenance = journal.Maintenance{}
			x.engine.Journal = x.j
			var calls []runnerCall
			x.commands = fakeRunner{calls: &calls, reply: func(name string, args []string) ([]byte, error) {
				if name == "kubectl" && slices.Equal(args, rconPasswordReadArgv) {
					return []byte("live-secret\n"), nil
				}
				if name == "kubectl" && whitelistSubcommand(args) != "" {
					switch whitelistSubcommand(args) {
					case "on":
						return []byte(tc.onReply + "\n"), nil
					case "off":
						return []byte("Whitelist is now turned off\n"), nil
					}
				}
				t.Errorf("unexpected command: %s %q", name, args)
				return nil, errors.New("unexpected command")
			}}

			if err := x.b0EnterMaintenance(); err != nil {
				t.Fatal(err)
			}

			persisted, err := journal.Read[journal.Journal](x.store.Path("journal", x.j.TransactionID))
			if err != nil {
				t.Fatal(err)
			}
			for name, j := range map[string]journal.Journal{"tx": x.j, "engine": x.engine.Journal, "persisted": persisted} {
				if j.Phase != "MAINTENANCE_PREPARED" || j.AccessState != "CLOSING" || !j.MaintenanceRequired || j.Lifecycle != "ACTIVE" {
					t.Fatalf("%s journal not at B0 state: %+v", name, j)
				}
				if err := j.Validate(); err != nil {
					t.Fatalf("%s journal invalid: %v", name, err)
				}
				m := j.Maintenance
				if m.PersistedEnabled == nil || m.RuntimeEnabled == nil || m.WhitelistExisted == nil || m.StartedAt == nil {
					t.Fatalf("%s snapshot incomplete: %+v", name, m)
				}
				if *m.RuntimeEnabled != tc.want {
					t.Fatalf("%s runtime enabled=%v, want %v", name, *m.RuntimeEnabled, tc.want)
				}
				if !m.StartedAt.Equal(time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC)) {
					t.Fatalf("%s startedAt=%v", name, *m.StartedAt)
				}
				if *m.WhitelistExisted && (m.WhitelistBackup == nil || m.WhitelistChecksum == nil) {
					t.Fatalf("%s backup/checksum missing while whitelist existed", name)
				}
			}
			if !tc.want && countSubcommand(calls, "off") != 1 {
				t.Fatalf("off-state probe must restore whitelist off: %+v", calls)
			}
		})
	}
}

// Given: a journal whose whitelist snapshot was already recorded (resume of an
// in-flight transaction, e.g. the INSTALL_COMPLETE journal stranded before this fix)
// When: B0 runs again
// Then: the recorded snapshot is not overwritten and no probe command is issued
func TestB0KeepsRecordedSnapshotOnResume(t *testing.T) {
	x := gateTransaction(t)
	started := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	x.j.Maintenance = journal.Maintenance{WhitelistExisted: ptr(false), PersistedEnabled: ptr(true), RuntimeEnabled: ptr(true), StartedAt: &started}
	x.engine.Journal = x.j
	x.commands = fakeRunner{calls: &[]runnerCall{}, reply: func(name string, args []string) ([]byte, error) {
		t.Errorf("no command expected when snapshot is recorded: %s %q", name, args)
		return nil, errors.New("unexpected command")
	}}

	if err := x.b0EnterMaintenance(); err != nil {
		t.Fatal(err)
	}
	if got := *x.j.Maintenance.StartedAt; !got.Equal(started) {
		t.Fatalf("snapshot overwritten: %v", got)
	}
	if !*x.j.Maintenance.RuntimeEnabled || !*x.j.Maintenance.PersistedEnabled {
		t.Fatalf("snapshot clobbered: %+v", x.j.Maintenance)
	}
}

func whitelistSubcommand(args []string) string {
	if len(args) < 3 || args[0] != "exec" {
		return ""
	}
	for i, a := range args {
		if a == "rcon-cli" && i+4 < len(args) && args[i+3] == "whitelist" {
			return args[i+4]
		}
	}
	return ""
}

func countSubcommand(calls []runnerCall, sub string) int {
	n := 0
	for _, c := range calls {
		if whitelistSubcommand(c.args) == sub {
			n++
		}
	}
	return n
}
