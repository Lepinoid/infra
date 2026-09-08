package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lepinoid/infra/updater/internal/artifact"
	"github.com/lepinoid/infra/updater/internal/cluster"
	"github.com/lepinoid/infra/updater/internal/fsutil"
	"github.com/lepinoid/infra/updater/internal/journal"
	"github.com/lepinoid/infra/updater/internal/lease"
	"github.com/lepinoid/infra/updater/internal/players"
	"github.com/lepinoid/infra/updater/internal/status"
	updaterengine "github.com/lepinoid/infra/updater/internal/updater"
)

const pluginsDir = "/data/plugins"
const constRoot = "/data/plugins/.lepinoid"

type tx struct {
	ctx      context.Context
	commands cluster.Commands
	store    journal.Store
	engine   *updaterengine.Engine
	now      func() time.Time
	podName  string
	podUID   string
	j        journal.Journal
	plan     *manifestPlan
}

type manifestPlan struct {
	Desired                 journal.Manifest
	DesiredResourceVersion  string
	Current                 journal.Current
	Same                    bool
	ObservedGeneration      int64
	DeploymentRestartNeeded bool
}

var (
	errNoChange       = errors.New("no updater work required")
	errAPIUnavailable = errors.New("api-unavailable")
)

func ptr[T any](v T) *T { return &v }

func (t *tx) saveJournal() error { return t.engine.Save(t.ctx) }

func (t *tx) api(fn func(context.Context) error) error {
	bounded, cancel := context.WithTimeout(t.ctx, 30*time.Second)
	defer cancel()
	if err := fn(bounded); err != nil {
		return fmt.Errorf("%w: %v", errAPIUnavailable, err)
	}
	return nil
}

func (t *tx) rcon(args ...string) (string, error) {
	var out string
	err := t.api(func(ctx context.Context) error {
		body, err := t.commands.Exec(ctx, cluster.Pod{Name: t.podName, UID: t.podUID}, "minecraft", append([]string{"rcon-cli"}, args...)...)
		out = string(bytes.TrimSpace(body))
		return err
	})
	return out, err
}

func (t *tx) mcMonitor() (string, error) {
	out, err := t.commands.Run(t.ctx, nil, "mc-monitor", "status", "--host", "localhost", "--port", "25565")
	return string(bytes.TrimSpace(out)), err
}

func whitelistCommand(t *tx) updaterengine.WhitelistCommand {
	return func(ctx context.Context, sub string) (string, error) {
		bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		out, err := t.commands.Exec(bounded, cluster.Pod{Name: t.podName, UID: t.podUID}, "minecraft", "rcon-cli", "whitelist", sub)
		if err != nil {
			return "", err
		}
		return string(bytes.TrimSpace(out)), nil
	}
}

func (t *tx) a1Inventory() (journal.Inventory, error) {
	inv, err := t.store.Inspect()
	if err != nil {
		return inv, err
	}
	if inv.Blocked {
		return inv, updaterengine.ErrBlocked
	}
	var active []journal.Journal
	for _, j := range inv.Journals {
		if j.Lifecycle == "TERMINAL" && j.Outcome != nil && *j.Outcome == "FAILED_MANUAL_INTERVENTION" && j.Resolution == nil {
			return inv, updaterengine.ErrBlocked
		}
		if j.Lifecycle != "TERMINAL" {
			active = append(active, j)
		}
	}
	if len(active) > 1 {
		if err := t.writeBlocking("invariant-violation", "multiple non-terminal journals", journalIDs(inv.Journals)); err != nil {
			return inv, err
		}
		return inv, updaterengine.ErrBlocked
	}
	return inv, nil
}

func journalIDs(list []journal.Journal) []string {
	ids := make([]string, 0, len(list))
	for _, j := range list {
		ids = append(ids, j.TransactionID)
	}
	return ids
}

func (t *tx) writeBlocking(reason, detail string, observed []string) error {
	record := journal.Blocking{SchemaVersion: 1, Kind: "BLOCKING", Reason: reason, Detail: detail, ObservedJournalIDs: observed, DetectedAt: t.now().UTC(), DetectedByJobUID: t.podUID}
	return journal.Save(t.store.Path("journal", ".blocking", record.DetectedAt.Format("20060102T150405Z")), record)
}

