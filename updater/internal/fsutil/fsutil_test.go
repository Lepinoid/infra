package fsutil

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAtomicJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state")
	if err := WriteJSON(path, []byte(`{"schemaVersion":1}`)); err != nil {
		t.Fatal(err)
	}
	if err := WriteJSON(path, []byte(`{`)); err == nil {
		t.Fatal("accepted invalid JSON")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"schemaVersion":1}` {
		t.Fatal(string(got))
	}
}

func TestRejectSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	link := filepath.Join(dir, "link")
	if err := os.WriteFile(target, []byte("safe"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := SHA256(link); err == nil {
		t.Fatal("followed symlink")
	}
	if err := WriteJSON(link, []byte(`{}`)); err == nil {
		t.Fatal("replaced symlink")
	}
}

func TestMoveAtomic(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	if err := os.WriteFile(src, []byte("jar"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := Move(src, dst); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dst)
	if err != nil || string(got) != "jar" {
		t.Fatalf("%s %v", got, err)
	}
}
