package lease

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"
)

var ErrLost = errors.New("lease lost or renewal deadline exceeded")

const Duration = 60 * time.Second
const RenewDeadline = 30 * time.Second

const heartbeatInterval = 5 * time.Second

type Record struct {
	ResourceVersion string
	Holder          string
	RenewedAt       time.Time
	Duration        int
}
type API interface {
	Get(context.Context) (Record, error)
	Update(context.Context, Record) (Record, error)
}
type Lease struct {
	api         API
	holder      string
	now         func() time.Time
	logf        func(string, ...any)
	mu          sync.Mutex
	lastSuccess time.Time
	lostErr     error
}

func New(api API, holder string, now func() time.Time) *Lease {
	return &Lease{api: api, holder: holder, now: now, logf: log.Printf}
}

func (l *Lease) Acquire(ctx context.Context) (bool, error) {
	if l.holder == "" {
		return false, fmt.Errorf("%w: empty holder identity", ErrLost)
	}
	record, err := l.api.Get(ctx)
	if err != nil {
		return false, err
	}
	now := l.now()
	if record.Holder != "" && record.Holder != l.holder && now.Before(record.RenewedAt.Add(time.Duration(record.Duration)*time.Second)) {
		return false, nil
	}
	record.Holder = l.holder
	record.Duration = int(Duration / time.Second)
	record.RenewedAt = now
	if _, err := l.api.Update(ctx, record); err != nil {
		return false, err
	}
	l.mu.Lock()
	l.lastSuccess = now
	l.lostErr = nil
	l.mu.Unlock()
	return true, nil
}

// checkLocalLocked preserves the first loss cause even when a later fence check
// runs after the heartbeat has canceled the transaction.
func (l *Lease) checkLocalLocked() error {
	if l.lostErr == nil {
		now := l.now()
		if elapsed := now.Sub(l.lastSuccess); elapsed >= RenewDeadline {
			l.lostErr = fmt.Errorf("%w: renewal deadline exceeded: holder=%q last_success=%s elapsed=%s deadline=%s", ErrLost, l.holder, l.lastSuccess.UTC().Format(time.RFC3339Nano), elapsed, RenewDeadline)
		}
	}
	return l.lostErr
}

func (l *Lease) checkLocal() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.checkLocalLocked()
}

func (l *Lease) observe(ctx context.Context) (Record, error) {
	if err := context.Cause(ctx); err != nil {
		return Record{}, err
	}
	if err := l.checkLocal(); err != nil {
		return Record{}, err
	}
	r, err := l.api.Get(ctx)
	if err != nil {
		return Record{}, fmt.Errorf("read lease: %w", err)
	}
	if err := context.Cause(ctx); err != nil {
		return Record{}, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.checkLocalLocked(); err != nil {
		return Record{}, err
	}
	now := l.now()
	reason := ""
	if r.Holder != l.holder {
		reason = "holder mismatch"
	} else if !now.Before(r.RenewedAt.Add(time.Duration(r.Duration) * time.Second)) {
		reason = "record expired"
	}
	if reason != "" {
		l.lostErr = fmt.Errorf("%w: %s: expected_holder=%q observed_holder=%q resource_version=%q renew_time=%s duration_seconds=%d observed_at=%s last_success_elapsed=%s", ErrLost, reason, l.holder, r.Holder, r.ResourceVersion, r.RenewedAt.UTC().Format(time.RFC3339Nano), r.Duration, now.UTC().Format(time.RFC3339Nano), now.Sub(l.lastSuccess))
		return Record{}, l.lostErr
	}
	return r, nil
}

func (l *Lease) Check(ctx context.Context) error {
	_, err := l.observe(ctx)
	return err
}

func (l *Lease) renew(ctx context.Context) error {
	r, err := l.observe(ctx)
	if err != nil {
		return err
	}
	r.RenewedAt = l.now()
	r.Duration = int(Duration / time.Second)
	if _, err := l.api.Update(ctx, r); err != nil {
		return fmt.Errorf("update lease: %w", err)
	}
	if err := context.Cause(ctx); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	// An update that finishes after the local deadline cannot restore authority.
	if err := l.checkLocalLocked(); err != nil {
		return err
	}
	l.lastSuccess = l.now()
	return nil
}

func (l *Lease) Heartbeat(ctx context.Context, cancel context.CancelCauseFunc) {
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			bounded, stop := context.WithTimeout(ctx, heartbeatInterval)
			err := l.renew(bounded)
			stop()
			// Shutdown is not evidence of lost ownership, including cancellation
			// while an API request is in flight.
			if ctx.Err() != nil {
				return
			}
			l.mu.Lock()
			deadlineErr := l.checkLocalLocked()
			elapsed := l.now().Sub(l.lastSuccess)
			l.mu.Unlock()
			if err != nil {
				l.logf("lease heartbeat renewal failed: holder=%q last_success_elapsed=%s error=%v", l.holder, elapsed, err)
			}
			if deadlineErr != nil && !errors.Is(err, ErrLost) {
				// Retain the API failure which exhausted the renewal budget.
				err = errors.Join(deadlineErr, err)
			}
			if errors.Is(err, ErrLost) {
				l.logf("lease heartbeat lost authority: %v", err)
				cancel(err)
				return
			}
		}
	}
}

func (l *Lease) Release(ctx context.Context) error {
	r, err := l.api.Get(ctx)
	if err != nil {
		return err
	}
	if r.Holder != l.holder {
		return fmt.Errorf("%w: release holder mismatch: expected_holder=%q observed_holder=%q resource_version=%q", ErrLost, l.holder, r.Holder, r.ResourceVersion)
	}
	r.Holder = ""
	_, err = l.api.Update(ctx, r)
	return err
}
