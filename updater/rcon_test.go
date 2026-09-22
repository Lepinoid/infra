package main

import (
	"context"
	"errors"
	"slices"
	"testing"
)

var rconPasswordReadArgv = []string{"exec", "build-server-test", "-c", "minecraft", "--", "sh", "-c", "sed -n 's/^rcon.password=//p' /data/server.properties"}

// Given: the minecraft container's server.properties holds the live rcon password
// When: an rcon path executes (B2 player list / B3 checkpoint / whitelist)
// Then: the password is read from /data/server.properties first and rcon-cli is
// invoked as rcon-cli --password <pw> <args...>
func TestRCONPassesServerPropertiesPassword(t *testing.T) {
	var calls []runnerCall
	x := &tx{ctx: context.Background(), podName: "build-server-test", podUID: "uid-1"}
	x.commands = fakeRunner{calls: &calls, reply: func(name string, args []string) ([]byte, error) {
		if name == "kubectl" && slices.Equal(args, rconPasswordReadArgv) {
			return []byte("live-secret\n"), nil
		}
		if name == "kubectl" && len(args) >= 6 && args[5] == "rcon-cli" {
			return []byte("ok\n"), nil
		}
		t.Errorf("unexpected command: %s %q", name, args)
		return nil, errors.New("unexpected command")
	}}
	if _, err := x.rcon("list"); err != nil {
		t.Fatal(err)
	}
	if _, err := whitelistCommand(x)(context.Background(), "on"); err != nil {
		t.Fatal(err)
	}
	var rconCalls int
	for i, call := range calls {
		argv := call.args[5:]
		if slices.Equal(call.args, rconPasswordReadArgv) {
			continue
		}
		if argv[0] != "rcon-cli" {
			t.Fatalf("unexpected command: %+v", call)
		}
		rconCalls++
		if argv[1] != "--password" || argv[2] != "live-secret" {
			t.Fatalf("rcon-cli called without the server.properties password: %q", argv)
		}
		if !slices.Equal(calls[i-1].args, rconPasswordReadArgv) {
			t.Fatalf("password was not read immediately before rcon-cli: calls=%+v", calls[:i+1])
		}
	}
	if rconCalls != 2 {
		t.Fatalf("expected rcon() and whitelist paths, got %d rcon-cli calls: %+v", rconCalls, calls)
	}
}

// Given: the password cannot be obtained (exec failure or empty rcon.password)
// When: an rcon path executes
// Then: the error is classifiable as UNKNOWN (errRCONPassword) and no bare
// rcon-cli call is made
func TestRCONPasswordFailureIsUnknownAndSkipsRconCLI(t *testing.T) {
	for _, tc := range []struct {
		name string
		out  string
		fail bool
	}{
		{name: "exec error", fail: true},
		{name: "empty password", out: "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls []runnerCall
			x := &tx{ctx: context.Background(), podName: "build-server-test", podUID: "uid-1"}
			x.commands = fakeRunner{calls: &calls, reply: func(name string, args []string) ([]byte, error) {
				if name == "kubectl" && slices.Equal(args, rconPasswordReadArgv) {
					if tc.fail {
						return nil, errors.New("exec exited 1")
					}
					return []byte(tc.out), nil
				}
				t.Errorf("bare rcon-cli must not be called without a password: %s %q", name, args)
				return nil, errors.New("unexpected command")
			}}
			if _, err := x.rcon("list"); !errors.Is(err, errRCONPassword) {
				t.Fatalf("expected UNKNOWN-classifiable error, got %v", err)
			}
			if _, err := whitelistCommand(x)(context.Background(), "on"); !errors.Is(err, errRCONPassword) {
				t.Fatalf("expected UNKNOWN-classifiable error, got %v", err)
			}
			for _, call := range calls {
				if len(call.args) >= 6 && call.args[5] == "rcon-cli" {
					t.Fatalf("rcon-cli invoked despite password failure: %+v", call)
				}
			}
		})
	}
}
