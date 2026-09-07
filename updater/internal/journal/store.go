package journal

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/lepinoid/infra/updater/internal/fsutil"
)

type Store struct{ Root string }
type Inventory struct {
	Blocked          bool
	Journals         []Journal
	Recovery         []Recovery
	Flag             *Flag
	Probes           []Probe
	ProbeInterrupted bool
}

func Read[T any](path string) (T, error) {
	var zero T
	info, err := os.Lstat(path)
	if err != nil {
		return zero, err
	}
	if !info.Mode().IsRegular() {
		return zero, fsutil.ErrUnsafe
	}
	f, err := os.Open(path)
	if err != nil {
		return zero, err
	}
	defer f.Close()
	return Decode[T](f)
}

func Save[T any](path string, value T) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if _, err := Decode[T](bytes.NewReader(data)); err != nil {
		return err
	}
	return fsutil.WriteJSON(path, data)
}

func (s Store) Path(parts ...string) string {
	return filepath.Join(append([]string{s.Root}, parts...)...)
}

func entries(path string) ([]os.DirEntry, error) {
	list, err := os.ReadDir(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return list, err
}

func (s Store) Inspect() (Inventory, error) {
	var state Inventory
	blocks, err := entries(s.Path("journal/.blocking"))
	if err != nil {
		return state, err
	}
	if len(blocks) > 0 {
		state.Blocked = true
		return state, nil
	}
	journals, err := entries(s.Path("journal"))
	if err != nil {
		return state, err
	}
	for _, entry := range journals {
		if entry.Name() == ".blocking" && entry.IsDir() {
			continue
		}
		if entry.IsDir() {
			return state, ErrSchema
		}
		if strings.HasSuffix(entry.Name(), ".probe") {
			probe, err := Read[Probe](s.Path("journal", entry.Name()))
			if err != nil {
				return state, err
			}
			if err := probe.Validate(); err != nil {
				return state, err
			}
			if probe.TransactionID+".probe" != entry.Name() {
				return state, ErrSchema
			}
			state.Probes = append(state.Probes, probe)
			if probe.RuntimeBefore == nil {
				state.ProbeInterrupted = true
			}
			continue
		}
		j, err := Read[Journal](s.Path("journal", entry.Name()))
		if err != nil {
			return state, err
		}
		if err := j.Validate(); err != nil {
			return state, err
		}
		if j.TransactionID != entry.Name() {
			return state, ErrSchema
		}
		state.Journals = append(state.Journals, j)
		if j.Outcome != nil && *j.Outcome == "FAILED_MANUAL_INTERVENTION" && j.Resolution == nil {
			state.Blocked = true
		}
	}
	if state.Blocked {
		return state, nil
	}
	records, err := entries(s.Path("recovery"))
	if err != nil {
		return state, err
	}
	for _, entry := range records {
		record, err := Read[Recovery](s.Path("recovery", entry.Name()))
		if err != nil {
			return state, err
		}
		state.Recovery = append(state.Recovery, record)
	}
	flag, err := Read[Flag](s.Path("maintenance.flag"))
	if err == nil {
		state.Flag = &flag
	} else if !errors.Is(err, os.ErrNotExist) {
		return state, err
	}
	return state, nil
}

func (s Store) Init() error {
	for _, dir := range []string{"staging", "backup", "journal/.blocking", "recovery", "archive/blocking"} {
		if err := os.MkdirAll(s.Path(dir), 0755); err != nil {
			return err
		}
	}
	return fsutil.SyncDir(s.Root)
}
