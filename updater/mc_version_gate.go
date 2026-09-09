package main

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"

	"github.com/lepinoid/infra/updater/internal/cluster"
	"github.com/lepinoid/infra/updater/internal/status"
)

func (t *tx) mcMonitorJSON() (string, error) {
	var out []byte
	err := t.api(func(ctx context.Context) error {
		var err error
		out, err = t.commands.Exec(ctx, cluster.Pod{Name: t.podName, UID: t.podUID}, "minecraft", "mc-monitor", "status", "--host", "localhost", "--port", "25565", "--format", "json")
		return err
	})
	return string(bytes.TrimSpace(out)), err
}

func (t *tx) mcVersionGate(plan *manifestPlan) (bool, string) {
	out, err := t.mcMonitorJSON()
	if err != nil {
		return false, "mc-version-unknown"
	}
	m := status.ParseMonitor([]byte(out))
	var s status.Status
	err = t.api(func(ctx context.Context) error {
		pod := cluster.Pod{Name: t.podName, UID: t.podUID}
		data, err := t.commands.Exec(ctx, pod, "updater", "cat", "/data/plugins/LepinoidTools/updater-status.json")
		if err != nil {
			data, err = t.commands.Exec(ctx, pod, "minecraft", "cat", "/data/plugins/LepinoidTools/updater-status.json")
			if err != nil {
				return err
			}
		}
		return json.Unmarshal(bytes.TrimSpace(data), &s)
	})
	if err != nil {
		return false, "mc-version-unknown"
	}
	version := s.Minecraft(m, t.j.ExpectedPodUID, t.now())
	if version == "" {
		return false, "mc-version-unknown"
	}
	if !slices.Contains(plan.Desired.SupportedMinecraft, version) {
		return false, "mc-version-mismatch"
	}
	return true, ""
}

func (t *tx) suspendMCVersion(reason string) error {
	if err := t.j.Suspend(reason); err != nil {
		return err
	}
	t.engine.Journal = t.j
	return t.saveJournal()
}
