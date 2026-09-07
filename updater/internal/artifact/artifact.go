package artifact

import (
	"archive/tar"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"

	"github.com/lepinoid/infra/updater/internal/fsutil"
	"github.com/lepinoid/infra/updater/internal/journal"
)

var ErrArtifact = errors.New("artifact contract mismatch")

type Tools struct {
	File      string `json:"file"`
	Version   string `json:"version"`
	CommitSHA string `json:"commitSha"`
	SHA256    string `json:"sha256"`
}
type Multiverse struct {
	File    string `json:"file"`
	Version string `json:"version"`
	SHA256  string `json:"sha256"`
}
type Compatibility struct {
	SchemaVersion      int        `json:"schemaVersion"`
	Tools              Tools      `json:"lepinoidTools"`
	Multiverse         Multiverse `json:"multiverseCore"`
	SupportedMinecraft []string   `json:"supportedMinecraft"`
}

func Extract(reader io.Reader, directory string) error {
	tr := tar.NewReader(reader)
	seen := map[string]bool{}
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if !slices.Contains([]string{"LepinoidTools.jar", "Multiverse-Core.jar", "compatibility.json"}, header.Name) || seen[header.Name] || header.Typeflag != tar.TypeReg || header.Size < 0 || header.Size > 128<<20 {
			return ErrArtifact
		}
		seen[header.Name] = true
		path := filepath.Join(directory, header.Name)
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(f, tr)
		syncErr := f.Sync()
		closeErr := f.Close()
		if err := errors.Join(copyErr, syncErr, closeErr); err != nil {
			return err
		}
	}
	if len(seen) != 3 {
		return ErrArtifact
	}
	return fsutil.SyncDir(directory)
}

func Verify(directory string, m journal.Manifest) error {
	if err := m.Validate(); err != nil {
		return err
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	if len(entries) != 3 {
		return ErrArtifact
	}
	metadata, err := journal.Read[Compatibility](filepath.Join(directory, "compatibility.json"))
	if err != nil {
		return err
	}
	if metadata.Tools.File != "LepinoidTools.jar" || metadata.Multiverse.File != "Multiverse-Core.jar" || metadata.Tools.Version != m.Version || metadata.Tools.CommitSHA != m.PluginCommitSHA || metadata.Tools.SHA256 != m.ToolsSHA || metadata.Multiverse.SHA256 != m.MultiverseSHA || metadata.Multiverse.Version != m.MultiverseVersion || !slices.Equal(metadata.SupportedMinecraft, m.SupportedMinecraft) {
		return ErrArtifact
	}
	for _, jar := range []journal.Jar{{Name: "LepinoidTools.jar", SHA256: m.ToolsSHA}, {Name: "Multiverse-Core.jar", SHA256: m.MultiverseSHA}} {
		hash, err := fsutil.SHA256(filepath.Join(directory, jar.Name))
		if err != nil {
			return err
		}
		if hash != jar.SHA256 {
			return ErrArtifact
		}
	}
	return nil
}
