package lease

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

type memory struct {
	mu                sync.Mutex
	value             Record
	getErr, updateErr error
	updates           int
}

func (m *memory) Get(context.Context) (Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.value, m.getErr
}
func (m *memory) Update(_ context.Context, r Record) (Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.updateErr != nil {
		return Record{}, m.updateErr
	}
	m.value = r
	m.updates++
	return r, nil
}
func (m *memory) change(f func(*memory)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f(m)
}

type apiFuncs struct {
	get    func(context.Context) (Record, error)
	update func(context.Context, Record) (Record, error)
}

func (a apiFuncs) Get(ctx context.Context) (Record, error)              { return a.get(ctx) }
func (a apiFuncs) Update(ctx context.Context, r Record) (Record, error) { return a.update(ctx, r) }
func mustAcquire(t *testing.T, l *Lease) {
	t.Helper()
	if ok, err := l.Acquire(context.Background()); !ok || err != nil {
		t.Fatalf("Acquire: acquired=%v error=%v", ok, err)
	}
}
func startHeartbeat(l *Lease, ctx context.Context, cancel context.CancelCauseFunc) <-chan struct{} {
	done := make(chan struct{})
	go func() { defer close(done); l.Heartbeat(ctx, cancel) }()
	synctest.Wait()
	return done
}
func TestLiveHolderIsNotTaken(t *testing.T) {
	now := time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)
	api := &memory{value: Record{Holder: "other", RenewedAt: now, Duration: 60}}
	l := New(api, "self", func() time.Time { return now })
	got, err := l.Acquire(context.Background())
	if err != nil || got {
		t.Fatalf("%v %v", got, err)
	}
}
func TestExpiredHolderAndFencing(t *testing.T) {
	now := time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)
	api := &memory{value: Record{Holder: "old", RenewedAt: now.Add(-61 * time.Second), Duration: 60}}
	l := New(api, "self", func() time.Time { return now })
	mustAcquire(t, l)
	api.change(func(m *memory) { m.value.Holder = "new" })
	err := l.Check(context.Background())
	if !errors.Is(err, ErrLost) || !strings.Contains(err.Error(), `observed_holder="new"`) {
		t.Fatalf("lost lease diagnosis: %v", err)
	}
	api.change(func(m *memory) { m.value.Holder = "self" })
	if got := l.Check(context.Background()); got != err {
		t.Fatalf("first loss must remain sticky: got=%v want=%v", got, err)
	}
}
func TestCheckDiagnosesRecordExpiryAndLocalDeadline(t *testing.T) {
	for _, reason := range []string{"record expired", "renewal deadline exceeded"} {
		t.Run(reason, func(t *testing.T) {
			now := time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)
			api := &memory{}
			l := New(api, "self", func() time.Time { return now })
			mustAcquire(t, l)
			if reason == "record expired" {
				api.change(func(m *memory) { m.value.RenewedAt = now.Add(-Duration) })
			} else {
				now = now.Add(RenewDeadline)
			}
			err := l.Check(context.Background())
			if !errors.Is(err, ErrLost) || !strings.Contains(err.Error(), reason) || !strings.Contains(err.Error(), "self") {
				t.Fatalf("missing diagnosis: %v", err)
			}
		})
	}
}
func TestHeartbeatKeepsRenewingBeyondFormerFailureWindow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		api := &memory{}
		l := New(api, "self", time.Now)
		mustAcquire(t, l)
		ctx, cancel := context.WithCancelCause(context.Background())
		done := startHeartbeat(l, ctx, cancel)
		time.Sleep(12 * time.Minute)
		synctest.Wait()
		if err := ctx.Err(); err != nil {
			t.Fatalf("healthy lease canceled: %v", context.Cause(ctx))
		}
		api.change(func(m *memory) {
			if m.updates != 145 || time.Since(m.value.RenewedAt) != 0 {
				t.Errorf("renewals=%d last renewal age=%s", m.updates, time.Since(m.value.RenewedAt))
			}
		})
		cancel(nil)
		<-done
	})
}
func TestHeartbeatHolderDisappearanceKeepsCause(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		api := &memory{value: Record{ResourceVersion: "42"}}
		l := New(api, "self", time.Now)
		mustAcquire(t, l)
		ctx, cancel := context.WithCancelCause(context.Background())
		done := startHeartbeat(l, ctx, cancel)
		api.change(func(m *memory) { m.value.Holder = "" })
		time.Sleep(heartbeatInterval)
		<-done
		err := context.Cause(ctx)
		if !errors.Is(err, ErrLost) {
			t.Fatalf("cause=%v", err)
		}
		for _, detail := range []string{"holder mismatch", `expected_holder="self"`, `observed_holder=""`, `resource_version="42"`, "last_success_elapsed=5s"} {
			if !strings.Contains(err.Error(), detail) {
				t.Errorf("missing %q in %v", detail, err)
			}
		}
		if got := l.Check(ctx); got != err {
			t.Fatalf("canceled fence check lost cause: %v", got)
		}
	})
}
func TestHeartbeatTransientFailuresRecover(t *testing.T) {
	for _, failure := range []string{"get", "update"} {
		t.Run(failure, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				api := &memory{}
				l := New(api, "self", time.Now)
				var logs []string
				l.logf = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
				mustAcquire(t, l)
				ctx, cancel := context.WithCancelCause(context.Background())
				done := startHeartbeat(l, ctx, cancel)
				api.change(func(m *memory) {
					if failure == "get" {
						m.getErr = errors.New("temporary API outage")
					} else {
						m.updateErr = errors.New("temporary API outage")
					}
				})
				time.Sleep(20 * time.Second)
				synctest.Wait()
				if len(logs) != 4 || !strings.Contains(logs[3], "last_success_elapsed=20s") || !strings.Contains(logs[3], "temporary API outage") {
					t.Fatalf("missing renewal failure evidence: %v", logs)
				}
				api.change(func(m *memory) { m.getErr, m.updateErr = nil, nil })
				time.Sleep(time.Minute)
				synctest.Wait()
				if err := l.Check(ctx); err != nil {
					t.Fatalf("transient error did not recover: %v", err)
				}
				cancel(nil)
				<-done
			})
		})
	}
}
func TestHeartbeatStopsAtDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		api := &memory{}
		l := New(api, "self", time.Now)
		l.logf = func(string, ...any) {}
		mustAcquire(t, l)
		ctx, cancel := context.WithCancelCause(context.Background())
		done := startHeartbeat(l, ctx, cancel)
		api.change(func(m *memory) { m.getErr = errors.New("API unavailable") })
		time.Sleep(25 * time.Second)
		synctest.Wait()
		if ctx.Err() != nil {
			t.Fatalf("lease canceled before deadline: %v", context.Cause(ctx))
		}
		time.Sleep(5 * time.Second)
		<-done
		err := context.Cause(ctx)
		if !errors.Is(err, ErrLost) || !strings.Contains(err.Error(), "elapsed=30s") {
			t.Fatalf("deadline diagnosis: %v", err)
		}
		api.change(func(m *memory) { m.getErr = nil })
		if err := l.renew(context.Background()); !errors.Is(err, ErrLost) {
			t.Fatalf("deadline loss recovered without acquisition: %v", err)
		}
	})
}
func TestHeartbeatRequestTimeoutExhaustsDeadlineWithCause(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		api := &memory{}
		blocked := false
		l := New(apiFuncs{
			get: func(ctx context.Context) (Record, error) {
				if blocked {
					<-ctx.Done()
					return Record{}, ctx.Err()
				}
				return api.Get(ctx)
			}, update: api.Update,
		}, "self", time.Now)
		l.logf = func(string, ...any) {}
		mustAcquire(t, l)
		ctx, cancel := context.WithCancelCause(context.Background())
		blocked = true
		done := startHeartbeat(l, ctx, cancel)
		time.Sleep(RenewDeadline)
		<-done
		if err := context.Cause(ctx); !errors.Is(err, ErrLost) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("deadline lost request cause: %v", err)
		}
	})
}
func TestHeartbeatParentCancellationIsNotLeaseLoss(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		api := &memory{}
		started := make(chan struct{})
		blockRenewal := false
		l := New(apiFuncs{
			get: api.Get,
			update: func(ctx context.Context, r Record) (Record, error) {
				if blockRenewal {
					close(started)
					<-ctx.Done()
					return Record{}, ctx.Err()
				}
				return api.Update(ctx, r)
			},
		}, "self", time.Now)
		mustAcquire(t, l)
		ctx, cancel := context.WithCancelCause(context.Background())
		var reported error
		blockRenewal = true
		done := startHeartbeat(l, ctx, func(err error) { reported = err; cancel(err) })
		time.Sleep(heartbeatInterval)
		<-started
		parentCause := errors.New("operator shutdown")
		cancel(parentCause)
		<-done
		if reported != nil || context.Cause(ctx) != parentCause {
			t.Fatalf("shutdown misreported as loss: reported=%v cause=%v", reported, context.Cause(ctx))
		}
		if err := l.Check(context.Background()); err != nil {
			t.Fatalf("shutdown poisoned ownership state: %v", err)
		}
	})
}
func TestLateSuccessfulRenewalCannotExtendDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		api := &memory{}
		delay := time.Duration(0)
		l := New(apiFuncs{
			get:    api.Get,
			update: func(ctx context.Context, r Record) (Record, error) { time.Sleep(delay); return api.Update(ctx, r) },
		}, "self", time.Now)
		mustAcquire(t, l)
		time.Sleep(25 * time.Second)
		delay = 5 * time.Second
		if err := l.renew(context.Background()); !errors.Is(err, ErrLost) || !strings.Contains(err.Error(), "elapsed=30s") {
			t.Fatalf("late success restored lease authority: %v", err)
		}
	})
}
