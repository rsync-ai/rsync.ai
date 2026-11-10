package handlers

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// "Run after pipeline" for CDC on a window's trailing edge (cdc_trailing_edge.go).
//
// simulateCDCTrailingEdge runs the sweep's decision every minute (at :30 past) from
// start to end over a list of landing times, applying each step to the state the way
// sweepCDCTrailingEdge writes it, and returns each fire's boundary and the landing it
// covered. It is the sweep minus its two SQL statements, which the sqlmock test below
// pins.
type simFire struct{ boundary, landedThrough time.Time }

func simulateCDCTrailingEdge(t *testing.T, landings []time.Time, start, end time.Time, window time.Duration) []simFire {
	t.Helper()
	var s cdcTrailingEdgeState
	var fires []simFire
	for now := start; !now.After(end); now = now.Add(time.Minute) {
		var landedAt time.Time
		for _, l := range landings {
			if !l.After(now) && l.After(landedAt) {
				landedAt = l
			}
		}
		if landedAt.IsZero() {
			continue // the sweep only reads pipelines with a landing
		}
		step := decideCDCTrailingEdge(landedAt, s, now, window)
		switch {
		case step.Fire:
			fires = append(fires, simFire{step.Boundary, landedAt})
			s = cdcTrailingEdgeState{
				WindowEnd:     sql.NullTime{Time: step.Boundary, Valid: true},
				LandedThrough: sql.NullTime{Time: landedAt, Valid: true},
			}
		case step.MarkPending:
			s.PendingSince = sql.NullTime{Time: landedAt, Valid: true}
		}
	}
	return fires
}

func hm(t *testing.T, clock string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, "2026-10-03T"+clock+"Z")
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// § 13: a burst then silence builds the tail once. The burst lands at 10:03, 10:05 and
// 10:07 and nothing follows. The leading edge this replaced fired once, at 10:03, and
// the rows of 10:05 and 10:07 were never built; the trailing edge fires once, at the
// 10:15 boundary, after the whole burst.
func TestCDCTrailingEdge_ABurstThenSilenceBuildsTheTailOnce(t *testing.T) {
	burst := []time.Time{hm(t, "10:03:00"), hm(t, "10:05:00"), hm(t, "10:07:00")}
	fires := simulateCDCTrailingEdge(t, burst, hm(t, "10:00:30"), hm(t, "11:30:30"), 15*time.Minute)
	if len(fires) != 1 {
		t.Fatalf("fired %d times over a burst and an hour of silence, want once: %+v", len(fires), fires)
	}
	if want := hm(t, "10:15:00"); !fires[0].boundary.Equal(want) {
		t.Errorf("fired at the %s boundary, want the first one after the burst, %s", fires[0].boundary, want)
	}
	if last := burst[len(burst)-1]; !fires[0].landedThrough.Equal(last) {
		t.Errorf("the fire covered landings through %s, want the burst's last landing %s", fires[0].landedThrough, last)
	}
}

// A stream that never pauses builds once per window, never more, and its last rows are
// built at the boundary after it stops.
func TestCDCTrailingEdge_AContinuousStreamBuildsOncePerWindow(t *testing.T) {
	var stream []time.Time
	for l := hm(t, "10:00:00"); l.Before(hm(t, "11:00:00")); l = l.Add(30 * time.Second) {
		stream = append(stream, l)
	}
	fires := simulateCDCTrailingEdge(t, stream, hm(t, "10:00:30"), hm(t, "11:45:30"), 15*time.Minute)
	want := []time.Time{hm(t, "10:15:00"), hm(t, "10:30:00"), hm(t, "10:45:00"), hm(t, "11:00:00")}
	if len(fires) != len(want) {
		t.Fatalf("fired %d times over an hour of streaming, want once per window (%d): %+v", len(fires), len(want), fires)
	}
	for i, f := range fires {
		if !f.boundary.Equal(want[i]) {
			t.Errorf("fire %d at %s, want %s", i, f.boundary, want[i])
		}
	}
	if last := stream[len(stream)-1]; !fires[len(fires)-1].landedThrough.Equal(last) {
		t.Errorf("the last fire covered %s, want the stream's last landing %s", fires[len(fires)-1].landedThrough, last)
	}
}

