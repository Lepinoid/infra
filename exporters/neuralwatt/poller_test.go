package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// 429 は Retry-After の秒数に従う（間隔より長ければその分待つ）。
func TestRetryAfterSeconds(t *testing.T) {
	e := newEnv(t, envOpts{interval: time.Minute})
	e.api.set(serveStatus(429, map[string]string{"Retry-After": "120"}, ""))
	if d := e.step(t); d != 120*time.Second {
		t.Errorf("delay = %v, want 120s", d)
	}
}

// Retry-After が間隔より短くても、間隔は守る。
func TestRetryAfterShorterThanInterval(t *testing.T) {
	e := newEnv(t, envOpts{interval: time.Minute})
	e.api.set(serveStatus(429, map[string]string{"Retry-After": "3"}, ""))
	if d := e.step(t); d != time.Minute {
		t.Errorf("delay = %v, want the 60s interval", d)
	}
}

// Retry-After が HTTP-date のとき。
func TestRetryAfterHTTPDate(t *testing.T) {
	e := newEnv(t, envOpts{interval: 10 * time.Second})
	// fetch の中の Now() は 2 回目の呼び出し（1 回目は step の開始）。auto=250ms なので、基準は baseTime+250ms。
	date := baseTime.Add(250*time.Millisecond + 90*time.Second).UTC().Format(http.TimeFormat)
	e.api.set(serveStatus(429, map[string]string{"Retry-After": date}, ""))
	d := e.step(t)
	if d < 88*time.Second || d > 90*time.Second {
		t.Errorf("delay = %v, want about 90s", d)
	}
}

// 有効な Retry-After は短縮しない（24 時間・1 年・Duration で表せる上限付近まで）。
func TestRetryAfterIsNeverShortened(t *testing.T) {
	cases := []struct {
		header string
		want   time.Duration
	}{
		{"86400", 24 * time.Hour},
		{"86401", 24*time.Hour + time.Second},
		{"172800", 48 * time.Hour},
		{"31536000", 365 * 24 * time.Hour},
		{"9223372036", 9223372036 * time.Second},
	}
	for _, tc := range cases {
		e := newEnv(t, envOpts{interval: time.Minute})
		e.api.set(serveStatus(429, map[string]string{"Retry-After": tc.header}, ""))
		if d := e.step(t); d != tc.want {
			t.Errorf("Retry-After %s: delay = %v, want %v", tc.header, d, tc.want)
		}
	}
	// Duration で表せない値は、負にならず最大の待ち時間になる。
	for _, h := range []string{"9223372037", "99999999999999999999"} {
		got := parseRetryAfter(h, baseTime)
		if got == nil || *got != maxDuration {
			t.Errorf("Retry-After %s: got %v, want maxDuration", h, got)
		}
	}
	// HTTP-date も 24 時間を超えて従う。
	now := baseTime
	date := now.Add(72 * time.Hour).UTC().Format(http.TimeFormat)
	if got := parseRetryAfter(date, now); got == nil || *got != 72*time.Hour {
		t.Errorf("Retry-After %q: got %v, want 72h", date, got)
	}
}

// Retry-After が読めない・負のときは指数バックオフ（間隔を下回らない）。
func TestRetryAfterInvalidFallsBackToBackoff(t *testing.T) {
	for _, v := range []string{"soon", "-5", ""} {
		e := newEnv(t, envOpts{interval: time.Minute})
		e.api.set(serveStatus(429, map[string]string{"Retry-After": v}, ""))
		if d := e.step(t); d < time.Minute || d >= 2*time.Minute {
			t.Errorf("Retry-After %q: delay = %v, want within [60s, 120s)", v, d)
		}
	}
}

// Retry-After がない 429 は指数バックオフ。連続で伸び、上限は 10 分。成功で戻る。
func TestBackoffGrowsAndIsCapped(t *testing.T) {
	e := newEnv(t, envOpts{interval: time.Minute})
	e.api.set(serveStatus(429, nil, ""))
	var delays []time.Duration
	for range 8 {
		delays = append(delays, e.step(t))
	}
	// jitter=0 のとき、バックオフ b（2 倍ずつ、上限 10 分）の下半分になる。
	want := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 5 * time.Minute, 5 * time.Minute, 5 * time.Minute, 5 * time.Minute, 5 * time.Minute}
	for i := range want {
		if delays[i] != want[i] {
			t.Errorf("delay[%d] = %v, want %v", i, delays[i], want[i])
		}
	}
	// ジッターが最大でも上限（10 分）を超えない。
	e.app.poller.jitter = func() float64 { return 0.999999 }
	if d := e.step(t); d > maxBackoff {
		t.Errorf("delay %v exceeds the %v cap", d, maxBackoff)
	}
	e.api.set(serveFile(t, "quota_example.json"))
	if d := e.step(t); d != time.Minute {
		t.Errorf("after a success the delay is %v, want the interval", d)
	}
	e.app.poller.jitter = func() float64 { return 0 }
	e.api.set(serveStatus(429, nil, ""))
	if d := e.step(t); d != time.Minute {
		t.Errorf("the backoff did not reset after a success: %v", d)
	}
}

