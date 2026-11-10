package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

func envFrom(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadConfigDefaults(t *testing.T) {
	cfg, err := loadConfig(envFrom(map[string]string{"NEURALWATT_API_KEY_FILE": "/var/run/neuralwatt/api-key"}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.APIKeyFile != "/var/run/neuralwatt/api-key" || cfg.BaseURL != "https://api.neuralwatt.com" ||
		cfg.PollInterval != 60*time.Second || cfg.ListenAddress != ":8080" || cfg.MonthlyBudgetUSD != nil ||
		cfg.RequestTimeout != 10*time.Second {
		t.Errorf("unexpected defaults: %+v", cfg)
	}
}

func TestLoadConfigValues(t *testing.T) {
	cfg, err := loadConfig(envFrom(map[string]string{
		"NEURALWATT_API_KEY_FILE":       "/k",
		"NEURALWATT_BASE_URL":           "http://127.0.0.1:9999/",
		"NEURALWATT_POLL_INTERVAL":      "10s",
		"NEURALWATT_MONTHLY_BUDGET_USD": "20",
		"LISTEN_ADDRESS":                "127.0.0.1:9100",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BaseURL != "http://127.0.0.1:9999" || cfg.PollInterval != 10*time.Second ||
		cfg.MonthlyBudgetUSD == nil || *cfg.MonthlyBudgetUSD != 20 || cfg.ListenAddress != "127.0.0.1:9100" {
		t.Errorf("unexpected config: %+v", cfg)
	}
}

func TestLoadConfigRejects(t *testing.T) {
	base := map[string]string{"NEURALWATT_API_KEY_FILE": "/k"}
	with := func(k, v string) map[string]string {
		m := map[string]string{}
		for kk, vv := range base {
			m[kk] = vv
		}
		m[k] = v
		return m
	}
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"missing key file", map[string]string{}, "NEURALWATT_API_KEY_FILE is required"},
		{"blank key file", map[string]string{"NEURALWATT_API_KEY_FILE": "  "}, "NEURALWATT_API_KEY_FILE is required"},
		{"interval below the floor", with("NEURALWATT_POLL_INTERVAL", "9s"), "at least 10s"},
		{"interval without a unit", with("NEURALWATT_POLL_INTERVAL", "60"), "duration"},
		{"interval negative", with("NEURALWATT_POLL_INTERVAL", "-1m"), "at least 10s"},
		{"budget not a number", with("NEURALWATT_MONTHLY_BUDGET_USD", "twenty"), "positive number"},
		{"budget zero", with("NEURALWATT_MONTHLY_BUDGET_USD", "0"), "positive number"},
		{"budget negative", with("NEURALWATT_MONTHLY_BUDGET_USD", "-3"), "positive number"},
		{"budget NaN", with("NEURALWATT_MONTHLY_BUDGET_USD", "NaN"), "positive number"},
		{"base URL over plain http to a remote host", with("NEURALWATT_BASE_URL", "http://api.neuralwatt.com"), "loopback"},
		{"base URL with credentials", with("NEURALWATT_BASE_URL", "https://u:p@api.neuralwatt.com"), "credentials"},
		{"base URL with another scheme", with("NEURALWATT_BASE_URL", "ftp://api.neuralwatt.com"), "scheme"},
		{"base URL not absolute", with("NEURALWATT_BASE_URL", "api.neuralwatt.com"), "absolute"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadConfig(envFrom(tc.env))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

// 起動から終了まで（実時計・実ソケット）。取得し、/metrics と /healthz が答え、SIGTERM 相当で止まる。
func TestRunEndToEnd(t *testing.T) {
	api := newFakeAPI(t)
	api.set(serveFile(t, "quota_example.json"))
	keyPath := t.TempDir() + "/api-key"
	if err := os.WriteFile(keyPath, []byte(testKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var logs syncWriter
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, envFrom(map[string]string{
			"NEURALWATT_API_KEY_FILE":       keyPath,
			"NEURALWATT_BASE_URL":           api.srv.URL,
			"NEURALWATT_MONTHLY_BUDGET_USD": "20",
			"LISTEN_ADDRESS":                addr,
		}), &logs)
	}()

	get := func(path string) (int, string) {
		resp, err := http.Get("http://" + addr + path)
		if err != nil {
			return 0, err.Error()
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	deadline := time.Now().Add(10 * time.Second)
	var body string
	for time.Now().Before(deadline) {
		code, b := get("/metrics")
		if code == 200 && strings.Contains(b, "neuralwatt_exporter_fetch_success 1") {
			body = b
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if body == "" {
		t.Fatalf("no successful fetch was exposed in time; logs:\n%s", logs.String())
	}
	for _, want := range []string{
		"neuralwatt_credits_remaining_usd 32.6774",
		`neuralwatt_usage_cost_usd_total{window="current_month"} 160.1463`,
		"neuralwatt_monthly_budget_usd 20",
		"process_start_time_seconds ",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics lacks %q", want)
		}
	}
	if code, _ := get("/healthz"); code != 200 {
		t.Errorf("/healthz = %d", code)
	}
	if n := api.hits.Load(); n != 1 {
		t.Errorf("API hits = %d, want 1", n)
	}
	if strings.Contains(body, testKey) || strings.Contains(logs.String(), testKey) {
		t.Error("the key leaked")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("run returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not stop after the context was cancelled")
	}
}

func TestRunRejectsBadConfig(t *testing.T) {
	err := run(t.Context(), envFrom(map[string]string{}), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "NEURALWATT_API_KEY_FILE") {
		t.Errorf("err = %v", err)
	}
}
