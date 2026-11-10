package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

type failureCase struct {
	name   string
	result string
	// serve が nil でなければ、API の応答をこれにする。
	serve func(t *testing.T, e *testEnv, other *fakeAPI) http.HandlerFunc
	// breakKey が nil でなければ、取得の前にキーのファイルをこの状態にする。
	breakKey func(t *testing.T, e *testEnv)
	timeout  time.Duration
}

func failureCases() []failureCase {
	status := func(code int, headers map[string]string, body string) func(*testing.T, *testEnv, *fakeAPI) http.HandlerFunc {
		return func(*testing.T, *testEnv, *fakeAPI) http.HandlerFunc { return serveStatus(code, headers, body) }
	}
	file := func(name string) func(*testing.T, *testEnv, *fakeAPI) http.HandlerFunc {
		return func(t *testing.T, _ *testEnv, _ *fakeAPI) http.HandlerFunc { return serveFile(t, name) }
	}
	return []failureCase{
		{name: "401", result: resultUnauthorized, serve: status(401, nil, `{"error":{"type":"auth","message":"bad key"}}`)},
		{name: "402", result: resultPaymentRequired, serve: status(402, nil, `{"error":{"code":"allowance_exceeded"}}`)},
		{name: "429 with Retry-After", result: resultRateLimited, serve: status(429, map[string]string{"Retry-After": "30"}, "")},
		{name: "429 without Retry-After", result: resultRateLimited, serve: status(429, nil, "")},
		{name: "500", result: resultServerError, serve: status(500, nil, "boom")},
		{name: "503", result: resultServerError, serve: status(503, nil, "")},
		{name: "404 (other 4xx)", result: resultClientError, serve: status(404, nil, "")},
		{name: "timeout", result: resultNetworkError, timeout: 150 * time.Millisecond,
			serve: func(*testing.T, *testEnv, *fakeAPI) http.HandlerFunc {
				return func(w http.ResponseWriter, r *http.Request) {
					select {
					case <-r.Context().Done():
					case <-time.After(5 * time.Second):
					}
				}
			}},
		{name: "invalid JSON", result: resultInvalidResponse, serve: status(200, nil, `{"balance": {`)},
		{name: "not JSON", result: resultInvalidResponse, serve: status(200, nil, `<html>maintenance</html>`)},
		{name: "wrong type", result: resultInvalidResponse, serve: status(200, nil, `{"balance":{"credits_remaining_usd":"lots"}}`)},
		{name: "credits_remaining_usd missing", result: resultInvalidResponse, serve: file("quota_missing_remaining.json")},
		{name: "credits_remaining_usd null", result: resultInvalidResponse, serve: file("quota_null_remaining.json")},
		{name: "balance missing", result: resultInvalidResponse, serve: status(200, nil, `{}`)},
		{name: "unexpected 204", result: resultInvalidResponse, serve: status(204, nil, "")},
		{name: "response over 1 MiB", result: resultInvalidResponse,
			serve: func(*testing.T, *testEnv, *fakeAPI) http.HandlerFunc {
				return func(w http.ResponseWriter, _ *http.Request) {
					_, _ = w.Write([]byte(`{"balance":{"credits_remaining_usd":1},"pad":"`))
					_, _ = w.Write([]byte(strings.Repeat("x", maxResponseBytes)))
					_, _ = w.Write([]byte(`"}`))
				}
			}},
		{name: "redirect 302", result: resultInvalidResponse,
			serve: func(_ *testing.T, _ *testEnv, other *fakeAPI) http.HandlerFunc {
				return func(w http.ResponseWriter, r *http.Request) {
					http.Redirect(w, r, other.srv.URL+"/v1/quota", http.StatusFound)
				}
			}},
		{name: "redirect 307", result: resultInvalidResponse,
			serve: func(_ *testing.T, _ *testEnv, other *fakeAPI) http.HandlerFunc {
				return func(w http.ResponseWriter, r *http.Request) {
					http.Redirect(w, r, other.srv.URL+"/v1/quota", http.StatusTemporaryRedirect)
				}
			}},
		{name: "redirect 302 whose body is a valid quota response", result: resultInvalidResponse,
			serve: func(t *testing.T, _ *testEnv, other *fakeAPI) http.HandlerFunc {
				body := mustRead(t, "quota_example.json")
				return func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Location", other.srv.URL+"/v1/quota")
					w.WriteHeader(http.StatusFound)
					_, _ = w.Write(body)
				}
			}},
		{name: "201 whose body is a valid quota response", result: resultInvalidResponse,
			serve: func(t *testing.T, _ *testEnv, _ *fakeAPI) http.HandlerFunc {
				body := mustRead(t, "quota_example.json")
				return func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(http.StatusCreated)
					_, _ = w.Write(body)
				}
			}},
		{name: "response over 1 MiB (valid JSON followed by whitespace)", result: resultInvalidResponse,
			serve: func(t *testing.T, _ *testEnv, _ *fakeAPI) http.HandlerFunc {
				body := mustRead(t, "quota_example.json")
				return func(w http.ResponseWriter, _ *http.Request) {
					_, _ = w.Write(body)
					_, _ = w.Write([]byte(strings.Repeat(" ", maxResponseBytes)))
				}
			}},
		{name: "key file missing", result: resultCredentialError,
			breakKey: func(t *testing.T, e *testEnv) {
				if err := os.Remove(e.keyPath); err != nil {
					t.Fatal(err)
				}
			}},
		{name: "key file empty", result: resultCredentialError,
			breakKey: func(t *testing.T, e *testEnv) { e.writeKey(t, "") }},
		{name: "key file whitespace only", result: resultCredentialError,
			breakKey: func(t *testing.T, e *testEnv) { e.writeKey(t, " \n\t\n") }},
		{name: "key file with control characters", result: resultCredentialError,
			breakKey: func(t *testing.T, e *testEnv) { e.writeKey(t, "sk-a\x00b\r\nX-Injected: 1") }},
		{name: "key file is a directory", result: resultCredentialError,
			breakKey: func(t *testing.T, e *testEnv) {
				_ = os.Remove(e.keyPath)
				if err := os.Mkdir(e.keyPath, 0o700); err != nil {
					t.Fatal(err)
				}
			}},
		{name: "connection refused", result: resultNetworkError,
			serve: func(_ *testing.T, e *testEnv, _ *fakeAPI) http.HandlerFunc {
				e.api.srv.Close()
				return nil
			}},
	}
}

