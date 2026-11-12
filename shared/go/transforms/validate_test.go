package transforms

import (
	"reflect"
	"testing"
)

func TestNormalizeAndValidate_SortsAndFiltersByTable(t *testing.T) {
	raw := []map[string]any{
		{
			"transform_order": 2,
			"enabled":         true,
			"transform_config": map[string]any{
				"operation": "mask",
				"column":    "users.email",
				"table":     "users",
			},
		},
		{
			"transform_order": 1,
			"enabled":         true,
			"type":            "exclude",
			"config": map[string]any{
				"columns": []any{"password"},
			},
			"scope": map[string]any{"table": "orders"},
		},
	}

	out, warnings, err := NormalizeAndValidate(raw, "users", NormalizeModeExecution)
	if err != nil {
		t.Fatalf("unexpected error: %v (warnings=%v)", err, warnings)
	}
	if len(out) != 1 {
		t.Fatalf("expected 1 transform after table scoping, got %d (%v)", len(out), out)
	}
	if out[0].Type != "mask_pii" {
		t.Fatalf("expected mask_pii, got %q", out[0].Type)
	}
	if out[0].Order != 2 {
		t.Fatalf("expected order preserved from transform_order=2, got %d", out[0].Order)
	}
	if col, _ := out[0].Config["column"].(string); col != "users.email" {
		t.Fatalf("expected column preserved, got %q", col)
	}
}

