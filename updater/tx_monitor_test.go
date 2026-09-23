package main

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/lepinoid/infra/updater/internal/players"
)

func TestPlayerMonitorExecutesInMinecraftPod(t *testing.T) {
	x := gateTransaction(t)
	calls := &[]runnerCall{}
	x.commands = fakeRunner{calls: calls, reply: func(name string, args []string) ([]byte, error) {
		if name != "kubectl" {
			t.Fatalf("monitor ran locally: %s", name)
		}
		if slices.Equal(args, []string{"get", "pod", x.podName, "-o", "json"}) {
			return json.Marshal(map[string]any{"metadata": map[string]string{"name": x.podName, "uid": x.podUID}})
		}
		if len(args) < 6 || args[0] != "exec" || args[1] != x.podName || args[3] != "minecraft" {
			t.Fatalf("wrong target: %q", args)
		}
		switch {
		case args[5] == "sh":
			return []byte("secret\n"), nil
		case args[5] == "rcon-cli":
			return []byte("There are 0 of a max of 20 players online:"), nil
		case slices.Equal(args[5:], []string{"mc-monitor", "status", "--host", "localhost", "--port", "25565"}):
			return []byte("localhost:25565 : version=Paper 1.21.8 online=0 max=20 motd='test'"), nil
		default:
			t.Fatalf("unexpected exec: %q", args)
			return nil, nil
		}
	}}
	state, err := x.observePlayers()
	if err != nil || state != players.Zero {
		t.Fatalf("state=%s err=%v", state, err)
	}
}

func TestPlayerMonitorRefusesWrongPodUID(t *testing.T) {
	x := gateTransaction(t)
	calls := &[]runnerCall{}
	x.commands = fakeRunner{calls: calls, reply: func(_ string, args []string) ([]byte, error) {
		if args[0] == "exec" {
			t.Fatal("exec issued to wrong UID")
		}
		return json.Marshal(map[string]any{"metadata": map[string]string{"name": x.podName, "uid": "different"}})
	}}
	if _, err := x.mcMonitor(); err == nil {
		t.Fatal("wrong Pod accepted")
	}
}

func TestPlayerMonitorFailureDoesNotEstablishEmpty(t *testing.T) {
	x := gateTransaction(t)
	calls := &[]runnerCall{}
	x.commands = fakeRunner{calls: calls, reply: func(_ string, args []string) ([]byte, error) {
		if args[0] == "get" {
			return json.Marshal(map[string]any{"metadata": map[string]string{"name": x.podName, "uid": x.podUID}})
		}
		if args[5] == "sh" {
			return []byte("secret"), nil
		}
		if args[5] == "rcon-cli" {
			return []byte("There are 0 of a max of 20 players online:"), nil
		}
		return nil, errors.New("monitor connection failed")
	}}
	state, err := x.observePlayers()
	if err != nil || state != players.Unknown {
		t.Fatalf("state=%s err=%v", state, err)
	}
}
