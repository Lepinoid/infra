package cluster

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
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

func TestKubectlBaseArgs(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "token"), []byte("test-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { serviceAccountDir = "/var/run/secrets/kubernetes.io/serviceaccount" })
	serviceAccountDir = dir

	t.Run("flags with default server", func(t *testing.T) {
		args, err := kubectlBaseArgs()
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{
			"--server=https://kubernetes.default.svc",
			"--token=test-token",
			"--certificate-authority=" + dir + "/ca.crt",
			"--namespace=lepinoid",
			"--request-timeout=15s",
		} {
			if !slices.Contains(args, want) {
				t.Errorf("missing %q in %v", want, args)
			}
		}
		if slices.Contains(args, "test-token\n") || slices.Contains(args, "--token=test-token\n") {
			t.Error("token must be trimmed")
		}
	})

	t.Run("server from env", func(t *testing.T) {
		t.Setenv("KUBERNETES_SERVICE_HOST", "10.0.0.1")
		t.Setenv("KUBERNETES_SERVICE_PORT", "6443")
		args, err := kubectlBaseArgs()
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(args, "--server=https://10.0.0.1:6443") {
			t.Errorf("env override not applied: %v", args)
		}
	})

	t.Run("missing token fails", func(t *testing.T) {
		serviceAccountDir = filepath.Join(dir, "absent")
		if _, err := kubectlBaseArgs(); err == nil || !strings.Contains(err.Error(), "token") {
			t.Errorf("want token read error, got %v", err)
		}
		serviceAccountDir = dir
	})
}