func (t *tx) a2Manifest() (*manifestPlan, error) {
	var plan manifestPlan
	var cfg struct {
		Metadata struct {
			ResourceVersion string `json:"resourceVersion"`
		} `json:"metadata"`
		Data map[string]string `json:"data"`
	}
	err := t.api(func(ctx context.Context) error {
		data, err := t.commands.Kubectl(ctx, nil, "get", "configmap", "plugin-versions", "-o", "json")
		if err != nil {
			return err
		}
		return json.Unmarshal(data, &cfg)
	})
	if err != nil {
		return nil, err
	}
	plan.DesiredResourceVersion = cfg.Metadata.ResourceVersion
	if err := json.Unmarshal([]byte(cfg.Data["desired.json"]), &plan.Desired); err != nil {
		return nil, journal.ErrSchema
	}
	if err := plan.Desired.Validate(); err != nil {
		return nil, err
	}
	plan.Desired.ConfigMapResourceVersion = plan.DesiredResourceVersion

	switch current, err := journal.Read[journal.Current](t.store.Path("current")); {
	case err == nil:
		if err := current.Validate(); err != nil {
			return nil, err
		}
		plan.Current = current
	case errors.Is(err, os.ErrNotExist):
		if err := t.bootstrapCurrent(&plan); err != nil {
			return nil, err
		}
	default:
		return nil, err
	}
	plan.Same = journal.SameIdentity(plan.Desired, plan.Current.Manifest) && plan.Desired.Digest == plan.Current.Digest

	var generation, observed int64
	err = t.api(func(ctx context.Context) error {
		data, err := t.commands.Kubectl(ctx, nil, "get", "deployment", "build-server", "-o", "json")
		if err != nil {
			return err
		}
		var deploy struct {
			Metadata struct {
				Generation int64 `json:"generation"`
			} `json:"metadata"`
			Status struct {
				ObservedGeneration int64 `json:"observedGeneration"`
			} `json:"status"`
		}
		if err := json.Unmarshal(data, &deploy); err != nil {
			return err
		}
		generation, observed = deploy.Metadata.Generation, deploy.Status.ObservedGeneration
		return nil
	})
	if err != nil {
		return nil, err
	}
	plan.ObservedGeneration = generation
	plan.DeploymentRestartNeeded = observed < generation
	return &plan, nil
}

func (t *tx) bootstrapCurrent(plan *manifestPlan) error {
	pair, err := livePair()
	if err != nil {
		return err
	}
	plan.Current = journal.Current{Manifest: plan.Desired, InstalledAt: t.now().UTC()}
	plan.Current.ToolsSHA = pair.Tools.SHA256
	plan.Current.MultiverseSHA = pair.Multiverse.SHA256
	if err := journal.Save(t.store.Path("current"), plan.Current); err != nil {
		return err
	}
	plan.Same = false
	return nil
}

func livePair() (journal.Pair, error) {
	var pair journal.Pair
	tools, err := fsutil.SHA256(filepath.Join(pluginsDir, "LepinoidTools.jar"))
	if err != nil {
		return pair, err
	}
	multiverse, err := fsutil.SHA256(filepath.Join(pluginsDir, "Multiverse-Core.jar"))
	if err != nil {
		return pair, err
	}
	pair.Tools = journal.Jar{Name: "LepinoidTools.jar", SHA256: tools}
	pair.Multiverse = journal.Jar{Name: "Multiverse-Core.jar", SHA256: multiverse}
	return pair, nil
}

func (t *tx) a3Drift(plan *manifestPlan) error {
	pair, err := livePair()
	if err == nil && pair.Tools.SHA256 == plan.Current.ToolsSHA && pair.Multiverse.SHA256 == plan.Current.MultiverseSHA {
		if plan.Same {
			return errNoChange
		}
		return nil
	}
	t.j = journal.Journal{
		SchemaVersion:     1,
		TransactionID:     newUUID(t.now),
		Phase:             "DRIFT_DETECTED",
		Lifecycle:         "ACTIVE",
		AccessState:       "OPEN",
		ExpectedPodUID:    t.podUID,
		TargetManifest:    plan.Desired,
		SourceDigest:      plan.Current.Digest,
		TargetDigest:      plan.Desired.Digest,
		Source:            sourcePairFromManifest(plan.Current.Manifest),
		Target:            sourcePairFromManifest(plan.Desired),
		StagingPath:       filepath.Join("staging", plan.Desired.Digest),
		BackupPath:        filepath.Join("backup", plan.Current.Digest),
		CommitCandidate:   ptr("TARGET"),
		FencingGeneration: plan.ObservedGeneration,
	}
	t.engine.Journal = t.j
	return t.saveJournal()
}

