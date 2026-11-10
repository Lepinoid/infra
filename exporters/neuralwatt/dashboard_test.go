package main

import (
	"encoding/json"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

const dashboardFile = "../../grafana/dashboard-neuralwatt.yaml"

var metricNameRE = regexp.MustCompile(`neuralwatt_[a-z_]+`)

// goldenMetricNames は testdata/metrics.golden（全指標）に出る指標名。
func goldenMetricNames(t *testing.T) map[string]bool {
	t.Helper()
	b, err := os.ReadFile("testdata/metrics.golden")
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "neuralwatt_") {
			names[metricNameRE.FindString(line)] = true
		}
	}
	return names
}

// dashboardJSON は ConfigMap の neuralwatt.json（YAML のリテラルブロック）を取り出す。
// YAML の依存を足さないため、ファイルの形（data.neuralwatt.json: | の下を 4 字下げ）に決め打ちで読む。
func dashboardJSON(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(dashboardFile)
	if err != nil {
		t.Fatal(err)
	}
	const marker = "  neuralwatt.json: |\n"
	_, rest, ok := strings.Cut(string(b), marker)
	if !ok {
		t.Fatalf("%s: %q not found", dashboardFile, marker)
	}
	var out []string
	for _, line := range strings.Split(rest, "\n") {
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "    ") {
			t.Fatalf("%s: the block is not indented by 4 spaces: %q", dashboardFile, line)
		}
		out = append(out, line[4:])
	}
	return []byte(strings.Join(out, "\n"))
}

func collectExprs(v any, out *[]string) {
	switch x := v.(type) {
	case map[string]any:
		for k, vv := range x {
			if s, ok := vv.(string); ok && k == "expr" {
				*out = append(*out, s)
				continue
			}
			collectExprs(vv, out)
		}
	case []any:
		for _, vv := range x {
			collectExprs(vv, out)
		}
	}
}

// metrics.golden は契約の表の全指標を含む。
func TestGoldenCoversContractNames(t *testing.T) {
	got := goldenMetricNames(t)
	for _, name := range contractMetricNames {
		if !got[name] {
			t.Errorf("testdata/metrics.golden lacks %s", name)
		}
	}
	for name := range got {
		found := false
		for _, c := range contractMetricNames {
			found = found || c == name
		}
		if !found {
			t.Errorf("testdata/metrics.golden has %s which is not in the contract", name)
		}
	}
}

// ダッシュボードが JSON として正しく、全 expr の neuralwatt_* の名前が exporter の指標の名前の集合に含まれる。
func TestDashboardMetricNamesAreExported(t *testing.T) {
	var dash map[string]any
	if err := json.Unmarshal(dashboardJSON(t), &dash); err != nil {
		t.Fatalf("neuralwatt.json is not valid JSON: %v", err)
	}
	if dash["uid"] != "neuralwatt" || dash["editable"] != false {
		t.Errorf("uid=%v editable=%v", dash["uid"], dash["editable"])
	}
	var exprs []string
	collectExprs(dash, &exprs)
	if len(exprs) == 0 {
		t.Fatal("no expr found in the dashboard")
	}
	exported := goldenMetricNames(t)
	used := map[string]bool{}
	for _, e := range exprs {
		for _, name := range metricNameRE.FindAllString(e, -1) {
			used[name] = true
			if !exported[name] {
				t.Errorf("expr %q uses %s, which the exporter does not expose", e, name)
			}
		}
	}
	var names []string
	for n := range used {
		names = append(names, n)
	}
	sort.Strings(names)
	t.Logf("%d exprs use %d metric names: %v", len(exprs), len(names), names)
}

// 全パネルのデータソースは uid prometheus。
func TestDashboardDatasources(t *testing.T) {
	var dash map[string]any
	if err := json.Unmarshal(dashboardJSON(t), &dash); err != nil {
		t.Fatal(err)
	}
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if ds, ok := x["datasource"].(map[string]any); ok && ds["uid"] != "prometheus" {
				t.Errorf("unexpected datasource %v", ds)
			}
			for _, vv := range x {
				walk(vv)
			}
		case []any:
			for _, vv := range x {
				walk(vv)
			}
		}
	}
	walk(dash)
	if time, _ := dash["time"].(map[string]any); time["from"] != "now-7d" {
		t.Errorf("default range = %v, want now-7d", time)
	}
}

// 現在値のパネル（stat・gauge・table）の全 target は instant クエリ（instant: true、range: false）。
// range クエリ + lastNotNull だと、系列が消えた（上限なし・サブスクリプションなし）あとも、表示期間内の過去の値や
// 古いラベルの系列を出し続ける。履歴は時系列パネルで見る。
func TestDashboardCurrentValuePanelsUseInstantQueries(t *testing.T) {
	var dash struct {
		Panels []struct {
			Title   string `json:"title"`
			Type    string `json:"type"`
			Targets []struct {
				Expr    string `json:"expr"`
				Instant *bool  `json:"instant"`
				Range   *bool  `json:"range"`
			} `json:"targets"`
		} `json:"panels"`
	}
	if err := json.Unmarshal(dashboardJSON(t), &dash); err != nil {
		t.Fatal(err)
	}
	var current, series int
	for _, p := range dash.Panels {
		switch p.Type {
		case "stat", "gauge", "table":
			current++
			if len(p.Targets) == 0 {
				t.Errorf("panel %q has no targets", p.Title)
			}
			for _, tg := range p.Targets {
				if tg.Instant == nil || !*tg.Instant || tg.Range == nil || *tg.Range {
					t.Errorf("panel %q (%s): target %q must set instant: true and range: false", p.Title, p.Type, tg.Expr)
				}
			}
		case "timeseries":
			series++
			for _, tg := range p.Targets {
				if tg.Instant != nil && *tg.Instant {
					t.Errorf("time series panel %q: target %q must not be instant", p.Title, tg.Expr)
				}
			}
		}
	}
	if current < 10 || series < 8 {
		t.Errorf("found %d current-value panels and %d time series panels; the dashboard changed shape", current, series)
	}
}
