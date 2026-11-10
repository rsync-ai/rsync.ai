package handlers

import (
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// A table-selection pause must say why it paused. The card rendered
// blocking_reason.description, which for a DAG wait was the adapter's
// "Waiting for user input", and the executor's reason beside it said "You
// didn’t specify a table" even when the request named a table the source does
// not have (pipeline 05ace10c: "the events collection", mongodb, 6 tables).

func availableTables(refs ...[2]string) []interface{} {
	out := make([]interface{}, 0, len(refs))
	for _, r := range refs {
		out = append(out, map[string]interface{}{"schema": r[0], "name": r[1], "row_count": float64(10), "columns": float64(3)})
	}
	return out
}

func mongoParkDetails() map[string]interface{} {
	return map[string]interface{}{
		"action_needed": "table_selection",
		"source_type":   "mongodb",
		"reason":        "You didn’t specify a table. Found 6 tables/resources in mongodb. Select what to sync.",
		"available_tables": availableTables(
			[2]string{"datingapp", "users"}, [2]string{"datingapp", "matches"},
			[2]string{"datingapp", "messages"}, [2]string{"datingapp", "likes"},
			[2]string{"datingapp", "reports"}, [2]string{"datingapp", "profiles"},
		),
		"total_tables_available": float64(6),
		"tables_truncated":       false,
	}
}

func TestIsTableSelectionPark(t *testing.T) {
	cases := []struct {
		name    string
		typ     string
		details map[string]interface{}
		want    bool
	}{
		{"batch table_selection", "table_selection", nil, true},
		{"alias needs_tables", "needs_tables", nil, true},
		{"alias needs_table_selection", "needs_table_selection", nil, true},
		{"DAG node wait asking for tables", "node_input", map[string]interface{}{"action_needed": "table_selection", "node_id": "load_gcs"}, true},
		{"DAG node wait asking for something else", "node_input", map[string]interface{}{"action_needed": "approval"}, false},
		{"DAG node wait with no details", "node_input", nil, false},
		{"connection wait", "connection_config", map[string]interface{}{"action_needed": "table_selection"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isTableSelectionPark(tc.typ, tc.details); got != tc.want {
				t.Fatalf("isTableSelectionPark(%q, %v) = %v, want %v", tc.typ, tc.details, got, tc.want)
			}
		})
	}
}

func TestTableSelectionReason(t *testing.T) {
	t.Run("a named table the source lacks is named as missing", func(t *testing.T) {
		got := tableSelectionReason("move the events collection from mongo to gcs daily", mongoParkDetails())
		for _, want := range []string{"Couldn't find `events` in the source.", "Found 6 tables/resources in mongodb.", "Select what to sync."} {
			if !strings.Contains(got, want) {
				t.Fatalf("reason %q lacks %q", got, want)
			}
		}
		if strings.Contains(got, "specify") {
			t.Fatalf("reason %q still says no table was specified", got)
		}
	})

	t.Run("only the missing names are listed as missing", func(t *testing.T) {
		got := tableSelectionReason("sync users and events tables to gcs", mongoParkDetails())
		if !strings.Contains(got, "Couldn't find `events` in the source.") {
			t.Fatalf("reason %q lacks the missing name", got)
		}
		if strings.Contains(got, "`users` in the source") {
			t.Fatalf("reason %q calls a present table missing", got)
		}
	})

	t.Run("a bare name in two schemas asks which one", func(t *testing.T) {
		d := mongoParkDetails()
		d["available_tables"] = availableTables([2]string{"a", "orders"}, [2]string{"b", "orders"}, [2]string{"a", "users"})
		d["total_tables_available"] = float64(3)
		got := tableSelectionReason("sync the orders table", d)
		if !strings.Contains(got, "`orders` exists in more than one schema (`a.orders`, `b.orders`).") {
			t.Fatalf("reason %q does not name the ambiguity", got)
		}
	})

	t.Run("named tables that all exist are offered for confirmation", func(t *testing.T) {
		got := tableSelectionReason("sync the users table", mongoParkDetails())
		if !strings.Contains(got, "Confirm the tables you named: `datingapp.users`.") {
			t.Fatalf("reason %q does not ask to confirm the named table", got)
		}
		if strings.Contains(got, "Couldn't find") {
			t.Fatalf("reason %q calls a present table missing", got)
		}
	})

	t.Run("a cut list never calls an unlisted name missing", func(t *testing.T) {
		d := mongoParkDetails()
		d["total_tables_available"] = float64(12000)
		d["tables_truncated"] = true
		got := tableSelectionReason("sync the events table", d)
		if strings.Contains(got, "Couldn't find") {
			t.Fatalf("reason %q claims a name is missing from a truncated list", got)
		}
		for _, want := range []string{"`events` isn't among the 6 listed", "type its name", "Listed 6 of 12000 tables/resources in mongodb."} {
			if !strings.Contains(got, want) {
				t.Fatalf("reason %q lacks %q", got, want)
			}
		}
	})

	t.Run("no named table keeps the executor's reason", func(t *testing.T) {
		d := mongoParkDetails()
		if got := tableSelectionReason("move everything from mongo to gcs", d); got != d["reason"] {
			t.Fatalf("reason = %q, want the executor's %q", got, d["reason"])
		}
	})

	// The stored request of a chat CDC create starts with the gateway's own
	// "Stream CDC from <src> to <dst>." — prod a6ca2103 said "Couldn't find `cdc`".
	t.Run("a chat CDC request blames no table", func(t *testing.T) {
		d := mongoParkDetails()
		got := tableSelectionReason("Stream CDC from postgresql to mongodb. Stream changes from Postgresql Source to MongoDB Dest", d)
		if got != d["reason"] {
			t.Fatalf("reason = %q, want the executor's %q", got, d["reason"])
		}
	})

	t.Run("no table list keeps the executor's discovery reason", func(t *testing.T) {
		d := mongoParkDetails()
		d["available_tables"] = []interface{}{}
		d["reason"] = "Schema discovery failed (timeout). Enter a table/resource name manually (e.g. `users` or `mydb.users`)."
		if got := tableSelectionReason("sync the events table", d); got != d["reason"] {
			t.Fatalf("reason = %q, want the discovery failure %q", got, d["reason"])
		}
	})

	t.Run("nothing to say returns empty so the stored text stays", func(t *testing.T) {
		if got := tableSelectionReason("", map[string]interface{}{}); got != "" {
			t.Fatalf("reason = %q, want empty", got)
		}
	})

	t.Run("an unknown source type still reads", func(t *testing.T) {
		d := mongoParkDetails()
		delete(d, "source_type")
		got := tableSelectionReason("sync the events table", d)
		if !strings.Contains(got, "Found 6 tables/resources in the source.") {
			t.Fatalf("reason %q lacks the source-neutral tail", got)
		}
	})
}

// Through the real handler: a DAG node_input wait (the 05ace10c shape) must
// reach the client with the reason, not the adapter's "Waiting for user input".
func TestGetPipelineState_TableSelectionParkSaysWhy(t *testing.T) {
	body := serveParkState(t, "node_input", "Waiting for user input",
		`{"action_needed":"table_selection","node_id":"load_gcs","source_type":"mongodb",`+
			`"reason":"You didn’t specify a table. Found 6 tables/resources in mongodb. Select what to sync.",`+
			`"available_tables":[{"schema":"datingapp","name":"users"},{"schema":"datingapp","name":"matches"},`+
			`{"schema":"datingapp","name":"messages"},{"schema":"datingapp","name":"likes"},`+
			`{"schema":"datingapp","name":"reports"},{"schema":"datingapp","name":"profiles"}],`+
			`"total_tables_available":6,"tables_truncated":false}`,
		func(m sqlmock.Sqlmock) {
			m.ExpectQuery(`SELECT natural_language_request FROM pipelines WHERE id = \$1`).
				WithArgs(gatePipeID).
				WillReturnRows(sqlmock.NewRows([]string{"natural_language_request"}).
					AddRow("move the events collection from my mongo to gcs daily"))
		})
	br, _ := body["blocking_reason"].(map[string]interface{})
	desc, _ := br["description"].(string)
	if !strings.Contains(desc, "Couldn't find `events` in the source.") {
		t.Fatalf("blocking_reason.description = %q, want it to name the missing table", desc)
	}
	if msg, _ := body["message"].(string); msg != desc {
		t.Fatalf("message = %q, want it to follow the description %q", msg, desc)
	}
}

// A wait that is not a table selection keeps its stored description and reads
// nothing more from the database.
func TestGetPipelineState_OtherParkKeepsItsDescription(t *testing.T) {
	body := serveParkState(t, "node_input", "Approve the schema change",
		`{"action_needed":"approval","node_id":"load_gcs"}`, nil)
	br, _ := body["blocking_reason"].(map[string]interface{})
	if desc, _ := br["description"].(string); desc != "Approve the schema change" {
		t.Fatalf("blocking_reason.description = %q, want the stored text", desc)
	}
}

func serveParkState(t *testing.T, blockingType, blockingDesc, metadata string, extra func(sqlmock.Sqlmock)) map[string]interface{} {
	t.Helper()
	return serveState(t, stateFixture{
		progressStatus: "waiting_for_user", pipelineStatus: "running", syncMode: "batch",
		execStatus:   "running",
		blockingType: blockingType, blockingDesc: blockingDesc,
		message: blockingDesc, metadata: metadata,
		extra: extra,
	})
}

// The chat's one-time transform step for named tables is a table-selection wait
// whose details carry review_transforms_only plus the tables. Only that pair
// marks it: the flag alone (or a stale false from an earlier wait) is an
// ordinary picker that keeps its reason text and AI ranking.
func TestIsTransformsReviewPark(t *testing.T) {
	cases := []struct {
		name    string
		details map[string]interface{}
		want    bool
	}{
		{"review with tables (JSON)", map[string]interface{}{"review_transforms_only": true, "preselected_tables": []interface{}{"public.users"}}, true},
		{"review with tables (in-process)", map[string]interface{}{"review_transforms_only": true, "preselected_tables": []string{"public.users"}}, true},
		{"review without tables", map[string]interface{}{"review_transforms_only": true, "preselected_tables": []interface{}{}}, false},
		{"ordinary wait (always-written false)", map[string]interface{}{"review_transforms_only": false, "preselected_tables": []interface{}{}}, false},
		{"older worker: keys absent", map[string]interface{}{"available_tables": []interface{}{}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isTransformsReviewPark(tc.details); got != tc.want {
				t.Fatalf("isTransformsReviewPark = %v, want %v", got, tc.want)
			}
		})
	}
}
