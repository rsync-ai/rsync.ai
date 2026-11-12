package handlers

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"api-gateway/internal/db"

	"github.com/DATA-DOG/go-sqlmock"
)

// Bug class: an 'all' fan-in waits on a model upstream that has nothing pending. A model
// that only upstream events rebuild, and whose upstreams have produced nothing since its
// last build, will not rebuild again; waiting for it skips the fan-in on every sibling
// fire, forever. Seen on prod: two gold models skipped every hour from 10-15 on, held by
// a silver model on an idle CDC pipeline.

var ancestryQuery = `WITH RECURSIVE closure[\s\S]*cdc_table_watermarks`

// ancestryRows is the shape the ancestry lookup scans: one row per edge of each model
// above a stale model upstream, and one row with no edge for a model that has none.
func ancestryRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"model_id", "baseline", "upstream_id", "is_model", "model_succeeded_at",
		"pipeline_completed_at", "is_cdc", "landed_at"})
}

// The prod shape: Active customers by month waits on Transactions (silver, firing),
// Customers (silver, rebuilt after the baseline) and Accounts (silver), which hangs off
// a CDC pipeline whose last rows landed before Accounts last rebuilt.
func TestUnsatisfiedUpstreams_AModelUpstreamWithNothingPendingDoesNotHoldTheFanIn(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	baseline := time.Date(2026, 10, 15, 16, 1, 0, 0, time.UTC)
	accounts, customers, cdc := provModelB, provRunA, provRunB

	mock.ExpectQuery(unsatisfiedQuery).
		WithArgs(provSchedule, provUpstream).
		WillReturnRows(upstreamFactRows().
			AddRow(accounts, true, baseline, baseline.Add(-5*time.Second), nil, false, nil, nil).
			AddRow(customers, true, baseline, baseline.Add(8*24*time.Hour), nil, false, nil, nil))
	mock.ExpectQuery(ancestryQuery).
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(ancestryRows().
			AddRow(accounts, baseline.Add(-8*time.Second), cdc, false, nil, nil, true, baseline.Add(-13*time.Minute)))

	got, err := unsatisfiedUpstreams(context.Background(), db.DB, provSchedule, provUpstream)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("waiting on %v, want nothing: Accounts has nothing pending", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// The control, the loan shape: Loan payments (silver) has not rebuilt since the Nightly
// run completed after its last build started, so the fan-in still waits for it.
func TestUnsatisfiedUpstreams_AModelUpstreamWithAPendingCompletionStillHoldsTheFanIn(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	baseline := time.Date(2026, 10, 22, 2, 0, 13, 0, time.UTC)
	payments, nightly := provModelB, provRunB

	mock.ExpectQuery(unsatisfiedQuery).
		WithArgs(provSchedule, provUpstream).
		WillReturnRows(upstreamFactRows().
			AddRow(payments, true, baseline, baseline.Add(-time.Second), nil, false, nil, nil))
	mock.ExpectQuery(ancestryQuery).
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(ancestryRows().
			AddRow(payments, baseline.Add(-8*time.Second), nightly, false, nil, baseline.Add(24*time.Hour-10*time.Second), false, nil))

	got, err := unsatisfiedUpstreams(context.Background(), db.DB, provSchedule, provUpstream)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != payments {
		t.Errorf("waiting on %v, want Loan payments", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// A model upstream that has never built has nothing to read: it waits, and the ancestry
// is not even looked up (sqlmock fails on a query it does not expect).
func TestUnsatisfiedUpstreams_ANeverBuiltModelUpstreamWaitsWithoutTheAncestryLookup(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	baseline := time.Date(2026, 10, 15, 16, 1, 0, 0, time.UTC)

	mock.ExpectQuery(unsatisfiedQuery).
		WithArgs(provSchedule, provUpstream).
		WillReturnRows(upstreamFactRows().
			AddRow(provModelB, true, baseline, nil, nil, false, nil, nil))

	got, err := unsatisfiedUpstreams(context.Background(), db.DB, provSchedule, provUpstream)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != provModelB {
		t.Errorf("waiting on %v, want the never-built model", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestModelAncestrySettled(t *testing.T) {
	base := time.Date(2026, 10, 15, 16, 0, 52, 0, time.UTC)
	at := func(d time.Duration) sql.NullTime { return sql.NullTime{Time: base.Add(d), Valid: true} }
	none := sql.NullTime{}
	cdc := func(landed sql.NullTime) ancestryEdge { return ancestryEdge{ID: "cdc", IsCDC: true, LandedAt: landed} }
	batch := func(done sql.NullTime) ancestryEdge { return ancestryEdge{ID: "batch", PipelineCompletedAt: done} }
	model := func(id string, built sql.NullTime) ancestryEdge {
		return ancestryEdge{ID: id, IsModel: true, ModelSucceededAt: built}
	}
	node := func(baseline sql.NullTime, edges ...ancestryEdge) ancestryNode {
		return ancestryNode{Baseline: baseline, Edges: edges}
	}

	for _, tc := range []struct {
		name string
		a    modelAncestry
		want bool
	}{
		{"idle CDC parent: last landing before the last build", modelAncestry{
			"m": node(at(0), cdc(at(-12*time.Minute)))}, true},
		{"CDC parent that never landed", modelAncestry{
			"m": node(at(0), cdc(none))}, true},
		{"CDC landing after the last build started is pending", modelAncestry{
			"m": node(at(0), cdc(at(time.Second)))}, false},
		{"CDC landing at the baseline instant is not after it", modelAncestry{
			"m": node(at(0), cdc(at(0)))}, true},
		{"batch completion after the last build is pending", modelAncestry{
			"m": node(at(0), batch(at(time.Hour)))}, false},
		{"batch pipeline with no completion since", modelAncestry{
			"m": node(at(0), batch(none))}, true},
		{"a landing on a batch pipeline is not a completion", modelAncestry{
			"m": node(at(0), ancestryEdge{ID: "batch", LandedAt: at(time.Hour)})}, true},
		{"model parent rebuilt after the last build is pending", modelAncestry{
			"m": node(at(0), model("p", at(time.Minute))),
			"p": node(at(time.Second), cdc(none))}, false},
		{"two levels, all idle", modelAncestry{
			"m": node(at(0), model("p", at(-time.Minute))),
			"p": node(at(-2*time.Minute), cdc(at(-time.Hour)))}, true},
		{"two levels, grandparent source pending", modelAncestry{
			"m": node(at(0), model("p", at(-time.Minute))),
			"p": node(at(-2*time.Minute), cdc(at(time.Hour)))}, false},
		{"a model with no upstreams to watch is unknown", modelAncestry{
			"m": node(at(0))}, false},
		{"a model missing from the ancestry is unknown", modelAncestry{}, false},
		{"an ancestor with no upstreams to watch is unknown", modelAncestry{
			"m":    node(at(0), model("cron", at(-time.Minute))),
			"cron": node(at(-time.Minute))}, false},
		{"an ancestor missing from the ancestry is unknown", modelAncestry{
			"m": node(at(0), model("gone", at(-time.Minute)))}, false},
		{"a ring is unknown", modelAncestry{
			"m": node(at(0), model("p", at(-time.Minute))),
			"p": node(at(-time.Minute), model("m", at(-time.Second)))}, false},
		{"a diamond over one idle source", modelAncestry{
			"m": node(at(0), model("x", at(-time.Minute)), model("y", at(-time.Minute))),
			"x": node(at(-2*time.Minute), model("z", at(-3*time.Minute))),
			"y": node(at(-2*time.Minute), model("z", at(-3*time.Minute))),
			"z": node(at(-4*time.Minute), cdc(at(-time.Hour)))}, true},
		{"one pending edge among idle ones", modelAncestry{
			"m": node(at(0), cdc(at(-time.Hour)), batch(none), batch(at(time.Minute)))}, false},
		{"never built: any parent output is pending", modelAncestry{
			"m": node(none, cdc(at(-time.Hour)))}, false},
		{"never built and no parent output", modelAncestry{
			"m": node(none, cdc(none), batch(none))}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.a.settled("m"); got != tc.want {
				t.Errorf("settled = %v, want %v", got, tc.want)
			}
		})
	}
}
