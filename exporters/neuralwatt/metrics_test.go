package main

import (
	"strings"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
)

func ptr[T any](v T) *T { return &v }

func unix(s string) float64 {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return float64(t.Unix())
}

// 公式ドキュメントの例（testdata/quota_example.json）で、名前・型・値が期待どおりに出る。
func TestExampleResponseMetrics(t *testing.T) {
	e := newEnv(t, envOpts{budget: ptr(20.0)})
	e.api.set(serveFile(t, "quota_example.json"))
	e.step(t)

	e.compareGolden(t, "metrics_doc_example.golden")

	lastSuccess := unixSeconds(baseTime.Add(250 * time.Millisecond))
	want := map[string]float64{
		"neuralwatt_exporter_fetch_success":                                                      1,
		"neuralwatt_exporter_last_success_timestamp_seconds":                                     lastSuccess,
		"neuralwatt_exporter_fetch_duration_seconds":                                             0.25,
		`neuralwatt_exporter_fetches_total{result="ok"}`:                                         1,
		"neuralwatt_quota_snapshot_timestamp_seconds":                                            unix("2026-04-16T18:30:00Z"),
		"neuralwatt_credits_remaining_usd":                                                       32.6774,
		"neuralwatt_credits_total_usd":                                                           52.34,
		`neuralwatt_account_info{accounting_method="energy",rate_limit_tier="standard"}`:         1,
		`neuralwatt_usage_cost_usd_total{window="current_month"}`:                                160.1463,
		`neuralwatt_usage_cost_usd_total{window="lifetime"}`:                                     243.9145,
		`neuralwatt_usage_requests_total{window="current_month"}`:                                23902,
		`neuralwatt_usage_requests_total{window="lifetime"}`:                                     37801,
		`neuralwatt_usage_tokens_total{window="current_month"}`:                                  1116658995,
		`neuralwatt_usage_tokens_total{window="lifetime"}`:                                       1235477176,
		`neuralwatt_usage_energy_kwh_total{window="current_month"}`:                              9.7278,
		`neuralwatt_usage_energy_kwh_total{window="lifetime"}`:                                   15.6009,
		`neuralwatt_subscription_info{billing_interval="month",plan="standard",status="active"}`: 1,
		"neuralwatt_subscription_kwh_included":                                                   20,
		"neuralwatt_subscription_kwh_used":                                                       13.9023,
		"neuralwatt_subscription_kwh_remaining":                                                  6.0977,
		"neuralwatt_subscription_in_overage":                                                     0,
		"neuralwatt_subscription_period_end_timestamp_seconds":                                   unix("2026-05-11T05:05:25Z"),
		"neuralwatt_monthly_budget_usd":                                                          20,
	}
	for _, r := range allResults[1:] {
		want[`neuralwatt_exporter_fetches_total{result="`+r+`"}`] = 0
	}
	got := e.samples(t)
	for k, v := range want {
		g, ok := got[k]
		if !ok {
			t.Errorf("series %s is missing", k)
		} else if g != v {
			t.Errorf("%s = %v, want %v", k, g, v)
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			t.Errorf("unexpected series %s", k)
		}
	}
}

// 指標の型（counter か gauge か）が契約の表どおり。
func TestMetricTypes(t *testing.T) {
	e := newEnv(t, envOpts{budget: ptr(20.0)})
	e.api.set(serveFile(t, "quota_allowance.json"))
	e.step(t)
	families, err := e.app.registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	types := map[string]dto.MetricType{}
	for _, mf := range families {
		types[mf.GetName()] = mf.GetType()
	}
	counters := map[string]bool{
		"neuralwatt_exporter_fetches_total": true,
		"neuralwatt_usage_cost_usd_total":   true,
		"neuralwatt_usage_requests_total":   true,
		"neuralwatt_usage_tokens_total":     true,
		"neuralwatt_usage_energy_kwh_total": true,
	}
	for _, name := range contractMetricNames {
		typ, ok := types[name]
		if !ok {
			t.Errorf("%s is not exposed", name)
			continue
		}
		want := dto.MetricType_GAUGE
		if counters[name] {
			want = dto.MetricType_COUNTER
		}
		if typ != want {
			t.Errorf("%s has type %v, want %v", name, typ, want)
		}
	}
}

