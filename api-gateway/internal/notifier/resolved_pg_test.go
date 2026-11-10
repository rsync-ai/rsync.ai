//go:build integration_pg

// Real-PostgreSQL coverage for the resolved-notice gate and the relapse-aware dedup
// (resolved.go, notifier.go recentDuplicate). sqlmock matches the SQL text and never
// runs it, so only a real server shows which rows "since the last resolved notice",
// the opened_at bound and a NULL bound actually select.
//
// Not part of the default suite. It needs a live server, nothing else: each test
// shadows pipeline_notifications with a temp table on a single connection.
//
//	docker run -d --rm --name notifier-pg -e POSTGRES_PASSWORD=verify -p 55447:5432 postgres:16-alpine
//	NOTIFIER_PG_DSN='postgres://postgres:verify@localhost:55447/postgres?sslmode=disable' \
//	    go test -tags integration_pg ./internal/notifier/ -run PG_ -v
//	docker rm -f notifier-pg
package notifier

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	_ "github.com/rsync-ai/shared/pgdriver"
)

const (
	pgRaiseKey    = "raise"
	pgResolvedKey = "resolved"
)

// notifierPG returns a Notifier on one connection whose pipeline_notifications is a
// temp table holding a row per key at each age (minutes before the server's NOW()).
func notifierPG(t *testing.T, rows map[string][]int) *Notifier {
	t.Helper()
	dsn := os.Getenv("NOTIFIER_PG_DSN")
	if dsn == "" {
		t.Skip("NOTIFIER_PG_DSN not set — see the file header for the commands that provide one")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	// A temp table lives on its connection: keep the pool to that one.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`CREATE TEMP TABLE pipeline_notifications (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		dedup_key TEXT NOT NULL,
		created_at TIMESTAMPTZ NOT NULL,
		metadata JSONB NOT NULL DEFAULT '{}'::jsonb)`); err != nil {
		t.Fatalf("temp table: %v", err)
	}
	for key, ages := range rows {
		for _, age := range ages {
			if _, err := db.Exec(`INSERT INTO pipeline_notifications (dedup_key, created_at)
				VALUES ($1, NOW() - make_interval(mins => $2))`, key, age); err != nil {
				t.Fatalf("seed %s %dm: %v", key, age, err)
			}
		}
	}
	return NewEmitter(db)
}

func openedMinutesAgo(t *testing.T, n *Notifier, mins int) string {
	t.Helper()
	var now time.Time
	if err := n.db.QueryRow(`SELECT NOW()`).Scan(&now); err != nil {
		t.Fatal(err)
	}
	return now.Add(-time.Duration(mins) * time.Minute).UTC().Format(time.RFC3339Nano)
}

func TestPG_ResolvedNoticeDue(t *testing.T) {
	for _, tc := range []struct {
		name      string
		raise     []int
		resolved  []int
		openedAgo int // 0: no opened_at sent
		want      bool
	}{
		{"never pushed", nil, nil, 0, false},
		{"open ten minutes", []int{10}, nil, 0, true},
		{"open three minutes", []int{3}, nil, 0, false},
		{"already answered", []int{10}, []int{8}, 0, false},
		{"deduped against a 50-minute row, episode two minutes old", []int{50}, nil, 2, false},
		{"deduped against a 50-minute row, episode ten minutes old", []int{50}, nil, 10, true},
		{"only another episode's row, five hours old", []int{300}, nil, 10, false},
		{"relapse after an answered episode", []int{90, 10}, []int{80}, 10, true},
		{"relapse after an answered episode, three minutes old", []int{90, 3}, []int{80}, 3, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := notifierPG(t, map[string][]int{pgRaiseKey: tc.raise, pgResolvedKey: tc.resolved})
			opened := ""
			if tc.openedAgo > 0 {
				opened = openedMinutesAgo(t, n, tc.openedAgo)
			}
			got, err := n.resolvedNoticeDue(context.Background(), pgRaiseKey, pgResolvedKey, opened)
			if err != nil {
				t.Fatalf("resolvedNoticeDue: %v", err)
			}
			if got != tc.want {
				t.Errorf("due = %v, want %v (raise rows %v, resolved rows %v, opened %q)", got, tc.want, tc.raise, tc.resolved, opened)
			}
		})
	}
}

func TestPG_RecentDuplicate(t *testing.T) {
	for _, tc := range []struct {
		name        string
		raise       []int
		resolved    []int
		resolvedKey string
		want        bool
	}{
		{"alert with no resolved notice, 20 minutes old", []int{20}, nil, "", true},
		{"alert with no resolved notice, 90 minutes old", []int{90}, nil, "", false},
		{"paired alert, nothing answered yet", []int{20}, nil, pgResolvedKey, true},
		{"paired alert answered since: the relapse is delivered", []int{20}, []int{10}, pgResolvedKey, false},
		{"paired alert raised again after the answer", []int{20, 5}, []int{10}, pgResolvedKey, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := notifierPG(t, map[string][]int{pgRaiseKey: tc.raise, pgResolvedKey: tc.resolved})
			id, err := n.recentDuplicate(context.Background(), pgRaiseKey, tc.resolvedKey, "", "")
			if err != nil {
				t.Fatalf("recentDuplicate: %v", err)
			}
			if got := id != ""; got != tc.want {
				t.Errorf("duplicate = %v (%s), want %v", got, fmt.Sprint(id), tc.want)
			}
		})
	}
}

// pgRow is one stored notification: its key, its age in minutes before the server's
// NOW(), and the severity its producer sent ("" for a row stored before that was kept).
type pgRow struct {
	key string
	age int
	sev string
}

func TestPG_RecentDuplicate_OncePerEpisode(t *testing.T) {
	for _, tc := range []struct {
		name        string
		rows        []pgRow
		resolvedKey string
		openedAgo   int // 0: no opened_at sent
		severity    string
		want        bool
	}{
		{"no opened_at: a 90-minute row is outside the window", []pgRow{{pgRaiseKey, 90, "critical"}}, "", 0, "critical", false},
		{"in the episode: a 90-minute row repeats it", []pgRow{{pgRaiseKey, 90, "critical"}}, "", 180, "critical", true},
		{"in the episode: a five-hour row repeats it", []pgRow{{pgRaiseKey, 300, "critical"}}, "", 360, "critical", true},
		{"another episode's row, before this one opened", []pgRow{{pgRaiseKey, 90, "critical"}}, "", 30, "critical", false},
		{"escalation: the episode's warning does not repeat a critical", []pgRow{{pgRaiseKey, 90, "warning"}}, "", 180, "critical", false},
		{"escalation, then: the episode's critical repeats it", []pgRow{{pgRaiseKey, 90, "warning"}, {pgRaiseKey, 30 + 60, "critical"}}, "", 180, "critical", true},
		{"de-escalation: the episode's critical repeats a warning", []pgRow{{pgRaiseKey, 90, "critical"}}, "", 180, "warning", true},
		{"a row stored before raw_severity was kept does not repeat a critical", []pgRow{{pgRaiseKey, 90, ""}}, "", 180, "critical", false},
		{"a raise with no severity is repeated by any row of its episode", []pgRow{{pgRaiseKey, 90, ""}}, "", 180, "", true},
		{"inside the window any severity repeats it, as before", []pgRow{{pgRaiseKey, 20, "warning"}}, "", 180, "critical", true},
		{"answered since: the relapse is delivered", []pgRow{{pgRaiseKey, 90, "critical"}, {pgResolvedKey, 60, ""}}, pgResolvedKey, 180, "critical", false},
		{"answered before the row: still a repeat", []pgRow{{pgRaiseKey, 90, "critical"}, {pgResolvedKey, 100, ""}}, pgResolvedKey, 180, "critical", true},
		{"another key's row is no repeat", []pgRow{{"other", 90, "critical"}}, "", 180, "critical", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := notifierPG(t, nil)
			for _, r := range tc.rows {
				meta := `{}`
				if r.sev != "" {
					meta = `{"raw_severity":"` + r.sev + `"}`
				}
				if _, err := n.db.Exec(`INSERT INTO pipeline_notifications (dedup_key, created_at, metadata)
					VALUES ($1, NOW() - make_interval(mins => $2), $3::jsonb)`, r.key, r.age, meta); err != nil {
					t.Fatalf("seed %+v: %v", r, err)
				}
			}
			opened := ""
			if tc.openedAgo > 0 {
				opened = openedMinutesAgo(t, n, tc.openedAgo)
			}
			id, err := n.recentDuplicate(context.Background(), pgRaiseKey, tc.resolvedKey, opened, tc.severity)
			if err != nil {
				t.Fatalf("recentDuplicate: %v", err)
			}
			if got := id != ""; got != tc.want {
				t.Errorf("duplicate = %v (%s), want %v (rows %+v, opened %q, severity %q)", got, id, tc.want, tc.rows, opened, tc.severity)
			}
		})
	}
}
