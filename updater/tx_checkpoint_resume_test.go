package main

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/lepinoid/infra/updater/internal/gate"
	"github.com/lepinoid/infra/updater/internal/journal"
	"github.com/lepinoid/infra/updater/internal/status"
	updaterengine "github.com/lepinoid/infra/updater/internal/updater"
)

func checkpointReload(t *testing.T, previous *tx) *tx {
	t.Helper()
	j, err := journal.Read[journal.Journal](previous.store.Path("journal", previous.j.TransactionID))
	if err != nil {
		t.Fatal(err)
	}
	engine := *previous.engine
	x := &tx{ctx: context.Background(), commands: previous.commands, store: previous.store, engine: &engine,
		now: previous.now, podName: previous.podName, podUID: previous.podUID, j: j, plan: previous.plan}
	engine.Journal = j
	if err := x.reactivateSuspended(); err != nil {
		t.Fatal(err)
	}
	return x
}

func checkpointRecord(t *testing.T, x *tx) journal.Checkpoint {
	t.Helper()
	c, err := x.loadCheckpoint()
	if err != nil || c == nil {
		t.Fatalf("missing checkpoint record: %+v %v", c, err)
	}
	return *c
}

func checkpointRunning(pod string, started time.Time) status.Status {
	s := checkpointSucceeded(pod, started)
	s.AutoCommit.StartedAt = ptr(started)
	s.AutoCommit.State, s.AutoCommit.Result = "running", ptr("pending")
	s.AutoCommit.CompletedAt, s.AutoCommit.PushedCommitSHA = nil, nil
	return s
}

func TestCheckpointTimeoutResumesAcrossJobsWithoutAnotherRequest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		x := checkpointTransaction(t)
		started := time.Now()
		running := true
		r := &checkpointRunner{}
		r.read = func(context.Context, string) ([]byte, error) {
			record := checkpointRecord(t, x)
			if record.State != "accepted" || record.OperationID == nil || *record.OperationID != checkpointTestID {
				t.Fatalf("acceptance was not durable before polling: %+v", record)
			}
			if running {
				return checkpointJSON(t, checkpointRunning(x.j.ExpectedPodUID, started)), nil
			}
			// The save finishes between Jobs, and its event snapshot has aged.
			s := checkpointSucceeded(x.j.ExpectedPodUID, started.Add(11*time.Minute))
			s.AutoCommit.StartedAt = ptr(started)
			return checkpointJSON(t, s), nil
		}
		x.commands = r
		for job := 0; job < 2; job++ {
			assertCheckpointSuspended(t, x, x.b4Checkpoint(), "checkpoint-timeout")
			if r.requests != 1 {
				t.Fatalf("job %d reissued an accepted operation: %d", job, r.requests)
			}
			x = checkpointReload(t, x)
		}
		time.Sleep(3 * time.Minute)
		running = false
		if err := x.b4Checkpoint(); err != nil {
			t.Fatal(err)
		}
		if r.requests != 1 || checkpointRecord(t, x).State != "succeeded" {
			t.Fatalf("resume did not finish the original save: requests=%d record=%+v", r.requests, checkpointRecord(t, x))
		}
	})
}

func TestCheckpointCancellationAndReadFailureRetainAcceptedOperation(t *testing.T) {
	for _, mode := range []string{"cancel", "read failure"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				x := checkpointTransaction(t)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				x.ctx = ctx
				r := &checkpointRunner{read: func(context.Context, string) ([]byte, error) {
					if mode == "cancel" {
						cancel()
					}
					return nil, errors.New("status transport failed")
				}}
				x.commands = r
				err := x.b4Checkpoint()
				if mode == "cancel" {
					if !errors.Is(err, context.Canceled) {
						t.Fatalf("cancellation lost: %v", err)
					}
				} else {
					assertCheckpointSuspended(t, x, err, "checkpoint-timeout")
				}
				x = checkpointReload(t, x)
				r.read = func(context.Context, string) ([]byte, error) {
					return checkpointJSON(t, checkpointSucceeded(x.j.ExpectedPodUID, time.Now())), nil
				}
				if err := x.b4Checkpoint(); err != nil || r.requests != 1 {
					t.Fatalf("resume reissued save: requests=%d err=%v", r.requests, err)
				}
			})
		})
	}
}