// 429 のあと、Retry-After の間は再取得しない（偽の時計で確かめる）。
func TestNoRefetchDuringRetryAfter(t *testing.T) {
	e := newEnv(t, envOpts{interval: time.Minute})
	e.api.set(serveStatus(429, map[string]string{"Retry-After": "300"}, ""))
	ctx := t.Context()
	done := make(chan struct{})
	go func() { defer close(done); e.app.poller.run(ctx) }()
	t.Cleanup(func() { <-done })

	e.clock.awaitTimer(t)
	if n := e.api.hits.Load(); n != 1 {
		t.Fatalf("hits = %d after start, want 1", n)
	}
	if w := e.clock.lastWait(); w != 5*time.Minute {
		t.Fatalf("waiting %v, want 5m (Retry-After)", w)
	}
	// 通常の間隔（1 分）を何度過ぎても取得しない。
	for range 4 {
		e.clock.Advance(time.Minute)
	}
	time.Sleep(100 * time.Millisecond)
	if n := e.api.hits.Load(); n != 1 {
		t.Fatalf("refetched during Retry-After: hits = %d", n)
	}
	// 成功に切り替えて 5 分に達すると再取得する。
	e.api.set(serveFile(t, "quota_example.json"))
	e.clock.Advance(time.Minute)
	e.clock.awaitTimer(t)
	if n := e.api.hits.Load(); n != 2 {
		t.Fatalf("hits = %d after Retry-After elapsed, want 2", n)
	}
	if e.samples(t)["neuralwatt_exporter_fetch_success"] != 1 {
		t.Error("the refetch did not succeed")
	}
}

// 取得は設定した間隔ごとに 1 回で、/metrics へのアクセスの回数に影響されない。
func TestFetchRateIndependentOfScrapes(t *testing.T) {
	e := newEnv(t, envOpts{interval: time.Minute})
	e.api.set(serveFile(t, "quota_example.json"))
	ctx := t.Context()
	done := make(chan struct{})
	go func() { defer close(done); e.app.poller.run(ctx) }()
	t.Cleanup(func() { <-done })

	e.clock.awaitTimer(t)
	scrape := func(n int) {
		for range n {
			rec := httptest.NewRecorder()
			e.app.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "neuralwatt_credits_remaining_usd") {
				t.Fatalf("scrape failed: %d", rec.Code)
			}
		}
	}
	scrape(200)
	if n := e.api.hits.Load(); n != 1 {
		t.Fatalf("hits = %d after 200 scrapes, want 1", n)
	}
	e.clock.Advance(59 * time.Second)
	scrape(200)
	time.Sleep(50 * time.Millisecond)
	if n := e.api.hits.Load(); n != 1 {
		t.Fatalf("hits = %d before the interval elapsed, want 1", n)
	}
	e.clock.Advance(time.Second)
	e.clock.awaitTimer(t)
	if n := e.api.hits.Load(); n != 2 {
		t.Fatalf("hits = %d after one interval, want 2", n)
	}
	scrape(200)
	if n := e.api.hits.Load(); n != 2 {
		t.Fatalf("hits = %d after more scrapes, want 2", n)
	}
}

// 同時に飛ぶ取得は 1 本だけ（step は直列に動く）。
func TestOnlyOneRequestInFlight(t *testing.T) {
	e := newEnv(t, envOpts{interval: time.Minute})
	var mu sync.Mutex
	current, peak := 0, 0
	e.api.set(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		current++
		peak = max(peak, current)
		mu.Unlock()
		serveFile(t, "quota_example.json")(w, r)
		mu.Lock()
		current--
		mu.Unlock()
	})
	ctx := t.Context()
	done := make(chan struct{})
	go func() { defer close(done); e.app.poller.run(ctx) }()
	t.Cleanup(func() { <-done })
	for range 3 {
		e.clock.awaitTimer(t)
		e.clock.Advance(time.Minute)
	}
	e.clock.awaitTimer(t)
	mu.Lock()
	defer mu.Unlock()
	if peak != 1 || e.api.hits.Load() != 4 {
		t.Errorf("peak in-flight requests = %d, hits = %d; want 1 and 4", peak, e.api.hits.Load())
	}
}

// ログは状態（成功↔失敗）と結果の分類が変わったときだけ出る。
func TestLogsOnlyOnTransitions(t *testing.T) {
	e := newEnv(t, envOpts{})
	e.api.set(serveStatus(500, nil, ""))
	for range 5 {
		e.step(t)
	}
	e.api.set(serveStatus(401, nil, ""))
	for range 3 {
		e.step(t)
	}
	e.api.set(serveFile(t, "quota_example.json"))
	for range 4 {
		e.step(t)
	}
	lines := strings.Split(strings.TrimSpace(e.logText()), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d log lines, want 3:\n%s", len(lines), e.logText())
	}
	for i, want := range []string{`"result":"server_error"`, `"result":"unauthorized"`, `"result":"ok"`} {
		if !strings.Contains(lines[i], want) {
			t.Errorf("line %d = %s, want %s", i, lines[i], want)
		}
	}
}
