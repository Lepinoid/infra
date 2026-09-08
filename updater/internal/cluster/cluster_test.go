package cluster

import (
	"context"
	"testing"
	"time"
)

func TestCommandTimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := (Commands{Timeout: time.Second}).Run(ctx, nil, "go", "version")
	if err == nil {
		t.Fatal("cancelled command ran")
	}
}
