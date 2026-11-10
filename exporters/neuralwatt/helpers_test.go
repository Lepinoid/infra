package main

import (
	"bytes"
	"flag"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
)

var update = flag.Bool("update", false, "rewrite testdata/*.golden from the current output (review the diff before committing)")

// testKey は識別できるテスト用の値。ログ・/metrics・エラー文に現れてはならない。
const testKey = "sk-test-NEEDLE-9f3b7c1d52e84a60"

// contract の表にある、exporter が出しうる指標名（process_* と go_* を除く）。
var contractMetricNames = []string{
	"neuralwatt_exporter_fetch_success",
	"neuralwatt_exporter_last_success_timestamp_seconds",
	"neuralwatt_exporter_fetch_duration_seconds",
	"neuralwatt_exporter_fetches_total",
	"neuralwatt_quota_snapshot_timestamp_seconds",
	"neuralwatt_credits_remaining_usd",
	"neuralwatt_credits_total_usd",
	"neuralwatt_account_info",
	"neuralwatt_usage_cost_usd_total",
	"neuralwatt_usage_requests_total",
	"neuralwatt_usage_tokens_total",
	"neuralwatt_usage_energy_kwh_total",
	"neuralwatt_limits_overage_limit_usd",
	"neuralwatt_subscription_info",
	"neuralwatt_subscription_kwh_included",
	"neuralwatt_subscription_kwh_used",
	"neuralwatt_subscription_kwh_remaining",
	"neuralwatt_subscription_in_overage",
	"neuralwatt_subscription_period_end_timestamp_seconds",
	"neuralwatt_key_allowance_limit_usd",
	"neuralwatt_key_allowance_spent_usd",
	"neuralwatt_key_allowance_remaining_usd",
	"neuralwatt_key_allowance_blocked",
	"neuralwatt_key_allowance_reset_timestamp_seconds",
	"neuralwatt_monthly_budget_usd",
}

// alwaysPresent は一度も成功していなくても出る系列の名前。
var alwaysPresent = map[string]bool{
	"neuralwatt_exporter_fetch_success":                  true,
	"neuralwatt_exporter_last_success_timestamp_seconds": true,
	"neuralwatt_exporter_fetch_duration_seconds":         true,
	"neuralwatt_exporter_fetches_total":                  true,
	"neuralwatt_monthly_budget_usd":                      true,
}

var baseTime = time.Date(2026, 4, 16, 18, 31, 0, 0, time.UTC)

// fakeClock は時刻と After を手で進める。Now() は呼ばれるたびに auto だけ進む（取得の所要時間を決めるため）。
type fakeClock struct {
	mu         sync.Mutex
	now        time.Time
	auto       time.Duration
	timers     []*fakeTimer
	waits      []time.Duration
	registered chan struct{}
}

type fakeTimer struct {
	deadline time.Time
	ch       chan time.Time
	fired    bool
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: baseTime, registered: make(chan struct{}, 64)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.now
	c.now = c.now.Add(c.auto)
	return t
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	t := &fakeTimer{deadline: c.now.Add(d), ch: make(chan time.Time, 1)}
	c.timers = append(c.timers, t)
	c.waits = append(c.waits, d)
	c.mu.Unlock()
	c.registered <- struct{}{}
	return t.ch
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	for _, t := range c.timers {
		if !t.fired && !t.deadline.After(c.now) {
			t.fired = true
			t.ch <- c.now
		}
	}
}

func (c *fakeClock) lastWait() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.waits[len(c.waits)-1]
}

func (c *fakeClock) awaitTimer(t *testing.T) {
	t.Helper()
	select {
	case <-c.registered:
	case <-time.After(5 * time.Second):
		t.Fatal("poller did not wait on the clock")
	}
}

// fakeAPI は偽の Neuralwatt。handler を差し替えて応答を変える。
type fakeAPI struct {
	srv  *httptest.Server
	mu   sync.RWMutex
	h    http.HandlerFunc
	hits atomic.Int64
	// auths は受け取った Authorization ヘッダ。
	auths []string
}

func newFakeAPI(t *testing.T) *fakeAPI {
	t.Helper()
	f := &fakeAPI{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		f.mu.Lock()
		f.auths = append(f.auths, r.Header.Get("Authorization"))
		h := f.h
		f.mu.Unlock()
		if r.URL.Path != "/v1/quota" || r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		h(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPI) set(h http.HandlerFunc) {
	f.mu.Lock()
	f.h = h
	f.mu.Unlock()
}

func (f *fakeAPI) authHeaders() []string {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return append([]string(nil), f.auths...)
}

func serveFile(t *testing.T, name string) http.HandlerFunc {
	t.Helper()
	body := mustRead(t, name)
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}
}

func mustRead(t *testing.T, name string) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func serveStatus(code int, headers map[string]string, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}
}