// key.allowance があるときの系列（ラベル key_name・period を含む）と overage_limit。
func TestAllowanceMetrics(t *testing.T) {
	e := newEnv(t, envOpts{})
	e.api.set(serveFile(t, "quota_allowance.json"))
	e.step(t)
	got := e.samples(t)
	lbl := `{key_name="my-production-key",period="monthly"}`
	want := map[string]float64{
		"neuralwatt_key_allowance_limit_usd" + lbl:               10,
		"neuralwatt_key_allowance_spent_usd" + lbl:               8.25,
		"neuralwatt_key_allowance_remaining_usd" + lbl:           1.75,
		"neuralwatt_key_allowance_blocked" + lbl:                 0,
		"neuralwatt_key_allowance_reset_timestamp_seconds" + lbl: unix("2026-05-01T00:00:00Z"),
		"neuralwatt_limits_overage_limit_usd":                    5,
	}
	for k, v := range want {
		if g, ok := got[k]; !ok || g != v {
			t.Errorf("%s = %v (present=%v), want %v", k, g, ok, v)
		}
	}
	if _, ok := got["neuralwatt_monthly_budget_usd"]; ok {
		t.Error("monthly budget must be absent when it is not configured")
	}
}

// subscription: null・key.allowance: null・overage_limit_usd: null では、対応する系列が出ない（0 にならない）。
// 未知のフィールド・legacy_credits_usd は無視される。
func TestNullsOmitSeries(t *testing.T) {
	e := newEnv(t, envOpts{})
	e.api.set(serveFile(t, "quota_nulls.json"))
	e.step(t)

	e.compareGolden(t, "metrics_nulls.golden")

	got := e.samples(t)
	for k := range got {
		for _, prefix := range []string{"neuralwatt_subscription_", "neuralwatt_key_allowance_", "neuralwatt_limits_overage"} {
			if strings.HasPrefix(k, prefix) {
				t.Errorf("series %s must be absent (null in the response)", k)
			}
		}
	}
	if got["neuralwatt_credits_remaining_usd"] != 32.6774 {
		t.Errorf("balance = %v", got["neuralwatt_credits_remaining_usd"])
	}
}

// サブスクリプション・上限の各値が null のときは、その値の系列だけが出ない。
func TestPerValueNullsOmitSeries(t *testing.T) {
	e := newEnv(t, envOpts{})
	e.api.set(serveFile(t, "quota_partial_nulls.json"))
	e.step(t)

	e.compareGolden(t, "metrics_partial_nulls.golden")

	got := e.samples(t)
	absent := []string{
		"neuralwatt_credits_total_usd",
		"neuralwatt_quota_snapshot_timestamp_seconds",
		`neuralwatt_usage_energy_kwh_total{window="current_month"}`,
		`neuralwatt_usage_cost_usd_total{window="lifetime"}`,
		"neuralwatt_subscription_kwh_included",
		"neuralwatt_subscription_kwh_used",
		"neuralwatt_subscription_kwh_remaining",
		"neuralwatt_subscription_in_overage",
		"neuralwatt_subscription_period_end_timestamp_seconds",
		`neuralwatt_key_allowance_spent_usd{key_name="",period="daily"}`,
		`neuralwatt_key_allowance_remaining_usd{key_name="",period="daily"}`,
		`neuralwatt_key_allowance_reset_timestamp_seconds{key_name="",period="daily"}`,
	}
	for _, k := range absent {
		if v, ok := got[k]; ok {
			t.Errorf("series %s must be absent, got %v", k, v)
		}
	}
	present := map[string]float64{
		`neuralwatt_subscription_info{billing_interval="",plan="standard",status="trialing"}`: 1,
		`neuralwatt_key_allowance_limit_usd{key_name="",period="daily"}`:                      10,
		`neuralwatt_key_allowance_blocked{key_name="",period="daily"}`:                        1,
		`neuralwatt_usage_cost_usd_total{window="current_month"}`:                             160.1463,
	}
	for k, v := range present {
		if g, ok := got[k]; !ok || g != v {
			t.Errorf("%s = %v (present=%v), want %v", k, g, ok, v)
		}
	}
}