func TestCheckpointSuccessSurvivesCrashBeforePhaseAdvance(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		x := checkpointTransaction(t)
		r := &checkpointRunner{read: func(context.Context, string) ([]byte, error) {
			return checkpointJSON(t, checkpointSucceeded(x.j.ExpectedPodUID, time.Now())), nil
		}}
		x.commands = r
		if err := x.b4Checkpoint(); err != nil {
			t.Fatal(err)
		}
		before := checkpointRecord(t, x)
		x = checkpointReload(t, x)
		if x.j.Phase != "MAINTENANCE_ACTIVE" {
			t.Fatal("test did not stop before the phase transition")
		}
		r.read = func(context.Context, string) ([]byte, error) {
			t.Fatal("durable success must survive status being overwritten by PERIODIC")
			return nil, nil
		}
		if err := x.b4Checkpoint(); err != nil || r.requests != 1 || r.reads != 1 {
			t.Fatalf("recorded success was not reused: requests=%d reads=%d err=%v", r.requests, r.reads, err)
		}
		if after := checkpointRecord(t, x); !reflect.DeepEqual(before, after) {
			t.Fatalf("success evidence changed: before=%+v after=%+v", before, after)
		}
		x.engine.Fence.Lease = func(context.Context) error { return updaterengine.ErrFenced }
		if err := x.b4Checkpoint(); !errors.Is(err, updaterengine.ErrFenced) {
			t.Fatalf("recorded success bypassed fencing: %v", err)
		}
	})
}

func TestCheckpointLostResponseRecoversFromStatus(t *testing.T) {
	for _, state := range []string{"running", "completed"} {
		t.Run(state, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				x := checkpointTransaction(t)
				started := time.Now()
				r := &checkpointRunner{issue: func(context.Context) ([]byte, error) {
					if c := checkpointRecord(t, x); c.State != "requesting" || c.OperationID != nil {
						t.Fatalf("intent missing before RCON: %+v", c)
					}
					return nil, errors.New("accepted command response was lost")
				}}
				r.read = func(context.Context, string) ([]byte, error) {
					if state == "running" && r.reads == 1 {
						return checkpointJSON(t, checkpointRunning(x.j.ExpectedPodUID, started)), nil
					}
					s := checkpointSucceeded(x.j.ExpectedPodUID, time.Now())
					s.AutoCommit.StartedAt = ptr(started)
					return checkpointJSON(t, s), nil
				}
				x.commands = r
				if err := x.b4Checkpoint(); err != nil || r.requests != 1 {
					t.Fatalf("lost response caused another save: requests=%d err=%v", r.requests, err)
				}
			})
		})
	}
}

func TestCheckpointIntentCrashDoesNotReissueUnknownRequest(t *testing.T) {
	for _, observation := range []string{"old checkpoint", "periodic", "wrong pod", "wrong schema", "invalid id", "unavailable", "valid running"} {
		t.Run(observation, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				x := checkpointTransaction(t)
				intent := journal.Checkpoint{SchemaVersion: 1, TransactionID: x.j.TransactionID, PodUID: x.j.ExpectedPodUID, RequestedAt: time.Now(), State: "requesting"}
				if err := x.saveCheckpoint(intent); err != nil {
					t.Fatal(err)
				}
				x = checkpointReload(t, x)
				r := &checkpointRunner{read: func(context.Context, string) ([]byte, error) {
					s := checkpointRunning(x.j.ExpectedPodUID, intent.RequestedAt)
					switch observation {
					case "old checkpoint":
						s.AutoCommit.StartedAt = ptr(intent.RequestedAt.Add(-time.Second))
					case "periodic":
						s.AutoCommit.Trigger = ptr("PERIODIC")
					case "wrong pod":
						s.ServerInstanceID = "other-pod"
					case "wrong schema":
						s.SchemaVersion = 2
					case "invalid id":
						s.AutoCommit.OperationID = ptr("invalid")
					case "unavailable":
						return nil, errors.New("no status")
					}
					return checkpointJSON(t, s), nil
				}}
				x.commands = r
				for job := 0; job < 2; job++ {
					assertCheckpointSuspended(t, x, x.b4Checkpoint(), "checkpoint-timeout")
					x = checkpointReload(t, x)
				}
				if r.requests != 0 {
					t.Fatalf("unknown request reissued %d times", r.requests)
				}
				c := checkpointRecord(t, x)
				if (c.OperationID != nil) != (observation == "valid running") {
					t.Fatalf("incorrect adoption: %+v", c)
				}
			})
		})
	}
}

