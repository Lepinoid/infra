package cluster

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"time"

	"github.com/lepinoid/infra/updater/internal/lease"
)

type Commands struct{ Timeout time.Duration }

func (c Commands) Run(ctx context.Context, input []byte, name string, args ...string) ([]byte, error) {
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	bounded, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(bounded, name, args...)
	cmd.Stdin = bytes.NewReader(input)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s: %w: %s", name, err, stderr.String())
	}
	return out, nil
}

func (c Commands) Kubectl(ctx context.Context, input []byte, args ...string) ([]byte, error) {
	return c.Run(ctx, input, "kubectl", append([]string{"--namespace=lepinoid", "--request-timeout=15s"}, args...)...)
}

type LeaseAPI struct{ Commands Commands }
type leaseWire struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   struct {
		Name            string `json:"name"`
		ResourceVersion string `json:"resourceVersion"`
	} `json:"metadata"`
	Spec struct {
		Holder    string    `json:"holderIdentity"`
		RenewedAt time.Time `json:"renewTime"`
		Duration  int       `json:"leaseDurationSeconds"`
	} `json:"spec"`
}

func (a LeaseAPI) Get(ctx context.Context) (lease.Record, error) {
	data, err := a.Commands.Kubectl(ctx, nil, "get", "lease", "plugin-updater", "-o", "json")
	if err != nil {
		return lease.Record{}, err
	}
	var wire leaseWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return lease.Record{}, err
	}
	return lease.Record{ResourceVersion: wire.Metadata.ResourceVersion, Holder: wire.Spec.Holder, RenewedAt: wire.Spec.RenewedAt, Duration: wire.Spec.Duration}, nil
}

func (a LeaseAPI) Update(ctx context.Context, r lease.Record) (lease.Record, error) {
	wire := leaseWire{APIVersion: "coordination.k8s.io/v1", Kind: "Lease"}
	wire.Metadata.Name = "plugin-updater"
	wire.Metadata.ResourceVersion = r.ResourceVersion
	wire.Spec.Holder = r.Holder
	wire.Spec.RenewedAt = r.RenewedAt
	wire.Spec.Duration = r.Duration
	data, err := json.Marshal(wire)
	if err != nil {
		return lease.Record{}, err
	}
	out, err := a.Commands.Kubectl(ctx, data, "replace", "-f", "-", "-o", "json")
	if err != nil {
		return lease.Record{}, err
	}
	if err := json.Unmarshal(out, &wire); err != nil {
		return lease.Record{}, err
	}
	r.ResourceVersion = wire.Metadata.ResourceVersion
	return r, nil
}

type Pod struct {
	Name string
	UID  string
}

func (c Commands) Pod(ctx context.Context) (Pod, error) {
	data, err := c.Kubectl(ctx, nil, "get", "pods", "-l", "app=build-server", "-o", "json")
	if err != nil {
		return Pod{}, err
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name    string     `json:"name"`
				UID     string     `json:"uid"`
				Deleted *time.Time `json:"deletionTimestamp"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal(data, &list); err != nil {
		return Pod{}, err
	}
	var pods []Pod
	for _, item := range list.Items {
		if item.Metadata.Deleted == nil {
			pods = append(pods, Pod{item.Metadata.Name, item.Metadata.UID})
		}
	}
	if len(pods) != 1 {
		return Pod{}, errors.New("expected exactly one build-server Pod")
	}
	return pods[0], nil
}

func (c Commands) Exec(ctx context.Context, pod Pod, container string, args ...string) ([]byte, error) {
	return c.Kubectl(ctx, nil, append([]string{"exec", pod.Name, "-c", container, "--"}, args...)...)
}