// 一度も成功していない間は last_success_timestamp は 0、成功後はその時刻。
func TestLastSuccessTimestamp(t *testing.T) {
	e := newEnv(t, envOpts{})
	e.api.set(serveStatus(500, nil, ""))
	if got := e.samples(t); got["neuralwatt_exporter_last_success_timestamp_seconds"] != 0 {
		t.Errorf("before the first fetch = %v, want 0", got["neuralwatt_exporter_last_success_timestamp_seconds"])
	}
	if _, ok := e.samples(t)["neuralwatt_exporter_fetch_success"]; ok {
		t.Error("fetch_success must be absent before the first fetch finishes")
	}
	e.step(t)
	if got := e.samples(t); got["neuralwatt_exporter_last_success_timestamp_seconds"] != 0 {
		t.Errorf("after a failure = %v, want 0", got["neuralwatt_exporter_last_success_timestamp_seconds"])
	}

	e.api.set(serveFile(t, "quota_example.json"))
	e.clock.Advance(time.Hour)
	e.step(t)
	// 1 回目の失敗で 500ms、成功の取得の開始から終了までで 250ms 進む。
	want := unixSeconds(baseTime.Add(time.Hour + 750*time.Millisecond))
	got := e.samples(t)["neuralwatt_exporter_last_success_timestamp_seconds"]
	if got != want {
		t.Errorf("after success = %v, want %v", got, want)
	}

	e.api.set(serveStatus(500, nil, ""))
	e.clock.Advance(time.Hour)
	e.step(t)
	if got := e.samples(t)["neuralwatt_exporter_last_success_timestamp_seconds"]; got != want {
		t.Errorf("a later failure changed the timestamp: %v, want %v", got, want)
	}
}

// 一度も成功していない間は、データ系列を出さない（exporter 自身の指標だけ）。
func TestNoDataBeforeFirstSuccess(t *testing.T) {
	e := newEnv(t, envOpts{budget: ptr(20.0)})
	e.api.set(serveStatus(500, nil, ""))
	e.step(t)
	e.compareGolden(t, "metrics_never_succeeded.golden")
	if data := dataSamples(e.samples(t)); len(data) != 0 {
		t.Errorf("data series exposed without a success: %v", data)
	}
}

// process_start_time_seconds（起動時刻）と Go の指標が /metrics に出る。
func TestProcessStartTime(t *testing.T) {
	e := newEnv(t, envOpts{})
	body := e.metricsBody(t)
	var found bool
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "process_start_time_seconds ") {
			found = true
		}
	}
	if !found {
		t.Fatalf("process_start_time_seconds is not exposed:\n%s", body)
	}
	families, err := e.app.registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range families {
		if mf.GetName() == "process_start_time_seconds" {
			v := mf.GetMetric()[0].GetGauge().GetValue()
			if v <= 1.6e9 || v > float64(time.Now().Unix())+1 {
				t.Errorf("process_start_time_seconds = %v is not a plausible start time", v)
			}
		}
	}
	if !strings.Contains(body, "go_goroutines ") {
		t.Error("go collector is not registered")
	}
}

// 全指標（key.allowance・overage_limit・サブスクリプションあり、予算あり）の golden。
// ダッシュボードの expr に出る名前の集合は、この golden の名前の集合に含まれなければならない（dashboard_test.go）。
func TestFullMetricsGolden(t *testing.T) {
	e := newEnv(t, envOpts{budget: ptr(20.0)})
	e.api.set(serveFile(t, "quota_allowance.json"))
	e.step(t)
	e.compareGolden(t, "metrics.golden")
}