func sourcePairFromManifest(m journal.Manifest) journal.Pair {
	return journal.Pair{
		Tools:      journal.Jar{Name: "LepinoidTools.jar", SHA256: m.ToolsSHA},
		Multiverse: journal.Jar{Name: "Multiverse-Core.jar", SHA256: m.MultiverseSHA},
	}
}

func (t *tx) a4Stage(plan *manifestPlan) error {
	dir := t.store.Path("staging", plan.Desired.Digest)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	if err := artifact.Verify(dir, plan.Desired); err == nil {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(dir, entry.Name())); err != nil {
			return err
		}
	}
	var blob []byte
	if err := t.api(func(ctx context.Context) error {
		data, err := t.commands.Run(ctx, nil, "oras", "blob", "fetch", plan.Desired.OCIRepository+"@"+plan.Desired.Digest, "--output", "-")
		if err != nil {
			return err
		}
		blob = data
		return nil
	}); err != nil {
		return err
	}
	if err := artifact.Extract(bytes.NewReader(blob), dir); err != nil {
		return err
	}
	return artifact.Verify(dir, plan.Desired)
}

func (t *tx) a5GateClosed() error {
	if err := t.engine.Flag(t.ctx); err != nil {
		return err
	}
	return t.engine.WaitGate(t.ctx, true)
}

func (t *tx) a6Probe() error {
	return t.engine.ProbeRuntime(t.ctx, whitelistCommand(t))
}

func (t *tx) observePlayers() (players.State, error) {
	rconOut, err1 := t.rcon("list")
	monitorOut, err2 := t.mcMonitor()
	if err1 != nil || err2 != nil {
		return players.Unknown, nil
	}
	return players.Decide(players.RCON(rconOut), players.Monitor(monitorOut)), nil
}

func (t *tx) b3EmptyPlayers() error {
	deadline := t.now().Add(10 * time.Minute)
	var zeroSince time.Time
	for {
		if !t.now().Before(deadline) {
			return t.engine.Suspend(t.ctx, "checkpoint-timeout")
		}
		state, err := t.observePlayers()
		if err != nil {
			return err
		}
		switch state {
		case players.Unknown:
			return t.engine.Suspend(t.ctx, "player-count-unknown")
		case players.NonZero:
			zeroSince = time.Time{}
		case players.Zero:
			if zeroSince.IsZero() {
				zeroSince = t.now()
			}
			if t.now().Sub(zeroSince) >= time.Minute {
				return nil
			}
		}
		select {
		case <-t.ctx.Done():
			return t.ctx.Err()
		case <-time.After(15 * time.Second):
		}
	}
}

func (t *tx) b4Checkpoint() error {
	deadline := t.now().Add(60 * time.Second)
	for {
		if !t.now().Before(deadline) {
			return t.engine.Suspend(t.ctx, "checkpoint-timeout")
		}
		out, err := t.rcon("lepinoidtools", "updater", "checkpoint")
		if err != nil {
			return err
		}
		id, reason := status.Checkpoint(out)
		if id != "" {
			return nil
		}
		switch reason {
		case "checkpoint-busy", "checkpoint-unavailable":
			select {
			case <-t.ctx.Done():
				return t.ctx.Err()
			case <-time.After(5 * time.Second):
			}
		default:
			return t.engine.Suspend(t.ctx, reason)
		}
	}
}

