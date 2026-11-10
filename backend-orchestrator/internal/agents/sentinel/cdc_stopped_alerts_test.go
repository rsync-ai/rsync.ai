package sentinel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// The bug class: an alert raised by a CDC detector that reads running pipelines only
// outlives the pipeline's run. Stop the pipeline (or let the Sentinel's restart cap stop
// it) while capture-stopped is open, and nothing ever re-checks it: the alert stays open
// for good. cdc_stopped_alerts.go closes these alerts, silently, on every tick.

const stoppedTestPID = "66666666-7777-4888-8999-aaaaaaaaaaaa"

// TestStoppedPipelineAlertsCloseOnTheTick drives the real tick with Connect unreachable
// and no running pipeline: the alerts of a stopped pipeline must still be closed, with
// no resolved notice.
func TestStoppedPipelineAlertsCloseOnTheTick(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	sent := captureResolved(t)

	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()

	s := &CDCSentinel{
		db:         db,
		connectURL: deadURL,
		httpClient: &http.Client{Timeout: 2 * time.Second},
	}

	mock.ExpectQuery("SELECT id").
		WillReturnRows(sqlmock.NewRows([]string{"id"}))
	// Written out, not read from the vars: changing which alerts close, or on which
	// statuses, has to change this test too.
	mock.ExpectQuery(regexp.QuoteMeta("DELETE FROM sentinel_active_issues si")).
		WithArgs(
			string(ComponentTypeCDCPipeline),
			"stopped", "paused", "failed", "cancelled", "completed", "archived",
			"cdc-capture-stopped-", "cdc-source-stalled-", "cdc-lag-", "cdc-sink-lag-", "cdc-sink-absent-",
		).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).
			AddRow(captureStoppedIssueID(stoppedTestPID)).
			AddRow(sinkLagIssueID(stoppedTestPID)))
	mock.ExpectQuery(regexp.QuoteMeta("FROM pipeline_dependencies d")).
		WillReturnRows(captureRows())

	s.checkActivePipelines(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("the tick did not close the alerts of a stopped pipeline: %v\n\n"+
			"Every CDC detector reads running pipelines only, so nothing else ever will.", err)
	}
	if len(*sent) != 0 {
		t.Fatalf("closing a stopped pipeline's alerts sent %d resolved notice(s); it must be silent: "+
			"the owner already got the Stop notice, and the copy would say the stream is moving", len(*sent))
	}
}

// TestCloseOnStopSQLNeverMatchesTheActivePipelineQuery: the unordered tick tests match
// statements by regex, and "SELECT id" is how they recognise the active-pipeline read.
func TestCloseOnStopSQLNeverMatchesTheActivePipelineQuery(t *testing.T) {
	for _, re := range []string{"SELECT id", "FROM pipeline_dependencies d", "FROM cdc_resources", "SELECT cdc_mode FROM pipelines"} {
		if regexp.MustCompile(re).MatchString(closeOnStopSQL) {
			t.Fatalf("closeOnStopSQL matches %q, which the tick tests use for another statement", re)
		}
	}
}

// alertsKeptOnStop are the CDC alert ids a stopped pipeline keeps, and why.
var alertsKeptOnStop = map[string]string{
	"cdc-connector-down-": "the Sentinel's terminal verdict: it is why the pipeline stopped",
	"cdc-sink-wedged-":    "a terminal escalation: it closes once the sink applies again after Start",
	"cdc-rows-parked-":    "the rows stay parked whether or not the pipeline runs",
	"cdc-position-":       "raised only on a stopped pipeline: its resume position is gone",
	"cdc-wal-":            "keyed by slot, and a stopped pipeline's slot still holds WAL",
}

// notAlertIDs are cdc- literals that are not sentinel_active_issues ids.
var notAlertIDs = map[string]string{
	"cdc-sentinel-": "the Sentinel's worker id",
}

// TestEveryCDCAlertIsClosedOnStopOrKeptForAReason covers the class: a new CDC alert id
// must be put on one side or the other, or this fails. It reads every "cdc-…-" id
// literal in the package's source, which is how each builder and inline id is spelled.
func TestEveryCDCAlertIsClosedOnStopOrKeptForAReason(t *testing.T) {
	closed := map[string]bool{}
	for _, issueID := range alertsClosedOnStop {
		closed[issueID("")] = true
	}
	for prefix := range alertsKeptOnStop {
		if closed[prefix] {
			t.Errorf("%q is both closed on stop and kept", prefix)
		}
	}

	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	literal := regexp.MustCompile(`"(cdc-[a-z-]+-)(%s)?"`)
	seen := map[string]bool{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range literal.FindAllStringSubmatch(string(src), -1) {
			prefix := m[1]
			seen[prefix] = true
			if !closed[prefix] && alertsKeptOnStop[prefix] == "" && notAlertIDs[prefix] == "" {
				t.Errorf("%s: alert id %q is neither closed on stop (alertsClosedOnStop) nor kept "+
					"for a reason (alertsKeptOnStop). Does it describe a running stream? Then a "+
					"stopped pipeline would keep it open for good.", f, prefix)
			}
		}
	}
	if len(seen) == 0 {
		t.Fatal("found no cdc- id literals: the scan is broken, not the package clean")
	}
	for prefix := range closed {
		if !seen[prefix] {
			t.Errorf("closed-on-stop id %q is not spelled anywhere in the package", prefix)
		}
	}
	for prefix := range alertsKeptOnStop {
		if !seen[prefix] {
			t.Errorf("kept id %q is not spelled anywhere in the package: drop it from alertsKeptOnStop", prefix)
		}
	}
}

// TestAlertsClosedOnStopAreAPrefixPlusThePipelineID: the query matches each id as
// builder("") || pipeline id, which only holds if the builder is exactly that.
func TestAlertsClosedOnStopAreAPrefixPlusThePipelineID(t *testing.T) {
	for _, issueID := range alertsClosedOnStop {
		if got, want := issueID(stoppedTestPID), issueID("")+stoppedTestPID; got != want {
			t.Errorf("id %q is not its prefix %q + the pipeline id", got, issueID(""))
		}
	}
}
