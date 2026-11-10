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
