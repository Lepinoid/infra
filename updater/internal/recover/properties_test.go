package recover

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPersistedCASPreservesOtherKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.properties")
	if err := os.WriteFile(path, []byte("motd=hello\nwhite-list=false\nport=25565\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := SetWhitelist(path, false, true); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "motd=hello\nwhite-list=true\nport=25565\n" {
		t.Fatal(string(got))
	}
	if err := SetWhitelist(path, false, true); err == nil {
		t.Fatal("CAS conflict accepted")
	}
}
