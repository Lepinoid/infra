package updater

import (
	"context"
	"testing"

	"github.com/lepinoid/infra/updater/internal/journal"
)

func TestJarMutationRequiresClosedAck(t *testing.T) {
	engine := Engine{Journal: journal.Journal{AccessState: "CLOSING"}}
	if err := engine.JarMutation(context.Background(), func() error { t.Fatal("mutation ran"); return nil }); err == nil {
		t.Fatal("open access accepted")
	}
}
