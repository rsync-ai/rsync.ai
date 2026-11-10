package assistant

import (
	"encoding/json"
	"strings"
	"testing"
)

func rowsOf(n, size int) []any {
	out := make([]any, n)
	for i := range out {
		out[i] = strings.Repeat("r", size)
	}
	return out
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestFitResult_UnderTheCapIsUnchanged(t *testing.T) {
	in := `{"total":3, "items":[1,2,3]}`
	got, ok := fitResult(in, len(in))
	if !ok || got != in {
		t.Fatalf("fitResult = %q, %v; want the result untouched", got, ok)
	}
}

// The longest list goes first; a short list beside it is left whole.
func TestFitResult_CutsTheLongestList(t *testing.T) {
	in := mustJSON(t, map[string]any{"tables": rowsOf(3, 10), "foreign_keys": rowsOf(20, 100), "tables_matched": 3})
	max := len(in) / 2
	got, ok := fitResult(in, max)
	if !ok || len(got) > max {
		t.Fatalf("fitResult = %d characters, %v; want at most %d", len(got), ok, max)
	}
	var out struct {
		Tables        []any  `json:"tables"`
		ForeignKeys   []any  `json:"foreign_keys"`
		TablesMatched int    `json:"tables_matched"`
		Trimmed       string `json:"trimmed"`
	}
	if err := json.Unmarshal([]byte(got), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Tables) != 3 || out.TablesMatched != 3 {
		t.Fatalf("the short list or the count was cut: %s", got)
	}
	if n := len(out.ForeignKeys); n == 0 || n >= 20 || !strings.Contains(out.Trimmed, "of the 20 foreign_keys") {
		t.Fatalf("foreign_keys kept %d, trimmed %q", n, out.Trimmed)
	}
}

// When emptying the longest list is not enough, the next one is cut too, and
// the note names both.
func TestFitResult_CutsTheNextListWhenOneIsNotEnough(t *testing.T) {
	in := mustJSON(t, map[string]any{"a": rowsOf(10, 100), "b": rowsOf(10, 90), "total": 20})
	max := 700
	got, ok := fitResult(in, max)
	if !ok || len(got) > max {
		t.Fatalf("fitResult = %d characters, %v; want at most %d", len(got), ok, max)
	}
	var out struct {
		A, B    []any
		Total   int
		Trimmed string
	}
	if err := json.Unmarshal([]byte(got), &out); err != nil {
		t.Fatal(err)
	}
	if out.Total != 20 || len(out.A) != 0 || len(out.B) == 0 || len(out.B) >= 10 {
		t.Fatalf("a=%d b=%d total=%d", len(out.A), len(out.B), out.Total)
	}
	if !strings.Contains(out.Trimmed, "of the 10 a") || !strings.Contains(out.Trimmed, "of the 10 b") {
		t.Fatalf("trimmed = %q, want both lists named", out.Trimmed)
	}
}

func TestFitResult_RefusesWhatCannotBeCut(t *testing.T) {
	for name, in := range map[string]string{
		"text":       strings.Repeat("x", 300),
		"no list":    `{"note":"` + strings.Repeat("x", 300) + `"}`,
		"array":      `[` + strings.Repeat(`1,`, 150) + `1]`,
		"lists gone": mustJSON(t, map[string]any{"items": rowsOf(2, 10), "note": strings.Repeat("x", 300)}),
	} {
		if got, ok := fitResult(in, 100); ok {
			t.Errorf("%s: fitResult = %q, want it refused", name, got)
		}
	}
}