type testEnv struct {
	api     *fakeAPI
	app     *app
	clock   *fakeClock
	logs    *syncWriter
	keyPath string
	cfg     config
}

type envOpts struct {
	interval time.Duration
	timeout  time.Duration
	budget   *float64
	key      *string // nil なら testKey を書く。空文字列も書く。
	noKey    bool    // true ならキーのファイルを作らない
}

func newEnv(t *testing.T, o envOpts) *testEnv {
	t.Helper()
	api := newFakeAPI(t)
	keyPath := filepath.Join(t.TempDir(), "api-key")
	if !o.noKey {
		content := testKey + "\n"
		if o.key != nil {
			content = *o.key
		}
		if err := os.WriteFile(keyPath, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := config{
		APIKeyFile:       keyPath,
		BaseURL:          api.srv.URL,
		PollInterval:     time.Minute,
		MonthlyBudgetUSD: o.budget,
		ListenAddress:    "127.0.0.1:0",
		RequestTimeout:   5 * time.Second,
	}
	if o.interval != 0 {
		cfg.PollInterval = o.interval
	}
	if o.timeout != 0 {
		cfg.RequestTimeout = o.timeout
	}
	clk := newFakeClock()
	clk.auto = 250 * time.Millisecond
	logs := &syncWriter{}
	log := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	a, err := newApp(cfg, log, clk)
	if err != nil {
		t.Fatal(err)
	}
	a.poller.jitter = func() float64 { return 0 }
	return &testEnv{api: api, app: a, clock: clk, logs: logs, keyPath: keyPath, cfg: cfg}
}

type syncWriter struct {
	mu sync.Mutex
	w  bytes.Buffer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

func (s *syncWriter) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.String()
}

func (e *testEnv) logText() string {
	return e.logs.String()
}

func (e *testEnv) writeKey(t *testing.T, content string) {
	t.Helper()
	if err := os.WriteFile(e.keyPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// metricsBody は /metrics の本文（process と Go の collector を含む全体）。
func (e *testEnv) metricsBody(t *testing.T) string {
	t.Helper()
	rec := httptest.NewRecorder()
	e.app.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics returned %d", rec.Code)
	}
	return rec.Body.String()
}

// samples は neuralwatt_* の全系列を `name{label="v"}` → 値 の map にする。
func (e *testEnv) samples(t *testing.T) map[string]float64 {
	t.Helper()
	families, err := e.app.registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]float64{}
	for _, mf := range families {
		if !strings.HasPrefix(mf.GetName(), "neuralwatt_") {
			continue
		}
		for _, m := range mf.GetMetric() {
			out[seriesKey(mf.GetName(), m)] = metricValue(mf.GetType(), m)
		}
	}
	return out
}

func metricValue(typ dto.MetricType, m *dto.Metric) float64 {
	if typ == dto.MetricType_COUNTER {
		return m.GetCounter().GetValue()
	}
	return m.GetGauge().GetValue()
}

func seriesKey(name string, m *dto.Metric) string {
	var parts []string
	for _, l := range m.GetLabel() {
		parts = append(parts, l.GetName()+`="`+l.GetValue()+`"`)
	}
	sort.Strings(parts)
	if len(parts) == 0 {
		return name
	}
	return name + "{" + strings.Join(parts, ",") + "}"
}

// dataSamples は exporter 自身の指標と予算を除いた、Neuralwatt の値の系列だけ。
func dataSamples(all map[string]float64) map[string]float64 {
	out := map[string]float64{}
	for k, v := range all {
		name, _, _ := strings.Cut(k, "{")
		if !alwaysPresent[name] {
			out[k] = v
		}
	}
	return out
}

func (e *testEnv) step(t *testing.T) time.Duration {
	t.Helper()
	return e.app.poller.step(t.Context())
}

// compareGolden は neuralwatt_* の指標を testdata/<name> と比べる（-update で書き換える）。
func (e *testEnv) compareGolden(t *testing.T, name string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		families, err := e.app.registry.Gather()
		if err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		enc := expfmt.NewEncoder(&buf, expfmt.NewFormat(expfmt.TypeTextPlain))
		for _, mf := range families {
			if strings.HasPrefix(mf.GetName(), "neuralwatt_") {
				if err := enc.Encode(mf); err != nil {
					t.Fatal(err)
				}
			}
		}
		if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	golden, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer golden.Close()
	if err := testutil.GatherAndCompare(e.app.registry, golden, contractMetricNames...); err != nil {
		t.Fatalf("metrics differ from %s:\n%v", name, err)
	}
	if problems, err := testutil.GatherAndLint(e.app.registry, contractMetricNames...); err != nil || len(problems) > 0 {
		t.Fatalf("promlint: err=%v problems=%v", err, problems)
	}
}

var _ prometheus.Collector = (*collector)(nil)