func TestCheckpointAcceptanceWriteFailureLeavesRecoverableIntent(t *testing.T) {
	for _, fault := range []string{"lease loss", "disk write failure"} {
		t.Run(fault, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				x := checkpointTransaction(t)
				started := time.Now()
				lost := errors.New("lease lost after acceptance")
				r := &checkpointRunner{issue: func(context.Context) ([]byte, error) {
					if fault == "lease loss" {
						x.engine.Fence.Lease = func(context.Context) error { return lost }
					} else {
						if err := os.Chmod(x.store.Path("checkpoint"), 0555); err != nil {
							t.Fatal(err)
						}
						t.Cleanup(func() { _ = os.Chmod(x.store.Path("checkpoint"), 0755) })
					}
					return []byte("operationId=" + checkpointTestID), nil
				}}
				x.commands = r
				err := x.b4Checkpoint()
				if err == nil || r.reads != 0 || (fault == "lease loss" && !errors.Is(err, lost)) {
					t.Fatalf("acceptance without durable ID progressed: err=%v reads=%d", err, r.reads)
				}
				if c := checkpointRecord(t, x); c.State != "requesting" || c.OperationID != nil {
					t.Fatalf("uncommitted acceptance changed durable intent: %+v", c)
				}
				x.engine.Fence.Lease = func(context.Context) error { return nil }
				if err := os.Chmod(x.store.Path("checkpoint"), 0755); err != nil {
					t.Fatal(err)
				}
				x = checkpointReload(t, x)
				r.read = func(context.Context, string) ([]byte, error) {
					s := checkpointSucceeded(x.j.ExpectedPodUID, time.Now())
					s.AutoCommit.StartedAt = ptr(started)
					return checkpointJSON(t, s), nil
				}
				if err := x.b4Checkpoint(); err != nil || r.requests != 1 {
					t.Fatalf("recovery lost the accepted request: err=%v requests=%d", err, r.requests)
				}
			})
		})
	}
}

func TestCheckpointRecordFailureStopsBeforeRequest(t *testing.T) {
	for _, fault := range []string{"corrupt", "other transaction", "invalid operation", "write failure"} {
		t.Run(fault, func(t *testing.T) {
			x := checkpointTransaction(t)
			path := x.store.Path("checkpoint", x.j.TransactionID)
			c := journal.Checkpoint{SchemaVersion: 1, TransactionID: x.j.TransactionID, PodUID: x.j.ExpectedPodUID, RequestedAt: x.now().UTC(), State: "accepted", OperationID: ptr(checkpointTestID)}
			switch fault {
			case "corrupt":
				completionWrite(t, path, []byte("{"))
			case "other transaction":
				c.TransactionID = "650e8400-e29b-41d4-a716-446655440000"
			case "invalid operation":
				c.OperationID = ptr("not-a-uuid")
			case "write failure":
				if err := os.Chmod(x.store.Path("checkpoint"), 0555); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(x.store.Path("checkpoint"), 0755) })
			}
			if fault == "other transaction" || fault == "invalid operation" {
				if err := journal.Save(path, c); err != nil {
					t.Fatal(err)
				}
			}
			r := &checkpointRunner{}
			x.commands = r
			if err := x.b4Checkpoint(); err == nil || r.requests != 0 || r.reads != 0 {
				t.Fatalf("unsafe record permitted a request: err=%v requests=%d reads=%d", err, r.requests, r.reads)
			}
			if x.j.Phase != "MAINTENANCE_ACTIVE" {
				t.Fatal("record failure advanced transaction")
			}
		})
	}
}

