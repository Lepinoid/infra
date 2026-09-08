package lease

import (
	"context"
	"testing"
	"time"
)

type memory struct{ value Record }

func (m *memory) Get(context.Context) (Record, error)                { return m.value, nil }
func (m *memory) Update(_ context.Context, r Record) (Record, error) { m.value = r; return r, nil }

func TestLiveHolderIsNotTaken(t *testing.T) {
	now := time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)
	api := &memory{Record{Holder: "other", RenewedAt: now, Duration: 60}}
	l := New(api, "self", func() time.Time { return now })
	got, err := l.Acquire(context.Background())
	if err != nil || got {
		t.Fatalf("%v %v", got, err)
	}
}

func TestExpiredHolderAndFencing(t *testing.T) {
	now := time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)
	api := &memory{Record{Holder: "old", RenewedAt: now.Add(-61 * time.Second), Duration: 60}}
	l := New(api, "self", func() time.Time { return now })
	got, err := l.Acquire(context.Background())
	if err != nil || !got {
		t.Fatalf("%v %v", got, err)
	}
	api.value.Holder = "new"
	if err := l.Check(context.Background()); err == nil {
		t.Fatal("lost lease accepted")
	}
}