// 各失敗で: fetch_success が 0、該当 result の fetches_total が増える、
// 直前の成功の値が残る、一度も成功していなければデータ系列が出ない。
func TestFailureHandling(t *testing.T) {
	for _, tc := range failureCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Run("never succeeded", func(t *testing.T) {
				e, other := setupFailure(t, tc)
				e.step(t)
				got := e.samples(t)
				if got["neuralwatt_exporter_fetch_success"] != 0 {
					t.Errorf("fetch_success = %v, want 0", got["neuralwatt_exporter_fetch_success"])
				}
				if _, ok := got["neuralwatt_exporter_fetch_success"]; !ok {
					t.Error("fetch_success must be present (0) after a failed fetch")
				}
				assertCounts(t, got, map[string]float64{tc.result: 1})
				if got["neuralwatt_exporter_last_success_timestamp_seconds"] != 0 {
					t.Errorf("last_success_timestamp = %v, want 0", got["neuralwatt_exporter_last_success_timestamp_seconds"])
				}
				if data := dataSamples(got); len(data) != 0 {
					t.Errorf("data series must be absent, got %v", data)
				}
				if other != nil && other.hits.Load() != 0 {
					t.Errorf("the redirect was followed: the other host got %d requests", other.hits.Load())
				}
			})

			t.Run("after a success", func(t *testing.T) {
				e := newEnv(t, envOpts{timeout: tc.timeout})
				var other *fakeAPI
				if strings.HasPrefix(tc.name, "redirect") {
					other = newFakeAPI(t)
					other.set(serveFile(t, "quota_example.json"))
				}
				// 先に正常な API とキーで成功させ、そのあとで失敗の条件にする。
				e.api.set(serveFile(t, "quota_example.json"))
				e.step(t)
				before := e.samples(t)
				if before["neuralwatt_exporter_fetch_success"] != 1 {
					t.Fatalf("the setup fetch failed: %v", before["neuralwatt_exporter_fetch_success"])
				}
				if tc.serve != nil {
					if h := tc.serve(t, e, other); h != nil {
						e.api.set(h)
					}
				}
				if tc.breakKey != nil {
					tc.breakKey(t, e)
				}
				e.clock.Advance(time.Minute)
				e.step(t)
				after := e.samples(t)

				if after["neuralwatt_exporter_fetch_success"] != 0 {
					t.Errorf("fetch_success = %v, want 0", after["neuralwatt_exporter_fetch_success"])
				}
				assertCounts(t, after, map[string]float64{resultOK: 1, tc.result: 1})
				wantData, gotData := dataSamples(before), dataSamples(after)
				if len(gotData) != len(wantData) {
					t.Errorf("data series changed: before %d, after %d", len(wantData), len(gotData))
				}
				for k, v := range wantData {
					if g, ok := gotData[k]; !ok || g != v {
						t.Errorf("%s = %v (present=%v), want the earlier %v", k, g, ok, v)
					}
				}
				if after["neuralwatt_exporter_last_success_timestamp_seconds"] != before["neuralwatt_exporter_last_success_timestamp_seconds"] {
					t.Error("last_success_timestamp changed by a failure")
				}
				if after["neuralwatt_credits_remaining_usd"] != 32.6774 {
					t.Errorf("balance = %v, want the last good 32.6774", after["neuralwatt_credits_remaining_usd"])
				}
			})
		})
	}
}