func (t *tx) b5Backup(plan *manifestPlan) error {
	dir := t.store.Path("backup", plan.Current.Digest)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	metadata := journal.Metadata{SchemaVersion: 1, Digest: plan.Current.Digest, Pair: sourcePairFromManifest(plan.Current.Manifest)}
	metadata.BackedUpAt = ptr(t.now().UTC())
	for _, jar := range []journal.Jar{metadata.Pair.Tools, metadata.Pair.Multiverse} {
		data, err := os.ReadFile(filepath.Join(pluginsDir, jar.Name))
		if err != nil {
			return err
		}
		if err := fsutil.Write(filepath.Join(dir, jar.Name), data); err != nil {
			return err
		}
	}
	whitelist := filepath.Join("/data", "whitelist.json")
	if data, err := os.ReadFile(whitelist); err == nil {
		if err := fsutil.Write(filepath.Join(dir, "whitelist.json"), data); err != nil {
			return err
		}
		t.j.Maintenance.WhitelistBackup = ptr(base64.StdEncoding.EncodeToString(data))
		sum, err := fsutil.SHA256(whitelist)
		if err != nil {
			return err
		}
		t.j.Maintenance.WhitelistChecksum = ptr(sum)
		t.j.Maintenance.WhitelistExisted = ptr(true)
	} else if errors.Is(err, os.ErrNotExist) {
		t.j.Maintenance.WhitelistExisted = ptr(false)
	} else {
		return err
	}
	persisted, err := readPersistedWhitelist("/data/server.properties")
	if err != nil {
		return err
	}
	t.j.Maintenance.PersistedEnabled = ptr(persisted)
	t.j.Maintenance.StartedAt = ptr(t.now().UTC())
	if err := journal.Save(filepath.Join(dir, "metadata.json"), metadata); err != nil {
		return err
	}
	return t.saveJournal()
}

func (t *tx) b6Install(plan *manifestPlan) error {
	return t.engine.InstallPair(t.ctx, t.store.Path("staging", plan.Desired.Digest), sourcePairFromManifest(plan.Desired))
}

func (t *tx) b7AnnotateRestart(plan *manifestPlan) error {
	generation := plan.ObservedGeneration + 1
	t.j.ExpectedTemplateGeneration = ptr(generation)
	requested := t.now().UTC()
	t.j.RestartRequestedAt = &requested
	t.j.PreviousPodUID = ptr(t.podUID)
	if err := t.saveJournal(); err != nil {
		return err
	}
	return t.api(func(ctx context.Context) error {
		patch, err := json.Marshal(map[string]any{
			"spec": map[string]any{
				"template": map[string]any{
					"metadata": map[string]any{
						"annotations": map[string]string{
							"lepinoid.dev/restart-transaction":  t.j.TransactionID,
							"lepinoid.dev/restart-requested-at": requested.Format(time.RFC3339Nano),
						},
					},
				},
			},
		})
		if err != nil {
			return err
		}
		_, err = t.commands.Kubectl(ctx, patch, "patch", "deployment", "build-server", "--type=strategic", "--patch-file=-")
		return err
	})
}

func (t *tx) b8Rollout() error {
	deadline := t.now().Add(10 * time.Minute)
	for t.now().Before(deadline) {
		var list struct {
			Items []struct {
				Metadata struct {
					Name string `json:"name"`
					UID  string `json:"uid"`
				} `json:"metadata"`
				Status struct {
					Phase      string `json:"phase"`
					Containers []struct {
						Ready bool `json:"ready"`
					} `json:"containerStatuses"`
				} `json:"status"`
			} `json:"items"`
		}
		err := t.api(func(ctx context.Context) error {
			data, err := t.commands.Kubectl(ctx, nil, "get", "pods", "-l", "app=build-server", "-o", "json")
			if err != nil {
				return err
			}
			return json.Unmarshal(data, &list)
		})
		if err != nil {
			t.engine.Journal = t.j
			return t.engine.Suspend(t.ctx, "api-unavailable")
		}
		for _, pod := range list.Items {
			if pod.Metadata.UID == t.podUID || pod.Status.Phase != "Running" {
				continue
			}
			ready := len(pod.Status.Containers) > 0
			for _, c := range pod.Status.Containers {
				ready = ready && c.Ready
			}
			if ready {
				t.j.ReplacementPodUID = ptr(pod.Metadata.UID)
				t.j.ExpectedPodUID = pod.Metadata.UID
				t.engine.Journal = t.j
				return t.saveJournal()
			}
		}
		select {
		case <-t.ctx.Done():
			return t.ctx.Err()
		case <-time.After(10 * time.Second):
		}
	}
	t.engine.Journal = t.j
	return t.engine.Suspend(t.ctx, "checkpoint-timeout")
}

