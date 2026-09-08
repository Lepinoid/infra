package recover

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/lepinoid/infra/updater/internal/fsutil"
	"github.com/lepinoid/infra/updater/internal/journal"
)

var ErrUnrecoverable = errors.New("init-unrecoverable")

type GateManifest struct {
	SchemaVersion      int      `json:"schemaVersion"`
	File               string   `json:"file"`
	Version            string   `json:"version"`
	SHA256             string   `json:"sha256"`
	SupportedMinecraft []string `json:"supportedMinecraft"`
}
type Runner struct {
	Store  journal.Store
	Data   string
	PodUID string
	Now    func() time.Time
}

func (r Runner) Run() error {
	state, err := r.Store.Inspect()
	if err != nil {
		return err
	}
	if state.Blocked {
		return ErrUnrecoverable
	}
	if len(state.Journals) > 1 {
		return ErrUnrecoverable
	}
	if len(state.Journals) == 0 {
		if state.Flag != nil {
			return ErrUnrecoverable
		}
		current, err := journal.Read[journal.Current](r.Store.Path("current"))
		if err != nil {
			return err
		}
		return r.verifyPair(journal.Pair{Tools: journal.Jar{Name: "LepinoidTools.jar", SHA256: current.ToolsSHA}, Multiverse: journal.Jar{Name: "Multiverse-Core.jar", SHA256: current.MultiverseSHA}})
	}
	return r.Repair(state.Journals[0])
}

func (r Runner) verifyPair(pair journal.Pair) error {
	for _, jar := range []journal.Jar{pair.Tools, pair.Multiverse} {
		if jar.Name != "LepinoidTools.jar" && jar.Name != "Multiverse-Core.jar" {
			return ErrUnrecoverable
		}
		hash, err := fsutil.SHA256(filepath.Join(r.Data, "plugins", jar.Name))
		if err != nil {
			return err
		}
		if hash != jar.SHA256 {
			return ErrUnrecoverable
		}
	}
	return nil
}

func (r Runner) checksums() journal.Checksums {
	result := journal.Checksums{}
	if hash, err := fsutil.SHA256(filepath.Join(r.Data, "plugins/LepinoidTools.jar")); err == nil {
		result.Tools = &hash
	}
	if hash, err := fsutil.SHA256(filepath.Join(r.Data, "plugins/Multiverse-Core.jar")); err == nil {
		result.Multiverse = &hash
	}
	return result
}

func (r Runner) Repair(j journal.Journal) (result error) {
	flag, flagErr := journal.Read[journal.Flag](r.Store.Path("maintenance.flag"))
	if flagErr != nil && !errors.Is(flagErr, os.ErrNotExist) {
		return flagErr
	}
	if flagErr == nil && (flag.TransactionID != j.TransactionID || flag.JournalPath != "journal/"+j.TransactionID) {
		return ErrUnrecoverable
	}
	pair := j.Candidate()
	if j.Phase == "ACCESS_RESTORE_COMPLETE" {
		if j.CommitCandidate == nil || flagErr == nil {
			return ErrUnrecoverable
		}
		return r.verifyPair(pair)
	}
	if !j.MaintenanceRequired && flagErr != nil {
		return r.verifyPair(pair)
	}
	before, err := Whitelist(filepath.Join(r.Data, "server.properties"))
	if err != nil {
		return err
	}
	generation := j.FencingGeneration
	records, err := os.ReadDir(r.Store.Path("recovery"))
	if err != nil {
		return err
	}
	for _, entry := range records {
		rec, err := journal.Read[journal.Recovery](r.Store.Path("recovery", entry.Name()))
		if err != nil {
			return err
		}
		if rec.GenerationAfter > generation {
			generation = rec.GenerationAfter
		}
	}
	rec := journal.Recovery{SchemaVersion: 1, TransactionID: &j.TransactionID, Generation: generation + 1, PodUID: &j.ExpectedPodUID, ObservedPhase: &j.Phase, Before: r.checksums(), PersistedBefore: before, FlagBefore: flagErr == nil, GenerationBefore: generation, GenerationAfter: generation + 1, Result: "START_BLOCKED", Timestamp: r.Now().UTC()}
	path := r.Store.Path("recovery", fmt.Sprintf("%020d-%s", rec.GenerationAfter, r.PodUID))
	if err := journal.Save(path, rec); err != nil {
		return err
	}
	defer func() {
		after := r.checksums()
		rec.After = &after
		if result != nil {
			rec.Result = "UNRECOVERABLE"
		} else {
			rec.Result = "REPAIRED"
		}
		result = errors.Join(result, journal.Save(path, rec))
	}()
	flag = journal.Flag{SchemaVersion: 1, TransactionID: j.TransactionID, CreatedAt: r.Now().UTC(), JournalPath: "journal/" + j.TransactionID, FencingGeneration: rec.GenerationAfter}
	if err := journal.Save(r.Store.Path("maintenance.flag"), flag); err != nil {
		return err
	}
	rec.FlagAfter = true
	gate, err := journal.Read[GateManifest](r.Store.Path("gate-manifest.json"))
	if err != nil {
		return err
	}
	if gate.File != "lepinoid-tools-gate.jar" {
		return ErrUnrecoverable
	}
	if err := r.restoreJar(journal.Jar{Name: gate.File, SHA256: gate.SHA256}, r.Store.Path("backup/gate")); err != nil {
		return err
	}
	if j.Maintenance.PersistedEnabled == nil {
		return ErrUnrecoverable
	}
	if before != *j.Maintenance.PersistedEnabled {
		inherited := false
		for _, entry := range records {
			old, err := journal.Read[journal.Recovery](r.Store.Path("recovery", entry.Name()))
			if err != nil {
				return err
			}
			if old.TransactionID != nil && *old.TransactionID == j.TransactionID && old.GenerationAfter == generation && old.PersistedAfter == before && old.FlagAfter {
				inherited = true
			}
		}
		if !inherited {
			return ErrCAS
		}
	}
	if err := SetWhitelist(filepath.Join(r.Data, "server.properties"), before, true); err != nil {
		return err
	}
	rec.PersistedAfter = true
	for _, jar := range []journal.Jar{pair.Tools, pair.Multiverse} {
		backup := r.Store.Path("backup", j.SourceDigest)
		if j.CommitCandidate != nil && *j.CommitCandidate == "TARGET" {
			backup = r.Store.Path("staging", j.TargetDigest)
		}
		if err := r.restoreJar(jar, backup); err != nil {
			return err
		}
	}
	return r.verifyPair(pair)
}

func (r Runner) restoreJar(jar journal.Jar, backup string) error {
	if jar.Name != "LepinoidTools.jar" && jar.Name != "Multiverse-Core.jar" && jar.Name != "lepinoid-tools-gate.jar" {
		return ErrUnrecoverable
	}
	path := filepath.Join(r.Data, "plugins", jar.Name)
	if hash, err := fsutil.SHA256(path); err == nil && hash == jar.SHA256 {
		return nil
	}
	source := filepath.Join(backup, jar.Name)
	hash, err := fsutil.SHA256(source)
	if err != nil {
		return err
	}
	if hash != jar.SHA256 {
		return ErrUnrecoverable
	}
	data, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	return fsutil.Write(path, data)
}
