package cdc

import (
	"context"
	"database/sql/driver"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// A forced connection delete releases that connection's leftover slots and
// publications first (ReleaseConnectionObjects). The reapable rows it starts from
// are every connection's, so the filter is what keeps it off other sources.

func TestResourcesOfConnection(t *testing.T) {
	const ours = "3f0c2a8e-5b1d-4c7e-9a2f-6d8e0b1c2d3e"
	resources := []CDCResource{
		{ResourceName: "slot_ours", ConnectionID: ours},
		{ResourceName: "slot_ours_upper", ConnectionID: " 3F0C2A8E-5B1D-4C7E-9A2F-6D8E0B1C2D3E "},
		{ResourceName: "slot_other", ConnectionID: "9a8b7c6d-0000-4000-8000-000000000001"},
		{ResourceName: "slot_unrecorded", ConnectionID: ""},
	}

	got := resourcesOfConnection(resources, "  "+ours+"  ")
	if len(got) != 2 || got[0].ResourceName != "slot_ours" || got[1].ResourceName != "slot_ours_upper" {
		t.Fatalf("resourcesOfConnection = %+v; want exactly this connection's two rows", got)
	}
	for _, id := range []string{"", "   "} {
		if got := resourcesOfConnection(resources, id); len(got) != 0 {
			t.Fatalf("resourcesOfConnection(%q) = %+v; an empty id must keep nothing, never everything", id, got)
		}
	}
}

// recordedArg matches any value and remembers it, so the test can see which
// connection each config lookup was for.
type recordedArg struct{ seen *[]string }

func (a recordedArg) Match(v driver.Value) bool {
	s, _ := v.(string)
	*a.seen = append(*a.seen, s)
	return true
}

func TestReleaseConnectionObjects_LooksUpOnlyTheNamedConnection(t *testing.T) {
	const ours = "3f0c2a8e-5b1d-4c7e-9a2f-6d8e0b1c2d3e"
	const other = "9a8b7c6d-0000-4000-8000-000000000001"
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	cols := []string{
		"id", "pipeline_id", "connection_id", "source_table",
		"resource_type", "resource_name", "status",
		"database_type", "metadata", "created_at", "deleted_at", "last_verified_at",
	}
	row := func(rows *sqlmock.Rows, connectionID, kind, name string) *sqlmock.Rows {
		return rows.AddRow("r-"+name, nil, connectionID, nil, kind, name, "active",
			"postgresql", []byte(`{}`), time.Unix(1700000000, 0), nil, nil)
	}
	// The other connection's rows come first: a filter that let everything
	// through would look the other connection up before ours.
	mock.ExpectQuery(`resource_type = 'replication_slot'`).WillReturnRows(
		row(row(sqlmock.NewRows(cols), other, "replication_slot", "slot_other"), ours, "replication_slot", "slot_ours"))
	mock.ExpectQuery(`resource_type = 'publication'`).WillReturnRows(
		row(row(sqlmock.NewRows(cols), other, "publication", "pub_other"), ours, "publication", "pub_ours"))
	// Each lookup fails, so nothing reaches a source; only who was looked up matters.
	var looked []string
	for range 2 {
		mock.ExpectQuery(`FROM connections WHERE id = \$1`).
			WithArgs(recordedArg{&looked}).
			WillReturnError(errors.New("no source in a unit test"))
	}

	dropped, err := NewPostgreSQLManager(db).ReleaseConnectionObjects(context.Background(), ours)

	if err != nil || dropped != 0 {
		t.Fatalf("ReleaseConnectionObjects = %d, %v; want 0, nil", dropped, err)
	}
	if len(looked) != 2 || looked[0] != ours || looked[1] != ours {
		t.Fatalf("connection configs looked up = %q; want only %s, once for the slot and once for the publication", looked, ours)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("slot then publication, each for this connection: %v", err)
	}
}

// A ledger that cannot be read is an error for the caller, not "nothing to drop".
func TestReleaseConnectionObjects_ReportsALedgerReadFailure(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	mock.ExpectQuery(`resource_type = 'replication_slot'`).WillReturnError(errors.New("cdc_resources unreadable"))

	if _, err := NewPostgreSQLManager(db).ReleaseConnectionObjects(context.Background(), "3f0c2a8e-5b1d-4c7e-9a2f-6d8e0b1c2d3e"); err == nil {
		t.Fatal("a failed ledger read was reported as success")
	}
}