func (t *tx) b9Healthy(plan *manifestPlan) error {
	deadline := t.now().Add(10 * time.Minute)
	for t.now().Before(deadline) {
		var s status.Status
		readErr := t.api(func(ctx context.Context) error {
			data, err := t.commands.Exec(ctx, cluster.Pod{Name: t.podName, UID: t.j.ExpectedPodUID}, "updater", "cat", "/data/plugins/LepinoidTools/updater-status.json")
			if err != nil {
				data, err = t.commands.Exec(ctx, cluster.Pod{Name: t.podName, UID: t.j.ExpectedPodUID}, "minecraft", "cat", "/data/plugins/LepinoidTools/updater-status.json")
				if err != nil {
					return err
				}
			}
			return json.Unmarshal(bytes.TrimSpace(data), &s)
		})
		if readErr == nil {
			if monitor, err := t.mcMonitor(); err == nil {
				m := status.ParseMonitor([]byte(monitor))
				if m.Valid && s.Healthy(status.Startup{PodUID: t.j.ExpectedPodUID, PreviousPodUID: t.podUID, RequestedAt: *t.j.RestartRequestedAt, Manifest: plan.Desired}, t.now()) {
					return nil
				}
			}
		}
		select {
		case <-t.ctx.Done():
			return t.ctx.Err()
		case <-time.After(15 * time.Second):
		}
	}
	t.engine.Journal = t.j
	return t.engine.Suspend(t.ctx, "sidecar-unavailable")
}

func (t *tx) b11GC() error {
	gc := updaterengine.GCPlan{Protected: []string{t.j.SourceDigest, t.j.TargetDigest}}
	for _, section := range []string{"staging", "backup"} {
		entries, err := os.ReadDir(t.store.Path(section))
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if entry.IsDir() {
				gc.Generations = append(gc.Generations, updaterengine.Generation{Digest: section + "/" + entry.Name()})
			}
		}
	}
	return gc.Run(t.ctx, func(ctx context.Context) error { return t.engine.Fence.Check(ctx, t.j) }, func(path string) error {
		return os.RemoveAll(t.store.Path(path))
	})
}

func (t *tx) recoverEntry(j journal.Journal) error {
	t.j = j
	t.engine.Journal = j
	switch j.Phase {
	case "ACCESS_RESTORE_COMPLETE", "ROLLBACK_RESTART_REQUESTED":
		return nil
	default:
		return t.runActive()
	}
}

func (t *tx) runSupersede(old journal.Journal, plan *manifestPlan) error {
	aborted := "ABORTED"
	old.Lifecycle = "TERMINAL"
	old.Outcome = &aborted
	t.j = old
	t.engine.Journal = old
	if err := t.saveJournal(); err != nil {
		return err
	}
	t.j = journal.Journal{}
	return t.runTransactionActive(plan)
}

func (t *tx) runTransactionActive(plan *manifestPlan) error {
	t.plan = plan
	if t.j.TransactionID == "" {
		t.j = journal.Journal{
			SchemaVersion:     1,
			TransactionID:     newUUID(t.now),
			Phase:             "PREPARING",
			Lifecycle:         "ACTIVE",
			AccessState:       "OPEN",
			ExpectedPodUID:    t.podUID,
			TargetManifest:    plan.Desired,
			SourceDigest:      plan.Current.Digest,
			TargetDigest:      plan.Desired.Digest,
			Source:            sourcePairFromManifest(plan.Current.Manifest),
			Target:            sourcePairFromManifest(plan.Desired),
			StagingPath:       filepath.Join("staging", plan.Desired.Digest),
			BackupPath:        filepath.Join("backup", plan.Current.Digest),
			CommitCandidate:   ptr("TARGET"),
			FencingGeneration: plan.ObservedGeneration,
		}
		t.engine.Journal = t.j
		if err := t.saveJournal(); err != nil {
			return err
		}
	}
	return t.runActive()
}

