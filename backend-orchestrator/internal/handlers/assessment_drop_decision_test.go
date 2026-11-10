package handlers

import (
	"testing"

	"github.com/rsync-ai/backend-orchestrator/internal/assessor"
)

// A selected table that vanished from a batch source waits for a drop decision
// the run itself files. The pre-flight used to block that run with
// CDC_TABLE_NOT_FOUND_IN_SOURCE, so the decision could never be filed; a table
// the drift loop knows about must now warn, and only that table.
func TestWarnTablesAwaitingDropDecision(t *testing.T) {
	missing := func(obj string) assessor.Check {
		return assessor.Check{Code: codeTableNotFoundInSource, Severity: assessor.SeverityError, Object: obj}
	}
	result := func() *assessor.Result {
		r := &assessor.Result{Checks: []assessor.Check{
			missing("public.orders"),
			missing("public.never_synced"),
			{Code: "CDC_TABLE_MISSING_PRIMARY_KEY", Severity: assessor.SeverityWarning, Passed: true, Object: "public.users"},
		}}
		assessor.Summarize(r)
		return r
	}

	t.Run("a table with a baseline warns and stops blocking", func(t *testing.T) {
		r := result()
		r.Checks = r.Checks[:1]
		assessor.Summarize(r)
		if !r.BlocksStart() {
			t.Fatal("precondition: the missing table must block before the downgrade")
		}
		warnTablesAwaitingDropDecision(r, map[string]bool{"public.orders": true})
		if r.BlocksStart() {
			t.Fatalf("still blocks after the downgrade: %+v", r)
		}
		ch := r.Checks[0]
		if ch.Severity != assessor.SeverityWarning || ch.Passed {
			t.Fatalf("want an unpassed warning, got %+v", ch)
		}
		if ch.Remediation == nil || len(ch.Remediation.Steps) != 3 {
			t.Fatalf("want the approve/reject/restore steps, got %+v", ch.Remediation)
		}
		if r.OverallStatus != "warning" || r.WarningCount != 1 || r.FailedCount != 0 {
			t.Fatalf("counts not re-summarized: %+v", r)
		}
	})

	t.Run("a table without a baseline keeps blocking", func(t *testing.T) {
		r := result()
		warnTablesAwaitingDropDecision(r, map[string]bool{"public.orders": true})
		if !r.BlocksStart() {
			t.Fatal("public.never_synced has no baseline; the run would not leave it out, so it must still block")
		}
		if r.Checks[1].Severity != assessor.SeverityError {
			t.Fatalf("never_synced was downgraded: %+v", r.Checks[1])
		}
	})

	t.Run("nothing awaiting leaves the result as it was", func(t *testing.T) {
		r := result()
		warnTablesAwaitingDropDecision(r, nil)
		if r.Checks[0].Severity != assessor.SeverityError || !r.BlocksStart() {
			t.Fatalf("changed without an awaiting table: %+v", r)
		}
	})

	t.Run("missingSourceTables lists only failed missing-table checks", func(t *testing.T) {
		got := missingSourceTables(result())
		if len(got) != 2 || got[0] != "public.orders" || got[1] != "public.never_synced" {
			t.Fatalf("got %v", got)
		}
	})
}
