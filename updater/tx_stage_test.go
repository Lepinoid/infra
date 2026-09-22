package main

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lepinoid/infra/updater/internal/artifact"
	"github.com/lepinoid/infra/updater/internal/journal"
	updaterengine "github.com/lepinoid/infra/updater/internal/updater"
)

type stageFixture struct {
	manifest     journal.Manifest
	layerDigest  string
	tarBytes     []byte
	manifestJSON []byte
}

func testSHA256(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func newStageFixture(t *testing.T) stageFixture {
	t.Helper()
	tools := []byte("fake LepinoidTools.jar content for staging")
	multiverse := []byte("fake Multiverse-Core.jar content for staging")
	manifest := journal.Manifest{
		SchemaVersion:      1,
		Version:            "2026.09.22-1.21.8-aaaaa",
		OCIRepository:      "ghcr.io/lepinoid/lepinoid-tools",
		Digest:             "sha256:" + strings.Repeat("ab", 32),
		PluginCommitSHA:    strings.Repeat("0123456789", 4),
		ToolsSHA:           testSHA256(tools),
		MultiverseSHA:      testSHA256(multiverse),
		MultiverseVersion:  "5.8.1",
		SupportedMinecraft: []string{"1.21.8"},
	}
	compatibility, err := json.Marshal(artifact.Compatibility{
		SchemaVersion:      1,
		Tools:              artifact.Tools{File: "LepinoidTools.jar", Version: manifest.Version, CommitSHA: manifest.PluginCommitSHA, SHA256: manifest.ToolsSHA},
		Multiverse:         artifact.Multiverse{File: "Multiverse-Core.jar", Version: manifest.MultiverseVersion, SHA256: manifest.MultiverseSHA},
		SupportedMinecraft: manifest.SupportedMinecraft,
	})
	if err != nil {
		t.Fatal(err)
	}
	var blob bytes.Buffer
	tw := tar.NewWriter(&blob)
	for _, member := range []struct {
		name string
		data []byte
	}{{"LepinoidTools.jar", tools}, {"Multiverse-Core.jar", multiverse}, {"compatibility.json", compatibility}} {
		if err := tw.WriteHeader(&tar.Header{Name: member.name, Typeflag: tar.TypeReg, Mode: 0644, Size: int64(len(member.data))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(member.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	layerDigest := testSHA256(blob.Bytes())
	manifestJSON, err := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"config": map[string]any{
			"mediaType": "application/vnd.oci.empty.v1+json",
			"digest":    "sha256:" + strings.Repeat("44", 32),
			"size":      2,
		},
		"layers": []map[string]any{{
			"mediaType": "application/vnd.lepinoid.tools.bundle.layer.v1.tar",
			"digest":    "sha256:" + layerDigest,
			"size":      blob.Len(),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return stageFixture{manifest: manifest, layerDigest: "sha256:" + layerDigest, tarBytes: blob.Bytes(), manifestJSON: manifestJSON}
}

func stageTransaction(t *testing.T, m journal.Manifest) *tx {
	t.Helper()
	store := journal.Store{Root: filepath.Join(t.TempDir(), ".lepinoid")}
	if err := store.Init(); err != nil {
		t.Fatal(err)
	}
	now := func() time.Time { return time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC) }
	x := &tx{ctx: context.Background(), store: store, now: now, podName: "build-server-test", podUID: "b3e6d1f0-1a2b-4c5d-9e8f-001122334455", plan: &manifestPlan{Desired: m}}
	x.engine = &updaterengine.Engine{Store: store, Now: now, Fence: updaterengine.Fence{
		Lease: func(context.Context) error { return nil },
		Observe: func(context.Context) (updaterengine.Observation, error) {
			return updaterengine.Observation{PodUID: x.podUID, Generation: 0}, nil
		},
	}}
	return x
}

func orasStub(t *testing.T, fx stageFixture, descriptor, manifestBody, blob []byte, manifestErr error) fakeRunner {
	t.Helper()
	ref := fx.manifest.OCIRepository + "@" + fx.manifest.Digest
	var calls []runnerCall
	return fakeRunner{calls: &calls, reply: func(name string, args []string) ([]byte, error) {
		if name != "oras" {
			return nil, fmt.Errorf("unexpected command %s %q", name, args)
		}
		switch {
		case slices.Equal(args, []string{"manifest", "fetch", ref, "--descriptor"}):
			if manifestErr != nil {
				return nil, manifestErr
			}
			return descriptor, nil
		case slices.Equal(args, []string{"manifest", "fetch", ref, "--output", "-"}):
			return manifestBody, nil
		case slices.Equal(args, []string{"blob", "fetch", fx.manifest.OCIRepository + "@" + fx.layerDigest, "--output", "-"}):
			return blob, nil
		}
		return nil, fmt.Errorf("unexpected oras args %q", args)
	}}
}

// The PREPARING regression: tx.go a4Stage must resolve the manifest at the
// pinned digest and fetch the single tar layer by its blob digest. Fetching a
// blob by the *manifest* digest is the observed GHCR 404 loop (infra#36).
func TestA4StageFetchesLayerByManifestResolution(t *testing.T) {
	fx := newStageFixture(t)
	x := stageTransaction(t, fx.manifest)
	descriptor := []byte(fmt.Sprintf(`{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":%q,"size":%d}`, fx.manifest.Digest, len(fx.manifestJSON)))
	stub := orasStub(t, fx, descriptor, fx.manifestJSON, fx.tarBytes, nil)
	x.commands = stub
	if err := x.a4Stage(x.plan); err != nil {
		t.Fatal(err)
	}
	if err := artifact.Verify(x.store.Path("staging", fx.manifest.Digest), fx.manifest); err != nil {
		t.Fatalf("staged bundle does not verify: %v", err)
	}
	for _, call := range *stub.calls {
		if call.name == "oras" && len(call.args) >= 3 && call.args[0] == "blob" && call.args[1] == "fetch" && strings.HasSuffix(call.args[2], "@"+fx.manifest.Digest) {
			t.Fatalf("blob fetch used the manifest digest (the infra#36 regression): %q", call.args)
		}
	}
}

// Descriptor digest mismatch must be rejected before any blob fetch.
func TestA4StageRejectsDescriptorDigestMismatch(t *testing.T) {
	fx := newStageFixture(t)
	x := stageTransaction(t, fx.manifest)
	foreign := "sha256:" + strings.Repeat("cd", 32)
	descriptor := []byte(fmt.Sprintf(`{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":%q,"size":1}`, foreign))
	stub := orasStub(t, fx, descriptor, fx.manifestJSON, fx.tarBytes, nil)
	x.commands = stub
	if err := x.a4Stage(x.plan); err == nil {
		t.Fatal("descriptor digest mismatch accepted")
	}
	for _, call := range *stub.calls {
		if call.name == "oras" && call.args[0] == "blob" {
			t.Fatalf("blob fetch happened despite digest mismatch: %q", call.args)
		}
	}
	entries := func() []os.DirEntry {
		list, err := os.ReadDir(x.store.Path("staging", fx.manifest.Digest))
		if err != nil {
			t.Fatal(err)
		}
		return list
	}()
	if len(entries) != 0 {
		t.Fatalf("staging populated despite rejection: %d entries", len(entries))
	}
}

// A manifest that is not exactly one uncompressed tar layer is unsupported:
// gzip layers would silently corrupt artifact.Extract (plain tar reader).
func TestA4StageRejectsNonSingleTarLayerManifest(t *testing.T) {
	for _, tc := range []struct {
		name string
		body func(fx stageFixture) []byte
	}{
		{"two layers", func(fx stageFixture) []byte {
			var m map[string]any
			if err := json.Unmarshal(fx.manifestJSON, &m); err != nil {
				t.Fatal(err)
			}
			layers := m["layers"].([]any)
			m["layers"] = append(layers, layers[0])
			body, err := json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			return body
		}},
		{"gzip layer", func(fx stageFixture) []byte {
			var m map[string]any
			if err := json.Unmarshal(fx.manifestJSON, &m); err != nil {
				t.Fatal(err)
			}
			m["layers"].([]any)[0].(map[string]any)["mediaType"] = "application/vnd.oci.image.layer.v1.tar+gzip"
			body, err := json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			return body
		}},
		{"oci standard tar layer", func(fx stageFixture) []byte {
			// The publish contract is application/vnd.lepinoid.tools.bundle.layer.v1.tar;
			// even the OCI standard uncompressed tar type must be rejected.
			var m map[string]any
			if err := json.Unmarshal(fx.manifestJSON, &m); err != nil {
				t.Fatal(err)
			}
			m["layers"].([]any)[0].(map[string]any)["mediaType"] = "application/vnd.oci.image.layer.v1.tar"
			body, err := json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			return body
		}},
		{"non sha256 layer digest", func(fx stageFixture) []byte {
			var m map[string]any
			if err := json.Unmarshal(fx.manifestJSON, &m); err != nil {
				t.Fatal(err)
			}
			m["layers"].([]any)[0].(map[string]any)["digest"] = "md5:zzz"
			body, err := json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			return body
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newStageFixture(t)
			x := stageTransaction(t, fx.manifest)
			descriptor := []byte(fmt.Sprintf(`{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":%q,"size":1}`, fx.manifest.Digest))
			x.commands = orasStub(t, fx, descriptor, tc.body(fx), fx.tarBytes, nil)
			if err := x.a4Stage(x.plan); err == nil {
				t.Fatal("non-conforming manifest accepted")
			}
		})
	}
}

// Publish-contract regression (infra#36 round-trip 1): LepinoidTools
// ci/publish-bundle.sh publishes the layer as
// application/vnd.lepinoid.tools.bundle.layer.v1.tar. Assuming the OCI
// standard tar type rejected the real bundle and stopped staging in
// production. The mediaType is spelled out explicitly here so the contract
// under test does not silently drift with the shared fixture.
func TestA4StageAcceptsPublishContractLayerMediaType(t *testing.T) {
	fx := newStageFixture(t)
	x := stageTransaction(t, fx.manifest)
	var m map[string]any
	if err := json.Unmarshal(fx.manifestJSON, &m); err != nil {
		t.Fatal(err)
	}
	m["layers"].([]any)[0].(map[string]any)["mediaType"] = "application/vnd.lepinoid.tools.bundle.layer.v1.tar"
	manifestBody, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	descriptor := []byte(fmt.Sprintf(`{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":%q,"size":%d}`, fx.manifest.Digest, len(manifestBody)))
	x.commands = orasStub(t, fx, descriptor, manifestBody, fx.tarBytes, nil)
	if err := x.a4Stage(x.plan); err != nil {
		t.Fatalf("publish-contract layer mediaType rejected: %v", err)
	}
	if err := artifact.Verify(x.store.Path("staging", fx.manifest.Digest), fx.manifest); err != nil {
		t.Fatalf("staged bundle does not verify: %v", err)
	}
}

// AC regression: when staging fetch fails, journal/ must hold no zero-byte or
// partially-written JSON. The active PREPARING journal stays a fully valid,
// decodable record; no temp leftovers remain.
func TestA4StageFailureLeavesJournalDirectoryClean(t *testing.T) {
	fx := newStageFixture(t)
	x := stageTransaction(t, fx.manifest)
	j := journal.Journal{
		SchemaVersion: 1, TransactionID: "550e8400-e29b-41d4-a716-446655440000", Phase: "PREPARING",
		Lifecycle: "ACTIVE", AccessState: "OPEN", ExpectedPodUID: x.podUID, TargetManifest: fx.manifest,
		SourceDigest:      "sha256:" + strings.Repeat("cd", 32),
		TargetDigest:      fx.manifest.Digest,
		Source:            journal.Pair{Tools: journal.Jar{Name: "LepinoidTools.jar", SHA256: strings.Repeat("11", 32)}, Multiverse: journal.Jar{Name: "Multiverse-Core.jar", SHA256: strings.Repeat("22", 32)}},
		Target:            sourcePairFromManifest(fx.manifest),
		StagingPath:       filepath.Join("staging", fx.manifest.Digest),
		BackupPath:        filepath.Join("backup", "sha256:"+strings.Repeat("cd", 32)),
		CommitCandidate:   ptr("TARGET"),
		FencingGeneration: 0,
	}
	x.j, x.engine.Journal = j, j
	if err := x.saveJournal(); err != nil {
		t.Fatal(err)
	}
	x.commands = orasStub(t, fx, nil, nil, nil, errors.New("ghcr returned 404"))
	if err := x.a4Stage(x.plan); err == nil {
		t.Fatal("staging fetch unexpectedly succeeded")
	}
	entries, err := os.ReadDir(x.store.Path("journal"))
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		count++
		info, err := entry.Info()
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() == 0 {
			t.Errorf("zero-byte leftover journal/%s", entry.Name())
		}
		if strings.HasPrefix(entry.Name(), ".") {
			t.Errorf("temp leftover journal/%s", entry.Name())
		}
		read, err := journal.Read[journal.Journal](x.store.Path("journal", entry.Name()))
		if err != nil {
			t.Errorf("partial/undecodable journal/%s: %v", entry.Name(), err)
			continue
		}
		if err := read.Validate(); err != nil {
			t.Errorf("invalid journal/%s: %v", entry.Name(), err)
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly the active journal entry, got %d", count)
	}
}

// The journal dir must stay inspectable after a failed staging fetch so that
// the next CronJob run resumes the same transaction instead of wedging.
func TestA4StageFailureKeepsStoreInspectable(t *testing.T) {
	fx := newStageFixture(t)
	x := stageTransaction(t, fx.manifest)
	j := journal.Journal{
		SchemaVersion: 1, TransactionID: "550e8400-e29b-41d4-a716-446655440000", Phase: "PREPARING",
		Lifecycle: "ACTIVE", AccessState: "OPEN", ExpectedPodUID: x.podUID, TargetManifest: fx.manifest,
		SourceDigest:      "sha256:" + strings.Repeat("cd", 32),
		TargetDigest:      fx.manifest.Digest,
		Source:            journal.Pair{Tools: journal.Jar{Name: "LepinoidTools.jar", SHA256: strings.Repeat("11", 32)}, Multiverse: journal.Jar{Name: "Multiverse-Core.jar", SHA256: strings.Repeat("22", 32)}},
		Target:            sourcePairFromManifest(fx.manifest),
		StagingPath:       filepath.Join("staging", fx.manifest.Digest),
		BackupPath:        filepath.Join("backup", "sha256:"+strings.Repeat("cd", 32)),
		CommitCandidate:   ptr("TARGET"),
		FencingGeneration: 0,
	}
	x.j, x.engine.Journal = j, j
	if err := x.saveJournal(); err != nil {
		t.Fatal(err)
	}
	x.commands = orasStub(t, fx, nil, nil, nil, errors.New("ghcr returned 404"))
	if err := x.a4Stage(x.plan); err == nil {
		t.Fatal("staging fetch unexpectedly succeeded")
	}
	inv, err := x.store.Inspect()
	if err != nil {
		t.Fatalf("store no longer inspectable after failed staging: %v", err)
	}
	if len(inv.Journals) != 1 || inv.Journals[0].TransactionID != j.TransactionID || inv.Journals[0].Phase != "PREPARING" {
		t.Fatalf("active journal lost: %+v", inv.Journals)
	}
}
