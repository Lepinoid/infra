package ready

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/lepinoid/infra/updater/internal/fsutil"
	"github.com/lepinoid/infra/updater/internal/gate"
	"github.com/lepinoid/infra/updater/internal/journal"
)

var ErrNotReady = errors.New("updater not ready")

type Config struct {
	Root     string
	PodUID   string
	Commands []string
	Now      func() time.Time
}

func Check(c Config) (result error) {
	for _, command := range c.Commands {
		if _, err := exec.LookPath(command); err != nil {
			return err
		}
	}
	dir, err := os.MkdirTemp(c.Root, ".ready-")
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, os.RemoveAll(dir)) }()
	src := filepath.Join(dir, "write")
	dst := filepath.Join(dir, "renamed")
	if err := fsutil.WriteJSON(src, []byte(`{"schemaVersion":1}`)); err != nil {
		return err
	}
	if err := fsutil.Move(src, dst); err != nil {
		return err
	}
	data, err := os.ReadFile(dst)
	if err != nil || string(data) != `{"schemaVersion":1}` {
		return ErrNotReady
	}
	store := journal.Store{Root: c.Root}
	inventory, err := store.Inspect()
	if err != nil {
		return err
	}
	if inventory.Blocked || len(inventory.Journals) > 1 {
		return ErrNotReady
	}
	var pair journal.Pair
	if len(inventory.Journals) == 0 {
		if inventory.Flag != nil {
			return ErrNotReady
		}
		current, err := journal.Read[journal.Current](store.Path("current"))
		if err != nil {
			return err
		}
		if err := current.Manifest.Validate(); err != nil {
			return err
		}
		pair = journal.Pair{Tools: journal.Jar{Name: "LepinoidTools.jar", SHA256: current.ToolsSHA}, Multiverse: journal.Jar{Name: "Multiverse-Core.jar", SHA256: current.MultiverseSHA}}
	} else {
		j := inventory.Journals[0]
		pair = j.Candidate()
		if j.CommitCandidate == nil && (j.Phase == "INSTALL_COMPLETE" || j.Phase == "RESTART_REQUESTED" || j.Phase == "VERIFYING") {
			pair = j.Target
		}
		if j.MaintenanceRequired || inventory.Flag != nil {
			if c.Now == nil || inventory.Flag == nil {
				return ErrNotReady
			}
			status, err := journal.Read[gate.Status](store.Path("gate-status.json"))
			if err != nil {
				return err
			}
			if !status.Active(gate.Identity{PodUID: c.PodUID, TransactionID: j.TransactionID, Generation: inventory.Flag.FencingGeneration}, c.Now()) {
				return ErrNotReady
			}
		}
	}
	for _, jar := range []journal.Jar{pair.Tools, pair.Multiverse} {
		if jar.Name != "LepinoidTools.jar" && jar.Name != "Multiverse-Core.jar" {
			return ErrNotReady
		}
		if err := fsutil.VerifyJar(filepath.Join(filepath.Dir(c.Root), jar.Name), jar.SHA256); err != nil {
			return err
		}
	}
	return nil
}