// The known limit, pinned so it stays a limit and not a loss: a landing just before a
// boundary that the sweep first reads after it, behind a newer landing past the
// boundary, is built at the next boundary — a window late, once.
func TestCDCTrailingEdge_ALandingSeenOnlyAfterItsBoundaryIsBuiltAWindowLate(t *testing.T) {
	landings := []time.Time{hm(t, "10:14:50"), hm(t, "10:15:10")}
	fires := simulateCDCTrailingEdge(t, landings, hm(t, "10:00:30"), hm(t, "11:00:30"), 15*time.Minute)
	if len(fires) != 1 || !fires[0].boundary.Equal(hm(t, "10:30:00")) || !fires[0].landedThrough.Equal(hm(t, "10:15:10")) {
		t.Fatalf("fires = %+v, want one at 10:30 covering 10:15:10", fires)
	}

	// Control: on its own, the same landing is built at its boundary.
	fires = simulateCDCTrailingEdge(t, landings[:1], hm(t, "10:00:30"), hm(t, "11:00:30"), 15*time.Minute)
	if len(fires) != 1 || !fires[0].boundary.Equal(hm(t, "10:15:00")) {
		t.Fatalf("fires = %+v, want one at 10:15", fires)
	}
}

func TestDecideCDCTrailingEdge(t *testing.T) {
	const w = 15 * time.Minute
	at := func(clock string) sql.NullTime { return sql.NullTime{Time: hm(t, clock), Valid: true} }
	now := hm(t, "10:20:00") // boundary 10:15

	for _, tc := range []struct {
		name     string
		landedAt string
		state    cdcTrailingEdgeState
		want     cdcTrailingEdgeStep
	}{
		{"never fired, landed before the boundary", "10:07:00", cdcTrailingEdgeState{},
			cdcTrailingEdgeStep{Fire: true}},
		{"never fired, landed after the boundary: parked", "10:16:00", cdcTrailingEdgeState{},
			cdcTrailingEdgeStep{MarkPending: true}},
		{"committed before the boundary after the fire there: waits for the next", "10:12:00",
			cdcTrailingEdgeState{WindowEnd: at("10:15:00"), LandedThrough: at("10:07:00")},
			cdcTrailingEdgeStep{MarkPending: true}},
		{"nothing new since the last fire", "10:07:00",
			cdcTrailingEdgeState{WindowEnd: at("10:00:00"), LandedThrough: at("10:07:00")},
			cdcTrailingEdgeStep{}},
		{"stream kept landing past the boundary, parked before it", "10:19:00",
			cdcTrailingEdgeState{WindowEnd: at("10:00:00"), LandedThrough: at("10:00:20"), PendingSince: at("10:01:00")},
			cdcTrailingEdgeStep{Fire: true}},
		{"parked after the boundary: wait for the next", "10:19:00",
			cdcTrailingEdgeState{WindowEnd: at("10:15:00"), LandedThrough: at("10:15:20"), PendingSince: at("10:16:00")},
			cdcTrailingEdgeStep{}},
		{"a parked landing the last fire covered is stale: parked afresh", "10:19:00",
			cdcTrailingEdgeState{WindowEnd: at("10:00:00"), LandedThrough: at("10:12:00"), PendingSince: at("10:10:00")},
			cdcTrailingEdgeStep{MarkPending: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.want.Boundary = hm(t, "10:15:00")
			if got := decideCDCTrailingEdge(hm(t, tc.landedAt), tc.state, now, w); got != tc.want {
				t.Fatalf("step = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestCDCWindowBoundaryAndOccurrence(t *testing.T) {
	for _, tc := range []struct {
		unix, want int64
	}{
		{0, 0}, {899, 0}, {900, 900}, {1799, 900}, {-1, -900}, {-900, -900}, {-901, -1800},
	} {
		if got := cdcWindowBoundary(time.Unix(tc.unix, 0), 15*time.Minute).Unix(); got != tc.want {
			t.Errorf("cdcWindowBoundary(%d, 15m) = %d, want %d", tc.unix, got, tc.want)
		}
	}
	b := hm(t, "10:15:00")
	if got, want := cdcTrailingEdgeOccurrence(15*time.Minute, b), fmt.Sprintf("cdc-trail-15m-%d", b.Unix()/900); got != want {
		t.Errorf("occurrence = %q, want %q", got, want)
	}
	if cdcTrailingEdgeOccurrence(5*time.Minute, b) == cdcTrailingEdgeOccurrence(15*time.Minute, b) {
		t.Error("two window sizes share an occurrence label")
	}
}

func TestCDCAfterPipelineInterval_FromEnv(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  time.Duration
	}{
		{"", 15 * time.Minute},
		{"5", 5 * time.Minute},
		{" 60 ", 60 * time.Minute},
		{"0", 15 * time.Minute},
		{"-3", 15 * time.Minute},
		{"abc", 15 * time.Minute},
		{"1.5", 15 * time.Minute},
	} {
		t.Run(fmt.Sprintf("%q", tc.value), func(t *testing.T) {
			t.Setenv(CDCAfterPipelineIntervalEnv, tc.value)
			if got := cdcAfterPipelineInterval(); got != tc.want {
				t.Errorf("interval for %q = %s, want %s", tc.value, got, tc.want)
			}
		})
	}
}

// The sweep's order is the restart guarantee: fire first, record after, and record
// nothing for a fire that failed, so the next sweep repeats it with the same occurrence.
func TestSweepCDCTrailingEdge_FiresThenRecords(t *testing.T) {
	const (
		fires   = "11111111-1111-1111-1111-111111111111"
		parks   = "22222222-2222-2222-2222-222222222222"
		settled = "33333333-3333-3333-3333-333333333333"
	)
	now := hm(t, "10:20:00")
	boundary := hm(t, "10:15:00")
	cols := []string{"id", "landed_at", "window_end", "landed_through", "pending_since", "now"}
	facts := func() *sqlmock.Rows {
		return sqlmock.NewRows(cols).
			AddRow(fires, hm(t, "10:07:00"), nil, nil, nil, now).
			AddRow(parks, hm(t, "10:16:00"), nil, nil, nil, now).
			AddRow(settled, hm(t, "10:07:00"), boundary, hm(t, "10:07:00"), nil, now)
	}
	sweepQuery := regexp.QuoteMeta(`FROM landed l`)
	fireWrite := regexp.QuoteMeta(cdcTrailingEdgeRecordFireSQL)
	parkWrite := regexp.QuoteMeta(cdcTrailingEdgeParkSQL)

	t.Run("a settled fire is recorded after it, a new landing is parked", func(t *testing.T) {
		database, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		defer database.Close()
		var calls []string
		fire := func(_ context.Context, _ *sql.DB, pipelineID, occurrence string) error {
			calls = append(calls, pipelineID+" "+occurrence)
			return nil
		}
		mock.ExpectQuery(sweepQuery).WillReturnRows(facts())
		mock.ExpectExec(fireWrite).WithArgs(fires, boundary, hm(t, "10:07:00")).WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectExec(parkWrite).WithArgs(parks, hm(t, "10:16:00")).WillReturnResult(sqlmock.NewResult(0, 1))

		res, err := sweepCDCTrailingEdge(context.Background(), database, 15*time.Minute, sql.NullTime{}, fire)
		if err != nil {
			t.Fatal(err)
		}
		if want := []string{fires + " " + cdcTrailingEdgeOccurrence(15*time.Minute, boundary)}; fmt.Sprint(calls) != fmt.Sprint(want) {
			t.Fatalf("fired %v, want %v", calls, want)
		}
		if (res != cdcTrailingEdgeSweepResult{Scanned: 3, Fired: 1, Pending: 1}) {
			t.Fatalf("result = %+v", res)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("a failed fire records nothing", func(t *testing.T) {
		database, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		defer database.Close()
		fire := func(context.Context, *sql.DB, string, string) error { return errors.New("lookup failed") }
		mock.ExpectQuery(sweepQuery).WillReturnRows(facts())
		mock.ExpectExec(parkWrite).WithArgs(parks, hm(t, "10:16:00")).WillReturnResult(sqlmock.NewResult(0, 1))

		res, err := sweepCDCTrailingEdge(context.Background(), database, 15*time.Minute, sql.NullTime{}, fire)
		if err != nil {
			t.Fatal(err)
		}
		if res.Fired != 0 || res.Failed != 1 {
			t.Fatalf("result = %+v, want the fire counted as failed and not recorded", res)
		}
		// sqlmock fails any statement it was not told to expect, so reaching here with
		// every expectation met means no state row was written for the failed fire.
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})
}
