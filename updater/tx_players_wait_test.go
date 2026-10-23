package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/lepinoid/infra/updater/internal/cluster"
)

// playersWaitRunner は B3 の在室確認と通知だけを受け付ける fake。
// counts[i] は i 回目の在室確認で観測させる人数で、使い切った後は最後の値を返し続ける。
type playersWaitRunner struct {
	podName, podUID string
	counts          []int
	polls           int
	tellraws        [][]string
	tellrawErr      error
}

func (r *playersWaitRunner) current() int {
	i := min(r.polls-1, len(r.counts)-1)
	return r.counts[max(i, 0)]
}

func (r *playersWaitRunner) Run(context.Context, []byte, string, ...string) ([]byte, error) {
	return nil, errors.New("unexpected generic command")
}

func (r *playersWaitRunner) Kubectl(_ context.Context, _ []byte, args ...string) ([]byte, error) {
	if slices.Equal(args, []string{"get", "pod", r.podName, "-o", "json"}) {
		return json.Marshal(map[string]any{"metadata": map[string]string{"name": r.podName, "uid": r.podUID}})
	}
	return nil, fmt.Errorf("unexpected kubectl command: %q", args)
}

func (r *playersWaitRunner) Exec(_ context.Context, _ cluster.Pod, container string, args ...string) ([]byte, error) {
	if container != "minecraft" {
		return nil, fmt.Errorf("unexpected container %q", container)
	}
	switch {
	case slices.Equal(args, []string{"sh", "-c", "sed -n 's/^rcon.password=//p' /data/server.properties"}):
		return []byte("wait-secret\n"), nil
	case slices.Equal(args, []string{"rcon-cli", "--password", "wait-secret", "list"}):
		r.polls++
		n := r.current()
		names := make([]string, n)
		for i := range names {
			names[i] = fmt.Sprintf("player%d", i)
		}
		return []byte(fmt.Sprintf("There are %d of a max of 20 players online: %s", n, strings.Join(names, ", "))), nil
	case slices.Equal(args, []string{"mc-monitor", "status", "--host", "localhost", "--port", "25565"}):
		return []byte(fmt.Sprintf("localhost:25565 : version=Paper 1.21.8 online=%d max=20 motd='test'", r.current())), nil
	case len(args) == 6 && slices.Equal(args[:5], []string{"rcon-cli", "--password", "wait-secret", "tellraw", "@a"}):
		r.tellraws = append(r.tellraws, slices.Clone(args[5:]))
		return nil, r.tellrawErr
	}
	return nil, fmt.Errorf("unexpected exec: %q", args)
}

func playersWaitTransaction(t *testing.T, counts ...int) (*tx, *playersWaitRunner, *bytes.Buffer) {
	t.Helper()
	x := checkpointTransaction(t)
	r := &playersWaitRunner{podName: x.podName, podUID: x.podUID, counts: counts}
	x.commands = r
	log := &bytes.Buffer{}
	x.engine.Log = log
	return x, r, log
}

func assertWaitingNotification(t *testing.T, r *playersWaitRunner) {
	t.Helper()
	if len(r.tellraws) != 1 {
		t.Fatalf("want exactly one notification per B3 run, got %d: %q", len(r.tellraws), r.tellraws)
	}
	var payload map[string]string
	if err := json.Unmarshal([]byte(r.tellraws[0][0]), &payload); err != nil {
		t.Fatalf("tellraw payload is not JSON text component: %q: %v", r.tellraws[0][0], err)
	}
	if payload["text"] != maintenanceWaitingMessage || payload["color"] != "yellow" || len(payload) != 2 {
		t.Fatalf("unexpected tellraw payload: %v", payload)
	}
}

func TestB3NotifiesOnceWhilePlayersRemain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		x, r, _ := playersWaitTransaction(t, 2, 1, 1, 0)
		if err := x.b3EmptyPlayers(); err != nil {
			t.Fatalf("B3 must proceed once players leave: %v", err)
		}
		if r.polls < 5 {
			t.Fatalf("B3 returned before observing a minute of zero players: polls=%d", r.polls)
		}
		assertWaitingNotification(t, r)
	})
}

func TestB3NotifiesOnceBeforeTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		x, r, _ := playersWaitTransaction(t, 1)
		assertCheckpointSuspended(t, x, x.b3EmptyPlayers(), "checkpoint-timeout")
		if r.polls < 10 {
			t.Fatalf("B3 must keep polling until the deadline: polls=%d", r.polls)
		}
		assertWaitingNotification(t, r)
	})
}

func TestB3DoesNotNotifyWhenAlreadyEmpty(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		x, r, _ := playersWaitTransaction(t, 0)
		if err := x.b3EmptyPlayers(); err != nil {
			t.Fatal(err)
		}
		if len(r.tellraws) != 0 {
			t.Fatalf("no one is online, but notified: %q", r.tellraws)
		}
	})
}

func TestB3NotificationFailureDoesNotStopTransaction(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		x, r, log := playersWaitTransaction(t, 1, 0)
		r.tellrawErr = errors.New("rcon unavailable")
		if err := x.b3EmptyPlayers(); err != nil {
			t.Fatalf("notification failure must not stop B3: %v", err)
		}
		assertWaitingNotification(t, r)
		if !strings.Contains(log.String(), "maintenance waiting notification failed") {
			t.Fatalf("notification failure was not logged: %q", log.String())
		}
	})
}
