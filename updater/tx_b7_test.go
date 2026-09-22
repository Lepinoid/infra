package main

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// Given: a transaction that reached INSTALL_COMPLETE
// When: b7 annotates the deployment restart
// Then: kubectl receives the patch via -p argument; --patch-file is never used
// (kubectl 1.34 does not interpret --patch-file=- as stdin)
func TestB7AnnotateRestartPassesPatchAsArgument(t *testing.T) {
	x := gateTransaction(t)
	var calls []runnerCall
	x.commands = fakeRunner{calls: &calls, reply: func(name string, args []string) ([]byte, error) {
		if name == "kubectl" && slices.Contains(args, "patch") {
			return nil, nil
		}
		t.Errorf("unexpected command: %s %q", name, args)
		return nil, errors.New("unexpected command")
	}}

	if err := x.b7AnnotateRestart(x.plan); err != nil {
		t.Fatal(err)
	}

	for _, call := range calls {
		for _, a := range call.args {
			if strings.HasPrefix(a, "--patch-file") {
				t.Fatalf("stdin-based patch is unsupported: %q", call.args)
			}
		}
	}
	for _, call := range calls {
		i := slices.Index(call.args, "-p")
		if i < 0 || i+1 >= len(call.args) {
			continue
		}
		if !strings.Contains(call.args[i+1], `"lepinoid.dev/restart-transaction":"`+x.j.TransactionID+`"`) {
			t.Fatalf("patch missing restart annotation: %s", call.args[i+1])
		}
		want := []string{"patch", "deployment", "build-server", "--type=strategic", "-p"}
		if !slices.Equal(call.args[:i+1], want) {
			t.Fatalf("unexpected patch argv: %q", call.args)
		}
		return
	}
	t.Fatalf("no -p patch call recorded: %+v", calls)
}