func TestCheckpointKnownOperationMismatchNeverStartsAnotherSave(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		x := checkpointTransaction(t)
		c := journal.Checkpoint{SchemaVersion: 1, TransactionID: x.j.TransactionID, PodUID: x.j.ExpectedPodUID, RequestedAt: time.Now(), State: "accepted", OperationID: ptr(checkpointTestID)}
		if err := x.saveCheckpoint(c); err != nil {
			t.Fatal(err)
		}
		r := &checkpointRunner{read: func(context.Context, string) ([]byte, error) {
			s := checkpointSucceeded(x.j.ExpectedPodUID, time.Now())
			s.AutoCommit.OperationID = ptr("650e8400-e29b-41d4-a716-446655440000")
			return checkpointJSON(t, s), nil
		}}
		x.commands = r
		for job := 0; job < 2; job++ {
			assertCheckpointSuspended(t, x, x.b4Checkpoint(), "checkpoint-timeout")
			x = checkpointReload(t, x)
		}
		if r.requests != 0 || *checkpointRecord(t, x).OperationID != checkpointTestID {
			t.Fatal("different operation replaced the accepted checkpoint")
		}
	})
}

func TestCheckpointFailedOperationRetriesOnlyInNextJob(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		x := checkpointTransaction(t)
		r := &checkpointRunner{read: func(context.Context, string) ([]byte, error) {
			s := checkpointSucceeded(x.j.ExpectedPodUID, time.Now())
			s.AutoCommit.Result, s.AutoCommit.PushedCommitSHA = ptr("failed"), nil
			return checkpointJSON(t, s), nil
		}}
		x.commands = r
		assertCheckpointSuspended(t, x, x.b4Checkpoint(), "checkpoint-failed")
		if r.requests != 1 || checkpointRecord(t, x).State != "failed" {
			t.Fatal("failed request retried in the same Job or was not recorded")
		}
		x = checkpointReload(t, x)
		newID := "650e8400-e29b-41d4-a716-446655440000"
		r.issue = func(context.Context) ([]byte, error) { return []byte("operationId=" + newID), nil }
		r.read = func(context.Context, string) ([]byte, error) {
			s := checkpointSucceeded(x.j.ExpectedPodUID, time.Now())
			s.AutoCommit.OperationID = ptr(newID)
			return checkpointJSON(t, s), nil
		}
		if err := x.b4Checkpoint(); err != nil || r.requests != 2 || *checkpointRecord(t, x).OperationID != newID {
			t.Fatalf("definitive failure could not retry: err=%v requests=%d", err, r.requests)
		}
	})
}

func TestCheckpointPodReplacementRequiresClosureAndFreshSave(t *testing.T) {
	for _, intact := range []bool{false, true} {
		t.Run(map[bool]string{false: "closure broken", true: "closure intact"}[intact], func(t *testing.T) {
			x := checkpointTransaction(t)
			c := journal.Checkpoint{SchemaVersion: 1, TransactionID: x.j.TransactionID, PodUID: "old-pod", RequestedAt: x.now().UTC(), State: "succeeded", OperationID: ptr(checkpointTestID), StartedAt: ptr(x.now().UTC()), CompletedAt: ptr(x.now().UTC()), PushedCommitSHA: ptr(strings.Repeat("a", 40))}
			if err := journal.Save(x.store.Path("checkpoint", x.j.TransactionID), c); err != nil {
				t.Fatal(err)
			}
			original := verifyInstalledPair
			t.Cleanup(func() { verifyInstalledPair = original })
			verifyInstalledPair = func(journal.Pair) error { return nil }
			if intact {
				if err := x.engine.Flag(x.ctx); err != nil {
					t.Fatal(err)
				}
				ack := gate.Status{SchemaVersion: 1, Version: "1", ServerInstanceID: x.j.ExpectedPodUID, Phase: "ACTIVE", ObservedTransaction: ptr(x.j.TransactionID), ObservedGeneration: ptr(x.j.FencingGeneration), UpdatedAt: x.now().UTC()}
				if err := journal.Save(x.store.Path("gate-status.json"), ack); err != nil {
					t.Fatal(err)
				}
			}
			r := &checkpointRunner{read: func(context.Context, string) ([]byte, error) {
				return checkpointJSON(t, checkpointSucceeded(x.j.ExpectedPodUID, x.now())), nil
			}}
			x.commands = r
			err := x.b4Checkpoint()
			if intact {
				if err != nil || r.requests != 1 || checkpointRecord(t, x).PodUID != x.j.ExpectedPodUID {
					t.Fatalf("replacement Pod did not receive a fresh save: err=%v requests=%d", err, r.requests)
				}
			} else if err == nil || r.requests != 0 || checkpointRecord(t, x).PodUID != "old-pod" {
				t.Fatalf("unverified closure discarded previous evidence: err=%v requests=%d", err, r.requests)
			}
		})
	}
}

