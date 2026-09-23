package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/lepinoid/infra/updater/internal/cluster"
	"github.com/lepinoid/infra/updater/internal/status"
	updaterengine "github.com/lepinoid/infra/updater/internal/updater"
)

// restartWait is the polling seam; cancellation always remains owned by the Job.
var restartWait = func(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type restartPod struct {
	Metadata struct {
		Name        string            `json:"name"`
		UID         string            `json:"uid"`
		Deleted     *time.Time        `json:"deletionTimestamp"`
		Annotations map[string]string `json:"annotations"`
	} `json:"metadata"`
	Spec struct {
		Containers []struct {
			Name string `json:"name"`
		} `json:"containers"`
	} `json:"spec"`
	Status struct {
		Phase      string `json:"phase"`
		Containers []struct {
			Name  string `json:"name"`
			Ready bool   `json:"ready"`
		} `json:"containerStatuses"`
	} `json:"status"`
}

func (p restartPod) ready() bool {
	if p.Status.Phase != "Running" || len(p.Spec.Containers) == 0 || len(p.Spec.Containers) != len(p.Status.Containers) {
		return false
	}
	ready := make(map[string]bool, len(p.Status.Containers))
	for _, c := range p.Status.Containers {
		ready[c.Name] = c.Ready
	}
	for _, c := range p.Spec.Containers {
		if c.Name == "" || !ready[c.Name] {
			return false
		}
	}
	return ready["minecraft"] && ready["updater"]
}

// Exec addresses a name, not a UID. Resolve both before issuing a command so a
// stale cached name cannot direct reads or RCON to a different Pod incarnation.
func (t *tx) readPod(ctx context.Context, want cluster.Pod) (restartPod, error) {
	var pod restartPod
	if want.Name == "" || want.UID == "" {
		return pod, fmt.Errorf("%w: empty pod identity", updaterengine.ErrFenced)
	}
	data, err := t.commands.Kubectl(ctx, nil, "get", "pod", want.Name, "-o", "json")
	if err != nil {
		return pod, err
	}
	if err := json.Unmarshal(data, &pod); err != nil {
		return pod, err
	}
	if pod.Metadata.Name != want.Name || pod.Metadata.UID != want.UID || pod.Metadata.Deleted != nil {
		return pod, fmt.Errorf("%w: expected pod %s/%s, observed %s/%s deleting=%t", updaterengine.ErrFenced, want.Name, want.UID, pod.Metadata.Name, pod.Metadata.UID, pod.Metadata.Deleted != nil)
	}
	return pod, nil
}

func (t *tx) currentExecPod(ctx context.Context) (cluster.Pod, error) {
	pod := cluster.Pod{Name: t.podName, UID: t.podUID}
	if t.podUID != t.j.ExpectedPodUID {
		return pod, fmt.Errorf("%w: execution pod does not match journal", updaterengine.ErrFenced)
	}
	_, err := t.readPod(ctx, pod)
	return pod, err
}

func (t *tx) mcMonitor() (string, error) {
	var out []byte
	err := t.api(func(ctx context.Context) error {
		pod, err := t.currentExecPod(ctx)
		if err != nil {
			return err
		}
		out, err = t.commands.Exec(ctx, pod, "minecraft", "mc-monitor", "status", "--host", "localhost", "--port", "25565")
		return err
	})
	return string(bytes.TrimSpace(out)), err
}

type podList struct {
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

func rolloutObservation(list *podList) string {
	if len(list.Items) == 0 {
		return "no build-server pods listed"
	}
	var parts []string
	for _, pod := range list.Items {
		ready := 0
		for _, c := range pod.Status.Containers {
			if c.Ready {
				ready++
			}
		}
		parts = append(parts, fmt.Sprintf("%s(uid=%s phase=%s ready=%d/%d)", pod.Metadata.Name, pod.Metadata.UID, pod.Status.Phase, ready, len(pod.Status.Containers)))
	}
	return strings.Join(parts, ", ")
}

func (t *tx) b8Rollout() error {
	if t.j.PreviousPodUID == nil || *t.j.PreviousPodUID == "" || t.j.RestartRequestedAt == nil || t.j.RestartRequestedAt.IsZero() || t.j.ExpectedTemplateGeneration == nil {
		return fmt.Errorf("%w: missing persisted restart identity", updaterengine.ErrFenced)
	}
	previous := *t.j.PreviousPodUID
	deadline := t.now().Add(10 * time.Minute)
	var last string
	closurePending := false
	for t.now().Before(deadline) {
		if err := t.engine.Fence.Lease(t.ctx); err != nil {
			return err
		}
		var list podList
		err := t.api(func(ctx context.Context) error {
			data, err := t.commands.Kubectl(ctx, nil, "get", "pods", "-l", "app=build-server", "-o", "json")
			if err != nil {
				return err
			}
			return json.Unmarshal(data, &list)
		})
		if err != nil {
			return t.suspend("api-unavailable")
		}
		last = rolloutObservation(&list)
		// Recreate should leave exactly one replacement. Never choose arbitrarily
		// between multiple Running replacements sharing the restart annotation.
		var candidate *restartPod
		for _, listed := range list.Items {
			if listed.Metadata.UID == previous || listed.Status.Phase != "Running" {
				continue
			}
			var pod restartPod
			err := t.api(func(ctx context.Context) error {
				var err error
				pod, err = t.readPod(ctx, cluster.Pod{Name: listed.Metadata.Name, UID: listed.Metadata.UID})
				return err
			})
			if err != nil {
				last = err.Error()
				continue
			}
			if !pod.ready() || pod.Metadata.Annotations["lepinoid.dev/restart-transaction"] != t.j.TransactionID || pod.Metadata.Annotations["lepinoid.dev/restart-requested-at"] != t.j.RestartRequestedAt.Format(time.RFC3339Nano) {
				continue
			}
			if candidate != nil {
				return fmt.Errorf("%w: multiple ready restart replacements", updaterengine.ErrFenced)
			}
			candidate = &pod
		}
		if candidate != nil {
			observed, err := t.engine.Fence.Observe(t.ctx)
			if err != nil {
				return err
			}
			if observed.Blocked || observed.RecoveryPending || observed.Generation != *t.j.ExpectedTemplateGeneration {
				return fmt.Errorf("%w: restart generation changed or recovery remains pending", updaterengine.ErrFenced)
			}
			// The old UID is the fence baseline until this verified restart boundary.
			observed.PodUID = candidate.Metadata.UID
			closurePending = true
			if err := t.verifyClosureIntact(observed); err != nil {
				last = err.Error()
			} else {
				t.podName, t.podUID = candidate.Metadata.Name, candidate.Metadata.UID
				proposed := t.j
				proposed.ExpectedPodUID, proposed.FencingGeneration = observed.PodUID, observed.Generation
				// RefreshFence writes the flag before saving the journal; fence first.
				if err := t.engine.Fence.Check(t.ctx, proposed); err != nil {
					return err
				}
				t.j.ReplacementPodUID = ptr(observed.PodUID)
				if err := t.refreshFence(observed, true); err != nil {
					return err
				}
				// init-recover may have advanced the flag generation. Wait for the
				// gate to acknowledge its synchronized value before allowing B9.
				if err := t.verifyClosureIntact(observed); err == nil {
					return nil
				} else {
					last = err.Error()
				}
			}
		}
		if err := restartWait(t.ctx, time.Second); err != nil {
			return err
		}
	}
	fmt.Fprintf(os.Stderr, "updater: rollout watch timed out: want a Ready closed replacement for uid %q; last observed: %s\n", previous, last)
	if closurePending {
		return t.suspend("closure-verification-failed")
	}
	return t.suspend("checkpoint-timeout")
}

func (t *tx) b9Healthy(plan *manifestPlan) error {
	if t.j.PreviousPodUID == nil || t.j.RestartRequestedAt == nil {
		return fmt.Errorf("%w: missing persisted startup identity", updaterengine.ErrFenced)
	}
	deadline := t.now().Add(10 * time.Minute)
	var lastStatus, lastMonitor string
	for t.now().Before(deadline) {
		if err := t.engine.Fence.Check(t.ctx, t.j); err != nil {
			return err
		}
		var s status.Status
		readErr := t.api(func(ctx context.Context) error {
			pod, err := t.currentExecPod(ctx)
			if err != nil {
				return err
			}
			data, err := t.commands.Exec(ctx, pod, "updater", "cat", "/data/plugins/LepinoidTools/updater-status.json")
			if err != nil {
				data, err = t.commands.Exec(ctx, pod, "minecraft", "cat", "/data/plugins/LepinoidTools/updater-status.json")
				if err != nil {
					return err
				}
			}
			lastStatus = strings.TrimSpace(string(data))
			return json.Unmarshal(bytes.TrimSpace(data), &s)
		})
		if readErr != nil {
			lastStatus = "updater-status unreadable: " + readErr.Error()
		} else if monitor, err := t.mcMonitorJSON(); err != nil {
			lastMonitor = "mc-monitor unreadable: " + err.Error()
		} else {
			lastMonitor = monitor
			m := status.ParseMonitor([]byte(monitor))
			if s.Minecraft(m, t.j.ExpectedPodUID, t.now()) != "" && s.Healthy(status.Startup{PodUID: t.j.ExpectedPodUID, PreviousPodUID: *t.j.PreviousPodUID, RequestedAt: *t.j.RestartRequestedAt, Manifest: plan.Desired}, t.now()) {
				return nil
			}
		}
		if err := restartWait(t.ctx, 15*time.Second); err != nil {
			return err
		}
	}
	fmt.Fprintf(os.Stderr, "updater: startup health wait timed out: want updater-status Healthy for podUID=%q previousPodUID=%q manifest=%s with valid mc-monitor; last updater-status: %s; last mc-monitor: %s\n", t.j.ExpectedPodUID, *t.j.PreviousPodUID, plan.Desired.Digest, lastStatus, lastMonitor)
	return t.suspend("sidecar-unavailable")
}
