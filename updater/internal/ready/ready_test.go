package ready

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMissingCurrentIsNotReady(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".lepinoid")
	if err := os.Mkdir(root, 0755); err != nil {
		t.Fatal(err)
	}
	if err := Check(Config{Root: root, PodUID: "pod", Commands: []string{"go"}}); err == nil {
		t.Fatal("unbootstrapped data accepted")
	}
}
