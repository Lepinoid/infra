package main

import (
	"context"
	"errors"
	"log"
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
	return runWithLease(ctx, lock, runTransaction)
}

func runWithLease(ctx context.Context, lock *lease.Lease, transaction func(context.Context, *lease.Lease) error) error {
	acquired, err := lock.Acquire(ctx)
	if err != nil {
		return err
	}
	if !acquired {
		return nil
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	heartbeatCtx, stopHeartbeat := context.WithCancel(ctx)
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		lock.Heartbeat(heartbeatCtx, cancel)
	}()
	err = transaction(ctx, lock)
	// Join before releasing so an in-flight renewal cannot race our own release
	// or report the intentional empty holder as a loss of authority.
	stopHeartbeat()
	<-heartbeatDone
	if cause := context.Cause(ctx); cause != nil && !errors.Is(err, cause) {
		err = errors.Join(err, cause)
	}
	releaseCtx, stopRelease := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopRelease()
	if releaseErr := lock.Release(releaseCtx); releaseErr != nil {
		log.Printf("lease release failed: %v", releaseErr)
	}
	return err
}