func TestNormalizeAndValidate_PreviewSkipsUnknown(t *testing.T) {
	raw := []map[string]any{
		{"type": "does_not_exist", "config": map[string]any{}},
		{"type": "truncate", "config": map[string]any{"column": "name", "max_length": 3}},
	}

	out, warnings, err := NormalizeAndValidate(raw, "", NormalizeModePreview)
	if err != nil {
		t.Fatalf("preview should not error, got: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("expected 1 valid transform, got %d", len(out))
	}
	if out[0].Type != "truncate" {
		t.Fatalf("expected truncate, got %q", out[0].Type)
	}
	if len(warnings) == 0 {
		t.Fatalf("expected warnings for unknown transform")
	}
}

func TestNormalizeAndValidate_BlocksRequiresFullDatasetForCDC(t *testing.T) {
	raw := []map[string]any{
		{
			"type":                  "sql",
			"requires_full_dataset": true,
			"config":                map[string]any{"query": "select 1"},
		},
	}

	_, _, err := NormalizeAndValidate(raw, "", NormalizeModeCDC)
	if err == nil {
		t.Fatalf("expected error blocking requires_full_dataset for CDC")
	}
}

func TestNormalizeAndValidate_RejectsSQLUntilEngineExists(t *testing.T) {
	mk := func() []map[string]any {
		return []map[string]any{
			{"transform_config": map[string]any{"operation": "sql", "query": "select 1"}},
		}
	}
	// Execution/CDC: hard error — no engine can run a sql transform yet, so it
	// must be rejected up front rather than hit the DuckDB stub mid-run.
	for _, mode := range []NormalizeMode{NormalizeModeExecution, NormalizeModeCDC} {
		if _, _, err := NormalizeAndValidate(mk(), "", mode); err == nil {
			t.Fatalf("mode %s: expected sql to be rejected", mode)
		}
	}
	// Preview: skipped with a warning, never a crash.
	out, warnings, err := NormalizeAndValidate(mk(), "", NormalizeModePreview)
	if err != nil {
		t.Fatalf("preview: unexpected error: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("preview: expected sql skipped, got %d transforms", len(out))
	}
	if len(warnings) == 0 {
		t.Fatalf("preview: expected a warning for unsupported sql")
	}
}

func TestNormalizeAndValidate_AcceptsNewTypes(t *testing.T) {
	// NormalizeAndValidate mutates its input config maps (strips "operation"),
	// so build a fresh copy per mode iteration.
	mkRaw := func() []map[string]any {
		return []map[string]any{
			{
				"transform_order":  0,
				"enabled":          true,
				"transform_config": map[string]any{"operation": "json_flatten", "column": "metadata", "max_depth": 2},
			},
			{
				"transform_order":  1,
				"enabled":          true,
				"transform_config": map[string]any{"operation": "array_expand", "column": "tags", "max_elements": 5},
			},
		}
	}
	for _, mode := range []NormalizeMode{NormalizeModeExecution, NormalizeModeCDC, NormalizeModePreview} {
		out, _, err := NormalizeAndValidate(mkRaw(), "", mode)
		if err != nil {
			t.Fatalf("mode %s: unexpected error: %v", mode, err)
		}
		if len(out) != 2 {
			t.Fatalf("mode %s: expected 2 transforms, got %d", mode, len(out))
		}
		if out[0].Type != "json_flatten" || out[1].Type != "array_expand" {
			t.Fatalf("mode %s: unexpected types %s, %s", mode, out[0].Type, out[1].Type)
		}
	}
}

func TestNormalizeAndValidate_RejectsBadNewTypeConfig(t *testing.T) {
	cases := []map[string]any{
		{"transform_config": map[string]any{"operation": "json_flatten"}},                                   // missing column
		{"transform_config": map[string]any{"operation": "json_flatten", "column": "x", "max_depth": -1}},   // negative depth
		{"transform_config": map[string]any{"operation": "array_expand"}},                                   // missing column
		{"transform_config": map[string]any{"operation": "array_expand", "column": "x", "max_elements": 0}}, // non-positive
	}
	for i, item := range cases {
		_, _, err := NormalizeAndValidate([]map[string]any{item}, "", NormalizeModeExecution)
		if err == nil {
			t.Fatalf("case %d: expected validation error, got nil", i)
		}
	}
}

// NormalizeAndValidate must leave its input as it found it. It used to rewrite the
// caller's config map in place — deleting `operation`, `table` and
// `requires_full_dataset`, renaming aliases — so the same chain validated twice
// failed the second time with "missing transform type". The CDC sink validates
// its cached chain once per message, so every message after the first discarded
// the cache and re-read transform_definitions from the control-plane database.
// Each config shape and each kind of rewrite is covered: a deleted key, a moved
// key (table into scope) and a renamed alias.
func TestNormalizeAndValidate_LeavesItsInputUnchanged(t *testing.T) {
	cases := map[string]func() []map[string]any{
		"transform_config, operation deleted": func() []map[string]any {
			return []map[string]any{{"transform_order": 1, "enabled": true, "transform_config": map[string]any{
				"operation": "mask", "column": "email", "mask_type": "hash", "hash_function": "sha256",
			}}}
		},
		"transform_config, table moved and alias renamed": func() []map[string]any {
			return []map[string]any{{"transform_order": 1, "enabled": true, "transform_config": map[string]any{
				"operation": "type_convert", "column": "qty", "to_type": "integer", "table": "orders",
				"requires_full_dataset": false,
			}}}
		},
		"config": func() []map[string]any {
			return []map[string]any{{"order": 1, "enabled": true, "type": "type_convert", "config": map[string]any{
				"column": "qty", "to_type": "integer", "table": "orders",
			}}}
		},
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			raw, pristine := build(), build()
			first, _, err := NormalizeAndValidate(raw, "orders", NormalizeModeCDC)
			if err != nil {
				t.Fatalf("first validation: %v", err)
			}
			if len(first) != 1 {
				t.Fatalf("first validation kept %d rules, want 1 — the case proves nothing", len(first))
			}
			if !reflect.DeepEqual(raw, pristine) {
				t.Errorf("validation rewrote its input:\n got %v\nwant %v", raw, pristine)
			}
			second, _, err := NormalizeAndValidate(raw, "orders", NormalizeModeCDC)
			if err != nil {
				t.Fatalf("the same chain validated a second time: %v", err)
			}
			if !reflect.DeepEqual(first, second) {
				t.Errorf("a second validation gave a different chain:\n got %+v\nwant %+v", second, first)
			}
		})
	}
}
