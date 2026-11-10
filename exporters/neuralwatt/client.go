package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// fetch の結果の分類。fetches_total の result ラベルと同じ。
const (
	resultOK              = "ok"
	resultUnauthorized    = "unauthorized"
	resultPaymentRequired = "payment_required"
	resultRateLimited     = "rate_limited"
	resultServerError     = "server_error"
	resultClientError     = "client_error"
	resultNetworkError    = "network_error"
	resultInvalidResponse = "invalid_response"
	resultCredentialError = "credential_error"
)

var allResults = []string{
	resultOK, resultUnauthorized, resultPaymentRequired, resultRateLimited, resultServerError,
	resultClientError, resultNetworkError, resultInvalidResponse, resultCredentialError,
}

// fetchError は取得の失敗。msg は固定の文言だけで、キー・Authorization・応答本文・下位のエラー文は含めない
// （ログや指標に出ても秘密が漏れないようにするため）。
type fetchError struct {
	result string
	msg    string
	// retryAfter は 429 の Retry-After（なければ nil）。
	retryAfter *time.Duration
}

func (e *fetchError) Error() string { return e.msg }

type clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

type quotaClient struct {
	endpoint string
	keyFile  string
	http     *http.Client
	clock    clock
}

func newQuotaClient(cfg config, clk clock) (*quotaClient, error) {
	endpoint, err := url.JoinPath(cfg.BaseURL, "v1", "quota")
	if err != nil {
		return nil, err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	return &quotaClient{
		endpoint: endpoint,
		keyFile:  cfg.APIKeyFile,
		clock:    clk,
		http: &http.Client{
			Transport: transport,
			Timeout:   cfg.RequestTimeout,
			// リダイレクトは追従しない。キーを別のホストへ送らないため、3xx は失敗として扱う。
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

// readKey は取得のたびにキーのファイルを読み直す（Secret の更新に再起動なしで追従する）。
func (c *quotaClient) readKey() (string, *fetchError) {
	raw, err := os.ReadFile(c.keyFile)
	if err != nil {
		return "", &fetchError{result: resultCredentialError, msg: "cannot read the API key file"}
	}
	key := strings.TrimSpace(string(raw))
	if key == "" {
		return "", &fetchError{result: resultCredentialError, msg: "the API key file is empty"}
	}
	for _, r := range key {
		if r < 0x21 || r > 0x7e {
			return "", &fetchError{result: resultCredentialError, msg: "the API key file contains characters that are not allowed in a header"}
		}
	}
	return key, nil
}

func (c *quotaClient) fetch(ctx context.Context) (*snapshot, *fetchError) {
	key, ferr := c.readKey()
	if ferr != nil {
		return nil, ferr
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint, nil)
	if err != nil {
		return nil, &fetchError{result: resultNetworkError, msg: "cannot build the request"}
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "neuralwatt-exporter")

	resp, err := c.http.Do(req)
	if err != nil {
		// err には URL などが入りうるので、文言には使わず種類だけを残す。
		if errors.Is(err, context.DeadlineExceeded) || isTimeout(err) {
			return nil, &fetchError{result: resultNetworkError, msg: "request timed out"}
		}
		return nil, &fetchError{result: resultNetworkError, msg: "request failed"}
	}
	defer resp.Body.Close()

	switch code := resp.StatusCode; {
	case code == http.StatusOK:
	case code == http.StatusUnauthorized:
		return nil, &fetchError{result: resultUnauthorized, msg: "API returned 401"}
	case code == http.StatusPaymentRequired:
		return nil, &fetchError{result: resultPaymentRequired, msg: "API returned 402"}
	case code == http.StatusTooManyRequests:
		return nil, &fetchError{result: resultRateLimited, msg: "API returned 429", retryAfter: parseRetryAfter(resp.Header.Get("Retry-After"), c.clock.Now())}
	case code >= 500:
		return nil, &fetchError{result: resultServerError, msg: "API returned a 5xx status"}
	case code >= 400:
		return nil, &fetchError{result: resultClientError, msg: "API returned a 4xx status"}
	default:
		// リダイレクト（追従しない）や 200 以外の 2xx など、想定していない応答。
		return nil, &fetchError{result: resultInvalidResponse, msg: "API returned an unexpected status"}
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, &fetchError{result: resultNetworkError, msg: "cannot read the response body"}
	}
	if len(body) > maxResponseBytes {
		return nil, &fetchError{result: resultInvalidResponse, msg: "response body is too large"}
	}
	snap, ferr := parseQuota(body)
	if ferr != nil {
		return nil, ferr
	}
	return snap, nil
}

func isTimeout(err error) bool {
	var t interface{ Timeout() bool }
	return errors.As(err, &t) && t.Timeout()
}

// parseRetryAfter は秒数または HTTP-date の Retry-After を読む。読めない・負の値は nil。
func parseRetryAfter(v string, now time.Time) *time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	var d time.Duration
	if secs, err := strconv.ParseInt(v, 10, 64); err == nil {
		if secs < 0 {
			return nil
		}
		if secs > int64(maxRetryAfter/time.Second) {
			secs = int64(maxRetryAfter / time.Second)
		}
		d = time.Duration(secs) * time.Second
	} else if t, err := http.ParseTime(v); err == nil {
		d = t.Sub(now)
		if d < 0 {
			d = 0
		}
		if d > maxRetryAfter {
			d = maxRetryAfter
		}
	} else {
		return nil
	}
	return &d
}

// --- 応答の読み取り ---

type quotaResponse struct {
	SnapshotAt *string `json:"snapshot_at"`
	Balance    *struct {
		CreditsRemainingUSD *float64 `json:"credits_remaining_usd"`
		TotalCreditsUSD     *float64 `json:"total_credits_usd"`
		AccountingMethod    string   `json:"accounting_method"`
	} `json:"balance"`
	Usage *struct {
		Lifetime     *usageWindowResponse `json:"lifetime"`
		CurrentMonth *usageWindowResponse `json:"current_month"`
	} `json:"usage"`
	Limits *struct {
		OverageLimitUSD *float64 `json:"overage_limit_usd"`
		RateLimitTier   string   `json:"rate_limit_tier"`
	} `json:"limits"`
	Subscription *struct {
		Plan             string   `json:"plan"`
		Status           string   `json:"status"`
		BillingInterval  string   `json:"billing_interval"`
		CurrentPeriodEnd *string  `json:"current_period_end"`
		KWhIncluded      *float64 `json:"kwh_included"`
		KWhUsed          *float64 `json:"kwh_used"`
		KWhRemaining     *float64 `json:"kwh_remaining"`
		InOverage        *bool    `json:"in_overage"`
	} `json:"subscription"`
	Key *struct {
		Name      string `json:"name"`
		Allowance *struct {
			LimitUSD     *float64 `json:"limit_usd"`
			Period       string   `json:"period"`
			SpentUSD     *float64 `json:"spent_usd"`
			RemainingUSD *float64 `json:"remaining_usd"`
			Blocked      *bool    `json:"blocked"`
			ResetAt      *string  `json:"reset_at"`
		} `json:"allowance"`
	} `json:"key"`
}

type usageWindowResponse struct {
	CostUSD   *float64 `json:"cost_usd"`
	Requests  *float64 `json:"requests"`
	Tokens    *float64 `json:"tokens"`
	EnergyKWh *float64 `json:"energy_kwh"`
}

// parseQuota は応答を寛容に読む。未知のフィールドは無視し、null・欠落は「その系列を出さない」にする。
// balance.credits_remaining_usd がなければ失敗（スナップショットは更新しない）。
func parseQuota(body []byte) (*snapshot, *fetchError) {
	var q quotaResponse
	if err := json.Unmarshal(body, &q); err != nil {
		return nil, &fetchError{result: resultInvalidResponse, msg: "response is not the expected JSON"}
	}
	if q.Balance == nil || q.Balance.CreditsRemainingUSD == nil {
		return nil, &fetchError{result: resultInvalidResponse, msg: "response has no balance.credits_remaining_usd"}
	}
	s := &snapshot{
		snapshotAt:       parseTime(q.SnapshotAt),
		creditsRemaining: *q.Balance.CreditsRemainingUSD,
		creditsTotal:     q.Balance.TotalCreditsUSD,
		accountingMethod: q.Balance.AccountingMethod,
	}
	if q.Limits != nil {
		s.rateLimitTier = q.Limits.RateLimitTier
		s.overageLimit = q.Limits.OverageLimitUSD
	}
	if q.Usage != nil {
		s.lifetime = toWindow(q.Usage.Lifetime)
		s.currentMonth = toWindow(q.Usage.CurrentMonth)
	}
	if sub := q.Subscription; sub != nil {
		s.subscription = &subscription{
			plan: sub.Plan, status: sub.Status, billingInterval: sub.BillingInterval,
			periodEnd:   parseTime(sub.CurrentPeriodEnd),
			kwhIncluded: sub.KWhIncluded, kwhUsed: sub.KWhUsed, kwhRemaining: sub.KWhRemaining,
			inOverage: sub.InOverage,
		}
	}
	if q.Key != nil && q.Key.Allowance != nil {
		a := q.Key.Allowance
		s.allowance = &allowance{
			keyName: q.Key.Name, period: a.Period,
			limit: a.LimitUSD, spent: a.SpentUSD, remaining: a.RemainingUSD,
			blocked: a.Blocked, resetAt: parseTime(a.ResetAt),
		}
	}
	return s, nil
}

func toWindow(w *usageWindowResponse) *usageWindow {
	if w == nil {
		return nil
	}
	return &usageWindow{cost: w.CostUSD, requests: w.Requests, tokens: w.Tokens, energyKWh: w.EnergyKWh}
}

// parseTime は RFC 3339 の時刻を読む。null・空・読めない値は nil（その系列を出さない）。
func parseTime(s *string) *time.Time {
	if s == nil || *s == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, *s)
	if err != nil {
		return nil
	}
	return &t
}
