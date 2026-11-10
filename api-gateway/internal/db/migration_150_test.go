package db

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Static guard for migration 150: CreateWorkflow (handlers/workflows.go)
// stores proposed_by 'none', and the CHECK from migration 130 refused it.

func TestMigration150_AWorkflowMayBeMadeByAPerson(t *testing.T) {
	matches, err := filepath.Glob(filepath.Join("..", "..", "migrations", "150_*.sql"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("expected exactly one 150_*.sql migration, found %v (err %v)", matches, err)
	}
	b, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatalf("read %s: %v", matches[0], err)
	}
	body := normSQL(stripSQLComments(string(b)))
	for _, want := range []string{
		// Migration 130 named no constraint, so PostgreSQL named it
		// <table>_<column>_check; dropping any other name leaves the old one.
		"alter table workflows drop constraint if exists workflows_proposed_by_check;",
		"alter table workflows add constraint workflows_proposed_by_check check (proposed_by in ('none', 'assistant', 'agent'));",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("migration 150 lost %q", want)
		}
	}
}
