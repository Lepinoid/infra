package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestManagementCLI(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state")
	var out bytes.Buffer
	if err := dispatch(context.Background(), []string{"write-json-atomic", path}, bytes.NewBufferString(`{"schemaVersion":1}`), &out); err != nil {
		t.Fatal(err)
	}
	if err := dispatch(context.Background(), []string{"sha256", path}, bytes.NewReader(nil), &out); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 65 {
		t.Fatal(out.String())
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}

func TestUnknownCommandFails(t *testing.T) {
	if err := dispatch(context.Background(), []string{"typo"}, bytes.NewReader(nil), new(bytes.Buffer)); err == nil {
		t.Fatal("unknown command accepted")
	}
}
