package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/lepinoid/infra/updater/internal/cluster"
	"github.com/lepinoid/infra/updater/internal/journal"
	"github.com/lepinoid/infra/updater/internal/status"
)

const checkpointTestID = "550e8400-e29b-41d4-a716-446655440000"

type checkpointRunner struct {
	issue           func(context.Context) ([]byte, error)
	read            func(context.Context, string) ([]byte, error)
	requests, reads int
	readContainers  []string
}

func (r *checkpointRunner) Run(context.Context, []byte, string, ...string) ([]byte, error) {
	return nil, errors.New("unexpected generic command")
}
func (r *checkpointRunner) Kubectl(context.Context, []byte, ...string) ([]byte, error) {
	return nil, errors.New("unexpected kubectl command")
}
func (r *checkpointRunner) Exec(ctx context.Context, _ cluster.Pod, container string, args ...string) ([]byte, error) {
	if slices.Equal(args, []string{"sh", "-c", "sed -n 's/^rcon.password=//p' /data/server.properties"}) {
		return []byte("checkpoint-secret\n"), nil
	}
	if slices.Equal(args, []string{"rcon-cli", "--password", "checkpoint-secret", "lepinoid", "updater-checkpoint"}) && container == "minecraft" {
		r.requests++
		if r.issue != nil {
			return r.issue(ctx)
		}
		return []byte("operationId=" + checkpointTestID), nil
	}
	if slices.Equal(args, []string{"cat", checkpointStatusPath}) {
		r.reads++
		r.readContainers = append(r.readContainers, container)
		return r.read(ctx, container)
	}
	return nil, fmt.Errorf("unexpected checkpoint command: %s %q", container, args)
}
func checkpointTransaction(t *testing.T) *tx {
	t.Helper()
	x := gateTransaction(t)
	x.now, x.engine.Now = time.Now, time.Now
	x.j.Phase, x.j.AccessState, x.j.MaintenanceRequired = "MAINTENANCE_ACTIVE", "CLOSED", true
	x.engine.Journal = x.j
	if err := x.saveJournal(); err != nil {
		t.Fatal(err)
	}
	return x
}
func checkpointSucceeded(pod string, at time.Time) status.Status {
	return status.Status{SchemaVersion: 1, ServerInstanceID: pod, Phase: "HEALTHY", UpdatedAt: at,
		AutoCommit: status.AutoCommit{OperationID: ptr(checkpointTestID), Trigger: ptr("UPDATER_CHECKPOINT"), State: "idle", Result: ptr("succeeded"), StartedAt: ptr(at.Add(-time.Second)), CompletedAt: ptr(at), PushedCommitSHA: ptr(strings.Repeat("a", 40))}}
}
func checkpointJSON(t *testing.T, s status.Status) []byte {
	t.Helper()
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
func assertCheckpointSuspended(t *testing.T, x *tx, err error, reason string) {
	t.Helper()
	if !errors.Is(err, errSuspended) {
		t.Fatalf("must stop caller after suspension, got %v", err)
	}
	persisted, readErr := journal.Read[journal.Journal](x.store.Path("journal", x.j.TransactionID))
	if readErr != nil {
		t.Fatal(readErr)
	}
	for name, j := range map[string]journal.Journal{"tx": x.j, "engine": x.engine.Journal, "disk": persisted} {
		if j.Lifecycle != "SUSPENDED" || j.SuspendReason == nil || *j.SuspendReason != reason || j.Phase != "MAINTENANCE_ACTIVE" || j.AccessState != "CLOSED" || !j.MaintenanceRequired {
			t.Fatalf("%s did not preserve closed checkpoint suspension: %+v", name, j)
		}
	}
}
func TestB4CheckpointWaitsForAcceptedOperation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		x := checkpointTransaction(t)
		started := time.Now()
		r := &checkpointRunner{}
		r.read = func(context.Context, string) ([]byte, error) {
			s := checkpointSucceeded(x.j.ExpectedPodUID, time.Now())
			if r.reads < 3 {
				s.AutoCommit.State, s.AutoCommit.Result = "running", ptr("pending")
				s.AutoCommit.CompletedAt, s.AutoCommit.PushedCommitSHA = nil, nil
			}
			return checkpointJSON(t, s), nil
		}
		x.commands = r
		if err := x.b4Checkpoint(); err != nil {
			t.Fatal(err)
		}
		if r.requests != 1 || r.reads != 3 || time.Since(started) != 2*checkpointPollInterval {
			t.Fatalf("acceptance must be followed by polling only: requests=%d reads=%d elapsed=%s", r.requests, r.reads, time.Since(started))
		}
		if x.j.Lifecycle != "ACTIVE" || x.j.Phase != "MAINTENANCE_ACTIVE" {
			t.Fatalf("checkpoint modified transaction phase: %+v", x.j)
		}
	})
}
func TestB4CheckpointAcceptsNoDiffSuccessAndOldEventTimestamp(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		x := checkpointTransaction(t)
		// A no-diff save returns an existing world HEAD, not the plugin build SHA.
		// Completion remains valid without a heartbeat or observed running state.
		s := checkpointSucceeded(x.j.ExpectedPodUID, time.Now().Add(-time.Minute))
		x.j.TargetManifest.PluginCommitSHA = strings.Repeat("b", 40)
		r := &checkpointRunner{read: func(context.Context, string) ([]byte, error) { return checkpointJSON(t, s), nil }}
		x.commands = r
		if err := x.b4Checkpoint(); err != nil {
			t.Fatal(err)
		}
		if r.requests != 1 || r.reads != 1 {
			t.Fatalf("immediate completed snapshot must be sufficient: %+v", r)
		}
	})
}
func TestB4CheckpointRejectsUnrelatedOrInvalidSuccess(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*status.Status)
	}{
		{"different operation", func(s *status.Status) { s.AutoCommit.OperationID = ptr("650e8400-e29b-41d4-a716-446655440000") }},
		{"previous Pod", func(s *status.Status) { s.ServerInstanceID = "old-pod" }},
		{"different trigger", func(s *status.Status) { s.AutoCommit.Trigger = ptr("PERIODIC") }},
		{"wrong schema", func(s *status.Status) { s.SchemaVersion = 2 }},
		{"unhealthy plugin", func(s *status.Status) { s.Phase = "UNHEALTHY" }},
		{"missing completion", func(s *status.Status) { s.AutoCommit.CompletedAt = nil }},
		{"zero completion", func(s *status.Status) { s.AutoCommit.CompletedAt = ptr(time.Time{}) }},
		{"completion before start", func(s *status.Status) { s.AutoCommit.CompletedAt = ptr(s.AutoCommit.StartedAt.Add(-time.Second)) }},
		{"future completion", func(s *status.Status) {
			s.AutoCommit.CompletedAt = ptr(time.Now().Add(24 * time.Hour))
			s.UpdatedAt = *s.AutoCommit.CompletedAt
		}},
		{"missing start", func(s *status.Status) { s.AutoCommit.StartedAt = nil }},
		{"completion after document", func(s *status.Status) { s.UpdatedAt = s.AutoCommit.CompletedAt.Add(-time.Second) }},
		{"invalid pushed SHA", func(s *status.Status) { s.AutoCommit.PushedCommitSHA = ptr("abcdef") }},
		{"null pushed SHA", func(s *status.Status) { s.AutoCommit.PushedCommitSHA = nil }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				x := checkpointTransaction(t)
				s := checkpointSucceeded(x.j.ExpectedPodUID, time.Now())
				tc.mutate(&s)
				r := &checkpointRunner{read: func(context.Context, string) ([]byte, error) { return checkpointJSON(t, s), nil }}
				x.commands = r
				started := time.Now()
				assertCheckpointSuspended(t, x, x.b4Checkpoint(), "checkpoint-timeout")
				if r.requests != 1 || time.Since(started) != checkpointWaitLimit {
					t.Fatalf("mismatch must only poll within deadline: requests=%d elapsed=%s", r.requests, time.Since(started))
				}
			})
		})
	}
}
func TestB4CheckpointFailedOperationSuspends(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		x := checkpointTransaction(t)
		s := checkpointSucceeded(x.j.ExpectedPodUID, time.Now())
		s.AutoCommit.Result, s.AutoCommit.PushedCommitSHA = ptr("failed"), nil
		r := &checkpointRunner{read: func(context.Context, string) ([]byte, error) { return checkpointJSON(t, s), nil }}
		x.commands = r
		assertCheckpointSuspended(t, x, x.b4Checkpoint(), "checkpoint-failed")
		if r.requests != 1 || r.reads != 1 {
			t.Fatalf("known failure must stop immediately: %+v", r)
		}
	})
}
func TestB4CheckpointRetriesBusyUnavailableAndTransientStatusRead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		x := checkpointTransaction(t)
		r := &checkpointRunner{}
		r.issue = func(context.Context) ([]byte, error) {
			switch r.requests {
			case 1:
				return []byte("error=busy"), nil
			case 2:
				return []byte("error=unavailable"), nil
			default:
				return []byte("operationId=" + checkpointTestID), nil
			}
		}
		r.read = func(context.Context, string) ([]byte, error) {
			if r.reads <= 2 {
				return nil, errors.New("temporary status read error")
			}
			return checkpointJSON(t, checkpointSucceeded(x.j.ExpectedPodUID, time.Now())), nil
		}
		x.commands = r
		if err := x.b4Checkpoint(); err != nil {
			t.Fatal(err)
		}
		if r.requests != 3 || r.reads != 3 || !slices.Equal(r.readContainers, []string{"updater", "minecraft", "updater"}) {
			t.Fatalf("unexpected retry/fallback behavior: %+v", r)
		}
	})
}
func TestB4CheckpointUnavailablePathsHaveBoundedDeadline(t *testing.T) {
	for _, mode := range []string{"busy", "unavailable", "request error", "status error", "invalid JSON", "running"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				x := checkpointTransaction(t)
				r := &checkpointRunner{}
				r.issue = func(context.Context) ([]byte, error) {
					switch mode {
					case "busy", "unavailable":
						return []byte("error=" + mode), nil
					case "request error":
						return nil, errors.New("temporary RCON transport error")
					default:
						return []byte("operationId=" + checkpointTestID), nil
					}
				}
				r.read = func(context.Context, string) ([]byte, error) {
					if mode == "status error" {
						return nil, errors.New("read failed")
					}
					if mode == "invalid JSON" {
						return []byte("{"), nil
					}
					s := checkpointSucceeded(x.j.ExpectedPodUID, time.Now())
					s.AutoCommit.State, s.AutoCommit.Result = "running", ptr("pending")
					return checkpointJSON(t, s), nil
				}
				x.commands = r
				started := time.Now()
				assertCheckpointSuspended(t, x, x.b4Checkpoint(), "checkpoint-timeout")
				if time.Since(started) != checkpointWaitLimit || time.Since(started) >= 29*time.Minute {
					t.Fatalf("wait exceeded bounded checkpoint budget: %s", time.Since(started))
				}
				if r.reads > 0 && r.requests != 1 {
					t.Fatalf("accepted operation was reissued: %+v", r)
				}
			})
		})
	}
}
func TestB4CheckpointCancellationStopsRequestReadAndPoll(t *testing.T) {
	for _, mode := range []string{"request", "read", "poll", "already canceled"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				x := checkpointTransaction(t)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				x.ctx = ctx
				r := &checkpointRunner{}
				r.issue = func(ctx context.Context) ([]byte, error) {
					if mode == "request" {
						<-ctx.Done()
						return nil, ctx.Err()
					}
					return []byte("operationId=" + checkpointTestID), nil
				}
				r.read = func(ctx context.Context, _ string) ([]byte, error) {
					if mode == "read" {
						<-ctx.Done()
						return nil, ctx.Err()
					}
					s := checkpointSucceeded(x.j.ExpectedPodUID, time.Now())
					s.AutoCommit.State, s.AutoCommit.Result = "running", ptr("pending")
					return checkpointJSON(t, s), nil
				}
				x.commands = r
				if mode == "already canceled" {
					cancel()
				} else {
					go func() { time.Sleep(2 * time.Second); cancel() }()
				}
				started := time.Now()
				if err := x.b4Checkpoint(); !errors.Is(err, context.Canceled) {
					t.Fatalf("cancellation ignored: %v", err)
				}
				if time.Since(started) > 2*time.Second || x.j.Lifecycle != "ACTIVE" {
					t.Fatalf("cancellation must stop immediately without journal mutation: elapsed=%s lifecycle=%s", time.Since(started), x.j.Lifecycle)
				}
				if mode == "read" && r.reads != 1 {
					t.Fatalf("canceled read attempted fallback: %d", r.reads)
				}
				if mode == "already canceled" && (r.requests != 0 || r.reads != 0) {
					t.Fatal("already canceled checkpoint issued commands")
				}
			})
		})
	}
}
func TestB4CheckpointInvalidAcceptanceStops(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		x := checkpointTransaction(t)
		r := &checkpointRunner{issue: func(context.Context) ([]byte, error) { return []byte("Unknown command"), nil }}
		x.commands = r
		assertCheckpointSuspended(t, x, x.b4Checkpoint(), "checkpoint-invalid-response")
		if r.requests != 1 || r.reads != 0 {
			t.Fatalf("invalid acknowledgement was accepted: %+v", r)
		}
	})
}

