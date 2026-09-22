package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/lepinoid/infra/updater/internal/lease"
)

type runLeaseAPI struct {
	mu       sync.Mutex
	record   lease.Record
	onUpdate func(context.Context, lease.Record) error
}

func (a *runLeaseAPI) Get(ctx context.Context) (lease.Record, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.record, nil
}
func (a *runLeaseAPI) Update(ctx context.Context, r lease.Record) (lease.Record, error) {
	if a.onUpdate != nil {
		if err := a.onUpdate(ctx, r); err != nil {
			return lease.Record{}, err
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.record = r
	return r, nil
}

func TestRunReturnsLeaseLossCause(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		api := &runLeaseAPI{}
		lock := lease.New(api, "job-uid", time.Now)
		err := runWithLease(context.Background(), lock, func(ctx context.Context, _ *lease.Lease) error {
			synctest.Wait()
			api.mu.Lock()
			api.record.Holder = ""
			api.mu.Unlock()
			<-ctx.Done()
			// A subprocess may expose only cancellation. The runner must still
			// return the actual heartbeat diagnosis.
			return ctx.Err()
		})
		if !errors.Is(err, lease.ErrLost) || !strings.Contains(err.Error(), `expected_holder="job-uid"`) || !strings.Contains(err.Error(), `observed_holder=""`) {
			t.Fatalf("run lost cancellation cause: %v", err)
		}
	})
}

func TestRunStopsAndJoinsHeartbeatBeforeRelease(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		api := &runLeaseAPI{}
		started := make(chan struct{})
		finished := make(chan struct{})
		acquired := false
		released := false
		api.onUpdate = func(ctx context.Context, r lease.Record) error {
			if !acquired {
				acquired = true
				return nil
			}
			if r.Holder == "" {
				select {
				case <-finished:
				default:
					t.Error("release ran before renewal stopped")
				}
				if ctx.Err() != nil {
					t.Errorf("release inherited canceled transaction: %v", ctx.Err())
				}
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) != 5*time.Second {
					t.Errorf("release is not bounded: %v %v", deadline, ok)
				}
				released = true
				return nil
			}
			close(started)
			<-ctx.Done()
			close(finished)
			return ctx.Err()
		}
		lock := lease.New(api, "job-uid", time.Now)
		err := runWithLease(context.Background(), lock, func(context.Context, *lease.Lease) error {
			<-started
			return nil
		})
		if err != nil || !released {
			t.Fatalf("run=%v released=%v", err, released)
		}
	})
}

func TestRunParentCancellationKeepsCauseAndBoundsRelease(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		api := &runLeaseAPI{}
		released := false
		api.onUpdate = func(ctx context.Context, r lease.Record) error {
			if r.Holder == "" {
				released = true
				<-ctx.Done()
				return ctx.Err()
			}
			return nil
		}
		lock := lease.New(api, "job-uid", time.Now)
		ctx, cancel := context.WithCancelCause(context.Background())
		parentCause := errors.New("operator shutdown")
		start := time.Now()
		err := runWithLease(ctx, lock, func(ctx context.Context, _ *lease.Lease) error {
			cancel(parentCause)
			return ctx.Err()
		})
		if !errors.Is(err, parentCause) || errors.Is(err, lease.ErrLost) || !released || time.Since(start) != 5*time.Second {
			t.Fatalf("run=%v released=%v elapsed=%s", err, released, time.Since(start))
		}
	})
}

func TestRunRequiresIdentity(t *testing.T) {
	t.Setenv("JOB_UID", "")
	if err := run(context.Background()); !errors.Is(err, errUsage) {
		t.Fatal(err)
	}
}
