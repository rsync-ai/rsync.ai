package workflows

import (
	"sort"
	"strings"
	"testing"
)

// reportedSet mirrors what postflightSilentDropCheck seeds while scanning stats rows:
// every reported name in BOTH its qualified and bare form, lower-cased.
func reportedSet(names ...string) map[string]bool {
	m := make(map[string]bool, len(names)*2)
	for _, n := range names {
		n = strings.ToLower(strings.TrimSpace(n))
		if n == "" {
			continue
		}
		m[n] = true
		if i := strings.LastIndex(n, "."); i >= 0 && i+1 < len(n) {
			m[n[i+1:]] = true
		}
	}
	return m
}

func TestDiffSelectedAgainstReported(t *testing.T) {
	tests := []struct {
		name        string
		selected    []string
		reported    map[string]bool
		wantMissing []string
		wantMatched int
	}{
		{
			// The blind spot this whole path exists for: the run completed, two tables
			// reported, and the third contributed nothing at all. Per-row inspection
			// cannot see it because there is no row to inspect.
			name:        "selected table with no stats row is missing",
			selected:    []string{"public.orders", "public.customers", "public.invoices"},
			reported:    reportedSet("public.orders", "public.customers"),
			wantMissing: []string{"public.invoices"},
			wantMatched: 2,
		},
		{
			name:        "all selected tables reported",
			selected:    []string{"public.orders", "public.customers"},
			reported:    reportedSet("public.orders", "public.customers"),
			wantMissing: nil,
			wantMatched: 2,
		},
		{
			// The sink reports qualified names; a selection persisted bare must still
			// match, or every run of that pipeline would be failed.
			name:        "bare selection matches qualified report",
			selected:    []string{"orders"},
			reported:    reportedSet("public.orders"),
			wantMissing: nil,
			wantMatched: 1,
		},
		{
			// And the inverse — a qualified selection against a bare report.
			name:        "qualified selection matches bare report",
			selected:    []string{"public.orders"},
			reported:    reportedSet("orders"),
			wantMissing: nil,
			wantMatched: 1,
		},
		{
			name:        "matching is case insensitive",
			selected:    []string{"Public.Orders"},
			reported:    reportedSet("public.orders"),
			wantMissing: nil,
			wantMatched: 1,
		},
		{
			// matched==0 is the caller's naming-mismatch signal: it must NOT be
			// reported as "every table was dropped".
			name:        "no overlap yields zero matched so the caller can bail",
			selected:    []string{"warehouse.orders"},
			reported:    reportedSet("analytics.shipments"),
			wantMissing: []string{"warehouse.orders"},
			wantMatched: 0,
		},
		{
			name:        "blank and whitespace-only selections are ignored",
			selected:    []string{"  ", "", "public.orders"},
			reported:    reportedSet("public.orders"),
			wantMissing: nil,
			wantMatched: 1,
		},
		{
			name:        "trailing dot does not strip to empty",
			selected:    []string{"orders."},
			reported:    reportedSet("orders"),
			wantMissing: []string{"orders."},
			wantMatched: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			missing, matched := diffSelectedAgainstReported(tt.selected, tt.reported)
			if matched != tt.wantMatched {
				t.Errorf("matched = %d, want %d", matched, tt.wantMatched)
			}
			sort.Strings(missing)
			want := append([]string(nil), tt.wantMissing...)
			sort.Strings(want)
			if len(missing) != len(want) {
				t.Fatalf("missing = %v, want %v", missing, want)
			}
			for i := range missing {
				if missing[i] != want[i] {
					t.Fatalf("missing = %v, want %v", missing, want)
				}
			}
		})
	}
}

// A pipeline with no persisted selection has nothing to diff against. The caller
// returns early on len(selected)==0, but the helper must also stay quiet rather than
// inventing a match count.
func TestDiffSelectedAgainstReportedEmptySelection(t *testing.T) {
	missing, matched := diffSelectedAgainstReported(nil, reportedSet("public.orders"))
	if len(missing) != 0 || matched != 0 {
		t.Fatalf("empty selection: missing=%v matched=%d, want none/0", missing, matched)
	}
}

// A selected table whose drop waits for a decision is left out of the run on
// purpose (orchestrator withoutTablesAwaitingDrop), so it reports no stats row.
// That is not a silent drop: failing the run for it hid the decision the user is
// being asked for (prod t-clickup list_task, 2026-10-05). Matching follows the
// executor's: exact, or bare when one side carries no schema, case-insensitive.
func TestSelectionWithoutTablesAwaitingDrop(t *testing.T) {
	selected := []string{"list_task", "team", "public.orders", "Legacy"}
	remaining := selectionWithoutTablesAwaitingDrop(selected, []string{"LIST_TASK", "orders", "app.legacy"})
	if strings.Join(remaining, ",") != "team" {
		t.Fatalf("remaining = %v, want [team]", remaining)
	}
	missing, matched := diffSelectedAgainstReported(remaining, reportedSet("team"))
	if len(missing) != 0 || matched != 1 {
		t.Errorf("missing=%v matched=%d, want none/1", missing, matched)
	}

	// Control: without the pending decisions every left-out table reads as dropped.
	missing, _ = diffSelectedAgainstReported(selected, reportedSet("team"))
	if len(missing) != 3 {
		t.Errorf("control: missing = %v, want the 3 left-out tables", missing)
	}

	// A different schema's table of the same name is not the one awaiting a decision.
	if got := selectionWithoutTablesAwaitingDrop([]string{"sales.orders"}, []string{"public.orders"}); len(got) != 1 {
		t.Errorf("sales.orders was excluded by public.orders's pending drop: %v", got)
	}
	if got := selectionWithoutTablesAwaitingDrop(selected, nil); len(got) != len(selected) {
		t.Errorf("no pending drops: remaining = %v, want the whole selection", got)
	}
}
