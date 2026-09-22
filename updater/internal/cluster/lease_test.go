package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lepinoid/infra/updater/internal/lease"
)

type leaseRunner struct {
	Runner
	call func([]byte, []string) ([]byte, error)
}

func (r leaseRunner) Kubectl(_ context.Context, input []byte, args ...string) ([]byte, error) {
	return r.call(input, args)
}

func TestLeaseUpdatePreservesMetadataAndUsesCAS(t *testing.T) {
	now := time.Date(2026, 9, 23, 1, 2, 3, 456789123, time.UTC)
	for _, holder := range []string{"job-uid", ""} {
		t.Run("holder="+holder, func(t *testing.T) {
			api := LeaseAPI{Commands: leaseRunner{call: func(input []byte, args []string) ([]byte, error) {
				want := []string{"patch", "lease", "plugin-updater", "--type=merge", "--field-manager=plugin-updater", "-p"}
				if len(args) != 9 || !slices.Equal(args[:6], want) || !slices.Equal(args[7:], []string{"-o", "json"}) || len(input) != 0 {
					t.Fatalf("unsafe lease update: %q", args)
				}
				var patch map[string]json.RawMessage
				if err := json.Unmarshal([]byte(args[6]), &patch); err != nil {
					t.Fatal(err)
				}
				if len(patch) != 2 {
					t.Fatalf("patch must only change metadata.resourceVersion and spec: %s", args[6])
				}
				var metadata map[string]string
				if err := json.Unmarshal(patch["metadata"], &metadata); err != nil {
					t.Fatal(err)
				}
				if len(metadata) != 1 || metadata["resourceVersion"] != "42" {
					t.Fatalf("patch loses CAS or overwrites Flux metadata: %s", patch["metadata"])
				}
				var spec map[string]any
				if err := json.Unmarshal(patch["spec"], &spec); err != nil {
					t.Fatal(err)
				}
				if len(spec) != 3 || spec["holderIdentity"] != holder || spec["renewTime"] != "2026-09-23T01:02:03.456789Z" || spec["leaseDurationSeconds"] != float64(60) {
					t.Fatalf("incorrect runtime lease patch: %s", patch["spec"])
				}
				return []byte(`{"metadata":{"resourceVersion":"43"}}`), nil
			}}}
			updated, err := api.Update(context.Background(), lease.Record{ResourceVersion: "42", Holder: holder, RenewedAt: now, Duration: 60})
			if err != nil || updated.ResourceVersion != "43" || updated.Holder != holder {
				t.Fatalf("update: %+v, %v", updated, err)
			}
		})
	}
}

func TestLeaseUpdateRejectsMissingResourceVersion(t *testing.T) {
	api := LeaseAPI{Commands: leaseRunner{call: func([]byte, []string) ([]byte, error) {
		t.Fatal("unconditional lease update issued")
		return nil, nil
	}}}
	if _, err := api.Update(context.Background(), lease.Record{}); err == nil || !strings.Contains(err.Error(), "resourceVersion") {
		t.Fatalf("want missing resourceVersion error, got %v", err)
	}
}

func TestLeaseUpdateDoesNotRetryConflictWithoutReacquiring(t *testing.T) {
	conflict := errors.New("resourceVersion conflict")
	calls := 0
	api := LeaseAPI{Commands: leaseRunner{call: func([]byte, []string) ([]byte, error) {
		calls++
		return nil, conflict
	}}}
	_, err := api.Update(context.Background(), lease.Record{ResourceVersion: "42"})
	if !errors.Is(err, conflict) || calls != 1 {
		t.Fatalf("conflicting lease update must fail closed: calls=%d err=%v", calls, err)
	}
}
