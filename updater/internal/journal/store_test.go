package journal

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBlockingDirectoryIsSeparate(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "journal/.blocking"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "journal/.blocking/record"), []byte(`{}`), 0644); err != nil {
		t.Fatal(err)
	}
	got, err := (Store{Root: root}).Inspect()
	if err != nil {
		t.Fatal(err)
	}
	if !got.Blocked || len(got.Journals) != 0 {
		t.Fatal(got)
	}
}