func setupFailure(t *testing.T, tc failureCase) (*testEnv, *fakeAPI) {
	t.Helper()
	e := newEnv(t, envOpts{timeout: tc.timeout})
	var other *fakeAPI
	if strings.HasPrefix(tc.name, "redirect") {
		other = newFakeAPI(t)
		other.set(serveFile(t, "quota_example.json"))
	}
	if tc.serve != nil {
		if h := tc.serve(t, e, other); h != nil {
			e.api.set(h)
		}
	} else {
		e.api.set(serveFile(t, "quota_example.json"))
	}
	if tc.breakKey != nil {
		tc.breakKey(t, e)
	}
	return e, other
}

// assertCounts は fetches_total が want のとおり（書かれていない result は 0）。
func assertCounts(t *testing.T, got map[string]float64, want map[string]float64) {
	t.Helper()
	for _, r := range allResults {
		key := `neuralwatt_exporter_fetches_total{result="` + r + `"}`
		g, ok := got[key]
		if !ok {
			t.Errorf("%s is missing (it must be initialised to 0)", key)
			continue
		}
		if g != want[r] {
			t.Errorf("%s = %v, want %v", key, g, want[r])
		}
	}
}

// 失敗のあとに成功すると fetch_success が 1 に戻り、値が新しくなる。
func TestRecoveryAfterFailure(t *testing.T) {
	e := newEnv(t, envOpts{})
	e.api.set(serveStatus(500, nil, ""))
	e.step(t)
	e.api.set(serveFile(t, "quota_example.json"))
	e.step(t)
	got := e.samples(t)
	if got["neuralwatt_exporter_fetch_success"] != 1 || got["neuralwatt_credits_remaining_usd"] != 32.6774 {
		t.Errorf("not recovered: %v", got)
	}
	assertCounts(t, got, map[string]float64{resultOK: 1, resultServerError: 1})
}

// 成功のたびにスナップショットが差し替わる（古い値が残らない）。
func TestSnapshotReplacedOnEachSuccess(t *testing.T) {
	e := newEnv(t, envOpts{})
	e.api.set(serveFile(t, "quota_allowance.json"))
	e.step(t)
	if _, ok := e.samples(t)[`neuralwatt_key_allowance_limit_usd{key_name="my-production-key",period="monthly"}`]; !ok {
		t.Fatal("allowance series missing after the first success")
	}
	e.api.set(serveFile(t, "quota_nulls.json"))
	e.step(t)
	for k := range e.samples(t) {
		if strings.HasPrefix(k, "neuralwatt_key_allowance_") {
			t.Errorf("stale series %s survived a newer snapshot without an allowance", k)
		}
	}
}

// キーのファイルは取得のたびに読み直す（回転に再起動なしで追従する）。前後の空白は除く。
func TestKeyFileIsReread(t *testing.T) {
	e := newEnv(t, envOpts{})
	e.api.set(serveFile(t, "quota_example.json"))
	e.writeKey(t, "  sk-test-first \n")
	e.step(t)
	e.writeKey(t, "\n\tsk-test-second\n\n")
	e.step(t)
	got := e.api.authHeaders()
	if len(got) != 2 || got[0] != "Bearer sk-test-first" || got[1] != "Bearer sk-test-second" {
		t.Errorf("Authorization headers = %q", got)
	}
}

// 資格情報の失敗（credential_error）では API を呼ばない。
func TestNoRequestWithoutKey(t *testing.T) {
	e := newEnv(t, envOpts{key: ptr("")})
	e.api.set(serveFile(t, "quota_example.json"))
	e.step(t)
	if n := e.api.hits.Load(); n != 0 {
		t.Errorf("the API was called %d times without a key", n)
	}
}

// 失敗してもプロセスは生きている: /healthz は 200 のまま。
func TestHealthzStaysUpOnAPIFailure(t *testing.T) {
	e := newEnv(t, envOpts{})
	e.api.set(serveStatus(401, nil, ""))
	e.step(t)
	for _, path := range []string{"/healthz", "/healthz"} {
		rec := httptest.NewRecorder()
		e.app.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s = %d", path, rec.Code)
		}
	}
	if e.samples(t)["neuralwatt_exporter_fetch_success"] != 0 {
		t.Fatal("setup: the fetch should have failed")
	}
}
