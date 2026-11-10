package db

import (
	"regexp"
	"strings"
	"testing"
)

// Every usage ledger names the workspace its rows belong to. llm_usage_events
// (057) was keyed by user and pipeline only, so per-workspace LLM spend could
// not be read; its siblings data_transfer_usage_events (073) and
// nl_query_usage_events (075) always carried workspace_id. Migration 124 adds
// it, and this scan fails for the next *_usage_events table that has none.

var (
	reUsageLedgerCreate = regexp.MustCompile(`create table if not exists (\w+_usage_events) \((.*?)\) ;`)
	reUsageLedgerAlter  = regexp.MustCompile(`alter table (?:if exists )?(\w+_usage_events) (.*?);`)
	reWorkspaceColumn   = regexp.MustCompile(`\bworkspace_id uuid\b[^,;]*`)
)

// usageLedgers maps each *_usage_events table to every column statement made on
// it across the migration sequence.
func usageLedgers(t *testing.T) map[string]string {
	t.Helper()
	ledgers := map[string]string{}
	for _, f := range loadMigrations(t) {
		// normSQL puts a space before the closing ';' only when the source had
		// whitespace there, so give every statement end one to match on.
		body := strings.ReplaceAll(normSQL(stripSQLComments(f.body)), ");", ") ;")
		for _, m := range reUsageLedgerCreate.FindAllStringSubmatch(body, -1) {
			ledgers[m[1]] += " " + m[2]
		}
		for _, m := range reUsageLedgerAlter.FindAllStringSubmatch(body, -1) {
			ledgers[m[1]] += " " + m[2]
		}
	}
	return ledgers
}

func TestUsageLedgerScan_FindsTheKnownLedgers(t *testing.T) {
	ledgers := usageLedgers(t)
	for _, want := range []string{"llm_usage_events", "data_transfer_usage_events", "nl_query_usage_events"} {
		if _, ok := ledgers[want]; !ok {
			t.Errorf("the scan did not find %s; it is broken, not the corpus clean (found %v)", want, ledgers)
		}
	}
}

func TestEveryUsageLedgerNamesItsWorkspace(t *testing.T) {
	for name, columns := range usageLedgers(t) {
		if !reWorkspaceColumn.MatchString(columns) {
			t.Errorf("%s has no workspace_id UUID column, so its rows cannot be read per workspace", name)
		}
	}
}

// llm-service inserts the row and charges the user's quota in one try block
// (src/utils/llm_cost.py _writer_loop), so a workspace value that fails the
// INSERT loses the quota charge too. A call outside any workspace still has a
// cost, and spend history outlives a deleted workspace.
func TestTheLLMLedgerWorkspaceCanNeverFailTheInsert(t *testing.T) {
	decl := reWorkspaceColumn.FindString(usageLedgers(t)["llm_usage_events"])
	if decl == "" {
		t.Fatal("llm_usage_events has no workspace_id UUID column")
	}
	for _, bad := range []string{"not null", "references"} {
		if strings.Contains(decl, bad) {
			t.Errorf("llm_usage_events.workspace_id is %q: %s would fail an insert, and the quota charge with it", decl, bad)
		}
	}
}
