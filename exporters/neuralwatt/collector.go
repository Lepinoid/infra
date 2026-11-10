package main

import (
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

type usageWindow struct {
	cost, requests, tokens, energyKWh *float64
}

type subscription struct {
	plan, status, billingInterval      string
	periodEnd                          *time.Time
	kwhIncluded, kwhUsed, kwhRemaining *float64
	inOverage                          *bool
}

type allowance struct {
	keyName, period         string
	limit, spent, remaining *float64
	blocked                 *bool
	resetAt                 *time.Time
}

// snapshot は最後に成功した /v1/quota の応答。nil のフィールドは「その系列を出さない」。
type snapshot struct {
	snapshotAt       *time.Time
	creditsRemaining float64
	creditsTotal     *float64
	accountingMethod string
	rateLimitTier    string
	overageLimit     *float64
	lifetime         *usageWindow
	currentMonth     *usageWindow
	subscription     *subscription
	allowance        *allowance
}

// state は取得の状態と、最後に成功したスナップショット。失敗しても snap は残す。
type state struct {
	mu          sync.RWMutex
	attempted   bool
	success     bool
	duration    time.Duration
	lastSuccess time.Time
	snap        *snapshot
	counts      map[string]uint64
}

func newState() *state {
	counts := make(map[string]uint64, len(allResults))
	for _, r := range allResults {
		counts[r] = 0
	}
	return &state{counts: counts}
}

// record は 1 回の取得の結果を反映する。snap は成功のときだけ渡す。
func (s *state) record(result string, d time.Duration, now time.Time, snap *snapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attempted = true
	s.duration = d
	s.counts[result]++
	if snap != nil {
		s.success = true
		s.lastSuccess = now
		s.snap = snap
		return
	}
	s.success = false
}

type collector struct {
	st     *state
	budget *float64

	fetchSuccess, lastSuccess, fetchDuration, fetches, snapshotTS   *prometheus.Desc
	creditsRemaining, creditsTotal, accountInfo                     *prometheus.Desc
	usageCost, usageRequests, usageTokens, usageEnergy              *prometheus.Desc
	overageLimit                                                    *prometheus.Desc
	subInfo, subIncluded, subUsed, subRemaining, subOverage, subEnd *prometheus.Desc
	keyLimit, keySpent, keyRemaining, keyBlocked, keyReset          *prometheus.Desc
	monthlyBudget                                                   *prometheus.Desc
}

func newCollector(st *state, budget *float64) *collector {
	d := func(name, help string, labels ...string) *prometheus.Desc {
		return prometheus.NewDesc(name, help, labels, nil)
	}
	keyLabels := []string{"key_name", "period"}
	win := []string{"window"}
	return &collector{
		st:     st,
		budget: budget,

		fetchSuccess:  d("neuralwatt_exporter_fetch_success", "1 if the latest fetch of /v1/quota succeeded, 0 if it failed. Absent until the first fetch finishes."),
		lastSuccess:   d("neuralwatt_exporter_last_success_timestamp_seconds", "Unix time of the last successful fetch. 0 if no fetch has succeeded yet."),
		fetchDuration: d("neuralwatt_exporter_fetch_duration_seconds", "Duration of the latest fetch attempt. Absent until the first fetch finishes."),
		fetches:       d("neuralwatt_exporter_fetches_total", "Fetch attempts of /v1/quota by result.", "result"),
		snapshotTS:    d("neuralwatt_quota_snapshot_timestamp_seconds", "snapshot_at of the last successful response."),

		creditsRemaining: d("neuralwatt_credits_remaining_usd", "Current USD credit balance (balance.credits_remaining_usd)."),
		creditsTotal:     d("neuralwatt_credits_total_usd", "Lifetime credits purchased or granted in USD (balance.total_credits_usd)."),
		accountInfo:      d("neuralwatt_account_info", "Account information. Always 1.", "accounting_method", "rate_limit_tier"),

		usageCost:     d("neuralwatt_usage_cost_usd_total", "Usage cost in USD. current_month resets to 0 at a calendar month boundary.", win...),
		usageRequests: d("neuralwatt_usage_requests_total", "Request count. current_month resets to 0 at a calendar month boundary.", win...),
		usageTokens:   d("neuralwatt_usage_tokens_total", "Token count (input + output). current_month resets to 0 at a calendar month boundary.", win...),
		usageEnergy:   d("neuralwatt_usage_energy_kwh_total", "Physical energy consumed in kWh. current_month resets to 0 at a calendar month boundary.", win...),

		overageLimit: d("neuralwatt_limits_overage_limit_usd", "Account-level overage spending cap in USD. Absent when no cap is set."),

		subInfo:      d("neuralwatt_subscription_info", "Subscription information. Always 1. Absent without a subscription.", "plan", "status", "billing_interval"),
		subIncluded:  d("neuralwatt_subscription_kwh_included", "kWh included in the current billing period."),
		subUsed:      d("neuralwatt_subscription_kwh_used", "Charged kWh drawn against the allowance this period."),
		subRemaining: d("neuralwatt_subscription_kwh_remaining", "kWh remaining in the allowance (charged basis)."),
		subOverage:   d("neuralwatt_subscription_in_overage", "1 if usage has exceeded the included allocation."),
		subEnd:       d("neuralwatt_subscription_period_end_timestamp_seconds", "Unix time of the end of the current subscription period."),

		keyLimit:     d("neuralwatt_key_allowance_limit_usd", "Spending limit of the API key in USD. Absent when the key has no limit.", keyLabels...),
		keySpent:     d("neuralwatt_key_allowance_spent_usd", "Amount spent by the API key in the current period in USD.", keyLabels...),
		keyRemaining: d("neuralwatt_key_allowance_remaining_usd", "Amount remaining for the API key in the current period in USD.", keyLabels...),
		keyBlocked:   d("neuralwatt_key_allowance_blocked", "1 if the API key is blocked because its limit was reached.", keyLabels...),
		keyReset:     d("neuralwatt_key_allowance_reset_timestamp_seconds", "Unix time when the API key's allowance resets.", keyLabels...),

		monthlyBudget: d("neuralwatt_monthly_budget_usd", "Configured monthly budget in USD (NEURALWATT_MONTHLY_BUDGET_USD). Absent when not configured."),
	}
}

func (c *collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{
		c.fetchSuccess, c.lastSuccess, c.fetchDuration, c.fetches, c.snapshotTS,
		c.creditsRemaining, c.creditsTotal, c.accountInfo,
		c.usageCost, c.usageRequests, c.usageTokens, c.usageEnergy, c.overageLimit,
		c.subInfo, c.subIncluded, c.subUsed, c.subRemaining, c.subOverage, c.subEnd,
		c.keyLimit, c.keySpent, c.keyRemaining, c.keyBlocked, c.keyReset, c.monthlyBudget,
	} {
		ch <- d
	}
}

// unixSeconds は秒の小数で返す。UnixNano を経由すると 1.7763642602499998e+09 のように丸め誤差が出る。
func unixSeconds(t time.Time) float64 { return float64(t.Unix()) + float64(t.Nanosecond())/1e9 }

func boolValue(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func (c *collector) Collect(ch chan<- prometheus.Metric) {
	c.st.mu.RLock()
	defer c.st.mu.RUnlock()

	emit := func(desc *prometheus.Desc, typ prometheus.ValueType, v float64, labels ...string) {
		ch <- prometheus.MustNewConstMetric(desc, typ, v, labels...)
	}
	gauge := func(desc *prometheus.Desc, v float64, labels ...string) {
		emit(desc, prometheus.GaugeValue, v, labels...)
	}
	optGauge := func(desc *prometheus.Desc, v *float64, labels ...string) {
		if v != nil {
			gauge(desc, *v, labels...)
		}
	}
	optBool := func(desc *prometheus.Desc, v *bool, labels ...string) {
		if v != nil {
			gauge(desc, boolValue(*v), labels...)
		}
	}
	optTime := func(desc *prometheus.Desc, v *time.Time, labels ...string) {
		if v != nil {
			gauge(desc, unixSeconds(*v), labels...)
		}
	}

	if c.st.attempted {
		gauge(c.fetchSuccess, boolValue(c.st.success))
		gauge(c.fetchDuration, c.st.duration.Seconds())
	}
	if c.st.lastSuccess.IsZero() {
		gauge(c.lastSuccess, 0)
	} else {
		gauge(c.lastSuccess, unixSeconds(c.st.lastSuccess))
	}
	for _, r := range allResults {
		emit(c.fetches, prometheus.CounterValue, float64(c.st.counts[r]), r)
	}
	optGauge(c.monthlyBudget, c.budget)

	s := c.st.snap
	if s == nil {
		return
	}
	optTime(c.snapshotTS, s.snapshotAt)
	gauge(c.creditsRemaining, s.creditsRemaining)
	optGauge(c.creditsTotal, s.creditsTotal)
	gauge(c.accountInfo, 1, s.accountingMethod, s.rateLimitTier)
	optGauge(c.overageLimit, s.overageLimit)

	for _, w := range []struct {
		name string
		win  *usageWindow
	}{{"current_month", s.currentMonth}, {"lifetime", s.lifetime}} {
		if w.win == nil {
			continue
		}
		for _, m := range []struct {
			desc *prometheus.Desc
			v    *float64
		}{{c.usageCost, w.win.cost}, {c.usageRequests, w.win.requests}, {c.usageTokens, w.win.tokens}, {c.usageEnergy, w.win.energyKWh}} {
			if m.v != nil {
				emit(m.desc, prometheus.CounterValue, *m.v, w.name)
			}
		}
	}

	if sub := s.subscription; sub != nil {
		gauge(c.subInfo, 1, sub.plan, sub.status, sub.billingInterval)
		optGauge(c.subIncluded, sub.kwhIncluded)
		optGauge(c.subUsed, sub.kwhUsed)
		optGauge(c.subRemaining, sub.kwhRemaining)
		optBool(c.subOverage, sub.inOverage)
		optTime(c.subEnd, sub.periodEnd)
	}

	if a := s.allowance; a != nil {
		labels := []string{a.keyName, a.period}
		optGauge(c.keyLimit, a.limit, labels...)
		optGauge(c.keySpent, a.spent, labels...)
		optGauge(c.keyRemaining, a.remaining, labels...)
		optBool(c.keyBlocked, a.blocked, labels...)
		optTime(c.keyReset, a.resetAt, labels...)
	}
}
