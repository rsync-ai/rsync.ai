package handlers

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"api-gateway/internal/charts"
)

// Each engine's date grain, pinned as a golden file: every grain, a week starting on
// Monday, Wednesday and Sunday, and a fiscal year starting in January, April and
// October. chart_grain_pg_test.go runs the Postgres and MySQL ones on a real engine.
//
//	go test ./internal/handlers -run TestChartDateGrain_Golden -update-chart-sql

type grainCase struct {
	grain        string
	week, fiscal int
}

// grainCases are the grain, week start and fiscal month each golden line is written for.
var grainCases = func() []grainCase {
	var out []grainCase
	add := func(g string, w, f int) { out = append(out, grainCase{g, w, f}) }
	add(charts.GrainDay, 1, 1)
	for _, w := range []int{1, 3, 7} {
		add(charts.GrainWeek, w, 1)
	}
	add(charts.GrainMonth, 1, 1)
	for _, g := range []string{charts.GrainQuarter, charts.GrainYear} {
		for _, f := range []int{1, 4, 10} {
			add(g, 1, f)
		}
	}
	return out
}()

func TestChartDateGrain_Golden(t *testing.T) {
	var b strings.Builder
	b.WriteString("# dialect grain week_start fiscal_month: the expression\n")
	for _, src := range goldenChartSources {
		fmt.Fprintf(&b, "\n-- %s\n", src.Dialect)
		for _, c := range grainCases {
			fmt.Fprintf(&b, "%s w%d f%d: %s\n", c.grain, c.week, c.fiscal, chartDateGrain(src.Dialect, c.grain, "e", c.week, c.fiscal))
		}
	}
	path := filepath.Join("testdata", "chart_sql", "date_grain.golden")
	if *updateChartSQL {
		if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (write it with -update-chart-sql)", err)
	}
	if got := b.String(); got != string(want) {
		t.Fatalf("%s differs from the code; the code writes:\n%s", path, got)
	}
}

// A week start or a fiscal month out of range is the default, never SQL built from it.
func TestChartDateGrain_OutOfRangeIsTheDefault(t *testing.T) {
	for _, d := range goldenChartSources {
		for _, g := range charts.Grains {
			want := chartDateGrain(d.Dialect, g, "e", 1, 1)
			for _, bad := range [][2]int{{0, 0}, {8, 13}, {-1, -5}} {
				if got := chartDateGrain(d.Dialect, g, "e", bad[0], bad[1]); got != want {
					t.Errorf("%s %s w%d f%d = %s, want %s", d.Dialect, g, bad[0], bad[1], got, want)
				}
			}
		}
	}
	// Control: an engine the gateway does not know gets the column as it is.
	if got := chartDateGrain("duckdb", charts.GrainWeek, "e", 1, 1); got != "e" {
		t.Errorf("unknown dialect = %s", got)
	}
}