func TestB4CheckpointInFlightCommandsRespectTotalDeadline(t *testing.T) {
	for _, mode := range []string{"request", "read"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				x := checkpointTransaction(t)
				r := &checkpointRunner{}
				r.issue = func(ctx context.Context) ([]byte, error) {
					if mode == "request" {
						<-ctx.Done()
						return nil, ctx.Err()
					}
					return []byte("operationId=" + checkpointTestID), nil
				}
				r.read = func(ctx context.Context, _ string) ([]byte, error) { <-ctx.Done(); return nil, ctx.Err() }
				x.commands = r
				started := time.Now()
				assertCheckpointSuspended(t, x, x.b4Checkpoint(), "checkpoint-timeout")
				if time.Since(started) != checkpointWaitLimit {
					t.Fatalf("blocked %s escaped total deadline: %s", mode, time.Since(started))
				}
				if mode == "read" && r.requests != 1 {
					t.Fatalf("accepted operation was reissued: %d", r.requests)
				}
			})
		})
	}
}

func TestB4CheckpointStatusFallsBackToMinecraft(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		x := checkpointTransaction(t)
		r := &checkpointRunner{read: func(_ context.Context, container string) ([]byte, error) {
			if container == "updater" {
				return nil, errors.New("sidecar unavailable")
			}
			return checkpointJSON(t, checkpointSucceeded(x.j.ExpectedPodUID, time.Now())), nil
		}}
		x.commands = r
		if err := x.b4Checkpoint(); err != nil {
			t.Fatal(err)
		}
		if r.requests != 1 || !slices.Equal(r.readContainers, []string{"updater", "minecraft"}) {
			t.Fatalf("unexpected successful fallback: %+v", r)
		}
	})
}
