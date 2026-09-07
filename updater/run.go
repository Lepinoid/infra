package main

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/lepinoid/infra/updater/internal/cluster"
	"github.com/lepinoid/infra/updater/internal/lease"
)

var errTransactionUnavailable = errors.New("transaction runner is not yet integrated; refusing all PVC mutations")

func run(ctx context.Context) error {
	holder := os.Getenv("JOB_UID")
	if holder == "" {
		return errUsage
	}
	commands := cluster.Commands{Timeout: 20 * time.Second}
	lock := lease.New(cluster.LeaseAPI{Commands: commands}, holder, time.Now)
	acquired, err := lock.Acquire(ctx)
	if err != nil {
		return err
	}
	if !acquired {
		return nil
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go lock.Heartbeat(ctx, cancel)
	defer func() {
		_ = lock.Release(context.Background())
	}()
	return runTransaction(ctx, lock)
}