func (t *tx) runActive() error {
	plan := t.plan
	if plan == nil {
		return errors.New("tx.plan unset")
	}
	type step struct {
		phase string
		run   func() error
	}
	steps := []step{
		{"PREPARING", func() error {
			if err := t.a4Stage(plan); err != nil {
				return err
			}
			t.j.Phase = "STAGED"
			t.engine.Journal = t.j
			return t.saveJournal()
		}},
		{"STAGED", func() error {
			t.j.AccessState = "CLOSING"
			t.j.Phase = "MAINTENANCE_PREPARED"
			t.engine.Journal = t.j
			return t.saveJournal()
		}},
		{"MAINTENANCE_PREPARED", func() error {
			if err := t.a5GateClosed(); err != nil {
				return err
			}
			t.j.AccessState = "CLOSED"
			t.j.Phase = "MAINTENANCE_ACTIVE"
			t.j.MaintenanceRequired = true
			t.engine.Journal = t.j
			if err := t.saveJournal(); err != nil {
				return err
			}
			return t.a6Probe()
		}},
		{"MAINTENANCE_ACTIVE", func() error {
			if err := t.b3EmptyPlayers(); err != nil {
				return err
			}
			if err := t.b4Checkpoint(); err != nil {
				return err
			}
			t.j.Phase = "BACKUP_STARTED"
			t.engine.Journal = t.j
			return t.saveJournal()
		}},
		{"BACKUP_STARTED", func() error {
			if err := t.b5Backup(plan); err != nil {
				return err
			}
			t.j.Phase = "BACKUP_COMPLETE"
			t.engine.Journal = t.j
			return t.saveJournal()
		}},
		{"BACKUP_COMPLETE", func() error {
			t.j.Phase = "INSTALL_STARTED"
			t.engine.Journal = t.j
			if err := t.saveJournal(); err != nil {
				return err
			}
			if err := t.b6Install(plan); err != nil {
				return err
			}
			t.j.Phase = "INSTALL_COMPLETE"
			t.engine.Journal = t.j
			return t.saveJournal()
		}},
		{"INSTALL_COMPLETE", func() error {
			if err := t.b7AnnotateRestart(plan); err != nil {
				return err
			}
			t.j.Phase = "RESTART_REQUESTED"
			t.engine.Journal = t.j
			return t.saveJournal()
		}},
		{"RESTART_REQUESTED", func() error {
			if err := t.b8Rollout(); err != nil {
				return err
			}
			t.j.Phase = "VERIFYING"
			t.engine.Journal = t.j
			return t.saveJournal()
		}},
		{"VERIFYING", func() error {
			if err := t.b9Healthy(plan); err != nil {
				return err
			}
			t.j.CommitCandidate = ptr("TARGET")
			t.engine.Journal = t.j
			return t.saveJournal()
		}},
		{"ACCESS_RESTORE_STARTED", t.openAccess},
	}
	for _, s := range steps {
		if order(t.j.Phase) > order(s.phase) {
			continue
		}
		if err := s.run(); err != nil {
			return err
		}
	}
	outcome := "SUCCEEDED"
	t.j.Lifecycle = "TERMINAL"
	t.j.Outcome = &outcome
	t.j.Phase = "ACCESS_RESTORE_COMPLETE"
	t.engine.Journal = t.j
	if err := t.saveJournal(); err != nil {
		return err
	}
	return t.b11GC()
}

func order(phase string) int {
	for i, p := range journal.Phases {
		if p == phase {
			return i
		}
	}
	return len(journal.Phases)
}

