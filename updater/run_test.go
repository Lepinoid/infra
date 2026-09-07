package main

import (
	"context"
	"errors"
	"testing"
)

func TestRunRequiresIdentity(t *testing.T) {
	t.Setenv("POD_UID", "")
	if err := run(context.Background()); !errors.Is(err, errUsage) {
		t.Fatal(err)
	}
}