func TestCheckpointArchiveResumesAfterJournalMoveFailure(t *testing.T) {
	x := completionTransaction(t)
	terminalPremaintenance(t, x, "PREPARING")
	x.j.Lifecycle, x.j.Outcome = "TERMINAL", ptr("ABORTED")
	if err := x.saveJournal(); err != nil {
		t.Fatal(err)
	}
	c := journal.Checkpoint{SchemaVersion: 1, TransactionID: x.j.TransactionID, PodUID: x.j.ExpectedPodUID, RequestedAt: x.now().UTC(), State: "requesting"}
	if err := x.saveCheckpoint(c); err != nil {
		t.Fatal(err)
	}
	obstruction := x.store.Path("archive", x.j.TransactionID)
	if err := os.Mkdir(obstruction, 0755); err != nil {
		t.Fatal(err)
	}
	if err := x.finalizeTerminal(); err == nil {
		t.Fatal("journal move failure was not reached")
	}
	archived, err := journal.Read[journal.Checkpoint](x.store.Path("archive", x.j.TransactionID+".checkpoint"))
	if err != nil || !reflect.DeepEqual(c, archived) {
		t.Fatalf("checkpoint evidence not archived first: %+v %v", archived, err)
	}
	if err := os.Remove(obstruction); err != nil {
		t.Fatal(err)
	}
	x = checkpointReload(t, x)
	if err := x.finalizeTerminal(); err != nil {
		t.Fatalf("archive retry failed after checkpoint was already moved: %v", err)
	}
	if _, err := os.Stat(x.store.Path("checkpoint", x.j.TransactionID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("active checkpoint survived archive: %v", err)
	}
}

func TestCheckpointGenerationRefreshKeepsAcceptedOperation(t *testing.T) {
	x := checkpointTransaction(t)
	c := journal.Checkpoint{SchemaVersion: 1, TransactionID: x.j.TransactionID, PodUID: x.j.ExpectedPodUID, RequestedAt: x.now(), State: "accepted", OperationID: ptr(checkpointTestID)}
	if err := x.saveCheckpoint(c); err != nil {
		t.Fatal(err)
	}
	x.j.FencingGeneration++
	x.engine.Fence.Observe = func(context.Context) (updaterengine.Observation, error) {
		return updaterengine.Observation{PodUID: x.j.ExpectedPodUID, Generation: x.j.FencingGeneration}, nil
	}
	if err := x.saveJournal(); err != nil {
		t.Fatal(err)
	}
	x = checkpointReload(t, x)
	r := &checkpointRunner{read: func(context.Context, string) ([]byte, error) {
		return checkpointJSON(t, checkpointSucceeded(x.j.ExpectedPodUID, x.now())), nil
	}}
	x.commands = r
	if err := x.b4Checkpoint(); err != nil || r.requests != 0 {
		t.Fatalf("generation refresh discarded accepted operation: err=%v requests=%d", err, r.requests)
	}
}