func (t *tx) openAccess() error {
	t.j.Phase = "ACCESS_RESTORE_STARTED"
	t.engine.Journal = t.j
	if err := t.saveJournal(); err != nil {
		return err
	}
	restore := func(ctx context.Context) error {
		target := t.j.Candidate()
		for _, jar := range []journal.Jar{target.Tools, target.Multiverse} {
			if err := fsutil.VerifyJar(filepath.Join(pluginsDir, jar.Name), jar.SHA256); err != nil {
				return err
			}
		}
		if t.j.Maintenance.WhitelistBackup != nil && t.j.Maintenance.WhitelistExisted != nil && *t.j.Maintenance.WhitelistExisted {
			data, err := base64.StdEncoding.DecodeString(*t.j.Maintenance.WhitelistBackup)
			if err != nil {
				return err
			}
			if err := fsutil.Write("/data/whitelist.json", data); err != nil {
				return err
			}
		}
		if t.j.Maintenance.PersistedEnabled != nil {
			if err := writePersistedWhitelist("/data/server.properties", *t.j.Maintenance.PersistedEnabled); err != nil {
				return err
			}
		}
		if t.j.Maintenance.RuntimeEnabled != nil && *t.j.Maintenance.RuntimeEnabled {
			if _, err := t.rcon("whitelist", "on"); err != nil {
				return err
			}
		}
		return nil
	}
	return t.engine.Open(t.ctx, restore)
}

func readPersistedWhitelist(path string) (bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "white-list="); ok {
			return rest == "true", nil
		}
	}
	return false, nil
}

func writePersistedWhitelist(path string, value bool) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	desiredLine := "white-list=false"
	if value {
		desiredLine = "white-list=true"
	}
	var lines []string
	replaced := false
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "white-list=") {
			if !replaced {
				lines = append(lines, desiredLine)
				replaced = true
			}
			continue
		}
		lines = append(lines, line)
	}
	if !replaced {
		lines = append(lines, desiredLine)
	}
	return fsutil.Write(path, []byte(strings.Join(lines, "\n")))
}

func newUUID(now func() time.Time) string {
	data, err := os.ReadFile("/proc/sys/kernel/random/uuid")
	if err != nil {
		return fmt.Sprintf("00000000-0000-4000-8000-%012x", now().UnixNano()%0xffffffffffff)
	}
	return strings.TrimSpace(string(data))
}

// runTransaction wires the Lease, PVC store, Inventory classification and the
// ACTIVE/SUSPENDED driver (cluster/run.md A0-A6 + B1-B11 統合入口).
func runTransaction(ctx context.Context, lock *lease.Lease) error {
	store := journal.Store{Root: constRoot}
	if err := store.Init(); err != nil {
		return err
	}
	t := &tx{
		ctx:      ctx,
		commands: cluster.Commands{Timeout: 20 * time.Second},
		store:    store,
		now:      time.Now,
	}
	pod, err := t.commands.Pod(ctx)
	if err != nil {
		return err
	}
	t.podName = pod.Name
	t.podUID = pod.UID
	fence := updaterengine.Fence{
		Lease: func(c context.Context) error { return lock.Check(c) },
		Observe: func(c context.Context) (updaterengine.Observation, error) {
			data, err := t.commands.Kubectl(c, nil, "get", "deployment", "build-server", "-o", "jsonpath={.metadata.generation}")
			if err != nil {
				return updaterengine.Observation{}, err
			}
			var generation int64
			if _, err := fmt.Sscanf(string(bytes.TrimSpace(data)), "%d", &generation); err != nil {
				return updaterengine.Observation{}, err
			}
			inv, err := store.Inspect()
			if err != nil {
				return updaterengine.Observation{}, err
			}
			return updaterengine.Observation{PodUID: t.podUID, Generation: generation, Blocked: inv.Blocked}, nil
		},
	}
	t.engine = &updaterengine.Engine{Store: store, Fence: fence, Now: time.Now}
	inv, err := t.a1Inventory()
	if err != nil {
		return err
	}
	plan, err := t.a2Manifest()
	if err != nil {
		return err
	}
	t.plan = plan
	for i := range inv.Journals {
		if inv.Journals[i].Lifecycle == "TERMINAL" {
			continue
		}
		if inv.Journals[i].TargetDigest == plan.Desired.Digest {
			t.engine.Journal = inv.Journals[i]
			return t.recoverEntry(inv.Journals[i])
		}
		return t.runSupersede(inv.Journals[i], plan)
	}
	if plan.Same && !plan.DeploymentRestartNeeded {
		return nil
	}
	if err := t.a3Drift(plan); err != nil {
		if errors.Is(err, errNoChange) {
			return nil
		}
		return err
	}
	return t.runTransactionActive(plan)
}
