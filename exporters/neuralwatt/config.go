package main

import (
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	defaultBaseURL      = "https://api.neuralwatt.com"
	defaultPollInterval = 60 * time.Second
	minPollInterval     = 10 * time.Second
	defaultListen       = ":8080"
	// requestTimeout は /v1/quota 1 回あたりの上限（接続・応答本文の読み取りを含む）。
	requestTimeout = 10 * time.Second
	// maxResponseBytes は応答本文の上限。これを超えたら invalid_response として扱う。
	maxResponseBytes = 1 << 20
	// maxBackoff は Retry-After がない 429 の指数バックオフの上限。
	maxBackoff = 10 * time.Minute
	// maxRetryAfter は Retry-After に従う待ちの上限（異常な値で取得が止まり続けるのを防ぐ）。
	maxRetryAfter = 24 * time.Hour
)

// config は環境変数から読む設定。API キーの値は持たない（ファイルのパスだけ）。
type config struct {
	APIKeyFile       string
	BaseURL          string
	PollInterval     time.Duration
	MonthlyBudgetUSD *float64
	ListenAddress    string
	// RequestTimeout は環境変数では変えられない（テストだけが差し替える）。
	RequestTimeout time.Duration
}

func loadConfig(getenv func(string) string) (config, error) {
	cfg := config{
		APIKeyFile:     strings.TrimSpace(getenv("NEURALWATT_API_KEY_FILE")),
		BaseURL:        defaultBaseURL,
		PollInterval:   defaultPollInterval,
		ListenAddress:  defaultListen,
		RequestTimeout: requestTimeout,
	}
	if cfg.APIKeyFile == "" {
		return config{}, errors.New("NEURALWATT_API_KEY_FILE is required")
	}
	if v := strings.TrimSpace(getenv("NEURALWATT_BASE_URL")); v != "" {
		if err := validateBaseURL(v); err != nil {
			return config{}, fmt.Errorf("NEURALWATT_BASE_URL: %w", err)
		}
		cfg.BaseURL = strings.TrimRight(v, "/")
	}
	if v := strings.TrimSpace(getenv("NEURALWATT_POLL_INTERVAL")); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return config{}, errors.New("NEURALWATT_POLL_INTERVAL must be a duration such as 60s")
		}
		if d < minPollInterval {
			return config{}, fmt.Errorf("NEURALWATT_POLL_INTERVAL must be at least %s", minPollInterval)
		}
		cfg.PollInterval = d
	}
	if v := strings.TrimSpace(getenv("NEURALWATT_MONTHLY_BUDGET_USD")); v != "" {
		b, err := strconv.ParseFloat(v, 64)
		if err != nil || math.IsNaN(b) || math.IsInf(b, 0) || b <= 0 {
			return config{}, errors.New("NEURALWATT_MONTHLY_BUDGET_USD must be a positive number")
		}
		cfg.MonthlyBudgetUSD = &b
	}
	if v := strings.TrimSpace(getenv("LISTEN_ADDRESS")); v != "" {
		cfg.ListenAddress = v
	}
	return cfg, nil
}

// validateBaseURL は API キーを平文で送らないよう、https か、ループバック宛ての http だけを許す。
func validateBaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return errors.New("must be an absolute URL")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("must not contain credentials, a query or a fragment")
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		host := u.Hostname()
		if host == "localhost" {
			return nil
		}
		if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
			return nil
		}
		return errors.New("http is allowed only for loopback hosts")
	default:
		return errors.New("scheme must be https")
	}
}
