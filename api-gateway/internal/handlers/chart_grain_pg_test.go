//go:build integration_pg

// Each date grain run on a real engine (dashboards-design.md § 14.3): for every day from
// 2023-11-20 to 2026-03-19, as a date and as a timestamp at 23:30, each grain with each
// week start (1-7) and fiscal month (1-12) must be the first day of the period worked out
// in Go. Text alone cannot show a week starting on the right day; the engine can.
//
//	docker run -d --rm --name grain-pg -e POSTGRES_PASSWORD=verify -p 55473:5432 postgres:16-alpine
//	docker run -d --rm --name grain-my -e MYSQL_ROOT_PASSWORD=verify -p 55474:3306 mysql:8.4
//	CHART_GRAIN_PG_DSN='postgres://postgres:verify@localhost:55473/postgres?sslmode=disable' \
//	CHART_GRAIN_MYSQL_DSN='root:verify@tcp(localhost:55474)/' \
//	    go test -tags integration_pg ./internal/handlers/ -run PG_ChartDateGrain -v
//	docker rm -f grain-pg grain-my
package handlers

import (
	"database/sql"
	"os"
	"testing"
	"time"

	"api-gateway/internal/charts"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/rsync-ai/shared/pgdriver"
)

const grainDays = 850

var grainFirstDay = time.Date(2023, 11, 20, 0, 0, 0, 0, time.UTC)

// grainEngines is each engine the test runs: its driver, its DSN's variable, and its
// query over every day, as a date d and a timestamp t, given the grain of each.
var grainEngines = []struct {
	dialect, driver, env string
	query                func(onDate, onTime string) string
}{
	{"postgres", "postgres", "CHART_GRAIN_PG_DSN", func(onDate, onTime string) string {
		return "SELECT CAST(d AS text), CAST(CAST((" + onDate + ") AS date) AS text), CAST(CAST((" + onTime + ") AS date) AS text) " +
			"FROM (SELECT DATE '2023-11-20' + i AS d, (DATE '2023-11-20' + i) + TIME '23:30' AS t " +
			"FROM generate_series(0, 849) AS i) x ORDER BY d"
	}},
	{"mysql", "mysql", "CHART_GRAIN_MYSQL_DSN", func(onDate, onTime string) string {
		return "WITH RECURSIVE n(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM n WHERE i < 849) " +
			"SELECT CAST(d AS CHAR), CAST(CAST((" + onDate + ") AS DATE) AS CHAR), CAST(CAST((" + onTime + ") AS DATE) AS CHAR) " +
			"FROM (SELECT DATE_ADD(DATE '2023-11-20', INTERVAL i DAY) AS d, " +
			"TIMESTAMP(DATE_ADD(DATE '2023-11-20', INTERVAL i DAY), '23:30:00') AS t FROM n) x ORDER BY d"
	}},
}

func TestPG_ChartDateGrain_EachEngineStartsEachPeriodRight(t *testing.T) {
	for _, eng := range grainEngines {
		t.Run(eng.dialect, func(t *testing.T) {
			dsn := os.Getenv(eng.env)
			if dsn == "" {
				t.Skipf("set %s to a disposable %s", eng.env, eng.dialect)
			}
			db, err := sql.Open(eng.driver, dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			checked := 0
			for _, g := range charts.Grains {
				weeks, fiscals := []int{1}, []int{1}
				switch g {
				case charts.GrainWeek:
					weeks = []int{1, 2, 3, 4, 5, 6, 7}
				case charts.GrainQuarter, charts.GrainYear:
					fiscals = []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}
				}
				for _, w := range weeks {
					for _, f := range fiscals {
						q := eng.query(chartDateGrain(eng.dialect, g, "d", w, f), chartDateGrain(eng.dialect, g, "t", w, f))
						checked += checkGrainRows(t, db, q, g, w, f)
					}
				}
			}
			// Control: every day of every case was read, so a query that returned
			// nothing cannot pass.
			if want := grainDays * (2 + 7 + 12*2); checked != want {
				t.Fatalf("checked %d days, want %d", checked, want)
			}
			t.Logf("%s: %d days × 2 column types match", eng.dialect, checked)
		})
	}
}

// checkGrainRows runs q and compares each day's grain, on a date and on a timestamp, to
// grainStart's; it returns how many days it read.
func checkGrainRows(t *testing.T, db *sql.DB, q, grain string, week, fiscal int) int {
	t.Helper()
	rows, err := db.Query(q)
	if err != nil {
		t.Fatalf("%s w%d f%d: %v\n%s", grain, week, fiscal, err, q)
	}
	defer rows.Close()
	n, wrong := 0, 0
	for rows.Next() {
		var day, onDate, onTime string
		if err := rows.Scan(&day, &onDate, &onTime); err != nil {
			t.Fatal(err)
		}
		want := grainStart(grainFirstDay.AddDate(0, 0, n), grain, week, fiscal).Format(time.DateOnly)
		if day != grainFirstDay.AddDate(0, 0, n).Format(time.DateOnly) {
			t.Fatalf("row %d is %s", n, day)
		}
		if (onDate != want || onTime != want) && wrong < 3 {
			wrong++
			t.Errorf("%s w%d f%d on %s: date %s, timestamp %s, want %s", grain, week, fiscal, day, onDate, onTime, want)
		}
		n++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return n
}

// grainStart is the first day of the period day falls in, worked out in Go: the day a
// week starting on weekStart (1 Monday … 7 Sunday) began, or the first day of its month,
// or of a quarter or a year starting in fiscalMonth.
func grainStart(day time.Time, grain string, weekStart, fiscalMonth int) time.Time {
	switch grain {
	case charts.GrainWeek:
		iso := (int(day.Weekday())+6)%7 + 1
		return day.AddDate(0, 0, -((iso - weekStart + 7) % 7))
	case charts.GrainMonth:
		return day.AddDate(0, 0, 1-day.Day())
	case charts.GrainQuarter, charts.GrainYear:
		// On the first of a month AddDate moves whole months, with no overflow.
		shifted := day.AddDate(0, 0, 1-day.Day()).AddDate(0, 1-fiscalMonth, 0)
		m := 1
		if grain == charts.GrainQuarter {
			m = (int(shifted.Month())-1)/3*3 + 1
		}
		return shifted.AddDate(0, m-int(shifted.Month()), 0).AddDate(0, fiscalMonth-1, 0)
	}
	return day
}
