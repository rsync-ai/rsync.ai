package executor

import (
	"strings"
	"testing"
)

// TestExtractExportRowsColumns pins the normalization the delegated Data Explorer
// path (ExplorerQuery) relies on to turn a connector's MCP `export` result into
// {rows, columns}. This is the primary offline proof for the BigQuery delegated
// query path, which cannot be live-verified without a BigQuery connection + creds.
func TestExtractExportRowsColumns(t *testing.T) {
	t.Run("data key with explicit columns preserves order", func(t *testing.T) {
		result := map[string]interface{}{
			"data": []interface{}{
				map[string]interface{}{"id": float64(1), "name": "a"},
				map[string]interface{}{"id": float64(2), "name": "b"},
			},
			"columns": []interface{}{"id", "name"},
		}
		rows, cols := extractExportRowsColumns(result, 100)
		if len(rows) != 2 {
			t.Fatalf("rows=%d, want 2", len(rows))
		}
		if len(cols) != 2 || cols[0] != "id" || cols[1] != "name" {
			t.Errorf("cols=%v, want [id name] (BigQuery emits an explicit ordered column list)", cols)
		}
	})

	t.Run("records key is accepted as a fallback", func(t *testing.T) {
		result := map[string]interface{}{
			"records": []interface{}{map[string]interface{}{"x": "1"}},
		}
		rows, cols := extractExportRowsColumns(result, 100)
		if len(rows) != 1 {
			t.Fatalf("rows=%d, want 1", len(rows))
		}
		if len(cols) != 1 || cols[0] != "x" {
			t.Errorf("cols=%v, want [x] backfilled from the first row", cols)
		}
	})

	t.Run("a backfilled column list has one order, run after run", func(t *testing.T) {
		// KI-DELEGATED-QUERY-COLUMN-ORDER: a row is a Go map, so a list read off
		// its keys came back in a new order on every call.
		want := []string{"alpha", "amount", "created_at", "id", "mid", "zeta"}
		for i := 0; i < 50; i++ {
			result := map[string]interface{}{
				"data": []interface{}{map[string]interface{}{
					"zeta": 1, "id": 2, "alpha": 3, "created_at": 4, "mid": 5, "amount": 6,
				}},
			}
			_, cols := extractExportRowsColumns(result, 100)
			if strings.Join(cols, ",") != strings.Join(want, ",") {
				t.Fatalf("call %d: cols=%v, want %v (sorted, never map order)", i, cols, want)
			}
		}
	})

	t.Run("limit trims oversize result sets", func(t *testing.T) {
		result := map[string]interface{}{
			"data": []interface{}{
				map[string]interface{}{"a": 1},
				map[string]interface{}{"a": 2},
				map[string]interface{}{"a": 3},
			},
			"columns": []interface{}{"a"},
		}
		rows, _ := extractExportRowsColumns(result, 2)
		if len(rows) != 2 {
			t.Fatalf("rows=%d, want 2 (trimmed to limit)", len(rows))
		}
	})

	t.Run("does not impose SampleRows' 100-row cap", func(t *testing.T) {
		// Regression guard: the Explorer must return more than 100 rows (SampleRows
		// caps at 100; ExplorerQuery deliberately does not). 300 rows at limit 500
		// must all survive.
		data := make([]interface{}, 300)
		for i := range data {
			data[i] = map[string]interface{}{"a": i}
		}
		result := map[string]interface{}{"data": data, "columns": []interface{}{"a"}}
		rows, _ := extractExportRowsColumns(result, 500)
		if len(rows) != 300 {
			t.Fatalf("rows=%d, want 300 (no 100-row cap on the Explorer path)", len(rows))
		}
	})

	t.Run("nil result yields empty, not nil-panic", func(t *testing.T) {
		rows, cols := extractExportRowsColumns(nil, 100)
		if len(rows) != 0 || len(cols) != 0 {
			t.Errorf("want empty rows+cols, got rows=%d cols=%d", len(rows), len(cols))
		}
	})
}
