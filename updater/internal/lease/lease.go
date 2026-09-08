package lease

import (
	"context"
	"errors"
	"sync"
	"time"
)

var ErrLost = errors.New("lease lost or renewal deadline exceeded")

const Duration = 60 * time.Second
const RenewDeadline = 30 * time.Second

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
	mu          sync.Mutex
	lastSuccess time.Time
	lost        bool
}

func New(api API, holder string, now func() time.Time) *Lease {
	return &Lease{api: api, holder: holder, now: now}
}

func (l *Lease) Acquire(ctx context.Context) (bool, error) {
	if l.holder == "" {
		return false, ErrLost
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
	l.lost = false
	l.mu.Unlock()
	return true, nil
}

func (l *Lease) Check(ctx context.Context) error {
	l.mu.Lock()
	expired := l.lost || l.now().Sub(l.lastSuccess) >= RenewDeadline
	l.mu.Unlock()
	if expired {
		return ErrLost
	}
	r, err := l.api.Get(ctx)
	if err != nil {
		return err
	}
	if r.Holder != l.holder || !l.now().Before(r.RenewedAt.Add(time.Duration(r.Duration)*time.Second)) {
		l.mu.Lock()
		l.lost = true
		l.mu.Unlock()
		return ErrLost
	}
	return nil
}

func (l *Lease) renew(ctx context.Context) error {
	if err := l.Check(ctx); err != nil {
		return err
	}
	r, err := l.api.Get(ctx)
	if err != nil {
		return err
	}
	if r.Holder != l.holder {
		return ErrLost
	}
	r.RenewedAt = l.now()
	r.Duration = int(Duration / time.Second)
	if _, err := l.api.Update(ctx, r); err != nil {
		return err
	}
	l.mu.Lock()
	l.lastSuccess = l.now()
	l.mu.Unlock()
	return nil
}

func (l *Lease) Heartbeat(ctx context.Context, cancel context.CancelFunc) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			bounded, stop := context.WithTimeout(ctx, 5*time.Second)
			err := l.renew(bounded)
			stop()
			l.mu.Lock()
			expired := l.now().Sub(l.lastSuccess) >= RenewDeadline
			l.mu.Unlock()
			if errors.Is(err, ErrLost) || expired {
				l.mu.Lock()
				l.lost = true
				l.mu.Unlock()
				cancel()
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
		return ErrLost
	}
	r.Holder = ""
	_, err = l.api.Update(ctx, r)
	return err
}
