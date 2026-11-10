package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// テスト用のキーの値が、ログ・/metrics・エラー文のどこにも現れない。
// 偽の API がキー（Authorization ヘッダ）を応答本文・ヘッダ・Location にそのまま返す場合も含める。
func TestKeyNeverLeaks(t *testing.T) {
	echo := func(code int, mk func(auth string) (map[string]string, string)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			auth := r.Header.Get("Authorization")
			headers, body := mk(auth)
			for k, v := range headers {
				w.Header().Set(k, v)
			}
			w.WriteHeader(code)
			_, _ = w.Write([]byte(body))
		}
	}
	example, err := os.ReadFile(filepath.Join("testdata", "quota_example.json"))
	if err != nil {
		t.Fatal(err)
	}
	withEcho := func(field string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			var m map[string]any
			if err := json.Unmarshal(example, &m); err != nil {
				t.Error(err)
			}
			m[field] = r.Header.Get("Authorization")
			w.Header().Set("X-Debug-Auth", r.Header.Get("Authorization"))
			_ = json.NewEncoder(w).Encode(m)
		}
	}
	cases := []struct {
		name   string
		handle http.HandlerFunc
	}{
		{"200 with the key in an unknown field", withEcho("debug_auth")},
		{"200 with the key in a name field", func(w http.ResponseWriter, r *http.Request) {
			var m map[string]any
			_ = json.Unmarshal(example, &m)
			m["key"] = map[string]any{"name": "ignored-" + r.Header.Get("Authorization"), "allowance": nil}
			_ = json.NewEncoder(w).Encode(m)
		}},
		{"401 with the key in the body", echo(401, func(a string) (map[string]string, string) {
			return nil, `{"error":{"message":"invalid key ` + a + `"}}`
		})},
		{"402 with the key in the body", echo(402, func(a string) (map[string]string, string) {
			return map[string]string{"X-Debug-Auth": a}, `{"error":{"message":"` + a + `"}}`
		})},
		{"429 with the key in the body", echo(429, func(a string) (map[string]string, string) {
			return map[string]string{"Retry-After": "30"}, a
		})},
		{"500 with the key in the body", echo(500, func(a string) (map[string]string, string) { return nil, "internal error: " + a })},
		{"invalid JSON containing the key", echo(200, func(a string) (map[string]string, string) { return nil, `{"balance": "` + a })},
		{"wrong type containing the key", echo(200, func(a string) (map[string]string, string) {
			return nil, `{"balance":{"credits_remaining_usd":"` + a + `"}}`
		})},
		{"redirect with the key in Location", echo(302, func(a string) (map[string]string, string) {
			return map[string]string{"Location": "https://redirect.invalid/quota?token=" + a}, a
		})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, envOpts{budget: ptr(20.0)})
			e.api.set(tc.handle)
			ferr := fetchOnce(t, e)
			e.step(t)

			auths := e.api.authHeaders()
			if len(auths) < 1 || auths[0] != "Bearer "+testKey {
				t.Fatalf("the API did not receive the key (test would be vacuous): %q", auths)
			}
			assertNoLeak(t, e, ferr)
		})
	}

	// 成功（200）の応答の文字列のうち指標のラベルになるものに、送信したキーが入っている場合は、応答を棄却する
	// （key.allowance が有効で、サブスクリプションもある応答。部分文字列として入っている場合を含む）。
	labelCases := []struct {
		name   string
		inject func(m map[string]any, key string)
	}{
		{"key.name equals the key", func(m map[string]any, key string) { m["key"].(map[string]any)["name"] = key }},
		{"key.name contains the key", func(m map[string]any, key string) { m["key"].(map[string]any)["name"] = "prod-" + key + "-1" }},
		{"key.allowance.period contains the key", func(m map[string]any, key string) {
			m["key"].(map[string]any)["allowance"].(map[string]any)["period"] = "monthly-" + key
		}},
		{"subscription.plan contains the key", func(m map[string]any, key string) { m["subscription"].(map[string]any)["plan"] = key }},
		{"subscription.status contains the key", func(m map[string]any, key string) { m["subscription"].(map[string]any)["status"] = "x" + key }},
		{"subscription.billing_interval contains the key", func(m map[string]any, key string) {
			m["subscription"].(map[string]any)["billing_interval"] = key + "x"
		}},
		{"balance.accounting_method contains the key", func(m map[string]any, key string) { m["balance"].(map[string]any)["accounting_method"] = key }},
		{"limits.rate_limit_tier contains the key", func(m map[string]any, key string) { m["limits"].(map[string]any)["rate_limit_tier"] = "t-" + key }},
	}
	allowanceExample := mustRead(t, "quota_allowance.json")
	for _, lc := range labelCases {
		t.Run("label: "+lc.name, func(t *testing.T) {
			e := newEnv(t, envOpts{budget: ptr(20.0)})
			e.api.set(func(w http.ResponseWriter, r *http.Request) {
				var m map[string]any
				if err := json.Unmarshal(allowanceExample, &m); err != nil {
					t.Error(err)
				}
				lc.inject(m, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
				_ = json.NewEncoder(w).Encode(m)
			})
			ferr := fetchOnce(t, e)
			if ferr == nil || ferr.result != resultInvalidResponse {
				t.Fatalf("a response with the key in a label must be rejected as invalid_response, got %v", ferr)
			}
			e.step(t)
			if got := e.samples(t); got["neuralwatt_exporter_fetch_success"] != 0 {
				t.Errorf("fetch_success = %v, want 0", got["neuralwatt_exporter_fetch_success"])
			}
			if data := dataSamples(e.samples(t)); len(data) != 0 {
				t.Errorf("data series from a rejected response: %v", data)
			}
			assertNoLeak(t, e, ferr)
		})
	}

	// 対照: 同じ応答でキーを含まなければ、ラベルは /metrics に出る（上の棄却が、ラベルを出す経路を通っていることの確認）。
	t.Run("label: control without the key", func(t *testing.T) {
		e := newEnv(t, envOpts{})
		e.api.set(serveFile(t, "quota_allowance.json"))
		e.step(t)
		if _, ok := e.samples(t)[`neuralwatt_key_allowance_limit_usd{key_name="my-production-key",period="monthly"}`]; !ok {
			t.Fatal("the allowance series with the key_name label is not exposed")
		}
	})

	// キーのファイルが壊れているとき（キーを含むが不正な文字がある）も、エラー文・ログに値を出さない。
	t.Run("malformed key file", func(t *testing.T) {
		e := newEnv(t, envOpts{key: ptr(testKey + "\x00\r\nX-Injected: 1")})
		e.api.set(serveFile(t, "quota_example.json"))
		ferr := fetchOnce(t, e)
		e.step(t)
		if ferr == nil || ferr.result != resultCredentialError {
			t.Fatalf("want credential_error, got %v", ferr)
		}
		if n := e.api.hits.Load(); n != 0 {
			t.Errorf("a malformed key was sent to the API (%d requests)", n)
		}
		assertNoLeak(t, e, ferr)
	})
}

func fetchOnce(t *testing.T, e *testEnv) *fetchError {
	t.Helper()
	_, ferr := e.app.poller.client.fetch(t.Context())
	return ferr
}

func assertNoLeak(t *testing.T, e *testEnv, ferr *fetchError) {
	t.Helper()
	logs, metrics := e.logText(), e.metricsBody(t)
	if strings.TrimSpace(logs) == "" {
		t.Error("no log output was captured (test would be vacuous)")
	}
	texts := map[string]string{"log": logs, "/metrics": metrics}
	if ferr != nil {
		texts["error text"] = ferr.Error()
	}
	for where, text := range texts {
		for _, needle := range []string{testKey, "NEEDLE", "sk-test", "Bearer"} {
			if strings.Contains(text, needle) {
				t.Errorf("%s contains %q:\n%s", where, needle, text)
			}
		}
	}
}
